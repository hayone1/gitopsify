package config

import (
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
)

// KindServer is the kind of the web server's configuration
// (gitopsify-server.yaml).
const KindServer = "Server"

// Authentication modes of the web server.
const (
	// AuthLocal is one user, the person who started gitopsify ui: a random
	// token in the URL it opens logs the browser in. Only for localhost.
	AuthLocal = "local"
	// AuthOIDC signs people in with an OpenID Connect provider (Dex,
	// Keycloak, Okta, Entra ID, Google...).
	AuthOIDC = "oidc"
)

// Roles, from least to most.
const (
	RoleNone     = ""
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleAdmin    = "admin"
)

// Server configures gitopsify's web server (gitopsify server): who can sign
// in and do what, which clusters and Git remotes it uses, where it keeps
// runs. It is a resource in the Kubernetes style; see documents.go.
type Server struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata,omitzero"`
	Spec       ServerSpec `json:"spec"`
}

// ServerSpec is the server's configuration.
type ServerSpec struct {
	// Listen is the address to listen on, such as :8080.
	Listen string `json:"listen,omitempty"`
	// ExternalURL is the address people reach the server at, such as
	// https://gitopsify.example.com. OIDC redirects and cookies use it.
	ExternalURL string `json:"externalURL,omitempty"`
	// DataDir holds runs, their output and the audit log.
	DataDir string `json:"dataDir,omitempty"`
	// TLS serves HTTPS itself; usually an ingress terminates TLS instead.
	TLS  *ServerTLS `json:"tls,omitempty"`
	Auth ServerAuth `json:"auth,omitzero"`
	RBAC ServerRBAC `json:"rbac,omitzero"`
	// Clusters are the clusters people can read and target, by name.
	Clusters []ServerCluster `json:"clusters,omitempty"`
	// Git holds credentials for pushing generated repositories.
	Git ServerGit `json:"git,omitzero"`
	// Runs limits the work the server does.
	Runs ServerRuns `json:"runs,omitzero"`
	// Audit configures the audit log.
	Audit ServerAudit `json:"audit,omitzero"`
	// TrustForwardedFor takes the client address from X-Forwarded-For (for
	// rate limits and the audit log). Set it only behind a proxy that
	// overwrites the header.
	TrustForwardedFor bool `json:"trustForwardedFor,omitempty"`
}

// ServerTLS is a certificate and its key, as PEM files.
type ServerTLS struct {
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
}

// ServerAuth is how people and programs sign in.
type ServerAuth struct {
	// Mode is local or oidc.
	Mode string      `json:"mode,omitempty"`
	OIDC *ServerOIDC `json:"oidc,omitempty"`
	// SessionKey signs and encrypts session cookies: 32 random bytes or
	// more, base64 or hex. Set it (from a Secret) so sessions survive
	// restarts and work across replicas; a random key is used otherwise.
	SessionKey SettingValue `json:"sessionKey,omitzero"`
	// SessionTTL is how long a sign-in lasts, such as 12h.
	SessionTTL string `json:"sessionTTL,omitempty"`
	// Tokens are API tokens for automation (Authorization: Bearer ...).
	Tokens []ServerToken `json:"tokens,omitempty"`
}

// ServerOIDC is the OpenID Connect provider people sign in with.
type ServerOIDC struct {
	// Issuer is the provider's issuer URL.
	Issuer       string       `json:"issuer"`
	ClientID     string       `json:"clientID"`
	ClientSecret SettingValue `json:"clientSecret,omitzero"`
	// Scopes requested besides openid; default profile, email, groups.
	Scopes []string `json:"scopes,omitempty"`
	// UsernameClaim names the user (default email, then preferred_username,
	// then sub).
	UsernameClaim string `json:"usernameClaim,omitempty"`
	// GroupsClaim lists the user's groups (default groups).
	GroupsClaim string `json:"groupsClaim,omitempty"`
	// AllowedDomains, when set, admits only these email domains.
	AllowedDomains []string `json:"allowedDomains,omitempty"`
	// CAFile trusts a private CA for the issuer.
	CAFile string `json:"caFile,omitempty"`
}

// ServerToken is an API token. Only its SHA-256 is stored.
type ServerToken struct {
	Name string `json:"name"`
	// SHA256 is the hex SHA-256 of the token (gitopsify server token makes
	// one and prints both).
	SHA256 string `json:"sha256"`
	// Role and Clusters are what the token may do, as in a binding.
	Role     string   `json:"role"`
	Clusters []string `json:"clusters,omitempty"`
}

