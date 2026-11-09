package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hayone1/gitopsify/pkg/secrets"
	"github.com/hayone1/gitopsify/pkg/yamlutil"
)

// Tree is an in-memory directory of generated files, keyed by slash path.
type Tree struct {
	files map[string][]byte
	// fingerprints are what encrypted files encrypt, by path.
	fingerprints map[string]string
	// SourceID identifies the source cluster (model.ClusterInfo.ID): Write
	// refuses, without Force, an output another cluster produced.
	SourceID string
	// roots are directories that are repositories of their own (one per
	// environment): each gets the manifest of its files, so a checkout of
	// that repository alone knows what an earlier run generated.
	roots []string
}

// NewTree returns an empty tree.
func NewTree() *Tree { return &Tree{files: map[string][]byte{}} }

// Add stores a file, replacing any previous content.
func (t *Tree) Add(p string, data []byte) { t.files[path.Clean(p)] = data }

// AddYAML stores one or more objects as a YAML stream.
func (t *Tree) AddYAML(p string, objs ...any) error {
	b, err := yamlutil.MarshalDocs(objs...)
	if err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	t.Add(p, b)
	return nil
}

// Get returns a file's content.
func (t *Tree) Get(p string) ([]byte, bool) {
	b, ok := t.files[path.Clean(p)]
	return b, ok
}

