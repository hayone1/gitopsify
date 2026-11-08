package provider

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Source is where a provider's releases are: github.com/<Owner>/<Repo>.
type Source struct {
	Owner string
	Repo  string
	Name  string
	// RepoID is the repository's numeric GitHub ID, from the index. When set,
	// Install refuses a repository with another ID (an account renamed and
	// taken over).
	RepoID int64
}

// String returns owner/name, the short form people type.
func (s Source) String() string { return s.Owner + "/" + s.Name }

// ParseSource parses "owner/name", "owner/gitopsify-provider-name" or
// "github.com/owner/gitopsify-provider-name".
func ParseSource(s string) (Source, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "github.com/")
	s = strings.TrimSuffix(s, ".git")
	owner, repo, ok := strings.Cut(s, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return Source{}, fmt.Errorf("%q isn't a provider source; use owner/name, as in hayone1/grafana", s)
	}
	name := strings.TrimPrefix(repo, Prefix)
	if !ValidName(name) {
		return Source{}, fmt.Errorf("%q: %q isn't a provider name", s, name)
	}
	return Source{Owner: owner, Repo: Prefix + name, Name: name}, nil
}

var versionRE = regexp.MustCompile(`^v?[0-9A-Za-z][0-9A-Za-z.+_-]*$`)

// Installer downloads providers from GitHub releases.
type Installer struct {
	// Dir is the providers directory (Dir() when empty).
	Dir string
	// API is the GitHub API base URL (https://api.github.com when empty).
	API string
	// Token authenticates to GitHub (private repositories, rate limits);
	// GITHUB_TOKEN or GH_TOKEN when empty.
	Token string
	// OS and Arch select the release asset (this machine's when empty).
	OS, Arch string
	HTTP     *http.Client
	// Progress receives what the installer is doing.
	Progress func(string)
	// Checksums pins the archive's SHA-256 per platform ("linux_amd64"); a
	// download that doesn't match is refused.
	Checksums map[string]string
	// Verify runs the downloaded executable before it replaces the
	// installed one; Install describes it with a Client when nil.
	Verify func(ctx context.Context, path, name string) (version string, err error)
}

type release struct {
	TagName string  `json:"tag_name"`
	Assets  []asset `json:"assets"`
}

type asset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

