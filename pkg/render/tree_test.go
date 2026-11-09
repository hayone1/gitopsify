package render

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Paths partly come from the cluster (Helm release files): none may leave
// the output directory or touch .git, even through a symlink in it.
func TestTreeWritePaths(t *testing.T) {
	for _, p := range []string{"../escape.yaml", "apps/x/charts/c/templates/../../../../../../escape.sh",
		".git/config", "apps/.GIT/hooks/pre-commit", "/etc/escape", `apps\..\..\escape`} {
		dir := t.TempDir()
		tr := NewTree()
		tr.Add("README.md", []byte("ok"))
		tr.Add(p, []byte("x"))
		if _, err := tr.Write(dir, WriteOptions{}); err == nil {
			t.Errorf("%q was written", p)
		}
		if _, err := os.Stat(filepath.Join(dir, "README.md")); err == nil {
			t.Errorf("%q: the tree was partly written before the refusal", p)
		}
	}

	// A cloned output directory with a symlink pointing outside.
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "apps")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	tr := NewTree()
	tr.Add("apps/web/deployment.yaml", []byte("kind: Deployment\n"))
	if _, err := tr.Write(dir, WriteOptions{Force: true}); err == nil {
		t.Error("wrote through a symlink")
	}
	if entries, _ := os.ReadDir(outside); len(entries) > 0 {
		t.Errorf("files outside the output directory: %v", entries)
	}

	// A manifest naming files outside isn't followed when removing.
	dir = t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	_ = os.WriteFile(victim, []byte("keep"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, ".gitopsify"), 0o755)
	rel, _ := filepath.Rel(dir, victim)
	_ = os.WriteFile(filepath.Join(dir, manifestFile), []byte(filepath.ToSlash(rel)+"\n.git/config\n"), 0o644)
	tr = NewTree()
	tr.Add("README.md", []byte("ok"))
	if wr, err := tr.Write(dir, WriteOptions{}); err != nil || len(wr.Removed) > 0 {
		t.Errorf("removed %v (%v)", wr, err)
	}
	if b, _ := os.ReadFile(victim); !strings.Contains(string(b), "keep") {
		t.Error("a file outside the output directory was removed")
	}
}

// Regenerating removes the files the previous run wrote and this one
// doesn't, and the directories they leave empty.
func TestTreeWriteRemovesStale(t *testing.T) {
	dir := t.TempDir()
	tr := NewTree()
	tr.Add("apps/a/x.yaml", []byte("a"))
	tr.Add("apps/b/y.yaml", []byte("b"))
	if _, err := tr.Write(dir, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	tr = NewTree()
	tr.Add("apps/a/x.yaml", []byte("a2"))
	wr, err := tr.Write(dir, WriteOptions{})
	if err != nil || len(wr.Removed) != 1 || wr.Removed[0] != "apps/b/y.yaml" {
		t.Fatalf("removed %v (%v)", wr, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "apps", "b")); !os.IsNotExist(err) {
		t.Error("the emptied directory stayed")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "apps", "a", "x.yaml")); string(b) != "a2" {
		t.Errorf("x.yaml: %q", b)
	}
}

