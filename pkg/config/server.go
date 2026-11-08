package config

import (
	"fmt"
	"net"
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
	// DataDir holds runs, their output and the audit log (with file
	// storage), or this replica's working copies (with PostgreSQL).
	DataDir string `json:"dataDir,omitempty"`
	// Storage is where runs and the audit log are kept.
	Storage ServerStorage `json:"storage,omitzero"`
	// TLS serves HTTPS itself; usually an ingress terminates TLS instead.
	TLS  *ServerTLS `json:"tls,omitempty"`
	Auth ServerAuth `json:"auth,omitzero"`
	RBAC ServerRBAC `json:"rbac,omitzero"`
	// Clusters are the clusters people can read and target, by name.
	Clusters []ServerCluster `json:"clusters,omitempty"`
	// Tools are instances of other tools (Grafana, Keycloak...) people can
	// export through providers, by name.
	Tools []ServerTool `json:"tools,omitempty"`
	// Providers configures where providers are installed from and to.
	Providers ServerProviders `json:"providers,omitzero"`
	// Git holds credentials for pushing generated repositories.
	Git ServerGit `json:"git,omitzero"`
	// Runs limits the work the server does.
	Runs ServerRuns `json:"runs,omitzero"`
	// Audit configures the audit log.
	Audit ServerAudit `json:"audit,omitzero"`
	// Notifications post to webhooks when runs finish.
	Notifications []ServerNotification `json:"notifications,omitempty"`
	// TrustedProxies are the addresses (CIDRs) of the proxies in front of
	// the server, such as the ingress controller's pods. From them, the client
	// address (for rate limits and the audit log) is the last
	// X-Forwarded-For entry that isn't a trusted proxy.
	TrustedProxies []string `json:"trustedProxies,omitempty"`
	// Metrics protects /metrics.
	Metrics ServerMetrics `json:"metrics,omitzero"`
}

// ServerMetrics protects /metrics.
type ServerMetrics struct {
	// Token, when set, must be sent as "Authorization: Bearer <token>".
	Token SettingValue `json:"token,omitzero"`
}

// ServerStorage is where runs and the audit log are kept.
type ServerStorage struct {
	// Type is file (dataDir; one replica) or postgres (shared, so several
	// replicas can serve and work).
	Type     string          `json:"type,omitempty"`
	Postgres *ServerPostgres `json:"postgres,omitempty"`
}

// ServerPostgres is a PostgreSQL database.
type ServerPostgres struct {
	// URL is a connection string, such as
	// postgres://gitopsify:<password>@db:5432/gitopsify?sslmode=require,
	// usually from env or file.
	URL SettingValue `json:"url"`
	// MaxConnections caps the connection pool (default: 4 or the number of
	// CPUs, whichever is more).
	MaxConnections int `json:"maxConnections,omitempty"`
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
	// Scopes requested besides openid; default profile and email. Dex
	// needs groups too for the groups claim.
	Scopes []string `json:"scopes,omitempty"`
	// UsernameClaim names the user (default email, then preferred_username,
	// then sub).
	UsernameClaim string `json:"usernameClaim,omitempty"`
	// GroupsClaim lists the user's groups (default groups).
	GroupsClaim string `json:"groupsClaim,omitempty"`
	// AllowedDomains, when set, admits only these (verified) email domains.
	AllowedDomains []string `json:"allowedDomains,omitempty"`
	// TrustUnverifiedEmail uses the email claim even when the token doesn't
	// say email_verified: true. Only for providers whose emails can't be set
	// by users; Microsoft Entra ID's can (use usernameClaim: oid there).
	TrustUnverifiedEmail bool `json:"trustUnverifiedEmail,omitempty"`
	// CAFile trusts a private CA for the issuer.
	CAFile string `json:"caFile,omitempty"`
}

// ServerToken is an API token. Only its SHA-256 is stored.
type ServerToken struct {
	Name string `json:"name"`
	// SHA256 is the hex SHA-256 of the token (gitopsify server token makes
	// one and prints both).
	SHA256 string `json:"sha256"`
	// Role, Clusters and Tools are what the token may do, as in a binding.
	Role     string   `json:"role"`
	Clusters []string `json:"clusters,omitempty"`
	Tools    []string `json:"tools,omitempty"`
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
	// Clusters and Tools limit the binding to these clusters and tools
	// (globs). With neither, it applies everywhere, and server-wide actions
	// (the audit log, installing providers) need such a binding. With
	// clusters only, it gives nothing on tools, and the other way round.
	Clusters []string `json:"clusters,omitempty"`
	Tools    []string `json:"tools,omitempty"`
}

// ServerTool is an instance of a tool people can export.
type ServerTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Provider reads the tool, such as grafana.
	Provider string `json:"provider"`
	// Settings are the provider's connection settings; secrets from env or
	// file. Settings left out come from the provider's environment
	// variables (GRAFANA_URL...), as with gitopsify export.
	Settings map[string]SettingValue `json:"settings,omitempty"`
}

