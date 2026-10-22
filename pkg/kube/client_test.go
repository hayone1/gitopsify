package kube

import (
	"encoding/base64"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hayone1/gitopsify/pkg/config"
)

// idToken is an unsigned JWT that expires in 2100; client-go's oidc provider
// only reads its expiry.
var idToken = "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://issuer.invalid","exp":4102444800}`)) + ".c2ln"

// apiServer answers /version for requests carrying the bearer token "good".
func apiServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "Bearer good" && a != "Bearer "+idToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","code":401,"message":"Unauthorized"}`))
			return
		}
		if r.URL.Path == "/version" {
			_, _ = w.Write([]byte(`{"major":"1","minor":"37","gitVersion":"v1.37.0"}`))
			return
		}
		http.NotFound(w, r)
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, ca
}

const kubeconfigTpl = `apiVersion: v1
kind: Config
current-context: %[1]s
clusters:
- name: %[1]s
  cluster:
    server: %[2]s
    insecure-skip-tls-verify: true
contexts:
- name: %[1]s
  context: {cluster: %[1]s, user: %[1]s}
users:
- name: %[1]s
  user:
%[3]s
`

func writeKubeconfig(t *testing.T, name, server, user string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name+".yaml")
	content := strings.NewReplacer("%[1]s", name, "%[2]s", server, "%[3]s", user).Replace(kubeconfigTpl)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStandaloneServerAndToken(t *testing.T) {
	srv, ca := apiServer(t)
	// A kubeconfig pointing elsewhere must not leak into an explicit server.
	t.Setenv("KUBECONFIG", writeKubeconfig(t, "other", "https://other.invalid:6443", "    token: other"))
	c, err := Connect(config.Access{APIServer: srv.URL, Token: "good", CertificateAuthority: ca}, ConnectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := c.Ping(); err != nil || v != "v1.37.0" {
		t.Fatalf("Ping = %q, %v", v, err)
	}
	if c.Auth != "bearer token" || c.Context != srv.URL {
		t.Errorf("auth %q, name %q", c.Auth, c.Context)
	}
}

func TestTokenFileAndTLSServerName(t *testing.T) {
	srv, ca := apiServer(t)
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("good"), 0o600); err != nil {
		t.Fatal(err)
	}
	// httptest certificates are valid for example.com and 127.0.0.1.
	url := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	c, err := Connect(config.Access{APIServer: url, TokenFile: tok, CertificateAuthority: ca}, ConnectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ping(); err == nil || !strings.Contains(err.Error(), "--tls-server-name") {
		t.Fatalf("expected a --tls-server-name hint, got %v", err)
	}
	c, err = Connect(config.Access{APIServer: url, TokenFile: tok, CertificateAuthority: ca, TLSServerName: "example.com"}, ConnectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ping(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.Auth, "bearer token from ") {
		t.Errorf("auth %q", c.Auth)
	}
}

func TestExplainedErrors(t *testing.T) {
	srv, ca := apiServer(t)
	c, err := Connect(config.Access{APIServer: srv.URL, Token: "bad", CertificateAuthority: ca}, ConnectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ping(); err == nil || !strings.Contains(err.Error(), "rejected the credentials") {
		t.Errorf("401: %v", err)
	}
	c, _ = Connect(config.Access{APIServer: srv.URL, Token: "good"}, ConnectOptions{})
	if _, err := c.Ping(); err == nil || !strings.Contains(err.Error(), "--certificate-authority") {
		t.Errorf("untrusted CA: %v", err)
	}
	c, _ = Connect(config.Access{APIServer: "https://127.0.0.1:1", Token: "good", InsecureSkipTLSVerify: true}, ConnectOptions{})
	if _, err := c.Ping(); err == nil || !strings.Contains(err.Error(), "Can't reach") {
		t.Errorf("unreachable: %v", err)
	}
}

func TestKubeconfigOverridesAndLists(t *testing.T) {
	srv, _ := apiServer(t)
	a := writeKubeconfig(t, "a", srv.URL, "    token: good")
	b := writeKubeconfig(t, "b", "https://b.invalid:6443", "    token: other")
	t.Setenv("KUBECONFIG", "")
	// Several files merge like KUBECONFIG; pick the context from the second.
	c, err := Connect(config.Access{Kubeconfig: b + string(os.PathListSeparator) + a, Context: "a"}, ConnectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ping(); err != nil {
		t.Fatal(err)
	}
	if c.Context != "a" || c.Auth != "bearer token, TLS verification off" {
		t.Errorf("context %q auth %q", c.Context, c.Auth)
	}
	// The server and token override the context's.
	c, err = Connect(config.Access{Kubeconfig: b, APIServer: srv.URL, Token: "good"}, ConnectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ping(); err != nil {
		t.Fatal(err)
	}
	if _, err := Connect(config.Access{Kubeconfig: a, Context: "nope"}, ConnectOptions{}); err == nil || !strings.Contains(err.Error(), "contexts in the kubeconfig: a") {
		t.Errorf("unknown context: %v", err)
	}
}

func TestHomeExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srv, _ := apiServer(t)
	p := writeKubeconfig(t, "h", srv.URL, "    token: good")
	if err := os.Rename(p, filepath.Join(home, "kc.yaml")); err != nil {
		t.Fatal(err)
	}
	c, err := Connect(config.Access{Kubeconfig: "~/kc.yaml"}, ConnectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ping(); err != nil {
		t.Fatal(err)
	}
}

func TestOIDCAndExecPlugins(t *testing.T) {
	srv, _ := apiServer(t)
	oidc := writeKubeconfig(t, "oidc", srv.URL, `    auth-provider:
      name: oidc
      config:
        client-id: gitopsify
        idp-issuer-url: https://issuer.invalid
        id-token: `+idToken)
	// The provider must be registered: without it the client can't be built.
	c, err := Connect(config.Access{Kubeconfig: oidc}, ConnectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth != "auth provider oidc, TLS verification off" {
		t.Errorf("auth %q", c.Auth)
	}
	if _, err := c.Ping(); err != nil {
		t.Fatalf("oidc id-token: %v", err)
	}

	gke := writeKubeconfig(t, "gke", srv.URL, `    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: gke-gcloud-auth-plugin-not-installed
      interactiveMode: IfAvailable
      provideClusterInfo: true`)
	cfg, _, err := RESTConfig(config.Access{Kubeconfig: gke}, ConnectOptions{NoInput: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ExecProvider.StdinUnavailable {
		t.Error("--no-input must keep credential plugins from prompting")
	}
	c, err = Connect(config.Access{Kubeconfig: gke}, ConnectOptions{NoInput: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ping(); err == nil || !strings.Contains(err.Error(), "gets credentials by running") {
		t.Errorf("missing plugin: %v", err)
	}
}

func TestDescribeKnownPlugins(t *testing.T) {
	srv, _ := apiServer(t)
	eks := writeKubeconfig(t, "eks", srv.URL, `    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: aws
      args: [eks, get-token, --cluster-name, prod]`)
	c, err := Connect(config.Access{Kubeconfig: eks, Impersonate: "jane", ImpersonateGroups: []string{"ops"}}, ConnectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := "credential plugin aws (Amazon EKS), impersonating jane (groups ops), TLS verification off"
	if c.Auth != want {
		t.Errorf("auth\n got %q\nwant %q", c.Auth, want)
	}
}

func TestInvalidAccess(t *testing.T) {
	for _, a := range []config.Access{
		{InCluster: true, Context: "x"},
		{ClientCertificate: "c.pem"},
		{InsecureSkipTLSVerify: true, CertificateAuthority: "ca.pem"},
		{APIServer: "10.0.0.1:6443"},
		{Timeout: "soon"},
		{Token: "a", TokenFile: "b"},
	} {
		if _, err := Connect(a, ConnectOptions{}); err == nil {
			t.Errorf("%+v: expected an error", a)
		}
	}
	if _, err := Connect(config.Access{InCluster: true}, ConnectOptions{}); err == nil || !strings.Contains(err.Error(), "inside a pod") {
		t.Errorf("in-cluster outside a pod: %v", err)
	}
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	_, err := Connect(config.Access{}, ConnectOptions{})
	if err == nil || !strings.Contains(err.Error(), "no kubeconfig found") {
		t.Errorf("no kubeconfig: %v", err)
	}
}
