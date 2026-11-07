package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/hayone1/gitopsify/providers"
)

// DefaultIndexURL is the published index. The copy built into the binary is
// used when it can't be fetched.
const DefaultIndexURL = "https://raw.githubusercontent.com/hayone1/gitopsify/main/providers/index.yaml"

// EnvIndex overrides the index location (a URL or a file), for private
// indexes.
const EnvIndex = "GITOPSIFY_PROVIDER_INDEX"

// Index lists the providers that can be installed by name.
type Index struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Providers  []IndexEntry `json:"providers"`
	// Location is where the index was read from.
	Location string `json:"-"`
}

// IndexEntry is one provider in the index.
type IndexEntry struct {
	Name        string   `json:"name"`
	Source      string   `json:"source"`
	Description string   `json:"description"`
	Homepage    string   `json:"homepage,omitempty"`
	Maturity    string   `json:"maturity,omitempty"`
	Outputs     []string `json:"outputs,omitempty"`
	Maintainers []string `json:"maintainers,omitempty"`
	// RepoID is the repository's numeric GitHub ID (gh api repos/OWNER/REPO
	// --jq .id), checked before installing.
	RepoID int64 `json:"repoId,omitempty"`
}

// ParseIndex parses and validates an index, refusing unknown fields (for the
// index in this repository and local files).
func ParseIndex(data []byte) (*Index, error) { return parseIndex(data, true) }

func parseIndex(data []byte, strict bool) (*Index, error) {
	var idx Index
	unmarshal := yaml.Unmarshal
	if strict {
		unmarshal = yaml.UnmarshalStrict
	}
	if err := unmarshal(data, &idx); err != nil {
		return nil, err
	}
	if idx.Kind != "ProviderIndex" {
		return nil, fmt.Errorf("kind is %q, want ProviderIndex", idx.Kind)
	}
	seen := map[string]bool{}
	for i, e := range idx.Providers {
		if !ValidName(e.Name) {
			return nil, fmt.Errorf("providers[%d]: %q isn't a provider name", i, e.Name)
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("providers[%d]: %s is listed twice", i, e.Name)
		}
		seen[e.Name] = true
		src, err := ParseSource(e.Source)
		if err != nil {
			return nil, fmt.Errorf("providers[%d] (%s): %w", i, e.Name, err)
		}
		if src.Name != e.Name {
			return nil, fmt.Errorf("providers[%d]: source %s is for %q, not %q", i, e.Source, src.Name, e.Name)
		}
		if e.Description == "" {
			return nil, fmt.Errorf("providers[%d] (%s) has no description", i, e.Name)
		}
	}
	return &idx, nil
}

// LoadIndex reads the index from location (a URL or a file). With an empty
// location it reads $GITOPSIFY_PROVIDER_INDEX, else the published index, and
// falls back to the built-in copy when that can't be fetched.
func LoadIndex(ctx context.Context, location string) (*Index, error) {
	explicit := location != ""
	if location == "" {
		location = os.Getenv(EnvIndex)
		explicit = location != ""
	}
	if location == "" {
		location = DefaultIndexURL
	}
	if strings.HasPrefix(location, "http://") {
		return nil, fmt.Errorf("the provider index %s must be served over https", location)
	}
	builtIn := func() (*Index, error) {
		idx, err := ParseIndex(providers.Index)
		if err != nil {
			return nil, err
		}
		idx.Location = "built-in"
		return idx, nil
	}
	data, err := fetch(ctx, location)
	if err != nil {
		if explicit {
			return nil, fmt.Errorf("reading the provider index %s: %w", location, err)
		}
		return builtIn()
	}
	// A newer index may have fields this version doesn't know: read it
	// leniently, and fall back to the built-in copy if it still fails.
	idx, err := parseIndex(data, false)
	if err != nil {
		if explicit {
			return nil, fmt.Errorf("provider index %s: %w", location, err)
		}
		return builtIn()
	}
	idx.Location = location
	return idx, nil
}

func fetch(ctx context.Context, location string) ([]byte, error) {
	if !strings.HasPrefix(location, "http://") && !strings.HasPrefix(location, "https://") {
		return os.ReadFile(location)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "gitopsify")
	// The repository may be private (raw.githubusercontent.com takes tokens).
	if strings.HasPrefix(location, "https://raw.githubusercontent.com/") {
		if t := firstEnv("GITHUB_TOKEN", "GH_TOKEN"); t != "" {
			req.Header.Set("Authorization", "token "+t)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// Lookup returns the entry for a provider name.
func (idx *Index) Lookup(name string) (*IndexEntry, bool) {
	for i := range idx.Providers {
		if idx.Providers[i].Name == name {
			return &idx.Providers[i], true
		}
	}
	return nil, false
}

// Search returns the entries whose name, description or source contain term
// (all of them for an empty term), by name.
func (idx *Index) Search(term string) []IndexEntry {
	term = strings.ToLower(strings.TrimSpace(term))
	var out []IndexEntry
	for _, e := range idx.Providers {
		hay := strings.ToLower(e.Name + " " + e.Description + " " + e.Source)
		if term == "" || strings.Contains(hay, term) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Resolve turns what someone typed ("grafana", "grafana@v0.2.0",
// "owner/grafana@v0.2.0") into a source and a version, looking short names up
// in the index.
func Resolve(ctx context.Context, ref, indexLocation string) (Source, string, error) {
	ref, version, _ := strings.Cut(ref, "@")
	if strings.Contains(ref, "/") {
		src, err := ParseSource(ref)
		return src, version, err
	}
	if !ValidName(ref) {
		return Source{}, "", fmt.Errorf("%q isn't a provider name or source", ref)
	}
	idx, err := LoadIndex(ctx, indexLocation)
	if err != nil {
		return Source{}, "", err
	}
	e, ok := idx.Lookup(ref)
	if !ok {
		return Source{}, "", fmt.Errorf("%s isn't in the provider index (%s); install it by source instead: gitopsify provider install <owner>/%s", ref, idx.Location, ref)
	}
	src, err := ParseSource(e.Source)
	src.RepoID = e.RepoID
	return src, version, err
}
