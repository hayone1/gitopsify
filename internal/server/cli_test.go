package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/gitrepo"
)

// cliSignIn runs the browser's part of a command-line sign-in, confirming
// it on the /cli-login page, and returns the code the browser hands over: to
// a loopback listener, or (no port) shown to paste.
func cliSignIn(t *testing.T, e *testEnv, claims map[string]any, challenge, state string, loopback bool) (code, gotState string, browser *client) {
	t.Helper()
	browser = e.anonymous()
	e.idp.as(claims)
	q := url.Values{"cli_challenge": {challenge}, "cli_state": {state}}
	got := make(chan url.Values, 1)
	if loopback {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got <- r.URL.Query()
			_, _ = w.Write([]byte("ok"))
		})}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })
		q.Set("cli_port", strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
	}
	resp, err := browser.http.Get(e.srv.URL + "/api/v1/auth/login?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/cli-login" {
		t.Fatalf("the sign-in ended at %s, not the confirmation page", resp.Request.URL)
	}
	// Nothing reached the command line before the confirmation.
	select {
	case <-got:
		t.Fatal("a code was handed over without a confirmation")
	default:
	}
	var pending struct {
		User      map[string]string `json:"user"`
		CheckCode string            `json:"checkCode"`
		Paste     bool              `json:"paste"`
	}
	if status := browser.get("/api/v1/auth/cli", &pending); status != http.StatusOK || pending.CheckCode != CLICheckCode(challenge) || pending.Paste == loopback {
		t.Fatalf("pending: %d %+v", status, pending)
	}
	var answer map[string]string
	if status := browser.do(http.MethodPost, "/api/v1/auth/cli", map[string]bool{"confirm": true}, &answer); status != http.StatusOK {
		t.Fatalf("confirm: %d", status)
	}
	if !loopback {
		v, _ := url.ParseQuery(answer["code"])
		return v.Get("code"), v.Get("state"), browser
	}
	if !strings.HasPrefix(answer["redirect"], "http://127.0.0.1:") {
		t.Fatalf("redirect: %q", answer["redirect"])
	}
	r2, err := browser.http.Get(answer["redirect"])
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	select {
	case v := <-got:
		return v.Get("code"), v.Get("state"), browser
	case <-time.After(5 * time.Second):
		t.Fatal("the browser never reached the command line")
	}
	return "", "", browser
}

func exchange(t *testing.T, e *testEnv, code, verifier string) (int, map[string]any) {
	t.Helper()
	var out map[string]any
	status := e.anonymous().do(http.MethodPost, "/api/v1/auth/token", map[string]string{"code": code, "verifier": verifier}, &out)
	return status, out
}

func TestCLISignIn(t *testing.T) {
	e := newTestEnv(t, nil)
	verifier := oauth2.GenerateVerifier()
	challenge := oauth2.S256ChallengeFromVerifier(verifier)
	code, state, browser := cliSignIn(t, e, ada, challenge, "st4te", true)
	if state != "st4te" || code == "" {
		t.Fatalf("callback: code %q state %q", code, state)
	}
	// The browser isn't signed in: the session is the command line's.
	if status := browser.get("/api/v1/me", nil); status != http.StatusUnauthorized {
		t.Errorf("the browser got a session: %d", status)
	}
	// The code is worth nothing without the verifier.
	if status, _ := exchange(t, e, code, oauth2.GenerateVerifier()); status != http.StatusUnauthorized {
		t.Errorf("wrong verifier: %d", status)
	}
	status, out := exchange(t, e, code, verifier)
	token, _ := out["token"].(string)
	if status != http.StatusOK || !strings.HasPrefix(token, cliTokenPrefix) {
		t.Fatalf("exchange: %d %v", status, out)
	}

	cli := e.anonymous()
	auth := func(tok string) []string { return []string{"Authorization", "Bearer " + tok} }
	var m me
	if status := cli.do(http.MethodGet, "/api/v1/me", nil, &m, auth(token)...); status != http.StatusOK {
		t.Fatalf("me with the token: %d", status)
	}
	if m.User.Username != "ada@acme.io" || m.User.Via != "cli" || m.Clusters["dev"] != "operator" || m.CSRF != "" || m.Expires == nil {
		t.Errorf("me: %+v", m)
	}
	// A bearer token needs no CSRF header to change things.
	var run Run
	if status := cli.do(http.MethodPost, "/api/v1/runs", runRequest{Type: RunScan, Cluster: "dev", Migration: migration}, &run, auth(token)...); status != http.StatusCreated {
		t.Errorf("starting a run with the token: %d", status)
	}
	waitRun(t, e.s, run.ID)
	if run.CreatedBy != "ada@acme.io" {
		t.Errorf("run by %q", run.CreatedBy)
	}
	// Tampered tokens, and a cookie session sent as a token, get nothing.
	cookie := ""
	for _, ck := range e.login(ada).http.Jar.Cookies(mustURL(e.srv.URL)) {
		if ck.Name == sessionCookie {
			cookie = ck.Value
		}
	}
	for name, tok := range map[string]string{"tampered": token[:len(token)-2] + "xx", "cookie": cliTokenPrefix + cookie, "code": cliTokenPrefix + code} {
		if status := cli.do(http.MethodGet, "/api/v1/me", nil, nil, auth(tok)...); status != http.StatusUnauthorized {
			t.Errorf("%s as a token: %d", name, status)
		}
	}
	// An expired code is refused.
	old, _ := e.s.auth.sealer.seal(cliCodePurpose, cliCode{User: User{Username: "ada@acme.io", Via: "cli"}, Challenge: challenge, Expires: time.Now().Add(-time.Second).Unix()})
	if status, _ := exchange(t, e, old, verifier); status != http.StatusUnauthorized {
		t.Errorf("expired code: %d", status)
	}
	evs, _ := e.s.audit.query(auditQuery{Action: "auth."})
	found := false
	for _, ev := range evs {
		found = found || (ev.Action == "auth.login" && ev.Via == "cli" && ev.Outcome == "ok" && ev.Actor == "ada@acme.io")
	}
	if !found {
		t.Errorf("no audited command-line sign-in: %+v", evs)
	}
}

