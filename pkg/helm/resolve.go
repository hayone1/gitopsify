package helm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	yamlv3 "go.yaml.in/yaml/v3"
	"sigs.k8s.io/yaml"

	"github.com/hayone1/gitopsify/pkg/model"
)

// Resolver finds the repository a chart version was installed from. Helm
// does not record it, so it is looked up in the configuration, the local Helm
// repository cache and optionally Artifact Hub.
type Resolver struct {
	// Repositories maps chart names to repository URLs.
	Repositories map[string]string
	// Order of resolvers to try: "local", "artifacthub".
	Order []string
	// RepoConfig and RepoCache override Helm's repositories.yaml and cache
	// directory locations.
	RepoConfig string
	RepoCache  string
	// ArtifactHubURL overrides https://artifacthub.io (for tests).
	ArtifactHubURL string
	HTTPClient     *http.Client

	once  sync.Once
	local map[string][]localChart
	err   error
}

type localChart struct {
	repo     string
	versions map[string]bool
}

// Resolve returns where chart@version can be pulled from, or nil.
func (r *Resolver) Resolve(ctx context.Context, chart, version string) (*model.ChartRepo, []string, error) {
	if u, ok := r.Repositories[chart]; ok && u != "" {
		return &model.ChartRepo{URL: strings.TrimSuffix(u, "/"), Source: "config"}, nil, nil
	}
	var warnings []string
	for _, how := range r.Order {
		switch how {
		case "local":
			repo, err := r.resolveLocal(chart, version)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("reading the local Helm repository cache: %v", err))
				continue
			}
			if repo != nil {
				return repo, warnings, nil
			}
		case "artifacthub":
			repo, w, err := r.resolveArtifactHub(ctx, chart, version)
			warnings = append(warnings, w...)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("searching Artifact Hub for %s %s: %v", chart, version, err))
				continue
			}
			if repo != nil {
				return repo, warnings, nil
			}
		}
	}
	return nil, warnings, nil
}

func (r *Resolver) resolveLocal(chart, version string) (*model.ChartRepo, error) {
	r.once.Do(func() { r.local, r.err = loadLocalIndexes(r.repoConfig(), r.repoCache()) })
	if r.err != nil {
		return nil, r.err
	}
	var matches []string
	seen := map[string]bool{}
	for _, c := range r.local[chart] {
		if c.versions[version] && !seen[c.repo] {
			seen[c.repo] = true
			matches = append(matches, c.repo)
		}
	}
	if len(matches) == 0 {
		return nil, nil
	}
	sort.Strings(matches)
	src := "local Helm repository cache"
	if len(matches) > 1 {
		src += fmt.Sprintf(" (also in %s)", strings.Join(matches[1:], ", "))
	}
	return &model.ChartRepo{URL: matches[0], Source: src}, nil
}

func loadLocalIndexes(configPath, cacheDir string) (map[string][]localChart, error) {
	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return map[string][]localChart{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Repositories []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"repositories"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", configPath, err)
	}
	out := map[string][]localChart{}
	for _, repo := range cfg.Repositories {
		f, err := os.Open(filepath.Join(cacheDir, repo.Name+"-index.yaml"))
		if err != nil {
			continue
		}
		// Index files can be tens of megabytes; decode only what is needed.
		var idx struct {
			Entries map[string][]struct {
				Version string `yaml:"version"`
			} `yaml:"entries"`
		}
		err = yamlv3.NewDecoder(f).Decode(&idx)
		_ = f.Close()
		if err != nil {
			continue
		}
		for name, versions := range idx.Entries {
			lc := localChart{repo: strings.TrimSuffix(repo.URL, "/"), versions: map[string]bool{}}
			for _, v := range versions {
				lc.versions[v.Version] = true
			}
			out[name] = append(out[name], lc)
		}
	}
	return out, nil
}

func (r *Resolver) repoConfig() string {
	if r.RepoConfig != "" {
		return r.RepoConfig
	}
	if v := os.Getenv("HELM_REPOSITORY_CONFIG"); v != "" {
		return v
	}
	return filepath.Join(helmDir(configHome()), "repositories.yaml")
}

func (r *Resolver) repoCache() string {
	if r.RepoCache != "" {
		return r.RepoCache
	}
	if v := os.Getenv("HELM_REPOSITORY_CACHE"); v != "" {
		return v
	}
	return filepath.Join(helmDir(cacheHome()), "repository")
}

func helmDir(base string) string { return filepath.Join(base, "helm") }

// configHome and cacheHome follow the lookup Helm itself uses (helmpath).
func configHome() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Preferences")
	case "windows":
		return os.Getenv("APPDATA")
	}
	return filepath.Join(home, ".config")
}

func cacheHome() string {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Caches")
	case "windows":
		return os.Getenv("TEMP")
	}
	return filepath.Join(home, ".cache")
}

type ahPackage struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Repository struct {
		Name              string `json:"name"`
		URL               string `json:"url"`
		VerifiedPublisher bool   `json:"verified_publisher"`
		Official          bool   `json:"official"`
	} `json:"repository"`
}

func (r *Resolver) resolveArtifactHub(ctx context.Context, chart, version string) (*model.ChartRepo, []string, error) {
	base := r.ArtifactHubURL
	if base == "" {
		base = "https://artifacthub.io"
	}
	hc := r.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	q := url.Values{"ts_query_web": {chart}, "kind": {"0"}, "limit": {"60"}}
	var res struct {
		Packages []ahPackage `json:"packages"`
	}
	if err := getJSON(ctx, hc, base+"/api/v1/packages/search?"+q.Encode(), &res); err != nil {
		return nil, nil, err
	}
	var found []ahPackage
	for _, p := range res.Packages {
		if p.Name != chart {
			continue
		}
		ok, err := ahHasVersion(ctx, hc, base, p.Repository.Name, p.Name, version)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			found = append(found, p)
		}
	}
	if len(found) == 0 {
		return nil, nil, nil
	}
	sort.SliceStable(found, func(i, j int) bool {
		a, b := found[i].Repository, found[j].Repository
		if a.Official != b.Official {
			return a.Official
		}
		return a.VerifiedPublisher && !b.VerifiedPublisher
	})
	pick := found[0]
	var warnings []string
	if len(found) > 1 {
		var others []string
		for _, p := range found[1:] {
			others = append(others, p.Repository.URL)
		}
		warnings = append(warnings, fmt.Sprintf("chart %s %s exists in several Artifact Hub repositories; picked %s (others: %s). Set helm.repositories.%s to choose", chart, version, pick.Repository.URL, strings.Join(others, ", "), chart))
	}
	return &model.ChartRepo{URL: strings.TrimSuffix(pick.Repository.URL, "/"), Source: "Artifact Hub (" + pick.Repository.Name + ")"}, warnings, nil
}

func ahHasVersion(ctx context.Context, hc *http.Client, base, repo, chart, version string) (bool, error) {
	u := fmt.Sprintf("%s/api/v1/packages/helm/%s/%s/%s", base, url.PathEscape(repo), url.PathEscape(chart), url.PathEscape(version))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

func getJSON(ctx context.Context, hc *http.Client, u string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}
