package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/hayone1/gitopsify/pkg/config"
)

// fakeIDP is an OpenID Connect provider for tests: it signs in whoever the
// test says, with the claims it gives.
type fakeIDP struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	next   map[string]any            // claims of the next sign-in
	codes  map[string]map[string]any // code -> claims (with nonce)
	pkce   map[string]string         // code -> code_challenge
	issuer string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIDP{t: t, key: key, codes: map[string]map[string]any{}, pkce: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.issuer, "authorization_endpoint": f.issuer + "/auth", "token_endpoint": f.issuer + "/token",
			"jwks_uri": f.issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		claims := f.next
		f.mu.Unlock()
		if claims == nil {
			http.Redirect(w, r, q.Get("redirect_uri")+"?error=access_denied&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
			return
		}
		c := map[string]any{}
		for k, v := range claims {
			c[k] = v
		}
		c["nonce"] = q.Get("nonce")
		code := randomString(12)
		f.mu.Lock()
		f.codes[code] = c
		f.pkce[code] = q.Get("code_challenge")
		f.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		code := r.PostForm.Get("code")
		f.mu.Lock()
		claims, ok := f.codes[code]
		challenge := f.pkce[code]
		delete(f.codes, code)
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		claims["iss"], claims["aud"] = f.issuer, "gitopsify"
		claims["iat"], claims["exp"] = time.Now().Unix(), time.Now().Add(time.Hour).Unix()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": f.sign(claims)})
	})
	f.srv = httptest.NewServer(mux)
	f.issuer = f.srv.URL
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIDP) sign(claims map[string]any) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		f.t.Fatal(err)
	}
	b, _ := json.Marshal(claims)
	obj, err := signer.Sign(b)
	if err != nil {
		f.t.Fatal(err)
	}
	s, _ := obj.CompactSerialize()
	return s
}

func (f *fakeIDP) as(claims map[string]any) {
	f.mu.Lock()
	f.next = claims
	f.mu.Unlock()
}

// testEnv is a server signed in to by a fake IdP.
type testEnv struct {
	s   *Server
	srv *httptest.Server
	idp *fakeIDP
}

// testServer starts a server with two clusters (dev impersonates), RBAC
// bindings and an API token. mutate changes the configuration first.
func testServer(t *testing.T, mutate func(*config.Server)) *Server {
	return newTestEnv(t, mutate).s
}

const testToken = "gtk_test_token_for_ci"

func newTestEnv(t *testing.T, mutate func(*config.Server)) *testEnv {
	t.Helper()
	idp := newFakeIDP(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	external := "http://" + ln.Addr().String()
	cfg := &config.Server{Spec: config.ServerSpec{
		ExternalURL: external, DataDir: t.TempDir(),
		Auth: config.ServerAuth{OIDC: &config.ServerOIDC{Issuer: idp.issuer, ClientID: "gitopsify", ClientSecret: config.SettingValue{Value: "secret"}},
			SessionKey: config.SettingValue{Value: "0123456789abcdef0123456789abcdef"},
			Tokens:     []config.ServerToken{{Name: "ci", SHA256: TokenHash(testToken), Role: "operator", Clusters: []string{"dev"}}}},
		RBAC: config.ServerRBAC{DefaultRole: "viewer", Bindings: []config.ServerBinding{
			{Role: "admin", Groups: []string{"platform"}},
			{Role: "operator", Groups: []string{"shop"}, Clusters: []string{"dev"}},
		}},
		Clusters: []config.ServerCluster{
			{Name: "dev", Impersonate: true, Access: config.Access{APIServer: "https://127.0.0.1:1"}},
			{Name: "prod", Access: config.Access{APIServer: "https://127.0.0.1:2"}},
		},
	}}
	if mutate != nil {
		mutate(cfg)
	}
	cfg.Default()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s, err := New(context.Background(), Options{Config: cfg, Version: "test", Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: s.Handler()}}
	srv.Start()
	t.Cleanup(func() { srv.Close(); s.close(context.Background()) })
	return &testEnv{s: s, srv: srv, idp: idp}
}

// client is a browser: a cookie jar that follows redirects.
type client struct {
	t    *testing.T
	env  *testEnv
	http *http.Client
	csrf string
}

func (e *testEnv) anonymous() *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: e.t(), env: e, http: &http.Client{Jar: jar}}
}

