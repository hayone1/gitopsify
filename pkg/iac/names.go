package iac

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// StateFile records what an export wrote, so the next export keeps names
// stable and knows which files it owns.
const StateFile = ".gitopsify/export.json"

// State is the content of StateFile.
type State struct {
	APIVersion string `json:"apiVersion"`
	Provider   string `json:"provider"`
	// Workspaces by ID (<instance>/<partition ID>, or <instance>/ for
	// instance-wide objects).
	Workspaces map[string]*WorkspaceState `json:"workspaces"`
	// Resources by <instance>/<key>.
	Resources map[string]*ResourceState `json:"resources"`
	// Files owned by each output unit: format → workspace ID → kind ("" for
	// the workspace's own files) → paths relative to the output directory.
	Files map[string]map[string]map[string][]string `json:"files"`
	// Hashes holds the SHA-256 of every file the export wrote, except the
	// git-ignored secret files, to tell hand edits apart (and, later, to
	// merge them).
	Hashes map[string]string `json:"hashes,omitempty"`
	// Retired holds old addresses of objects that changed kind, partition or
	// Terraform type: they keep their removed blocks and reserved names.
	Retired []*ResourceState `json:"retired,omitempty"`
}

// WorkspaceState is where a workspace is written.
type WorkspaceState struct {
	Dir       string `json:"dir"`
	Namespace string `json:"namespace,omitempty"`
}

// ResourceState is how a resource was named.
type ResourceState struct {
	Kind      string `json:"kind"`
	Workspace string `json:"workspace"`
	// Terraform is the resource's address (type.name).
	Terraform string `json:"terraform,omitempty"`
	// Crossplane is the managed resource's name.
	Crossplane string `json:"crossplane,omitempty"`
	// Files maps the provider's file paths to the paths written, relative to
	// the workspace directory.
	Files map[string]string `json:"files,omitempty"`
	// Gone marks a resource that was exported before and isn't any more; its
	// removed block is kept so it is never destroyed.
	Gone bool `json:"gone,omitempty"`
	// Secrets holds the Kubernetes Secret names its secrets were written to:
	// "" for the Secret of its string values, an attribute path for a map.
	Secrets map[string]string `json:"secrets,omitempty"`
}

// NewState returns an empty state.
func NewState(provider string) *State {
	return &State{APIVersion: "gitopsify.io/v1alpha1", Provider: provider, Workspaces: map[string]*WorkspaceState{},
		Resources: map[string]*ResourceState{}, Files: map[string]map[string]map[string][]string{}}
}

// LoadState reads the state of the export in dir; it returns nil without an
// error when there is none.
func LoadState(dir string) (*State, error) {
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(StateFile)))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s := &State{}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("%s: %w", StateFile, err)
	}
	if s.Workspaces == nil {
		s.Workspaces = map[string]*WorkspaceState{}
	}
	if s.Resources == nil {
		s.Resources = map[string]*ResourceState{}
	}
	if s.Files == nil {
		s.Files = map[string]map[string]map[string][]string{}
	}
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w (it is written by gitopsify; restore it from Git or delete it to start over)", StateFile, err)
	}
	return s, nil
}

var (
	stateAddr = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[A-Za-z_][A-Za-z0-9_-]*$`)
	stateName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	stateDir  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?/[_a-z0-9][-_a-z0-9]*$`)
	statePath = regexp.MustCompile(`^[-_a-z0-9.]+(/[-_a-z0-9.]+)*$`)
)

// validate rejects names and paths gitopsify wouldn't have written: they end
// up in HCL and file paths.
func (s *State) validate() error {
	for id, w := range s.Workspaces {
		if !stateDir.MatchString(w.Dir) || (w.Namespace != "" && !stateName.MatchString(w.Namespace)) {
			return fmt.Errorf("workspace %s has an invalid directory or namespace", id)
		}
	}
	all := map[string]*ResourceState{}
	for k, r := range s.Resources {
		all[k] = r
	}
	for i, r := range s.Retired {
		all[fmt.Sprintf("retired[%d]", i)] = r
	}
	for k, r := range all {
		for _, n := range r.Secrets {
			if !stateName.MatchString(n) {
				return fmt.Errorf("resource %s has an invalid Secret name %q", k, n)
			}
		}
		if r.Terraform != "" && !stateAddr.MatchString(r.Terraform) {
			return fmt.Errorf("resource %s has an invalid Terraform address %q", k, r.Terraform)
		}
		if r.Crossplane != "" && !stateName.MatchString(r.Crossplane) {
			return fmt.Errorf("resource %s has an invalid name %q", k, r.Crossplane)
		}
		for _, f := range r.Files {
			if !statePath.MatchString(f) || !local(f) {
				return fmt.Errorf("resource %s has an invalid file %q", k, f)
			}
		}
	}
	for _, wss := range s.Files {
		for _, kinds := range wss {
			for _, paths := range kinds {
				for _, p := range paths {
					if !local(p) {
						return fmt.Errorf("invalid file %q", p)
					}
				}
			}
		}
	}
	return nil
}

