// Package server is gitopsify's web server: the API the web UI uses, with
// sign-in (local or OpenID Connect), roles per cluster, an audit log, and
// runs (scans and migrations) that work in the background and stream their
// progress.
package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/time/rate"

	"github.com/hayone1/gitopsify/internal/telemetry"
	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/provider"
	"github.com/hayone1/gitopsify/sdk"
)

// Options configure a Server.
type Options struct {
	Config *config.Server
	// Local is gitopsify ui: one user, signed in with LocalToken, clusters
	// from the kubeconfig.
	Local      bool
	LocalToken string
	Kubeconfig string
	Version    string
	Log        *slog.Logger
	// UI is the built web app (index.html at its root); nil serves a note
	// that this build has none.
	UI fs.FS
}

// Server is the web server.
type Server struct {
	cfg        *config.ServerSpec
	local      bool
	kubeconfig string
	version    string
	log        *slog.Logger
	auth       *authenticator
	policy     *policy
	audit      *auditLog
	runs       *runs
	metrics    *metrics
	ui         *uiHandler
	limits     *limiter
	descs      *describes
	proxies    []*net.IPNet
	metricsKey string
	store      store
	notify     *notifier
	handler    http.Handler
	stop       chan struct{}
	stopOnce   sync.Once
	closeOnce  sync.Once
	// pushes counts pushes requested through the API; shutdown waits for
	// them. pushMu orders their Add before its Wait.
	pushes     sync.WaitGroup
	pushMu     sync.Mutex
	pushesShut bool
}