// Install downloads version ("" or "latest" for the latest release) of the
// provider at src, checks it against the release's checksums, makes sure it
// runs, and installs it.
func (i *Installer) Install(ctx context.Context, src Source, version string) (*Installed, error) {
	dir := i.Dir
	if dir == "" {
		var err error
		if dir, err = Dir(); err != nil {
			return nil, err
		}
	}
	goos, arch := i.OS, i.Arch
	if goos == "" {
		goos = runtime.GOOS
	}
	if arch == "" {
		arch = runtime.GOARCH
	}

	if src.RepoID != 0 {
		if err := i.checkRepoID(ctx, src); err != nil {
			return nil, err
		}
	}
	rel, err := i.release(ctx, src, version)
	if err != nil {
		return nil, err
	}
	archive, sums := pickAssets(rel.Assets, src.Name, goos, arch)
	if archive == nil {
		var names []string
		for _, a := range rel.Assets {
			names = append(names, a.Name)
		}
		return nil, fmt.Errorf("%s %s has no build for %s/%s (assets: %s); providers publish %s_<version>_<os>_<arch>.tar.gz",
			src, rel.TagName, goos, arch, strings.Join(names, ", "), Prefix+src.Name)
	}
	if sums == nil {
		return nil, fmt.Errorf("%s %s publishes no checksums file (checksums.txt or SHA256SUMS), so the download can't be verified", src, rel.TagName)
	}

	i.progress("downloading %s", archive.Name)
	data, err := i.download(ctx, archive.URL)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", archive.Name, err)
	}
	sumData, err := i.download(ctx, sums.URL)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", sums.Name, err)
	}
	want, err := checksumFor(sumData, archive.Name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", sums.Name, err)
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("%s doesn't match its checksum in %s: the download is corrupt or was tampered with", archive.Name, sums.Name)
	}
	if len(i.Checksums) > 0 {
		pin, ok := i.Checksums[goos+"_"+arch]
		if !ok {
			return nil, fmt.Errorf("checksums are pinned, but not for %s/%s: pin it (gitopsify export --save on a machine that installs this version records every platform)", goos, arch)
		}
		if !strings.EqualFold(pin, want) {
			return nil, fmt.Errorf("%s %s for %s/%s has checksum %s, but %s was pinned: the release changed since it was pinned", src, rel.TagName, goos, arch, want, pin)
		}
	}
	i.progress("checksum verified")

	exe := Prefix + src.Name
	if goos == "windows" {
		exe += ".exe"
	}
	bin, err := extract(archive.Name, data, exe)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(dir, ".install-"+src.Name+"-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	tmpExe := filepath.Join(tmp, exe)
	if err := os.WriteFile(tmpExe, bin, 0o755); err != nil {
		return nil, err
	}
	binSum := sha256.Sum256(bin)
	receipt := Receipt{Source: src.String(), Version: rel.TagName, Asset: archive.Name, SHA256: want,
		Binary: hex.EncodeToString(binSum[:]), Checksums: platformChecksums(rel.Assets, src.Name, sumData)}
	if goos == runtime.GOOS && arch == runtime.GOARCH {
		verify := i.Verify
		if verify == nil {
			verify = func(ctx context.Context, p, name string) (string, error) {
				d, err := (&Client{Name: name, Path: p}).Describe(ctx)
				if err != nil {
					return "", err
				}
				return d.Version, nil
			}
		}
		i.progress("checking that it runs")
		if _, err := verify(ctx, tmpExe, src.Name); err != nil {
			return nil, fmt.Errorf("the downloaded provider doesn't work: %w", err)
		}
	}
	rb, _ := json.MarshalIndent(receipt, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, receiptFile), append(rb, '\n'), 0o644); err != nil {
		return nil, err
	}
	// Swap the directories so a failed install leaves the old version.
	final := filepath.Join(dir, src.Name)
	old := final + ".old"
	_ = os.RemoveAll(old)
	if _, err := os.Stat(final); err == nil {
		if err := os.Rename(final, old); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Rename(old, final)
		return nil, err
	}
	_ = os.RemoveAll(old)
	return &Installed{Name: src.Name, Path: filepath.Join(final, exe), Origin: OriginInstalled, Source: receipt.Source, Version: receipt.Version, SHA256: receipt.SHA256,
		Binary: receipt.Binary, Checksums: receipt.Checksums}, nil
}

func (i *Installer) progress(format string, args ...any) {
	if i.Progress != nil {
		i.Progress(fmt.Sprintf(format, args...))
	}
}