// Paths returns all file paths, sorted.
func (t *Tree) Paths() []string {
	out := make([]string, 0, len(t.files))
	for p := range t.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// The state of the previous generation, in the output (and in each
// per-environment repository, with paths relative to it):
//
//   - manifestFile lists the files gitopsify wrote, so a regeneration can
//     remove those it no longer produces without touching files people added;
//   - checksumsFile has the SHA-256 of each as written, so hand edits show
//     (git-ignored files with Secret values are left out: a hash of a
//     guessable value would give it away);
//   - encryptedFile has, for sealed and SOPS-encrypted Secrets, a fingerprint
//     of what was encrypted (never the values), so an unchanged Secret keeps
//     its ciphertext instead of a fresh one each run.
const (
	manifestFile  = ".gitopsify/generated.txt"
	checksumsFile = ".gitopsify/checksums.txt"
	encryptedFile = ".gitopsify/encrypted.txt"
	// sourceFile names the source cluster (kube-system's UID).
	sourceFile = ".gitopsify/source.txt"
)

// Merge policies for files already in the output directory.
const (
	MergeOverwrite = "overwrite"
	MergeKeep      = "keep"
	MergeFail      = "fail"
)

// WriteOptions say how Write treats what is already in the directory.
type WriteOptions struct {
	// Force writes into a non-empty directory gitopsify didn't generate,
	// and overwrites files there that it didn't write unless Unmanaged
	// says otherwise.
	Force bool
	// Edited is what happens to generated files changed by hand since
	// gitopsify wrote them: MergeOverwrite (default), MergeKeep or
	// MergeFail. Kept files are reported and stay as they are.
	Edited string
	// Unmanaged is what happens to a file gitopsify didn't write where it
	// writes one: MergeFail (default; MergeOverwrite with Force),
	// MergeOverwrite or MergeKeep.
	Unmanaged string
	// Keep are globs of paths gitopsify never writes or removes; * stays
	// within a directory, ** crosses them.
	Keep []string
	// DryRun works out what would happen without writing anything.
	DryRun bool
}

// WriteResult is what Write did, or would do with DryRun.
type WriteResult struct {
	// Written are the files written (unchanged ones included).
	Written []string
	// Changed are the files created, changed or removed, gitopsify's own
	// state files included, but not the git-ignored Secret files: what a
	// commit would hold. With a repository per environment, only the
	// files in those repositories count.
	Changed []string
	// Created are the written files that didn't exist.
	Created []string
	// Outside are files that changed outside the environments' repositories
	// (repo-per-env): not pushed, so not in Changed.
	Outside []string
	Removed []string
	// Edited are generated files changed by hand since gitopsify wrote
	// them; they are overwritten (or removed) unless they are Kept.
	Edited []string
	// Kept are files left as they were: hand-edited ones (Edited: keep),
	// files gitopsify didn't write (Unmanaged: keep) and Keep paths.
	Kept []string
	// Overwritten are files gitopsify didn't write that it replaced.
	Overwritten []string
	// Reused are encrypted Secrets kept as they were, since what they
	// encrypt didn't change.
	Reused []string
}

// ConflictError is a run stopped by a fail policy.
type ConflictError struct {
	Dir       string
	Edited    []string
	Unmanaged []string
}

func (e *ConflictError) Error() string {
	var parts []string
	if len(e.Edited) > 0 {
		parts = append(parts, fmt.Sprintf("%d generated files were changed by hand since gitopsify wrote them (%s); "+
			"set output.merge.edited (--edited) to overwrite or keep", len(e.Edited), listSome(e.Edited)))
	}
	if len(e.Unmanaged) > 0 {
		parts = append(parts, fmt.Sprintf("%d files gitopsify didn't write are where it writes (%s); "+
			"move them, or set output.merge.unmanaged (--unmanaged) to overwrite or keep, or keep them with output.merge.keep (--keep)",
			len(e.Unmanaged), listSome(e.Unmanaged)))
	}
	return e.Dir + ": nothing written: " + strings.Join(parts, "; ")
}

func listSome(list []string) string {
	if len(list) <= 5 {
		return strings.Join(list, ", ")
	}
	return strings.Join(list[:5], ", ") + fmt.Sprintf(" and %d more", len(list)-5)
}

// Fingerprint records what an encrypted file encrypts (never the values):
// when it is the same as last time, Write keeps the file's ciphertext.
func (t *Tree) Fingerprint(p, fp string) {
	if t.fingerprints == nil {
		t.fingerprints = map[string]string{}
	}
	t.fingerprints[path.Clean(p)] = fp
}

// prevState is what the previous generation left.
type prevState struct {
	found   bool
	files   map[string]bool
	sums    map[string]string
	encrypt map[string]string
}

func readState(root *os.Root, roots []string) prevState {
	st := prevState{files: map[string]bool{}, sums: map[string]string{}, encrypt: map[string]string{}}
	if root == nil {
		return st
	}
	// A repository's own state wins over the output's for its files: the
	// output's (outside every repository) may be older, from another
	// machine.
	var own []string
	inOwn := func(p string) bool {
		for _, r := range own {
			if strings.HasPrefix(p, r+"/") {
				return true
			}
		}
		return false
	}
	read := func(prefix string) bool {
		data, err := root.ReadFile(path.Join(prefix, manifestFile))
		if err != nil {
			return false
		}
		st.found = true
		for _, l := range strings.Split(string(data), "\n") {
			// The state comes from the output directory, which may be a
			// cloned repository: never let it name files outside of it.
			if l = strings.TrimSpace(l); l != "" && treePath(l) == nil {
				if p := path.Join(prefix, path.Clean(l)); prefix != "" || !inOwn(p) {
					st.files[p] = true
				}
			}
		}
		for file, m := range map[string]map[string]string{checksumsFile: st.sums, encryptedFile: st.encrypt} {
			data, _ := root.ReadFile(path.Join(prefix, file))
			for _, l := range strings.Split(string(data), "\n") {
				v, p, ok := strings.Cut(strings.TrimSpace(l), "  ")
				if ok && treePath(p) == nil {
					if p = path.Join(prefix, path.Clean(p)); prefix != "" || !inOwn(p) {
						m[p] = v
					}
				}
			}
		}
		return true
	}
	for _, r := range roots {
		if read(r) {
			own = append(own, r)
		}
	}
	read("")
	return st
}

// configFile is the configuration the output keeps: it is the input of the
// next run as much as an output, so editing it isn't a hand edit.
const configFile = "gitopsify.yaml"

// localSecret reports whether a path is a git-ignored file with Secret
// values (.secrets/): rewritten every run, never hashed.
func localSecret(p string) bool {
	return strings.HasPrefix(p, secrets.LocalDir+"/") || strings.Contains(p, "/"+secrets.LocalDir+"/")
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Write writes the tree under dir. Files the previous generation wrote and
// this one doesn't are removed. Without Force, Write refuses a non-empty
// directory that gitopsify didn't generate. What happens to files people
// changed or added is up to the options; with a fail policy, nothing is
// written.
//
// Paths come partly from the cluster (Helm release files): each must stay
// inside dir and out of .git, and everything goes through an os.Root, so a
// symlink in a cloned output directory can't lead elsewhere either.
func (t *Tree) Write(dir string, o WriteOptions) (*WriteResult, error) {
	paths := t.Paths()
	for _, p := range paths {
		if err := treePath(p); err != nil {
			return nil, err
		}
	}
	keep, err := compileGlobs(o.Keep)
	if err != nil {
		return nil, err
	}
	var root *os.Root
	if !o.DryRun {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	if r, err := os.OpenRoot(dir); err == nil {
		root = r
		defer root.Close()
	} else if !o.DryRun || !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	st := readState(root, t.roots)
	if t.SourceID != "" && !o.Force && root != nil {
		if prev, err := root.ReadFile(sourceFile); err == nil {
			if p := strings.TrimSpace(string(prev)); p != "" && p != t.SourceID {
				return nil, fmt.Errorf("%s was generated from another cluster (kube-system %s, this one is %s): check the context, or pass --force to replace it", dir, p, t.SourceID)
			}
		}
	}
	if !st.found && !o.Force && root != nil {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Name() != ".git" {
				return nil, fmt.Errorf("%s is not empty and was not generated by gitopsify; use --force to write into it", dir)
			}
		}
	}
	edited := firstSet(o.Edited, MergeOverwrite)
	unmanaged := o.Unmanaged
	if unmanaged == "" {
		unmanaged = MergeFail
		if o.Force {
			unmanaged = MergeOverwrite
		}
	}
	read := func(p string) ([]byte, bool) {
		if root == nil {
			return nil, false
		}
		b, err := root.ReadFile(p)
		return b, err == nil
	}

	res := &WriteResult{}
	conflict := &ConflictError{Dir: dir}
	type write struct {
		p    string
		data []byte
	}
	var writes []write
	owned := map[string]bool{} // the new manifest
	sums, encrypt := map[string]string{}, map[string]string{}
	for _, p := range paths {
		data := t.files[p]
		old, exists := read(p)
		if matchAny(keep, p) {
			if exists {
				res.Kept = append(res.Kept, p)
				if st.files[p] { // still gitopsify's, as it was
					owned[p] = true
					if s, ok := st.sums[p]; ok {
						sums[p] = s
					}
				}
			}
			continue
		}
		local := localSecret(p)
		if fp := t.fingerprints[p]; fp != "" && exists && st.encrypt[p] == fp && (st.sums[p] == "" || st.sums[p] == sum(old)) {
			data = old
			res.Reused = append(res.Reused, p)
		}
		switch {
		case !exists || local || bytes.Equal(old, data):
		case st.files[p]:
			if s, ok := st.sums[p]; ok && s != sum(old) && p != configFile {
				res.Edited = append(res.Edited, p)
				switch edited {
				case MergeKeep:
					res.Kept = append(res.Kept, p)
					owned[p], sums[p] = true, s // still edited next time
					continue
				case MergeFail:
					conflict.Edited = append(conflict.Edited, p)
					continue
				}
			}
		default: // a file gitopsify didn't write
			switch unmanaged {
			case MergeKeep:
				res.Kept = append(res.Kept, p)
				continue
			case MergeFail:
				conflict.Unmanaged = append(conflict.Unmanaged, p)
				continue
			}
			res.Overwritten = append(res.Overwritten, p)
		}
		owned[p] = true
		if !exists {
			res.Created = append(res.Created, p)
		}
		if !local {
			sums[p] = sum(data)
			if !exists || !bytes.Equal(old, data) {
				res.Changed = append(res.Changed, p)
			}
		}
		if fp := t.fingerprints[p]; fp != "" {
			encrypt[p] = fp
		}
		writes = append(writes, write{p, data})
	}

	current := map[string]bool{}
	for _, p := range paths {
		current[p] = true
	}
	var stale []string
	for p := range st.files {
		if !current[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(stale)
	var removals []string
	for _, p := range stale {
		old, exists := read(p)
		if !exists {
			continue
		}
		if matchAny(keep, p) {
			res.Kept = append(res.Kept, p)
			continue
		}
		if s, ok := st.sums[p]; ok && s != sum(old) && !localSecret(p) {
			res.Edited = append(res.Edited, p)
			switch edited {
			case MergeKeep:
				res.Kept = append(res.Kept, p) // yours from now on
				continue
			case MergeFail:
				conflict.Edited = append(conflict.Edited, p)
				continue
			}
		}
		removals = append(removals, p)
		if !localSecret(p) {
			res.Changed = append(res.Changed, p)
		}
	}
	if len(conflict.Edited)+len(conflict.Unmanaged) > 0 {
		return res, conflict
	}

	// gitopsify's state, for the output and for each repository in it.
	state := map[string][]byte{}
	addState := func(prefix string) {
		var files, sumLines, encLines []string
		for _, p := range sortedSet(owned) {
			rel := p
			if prefix != "" {
				var ok bool
				if rel, ok = strings.CutPrefix(p, prefix+"/"); !ok {
					continue
				}
			}
			files = append(files, rel)
			if s, ok := sums[p]; ok {
				sumLines = append(sumLines, s+"  "+rel)
			}
			if e, ok := encrypt[p]; ok {
				encLines = append(encLines, e+"  "+rel)
			}
		}
		state[path.Join(prefix, manifestFile)] = []byte(strings.Join(files, "\n") + "\n")
		state[path.Join(prefix, checksumsFile)] = []byte(strings.Join(sumLines, "\n") + "\n")
		if len(encLines) > 0 {
			state[path.Join(prefix, encryptedFile)] = []byte(strings.Join(encLines, "\n") + "\n")
		} else if _, ok := read(path.Join(prefix, encryptedFile)); ok {
			state[path.Join(prefix, encryptedFile)] = nil // no longer needed
		}
	}
	addState("")
	for _, r := range t.roots {
		addState(r)
	}
	if t.SourceID != "" {
		state[sourceFile] = []byte(t.SourceID + "\n")
		for _, r := range t.roots {
			state[path.Join(r, sourceFile)] = []byte(t.SourceID + "\n")
		}
	}
	for _, p := range sortedKeys(state) {
		if old, exists := read(p); !exists && state[p] != nil || exists && !bytes.Equal(old, state[p]) {
			res.Changed = append(res.Changed, p)
		}
	}
	if len(t.roots) > 0 {
		// Files outside the environments' repositories aren't pushed.
		var in []string
		for _, p := range res.Changed {
			inside := false
			for _, r := range t.roots {
				if strings.HasPrefix(p, r+"/") {
					inside = true
					break
				}
			}
			if inside {
				in = append(in, p)
			} else {
				res.Outside = append(res.Outside, p)
			}
		}
		res.Changed = in
	}
	sort.Strings(res.Changed)
	for _, w := range writes {
		res.Written = append(res.Written, w.p)
	}
	res.Removed = removals
	if o.DryRun {
		return res, nil
	}

	for _, w := range writes {
		if err := root.MkdirAll(path.Dir(w.p), 0o755); err != nil {
			return nil, err
		}
		mode := os.FileMode(0o644)
		switch {
		case localSecret(w.p):
			mode = 0o600
		case strings.HasSuffix(w.p, ".sh"):
			mode = 0o755
		}
		if err := root.WriteFile(w.p, w.data, mode); err != nil {
			return nil, err
		}
	}
	for _, p := range removals {
		if err := root.Remove(p); err == nil {
			removeEmptyParents(root, path.Dir(p))
		}
	}
	for _, p := range sortedKeys(state) {
		if state[p] == nil {
			_ = root.Remove(p)
			continue
		}
		if err := root.MkdirAll(path.Dir(p), 0o755); err != nil {
			return nil, err
		}
		if err := root.WriteFile(p, state[p], 0o644); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func firstSet(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// compileGlobs turns path globs into expressions: * and ? stay within a
// directory, ** crosses them, and a glob naming a directory covers what is
// in it.
func compileGlobs(globs []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, g := range globs {
		g = strings.Trim(path.Clean(strings.TrimSpace(g)), "/")
		if g == "" || g == "." || !filepath.IsLocal(filepath.FromSlash(g)) {
			return nil, fmt.Errorf("keep %q: want a path inside the output directory", g)
		}
		var b strings.Builder
		b.WriteString("^")
		for i := 0; i < len(g); i++ {
			switch c := g[i]; {
			case c == '*' && i+2 < len(g) && g[i+1] == '*' && g[i+2] == '/' && (i == 0 || g[i-1] == '/'):
				b.WriteString("(.*/)?") // **/ is any directories, or none
				i += 2
			case c == '*' && i+1 < len(g) && g[i+1] == '*':
				b.WriteString(".*")
				i++
			case c == '*':
				b.WriteString("[^/]*")
			case c == '?':
				b.WriteString("[^/]")
			default:
				b.WriteString(regexp.QuoteMeta(string(c)))
			}
		}
		b.WriteString("(/.*)?$")
		re, err := regexp.Compile(b.String())
		if err != nil {
			return nil, fmt.Errorf("keep %q: %w", g, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func matchAny(res []*regexp.Regexp, p string) bool {
	for _, re := range res {
		if re.MatchString(p) {
			return true
		}
	}
	return false
}

// treePath refuses paths that would leave the output directory or write
// into Git's own files.
func treePath(p string) error {
	if !filepath.IsLocal(filepath.FromSlash(p)) || strings.Contains(p, "\\") {
		return fmt.Errorf("refusing to write %q: it isn't a path inside the output directory", p)
	}
	for _, c := range strings.Split(p, "/") {
		if strings.EqualFold(c, ".git") {
			return fmt.Errorf("refusing to write %q: it is inside .git", p)
		}
	}
	return nil
}

// removeEmptyParents removes dir (slash path inside root) and its parents
// while they are empty.
func removeEmptyParents(root *os.Root, dir string) {
	for dir != "." && dir != "/" && dir != "" {
		f, err := root.Open(dir)
		if err != nil {
			return
		}
		names, err := f.Readdirnames(1)
		f.Close()
		if err == nil || len(names) > 0 {
			return // not empty (or unreadable)
		}
		if root.Remove(dir) != nil {
			return
		}
		dir = path.Dir(dir)
	}
}