// A repository per environment keeps the list of its files: a checkout of
// that repository alone (CI, the server) removes what a later run doesn't
// generate.
func TestTreeWriteRootManifests(t *testing.T) {
	dir := t.TempDir()
	tr := NewTree()
	tr.roots = []string{"uat"}
	tr.Add("uat/apps/a/x.yaml", []byte("a"))
	tr.Add("uat/apps/b/y.yaml", []byte("b"))
	if _, err := tr.Write(dir, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	// Only the environment's repository is there now (a fresh checkout).
	fresh := t.TempDir()
	if err := os.CopyFS(filepath.Join(fresh, "uat"), os.DirFS(filepath.Join(dir, "uat"))); err != nil {
		t.Fatal(err)
	}
	tr = NewTree()
	tr.roots = []string{"uat"}
	tr.Add("uat/apps/a/x.yaml", []byte("a"))
	wr, err := tr.Write(fresh, WriteOptions{})
	if err != nil || len(wr.Removed) != 1 || wr.Removed[0] != "uat/apps/b/y.yaml" {
		t.Fatalf("removed %v (%v)", wr, err)
	}
}

// TestTreeWriteMerge: hand edits to generated files, files gitopsify didn't
// write, and keep paths, under each policy.
func TestTreeWriteMerge(t *testing.T) {
	gen := func(files map[string]string) *Tree {
		tr := NewTree()
		for p, c := range files {
			tr.Add(p, []byte(c))
		}
		return tr
	}
	read := func(dir, p string) string {
		b, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		return string(b)
	}
	setup := func(t *testing.T) string {
		dir := t.TempDir()
		if _, err := gen(map[string]string{"apps/a.yaml": "a1", "apps/b.yaml": "b1", "apps/c.yaml": "c1", ".secrets/s.yaml": "pw"}).Write(dir, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(dir, "apps/a.yaml"), []byte("mine"), 0o644)   // edited
		_ = os.WriteFile(filepath.Join(dir, "apps/c.yaml"), []byte("mine c"), 0o644) // edited, then no longer generated
		_ = os.WriteFile(filepath.Join(dir, "apps/d.yaml"), []byte("theirs"), 0o644) // not gitopsify's
		return dir
	}
	next := map[string]string{"apps/a.yaml": "a2", "apps/b.yaml": "b2", "apps/d.yaml": "d2", ".secrets/s.yaml": "pw2"}

	t.Run("defaults", func(t *testing.T) {
		dir := setup(t)
		_, err := gen(next).Write(dir, WriteOptions{})
		var ce *ConflictError
		if !errors.As(err, &ce) || len(ce.Unmanaged) != 1 || ce.Unmanaged[0] != "apps/d.yaml" || len(ce.Edited) != 0 {
			t.Fatalf("want a conflict on apps/d.yaml: %v", err)
		}
		if read(dir, "apps/b.yaml") != "b1" {
			t.Error("a conflict wrote files")
		}
		// --force overwrites the unmanaged file; edits are overwritten with a report.
		wr, err := gen(next).Write(dir, WriteOptions{Force: true})
		if err != nil {
			t.Fatal(err)
		}
		if read(dir, "apps/a.yaml") != "a2" || read(dir, "apps/d.yaml") != "d2" {
			t.Errorf("files: a=%q d=%q", read(dir, "apps/a.yaml"), read(dir, "apps/d.yaml"))
		}
		if !reflect.DeepEqual(wr.Edited, []string{"apps/a.yaml", "apps/c.yaml"}) || !reflect.DeepEqual(wr.Overwritten, []string{"apps/d.yaml"}) ||
			!reflect.DeepEqual(wr.Removed, []string{"apps/c.yaml"}) {
			t.Errorf("result: %+v", wr)
		}
		for _, p := range wr.Changed {
			if strings.Contains(p, ".secrets") {
				t.Errorf("git-ignored secret file counted as a change: %v", wr.Changed)
			}
		}
		if sums := read(dir, checksumsFile); strings.Contains(sums, ".secrets") || !strings.Contains(sums, "apps/d.yaml") {
			t.Errorf("checksums: %s", sums)
		}
	})

	t.Run("keep", func(t *testing.T) {
		dir := setup(t)
		wr, err := gen(next).Write(dir, WriteOptions{Edited: MergeKeep, Unmanaged: MergeKeep})
		if err != nil {
			t.Fatal(err)
		}
		if read(dir, "apps/a.yaml") != "mine" || read(dir, "apps/c.yaml") != "mine c" || read(dir, "apps/d.yaml") != "theirs" || read(dir, "apps/b.yaml") != "b2" {
			t.Errorf("files: %v", wr)
		}
		// Still edited next time; c and d are the user's now.
		wr, err = gen(next).Write(dir, WriteOptions{Edited: MergeKeep, Unmanaged: MergeKeep})
		if err != nil || !reflect.DeepEqual(wr.Edited, []string{"apps/a.yaml"}) || len(wr.Changed) != 0 {
			t.Errorf("second run: %+v %v", wr, err)
		}
		if strings.Contains(read(dir, manifestFile), "apps/c.yaml") {
			t.Error("a kept, no longer generated file stayed gitopsify's")
		}
	})

	t.Run("fail", func(t *testing.T) {
		dir := setup(t)
		_, err := gen(next).Write(dir, WriteOptions{Edited: MergeFail, Unmanaged: MergeOverwrite})
		var ce *ConflictError
		if !errors.As(err, &ce) || !reflect.DeepEqual(ce.Edited, []string{"apps/a.yaml", "apps/c.yaml"}) {
			t.Fatalf("want edit conflicts: %v", err)
		}
		if !strings.Contains(err.Error(), "--edited") {
			t.Errorf("message: %v", err)
		}
	})

	t.Run("keep globs", func(t *testing.T) {
		dir := setup(t)
		wr, err := gen(next).Write(dir, WriteOptions{Keep: []string{"apps/*.yaml"}})
		if err != nil {
			t.Fatal(err)
		}
		if read(dir, "apps/b.yaml") != "b1" || read(dir, "apps/a.yaml") != "mine" || len(wr.Removed) != 0 {
			t.Errorf("keep paths were touched: %+v", wr)
		}
		if _, err := compileGlobs([]string{"../x"}); err == nil {
			t.Error("a glob outside the output was accepted")
		}
		res, _ := compileGlobs([]string{"apps/**/prod", "*.md", "**/values.yaml"})
		for p, want := range map[string]bool{"apps/web/envs/prod/k.yaml": true, "apps/prod": true, "apps/production": false,
			"README.md": true, "apps/README.md": false, "values.yaml": true, "apps/web/base/values.yaml": true} {
			if matchAny(res, p) != want {
				t.Errorf("%s: %v", p, !want)
			}
		}
	})

	t.Run("dry run", func(t *testing.T) {
		dir := setup(t)
		before := read(dir, "apps/b.yaml")
		wr, err := gen(next).Write(dir, WriteOptions{Force: true, DryRun: true})
		if err != nil || len(wr.Changed) == 0 || read(dir, "apps/b.yaml") != before {
			t.Errorf("dry run: %+v %v", wr, err)
		}
		if wr, err := gen(next).Write(filepath.Join(t.TempDir(), "new"), WriteOptions{DryRun: true}); err != nil || len(wr.Written) != 4 {
			t.Errorf("dry run into a new directory: %+v %v", wr, err)
		}
	})
}

// TestTreeWriteKeepsCiphertext: an encrypted file whose fingerprint didn't
// change keeps its earlier content; a changed fingerprint or a hand edit
// doesn't.
func TestTreeWriteKeepsCiphertext(t *testing.T) {
	dir := t.TempDir()
	write := func(cipher, fp string) *WriteResult {
		t.Helper()
		tr := NewTree()
		tr.Add("apps/web/sealedsecret-db.yaml", []byte(cipher))
		if fp != "" {
			tr.Fingerprint("apps/web/sealedsecret-db.yaml", fp)
		}
		wr, err := tr.Write(dir, WriteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return wr
	}
	got := func() string {
		b, _ := os.ReadFile(filepath.Join(dir, "apps/web/sealedsecret-db.yaml"))
		return string(b)
	}
	write("cipher-1", "fp1")
	if wr := write("cipher-2", "fp1"); got() != "cipher-1" || len(wr.Changed) != 0 || len(wr.Reused) != 1 {
		t.Errorf("same secret: %q %+v", got(), wr)
	}
	if write("cipher-3", "fp2"); got() != "cipher-3" {
		t.Errorf("changed secret: %q", got())
	}
	if write("cipher-4", ""); got() != "cipher-4" {
		t.Errorf("no fingerprint: %q", got())
	}
	if strings.Contains(string(must(os.ReadFile(filepath.Join(dir, encryptedFile)))), "sealedsecret") {
		t.Error("stale fingerprint kept")
	}
	write("cipher-5", "fp3")
	_ = os.WriteFile(filepath.Join(dir, "apps/web/sealedsecret-db.yaml"), []byte("resealed by hand"), 0o644)
	if wr := write("cipher-6", "fp3"); got() != "cipher-6" || len(wr.Edited) != 1 {
		t.Errorf("hand-edited: %q %+v", got(), wr)
	}
}

func must[T any](v T, _ error) T { return v }

// TestTreeWriteStateOwnership: an environment repository's own state wins
// over the output's older one, and the kept gitopsify.yaml is the next run's
// input, never a hand edit.
func TestTreeWriteStateOwnership(t *testing.T) {
	dir := t.TempDir()
	tr := NewTree()
	tr.roots = []string{"uat"}
	tr.Add("uat/apps/a.yaml", []byte("a"))
	tr.Add("gitopsify.yaml", []byte("spec: {}\n"))
	if _, err := tr.Write(dir, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	// Elsewhere (CI), a.yaml became the user's (released from the uat
	// repository's state); this machine's root state is older.
	_ = os.WriteFile(filepath.Join(dir, "uat", manifestFile), []byte("other.yaml\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "uat", checksumsFile), []byte(""), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "uat/apps/a.yaml"), []byte("mine"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "gitopsify.yaml"), []byte("spec: {edited: true}\n"), 0o644)
	tr = NewTree()
	tr.roots = []string{"uat"}
	tr.Add("uat/apps/a.yaml", []byte("a2"))
	tr.Add("gitopsify.yaml", []byte("spec: {}\n"))
	_, err := tr.Write(dir, WriteOptions{Edited: MergeFail})
	var ce *ConflictError
	if !errors.As(err, &ce) || len(ce.Unmanaged) != 1 || ce.Unmanaged[0] != "uat/apps/a.yaml" || len(ce.Edited) != 0 {
		t.Errorf("want a.yaml treated as the user's file and gitopsify.yaml as input: %v", err)
	}
	// With a repository per environment, only its files count as changes.
	wr, err := tr.Write(dir, WriteOptions{Unmanaged: MergeOverwrite, Edited: MergeFail})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range wr.Changed {
		if !strings.HasPrefix(p, "uat/") {
			t.Errorf("%s counted as a change outside the repositories", p)
		}
	}
}

// TestTreeWriteOtherCluster: an output made from one cluster isn't
// silently replaced from another (a laptop's context, say).
func TestTreeWriteOtherCluster(t *testing.T) {
	dir := t.TempDir()
	write := func(id string, force bool) error {
		tr := NewTree()
		tr.SourceID = id
		tr.Add("a.yaml", []byte(id))
		_, err := tr.Write(dir, WriteOptions{Force: force})
		return err
	}
	if err := write("uid-test", false); err != nil {
		t.Fatal(err)
	}
	if err := write("uid-test", false); err != nil {
		t.Errorf("same cluster: %v", err)
	}
	if err := write("uid-laptop", false); err == nil || !strings.Contains(err.Error(), "another cluster") {
		t.Errorf("another cluster: %v", err)
	}
	if err := write("", false); err != nil {
		t.Errorf("files (no identity): %v", err)
	}
	if err := write("uid-laptop", true); err != nil {
		t.Errorf("--force: %v", err)
	}
}
