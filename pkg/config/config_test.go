package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsValidate(t *testing.T) {
	c := New()
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	if c.Output.ArgoCD.Prune == nil || !*c.Output.ArgoCD.Prune {
		t.Error("pruning is on by default")
	}
	a := &Config{Adopt: Adopt{Enabled: true}}
	a.Default()
	if *a.Output.ArgoCD.Prune {
		t.Error("takeover must default to no pruning")
	}
}

func TestValidateErrors(t *testing.T) {
	c := New()
	c.Output.Tool = ToolArgoCD
	c.Secrets.Strategy = SecretsPlain
	c.Transforms = []Transform{{Type: "replace"}, {Type: "nope"}}
	c.Environments = []Environment{{Name: "UAT"}, {Name: "x"}, {Name: "x"}}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"output.repo.url", "allowPlain", "replace needs from", "unknown transform type", `"UAT" must be a DNS label`, "defined twice"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}

func TestLoadStrict(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	_ = os.WriteFile(good, []byte("apiVersion: gitopsify.io/v1alpha1\nkind: Migration\nscope:\n  namespaces: [shop]\n  rules:\n    - action: exclude\n      kinds: [Secret]\n"), 0o600)
	c, err := Load(good)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Scope.Rules) != 1 || c.Scope.Rules[0].Kinds[0] != "Secret" {
		t.Errorf("inline selector not parsed: %+v", c.Scope.Rules)
	}
	bad := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(bad, []byte("apiVersion: gitopsify.io/v1alpha1\nkind: Migration\nscope:\n  namespace: [shop]\n"), 0o600)
	if _, err := Load(bad); err == nil {
		t.Error("unknown field accepted")
	}
}

func TestAccessInConfigFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "access.yaml")
	_ = os.WriteFile(p, []byte(`apiVersion: gitopsify.io/v1alpha1
kind: Migration
source:
  kubeconfig: ~/.kube/test.yaml
  context: test
  impersonate: jane
environments:
  - name: uat
    cluster:
      server: https://kubernetes.default.svc
      apiServer: https://uat.example.com:6443
      tokenFile: /var/run/secrets/uat/token
      certificateAuthority: uat-ca.pem
`), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	c.Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	e := c.Environments[0].Cluster
	if e.Server != "https://kubernetes.default.svc" || e.APIServer != "https://uat.example.com:6443" || !e.Configured() {
		t.Errorf("cluster access not parsed: %+v", e)
	}
	if c.Source.Context != "test" || c.Source.Impersonate != "jane" {
		t.Errorf("source access not parsed: %+v", c.Source)
	}

	// A token never goes into the file, and so never into the repository.
	c.Source.Token = "tok-3x4mpl3"
	b, _ := c.Marshal()
	if strings.Contains(string(b), "tok-3x4mpl3") {
		t.Errorf("token written to the configuration:\n%s", b)
	}
	tok := filepath.Join(dir, "token.yaml")
	_ = os.WriteFile(tok, []byte("apiVersion: gitopsify.io/v1alpha1\nkind: Migration\nsource:\n  token: abc\n"), 0o600)
	if _, err := Load(tok); err == nil || !strings.Contains(err.Error(), "tokens don't belong") {
		t.Errorf("token in file: %v", err)
	}

	bad := New()
	bad.Source.InCluster, bad.Source.Context = true, "x"
	bad.Environments[0].Cluster.APIServer = "uat:6443"
	err = bad.Validate()
	for _, want := range []string{"source: inCluster", "apiServer \"uat:6443\""} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestExpandHome(t *testing.T) {
	t.Setenv("HOME", "/home/me")
	sep := string(os.PathListSeparator)
	if got := ExpandHome("~/a" + sep + "/b" + sep + "~"); got != "/home/me/a"+sep+"/b"+sep+"/home/me" {
		t.Errorf("got %q", got)
	}
	if got := ExpandHome("a~b"); got != "a~b" {
		t.Errorf("got %q", got)
	}
}

// TestSchemaIsCurrent keeps docs/gitopsify.schema.json in step with Config;
// run make docs after changing the configuration.
func TestSchemaIsCurrent(t *testing.T) {
	want, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(want, &probe); err != nil {
		t.Fatal(err)
	}
	props := probe["properties"].(map[string]any)
	src := props["source"].(map[string]any)["properties"].(map[string]any)
	if _, ok := src["token"]; ok {
		t.Error("token must not be part of the schema")
	}
	if _, ok := src["apiServer"]; !ok {
		t.Error("inline access fields missing from source")
	}
	got, err := os.ReadFile("../../docs/gitopsify.schema.json")
	if err != nil || string(got) != string(want) {
		if os.Getenv("UPDATE_DOCS") != "" {
			if err := os.WriteFile("../../docs/gitopsify.schema.json", want, 0o644); err != nil {
				t.Fatal(err)
			}
			return
		}
		t.Error("docs/gitopsify.schema.json is out of date: run make docs")
	}
}
