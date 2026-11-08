package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hayone1/gitopsify/pkg/config"
)

func TestSealer(t *testing.T) {
	s, _ := newSealer([]byte("0123456789abcdef0123456789abcdef"))
	v, err := s.seal("a", session{User: User{Username: "ada"}, Expires: 1})
	if err != nil {
		t.Fatal(err)
	}
	var got session
	if err := s.open("a", v, &got); err != nil || got.User.Username != "ada" {
		t.Fatalf("%+v %v", got, err)
	}
	if err := s.open("b", v, &got); err == nil {
		t.Error("a cookie sealed for one purpose opened for another")
	}
	tampered := []byte(v)
	tampered[len(tampered)/2] ^= 1
	if err := s.open("a", string(tampered), &got); err == nil {
		t.Error("a tampered cookie opened")
	}
	other, _ := newSealer([]byte("another key, also thirty-two bytes!"))
	if err := other.open("a", v, &got); err == nil {
		t.Error("another key opened the cookie")
	}
}

func TestSessionKey(t *testing.T) {
	if _, ok, err := sessionKey(config.SettingValue{}); err != nil || ok {
		t.Errorf("no key: random, not persistent (%v %v)", ok, err)
	}
	if k, ok, err := sessionKey(config.SettingValue{Value: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}); err != nil || !ok || len(k) != 32 {
		t.Errorf("base64: %d %v %v", len(k), ok, err)
	}
	if _, _, err := sessionKey(config.SettingValue{Value: "short"}); err == nil {
		t.Error("a short key was accepted")
	}
}

func TestUserFromClaims(t *testing.T) {
	c := &config.ServerOIDC{GroupsClaim: "groups"}
	u, err := userFromClaims(c, map[string]any{"sub": "1", "email": "ada@acme.io", "email_verified": true, "name": "Ada", "groups": []any{"ops", "dev"}})
	if err != nil || u.Username != "ada@acme.io" || u.Name != "Ada" || strings.Join(u.Groups, ",") != "ops,dev" {
		t.Fatalf("%+v %v", u, err)
	}
	// An unverified email doesn't identify anyone, nor one nobody said was
	// verified (Entra ID sends no email_verified).
	u, _ = userFromClaims(c, map[string]any{"sub": "1", "email": "ada@acme.io", "email_verified": false, "preferred_username": "ada"})
	if u.Username != "ada" || u.Email != "" {
		t.Errorf("unverified email: %+v", u)
	}
	u, _ = userFromClaims(c, map[string]any{"sub": "7", "email": "alice@corp.com"})
	if u.Username != "7" || u.Email != "" {
		t.Errorf("email without email_verified: %+v", u)
	}
	trusting := &config.ServerOIDC{GroupsClaim: "groups", TrustUnverifiedEmail: true}
	if u, _ := userFromClaims(trusting, map[string]any{"sub": "7", "email": "alice@corp.com"}); u.Username != "alice@corp.com" {
		t.Errorf("trustUnverifiedEmail: %+v", u)
	}
	for _, name := range []string{"local", "token:ci", "system:admin"} {
		if _, err := userFromClaims(c, map[string]any{"sub": name}); err == nil {
			t.Errorf("reserved username %q signed in", name)
		}
	}
	c.AllowedDomains = []string{"acme.io"}
	if _, err := userFromClaims(c, map[string]any{"sub": "1", "email": "eve@evil.io"}); err == nil {
		t.Error("an email outside the allowed domains signed in")
	}
	if _, err := userFromClaims(c, map[string]any{"sub": "2"}); err == nil {
		t.Error("no email with allowed domains signed in")
	}
	c = &config.ServerOIDC{GroupsClaim: "roles", UsernameClaim: "upn"}
	u, _ = userFromClaims(c, map[string]any{"sub": "1", "upn": "ada@corp", "roles": "a, b,"})
	if u.Username != "ada@corp" || strings.Join(u.Groups, ",") != "a,b" {
		t.Errorf("custom claims: %+v", u)
	}
}

func TestSafeRedirect(t *testing.T) {
	for in, want := range map[string]string{
		"/runs/1": "/runs/1", "": "/", "https://evil.io": "/", "//evil.io": "/", "/\\evil.io": "/", "/a\r\nSet-Cookie: x": "/",
		"/\t/evil.example": "/", "/ /evil.example": "/ /evil.example", "/runs?x=1": "/runs?x=1", "/\x00x": "/",
	} {
		if got := safeRedirect(in); got != want {
			t.Errorf("safeRedirect(%q) = %q", in, got)
		}
	}
}

