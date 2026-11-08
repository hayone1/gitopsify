package iac

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// ownedAfter returns, for one format, the files every unit will own after
// writing: what this export produced, and what earlier exports wrote for
// units this one didn't touch.
func (r *Result) ownedAfter(format string) map[string]map[string][]string {
	out := map[string]map[string][]string{}
	for ws, kinds := range r.previous.Files[format] {
		for kind, paths := range kinds {
			if _, produced := r.units[unitKey{format, ws, kind}]; produced {
				continue
			}
			if out[ws] == nil {
				out[ws] = map[string][]string{}
			}
			out[ws][kind] = append([]string(nil), paths...)
		}
	}
	for k, u := range r.units {
		if k.format != format {
			continue
		}
		if out[k.ws] == nil {
			out[k.ws] = map[string][]string{}
		}
		out[k.ws][k.kind] = sortedFiles(u.files)
	}
	return out
}

func sortedFiles(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Files returns every file this export writes, by path.
func (r *Result) Files() map[string][]byte {
	out := map[string][]byte{}
	for _, u := range r.units {
		for p, b := range u.files {
			out[p] = b
		}
	}
	return out
}

// HandEdited returns the files this export would overwrite or remove that
// were changed since the last export wrote them.
func (r *Result) HandEdited(dir string) []string {
	var out []string
	seen := map[string]bool{}
	check := func(p string) {
		want, ok := r.previous.Hashes[p]
		if seen[p] || !ok || !local(p) || p == ".gitignore" {
			return
		}
		seen[p] = true
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			return
		}
		if hashOf(b) != want {
			out = append(out, p)
		}
	}
	for k, u := range r.units {
		for p := range u.files {
			check(p)
		}
		for _, p := range r.previous.owned(k.format, k.ws, k.kind) {
			check(p)
		}
	}
	sort.Strings(out)
	return out
}

func hashOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Write writes the export under dir: the files of every unit it produced,
// removing what those units wrote last time and no longer write. Units it
// didn't produce (other formats, workspaces or kinds out of scope, scopes
// that weren't read completely) are left as they are. Without force, Write
// refuses a non-empty directory that holds no earlier export. Every write and
// removal stays inside dir, also through symbolic links.
func (r *Result) Write(dir string, force bool) (written, removed []string, err error) {
	if err := checkDir(dir, force); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()

	for k := range r.units {
		r.State.setOwned(k.format, k.ws, k.kind, nil)
	}
	keys := make([]unitKey, 0, len(r.units))
	for k := range r.units {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.format != b.format {
			return a.format < b.format
		}
		if a.ws != b.ws {
			return a.ws < b.ws
		}
		return a.kind < b.kind
	})
	current := map[string]bool{}
	for _, u := range r.units {
		for p := range u.files {
			current[p] = true
		}
	}
	if r.State.Hashes == nil {
		r.State.Hashes = map[string]string{}
	}
	for _, k := range keys {
		u := r.units[k]
		paths := sortedFiles(u.files)
		for _, p := range paths {
			if !local(p) {
				return written, removed, fmt.Errorf("refusing to write %q outside the output directory", p)
			}
			data := u.files[p]
			if p == ".gitignore" {
				data = mergeGitignore(root, data)
			}
			if u.secret[p] {
				// A link anywhere on the way could send secrets into a
				// tracked directory.
				if err := noLinks(root, p); err != nil {
					return written, removed, err
				}
			}
			if err := root.MkdirAll(path.Dir(p), 0o755); err != nil {
				return written, removed, err
			}
			mode := os.FileMode(0o644)
			if u.secret[p] {
				mode = 0o600
			}
			if err := root.WriteFile(p, data, mode); err != nil {
				return written, removed, err
			}
			if u.secret[p] {
				if err := root.Chmod(p, 0o600); err != nil {
					return written, removed, err
				}
			}
			if u.secret[p] {
				// No hash of a secret file in the tracked state: it would let
				// anyone who can read the repository check guesses of the
				// secrets offline. Secret files are rewritten from the tool on
				// every export, so they need no hand-edit detection.
				delete(r.State.Hashes, p)
			} else {
				r.State.Hashes[p] = hashOf(data)
			}
			written = append(written, p)
		}
		if k.format != "" {
			r.State.setOwned(k.format, k.ws, k.kind, paths)
		}
		for _, p := range r.previous.owned(k.format, k.ws, k.kind) {
			if current[p] || !local(p) {
				continue
			}
			if err := root.Remove(p); err == nil {
				removed = append(removed, p)
				delete(r.State.Hashes, p)
				removeEmptyParents(root, path.Dir(p))
			}
		}
	}
	b, err := json.MarshalIndent(r.State, "", "  ")
	if err != nil {
		return written, removed, err
	}
	if err := root.MkdirAll(path.Dir(StateFile), 0o755); err != nil {
		return written, removed, err
	}
	return written, removed, root.WriteFile(StateFile, append(b, '\n'), 0o644)
}

// noLinks fails when any part of p is a symbolic link.
func noLinks(root *os.Root, p string) error {
	parts := strings.Split(p, "/")
	for i := range parts {
		fi, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return nil // doesn't exist yet
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write secrets through the symbolic link %s", strings.Join(parts[:i+1], "/"))
		}
	}
	return nil
}

// gitignoreHeader starts the .gitignore gitopsify writes; a .gitignore
// without it belongs to someone else, and gets the missing lines appended.
const gitignoreHeader = "# Written by gitopsify export."

func mergeGitignore(root *os.Root, ours []byte) []byte {
	old, err := root.ReadFile(".gitignore")
	if err != nil || strings.HasPrefix(string(old), gitignoreHeader) {
		return ours
	}
	have := map[string]bool{}
	for _, l := range strings.Split(string(old), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var add []string
	for _, l := range strings.Split(string(ours), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") && !have[l] {
			add = append(add, l)
		}
	}
	if len(add) == 0 {
		return old
	}
	out := strings.TrimRight(string(old), "\n") + "\n\n# Added by gitopsify export.\n" + strings.Join(add, "\n") + "\n"
	return []byte(out)
}

// local reports whether p is a clean relative path inside the output.
func local(p string) bool {
	return p != "" && path.Clean(p) == p && filepath.IsLocal(filepath.FromSlash(p)) && !strings.HasPrefix(p, ".gitopsify/")
}

func checkDir(dir string, force bool) error {
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(StateFile))); err == nil {
		return nil
	}
	if force {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitopsify", "generated.txt")); err == nil {
		return fmt.Errorf("%s holds a cluster repository written by gitopsify generate; export into another directory, or use --force", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // doesn't exist yet
	}
	for _, e := range entries {
		if e.Name() != ".git" {
			return fmt.Errorf("%s is not empty and holds no earlier export; use --force to write into it", dir)
		}
	}
	return nil
}

func removeEmptyParents(root *os.Root, dir string) {
	for dir != "." && dir != "/" && dir != "" {
		f, err := root.Open(dir)
		if err != nil {
			return
		}
		entries, err := f.ReadDir(1)
		f.Close()
		if err != nil || len(entries) > 0 {
			return
		}
		if root.Remove(dir) != nil {
			return
		}
		dir = path.Dir(dir)
	}
}
