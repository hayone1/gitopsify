package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hayone1/gitopsify/internal/remote"
	"github.com/hayone1/gitopsify/internal/server"
	"github.com/hayone1/gitopsify/pkg/config"
)

const remoteToken = "gtk_cli_test_token"

// remoteServer starts a gitopsify server that admits remoteToken as an
// operator, with a cluster it can't reach, and points the command line's
// saved sign-ins at a temporary file.
func remoteServer(t *testing.T) string {
	t.Helper()
	t.Setenv(remote.EnvFile, filepath.Join(t.TempDir(), "servers.yaml"))
	t.Setenv(remote.EnvServer, "")
	t.Setenv(remote.EnvToken, "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Server{Spec: config.ServerSpec{ExternalURL: "http://" + ln.Addr().String(), DataDir: t.TempDir(),
		Auth:     config.ServerAuth{Mode: config.AuthLocal, Tokens: []config.ServerToken{{Name: "cli", SHA256: server.TokenHash(remoteToken), Role: "operator"}}},
		Clusters: []config.ServerCluster{{Name: "dev", Access: config.Access{APIServer: "https://127.0.0.1:1"}}}}}
	cfg.Default()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv, err := server.New(ctx, server.Options{Config: cfg, Version: "test", Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = srv.ListenAndServe(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return "http://" + ln.Addr().String()
}

// withStdin runs fn with standard input reading s.
func withStdin(t *testing.T, s string, fn func()) {
	t.Helper()
	r, w, _ := os.Pipe()
	_, _ = w.WriteString(s)
	w.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	fn()
}

func TestRemoteCommands(t *testing.T) {
	addr := remoteServer(t)
	dir := t.TempDir()
	t.Chdir(dir)

	if code, _, errOut := run(t, "run", "list"); code != 1 || !strings.Contains(errOut, "gitopsify login") {
		t.Errorf("run list before login: %d %s", code, errOut)
	}
	withStdin(t, "wrong\n", func() {
		if code, _, errOut := run(t, "login", addr, "--token-stdin"); code != 1 || !strings.Contains(errOut, "sign-in") {
			t.Errorf("login with a wrong token: %d %s", code, errOut)
		}
	})
	withStdin(t, remoteToken+"\n", func() {
		if code, _, errOut := run(t, "login", addr, "--token-stdin", "--name", "test"); code != 0 {
			t.Fatalf("login: %s", errOut)
		}
	})
	if fi, err := os.Stat(os.Getenv(remote.EnvFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("servers file: %v %v", fi, err)
	}
	code, out, errOut := run(t, "whoami", "--format", "json")
	var me remote.Me
	if code != 0 || json.Unmarshal([]byte(out), &me) != nil || me.User.Username != "token:cli" || me.Clusters["dev"] != "operator" {
		t.Errorf("whoami: %d %s %s", code, out, errOut)
	}
	if code, out, _ := run(t, "whoami", "--all", "--format", "json"); code != 0 || strings.Contains(out, remoteToken) || !strings.Contains(out, `"current": "test"`) {
		t.Errorf("whoami --all (never prints tokens): %d %s", code, out)
	}

	// A migration of a cluster the server can't reach fails: --wait says so.
	file := filepath.Join(dir, "gitopsify.yaml")
	_ = os.WriteFile(file, []byte("apiVersion: gitopsify.io/v1alpha1\nkind: Migration\nmetadata: {name: shop}\nspec:\n  source: {context: dev, kubeconfig: /home/me/.kube/config}\n  output: {tool: none, dir: ./gitops}\n"), 0o644)
	code, out, errOut = run(t, "run", "start", "--scan", "--wait", "--format", "json")
	var r server.Run
	if code != 1 || json.Unmarshal([]byte(out), &r) != nil || r.Status != server.StatusFailed || r.Cluster != "dev" {
		t.Fatalf("run start --wait: %d %s %s", code, out, errOut)
	}
	if strings.Contains(string(r.Spec), "kubeconfig") {
		t.Errorf("local access settings were sent: %s", r.Spec)
	}
	if code, out, _ := run(t, "run", "list"); code != 0 || !strings.Contains(out, r.ID) || !strings.Contains(out, "failed") {
		t.Errorf("run list: %d %s", code, out)
	}
	if code, out, _ := run(t, "run", "logs", r.ID); code != 0 || out == "" {
		t.Errorf("run logs: %d %q", code, out)
	}
	if code, out, _ := run(t, "run", "logs", "-f", r.ID); code != 0 || out == "" {
		t.Errorf("run logs -f of a finished run: %d %q", code, out)
	}
	if code, out, _ := run(t, "run", "get", r.ID); code != 0 || !strings.Contains(out, "failed") {
		t.Errorf("run get: %d %s", code, out)
	}
	if code, _, _ := run(t, "run", "wait", r.ID); code != 1 {
		t.Errorf("run wait on a failed run: %d", code)
	}
	if code, _, errOut := run(t, "run", "publish", r.ID); code != 1 || !strings.Contains(errOut, "succeeded") {
		t.Errorf("publishing a failed scan: %d %s", code, errOut)
	}
	if code, _, errOut := run(t, "run", "get", "nope"); code != 1 || !strings.Contains(errOut, "no such run") {
		t.Errorf("unknown run: %d %s", code, errOut)
	}
	if code, _, _ := run(t, "run", "delete", r.ID); code != 0 {
		t.Errorf("run delete: %d", code)
	}

	// Mistakes stop before anything happens on the server.
	if code, _, errOut := run(t, "run", "start", "--scan", "--format", "bogus"); code != 1 || !strings.Contains(errOut, "--format") {
		t.Errorf("bad --format: %d %s", code, errOut)
	}
	if code, _, errOut := run(t, "run", "list", "--context", "prod"); code != 1 || !strings.Contains(errOut, "--context") {
		t.Errorf("a source-cluster flag on a server command: %d %s", code, errOut)
	}
	if code, _, errOut := run(t, "run", "list", "--format", "text"); code != 0 {
		t.Errorf("--format text: %d %s", code, errOut)
	}

	// CI: no saved sign-in, the environment names the server.
	if code, _, _ := run(t, "logout"); code != 0 {
		t.Error("logout")
	}
	t.Setenv(remote.EnvServer, addr)
	t.Setenv(remote.EnvToken, remoteToken)
	if code, _, errOut := run(t, "run", "list", "-q"); code != 0 {
		t.Errorf("run list from the environment: %s", errOut)
	}
}

func TestRunRequestFrom(t *testing.T) {
	dir := t.TempDir()
	m := filepath.Join(dir, "m.yaml")
	_ = os.WriteFile(m, []byte(`apiVersion: gitopsify.io/v1alpha1
kind: Migration
metadata: {name: shop}
spec:
  source: {context: test, kubeconfig: ~/.kube/config, tokenFile: /tmp/t}
  environments:
    - name: uat
      cluster: {context: uat, kubeconfig: /x, server: https://kubernetes.default.svc}
  output: {tool: argocd, dir: ./gitops, repo: {url: https://github.com/acme/gitops.git}}
`), 0o644)
	req, err := runRequestFrom(m, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if req.Type != server.RunMigrate || req.Cluster != "test" {
		t.Errorf("request: %+v", req)
	}
	for _, leak := range []string{"kubeconfig", "tokenFile", "/x", "./gitops"} {
		if strings.Contains(req.Migration, leak) {
			t.Errorf("sent %s: %s", leak, req.Migration)
		}
	}
	for _, keep := range []string{`"context":"uat"`, "kubernetes.default.svc", "acme/gitops"} {
		if !strings.Contains(req.Migration, keep) {
			t.Errorf("lost %s: %s", keep, req.Migration)
		}
	}
	if req, _ := runRequestFrom(m, "prod", "", true); req.Cluster != "prod" || req.Type != server.RunScan {
		t.Errorf("--cluster --scan: %+v", req)
	}

	x := filepath.Join(dir, "x.yaml")
	_ = os.WriteFile(x, []byte(`apiVersion: gitopsify.io/v1alpha1
kind: Export
metadata: {name: grafana}
spec:
  provider: {name: grafana}
  instances:
    - name: grafana-prod
      settings: {url: {value: https://grafana.acme.io}, token: {env: GRAFANA_TOKEN}}
  output: {format: terraform}
`), 0o644)
	req, err = runRequestFrom(x, "", "", false)
	if err != nil || req.Type != server.RunExport || req.Tool != "grafana-prod" || strings.Contains(req.Export, "instances") {
		t.Errorf("export: %+v %v", req, err)
	}
	if _, err := runRequestFrom(x, "", "", true); err == nil {
		t.Error("--scan of an export")
	}
}

func TestExtractRun(t *testing.T) {
	archive := func(entries ...tar.Header) io.Reader {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for _, h := range entries {
			body := "x"
			h.Size = int64(len(body))
			if h.Typeflag == tar.TypeDir || h.Typeflag == tar.TypeSymlink {
				h.Size = 0
			}
			_ = tw.WriteHeader(&h)
			if h.Size > 0 {
				_, _ = tw.Write([]byte(body))
			}
		}
		_ = tw.Close()
		_ = gz.Close()
		return &buf
	}
	dir := filepath.Join(t.TempDir(), "out")
	n, top, err := extractRun(archive(
		tar.Header{Name: "shop-1/", Typeflag: tar.TypeDir, Mode: 0o755},
		tar.Header{Name: "shop-1/apps/web/deployment.yaml", Typeflag: tar.TypeReg, Mode: 0o644},
		tar.Header{Name: "shop-1/apply.sh", Typeflag: tar.TypeReg, Mode: 0o755},
		tar.Header{Name: "shop-1/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		tar.Header{Name: "elsewhere/x", Typeflag: tar.TypeReg, Mode: 0o644},
	), dir)
	if err != nil || n != 2 || top != "shop-1" {
		t.Fatalf("extract: %d %q %v", n, top, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "apply.sh")); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("apply.sh: %v %v", fi, err)
	}
	for _, p := range []string{"link", "x", "elsewhere"} {
		if _, err := os.Lstat(filepath.Join(dir, p)); err == nil {
			t.Errorf("%s was written", p)
		}
	}
	if _, _, err := extractRun(archive(tar.Header{Name: "shop-1/a", Typeflag: tar.TypeReg}), dir); err == nil {
		t.Error("extracted into a non-empty directory")
	}
	if _, _, err := extractRun(archive(tar.Header{Name: "shop-1/../../evil", Typeflag: tar.TypeReg}), filepath.Join(t.TempDir(), "o")); err == nil {
		// path.Clean turns it into ../evil, outside the top directory:
		// skipped, so nothing is written.
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil")); err == nil {
			t.Error("wrote outside the directory")
		}
	}
}

func TestGitAuthEnv(t *testing.T) {
	t.Setenv("GITOPSIFY_PUSH_TOKEN", "")
	if env := gitAuthEnv("https://github.com/acme/gitops.git"); env != nil {
		t.Errorf("without a token: %v", env)
	}
	t.Setenv("GITOPSIFY_PUSH_TOKEN", "tok")
	env := strings.Join(gitAuthEnv("https://github.com/acme/gitops.git"), " ")
	if !strings.Contains(env, "http.https://github.com/.extraHeader") || !strings.Contains(env, "Authorization: Basic eC1hY2Nlc3MtdG9rZW46dG9r") {
		t.Errorf("env: %s", env)
	}
	for _, remote := range []string{"git@github.com:acme/gitops.git", "http://git.local/x.git", "https://u:p@github.com/x.git"} {
		if env := gitAuthEnv(remote); env != nil {
			t.Errorf("%s: %v", remote, env)
		}
	}
}
