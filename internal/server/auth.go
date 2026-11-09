package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/hayone1/gitopsify/pkg/config"
)

// User is who a request acts for.
type User struct {
	// Username identifies the person in RBAC bindings, audit events and
	// Kubernetes impersonation (email, preferred_username or sub).
	Username string   `json:"username"`
	Name     string   `json:"name,omitempty"`
	Email    string   `json:"email,omitempty"`
	Groups   []string `json:"groups,omitempty"`
	// Via is how they signed in: local, oidc or token.
	Via string `json:"via"`
}

const (
	sessionCookie = "gitopsify_session"
	loginCookie   = "gitopsify_login"
	csrfHeader    = "X-Gitopsify-CSRF"
)

// session is what the session cookie holds, encrypted.
type session struct {
	User    User   `json:"u"`
	Expires int64  `json:"e"`
	CSRF    string `json:"c"`
}

// loginState is what the short-lived login cookie holds during an OIDC
// sign-in.
type loginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Redirect string `json:"r"`
	Expires  int64  `json:"e"`
}

// sealer encrypts and authenticates cookie values (AES-256-GCM).
type sealer struct{ aead cipher.AEAD }

func newSealer(key []byte) (*sealer, error) {
	k := sha256.Sum256(append([]byte("gitopsify-session-v1:"), key...))
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sealer{aead: aead}, nil
}

func (s *sealer) seal(purpose string, v any) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := s.aead.Seal(nonce, nonce, plain, []byte(purpose))
	return base64.RawURLEncoding.EncodeToString(out), nil
}

func (s *sealer) open(purpose, value string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return errors.New("malformed cookie")
	}
	n := s.aead.NonceSize()
	plain, err := s.aead.Open(nil, raw[:n], raw[n:], []byte(purpose))
	if err != nil {
		return errors.New("invalid cookie")
	}
	return json.Unmarshal(plain, v)
}

// sessionKey decodes the configured key, or makes a random one.
func sessionKey(v config.SettingValue) ([]byte, bool, error) {
	if v.Value == "" && v.Env == "" && v.File == "" {
		k := make([]byte, 32)
		_, err := rand.Read(k)
		return k, false, err
	}
	s, err := v.Resolve()
	if err != nil {
		return nil, false, fmt.Errorf("spec.auth.sessionKey: %w", err)
	}
	s = strings.TrimSpace(s)
	for _, dec := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString, base64.RawStdEncoding.DecodeString,
		base64.URLEncoding.DecodeString, base64.RawURLEncoding.DecodeString, hex.DecodeString,
	} {
		if b, err := dec(s); err == nil && len(b) >= 32 {
			return b, true, nil
		}
	}
	if len(s) >= 32 {
		return []byte(s), true, nil
	}
	return nil, false, errors.New("spec.auth.sessionKey must hold at least 32 bytes (openssl rand -base64 32)")
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// TokenHash is the SHA-256 of an API token, as spec.auth.tokens stores it.
func TokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// NewToken makes a random API token.
func NewToken() string { return "gtk_" + randomString(32) }

// authenticator signs people in and finds the user of a request.
type authenticator struct {
	mode     string
	sealer   *sealer
	ttl      time.Duration
	secure   bool
	tokens   []config.ServerToken
	local    string // the local mode's sign-in token, until it is used
	localMu  sync.Mutex
	oidc     *oidcLogin
	external *url.URL
	// cookie is the session cookie's name. Cookies ignore ports: local mode
	// adds its port, so two gitopsify ui on one machine don't sign each
	// other out.
	cookie string
}

func (a *authenticator) cookieName() string {
	if a.cookie != "" {
		return a.cookie
	}
	return sessionCookie
}

type oidcLogin struct {
	cfg      *config.ServerOIDC
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
	client   *http.Client
}

