package config

import (
	"os"
	"strings"
	"testing"
)

// TestServerSchemaIsCurrent keeps docs/gitopsify-server.schema.json in step.
func TestServerSchemaIsCurrent(t *testing.T) {
	want, err := ServerSchema()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../../docs/gitopsify-server.schema.json")
	if err != nil || string(got) != string(want) {
		if os.Getenv("UPDATE_DOCS") != "" {
			if err := os.WriteFile("../../docs/gitopsify-server.schema.json", want, 0o644); err != nil {
				t.Fatal(err)
			}
			return
		}
		t.Error("docs/gitopsify-server.schema.json is out of date: run make docs")
	}
	if !strings.Contains(string(want), `"postgres"`) || !strings.Contains(string(want), `"trustedProxies"`) {
		t.Error("the schema lacks fields")
	}
}

func TestLoadServer(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/server.yaml"
	ok := "apiVersion: gitopsify.io/v1alpha1\nkind: Server\nmetadata: {name: s}\nspec:\n  externalURL: https://g.acme.io\n  auth:\n    oidc: {issuer: https://idp, clientID: g}\n"
	if err := os.WriteFile(p, []byte(ok), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := LoadServer(p); err != nil || s.Spec.Storage.Type != "file" {
		t.Fatalf("%+v %v", s, err)
	}
	for name, spec := range map[string]string{
		"local mode":   "  auth: {mode: local}\n",
		"path in url":  "  externalURL: https://g.acme.io/gitopsify\n  auth:\n    oidc: {issuer: https://idp, clientID: g}\n",
		"unknown key":  "  externalURL: https://g.acme.io\n  auth:\n    oidc: {issuer: https://idp, clientID: g}\n  nope: 1\n",
		"no pg url":    "  externalURL: https://g.acme.io\n  storage: {type: postgres}\n  auth:\n    oidc: {issuer: https://idp, clientID: g}\n",
		"ssh no hosts": "  externalURL: https://g.acme.io\n  git: {credentials: [{host: x, sshKeyFile: /k}]}\n  auth:\n    oidc: {issuer: https://idp, clientID: g}\n",
		"bad proxy":    "  externalURL: https://g.acme.io\n  trustedProxies: [nope]\n  auth:\n    oidc: {issuer: https://idp, clientID: g}\n",
		"zero ttl":     "  externalURL: https://g.acme.io\n  auth:\n    sessionTTL: 0s\n    oidc: {issuer: https://idp, clientID: g}\n",
	} {
		if err := os.WriteFile(p, []byte("apiVersion: gitopsify.io/v1alpha1\nkind: Server\nmetadata: {name: s}\nspec:\n"+spec), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadServer(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