// ServerNotification posts run results to a webhook.
type ServerNotification struct {
	Name string `json:"name"`
	// URL of the webhook; usually a secret (Slack and Teams URLs are), so
	// from env or file.
	URL SettingValue `json:"url"`
	// Format is slack, teams or json (the run, as the API returns it).
	Format string `json:"format,omitempty"`
	// Events are run.succeeded, run.failed and run.canceled (default the
	// first two).
	Events []string `json:"events,omitempty"`
	// Types limits it to scan, migrate or export runs (default all).
	Types []string `json:"types,omitempty"`
}

// ServerProviders configures providers on the server.
type ServerProviders struct {
	// Dir holds installed providers (default <dataDir>/providers).
	Dir string `json:"dir,omitempty"`
	// Index is the provider index, a URL or a file (default the published
	// one).
	Index string `json:"index,omitempty"`
	// DisableInstall stops admins from installing providers from the UI
	// (for air-gapped servers whose image brings its providers).
	DisableInstall bool `json:"disableInstall,omitempty"`
	// Env are the server's environment variables providers see besides
	// what programs need (PATH, HOME, proxies, certificates, the in-cluster
	// API address): globs such as AWS_* for providers that use the pod's
	// cloud identity. The server's own secrets are never passed.
	Env []string `json:"env,omitempty"`
}

// ServerCluster is a cluster people can use by name.
type ServerCluster struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Access is how the server connects.
	Access Access `json:"access,omitzero"`
	// Impersonate makes requests as the signed-in user and their groups
	// (Kubernetes RBAC then applies per person). The server's own identity
	// needs the impersonate verb. Groups and usernames starting with
	// "system:" are never impersonated.
	Impersonate bool `json:"impersonate,omitempty"`
	// ImpersonateUserPrefix and ImpersonateGroupPrefix are put before the
	// username and groups, as the API server's --oidc-username-prefix and
	// --oidc-groups-prefix do (for example "oidc:").
	ImpersonateUserPrefix  string `json:"impersonateUserPrefix,omitempty"`
	ImpersonateGroupPrefix string `json:"impersonateGroupPrefix,omitempty"`
	// ImpersonateGroups are the groups impersonated (globs, matched before
	// the prefix is added); empty impersonates all but system: and
	// kubeadm: groups. Set it, or a prefix, when people can name their own
	// groups in the identity provider.
	ImpersonateGroups []string `json:"impersonateGroups,omitempty"`
	// SealedSecrets is where the cluster's sealed-secrets controller runs
	// (default kube-system/sealed-secrets-controller). Migrations sealing
	// for this cluster use it; they can't name another controller unless
	// spec.runs.allowSecretValues is set.
	SealedSecrets *ServerSealedSecrets `json:"sealedSecrets,omitempty"`
	// Token is a bearer token for the cluster, from env or file.
	Token SettingValue `json:"token,omitzero"`
}

// ServerSealedSecrets is where a cluster's sealed-secrets controller runs.
type ServerSealedSecrets struct {
	ControllerNamespace string `json:"controllerNamespace,omitempty"`
	ControllerName      string `json:"controllerName,omitempty"`
}

// ServerGit is how the server pushes to Git remotes.
type ServerGit struct {
	// Repositories are the remotes runs may check out and push to, per
	// cluster and tool: the server's credentials write to them on people's
	// behalf. Empty allows every https and ssh remote, which suits a server
	// for one team only.
	Repositories []GitRepository `json:"repositories,omitempty"`
	// Credentials by host. HTTPS remotes get the token; SSH remotes the key.
	Credentials []GitCredential `json:"credentials,omitempty"`
	// AllowHTTP allows plain http:// remotes (test servers).
	AllowHTTP bool `json:"allowHTTP,omitempty"`
	// AuthorName and AuthorEmail sign the server's commits (default:
	// gitopsify, the signed-in user's email when known).
	AuthorName  string `json:"authorName,omitempty"`
	AuthorEmail string `json:"authorEmail,omitempty"`
}

