// Package gitrepo commits and pushes the generated repository with the git
// CLI, so the user's credential helpers and SSH agent apply.
package gitrepo

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Options configure Publish.
type Options struct {
	Dir    string
	Remote string
	// Branch is the branch the output belongs on (the one Argo CD or
	// Flux follows).
	Branch  string
	Message string
	Push    bool
	// PushTo pushes to this branch instead of Branch, for a pull request
	// into Branch; nothing is pushed when the output doesn't differ from
	// Branch. A branch under gitopsify/ (ReplacePrefix) is replaced when
	// every commit on it that the output doesn't have was made by gitopsify
	// and not changed since; any other branch is only fast-forwarded.
	PushTo string
	// AuthorName and AuthorEmail sign the commit; empty uses git's
	// configuration (or gitopsify when there is none).
	AuthorName, AuthorEmail string
	// Env is added to git's environment (credentials for the remote, as the
	// web server passes them).
	Env []string
	// Context stops git when it ends (nil: never).
	Context context.Context
}

// Result describes what Publish did.
type Result struct {
	Initialized bool
	Committed   bool
	Pushed      bool
	// Unchanged: the remote's Branch already holds the output, so
	// nothing was pushed.
	Unchanged bool
	// Replaced is the commit PushTo pointed to when gitopsify replaced it
	// (to recover it if needed).
	Replaced string
	// UpToDate: PushTo already held the same files, so nothing was pushed.
	UpToDate bool
	Commit   string
}

// Trailer ends the message of every commit gitopsify makes: a pull request
// branch whose commits all have it can be replaced (Options.PushTo).
const Trailer = "Generated-by: gitopsify"

// ReplacePrefix starts the names of the branches gitopsify may replace:
// others (the branches Argo CD or Flux follow) are never force-pushed.
const ReplacePrefix = "gitopsify/"

// Checkout prepares an output directory that isn't a Git repository yet for a
// remote that already has the branch: it clones the branch into it, so the
// generated files are written on top of the remote's history (and files
// people added there survive) instead of on an unrelated root commit that
// can't be pushed. It does nothing when the directory already is a
// repository, holds files, or the remote has no such branch. It reports
// whether it checked anything out.
func Checkout(dir, remote, branch string) (bool, error) {
	return CheckoutEnv(context.Background(), dir, remote, branch, nil)
}