// New builds a server: it connects to the OIDC provider, opens the data
// directory and loads earlier runs.
func New(ctx context.Context, o Options) (*Server, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	cfg := &o.Config.Spec
	s := &Server{cfg: cfg, local: o.Local, kubeconfig: o.Kubeconfig, version: o.Version, log: o.Log, metrics: newMetrics(),
		stop: make(chan struct{})}
	key, persistent, err := sessionKey(cfg.Auth.SessionKey)
	if err != nil {
		return nil, err
	}
	if !persistent && !o.Local {
		o.Log.Warn("spec.auth.sessionKey isn't set: sign-ins end when the server restarts and don't work across replicas")
	}
	sl, err := newSealer(key)
	if err != nil {
		return nil, err
	}
	external, _ := url.Parse(cfg.ExternalURL)
	if external == nil || external.Host == "" {
		external = &url.URL{Scheme: "http", Host: "localhost" + cfg.Listen}
	}
	s.auth = &authenticator{mode: cfg.Auth.Mode, sealer: sl, ttl: config.Duration(cfg.Auth.SessionTTL),
		secure: external.Scheme == "https", tokens: cfg.Auth.Tokens, local: o.LocalToken, external: external}
	if o.Local {
		s.auth.mode = config.AuthLocal
	}
	if s.auth.mode == config.AuthLocal && external.Port() != "" {
		s.auth.cookie = sessionCookie + "_" + external.Port()
	}
	if s.auth.mode == config.AuthOIDC {
		if s.auth.oidc, err = newOIDC(ctx, cfg.Auth.OIDC, external); err != nil {
			return nil, err
		}
	}
	s.policy = &policy{local: s.auth.mode == config.AuthLocal, def: cfg.RBAC.DefaultRole, bindings: cfg.RBAC.Bindings}
	for _, h := range cfg.Notifications {
		if _, err := webhookURL(h); err != nil {
			return nil, fmt.Errorf("spec.notifications %s: %w", h.Name, err)
		}
	}
	s.notify = &notifier{hooks: cfg.Notifications, external: external.String(), s: s, client: &http.Client{
		Timeout: 15 * time.Second,
		// A webhook that redirects is misconfigured; following it would
		// send the payload elsewhere.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	if s.store, err = openStore(ctx, cfg); err != nil {
		return nil, err
	}
	s.audit = newAuditLog(s.store, cfg.Audit.Stdout, config.Duration(cfg.Audit.Retention), o.Log)
	if s.runs, err = newRuns(s.store, filepath.Join(cfg.DataDir, "runs"), cfg.Runs.Concurrency, config.Duration(cfg.Runs.Timeout),
		config.Duration(cfg.Runs.Retention), o.Log, s.metrics, s.notify.runFinished); err != nil {
		_ = s.store.close()
		return nil, fmt.Errorf("data directory %s: %w", cfg.DataDir, err)
	}
	if len(cfg.Git.Repositories) == 0 && len(cfg.Git.Credentials) > 0 && scoped(cfg) {
		o.Log.Warn("roles are limited to clusters or tools, but spec.git.repositories is empty: " +
			"an operator of any cluster or tool can push to every repository the server's Git credentials reach")
	}
	s.ui = newUIHandler(o.UI)
	s.descs = &describes{byID: map[string]*sdk.Description{}}
	if !o.Local && os.Getenv(provider.EnvDir) == "" {
		// Providers live with the server's data (gitopsify ui uses the
		// user's, like the CLI).
		_ = os.Setenv(provider.EnvDir, cfg.Providers.Dir)
	}
	s.limits = newLimiter()
	for _, p := range cfg.TrustedProxies {
		if !strings.Contains(p, "/") {
			if strings.Contains(p, ":") {
				p += "/128"
			} else {
				p += "/32"
			}
		}
		if _, n, err := net.ParseCIDR(p); err == nil {
			s.proxies = append(s.proxies, n)
		}
	}
	if cfg.Metrics.Token != (config.SettingValue{}) {
		if s.metricsKey, err = cfg.Metrics.Token.Resolve(); err != nil {
			s.close(context.Background())
			return nil, fmt.Errorf("spec.metrics.token: %w", err)
		}
		if s.metricsKey == "" {
			// An empty token would leave /metrics open.
			s.close(context.Background())
			return nil, errors.New("spec.metrics.token is empty (an empty variable or file?); /metrics would be open")
		}
	}
	// API requests get spans and http.server.* metrics when OpenTelemetry
	// is on (the UI's files and the probes don't); trace headers count from
	// trusted proxies only.
	s.handler = telemetry.Handler(s.routes(), "/api/", s.fromProxy)
	go s.housekeeping()
	return s, nil
}

// Handler returns the server's HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// ListenAndServe serves until ctx ends, then shuts down gracefully: running
// work is canceled and recorded as such.
func (s *Server) ListenAndServe(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second,
		ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelWarn)}
	// Live log streams never end on their own: end them, and stop the runs
	// (recorded as canceled), before waiting for requests to finish.
	srv.RegisterOnShutdown(func() { s.stopOnce.Do(func() { close(s.stop) }) })
	errc := make(chan error, 1)
	go func() {
		if t := s.cfg.TLS; t != nil {
			srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			errc <- srv.ServeTLS(ln, t.CertFile, t.KeyFile)
			return
		}
		errc <- srv.Serve(ln)
	}()
	select {
	case err := <-errc:
		s.close(context.Background())
		return err
	case <-ctx.Done():
	}
	s.stopOnce.Do(func() { close(s.stop) })
	stopRuns, cancelRuns := context.WithTimeout(context.Background(), 15*time.Second)
	s.runs.shutdown(stopRuns)
	cancelRuns()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := srv.Shutdown(shutdown)
	if errors.Is(err, context.DeadlineExceeded) {
		// Long requests (pushes, downloads) still going: a normal stop.
		s.log.Warn("stopping with requests still running")
		err = nil
	}
	// A push that reached the remote must still be recorded: wait for
	// pushes before closing the store (Kubernetes gives 30s by default).
	s.pushMu.Lock()
	s.pushesShut = true
	s.pushMu.Unlock()
	pushes := make(chan struct{})
	go func() { s.pushes.Wait(); close(pushes) }()
	select {
	case <-pushes:
	case <-time.After(20 * time.Second):
		s.log.Warn("stopping during a push; if it reached the remote, push the run again to record it")
	}
	closing, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelClose()
	s.close(closing)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

func (s *Server) close(ctx context.Context) {
	s.stopOnce.Do(func() { close(s.stop) })
	s.closeOnce.Do(func() {
		s.runs.shutdown(ctx)
		_ = s.store.close()
	})
}

// openStore opens the configured storage.
func openStore(ctx context.Context, cfg *config.ServerSpec) (store, error) {
	if cfg.Storage.Type == "postgres" {
		url, err := cfg.Storage.Postgres.URL.Resolve()
		if err != nil {
			return nil, fmt.Errorf("spec.storage.postgres.url: %w", err)
		}
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			return nil, err
		}
		return newPGStore(ctx, url, cfg.Storage.Postgres.MaxConnections)
	}
	st, err := newFileStore(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("data directory %s: %w", cfg.DataDir, err)
	}
	return st, nil
}

func (s *Server) housekeeping() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	s.runs.prune()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.runs.prune()
			s.audit.prune()
			s.limits.cleanup()
		}
	}
}

// --- request context and middleware ----------------------------------------

type ctxKey int