func newOIDC(ctx context.Context, c *config.ServerOIDC, external *url.URL) (*oidcLogin, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("spec.auth.oidc.caFile: %w", err)
		}
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("spec.auth.oidc.caFile %s holds no PEM certificates", c.CAFile)
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		client = &http.Client{Transport: tr, Timeout: 30 * time.Second}
	}
	ctx = oidc.ClientContext(ctx, client)
	provider, err := oidc.NewProvider(ctx, c.Issuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC issuer %s: %w", c.Issuer, err)
	}
	secret := ""
	if c.ClientSecret != (config.SettingValue{}) {
		if secret, err = c.ClientSecret.Resolve(); err != nil {
			return nil, fmt.Errorf("spec.auth.oidc.clientSecret: %w", err)
		}
	}
	scopes := append([]string{oidc.ScopeOpenID}, c.Scopes...)
	redirect := *external
	redirect.Path = strings.TrimSuffix(redirect.Path, "/") + "/api/v1/auth/callback"
	return &oidcLogin{
		cfg:      c,
		provider: provider,
		verifier: provider.Verifier(&oidc.Config{ClientID: c.ClientID}),
		oauth: oauth2.Config{ClientID: c.ClientID, ClientSecret: secret, Endpoint: provider.Endpoint(),
			RedirectURL: redirect.String(), Scopes: scopes},
		client: client,
	}, nil
}

// fromRequest returns the user of a request and their session (nil for API
// tokens), or nil when nobody is signed in.
func (a *authenticator) fromRequest(r *http.Request) (*User, *session, []config.ServerToken) {
	// The scheme is case-insensitive (RFC 9110).
	if scheme, rest, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
		tok := strings.TrimSpace(rest)
		hash := TokenHash(tok)
		for _, t := range a.tokens {
			if subtle.ConstantTimeCompare([]byte(hash), []byte(strings.ToLower(t.SHA256))) == 1 {
				return &User{Username: "token:" + t.Name, Name: t.Name, Via: "token"}, nil, []config.ServerToken{t}
			}
		}
		return nil, nil, nil
	}
	c, err := r.Cookie(a.cookieName())
	if err != nil {
		return nil, nil, nil
	}
	var s session
	if err := a.sealer.open(a.cookieName(), c.Value, &s); err != nil || time.Now().Unix() > s.Expires {
		return nil, nil, nil
	}
	return &s.User, &s, nil
}

func (a *authenticator) setSession(w http.ResponseWriter, u User) (*session, error) {
	s := &session{User: u, Expires: time.Now().Add(a.ttl).Unix(), CSRF: randomString(24)}
	v, err := a.sealer.seal(a.cookieName(), s)
	if err != nil {
		return nil, err
	}
	if len(v) > 3800 {
		return nil, errors.New("too many groups to fit in a session cookie; narrow the groups claim or the scopes")
	}
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(), Value: v, Path: "/", HttpOnly: true, Secure: a.secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(a.ttl.Seconds())})
	return s, nil
}

func (a *authenticator) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(), Value: "", Path: "/", HttpOnly: true, Secure: a.secure,
		SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

// safeRedirect keeps only same-site paths, so the login can't be used to
// send people elsewhere. Browsers drop tabs and newlines and read
// backslashes as slashes, so those are refused outright.
func safeRedirect(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\") ||
		strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "/"
	}
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || strings.HasPrefix(u.Path, "//") || strings.HasPrefix(p, "//") {
		return "/"
	}
	return p
}

// startLogin redirects to the OIDC provider.
func (a *authenticator) startLogin(w http.ResponseWriter, r *http.Request) error {
	st := loginState{State: randomString(24), Nonce: randomString(24), Verifier: oauth2.GenerateVerifier(),
		Redirect: safeRedirect(r.URL.Query().Get("redirect")), Expires: time.Now().Add(10 * time.Minute).Unix()}
	v, err := a.sealer.seal(loginCookie, st)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Value: v, Path: "/api/v1/auth", HttpOnly: true, Secure: a.secure,
		SameSite: http.SameSiteLaxMode, MaxAge: 600})
	u := a.oidc.oauth.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier))
	http.Redirect(w, r, u, http.StatusFound)
	return nil
}

// loginError is a failed sign-in: a code the login page turns into a fixed
// message (it never shows text from the URL), and the details for the
// audit log.
type loginError struct {
	code string
	err  error
}

func (e *loginError) Error() string { return e.err.Error() }

func loginFailed(code string, format string, args ...any) error {
	return &loginError{code: code, err: fmt.Errorf(format, args...)}
}