// GitRepository allows remotes matching URL (a glob, such as
// https://github.com/acme/shop-*) for runs on these clusters and tools
// (globs; with neither, all).
type GitRepository struct {
	URL      string   `json:"url"`
	Clusters []string `json:"clusters,omitempty"`
	Tools    []string `json:"tools,omitempty"`
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
	// AllowSecretValues allows what can put Secret values in runs' output
	// in clear: the local and plain Secret strategies, jsonPatch copy and
	// move, a custom SOPS encryptedRegex, sealing for another controller
	// or a wider scope than strict, and Helm values that look like
	// credentials (otherwise redacted).
	AllowSecretValues bool `json:"allowSecretValues,omitempty"`
	// SOPSRecipients are the age, PGP or KMS recipients SOPS may encrypt
	// to. A migration whose recipients aren't all listed is refused, unless
	// its cluster impersonates (people then only read what Kubernetes lets
	// them). Empty: SOPS only on impersonating clusters.
	SOPSRecipients []string `json:"sopsRecipients,omitempty"`
	// MaxQueuedPerUser caps the runs one person has queued or working
	// (default 10).
	MaxQueuedPerUser int `json:"maxQueuedPerUser,omitempty"`
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
	if s.Spec.Auth.Mode == AuthLocal {
		return nil, fmt.Errorf("%s: spec.auth.mode: local is for gitopsify ui on one machine; a server signs people in with oidc", path)
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
	if sp.Storage.Type == "" {
		sp.Storage.Type = "file"
	}
	if sp.Auth.Mode == "" {
		sp.Auth.Mode = AuthOIDC
	}
	if sp.Auth.SessionTTL == "" {
		sp.Auth.SessionTTL = "12h"
	}
	if o := sp.Auth.OIDC; o != nil {
		if len(o.Scopes) == 0 {
			// Not "groups": Keycloak, Okta, Entra ID and Google refuse unknown
			// scopes; Dex wants it (spec.auth.oidc.scopes: [profile, email, groups]).
			o.Scopes = []string{"profile", "email"}
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
	if sp.Runs.MaxQueuedPerUser == 0 {
		sp.Runs.MaxQueuedPerUser = 10
	}
	if sp.Audit.Retention == "" {
		sp.Audit.Retention = "2160h"
	}
	for i := range sp.Notifications {
		n := &sp.Notifications[i]
		if n.Format == "" {
			n.Format = "json"
		}
		if len(n.Events) == 0 {
			n.Events = []string{"run.succeeded", "run.failed"}
		}
	}
	if sp.Providers.Dir == "" {
		sp.Providers.Dir = path.Join(sp.DataDir, "providers")
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
		} else if strings.Trim(u.Path, "/") != "" {
			add("spec.externalURL %q has a path; serve gitopsify at the root of a host", sp.ExternalURL)
		}
	}
	for i, c := range sp.TrustedProxies {
		if _, _, err := net.ParseCIDR(c); err != nil {
			if net.ParseIP(c) == nil {
				add("spec.trustedProxies[%d] %q isn't a CIDR or an address", i, c)
			}
		}
	}
	for i, r := range sp.Git.Repositories {
		if r.URL == "" {
			add("spec.git.repositories[%d] has no url", i)
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
	switch sp.Storage.Type {
	case "file":
	case "postgres":
		if sp.Storage.Postgres == nil || sp.Storage.Postgres.URL == (SettingValue{}) {
			add("spec.storage.postgres.url is needed with spec.storage.type: postgres")
		}
	default:
		add("spec.storage.type %q: want file or postgres", sp.Storage.Type)
	}
	if d, err := time.ParseDuration(sp.Auth.SessionTTL); err != nil || d <= 0 {
		add("spec.auth.sessionTTL %q: want a positive duration, such as 12h", sp.Auth.SessionTTL)
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
	for i, n := range sp.Notifications {
		if n.Name == "" {
			add("spec.notifications[%d] has no name", i)
		}
		if n.URL == (SettingValue{}) {
			add("spec.notifications[%d] (%s) has no url", i, n.Name)
		}
		if n.Format != "json" && n.Format != "slack" && n.Format != "teams" {
			add("spec.notifications[%d].format %q: want json, slack or teams", i, n.Format)
		}
		for _, e := range n.Events {
			if e != "run.succeeded" && e != "run.failed" && e != "run.canceled" {
				add("spec.notifications[%d].events: %q: want run.succeeded, run.failed or run.canceled", i, e)
			}
		}
	}
	tools := map[string]bool{}
	for i, t := range sp.Tools {
		if !resourceName.MatchString(t.Name) || len(t.Name) > 63 {
			add("spec.tools[%d].name %q: lowercase letters, digits and dashes", i, t.Name)
		}
		if tools[t.Name] {
			add("spec.tools[%d].name %q is used twice", i, t.Name)
		}
		tools[t.Name] = true
		if t.Provider == "" {
			add("spec.tools[%d] (%s) names no provider", i, t.Name)
		}
	}
	for i, b := range sp.RBAC.Bindings {
		for _, c := range b.Tools {
			if _, err := path.Match(c, ""); err != nil {
				add("spec.rbac.bindings[%d].tools: %q: %v", i, c, err)
			}
		}
	}
	for i, g := range sp.Git.Credentials {
		if g.Host == "" {
			add("spec.git.credentials[%d].host is empty", i)
		}
		if g.SSHKeyFile != "" && g.KnownHostsFile == "" {
			add("spec.git.credentials[%d] (%s): sshKeyFile needs knownHostsFile (the host keys to trust)", i, g.Host)
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