const reqKey ctxKey = 0

// reqInfo is what the middleware learns about a request.
type reqInfo struct {
	id      string
	ip      string
	route   string
	user    *User
	session *session
	grants  []grant
}

func info(r *http.Request) *reqInfo {
	if v, ok := r.Context().Value(reqKey).(*reqInfo); ok {
		return v
	}
	return &reqInfo{}
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// middleware wraps every request: an ID, the client address, security
// headers, rate limits, panics, logs and metrics.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// A request ID from a trusted proxy ties its logs to ours; clients
		// don't pick what the audit log records.
		var given string
		if s.fromProxy(r) {
			given = sanitizeID(r.Header.Get("X-Request-Id"))
		}
		ri := &reqInfo{id: firstNonEmpty(given, randomString(9)), ip: s.clientIP(r)}
		r = r.WithContext(context.WithValue(r.Context(), reqKey, ri))
		sw := &statusWriter{ResponseWriter: w}
		h := w.Header()
		h.Set("X-Request-Id", ri.id)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if s.auth.secure {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		defer func() {
			p := recover()
			if p == http.ErrAbortHandler {
				// A handler broke the response off on purpose: the server
				// drops the connection.
				defer panic(p)
			} else if p != nil {
				s.log.Error("panic", "request", ri.id, "panic", p, "stack", string(debug.Stack()))
				if sw.code == 0 {
					writeError(sw, http.StatusInternalServerError, "internal error (request "+ri.id+")")
				}
			}
			route := firstNonEmpty(ri.route, "ui")
			s.metrics.request(r.Method, route, sw.code, time.Since(start))
			// The client address the audit log uses, not otelhttp's (the
			// first X-Forwarded-For entry, which the client picks).
			telemetry.Route(r.Context(), r.Method, ri.route, attribute.String("gitopsify.request.id", ri.id),
				attribute.String("client.address", ri.ip))
			if strings.HasPrefix(r.URL.Path, "/api/") {
				user := ""
				if ri.user != nil {
					user = ri.user.Username
				}
				s.log.InfoContext(r.Context(), "request", "id", ri.id, "method", r.Method, "path", r.URL.Path, "status", sw.code,
					"duration", time.Since(start).Round(time.Millisecond).String(), "user", user, "ip", ri.ip)
			}
		}()
		if strings.HasPrefix(r.URL.Path, "/api/") && !s.limits.allow(ri.ip, strings.HasPrefix(r.URL.Path, "/api/v1/auth/")) {
			h.Set("Retry-After", "1")
			writeError(sw, http.StatusTooManyRequests, "too many requests")
			return
		}
		next.ServeHTTP(sw, r)
	})
}

func sanitizeID(s string) string {
	if len(s) > 64 {
		return ""
	}
	for _, c := range s {
		if !(c == '-' || c == '_' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return ""
		}
	}
	return s
}

// clientIP is the caller's address. From a trusted proxy, it is the last
// X-Forwarded-For entry that isn't one (proxies append; the entries before
// a trusted one are whatever the client sent).
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !s.trusted(host) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		h := strings.TrimSpace(hops[i])
		if ap, err := netip.ParseAddrPort(h); err == nil {
			h = ap.Addr().String() // proxies that add the port: 192.0.2.1:4711, [2001:db8::1]:4711
		}
		if net.ParseIP(h) == nil {
			break
		}
		if !s.trusted(h) {
			return h
		}
		host = h
	}
	return host
}

// fromProxy reports a request from a trusted proxy.
func (s *Server) fromProxy(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return s.trusted(host)
}

func (s *Server) trusted(ip string) bool {
	addr := net.ParseIP(ip)
	if addr == nil {
		return false
	}
	for _, n := range s.proxies {
		if n.Contains(addr) {
			return true
		}
	}
	return false
}

// handlerFunc is an API handler with its request's info.
type handlerFunc func(w http.ResponseWriter, r *http.Request, ri *reqInfo)

