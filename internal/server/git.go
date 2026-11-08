package server

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"

	"github.com/hayone1/gitopsify/pkg/config"
)

// remoteHost returns the host of a Git remote and whether it is reached over
// SSH.
func remoteHost(remote string) (host string, ssh bool) {
	if u, err := url.Parse(remote); err == nil && u.Host != "" {
		return u.Hostname(), u.Scheme == "ssh"
	}
	// scp-like: git@github.com:acme/repo.git
	if at := strings.Index(remote, "@"); at >= 0 {
		rest := remote[at+1:]
		if colon := strings.Index(rest, ":"); colon > 0 {
			return rest[:colon], true
		}
	}
	return "", false
}

// gitEnv returns the environment that authenticates git to a remote with the
// configured credentials: an Authorization header for HTTPS (no token in
// URLs, argv or files), a key for SSH. Without credentials for the host it
// returns nothing and git uses the server's own configuration.
func gitEnv(creds []config.GitCredential, remote string) ([]string, error) {
	host, ssh := remoteHost(remote)
	if host == "" {
		return nil, nil
	}
	for _, c := range creds {
		if !strings.EqualFold(c.Host, host) {
			continue
		}
		if ssh {
			if c.SSHKeyFile == "" {
				return nil, fmt.Errorf("git credentials for %s have no sshKeyFile for the SSH remote %s", host, remote)
			}
			cmd := "ssh -i " + shellQuote(c.SSHKeyFile) + " -o IdentitiesOnly=yes -o BatchMode=yes"
			if c.KnownHostsFile != "" {
				cmd += " -o StrictHostKeyChecking=yes -o UserKnownHostsFile=" + shellQuote(c.KnownHostsFile)
			} else {
				cmd += " -o StrictHostKeyChecking=accept-new"
			}
			return []string{"GIT_SSH_COMMAND=" + cmd}, nil
		}
		if c.Token == (config.SettingValue{}) {
			return nil, nil
		}
		tok, err := c.Token.Resolve()
		if err != nil {
			return nil, fmt.Errorf("git credentials for %s: %w", host, err)
		}
		basic := base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + tok))
		return []string{
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=http.https://" + host + "/.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
			// No credential helper of the machine's user gets asked or told.
			"GIT_CONFIG_KEY_1=credential.helper",
			"GIT_CONFIG_VALUE_1=",
		}, nil
	}
	return nil, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