// ServerRBAC maps people to roles.
type ServerRBAC struct {
	// DefaultRole is the role of anyone who signs in and matches no binding:
	// empty (none: they can't do anything), viewer, operator or admin.
	DefaultRole string `json:"defaultRole,omitempty"`
	// Bindings give roles to users and groups, optionally only on some
	// clusters (globs). The highest matching role wins.
	Bindings []ServerBinding `json:"bindings,omitempty"`
}

// ServerBinding gives a role to users and groups.
type ServerBinding struct {
	Role   string   `json:"role"`
	Users  []string `json:"users,omitempty"`
	Groups []string `json:"groups,omitempty"`
	// Clusters limits the binding to these clusters (globs); empty means
	// all. Server-wide actions (audit log, provider installs) need a binding
	// without clusters.
	Clusters []string `json:"clusters,omitempty"`
}

// ServerCluster is a cluster people can use by name.
type ServerCluster struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Access is how the server connects.
	Access Access `json:"access,omitzero"`
	// Impersonate makes requests as the signed-in user and their groups
	// (Kubernetes RBAC then applies per person). The server's own identity
	// needs the impersonate verb.
	Impersonate bool `json:"impersonate,omitempty"`
	// Token is a bearer token for the cluster, from env or file.
	Token SettingValue `json:"token,omitzero"`
}

// ServerGit is how the server pushes to Git remotes.
type ServerGit struct {
	// Credentials by host. HTTPS remotes get the token; SSH remotes the key.
	Credentials []GitCredential `json:"credentials,omitempty"`
	// AuthorName and AuthorEmail sign the server's commits (default:
	// gitopsify, the signed-in user's email when known).
	AuthorName  string `json:"authorName,omitempty"`
	AuthorEmail string `json:"authorEmail,omitempty"`
}

// GitCredential authenticates to one Git host.
type GitCredential struct {
	// Host, such as github.com.
	Host string `json:"host"`
	// Username for HTTPS (default x-access-token, which GitHub and Gitea
	// accept with a token; GitLab wants oauth2).
	Username string       `json:"username,omitempty"`
	Token    SettingValue `json:"token,omitzero"`
	// SSHKeyFile is a private key for SSH remotes.
	SSHKeyFile string `json:"sshKeyFile,omitempty"`
	// KnownHostsFile verifies SSH hosts.
	KnownHostsFile string `json:"knownHostsFile,omitempty"`
}

// ServerRuns limits runs.
type ServerRuns struct {
	// Concurrency is how many runs work at once (default 2).
	Concurrency int `json:"concurrency,omitempty"`
	// Timeout of one run (default 30m).
	Timeout string `json:"timeout,omitempty"`
	// Retention is how long finished runs are kept (default 720h).
	Retention string `json:"retention,omitempty"`
	// AllowSecretValues allows the local and plain Secret strategies, which
	// store Secret values on the server and in its downloads.
	AllowSecretValues bool `json:"allowSecretValues,omitempty"`
}

// ServerAudit configures the audit log (JSON lines in dataDir/audit).
type ServerAudit struct {
	// Stdout also writes audit events to the server's log.
	Stdout bool `json:"stdout,omitempty"`
	// Retention is how long audit files are kept (default 2160h, 90 days).
	Retention string `json:"retention,omitempty"`
}