// api registers a JSON API route. minRole is what the caller needs at
// least on some cluster (empty: anyone signed in, even without a role;
// public: no sign-in at all). Handlers check cluster roles themselves.
func (s *Server) api(mux *http.ServeMux, pattern, minRole string, h handlerFunc) {
	route := pattern
	if _, p, ok := strings.Cut(pattern, " "); ok {
		route = p
	}
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		ri := info(r)
		ri.route = route
		if minRole != "public" {
			u, sess, toks := s.auth.fromRequest(r)
			if u == nil {
				writeError(w, http.StatusUnauthorized, "sign in first")
				return
			}
			ri.user, ri.session = u, sess
			ri.grants = s.policy.grants(u, toks)
			if !checkCSRF(r, sess) {
				s.deny(r, "csrf", "", "missing or wrong "+csrfHeader+" header")
				writeError(w, http.StatusForbidden, "missing or wrong "+csrfHeader+" header")
				return
			}
			if minRole != "" && !atLeast(anyRole(ri.grants), minRole) {
				s.deny(r, ri.route, "", "needs "+minRole)
				writeError(w, http.StatusForbidden, "you need the "+minRole+" role")
				return
			}
		}
		h(w, r, ri)
	})
}

// deny records a refused request.
func (s *Server) deny(r *http.Request, action, cluster, detail string) {
	ri := info(r)
	e := AuditEvent{Action: "access." + strings.Trim(action, "/"), Outcome: "denied", Cluster: cluster, Detail: detail, IP: ri.ip, Request: ri.id}
	if ri.user != nil {
		e.Actor, e.Via = ri.user.Username, ri.user.Via
	}
	s.audit.record(e)
}

// event records an audit event for the request's user.
func (s *Server) event(r *http.Request, action, outcome, cluster, target, detail string) {
	ri := info(r)
	e := AuditEvent{Action: action, Outcome: outcome, Cluster: cluster, Target: target, Detail: detail, IP: ri.ip, Request: ri.id}
	if ri.user != nil {
		e.Actor, e.Via = ri.user.Username, ri.user.Via
	}
	s.audit.record(e)
}

// can reports whether the request's user has role on cluster, and records
// a denial otherwise.
func (s *Server) can(w http.ResponseWriter, r *http.Request, ri *reqInfo, role, cluster, action string) bool {
	if atLeast(roleOn(ri.grants, cluster), role) {
		return true
	}
	s.deny(r, action, cluster, "needs "+role)
	writeError(w, http.StatusForbidden, fmt.Sprintf("you need the %s role on cluster %s", role, cluster))
	return false
}

func writeJSONResponse(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSONResponse(w, code, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return errors.New("send application/json")
	}
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("reading the request: %w", err)
	}
	return nil
}

// --- rate limits -------------------------------------------------------------

// limiter limits requests per client address: generous for the API, tight
// for sign-ins.
type limiter struct {
	mu   sync.Mutex
	api  map[string]*entry
	auth map[string]*entry
}

type entry struct {
	l    *rate.Limiter
	seen time.Time
}

func newLimiter() *limiter { return &limiter{api: map[string]*entry{}, auth: map[string]*entry{}} }

// maxLimited bounds the clients tracked per limit: beyond it, idle ones
// go, then new ones share one bucket (memory stays bounded).
const maxLimited = 50000

func (l *limiter) allow(ip string, auth bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, r, b := l.api, rate.Limit(30), 120
	if auth {
		m, r, b = l.auth, rate.Every(2*time.Second), 20
	}
	key := limitKey(ip)
	e := m[key]
	if e == nil {
		if len(m) >= maxLimited {
			for k, old := range m {
				if time.Since(old.seen) > time.Minute {
					delete(m, k)
				}
			}
			if len(m) >= maxLimited {
				key = "overflow"
				e = m[key]
			}
		}
		if e == nil {
			e = &entry{l: rate.NewLimiter(r, b)}
			m[key] = e
		}
	}
	e.seen = time.Now()
	return e.l.Allow()
}

// limitKey is the client a limit applies to: its IPv6 /64 (one host often
// has the whole prefix), or its address.
func limitKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil || a.Is4() || a.Is4In6() {
		return ip
	}
	p, err := a.Prefix(64)
	if err != nil {
		return ip
	}
	return p.String()
}

func (l *limiter) cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range []map[string]*entry{l.api, l.auth} {
		for k, e := range m {
			if time.Since(e.seen) > 30*time.Minute {
				delete(m, k)
			}
		}
	}
}

// scoped reports whether some binding or token is limited to clusters or
// tools.
func scoped(cfg *config.ServerSpec) bool {
	for _, b := range cfg.RBAC.Bindings {
		if len(b.Clusters)+len(b.Tools) > 0 {
			return true
		}
	}
	for _, t := range cfg.Auth.Tokens {
		if len(t.Clusters)+len(t.Tools) > 0 {
			return true
		}
	}
	return false
}

// startPush counts a push, unless the server is stopping.
func (s *Server) startPush() bool {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	if s.pushesShut {
		return false
	}
	s.pushes.Add(1)
	return true
}
