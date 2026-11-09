package gitrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// TestPushTo: a pull request branch is pushed only when the output differs
// from the base branch, replaced when it holds only gitopsify's commits,
// left alone when it already has the same files, and never replaced over
// someone else's commit.
func TestPushTo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@t")
	}
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	gitT(t, base, "init", "-q", "--bare", "-b", "main", remote)
	seed := filepath.Join(base, "seed")
	_ = os.MkdirAll(seed, 0o755)
	_ = os.WriteFile(filepath.Join(seed, "a.yaml"), []byte("a"), 0o644)
	if _, err := Publish(Options{Dir: seed, Remote: remote, Branch: "main", Message: "seed", Push: true}); err != nil {
		t.Fatal(err)
	}

	run := func(name, content string) *Result {
		t.Helper()
		dir := filepath.Join(base, name)
		if _, err := Checkout(dir, remote, "main"); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(content), 0o644)
		res, err := Publish(Options{Dir: dir, Remote: remote, Branch: "main", PushTo: "gitopsify/drift", Message: "drift", Push: true})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return res
	}
	if res := run("same", "a"); !res.Unchanged || res.Pushed {
		t.Errorf("no drift pushed something: %+v", res)
	}
	if out := gitT(t, remote, "branch", "--list"); strings.Contains(out, "drift") {
		t.Errorf("drift branch created without drift: %s", out)
	}
	if res := run("one", "b"); !res.Pushed || res.Replaced != "" {
		t.Errorf("first drift: %+v", res)
	}
	if res := run("again", "b"); res.Pushed || !res.UpToDate {
		t.Errorf("the same drift again: %+v", res)
	}
	if res := run("two", "c"); !res.Pushed || res.Replaced == "" {
		t.Errorf("new drift: %+v", res)
	}
	if got := gitT(t, remote, "show", "gitopsify/drift:a.yaml"); got != "c" {
		t.Errorf("drift branch holds %q", got)
	}
	if log := gitT(t, remote, "log", "--format=%s", "main"); log != "seed\n" {
		t.Errorf("main changed:\n%s", log)
	}

	// Someone adds a commit to the branch: gitopsify won't replace it.
	human := filepath.Join(base, "human")
	gitT(t, base, "clone", "-q", "-b", "gitopsify/drift", remote, human)
	_ = os.WriteFile(filepath.Join(human, "note.txt"), []byte("mine"), 0o644)
	gitT(t, human, "add", "-A")
	gitT(t, human, "commit", "-q", "-m", "my fix")
	gitT(t, human, "push", "-q", "origin", "gitopsify/drift")
	dir := filepath.Join(base, "three")
	if _, err := Checkout(dir, remote, "main"); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("d"), 0o644)
	_, err := Publish(Options{Dir: dir, Remote: remote, Branch: "main", PushTo: "gitopsify/drift", Message: "drift", Push: true})
	if err == nil || !strings.Contains(err.Error(), "didn't make") {
		t.Errorf("replaced someone's commit: %v", err)
	}

	// An amended gitopsify commit (the trailer stays) is someone's too.
	gitT(t, human, "reset", "-q", "--hard", "HEAD~1")
	_ = os.WriteFile(filepath.Join(human, "a.yaml"), []byte("fixed by hand"), 0o644)
	amend := exec.Command("git", "commit", "-q", "-a", "--amend", "--no-edit")
	amend.Dir = human
	amend.Env = append(os.Environ(), "GIT_COMMITTER_NAME=h", "GIT_COMMITTER_EMAIL=h@h")
	if out, err := amend.CombinedOutput(); err != nil {
		t.Fatalf("amend: %v %s", err, out)
	}
	gitT(t, human, "push", "-q", "-f", "origin", "gitopsify/drift")
	_, err = Publish(Options{Dir: dir, Remote: remote, Branch: "main", PushTo: "gitopsify/drift", Message: "drift", Push: true})
	if err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Errorf("replaced an amended commit: %v", err)
	}

	// A branch outside gitopsify/ is never replaced, only fast-forwarded.
	dir2 := filepath.Join(base, "four")
	if _, err := Checkout(dir2, remote, "main"); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir2, "a.yaml"), []byte("e"), 0o644)
	gitT(t, base, "-C", human, "push", "-q", "origin", "HEAD:refs/heads/prod")
	_, err = Publish(Options{Dir: dir2, Remote: remote, Branch: "main", PushTo: "prod", Message: "x", Push: true})
	if err == nil || !strings.Contains(err.Error(), "only branches under gitopsify/") {
		t.Errorf("replaced prod: %v", err)
	}
}

func TestSameRemote(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"https://github.com/acme/gitops.git", "https://github.com/acme/gitops", true},
		{"https://GitHub.com/acme/gitops/", "https://github.com/acme/gitops.git", true},
		{"https://x-access-token:t@github.com/acme/gitops", "https://github.com/acme/gitops.git", true},
		{"git@github.com:acme/gitops.git", "git@github.com:acme/gitops", true},
		{"https://github.com/acme/gitops", "https://github.com/acme/other", false},
	} {
		if got := SameRemote(c.a, c.b); got != c.same {
			t.Errorf("%s vs %s: %v", c.a, c.b, got)
		}
	}
}