func (s *State) owned(format, ws, kind string) []string {
	return s.Files[format][ws][kind]
}

func (s *State) setOwned(format, ws, kind string, paths []string) {
	if s.Files[format] == nil {
		s.Files[format] = map[string]map[string][]string{}
	}
	if s.Files[format][ws] == nil {
		s.Files[format][ws] = map[string][]string{}
	}
	if len(paths) == 0 {
		delete(s.Files[format][ws], kind)
		return
	}
	s.Files[format][ws][kind] = paths
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// slug lowercases s and joins its runs of letters and digits with sep; letters
// outside ASCII are dropped (accents are folded where simple).
func slug(s string, sep string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r < unicode.MaxASCII:
			b.WriteRune(r)
		default:
			if f, ok := fold[r]; ok {
				b.WriteString(f)
			} else {
				b.WriteByte(' ')
			}
		}
	}
	return strings.Trim(nonAlnum.ReplaceAllString(b.String(), sep), sep)
}

var fold = map[rune]string{'à': "a", 'á': "a", 'â': "a", 'ä': "a", 'ã': "a", 'å': "a", 'ç': "c", 'è': "e", 'é': "e", 'ê': "e", 'ë': "e",
	'ì': "i", 'í': "i", 'î': "i", 'ï': "i", 'ñ': "n", 'ò': "o", 'ó': "o", 'ô': "o", 'ö': "o", 'õ': "o", 'ø': "o", 'ù': "u", 'ú': "u",
	'û': "u", 'ü': "u", 'ý': "y", 'ÿ': "y", 'ß': "ss", 'æ': "ae", 'œ': "oe"}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:6]
}

// truncate shortens s to max bytes, keeping it unique with a hash of the
// whole.
func truncate(s string, max int, sep string) string {
	if len(s) <= max {
		return s
	}
	h := shortHash(s)
	return strings.TrimRight(s[:max-len(h)-len(sep)], sep+"_-") + sep + h
}

// tfBase is the Terraform identifier for a display name.
func tfBase(name, key string) string {
	s := truncate(slug(name, "_"), 64, "_")
	if s == "" {
		return "r_" + shortHash(key)
	}
	if s[0] >= '0' && s[0] <= '9' {
		s = "_" + s
	}
	return s
}

// k8sBase is the Kubernetes name for a display name (a DNS label).
func k8sBase(name, key string) string {
	s := truncate(slug(name, "-"), 63, "-")
	if s == "" {
		return "r-" + shortHash(key)
	}
	return s
}

// safeExt is a file extension that is safe in HCL strings and file names.
var safeExt = regexp.MustCompile(`^\.[a-z0-9]{1,10}$`)

var windowsReserved = regexp.MustCompile(`^(con|prn|aux|nul|com[0-9]|lpt[0-9])$`)

// fileSegment is a safe file or directory name for a display name.
func fileSegment(name, fallback string) string {
	s := truncate(slug(name, "-"), 80, "-")
	if s == "" {
		s = fallback
	}
	if windowsReserved.MatchString(s) {
		s += "-1"
	}
	return s
}

// safePath turns a provider's file path into a safe relative path: every
// directory is re-slugged and the extension kept.
func safePath(p, key string) string {
	parts := strings.Split(path.Clean(strings.ReplaceAll(p, `\`, "/")), "/")
	var out []string
	for i, seg := range parts {
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		if i == len(parts)-1 {
			ext := strings.ToLower(path.Ext(seg))
			base := strings.TrimSuffix(seg, path.Ext(seg))
			// Terraform loads .tf, .tf.json and .tfvars files next to the
			// configuration: a provider's file can't be one.
			lower := strings.ToLower(seg)
			tf := strings.HasSuffix(lower, ".tf") || strings.HasSuffix(lower, ".tf.json") || strings.Contains(lower, ".tfvars")
			if !safeExt.MatchString(ext) || tf {
				ext, base = "", seg
			}
			out = append(out, fileSegment(base, "file-"+shortHash(key))+ext)
			continue
		}
		out = append(out, fileSegment(seg, "dir"))
	}
	if len(out) == 0 {
		return "file-" + shortHash(key)
	}
	return strings.Join(out, "/")
}

// namer hands out unique names within one scope, case-insensitively.
type namer struct {
	used map[string]bool
	sep  string
}

func newNamer(sep string) *namer { return &namer{used: map[string]bool{}, sep: sep} }

func (n *namer) reserve(name string) { n.used[strings.ToLower(name)] = true }

func (n *namer) taken(name string) bool { return n.used[strings.ToLower(name)] }

// assign returns base, or base with a hash of key when base is taken.
func (n *namer) assign(base, key string) string {
	name := base
	if n.taken(name) {
		name = base + n.sep + shortHash(key)
		for i := 2; n.taken(name); i++ {
			name = fmt.Sprintf("%s%s%s%d", base, n.sep, shortHash(key), i)
		}
	}
	n.reserve(name)
	return name
}