// CheckoutEnv is Checkout with env added to git's environment, stopped
// when ctx ends.
func CheckoutEnv(ctx context.Context, dir, remote, branch string, env []string) (bool, error) {
	git := func(dir string, args ...string) (string, error) { return gitEnv(ctx, dir, env, args...) }
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
	out, err := git("", "ls-remote", "--heads", "--", remote, branch)
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
		{"remote", "add", "--", "origin", remote},
		{"fetch", "-q", "origin", "--", branch},
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
	ctx := o.Context
	if ctx == nil {
		ctx = context.Background()
	}
	git := func(dir string, args ...string) (string, error) { return gitEnv(ctx, dir, o.Env, args...) }
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
			if _, err := git(o.Dir, "remote", "add", "--", "origin", o.Remote); err != nil {
				return nil, err
			}
		case !SameRemote(current, o.Remote):
			return nil, fmt.Errorf("%s already has remote origin %s, not %s", o.Dir, RedactURLs(current), RedactURLs(o.Remote))
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
		args := []string{"commit", "-q", "-m", strings.TrimRight(o.Message, "\n") + "\n\n" + Trailer}
		if o.AuthorName != "" && o.AuthorEmail != "" {
			args = append([]string{"-c", "user.name=" + o.AuthorName, "-c", "user.email=" + o.AuthorEmail}, args...)
		} else if email, _ := git(o.Dir, "config", "user.email"); email == "" {
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
		head, err := git(o.Dir, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		base, err := remoteHead(git, o.Dir, o.Branch)
		if err != nil {
			return nil, err
		}
		if base == head {
			res.Unchanged = true
			return res, nil
		}
		if o.PushTo == "" || o.PushTo == o.Branch {
			if _, err := git(o.Dir, "push", "-u", "origin", "--", "HEAD:refs/heads/"+o.Branch); err != nil {
				return nil, err
			}
			res.Pushed = true
			return res, nil
		}
		lease := "refs/heads/" + o.PushTo + ":" // must not exist yet
		old, err := remoteHead(git, o.Dir, o.PushTo)
		if err != nil {
			return nil, err
		}
		if old != "" && !strings.HasPrefix(o.PushTo, ReplacePrefix) {
			// Not gitopsify's to replace: a fast-forward or nothing.
			if _, err := git(o.Dir, "push", "origin", "--", "HEAD:refs/heads/"+o.PushTo); err != nil {
				return nil, fmt.Errorf("%w (only branches under %s are replaced; this one can only move forward)", err, ReplacePrefix)
			}
			res.Pushed = true
			return res, nil
		}
		if old != "" {
			if err := replaceable(git, o.Dir, o.PushTo); err != nil {
				return nil, err
			}
			// The same files as last time: leave the branch (and its pull
			// request) alone.
			oldTree, err := git(o.Dir, "rev-parse", "FETCH_HEAD^{tree}")
			if err != nil {
				return nil, err
			}
			if headTree, err := git(o.Dir, "rev-parse", "HEAD^{tree}"); err == nil && headTree == oldTree {
				res.UpToDate = true
				return res, nil
			}
			lease += old
			res.Replaced = old
		}
		if _, err := git(o.Dir, "push", "--force-with-lease="+lease, "origin", "--", "HEAD:refs/heads/"+o.PushTo); err != nil {
			return nil, err
		}
		res.Pushed = true
	}
	return res, nil
}

// remoteHead returns the commit a branch of origin points to, or "" when it
// doesn't exist.
func remoteHead(git func(string, ...string) (string, error), dir, branch string) (string, error) {
	out, err := git(dir, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	// ls-remote matches the pattern's tail: a/refs/heads/x matches too.
	for _, l := range strings.Split(out, "\n") {
		if sha, ref, ok := strings.Cut(strings.TrimSpace(l), "\t"); ok && ref == "refs/heads/"+branch {
			return sha, nil
		}
	}
	return "", nil
}

// replaceable checks that every commit of origin's branch that HEAD doesn't
// have was made by gitopsify, so replacing the branch loses nobody's work.
func replaceable(git func(string, ...string) (string, error), dir, branch string) error {
	if _, err := git(dir, "fetch", "-q", "--no-tags", "origin", "--", "refs/heads/"+branch); err != nil {
		return err
	}
	// Committer and author differ once someone amends, rebases or
	// cherry-picks gitopsify's commit: then it is theirs too.
	out, err := git(dir, "log", "--format=%H%x00%ae%x00%ce%x00%an%x00%cn%x00%B%x1e", "FETCH_HEAD", "--not", "HEAD")
	if err != nil {
		return err
	}
	for _, c := range strings.Split(out, "\x1e") {
		f := strings.SplitN(strings.TrimSpace(c), "\x00", 6)
		if len(f) < 6 {
			continue
		}
		if !slices.Contains(strings.Split(f[5], "\n"), Trailer) || f[1] != f[2] || f[3] != f[4] {
			return fmt.Errorf("branch %s has commit %.12s that gitopsify didn't make (or that was changed since); merge or delete the branch, or push to another one", branch, f[0])
		}
	}
	return nil
}

// SameRemote reports whether two remote addresses name the same
// repository: CI checkouts often drop ".git" or add credentials.
func SameRemote(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(s), "/"), ".git")
		if u, err := url.Parse(s); err == nil && u.Host != "" {
			u.User = nil
			u.Host = strings.ToLower(u.Host)
			return u.String()
		}
		return s
	}
	return norm(a) == norm(b)
}

func gitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Fail instead of waiting for a password nobody will type.
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", RedactURLs(strings.Join(args, " ")), err, RedactURLs(strings.TrimSpace(stderr.String())))
	}
	return strings.TrimSpace(string(out)), nil
}

var urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/@\s]+@`)

// RedactURLs hides credentials in URLs (https://user:token@host/...):
// errors are printed, and may reach a telemetry collector.
func RedactURLs(s string) string { return urlUserinfo.ReplaceAllString(s, "${1}<redacted>@") }
