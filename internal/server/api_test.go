package server

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/hayone1/gitopsify/pkg/config"
)

var (
	ada   = map[string]any{"sub": "1", "email": "ada@acme.io", "email_verified": true, "groups": []string{"shop"}}
	bob   = map[string]any{"sub": "2", "email": "bob@acme.io", "email_verified": true}
	admin = map[string]any{"sub": "3", "email": "root@acme.io", "email_verified": true, "groups": []string{"platform"}}
)

const migration = "apiVersion: gitopsify.io/v1alpha1\nkind: Migration\nmetadata: {name: shop}\nspec:\n  output:\n    tool: none\n"

func TestRoles(t *testing.T) {
	e := newTestEnv(t, nil)
	adaC, bobC, adminC := e.login(ada), e.login(bob), e.login(admin)

	// bob is a viewer everywhere: he sees clusters but can't start runs.
	var clusters []clusterInfo
	bobC.get("/api/v1/clusters", &clusters)
	if len(clusters) != 2 || clusters[0].Role != "viewer" {
		t.Errorf("bob's clusters: %+v", clusters)
	}
	if code := bobC.do(http.MethodPost, "/api/v1/runs", runRequest{Type: RunScan, Cluster: "dev", Migration: migration}, nil); code != http.StatusForbidden {
		t.Errorf("viewer started a run: %d", code)
	}
	// ada operates dev, not prod.
	var run Run
	if code := adaC.do(http.MethodPost, "/api/v1/runs", runRequest{Type: RunScan, Cluster: "dev", Migration: migration}, &run); code != http.StatusCreated {
		t.Fatalf("operator on dev: %d", code)
	}
	if code := adaC.do(http.MethodPost, "/api/v1/runs", runRequest{Type: RunScan, Cluster: "prod", Migration: migration}, nil); code != http.StatusForbidden {
		t.Errorf("operator of dev ran on prod: %d", code)
	}
	// Only admins without cluster limits read the audit log.
	if code := adaC.get("/api/v1/audit", nil); code != http.StatusForbidden {
		t.Errorf("ada read the audit log: %d", code)
	}
	var evs []AuditEvent
	if code := adminC.get("/api/v1/audit?action=access.", &evs); code != 200 || len(evs) < 2 {
		t.Errorf("admin audit: %d %+v", code, evs)
	}
	// Anonymous requests get nothing but /info.
	anon := e.anonymous()
	if code := anon.get("/api/v1/runs", nil); code != http.StatusUnauthorized {
		t.Errorf("anonymous runs: %d", code)
	}
	if code := anon.get("/api/v1/info", nil); code != 200 {
		t.Errorf("info: %d", code)
	}
	// A cookie request that changes something needs the CSRF header.
	if code := adaC.do(http.MethodPost, "/api/v1/runs", runRequest{Type: RunScan, Cluster: "dev", Migration: migration}, nil, csrfHeader, "wrong"); code != http.StatusForbidden {
		t.Errorf("wrong CSRF: %d", code)
	}
	waitRun(t, e.s, run.ID)
}

func TestTokens(t *testing.T) {
	e := newTestEnv(t, nil)
	c := e.anonymous()
	auth := []string{"Authorization", "Bearer " + testToken}
	var run Run
	if code := c.do(http.MethodPost, "/api/v1/runs", runRequest{Type: RunScan, Cluster: "dev", Migration: migration}, &run, auth...); code != http.StatusCreated {
		t.Fatalf("token on dev: %d", code)
	}
	if run.CreatedBy != "token:ci" {
		t.Errorf("created by %q", run.CreatedBy)
	}
	if code := c.do(http.MethodPost, "/api/v1/runs", runRequest{Type: RunScan, Cluster: "prod", Migration: migration}, nil, auth...); code != http.StatusForbidden {
		t.Errorf("token limited to dev ran on prod: %d", code)
	}
	if code := c.do(http.MethodGet, "/api/v1/runs", nil, nil, "Authorization", "Bearer wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", code)
	}
	waitRun(t, e.s, run.ID)
}

