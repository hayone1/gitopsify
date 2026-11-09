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
	obj := func(m any, key string) map[string]any {
		t.Helper()
		v, ok := m.(map[string]any)[key].(map[string]any)
		if !ok {
			t.Fatalf("schema: no object at %q", key)
		}
		return v
	}
	// The resource form, and v0.1's layout with the fields at the top.
	if obj(probe, "then")["$ref"] != "#/$defs/export" || obj(obj(probe, "$defs"), "export")["properties"] == nil {
		t.Error("exports in the file aren't checked against their schema")
	}
	migration := obj(probe, "else")
	resource := obj(obj(migration, "then"), "properties")
	spec := obj(obj(resource, "spec"), "properties")
	flat := obj(obj(migration, "else"), "properties")
	if obj(obj(obj(resource, "metadata"), "properties"), "name")["pattern"] == nil {
		t.Error("metadata.name has no pattern")
	}
	for form, props := range map[string]map[string]any{"resource": spec, "v0.1": flat} {
		src := obj(obj(props, "source"), "properties")
		if _, ok := src["token"]; ok {
			t.Errorf("%s: token must not be part of the schema", form)
		}
		if _, ok := src["apiServer"]; !ok {
			t.Errorf("%s: inline access fields missing from source", form)
		}
		if _, ok := obj(obj(props, "helm"), "properties")["mode"].(map[string]any)["enum"]; !ok {
			t.Errorf("%s: enums missing", form)
		}
	}
	for _, f := range []string{"apiVersion", "kind", "metadata"} {
		if _, ok := spec[f]; ok {
			t.Errorf("%s inside spec", f)
		}
		if _, ok := resource[f]; !ok {
			t.Errorf("%s missing from the resource", f)
		}
	}
	if _, ok := flat["apiVersion"]; !ok {
		t.Error("v0.1 layout: apiVersion missing")
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

func TestPortable(t *testing.T) {
	c := New()
	c.Source.Access = Access{Context: "test", Kubeconfig: "/home/me/.kube/config", InCluster: true, TokenFile: "/run/token"}
	c.Environments = []Environment{{Name: "uat", Cluster: Cluster{Server: "https://kubernetes.default.svc", Access: Access{Context: "uat", Kubeconfig: "/x"}}}}
	c.Output.Dir = "/work/gitops"
	c.Default()
	b, err := c.Portable().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"/home/me", "inCluster", "/run/token", "/x", "/work/gitops"} {
		if strings.Contains(string(b), gone) {
			t.Errorf("%s kept:\n%s", gone, b)
		}
	}
	for _, kept := range []string{"context: test", "context: uat", "kubernetes.default.svc"} {
		if !strings.Contains(string(b), kept) {
			t.Errorf("%s lost:\n%s", kept, b)
		}
	}
	if c.Source.Access.Kubeconfig == "" || c.Output.Dir == "" {
		t.Error("Portable changed the configuration itself")
	}
	// A server fills access in from its registered clusters: their names
	// are what the copy keeps.
	c.Source.Access = Access{Context: "kind-test-admin", Kubeconfig: "/etc/kube"}
	c.Environments[0].Cluster.Access = Access{Context: "kind-uat-admin"}
	c.NameClusters("test", []string{"uat"})
	b, _ = c.Portable().Marshal()
	if strings.Contains(string(b), "admin") || !strings.Contains(string(b), "context: test") || !strings.Contains(string(b), "context: uat") {
		t.Errorf("registered names:\n%s", b)
	}
}