func (i *Installer) client() *http.Client {
	if i.HTTP != nil {
		return i.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (i *Installer) token() string {
	if i.Token != "" {
		return i.Token
	}
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("GH_TOKEN")
}

func (i *Installer) get(ctx context.Context, url, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "gitopsify")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if t := i.token(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	return i.client().Do(req)
}

func (i *Installer) release(ctx context.Context, src Source, version string) (*release, error) {
	api := strings.TrimSuffix(i.API, "/")
	if api == "" {
		api = "https://api.github.com"
	}
	base := fmt.Sprintf("%s/repos/%s/%s/releases", api, src.Owner, src.Repo)
	var urls []string
	switch version {
	case "", "latest":
		urls = []string{base + "/latest"}
	default:
		if !versionRE.MatchString(version) {
			return nil, fmt.Errorf("%q isn't a release version, such as v0.1.0", version)
		}
		urls = []string{base + "/tags/" + url.PathEscape(version)}
		if !strings.HasPrefix(version, "v") {
			urls = append(urls, base+"/tags/"+url.PathEscape("v"+version))
		}
	}
	var lastStatus int
	for _, u := range urls {
		resp, err := i.get(ctx, u, "application/vnd.github+json")
		if err != nil {
			return nil, fmt.Errorf("asking GitHub for %s's releases: %w", src, err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var rel release
			if err := json.Unmarshal(body, &rel); err != nil {
				return nil, fmt.Errorf("reading %s's release: %w", src, err)
			}
			return &rel, nil
		}
		lastStatus = resp.StatusCode
	}
	what := "the latest release"
	if version != "" && version != "latest" {
		what = "release " + version
	}
	switch lastStatus {
	case http.StatusNotFound:
		hint := "check the name and the version"
		if i.token() == "" {
			hint += "; for a private repository set GITHUB_TOKEN"
		}
		return nil, fmt.Errorf("github.com/%s/%s has no %s (%s)", src.Owner, src.Repo, what, hint)
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("GitHub refused the token in GITHUB_TOKEN/GH_TOKEN")
	case http.StatusForbidden, http.StatusTooManyRequests:
		return nil, fmt.Errorf("GitHub's API rate limit was hit; set GITHUB_TOKEN and try again")
	}
	return nil, fmt.Errorf("GitHub answered %d for %s of github.com/%s/%s", lastStatus, what, src.Owner, src.Repo)
}

func (i *Installer) checkRepoID(ctx context.Context, src Source) error {
	api := strings.TrimSuffix(i.API, "/")
	if api == "" {
		api = "https://api.github.com"
	}
	resp, err := i.get(ctx, fmt.Sprintf("%s/repos/%s/%s", api, src.Owner, src.Repo), "application/vnd.github+json")
	if err != nil {
		return fmt.Errorf("asking GitHub about %s: %w", src, err)
	}
	defer resp.Body.Close()
	var repo struct {
		ID int64 `json:"id"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&repo) != nil {
		return fmt.Errorf("github.com/%s/%s can't be found (HTTP %d)", src.Owner, src.Repo, resp.StatusCode)
	}
	if repo.ID != src.RepoID {
		return fmt.Errorf("github.com/%s/%s isn't the repository the provider index lists (ID %d, not %d): the account may have changed hands; not installing", src.Owner, src.Repo, repo.ID, src.RepoID)
	}
	return nil
}

func (i *Installer) download(ctx context.Context, url string) ([]byte, error) {
	resp, err := i.get(ctx, url, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 512<<20))
}

// pickAssets finds the archive for a platform and the checksums file.
func pickAssets(assets []asset, name, goos, arch string) (archive, sums *asset) {
	prefix := Prefix + name + "_"
	for k := range assets {
		a := &assets[k]
		lower := strings.ToLower(a.Name)
		switch {
		case lower == "sha256sums" || strings.HasSuffix(lower, "checksums.txt") || strings.HasSuffix(lower, "sha256sums.txt"):
			sums = a
		case strings.HasPrefix(a.Name, prefix):
			for _, ext := range []string{".tar.gz", ".tgz", ".zip"} {
				if strings.HasSuffix(a.Name, "_"+goos+"_"+arch+ext) {
					archive = a
				}
			}
		}
	}
	return archive, sums
}

// platformChecksums lists the archive checksum of every platform the release
// has, by "<os>_<arch>", so an export file can pin them all.
func platformChecksums(assets []asset, name string, list []byte) map[string]string {
	out := map[string]string{}
	prefix := Prefix + name + "_"
	for _, a := range assets {
		if !strings.HasPrefix(a.Name, prefix) {
			continue
		}
		base := a.Name
		for _, ext := range []string{".tar.gz", ".tgz", ".zip"} {
			base = strings.TrimSuffix(base, ext)
		}
		parts := strings.Split(base, "_")
		if len(parts) < 3 || base == a.Name {
			continue
		}
		if sum, err := checksumFor(list, a.Name); err == nil {
			out[parts[len(parts)-2]+"_"+parts[len(parts)-1]] = sum
		}
	}
	return out
}

// checksumFor finds a file's SHA-256 in a sha256sum-style list.
func checksumFor(list []byte, file string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(list))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == file {
			sum := strings.ToLower(fields[0])
			if len(sum) != 64 {
				return "", fmt.Errorf("the checksum for %s isn't SHA-256", file)
			}
			return sum, nil
		}
	}
	return "", fmt.Errorf("no checksum for %s", file)
}

// extract returns the executable named exe from a .tar.gz or .zip archive.
func extract(archiveName string, data []byte, exe string) ([]byte, error) {
	if strings.HasSuffix(archiveName, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", archiveName, err)
		}
		for _, f := range zr.File {
			if path.Base(f.Name) == exe && !f.FileInfo().IsDir() {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(io.LimitReader(rc, 512<<20))
			}
		}
		return nil, fmt.Errorf("%s doesn't contain %s", archiveName, exe)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", archiveName, err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s doesn't contain %s", archiveName, exe)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", archiveName, err)
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == exe {
			return io.ReadAll(io.LimitReader(tr, 512<<20))
		}
	}
}
