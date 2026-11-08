package server

import (
	"strings"
	"testing"

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
	// An unverified email doesn't identify anyone.
	u, _ = userFromClaims(c, map[string]any{"sub": "1", "email": "ada@acme.io", "email_verified": false, "preferred_username": "ada"})
	if u.Username != "ada" || u.Email != "" {
		t.Errorf("unverified email: %+v", u)
	}
	c.AllowedDomains = []string{"acme.io"}
	if _, err := userFromClaims(c, map[string]any{"sub": "1", "email": "eve@evil.io"}); err == nil {
		t.Error("an email outside the allowed domains signed in")
	}
	if _, err := userFromClaims(c, map[string]any{"sub": "2"}); err == nil {
		t.Error("no email with allowed domains signed in")
	}
	c = &config.ServerOIDC{GroupsClaim: "roles", UsernameClaim: "upn"}
	u, _ = userFromClaims(c, map[string]any{"sub": "1", "upn": "ada@corp", "roles": "a,b"})
	if u.Username != "ada@corp" || len(u.Groups) != 2 {
		t.Errorf("custom claims: %+v", u)
	}
}

func TestSafeRedirect(t *testing.T) {
	for in, want := range map[string]string{
		"/runs/1": "/runs/1", "": "/", "https://evil.io": "/", "//evil.io": "/", "/\\evil.io": "/", "/a\r\nSet-Cookie: x": "/",
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
	creds := []config.GitCredential{
		{Host: "github.com", Username: "x-access-token", Token: config.SettingValue{Env: "GH_TOKEN_TEST"}},
		{Host: "gitlab.acme.io", SSHKeyFile: "/keys/id", KnownHostsFile: "/keys/known_hosts"},
	}
	env, err := gitEnv(creds, "https://github.com/acme/gitops.git")
	if err != nil || len(env) != 5 || !strings.Contains(strings.Join(env, "\n"), "Authorization: Basic eC1hY2Nlc3MtdG9rZW46czNjcmV0") {
		t.Fatalf("https: %v %v", env, err)
	}
	env, _ = gitEnv(creds, "git@gitlab.acme.io:team/gitops.git")
	if len(env) != 1 || !strings.Contains(env[0], "-i '/keys/id'") || !strings.Contains(env[0], "StrictHostKeyChecking=yes") {
		t.Errorf("ssh: %v", env)
	}
	if env, _ := gitEnv(creds, "https://example.com/x.git"); env != nil {
		t.Errorf("unknown host: %v", env)
	}
	if _, err := gitEnv([]config.GitCredential{{Host: "github.com"}}, "ssh://git@github.com/acme/x.git"); err == nil {
		t.Error("ssh without a key")
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
		"source access":    "spec:\n  source:\n    kubeconfig: /etc/passwd\n",
		"env access":       "spec:\n  environments:\n    - name: prod\n      cluster:\n        tokenFile: /var/run/secrets/token\n",
		"unknown cluster":  "spec:\n  environments:\n    - name: prod\n      cluster:\n        context: nope\n",
		"repo path":        "spec:\n  output:\n    repo:\n      path: /tmp\n",
		"cert file":        "spec:\n  secrets:\n    sealedSecrets:\n      certFile: /etc/ssl/key.pem\n",
		"plain secrets":    "spec:\n  secrets:\n    strategy: plain\n",
		"override secrets": "spec:\n  secrets:\n    overrides:\n      - names: [x]\n        strategy: local\n",
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