// waitRun waits for a run to finish and returns it.
func waitRun(t *testing.T, s *Server, id string) *Run {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if r, ok := s.runs.get(id); ok && r.finished() {
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s didn't finish", id)
	return nil
}

func TestRunLifecycle(t *testing.T) {
	e := newTestEnv(t, nil)
	adaC := e.login(ada)
	// A scan of an unreachable cluster fails, with the reason.
	var run Run
	adaC.do(http.MethodPost, "/api/v1/runs", runRequest{Type: RunScan, Cluster: "dev", Migration: migration}, &run)
	done := waitRun(t, e.s, run.ID)
	if done.Status != StatusFailed || done.Error == "" || done.CreatedBy != "ada@acme.io" {
		t.Errorf("run: %+v", done)
	}
	var lines []LogLine
	adaC.get("/api/v1/runs/"+run.ID+"/log", &lines)
	if len(lines) == 0 || lines[len(lines)-1].Level != "error" {
		t.Errorf("log: %+v", lines)
	}
	// The spec keeps no access settings.
	var got Run
	adaC.get("/api/v1/runs/"+run.ID, &got)
	if strings.Contains(string(got.Spec), "127.0.0.1") || strings.Contains(string(got.Spec), "impersonate") {
		t.Errorf("the run's spec leaks access settings: %s", got.Spec)
	}
	// Bob (viewer) can see it but not delete it; ada, who started it, can.
	bobC := e.login(bob)
	if code := bobC.do(http.MethodDelete, "/api/v1/runs/"+run.ID, nil, nil); code != http.StatusForbidden {
		t.Errorf("bob deleted ada's run: %d", code)
	}
	if code := adaC.do(http.MethodDelete, "/api/v1/runs/"+run.ID, nil, nil); code != 200 {
		t.Errorf("ada deleting her run: %d", code)
	}
	if code := adaC.get("/api/v1/runs/"+run.ID, nil); code != http.StatusNotFound {
		t.Errorf("deleted run: %d", code)
	}
}

// fakeMigration records a finished migration with files, as the pipeline
// would.
func fakeMigration(t *testing.T, s *Server, cluster string, block chan struct{}) *Run {
	t.Helper()
	spec, _ := json.Marshal(map[string]any{"apiVersion": "gitopsify.io/v1alpha1", "kind": "Migration", "metadata": map[string]string{"name": "shop"},
		"spec": map[string]any{"output": map[string]any{"tool": "none"}}})
	r, err := s.runs.start(Run{Type: RunMigrate, Name: "shop", Cluster: cluster, CreatedBy: "ada@acme.io", Spec: spec},
		func(ctx context.Context, rc *runContext) error {
			rc.Info("writing files")
			dir := s.runs.outputDir(rc.ID())
			for p, c := range map[string]string{"apps/web/deployment.yaml": "kind: Deployment\n", "MIGRATION_REPORT.md": "# Report\n",
				".secrets/web.yaml": "password: hunter2\n", "envs/prod/x.secrets.auto.tfvars": "token = 1\n"} {
				_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
				_ = os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644)
			}
			if block != nil {
				select {
				case <-block:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			rc.update(func(r *Run) { r.Summary = &RunSummary{Apps: 1, Files: 2} })
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunFiles(t *testing.T) {
	e := newTestEnv(t, nil)
	r := fakeMigration(t, e.s, "dev", nil)
	waitRun(t, e.s, r.ID)
	c := e.login(bob)
	var files []fileEntry
	c.get("/api/v1/runs/"+r.ID+"/files", &files)
	if len(files) != 2 {
		t.Fatalf("files (secrets must be hidden): %+v", files)
	}
	for _, p := range []string{".secrets/web.yaml", "envs/prod/x.secrets.auto.tfvars", "../../run.json", "%2e%2e/run.json"} {
		if code := c.get("/api/v1/runs/"+r.ID+"/files/"+p, nil); code != http.StatusNotFound {
			t.Errorf("%s: %d", p, code)
		}
	}
	resp, err := c.http.Get(e.srv.URL + "/api/v1/runs/" + r.ID + "/files/apps/web/deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "kind: Deployment\n" {
		t.Errorf("file: %q", b)
	}
	// The archive has the files, without secrets.
	resp, err = c.http.Get(e.srv.URL + "/api/v1/runs/" + r.ID + "/archive")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	all := strings.Join(names, " ")
	if !strings.Contains(all, "deployment.yaml") || strings.Contains(all, "secrets") {
		t.Errorf("archive: %v", names)
	}
}

func TestRunEvents(t *testing.T) {
	e := newTestEnv(t, nil)
	block := make(chan struct{})
	r := fakeMigration(t, e.s, "dev", block)
	c := e.login(ada)
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/runs/"+r.ID+"/events", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	var events []string
	released := false
	for sc.Scan() {
		line := sc.Text()
		if ev, ok := strings.CutPrefix(line, "event: "); ok {
			events = append(events, ev)
			if !released && ev == "run" {
				close(block)
				released = true
			}
			if ev == "end" {
				break
			}
		}
	}
	got := strings.Join(events, ",")
	if !strings.HasPrefix(got, "log,run") || !strings.HasSuffix(got, "run,end") {
		t.Errorf("events: %s", got)
	}
	if final, _ := e.s.runs.get(r.ID); final.Status != StatusSucceeded || final.Summary == nil {
		t.Errorf("final: %+v", final)
	}
}

func TestCancelAndRestart(t *testing.T) {
	e := newTestEnv(t, nil)
	block := make(chan struct{})
	r := fakeMigration(t, e.s, "dev", block)
	c := e.login(ada)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := e.s.runs.get(r.ID); got.Status == StatusRunning {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code := c.do(http.MethodPost, "/api/v1/runs/"+r.ID+"/cancel", nil, nil); code != 200 {
		t.Fatalf("cancel: %d", code)
	}
	if got := waitRun(t, e.s, r.ID); got.Status != StatusCanceled {
		t.Errorf("status %s", got.Status)
	}
	// A run the server was working on when it stopped is failed on restart.
	r2 := fakeMigration(t, e.s, "dev", make(chan struct{}))
	dataDir := e.s.cfg.DataDir
	st, err := newFileStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := newRuns(st, filepath.Join(dataDir, "runs"), 1, time.Minute, time.Hour, e.s.log, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer again.shutdown(context.Background())
	if got, _ := again.get(r2.ID); got == nil || got.Status != StatusFailed || !strings.Contains(got.Error, "stopped") {
		t.Errorf("after restart: %+v", got)
	}
	e.s.runs.cancel(r2.ID)
	waitRun(t, e.s, r2.ID)

	// Stopping the server fails its runs as interrupted, not canceled.
	r3 := fakeMigration(t, e.s, "dev", make(chan struct{}))
	for time.Now().Before(deadline.Add(10 * time.Second)) {
		if got, _ := e.s.runs.get(r3.ID); got.Status == StatusRunning {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e.s.runs.shutdown(ctx)
	if got, _ := e.s.runs.get(r3.ID); got == nil || got.Status != StatusFailed || !strings.Contains(got.Error, "interrupted") {
		t.Errorf("after stopping: %+v", got)
	}
}

func TestHeadersAndUI(t *testing.T) {
	e := newTestEnv(t, nil)
	e.s.ui = newUIHandler(fstest.MapFS{
		"index.html":      {Data: []byte(`<html><script type="importmap">{"imports":{}}</script><script>window.x=1</script><script type="module" src="/_nuxt/a.js"></script><script type="application/json" id="d">{}</script></html>`)},
		"_nuxt/a.js":      {Data: []byte("console.log(1)")},
		"favicon.svg":     {Data: []byte("<svg/>")},
		"runs/index.html": {Data: []byte("prerendered")},
	})
	e.s.handler = e.s.routes()
	e.srv.Config.Handler = e.s.handler
	c := e.anonymous()
	for path, want := range map[string]string{"/": "window.x=1", "/runs/123": "window.x=1", "/runs": "window.x=1", "/_nuxt/a.js": "console.log"} {
		resp, err := c.http.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), want) {
			t.Errorf("%s: %q", path, b)
		}
		h := resp.Header
		if h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Request-Id") == "" {
			t.Errorf("%s: security headers %v", path, h)
		}
		if path == "/" {
			csp := h.Get("Content-Security-Policy")
			if !strings.Contains(csp, "frame-ancestors 'none'") || strings.Count(csp, "'sha256-") != 2 || strings.Contains(csp, "unsafe-inline' ;") {
				t.Errorf("csp: %s", csp)
			}
		}
		if path == "/_nuxt/a.js" && !strings.Contains(h.Get("Cache-Control"), "immutable") {
			t.Errorf("asset caching: %s", h.Get("Cache-Control"))
		}
	}
	if code := c.get("/_nuxt/missing.js", nil); code != http.StatusNotFound {
		t.Errorf("missing asset: %d", code)
	}
	if code := c.get("/api/v1/nope", nil); code != http.StatusNotFound {
		t.Errorf("unknown API: %d", code)
	}
	resp, _ := c.http.Get(e.srv.URL + "/metrics")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "gitopsify_http_requests_total") {
		t.Errorf("metrics: %s", b)
	}
}

func TestRateLimit(t *testing.T) {
	l := newLimiter()
	n := 0
	for range 100 {
		if l.allow("1.2.3.4", true) {
			n++
		}
	}
	if n > 25 {
		t.Errorf("auth limit let %d through", n)
	}
	if !l.allow("5.6.7.8", true) {
		t.Error("another address was limited")
	}
}

func TestNotifications(t *testing.T) {
	got := make(chan map[string]any, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- body
	}))
	defer hook.Close()
	// From a Kubernetes Secret made from a file: a trailing newline.
	t.Setenv("HOOK_URL", hook.URL+"\n")
	e := newTestEnv(t, func(c *config.Server) {
		c.Spec.Notifications = []config.ServerNotification{
			{Name: "slack", URL: config.SettingValue{Env: "HOOK_URL"}, Format: "slack"},
			{Name: "audit", URL: config.SettingValue{Env: "HOOK_URL"}, Events: []string{"run.canceled"}},
		}
	})
	r := fakeMigration(t, e.s, "dev", nil)
	waitRun(t, e.s, r.ID)
	select {
	case body := <-got:
		text, _ := body["text"].(string)
		if !strings.Contains(text, "migration shop succeeded on dev") || !strings.Contains(text, "/runs/"+r.ID) {
			t.Errorf("slack message: %q", text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no notification")
	}
	select {
	case body := <-got:
		t.Errorf("a hook for other events posted: %v", body)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestNotificationURLs(t *testing.T) {
	for _, bad := range []string{"not a url", "ftp://hooks.example/x", "https://"} {
		cfg := &config.Server{Spec: config.ServerSpec{ExternalURL: "http://127.0.0.1:1", DataDir: t.TempDir(),
			Auth:          config.ServerAuth{Mode: config.AuthLocal},
			Notifications: []config.ServerNotification{{Name: "hook", URL: config.SettingValue{Value: bad}}}}}
		cfg.Default()
		_, err := New(context.Background(), Options{Config: cfg, Local: true, Version: "test", Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err == nil || strings.Contains(err.Error(), bad) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