func TestCLISignInPaste(t *testing.T) {
	e := newTestEnv(t, nil)
	verifier := oauth2.GenerateVerifier()
	code, state, _ := cliSignIn(t, e, ada, oauth2.S256ChallengeFromVerifier(verifier), "abc", false)
	if code == "" || state != "abc" {
		t.Fatalf("page: code %q state %q", code, state)
	}
	if status, out := exchange(t, e, code, verifier); status != http.StatusOK {
		t.Errorf("exchange: %d %v", status, out)
	}
}

func TestCLISignInRefusals(t *testing.T) {
	e := newTestEnv(t, nil)
	// A sign-in someone else started, canceled on the page: no code, and
	// nothing left to confirm.
	browser := e.anonymous()
	e.idp.as(ada)
	resp, err := browser.http.Get(e.srv.URL + "/api/v1/auth/login?" + url.Values{"cli_challenge": {oauth2.S256ChallengeFromVerifier(oauth2.GenerateVerifier())}, "cli_state": {"x"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var answer map[string]string
	if status := browser.do(http.MethodPost, "/api/v1/auth/cli", map[string]bool{"confirm": false}, &answer); status != http.StatusOK || answer["code"] != "" || answer["status"] != "canceled" {
		t.Errorf("cancel: %d %v", status, answer)
	}
	if status := browser.do(http.MethodPost, "/api/v1/auth/cli", map[string]bool{"confirm": true}, nil); status != http.StatusNotFound {
		t.Errorf("confirm after cancel: %d", status)
	}
	// Another browser (no cookie) can't confirm, nor can a form post.
	if status := e.anonymous().do(http.MethodPost, "/api/v1/auth/cli", map[string]bool{"confirm": true}, nil); status != http.StatusNotFound {
		t.Errorf("confirm without the cookie: %d", status)
	}
	form, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/auth/cli", strings.NewReader("confirm=true"))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if r, err := browser.http.Do(form); err != nil || r.StatusCode != http.StatusBadRequest {
		t.Errorf("form post: %v %v", r, err)
	}

	good := oauth2.S256ChallengeFromVerifier(oauth2.GenerateVerifier())
	for name, q := range map[string]string{
		"short challenge":   "cli_challenge=abc&cli_state=x",
		"no state":          "cli_challenge=" + good,
		"odd state":         "cli_challenge=" + good + "&cli_state=" + url.QueryEscape("a/b"),
		"privileged port":   "cli_challenge=" + good + "&cli_state=x&cli_port=80",
		"port with a host":  "cli_challenge=" + good + "&cli_state=x&cli_port=" + url.QueryEscape("8080@evil.example"),
		"port out of range": "cli_challenge=" + good + "&cli_state=x&cli_port=70000",
	} {
		resp, err := e.anonymous().http.Get(e.srv.URL + "/api/v1/auth/login?" + q)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d (ended at %s)", name, resp.StatusCode, resp.Request.URL)
		}
	}
	// Servers without OIDC have no command-line sign-in.
	local := newTestEnv(t, func(c *config.Server) { c.Spec.Auth = config.ServerAuth{Mode: config.AuthLocal} })
	if status, _ := exchange(t, local, "x", "y"); status != http.StatusNotFound {
		t.Errorf("token exchange without OIDC: %d", status)
	}
}

// TestPublishBranch: pushing a run's output to a pull request branch
// leaves the base branch alone, pushes nothing when the output equals the
// base, and reports a second push of the same files as up to date.
func TestPublishBranch(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "--bare", "-b", "main", "gitops.git"}, {"-C", "gitops.git", "config", "http.receivepack", "true"}} {
		cmd := exec.Command(gitPath, args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	gitSrv := httptest.NewServer(&cgi.Handler{Path: gitPath, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}})
	defer gitSrv.Close()
	remote := gitSrv.URL + "/gitops.git"
	e := newTestEnv(t, func(c *config.Server) { c.Spec.Git.AllowHTTP = true })
	spec, _ := json.Marshal(map[string]any{"apiVersion": "gitopsify.io/v1alpha1", "kind": "Migration", "metadata": map[string]string{"name": "shop"},
		"spec": map[string]any{"output": map[string]any{"tool": "none", "repo": map[string]any{"url": remote}}}})
	start := func(content string) *Run {
		r, err := e.s.runs.start(context.Background(), Run{Type: RunMigrate, Name: "shop", Cluster: "dev", CreatedBy: "ada@acme.io", Spec: spec},
			func(ctx context.Context, rc *runContext) error {
				dir := e.s.runs.outputDir(rc.ID())
				// As migrations do: the output goes on top of the branch.
				if _, err := gitrepo.Checkout(dir, remote, "main"); err != nil {
					return err
				}
				_ = os.MkdirAll(filepath.Join(dir, "apps/web"), 0o755)
				return os.WriteFile(filepath.Join(dir, "apps/web/deployment.yaml"), []byte(content), 0o644)
			})
		if err != nil {
			t.Fatal(err)
		}
		waitRun(t, e.s, r.ID)
		return r
	}
	c := e.login(ada)
	publish := func(r *Run, body map[string]string) Publish {
		t.Helper()
		var got Run
		if status := c.do(http.MethodPost, "/api/v1/runs/"+r.ID+"/publish", body, &got); status != http.StatusOK {
			t.Fatalf("publish %v: %d", body, status)
		}
		return got.Publishes[len(got.Publishes)-1]
	}
	first := start("kind: Deployment\n")
	if p := publish(first, nil); !p.Pushed || p.Base != "" || p.Branch != "main" {
		t.Errorf("first push: %+v", p)
	}
	// The same output again, for a pull request: main already has it.
	if p := publish(first, map[string]string{"branch": "gitopsify/drift"}); p.Pushed || !p.Unchanged || p.Base != "main" {
		t.Errorf("no change, pull request branch: %+v", p)
	}
	if status := c.do(http.MethodPost, "/api/v1/runs/"+first.ID+"/publish", map[string]string{"branch": "refs/heads/x"}, nil); status != http.StatusBadRequest {
		t.Errorf("a ref as the branch: %d", status)
	}
	git := func(args ...string) string {
		out, _ := exec.Command(gitPath, append([]string{"-C", filepath.Join(root, "gitops.git")}, args...)...).CombinedOutput()
		return string(out)
	}
	if out := git("branch", "--list"); strings.Contains(out, "drift") {
		t.Errorf("drift branch pushed without a change: %s", out)
	}
	// A later run that differs goes to the branch; main stays.
	mainBefore := git("rev-parse", "main")
	second := start("kind: Deployment\nspec: {replicas: 2}\n")
	if p := publish(second, map[string]string{"branch": "gitopsify/drift"}); !p.Pushed || p.Base != "main" || p.Branch != "gitopsify/drift" {
		t.Errorf("drift push: %+v", p)
	}
	if git("rev-parse", "main") != mainBefore {
		t.Error("main moved")
	}
	if got := git("show", "gitopsify/drift:apps/web/deployment.yaml"); !strings.Contains(got, "replicas: 2") {
		t.Errorf("drift branch: %q", got)
	}
	if p := publish(second, map[string]string{"branch": "gitopsify/drift"}); p.Pushed || !p.UpToDate {
		t.Errorf("the same drift again: %+v", p)
	}
}