// finishLogin handles the provider's redirect back and returns the user and
// where to send them.
func (a *authenticator) finishLogin(w http.ResponseWriter, r *http.Request) (*User, string, error) {
	c, err := r.Cookie(loginCookie)
	if err != nil {
		return nil, "", loginFailed("expired", "the sign-in expired or cookies are blocked")
	}
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Value: "", Path: "/api/v1/auth", MaxAge: -1, HttpOnly: true, Secure: a.secure})
	var st loginState
	if err := a.sealer.open(loginCookie, c.Value, &st); err != nil || time.Now().Unix() > st.Expires {
		return nil, "", loginFailed("expired", "the sign-in expired")
	}
	q := r.URL.Query()
	// State first: any page can send a browser here with an error.
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		return nil, "", loginFailed("expired", "the sign-in state doesn't match")
	}
	if e := q.Get("error"); e != "" {
		return nil, "", loginFailed("refused", "the identity provider refused the sign-in: %.100s %.300s", e, q.Get("error_description"))
	}
	ctx := oidc.ClientContext(r.Context(), a.oidc.client)
	tok, err := a.oidc.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		return nil, "", loginFailed("token", "exchanging the code: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return nil, "", loginFailed("token", "the identity provider returned no ID token")
	}
	idt, err := a.oidc.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, "", loginFailed("token", "verifying the ID token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(st.Nonce)) != 1 {
		return nil, "", loginFailed("token", "the ID token's nonce doesn't match")
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return nil, "", loginFailed("token", "reading the ID token: %w", err)
	}
	u, err := userFromClaims(a.oidc.cfg, claims)
	if err != nil {
		return nil, "", err
	}
	return u, st.Redirect, nil
}

// userFromClaims maps ID token claims to a user.
func userFromClaims(c *config.ServerOIDC, claims map[string]any) (*User, error) {
	str := func(k string) string { s, _ := claims[k].(string); return s }
	email := str("email")
	if verified, _ := claims["email_verified"].(bool); !verified && !c.TrustUnverifiedEmail {
		email = "" // an address nobody verified identifies nobody
	}
	u := &User{Name: str("name"), Email: email, Via: "oidc"}
	switch {
	case c.UsernameClaim == "email":
		// The verified address only: anyone may type an address.
		if email == "" {
			return nil, loginFailed("claims", "the ID token's email isn't verified (email_verified), and usernameClaim is email")
		}
		u.Username = email
	case c.UsernameClaim != "":
		u.Username = str(c.UsernameClaim)
	case email != "":
		u.Username = email
	case str("preferred_username") != "":
		u.Username = str("preferred_username")
	default:
		u.Username = str("sub")
	}
	if u.Username == "" {
		return nil, loginFailed("claims", "the ID token has no %s claim", firstNonEmpty(c.UsernameClaim, "verified email, preferred_username or sub"))
	}
	// Names the server uses itself, and Kubernetes' reserved ones.
	if u.Username == "local" || strings.HasPrefix(u.Username, "token:") || strings.HasPrefix(u.Username, "system:") {
		return nil, loginFailed("claims", "the username %q is reserved", u.Username)
	}
	if u.Name == "" {
		u.Name = u.Username
	}
	switch g := claims[c.GroupsClaim].(type) {
	case []any:
		for _, v := range g {
			if s, ok := v.(string); ok {
				u.Groups = append(u.Groups, s)
			}
		}
	case string:
		for _, g := range strings.Split(g, ",") {
			if g = strings.TrimSpace(g); g != "" {
				u.Groups = append(u.Groups, g)
			}
		}
	}
	if len(c.AllowedDomains) > 0 {
		_, domain, ok := strings.Cut(email, "@")
		if !ok || !slices.ContainsFunc(c.AllowedDomains, func(d string) bool { return strings.EqualFold(d, domain) }) {
			return nil, loginFailed("domain", "%s isn't in an allowed email domain (or not verified)", firstNonEmpty(email, u.Username))
		}
	}
	return u, nil
}

// checkCSRF checks the CSRF header of a cookie-authenticated request that
// changes something. API tokens aren't sent automatically by browsers, so
// they need none.
func checkCSRF(r *http.Request, s *session) bool {
	if s == nil {
		return true
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	h := r.Header.Get(csrfHeader)
	return h != "" && subtle.ConstantTimeCompare([]byte(h), []byte(s.CSRF)) == 1
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// useLocalToken accepts the local mode's sign-in token once.
func (a *authenticator) useLocalToken(t string) bool {
	a.localMu.Lock()
	defer a.localMu.Unlock()
	if a.local == "" || !constantEq(t, a.local) {
		return false
	}
	a.local = ""
	return true
}
