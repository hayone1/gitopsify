// Package provider finds, installs and runs gitopsify providers: executables
// named gitopsify-provider-<name> that read a tool's state and speak the
// protocol in github.com/hayone1/gitopsify/sdk.
package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// Prefix starts the name of every provider executable.
const Prefix = "gitopsify-provider-"

// EnvDir overrides the directory providers are installed into.
const EnvDir = "GITOPSIFY_PROVIDERS_DIR"

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$`)

// ValidName reports whether s can be a provider name.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Dir returns the directory providers are installed into:
// $GITOPSIFY_PROVIDERS_DIR, else $XDG_DATA_HOME/gitopsify/providers, else the
// platform's per-user data directory.
func Dir() (string, error) {
	if d := os.Getenv(EnvDir); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "gitopsify", "providers"), nil
	}
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "gitopsify", "providers"), nil
		}
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "gitopsify", "providers"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "gitopsify", "providers"), nil
}

// Origins of a provider executable.
const (
	OriginInstalled = "installed"
	OriginPath      = "PATH"
	OriginFile      = "file"
)

// Installed is a provider executable gitopsify can run.
type Installed struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Origin string `json:"origin"`
	// Source and Version are known for installed providers.
	Source  string `json:"source,omitempty"`
	Version string `json:"version,omitempty"`
	// SHA256 is the checksum of the release archive it was installed from.
	SHA256 string `json:"sha256,omitempty"`
	// Binary is the SHA-256 the executable had when it was installed.
	Binary string `json:"-"`
	// Checksums are the release's archive checksums by "<os>_<arch>".
	Checksums map[string]string `json:"checksums,omitempty"`
	// Shadowed lists other executables with the same name that are not used.
	Shadowed []string `json:"shadowed,omitempty"`
}

// Receipt is written next to an installed provider.
type Receipt struct {
	Source  string `json:"source"`
	Version string `json:"version"`
	Asset   string `json:"asset"`
	SHA256  string `json:"sha256"`
	// Binary is the SHA-256 of the installed executable.
	Binary string `json:"binary,omitempty"`
	// Checksums are the release's archive checksums, by "<os>_<arch>".
	Checksums map[string]string `json:"checksums,omitempty"`
}

const receiptFile = "install.json"

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return Prefix + name + ".exe"
	}
	return Prefix + name
}

// ErrNotFound means no provider with that name is installed or on PATH.
var ErrNotFound = errors.New("provider not found")

// IsPath reports whether ref names a file rather than a provider: it starts
// with ./, ../, / or ~, or is a Windows path.
func IsPath(ref string) bool {
	return strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "../") || strings.HasPrefix(ref, "/") ||
		strings.HasPrefix(ref, "~") || strings.HasPrefix(ref, `.\`) || filepath.IsAbs(ref) || filepath.VolumeName(ref) != ""
}

// Find returns the provider to run for ref: a path to an executable, a name
// (grafana), or a source (owner/grafana), whose last part is the name.
// Installed providers come before those on PATH.
func Find(ref string) (*Installed, error) {
	if IsPath(ref) {
		p := ref
		if strings.HasPrefix(p, "~") {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			p = filepath.Join(home, p[1:])
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		if fi, err := os.Stat(abs); err != nil || fi.IsDir() {
			return nil, fmt.Errorf("%s: no such provider executable", ref)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(abs), Prefix), ".exe")
		return &Installed{Name: name, Path: abs, Origin: OriginFile}, nil
	}
	name := NameOf(ref)
	if !ValidName(name) {
		return nil, fmt.Errorf("%q isn't a provider name (lowercase letters, digits and dashes)", ref)
	}
	var found []*Installed
	if dir, err := Dir(); err == nil {
		if in, ok := readInstalled(dir, name); ok {
			found = append(found, in)
		}
	}
	if p, err := exec.LookPath(exeName(name)); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		found = append(found, &Installed{Name: name, Path: p, Origin: OriginPath})
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	use := found[0]
	for _, f := range found[1:] {
		use.Shadowed = append(use.Shadowed, f.Path)
	}
	return use, nil
}

// NameOf returns the provider name in a source: "owner/grafana",
// "owner/gitopsify-provider-grafana", "github.com/owner/..." and "grafana"
// all give "grafana". A version suffix (@v1.2.3) is dropped.
func NameOf(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	return strings.TrimPrefix(ref, Prefix)
}

func readInstalled(dir, name string) (*Installed, bool) {
	p := filepath.Join(dir, name, exeName(name))
	if fi, err := os.Stat(p); err != nil || fi.IsDir() {
		return nil, false
	}
	in := &Installed{Name: name, Path: p, Origin: OriginInstalled}
	if b, err := os.ReadFile(filepath.Join(dir, name, receiptFile)); err == nil {
		var r Receipt
		if json.Unmarshal(b, &r) == nil {
			in.Source, in.Version, in.SHA256, in.Binary, in.Checksums = r.Source, r.Version, r.SHA256, r.Binary, r.Checksums
		}
	}
	return in, true
}

// List returns every provider gitopsify can run: installed ones and
// gitopsify-provider-* executables on PATH, by name.
func List() ([]*Installed, error) {
	byName := map[string]*Installed{}
	var order []string
	add := func(in *Installed) {
		if prev, ok := byName[in.Name]; ok {
			if prev.Path != in.Path {
				prev.Shadowed = append(prev.Shadowed, in.Path)
			}
			return
		}
		byName[in.Name] = in
		order = append(order, in.Name)
	}
	if dir, err := Dir(); err == nil {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() {
				if in, ok := readInstalled(dir, e.Name()); ok {
					add(in)
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			n := e.Name()
			if !strings.HasPrefix(n, Prefix) || e.IsDir() {
				continue
			}
			if runtime.GOOS == "windows" {
				if !strings.EqualFold(filepath.Ext(n), ".exe") {
					continue
				}
				n = strings.TrimSuffix(n, filepath.Ext(n))
			}
			name := strings.TrimPrefix(n, Prefix)
			p := filepath.Join(d, e.Name())
			if !ValidName(name) || seen[p] || !executable(p) {
				continue
			}
			seen[p] = true
			add(&Installed{Name: name, Path: p, Origin: OriginPath})
		}
	}
	sort.Strings(order)
	out := make([]*Installed, 0, len(order))
	for _, n := range order {
		out = append(out, byName[n])
	}
	return out, nil
}

func executable(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || fi.Mode()&0o111 != 0
}

// Verify checks an installed provider against pinned archive checksums
// (by "<os>_<arch>") and against the hash its executable had when installed.
func (in *Installed) Verify(pins map[string]string) error {
	if in.Origin != OriginInstalled {
		return nil
	}
	if len(pins) > 0 {
		key := runtime.GOOS + "_" + runtime.GOARCH
		pin, ok := pins[key]
		switch {
		case !ok:
			return fmt.Errorf("checksums are pinned, but not for %s", key)
		case in.SHA256 == "":
			return fmt.Errorf("%s was installed without a checksum; reinstall it", in.Name)
		case !strings.EqualFold(pin, in.SHA256):
			return fmt.Errorf("the installed %s %s came from an archive with checksum %s, not the pinned %s", in.Name, in.Version, in.SHA256, pin)
		}
	}
	if in.Binary != "" {
		b, err := os.ReadFile(in.Path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != in.Binary {
			return fmt.Errorf("%s changed since it was installed; reinstall it", in.Path)
		}
	}
	return nil
}

// Remove deletes an installed provider.
func Remove(name string) error {
	if !ValidName(name) {
		return fmt.Errorf("%q isn't a provider name", name)
	}
	dir, err := Dir()
	if err != nil {
		return err
	}
	if _, ok := readInstalled(dir, name); !ok {
		return fmt.Errorf("%w: %s isn't installed in %s", ErrNotFound, name, dir)
	}
	return os.RemoveAll(filepath.Join(dir, name))
}
