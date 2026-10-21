package config

import (
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
