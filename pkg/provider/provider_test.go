package provider

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hayone1/gitopsify/providers"
	"github.com/hayone1/gitopsify/sdk"
)

// The test binary doubles as a provider when this variable is set.
const helperEnv = "GITOPSIFY_TEST_PROVIDER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		// Like sdk.Serve: the client's interrupt cancels the context (the
		// slow mode waits for it; on a quiet machine a context that never
		// ends would be a deadlock the runtime kills with exit status 2).
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		code := sdk.Run(ctx, helper{mode: mode}, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}
	os.Exit(m.Run())
}

type helper struct{ mode string }

// longLine is set by tests that need a huge stderr line first.
var longLine = func() {}

func (h helper) Describe() sdk.Description {
	longLine()
	d := sdk.Description{Name: "helper", Title: "Helper", Version: "v1.2.3", Partition: sdk.Noun{Singular: "org", Plural: "orgs"},
		Kinds:     []sdk.Kind{{Name: "Thing", Description: "things", Terraform: &sdk.TerraformType{Type: "helper_thing"}}},
		Terraform: &sdk.TerraformProvider{Name: "helper", Source: "acme/helper", Version: "1.0.0"}}
	if h.mode == "future" {
		d.Protocols = []string{"v2"}
	}
	return d
}

func (h helper) Check(ctx context.Context, req sdk.Request) (*sdk.CheckResult, error) {
	switch h.mode {
	case "unauthenticated":
		return nil, sdk.Errorf(sdk.CodeUnauthenticated, "token refused").WithHint("set HELPER_TOKEN")
	case "crash":
		fmt.Fprintln(os.Stderr, "fatal: something broke")
		os.Exit(3)
	}
	sdk.Logger(ctx).Info("connecting", "url", req.Settings["url"])
	return &sdk.CheckResult{Target: "Helper at " + req.Settings["url"], Partitions: []sdk.Partition{{ID: "1", Name: "Main"}}}, nil
}

