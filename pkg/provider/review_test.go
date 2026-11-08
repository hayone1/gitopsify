package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hayone1/gitopsify/sdk"
)

func TestInstallRecordsEveryPlatformAndVerifies(t *testing.T) {
	srv := fakeGitHub(t, []byte("binary"), false)
	dir := t.TempDir()
	i := &Installer{Dir: dir, API: srv.URL, Verify: func(context.Context, string, string) (string, error) { return "v1.0.0", nil }}
	in, err := i.Install(context.Background(), Source{Owner: "acme", Repo: "gitopsify-provider-demo", Name: "demo"}, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	key := runtime.GOOS + "_" + runtime.GOARCH
	if in.Checksums[key] != in.SHA256 || in.Binary == "" {
		t.Fatalf("receipt %+v", in)
	}
	got, _ := readInstalled(dir, "demo")
	if err := got.Verify(map[string]string{key: in.SHA256}); err != nil {
		t.Errorf("verify: %v", err)
	}
	if err := got.Verify(map[string]string{key: strings.Repeat("0", 64)}); err == nil || !strings.Contains(err.Error(), "not the pinned") {
		t.Errorf("wrong pin: %v", err)
	}
	if err := got.Verify(map[string]string{"plan9_mips": "x"}); err == nil || !strings.Contains(err.Error(), "not for") {
		t.Errorf("missing platform: %v", err)
	}
	if err := os.WriteFile(got.Path, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := got.Verify(nil); err == nil || !strings.Contains(err.Error(), "changed since it was installed") {
		t.Errorf("tampered: %v", err)
	}
}

func TestInstallRejectsOddVersions(t *testing.T) {
	i := &Installer{Dir: t.TempDir(), API: "http://127.0.0.1:1"}
	_, err := i.Install(context.Background(), Source{Owner: "acme", Repo: "gitopsify-provider-demo", Name: "demo"}, "../../x")
	if err == nil || !strings.Contains(err.Error(), "isn't a release version") {
		t.Fatalf("err = %v", err)
	}
}

func TestLongStderrLineDoesNotHang(t *testing.T) {
	c := helperClient(t, "longline")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := c.Describe(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("describe took %s", time.Since(start))
	}
}

func TestIndexOverHTTPIsRefused(t *testing.T) {
	if _, err := LoadIndex(context.Background(), "http://example.com/index.yaml"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewerIndexFieldsAreTolerated(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "kind: ProviderIndex\nfutureField: 1\nproviders:\n- name: demo\n  source: acme/demo\n  description: d\n  signingKey: abc\n")
	}))
	defer srv.Close()
	old := http.DefaultClient
	http.DefaultClient = srv.Client()
	defer func() { http.DefaultClient = old }()
	idx, err := LoadIndex(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.Lookup("demo"); !ok {
		t.Error("demo missing")
	}
}

// longline mode: the helper writes a stderr line far over the scanner's
// limit before answering.
func init() {
	longLine = func() {
		if os.Getenv(helperEnv) == "longline" {
			fmt.Fprintln(os.Stderr, strings.Repeat("x", 6<<20))
		}
	}
}

var _ = sdk.APIVersion