func TestPolicy(t *testing.T) {
	p := &policy{def: config.RoleViewer, bindings: []config.ServerBinding{
		{Role: config.RoleAdmin, Groups: []string{"platform"}},
		{Role: config.RoleOperator, Groups: []string{"team-*"}, Clusters: []string{"staging-*"}},
		{Role: config.RoleOperator, Users: []string{"Ada@Acme.io"}, Clusters: []string{"prod"}},
	}}
	ada := &User{Username: "ada@acme.io", Groups: []string{"team-shop"}, Via: "oidc"}
	g := p.grants(ada, nil)
	for cluster, want := range map[string]string{"staging-eu": "operator", "prod": "operator", "dev": "viewer"} {
		if got := roleOn(g, cluster); got != want {
			t.Errorf("ada on %s: %q, want %q", cluster, got, want)
		}
	}
	if serverRole(g) != config.RoleViewer {
		t.Errorf("ada's server role: %q", serverRole(g))
	}
	admin := p.grants(&User{Username: "bob", Groups: []string{"platform"}, Via: "oidc"}, nil)
	if serverRole(admin) != config.RoleAdmin {
		t.Error("platform isn't admin")
	}
	noone := (&policy{}).grants(&User{Username: "x", Via: "oidc"}, nil)
	if anyRole(noone) != "" {
		t.Error("someone without bindings got a role")
	}
	tok := p.grants(&User{Username: "token:ci", Via: "token"}, []config.ServerToken{{Name: "ci", Role: "operator", Clusters: []string{"dev"}}})
	if roleOn(tok, "dev") != "operator" || roleOn(tok, "prod") != "" || serverRole(tok) != "" {
		t.Errorf("token grants: %+v", tok)
	}
	if got := p.relevantGroups([]string{"team-shop", "random", "platform"}); strings.Join(got, ",") != "team-shop,platform" {
		t.Errorf("relevant groups: %v", got)
	}
}

