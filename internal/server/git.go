package server

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hayone1/gitopsify/pkg/config"
)

// scpLike is user@host:path, the SSH shorthand.
var scpLike = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[A-Za-z0-9._~/][A-Za-z0-9._~/@+-]*$`)

// checkRemote refuses Git remotes the server mustn't use: anything git
// could read as an option or a local path, transports other than https and
// ssh (http only when allowed), and credentials in the URL.
func checkRemote(remote string, allowHTTP bool) error {
	if remote == "" {
		return errors.New("the repository URL is empty")
	}
	if strings.HasPrefix(remote, "-") || strings.ContainsFunc(remote, func(r rune) bool { return r < 0x20 || r == 0x7f || r == ' ' || r == '\\' }) {
		return fmt.Errorf("%q isn't a repository URL", remote)
	}
	if scpLike.MatchString(remote) {
		return nil
	}
	u, err := url.Parse(remote)
	if err != nil || u.Host == "" || u.Opaque != "" || strings.HasPrefix(u.Host, "-") {
		return fmt.Errorf("%q isn't a repository URL: use https://host/owner/repo.git or git@host:owner/repo.git", remote)
	}
	switch u.Scheme {
	case "https", "ssh":
	case "http":
		if !allowHTTP {
			return fmt.Errorf("%q: plain http isn't allowed (spec.git.allowHTTP)", remote)
		}
	default:
		return fmt.Errorf("%q: only https and ssh repositories are supported", remote)
	}
	if _, has := u.User.Password(); has {
		return fmt.Errorf("the repository URL holds a password; the server's credentials (spec.git.credentials) are used instead")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q: no query or fragment in repository URLs", remote)
	}
	return nil
}

var branchRE = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// checkBranch accepts names git check-ref-format --branch would, roughly.
func checkBranch(b string) error {
	if b == "" {
		return nil
	}
	// A branch name, not HEAD or a full ref (refs/tags/... would push a tag).
	if b == "HEAD" || strings.HasPrefix(b, "refs/") {
		return fmt.Errorf("%q isn't a branch name", b)
	}
	if !branchRE.MatchString(b) || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") ||
		strings.HasSuffix(b, ".") || strings.HasSuffix(b, ".lock") || strings.Contains(b, "..") || strings.Contains(b, "//") ||
		strings.Contains(b, "/.") || strings.HasPrefix(b, ".") || len(b) > 200 {
		return fmt.Errorf("%q isn't a branch name", b)
	}
	return nil
}

// allowedRepo checks a remote against spec.git.repositories for a run on a
// cluster or a tool.
func allowedRepo(rules []config.GitRepository, remote, cluster, tool string) error {
	if len(rules) == 0 {
		return nil
	}
	norm := strings.TrimSuffix(remote, ".git")
	for _, r := range rules {
		pattern := strings.TrimSuffix(r.URL, ".git")
		if ok, _ := path.Match(pattern, norm); !ok && pattern != norm {
			continue
		}
		everywhere := len(r.Clusters) == 0 && len(r.Tools) == 0
		if everywhere || (cluster != "" && matchesAny(r.Clusters, cluster)) || (tool != "" && matchesAny(r.Tools, tool)) {
			return nil
		}
	}
	return fmt.Errorf("%s isn't a repository runs on %s may use (spec.git.repositories)", remote, firstNonEmpty(cluster, tool))
}

// checkRepo validates a remote and branch for a run on a cluster or tool.
func (s *Server) checkRepo(remote, branch, cluster, tool string) error {
	if err := checkRemote(remote, s.cfg.Git.AllowHTTP); err != nil {
		return err
	}
	if err := checkBranch(branch); err != nil {
		return err
	}
	return allowedRepo(s.cfg.Git.Repositories, remote, cluster, tool)
}

// remoteHost returns the host of a Git remote and whether it is reached over
// SSH.
func remoteHost(remote string) (host string, ssh bool) {
	if scpLike.MatchString(remote) {
		rest := remote[strings.Index(remote, "@")+1:]
		return rest[:strings.Index(rest, ":")], true
	}
	if u, err := url.Parse(remote); err == nil && u.Host != "" {
		return u.Hostname(), u.Scheme == "ssh"
	}
	return "", false
}

// gitEnv returns the environment git runs with for a remote: only https and
// ssh (and http when allowed), no symbolic links checked out (a cloned
// repository can't point the server's writes elsewhere), stalled transfers
// aborted, and the configured credentials: an Authorization header for
// HTTPS (no token in URLs, argv or files) or a key for SSH.
func gitEnv(g config.ServerGit, remote string) ([]string, error) {
	cfg := [][2]string{
		{"protocol.allow", "never"},
		{"protocol.https.allow", "always"},
		{"protocol.ssh.allow", "always"},
		{"core.symlinks", "false"},
		{"core.hooksPath", "/dev/null"},
		{"http.lowSpeedLimit", "1000"},
		{"http.lowSpeedTime", "60"},
		// No credential helper of the machine's user gets asked or told.
		{"credential.helper", ""},
	}
	if g.AllowHTTP {
		cfg = append(cfg, [2]string{"protocol.http.allow", "always"})
	}
	ssh := "ssh -o BatchMode=yes -o ConnectTimeout=30"
	host, isSSH := remoteHost(remote)
	for _, c := range g.Credentials {
		if host == "" || !strings.EqualFold(c.Host, host) {
			continue
		}
		if isSSH {
			if c.SSHKeyFile == "" {
				return nil, fmt.Errorf("git credentials for %s have no sshKeyFile for the SSH remote %s", host, remote)
			}
			ssh += " -i " + shellQuote(c.SSHKeyFile) + " -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=" + shellQuote(c.KnownHostsFile)
			break
		}
		if c.Token == (config.SettingValue{}) {
			break
		}
		tok, err := c.Token.Resolve()
		if err != nil {
			return nil, fmt.Errorf("git credentials for %s: %w", host, err)
		}
		basic := base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + tok))
		cfg = append(cfg, [2]string{"http.https://" + host + "/.extraHeader", "Authorization: Basic " + basic})
		break
	}
	env := []string{"GIT_PROTOCOL_FROM_USER=0", "GIT_SSH_COMMAND=" + ssh, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(cfg))}
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env, nil
}

// noSymlinks fails if dir holds symbolic links (core.symlinks=false should
// have kept them out of a checkout; this is the second line).
func noSymlinks(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			rel, _ := filepath.Rel(dir, p)
			return fmt.Errorf("the repository holds a symbolic link (%s), which the server doesn't write through", rel)
		}
		return nil
	})
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