func (e *testEnv) t() *testing.T { return e.idp.t }

// login signs in through the fake IdP and returns the browser.
func (e *testEnv) login(claims map[string]any) *client {
	c := e.anonymous()
	e.idp.as(claims)
	resp, err := c.http.Get(e.srv.URL + "/api/v1/auth/login?redirect=/runs")
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/runs" {
		c.t.Fatalf("sign-in ended at %s", resp.Request.URL)
	}
	var m me
	if code := c.get("/api/v1/me", &m); code != 200 {
		c.t.Fatalf("me: %d", code)
	}
	c.csrf = m.CSRF
	return c
}

func (c *client) do(method, path string, body any, out any, headers ...string) int {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, c.env.srv.URL+path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" && method != http.MethodGet {
		req.Header.Set(csrfHeader, c.csrf)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func (c *client) get(path string, out any) int { return c.do(http.MethodGet, path, nil, out) }

func TestOIDCLogin(t *testing.T) {
	e := newTestEnv(t, nil)
	c := e.login(map[string]any{"sub": "1", "email": "ada@acme.io", "email_verified": true, "name": "Ada", "groups": []string{"shop"}})
	var m me
	c.get("/api/v1/me", &m)
	if m.User.Username != "ada@acme.io" || m.User.Via != "oidc" || m.Clusters["dev"] != "operator" || m.Clusters["prod"] != "viewer" || m.CSRF == "" {
		t.Fatalf("me: %+v", m)
	}
	// The cookie is HttpOnly, SameSite and never readable by scripts.
	for _, ck := range c.http.Jar.Cookies(mustURL(e.srv.URL)) {
		if ck.Name == sessionCookie && ck.Value == "" {
			t.Error("empty session cookie")
		}
	}
	// Sign-out needs the CSRF header.
	if code := c.do(http.MethodPost, "/api/v1/auth/logout", nil, nil, csrfHeader, ""); code != http.StatusForbidden {
		t.Errorf("logout without CSRF: %d", code)
	}
	if code := c.do(http.MethodPost, "/api/v1/auth/logout", nil, nil); code != 200 {
		t.Errorf("logout: %d", code)
	}
	if code := c.get("/api/v1/me", nil); code != http.StatusUnauthorized {
		t.Errorf("after logout: %d", code)
	}
	evs, _ := e.s.audit.query(auditQuery{Action: "auth."})
	if len(evs) < 2 || evs[len(evs)-1].Action != "auth.login" || evs[len(evs)-1].Actor != "ada@acme.io" {
		t.Errorf("audit: %+v", evs)
	}
}

func TestOIDCRefusals(t *testing.T) {
	e := newTestEnv(t, nil)
	// The IdP refuses: back to the login page with the reason.
	c := e.anonymous()
	e.idp.as(nil)
	resp, err := c.http.Get(e.srv.URL + "/api/v1/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/login" || !strings.Contains(resp.Request.URL.RawQuery, "access_denied") {
		t.Errorf("refused sign-in ended at %s", resp.Request.URL)
	}
	// A callback without the login cookie (CSRF on the sign-in) is refused.
	other := e.anonymous()
	resp, _ = other.http.Get(e.srv.URL + "/api/v1/auth/callback?code=x&state=y")
	resp.Body.Close()
	if resp.Request.URL.Path != "/login" {
		t.Errorf("callback without a login: %s", resp.Request.URL)
	}
	// A forged session cookie gets nothing.
	forged := e.anonymous()
	forged.http.Jar.SetCookies(mustURL(e.srv.URL), []*http.Cookie{{Name: sessionCookie, Value: "eyJ1Ijp7InVzZXJuYW1lIjoiYWRtaW4ifX0"}})
	if code := forged.get("/api/v1/me", nil); code != http.StatusUnauthorized {
		t.Errorf("forged cookie: %d", code)
	}
}

func mustURL(s string) *url.URL {
	u, _ := url.Parse(s)
	return u
}