func TestGitEnv(t *testing.T) {
	t.Setenv("GH_TOKEN_TEST", "s3cret")
	g := config.ServerGit{Credentials: []config.GitCredential{
		{Host: "github.com", Username: "x-access-token", Token: config.SettingValue{Env: "GH_TOKEN_TEST"}},
		{Host: "gitlab.acme.io", SSHKeyFile: "/keys/id", KnownHostsFile: "/keys/known_hosts"},
	}}
	env, err := gitEnv(g, "https://github.com/acme/gitops.git")
	all := strings.Join(env, "\n")
	if err != nil || !strings.Contains(all, "Authorization: Basic eC1hY2Nlc3MtdG9rZW46czNjcmV0") {
		t.Fatalf("https: %v %v", env, err)
	}
	for _, want := range []string{"GIT_PROTOCOL_FROM_USER=0", "=protocol.allow\nGIT_CONFIG_VALUE_0=never", "core.symlinks", "http.lowSpeedTime"} {
		if !strings.Contains(all, want) {
			t.Errorf("env lacks %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "protocol.http.allow") {
		t.Error("http allowed by default")
	}
	env, _ = gitEnv(g, "git@gitlab.acme.io:team/gitops.git")
	all = strings.Join(env, "\n")
	if !strings.Contains(all, "-i '/keys/id'") || !strings.Contains(all, "StrictHostKeyChecking=yes") || !strings.Contains(all, "ConnectTimeout") {
		t.Errorf("ssh: %v", env)
	}
	if env, _ := gitEnv(g, "https://example.com/x.git"); strings.Contains(strings.Join(env, " "), "Authorization") {
		t.Errorf("credentials sent to another host: %v", env)
	}
	if _, err := gitEnv(config.ServerGit{Credentials: []config.GitCredential{{Host: "github.com"}}}, "ssh://git@github.com/acme/x.git"); err == nil {
		t.Error("ssh without a key")
	}
}

func TestCheckRemote(t *testing.T) {
	for remote, ok := range map[string]bool{
		"https://github.com/acme/gitops.git":      true,
		"ssh://git@github.com/acme/gitops.git":    true,
		"git@github.com:acme/gitops.git":          true,
		"--upload-pack=touch /tmp/pwned;":         false,
		"-u":                                      false,
		"file:///var/lib/gitopsify/runs/x/output": false,
		"/var/lib/gitopsify":                      false,
		"ext::sh -c id":                           false,
		"https://user:pass@github.com/acme/x":     false,
		"http://github.com/acme/x":                false,
		"https://-oProxyCommand=x/acme/x":         false,
		"git@github.com:acme/x.git --x":           false,
		"https://github.com/acme/x?y=1":           false,
		"":                                        false,
	} {
		if err := checkRemote(remote, false); (err == nil) != ok {
			t.Errorf("checkRemote(%q) = %v, want ok %v", remote, err, ok)
		}
	}
	if err := checkRemote("http://localhost:8099/x.git", true); err != nil {
		t.Errorf("http when allowed: %v", err)
	}
	for b, ok := range map[string]bool{"main": true, "release/1.0": true, "": true, "-f": false, "a..b": false, "x.lock": false, "a b": false, "/x": false} {
		if err := checkBranch(b); (err == nil) != ok {
			t.Errorf("checkBranch(%q) = %v", b, err)
		}
	}
}

func TestAllowedRepo(t *testing.T) {
	rules := []config.GitRepository{
		{URL: "https://github.com/acme/shop-*", Clusters: []string{"shop-*"}},
		{URL: "https://github.com/acme/grafana-iac.git", Tools: []string{"grafana-*"}},
		{URL: "https://github.com/acme/platform"},
	}
	for _, c := range []struct {
		remote, cluster, tool string
		ok                    bool
	}{
		{"https://github.com/acme/shop-gitops.git", "shop-prod", "", true},
		{"https://github.com/acme/shop-gitops.git", "payments", "", false},
		{"https://github.com/acme/payments-gitops.git", "shop-prod", "", false},
		{"https://github.com/acme/grafana-iac", "", "grafana-prod", true},
		{"https://github.com/acme/grafana-iac.git", "shop-prod", "", false},
		{"https://github.com/acme/platform.git", "anything", "", true},
	} {
		if err := allowedRepo(rules, c.remote, c.cluster, c.tool); (err == nil) != c.ok {
			t.Errorf("%s from %s%s: %v", c.remote, c.cluster, c.tool, err)
		}
	}
	if allowedRepo(nil, "https://x/y", "a", "") != nil {
		t.Error("no rules: everything is allowed")
	}
}

func TestNoSymlinks(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "apps"), 0o755)
	if err := noSymlinks(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/var/lib", filepath.Join(dir, "apps", "evil")); err != nil {
		t.Skip(err)
	}
	if err := noSymlinks(dir); err == nil {
		t.Error("a symlink went unnoticed")
	}
}

func TestImpersonation(t *testing.T) {
	s := testServer(t, func(c *config.Server) {
		c.Spec.Clusters[0].ImpersonateUserPrefix = "oidc:"
		c.Spec.Clusters[0].ImpersonateGroupPrefix = "oidc:"
	})
	c, _ := s.cluster("dev")
	a, err := s.access(c, &User{Username: "ada@acme.io", Groups: []string{"shop", "system:masters"}, Via: "oidc"})
	if err != nil || a.Impersonate != "oidc:ada@acme.io" || strings.Join(a.ImpersonateGroups, ",") != "system:authenticated,oidc:shop" {
		t.Errorf("impersonation: %q %v %v", a.Impersonate, a.ImpersonateGroups, err)
	}
	if _, err := s.access(c, &User{Username: "system:serviceaccount:kube-system:x", Via: "oidc"}); err == nil {
		t.Error("a system: username was impersonated")
	}
}

func TestClientIP(t *testing.T) {
	s := testServer(t, func(c *config.Server) { c.Spec.TrustedProxies = []string{"10.0.0.0/8"} })
	for _, c := range []struct{ remote, xff, want string }{
		{"203.0.113.9:1234", "1.2.3.4", "203.0.113.9"},              // not a proxy: the header is ignored
		{"10.1.2.3:1234", "198.51.100.7", "198.51.100.7"},           // one proxy
		{"10.1.2.3:1234", "6.6.6.6, 198.51.100.7", "198.51.100.7"},  // a forged first entry
		{"10.1.2.3:1234", "198.51.100.7, 10.9.9.9", "198.51.100.7"}, // two proxies
		{"10.1.2.3:1234", "", "10.1.2.3"},
	} {
		r, _ := http.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := s.clientIP(r); got != c.want {
			t.Errorf("%s via %s: %s, want %s", c.xff, c.remote, got, c.want)
		}
	}
}