func (h helper) Discover(ctx context.Context, req sdk.Request, out sdk.Emitter) error {
	if h.mode == "slow" {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := out.Partition(sdk.Partition{ID: "1", Name: "Main"}); err != nil {
		return err
	}
	for i := range 3 {
		if err := out.Resource(sdk.Resource{Key: fmt.Sprint(i), Kind: "Thing", Partition: "1", Name: fmt.Sprint("thing ", i), ImportID: fmt.Sprint(i),
			Attributes: map[string]any{"name": fmt.Sprint("thing ", i)}}); err != nil {
			return err
		}
	}
	return nil
}

func helperClient(t *testing.T, mode string) *Client {
	t.Helper()
	t.Setenv(helperEnv, mode)
	return &Client{Name: "helper", Path: os.Args[0]}
}

func TestClientRoundTrip(t *testing.T) {
	c := helperClient(t, "ok")
	var logs []string
	c.Log = func(l LogLine) { logs = append(logs, l.Level+" "+l.String()) }
	d, err := c.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != "v1.2.3" {
		t.Errorf("version %q", d.Version)
	}
	res, err := c.Check(context.Background(), sdk.Request{Settings: map[string]string{"url": "http://h"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Target != "Helper at http://h" {
		t.Errorf("target %q", res.Target)
	}
	if len(logs) != 1 || logs[0] != "INFO connecting url=http://h" {
		t.Errorf("logs %q", logs)
	}
	n := 0
	inv, err := c.Discover(context.Background(), sdk.Request{}, func(sdk.Resource) { n++ })
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Resources) != 3 || n != 3 {
		t.Errorf("%d resources, %d callbacks", len(inv.Resources), n)
	}
}

func TestClientErrors(t *testing.T) {
	_, err := helperClient(t, "unauthenticated").Check(context.Background(), sdk.Request{})
	var pe *Error
	if !errors.As(err, &pe) || pe.Code() != sdk.CodeUnauthenticated || !strings.Contains(err.Error(), "set HELPER_TOKEN") {
		t.Fatalf("err = %v", err)
	}

	_, err = helperClient(t, "crash").Check(context.Background(), sdk.Request{})
	if !errors.As(err, &pe) || !errors.Is(err, sdk.ErrIncomplete) || !strings.Contains(err.Error(), "fatal: something broke") {
		t.Fatalf("crash err = %v", err)
	}

	_, err = helperClient(t, "future").Describe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "protocol v2") {
		t.Fatalf("future err = %v", err)
	}

	_, err = (&Client{Name: "nope", Path: filepath.Join(t.TempDir(), "missing")}).Describe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "can't run") {
		t.Fatalf("missing err = %v", err)
	}
}

func TestClientCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := helperClient(t, "slow").Discover(ctx, sdk.Request{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("cancel took %s", time.Since(start))
	}
}

func TestFind(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDir, filepath.Join(dir, "providers"))
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	onPath := filepath.Join(bin, exeName("demo"))
	if err := os.WriteFile(onPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	in, err := Find("owner/demo")
	if err != nil {
		t.Fatal(err)
	}
	if in.Origin != OriginPath || in.Path != onPath {
		t.Errorf("found %+v", in)
	}

	inst := filepath.Join(dir, "providers", "demo")
	if err := os.MkdirAll(inst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inst, exeName("demo")), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inst, receiptFile), []byte(`{"source":"owner/demo","version":"v0.1.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	in, err = Find("demo")
	if err != nil {
		t.Fatal(err)
	}
	if in.Origin != OriginInstalled || in.Version != "v0.1.0" || len(in.Shadowed) != 1 {
		t.Errorf("found %+v", in)
	}
	list, err := List()
	if err != nil || len(list) != 1 || len(list[0].Shadowed) != 1 {
		t.Errorf("list %+v, %v", list, err)
	}

	if _, err := Find("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if in, err := Find(onPath); err != nil || in.Origin != OriginFile || in.Name != "demo" {
		t.Errorf("by path: %+v %v", in, err)
	}
	if err := Remove("demo"); err != nil {
		t.Fatal(err)
	}
	if in, _ := Find("demo"); in == nil || in.Origin != OriginPath {
		t.Errorf("after remove: %+v", in)
	}
}

func TestParseSourceAndName(t *testing.T) {
	for in, want := range map[string]string{
		"hayone1/grafana":                                 "hayone1/grafana",
		"hayone1/gitopsify-provider-grafana":              "hayone1/grafana",
		"github.com/acme/gitopsify-provider-keycloak.git": "acme/keycloak",
	} {
		src, err := ParseSource(in)
		if err != nil || src.String() != want {
			t.Errorf("ParseSource(%q) = %v, %v", in, src, err)
		}
	}
	for _, bad := range []string{"grafana", "a/b/c", "a/Bad_Name", "/x"} {
		if _, err := ParseSource(bad); err == nil {
			t.Errorf("ParseSource(%q) should fail", bad)
		}
	}
	if NameOf("acme/gitopsify-provider-x@v1") != "x" {
		t.Error("NameOf")
	}
}

func TestBuiltInIndexIsValid(t *testing.T) {
	idx, err := ParseIndex(providers.Index)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.Lookup("grafana"); !ok {
		t.Error("grafana missing from the index")
	}
	for i := 1; i < len(idx.Providers); i++ {
		if idx.Providers[i-1].Name > idx.Providers[i].Name {
			t.Errorf("index isn't sorted: %s before %s", idx.Providers[i-1].Name, idx.Providers[i].Name)
		}
	}
	if got := idx.Search("dashboards"); len(got) != 1 {
		t.Errorf("search: %v", got)
	}
}

func TestResolve(t *testing.T) {
	file := filepath.Join(t.TempDir(), "index.yaml")
	if err := os.WriteFile(file, []byte("kind: ProviderIndex\nproviders:\n- name: demo\n  source: acme/demo\n  description: d\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src, v, err := Resolve(context.Background(), "demo@v1.0.0", file)
	if err != nil || src.String() != "acme/demo" || v != "v1.0.0" {
		t.Fatalf("%v %q %v", src, v, err)
	}
	src, v, err = Resolve(context.Background(), "other/demo", file)
	if err != nil || src.String() != "other/demo" || v != "" {
		t.Fatalf("%v %q %v", src, v, err)
	}
	if _, _, err := Resolve(context.Background(), "nope", file); err == nil || !strings.Contains(err.Error(), "install it by source") {
		t.Fatalf("err = %v", err)
	}
}

// fakeGitHub serves one release of acme/gitopsify-provider-demo.
func fakeGitHub(t *testing.T, binary []byte, corrupt bool) *httptest.Server {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "README.md", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.WriteHeader(&tar.Header{Name: exeName("demo"), Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(binary)
	_ = tw.Close()
	_ = gz.Close()
	name := fmt.Sprintf("gitopsify-provider-demo_1.0.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(archive.Bytes())
	if corrupt {
		sum[0] ^= 0xff
	}
	sums := fmt.Sprintf("%s  %s\n%s  other.tar.gz\n", hex.EncodeToString(sum[:]), name, strings.Repeat("0", 64))

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/acme/gitopsify-provider-demo/releases/tags/v1.0.0", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(release{TagName: "v1.0.0", Assets: []asset{
			{Name: name, URL: srv.URL + "/dl/archive"},
			{Name: "gitopsify-provider-demo_1.0.0_checksums.txt", URL: srv.URL + "/dl/sums"},
		}})
	})
	mux.HandleFunc("/dl/archive", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/octet-stream" {
			http.Error(w, "accept", http.StatusBadRequest)
			return
		}
		_, _ = w.Write(archive.Bytes())
	})
	mux.HandleFunc("/dl/sums", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(sums)) })
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestInstall(t *testing.T) {
	srv := fakeGitHub(t, []byte("binary"), false)
	dir := t.TempDir()
	var verified string
	i := &Installer{Dir: dir, API: srv.URL, Token: "t", Verify: func(_ context.Context, p, name string) (string, error) {
		b, _ := os.ReadFile(p)
		verified = name + ":" + string(b)
		return "v1.0.0", nil
	}}
	in, err := i.Install(context.Background(), Source{Owner: "acme", Repo: "gitopsify-provider-demo", Name: "demo"}, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if verified != "demo:binary" {
		t.Errorf("verified %q", verified)
	}
	if in.Version != "v1.0.0" || in.Source != "acme/demo" {
		t.Errorf("installed %+v", in)
	}
	got, ok := readInstalled(dir, "demo")
	if !ok || got.Version != "v1.0.0" {
		t.Errorf("read back %+v", got)
	}

	// A failing verification keeps the installed version.
	i.Verify = func(context.Context, string, string) (string, error) { return "", errors.New("exec format error") }
	if _, err := i.Install(context.Background(), Source{Owner: "acme", Repo: "gitopsify-provider-demo", Name: "demo"}, "v1.0.0"); err == nil {
		t.Fatal("want an error")
	}
	if got, ok := readInstalled(dir, "demo"); !ok || got.Version != "v1.0.0" {
		t.Errorf("old install lost: %+v", got)
	}
}

func TestInstallRejectsBadChecksum(t *testing.T) {
	srv := fakeGitHub(t, []byte("binary"), true)
	i := &Installer{Dir: t.TempDir(), API: srv.URL, Verify: func(context.Context, string, string) (string, error) { return "", nil }}
	_, err := i.Install(context.Background(), Source{Owner: "acme", Repo: "gitopsify-provider-demo", Name: "demo"}, "v1.0.0")
	if err == nil || !strings.Contains(err.Error(), "doesn't match its checksum") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallMissingPlatformOrRelease(t *testing.T) {
	srv := fakeGitHub(t, []byte("binary"), false)
	i := &Installer{Dir: t.TempDir(), API: srv.URL, OS: "plan9", Arch: "mips"}
	_, err := i.Install(context.Background(), Source{Owner: "acme", Repo: "gitopsify-provider-demo", Name: "demo"}, "v1.0.0")
	if err == nil || !strings.Contains(err.Error(), "no build for plan9/mips") {
		t.Fatalf("err = %v", err)
	}
	_, err = i.Install(context.Background(), Source{Owner: "acme", Repo: "gitopsify-provider-demo", Name: "demo"}, "v9.9.9")
	if err == nil || !strings.Contains(err.Error(), "has no release v9.9.9") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallChecksPinsAndRepoID(t *testing.T) {
	srv := fakeGitHub(t, []byte("binary"), false)
	ok := func(context.Context, string, string) (string, error) { return "v1.0.0", nil }
	src := Source{Owner: "acme", Repo: "gitopsify-provider-demo", Name: "demo"}
	i := &Installer{Dir: t.TempDir(), API: srv.URL, Verify: ok, Checksums: map[string]string{runtime.GOOS + "_" + runtime.GOARCH: strings.Repeat("a", 64)}}
	if _, err := i.Install(context.Background(), src, "v1.0.0"); err == nil || !strings.Contains(err.Error(), "was pinned") {
		t.Fatalf("err = %v", err)
	}
	src.RepoID = 42
	i.Checksums = nil
	if _, err := i.Install(context.Background(), src, "v1.0.0"); err == nil || !strings.Contains(err.Error(), "can't be found") {
		t.Fatalf("err = %v", err)
	}
}
