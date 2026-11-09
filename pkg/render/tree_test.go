package render

import (
	"os"
	"path/filepath"
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
		if _, _, err := tr.Write(dir, false); err == nil {
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
	if _, _, err := tr.Write(dir, true); err == nil {
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
	if _, removed, err := tr.Write(dir, false); err != nil || len(removed) > 0 {
		t.Errorf("removed %v (%v)", removed, err)
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
	if _, _, err := tr.Write(dir, false); err != nil {
		t.Fatal(err)
	}
	tr = NewTree()
	tr.Add("apps/a/x.yaml", []byte("a2"))
	_, removed, err := tr.Write(dir, false)
	if err != nil || len(removed) != 1 || removed[0] != "apps/b/y.yaml" {
		t.Fatalf("removed %v (%v)", removed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "apps", "b")); !os.IsNotExist(err) {
		t.Error("the emptied directory stayed")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "apps", "a", "x.yaml")); string(b) != "a2" {
		t.Errorf("x.yaml: %q", b)
	}
}
