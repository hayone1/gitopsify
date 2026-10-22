// Package gitrepo commits and pushes the generated repository with the git
// CLI, so the user's credential helpers and SSH agent apply.
package gitrepo

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Options configure Publish.
type Options struct {
	Dir     string
	Remote  string
	Branch  string
	Message string
	Push    bool
}

// Result describes what Publish did.
type Result struct {
	Initialized bool
	Committed   bool
	Pushed      bool
	Commit      string
}

// Checkout prepares an output directory that isn't a Git repository yet for a
// remote that already has the branch: it clones the branch into it, so the
// generated files are written on top of the remote's history (and files
// people added there survive) instead of on an unrelated root commit that
// can't be pushed. It does nothing when the directory already is a
// repository, holds files, or the remote has no such branch. It reports
// whether it checked anything out.
func Checkout(dir, remote, branch string) (bool, error) {
	if remote == "" {
		return false, nil
	}
	if branch == "" {
		branch = "main"
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return false, nil
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return false, nil
	}
	out, err := git("", "ls-remote", "--heads", remote, branch)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(out) == "" {
		return false, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", branch},
		{"remote", "add", "origin", remote},
		{"fetch", "-q", "origin", branch},
		{"checkout", "-q", "-B", branch, "FETCH_HEAD"},
	} {
		if _, err := git(dir, args...); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Publish initializes the repository if needed, commits every change and
// optionally pushes it.
func Publish(o Options) (*Result, error) {
	if o.Branch == "" {
		o.Branch = "main"
	}
	res := &Result{}
	if _, err := os.Stat(filepath.Join(o.Dir, ".git")); os.IsNotExist(err) {
		if _, err := git(o.Dir, "init", "-q", "-b", o.Branch); err != nil {
			return nil, err
		}
		res.Initialized = true
	}
	if o.Remote != "" {
		current, err := git(o.Dir, "remote", "get-url", "origin")
		switch {
		case err != nil:
			if _, err := git(o.Dir, "remote", "add", "origin", o.Remote); err != nil {
				return nil, err
			}
		case current != o.Remote:
			return nil, fmt.Errorf("%s already has remote origin %s, not %s", o.Dir, current, o.Remote)
		}
	}
	if _, err := git(o.Dir, "add", "-A"); err != nil {
		return nil, err
	}
	status, err := git(o.Dir, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	if status != "" {
		args := []string{"commit", "-q", "-m", o.Message}
		if email, _ := git(o.Dir, "config", "user.email"); email == "" {
			// Fresh CI runners have no identity; commit as gitopsify.
			args = append([]string{"-c", "user.name=gitopsify", "-c", "user.email=gitopsify@users.noreply.github.com"}, args...)
		}
		if _, err := git(o.Dir, args...); err != nil {
			return nil, err
		}
		res.Committed = true
	}
	if res.Commit, err = git(o.Dir, "rev-parse", "--short", "HEAD"); err != nil {
		return nil, err
	}
	if o.Push {
		if o.Remote == "" {
			return nil, fmt.Errorf("cannot push %s: no repository URL configured", o.Dir)
		}
		if _, err := git(o.Dir, "push", "-u", "origin", "HEAD:"+o.Branch); err != nil {
			return nil, err
		}
		res.Pushed = true
	}
	return res, nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Fail instead of waiting for a password nobody will type.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}
