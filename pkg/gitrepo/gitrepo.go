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
		if _, err := git(o.Dir, "commit", "-q", "-m", o.Message); err != nil {
			return nil, fmt.Errorf("%w (is git user.name/user.email configured?)", err)
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
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}