// LoadServer reads the Server resource in path.
func LoadServer(path string) (*Server, error) {
	docs, err := ReadDocuments(path)
	if err != nil {
		return nil, err
	}
	found := Find(docs, KindServer, "")
	if len(found) != 1 {
		return nil, fmt.Errorf("%s: want one %s resource, found %d", path, KindServer, len(found))
	}
	s := &Server{}
	if err := decodeStrict(found[0].Raw, s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.Default()
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Default fills in defaults.
func (s *Server) Default() {
	if s.APIVersion == "" {
		s.APIVersion = APIVersion
	}
	if s.Kind == "" {
		s.Kind = KindServer
	}
	if s.Metadata.Name == "" {
		s.Metadata.Name = "gitopsify"
	}
	sp := &s.Spec
	if sp.Listen == "" {
		sp.Listen = ":8080"
	}
	if sp.DataDir == "" {
		sp.DataDir = "/var/lib/gitopsify"
	}
	if sp.Auth.Mode == "" {
		sp.Auth.Mode = AuthOIDC
	}
	if sp.Auth.SessionTTL == "" {
		sp.Auth.SessionTTL = "12h"
	}
	if o := sp.Auth.OIDC; o != nil {
		if len(o.Scopes) == 0 {
			o.Scopes = []string{"profile", "email", "groups"}
		}
		if o.GroupsClaim == "" {
			o.GroupsClaim = "groups"
		}
	}
	if sp.Runs.Concurrency == 0 {
		sp.Runs.Concurrency = 2
	}
	if sp.Runs.Timeout == "" {
		sp.Runs.Timeout = "30m"
	}
	if sp.Runs.Retention == "" {
		sp.Runs.Retention = "720h"
	}
	if sp.Audit.Retention == "" {
		sp.Audit.Retention = "2160h"
	}
	for i := range sp.Git.Credentials {
		if sp.Git.Credentials[i].Username == "" {
			sp.Git.Credentials[i].Username = "x-access-token"
		}
	}
}

// Validate checks the configuration.
func (s *Server) Validate() error {
	if err := s.Metadata.validate(); err != nil {
		return err
	}
	sp := &s.Spec
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	if sp.ExternalURL != "" {
		if u, err := url.Parse(sp.ExternalURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("spec.externalURL %q isn't an http(s) URL", sp.ExternalURL)
		}
	}
	switch sp.Auth.Mode {
	case AuthLocal:
	case AuthOIDC:
		o := sp.Auth.OIDC
		if o == nil || o.Issuer == "" || o.ClientID == "" {
			add("spec.auth.oidc needs issuer and clientID (or spec.auth.mode: local)")
		}
		if sp.ExternalURL == "" {
			add("spec.externalURL is needed with OIDC (the redirect URL is <externalURL>/api/v1/auth/callback)")
		}
	default:
		add("spec.auth.mode %q: want local or oidc", sp.Auth.Mode)
	}
	if _, err := time.ParseDuration(sp.Auth.SessionTTL); err != nil {
		add("spec.auth.sessionTTL: %v", err)
	}
	for _, d := range []struct{ name, v string }{{"runs.timeout", sp.Runs.Timeout}, {"runs.retention", sp.Runs.Retention}, {"audit.retention", sp.Audit.Retention}} {
		if _, err := time.ParseDuration(d.v); err != nil {
			add("spec.%s: %v", d.name, err)
		}
	}
	if sp.Runs.Concurrency < 1 {
		add("spec.runs.concurrency must be at least 1")
	}
	validRole := func(r string) bool { return r == RoleViewer || r == RoleOperator || r == RoleAdmin }
	if sp.RBAC.DefaultRole != "" && !validRole(sp.RBAC.DefaultRole) {
		add("spec.rbac.defaultRole %q: want viewer, operator or admin (or nothing)", sp.RBAC.DefaultRole)
	}
	for i, b := range sp.RBAC.Bindings {
		if !validRole(b.Role) {
			add("spec.rbac.bindings[%d].role %q: want viewer, operator or admin", i, b.Role)
		}
		if len(b.Users) == 0 && len(b.Groups) == 0 {
			add("spec.rbac.bindings[%d] names no users or groups", i)
		}
		for _, c := range b.Clusters {
			if _, err := path.Match(c, ""); err != nil {
				add("spec.rbac.bindings[%d].clusters: %q: %v", i, c, err)
			}
		}
	}
	seen := map[string]bool{}
	for i, t := range sp.Auth.Tokens {
		if t.Name == "" || seen[t.Name] {
			add("spec.auth.tokens[%d]: names must be set and unique", i)
		}
		seen[t.Name] = true
		if len(t.SHA256) != 64 || strings.Trim(strings.ToLower(t.SHA256), "0123456789abcdef") != "" {
			add("spec.auth.tokens[%d].sha256 isn't a hex SHA-256 (gitopsify server token makes one)", i)
		}
		if !validRole(t.Role) {
			add("spec.auth.tokens[%d].role %q: want viewer, operator or admin", i, t.Role)
		}
	}
	names := map[string]bool{}
	for i, c := range sp.Clusters {
		if !resourceName.MatchString(c.Name) || len(c.Name) > 63 {
			add("spec.clusters[%d].name %q: lowercase letters, digits and dashes", i, c.Name)
		}
		if names[c.Name] {
			add("spec.clusters[%d].name %q is used twice", i, c.Name)
		}
		names[c.Name] = true
		if err := c.Access.Validate(); err != nil {
			add("spec.clusters[%d] (%s): %v", i, c.Name, err)
		}
	}
	for i, g := range sp.Git.Credentials {
		if g.Host == "" {
			add("spec.git.credentials[%d].host is empty", i)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// Duration parses one of the spec's durations, already validated.
func Duration(s string) time.Duration {
	d, _ := time.ParseDuration(s)
	return d
}
