package gitrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestPushOnTopOfRemoteHistory: generating into a fresh directory for a
// remote that already has commits continues its history.
func TestPushOnTopOfRemoteHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	gitT(t, base, "init", "-q", "--bare", "-b", "main", remote)

	first := filepath.Join(base, "first")
	if done, err := Checkout(first, remote, "main"); err != nil || done {
		t.Fatalf("empty remote: %v %v", done, err)
	}
	_ = os.MkdirAll(first, 0o755)
	_ = os.WriteFile(filepath.Join(first, "a.yaml"), []byte("a"), 0o644)
	if _, err := Publish(Options{Dir: first, Remote: remote, Branch: "main", Message: "one", Push: true}); err != nil {
		t.Fatal(err)
	}

	second := filepath.Join(base, "second")
	done, err := Checkout(second, remote, "main")
	if err != nil || !done {
		t.Fatalf("checkout: %v %v", done, err)
	}
	_ = os.WriteFile(filepath.Join(second, "b.yaml"), []byte("b"), 0o644)
	res, err := Publish(Options{Dir: second, Remote: remote, Branch: "main", Message: "two", Push: true})
	if err != nil || !res.Pushed {
		t.Fatalf("push on top: %+v %v", res, err)
	}
	if log := gitT(t, remote, "log", "--format=%s", "main"); log != "two\none\n" {
		t.Errorf("history:\n%s", log)
	}
	// A directory with files, or a repository, is left alone.
	if done, _ := Checkout(second, remote, "main"); done {
		t.Error("checked out over an existing repository")
	}
}