func TestLocalTokenOnce(t *testing.T) {
	a := &authenticator{local: "tok"}
	if !a.useLocalToken("tok") || a.useLocalToken("tok") || a.useLocalToken("") {
		t.Error("the local link must work exactly once")
	}
}

func TestMetricsMethods(t *testing.T) {
	m := newMetrics()
	for i := range 50 {
		m.request("X"+strings.Repeat("Y", i), "ui", 405, time.Millisecond)
	}
	if len(m.requests) != 1 {
		t.Errorf("%d series from invented methods", len(m.requests))
	}
}

func TestServerConfig(t *testing.T) {
	s := &config.Server{Spec: config.ServerSpec{ExternalURL: "https://gitopsify.acme.io",
		Auth: config.ServerAuth{OIDC: &config.ServerOIDC{Issuer: "https://idp", ClientID: "gitopsify"}}}}
	s.Default()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Spec.Auth.Mode != config.AuthOIDC || s.Spec.Listen != ":8080" || s.Spec.Runs.Concurrency != 2 {
		t.Errorf("defaults: %+v", s.Spec)
	}
	bad := *s
	bad.Spec.RBAC = config.ServerRBAC{DefaultRole: "root", Bindings: []config.ServerBinding{{Role: "admin"}}}
	bad.Spec.Auth.Tokens = []config.ServerToken{{Name: "ci", SHA256: "nope", Role: "admin"}}
	bad.Spec.Clusters = []config.ServerCluster{{Name: "Prod"}, {Name: "Prod"}}
	err := bad.Validate()
	for _, want := range []string{"defaultRole", "names no users or groups", "sha256", "lowercase", "used twice"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error about %s, got %v", want, err)
		}
	}
	noURL := config.Server{Spec: config.ServerSpec{Auth: config.ServerAuth{OIDC: &config.ServerOIDC{Issuer: "x", ClientID: "y"}}}}
	noURL.Default()
	if err := noURL.Validate(); err == nil || !strings.Contains(err.Error(), "externalURL") {
		t.Errorf("OIDC without externalURL: %v", err)
	}
}

func TestPrepareMigration(t *testing.T) {
	s := testServer(t, nil)
	u := &User{Username: "ada", Via: "oidc"}
	cases := map[string]string{
		"source access":     "spec:\n  source:\n    kubeconfig: /etc/passwd\n",
		"env access":        "spec:\n  environments:\n    - name: prod\n      cluster:\n        tokenFile: /var/run/secrets/token\n",
		"unknown cluster":   "spec:\n  environments:\n    - name: prod\n      cluster:\n        context: nope\n",
		"repo path":         "spec:\n  output:\n    repo:\n      path: /tmp\n",
		"cert file":         "spec:\n  secrets:\n    sealedSecrets:\n      certFile: /etc/ssl/key.pem\n",
		"plain secrets":     "spec:\n  secrets:\n    strategy: plain\n",
		"override secrets":  "spec:\n  secrets:\n    overrides:\n      - names: [x]\n        strategy: local\n",
		"sealed, no target": "spec:\n  secrets:\n    strategy: sealed-secrets\n",
	}
	for name, spec := range cases {
		if _, err := s.prepareMigration("apiVersion: gitopsify.io/v1alpha1\nkind: Migration\nmetadata: {name: m}\n"+spec, "dev", u); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	cfg, err := s.prepareMigration("apiVersion: gitopsify.io/v1alpha1\nkind: Migration\nmetadata: {name: m}\nspec:\n  environments:\n    - name: prod\n      cluster:\n        context: prod\n", "dev", u)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source.Access.APIServer != "https://127.0.0.1:1" || cfg.Environments[0].Cluster.Access.APIServer != "https://127.0.0.1:2" {
		t.Errorf("access not filled in: %+v", cfg.Source.Access)
	}
	if cfg.Source.Access.Impersonate != "ada" || cfg.Environments[0].Cluster.Access.Impersonate != "" {
		t.Errorf("impersonation: dev %q, prod %q", cfg.Source.Access.Impersonate, cfg.Environments[0].Cluster.Access.Impersonate)
	}
}
