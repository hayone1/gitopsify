package pipeline

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/discovery"
	"github.com/hayone1/gitopsify/pkg/helm"
)

func TestCredentialPathsRedact(t *testing.T) {
	values := map[string]any{
		"auth": map[string]any{
			"postgresPassword": "hunter2",
			"existingSecret":   "db-credentials",
			"secretName":       "db",
			"enabled":          true,
		},
		"image":   map[string]any{"tag": "1.2"},
		"tokens":  []any{map[string]any{"apiKey": "k"}},
		"jwtRef":  "secret-ref",
		"replica": 2,
	}
	got := credentialPaths(values, "", true)
	want := []string{"auth.postgresPassword", "tokens[0].apiKey"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths %v, want %v", got, want)
	}
	auth := values["auth"].(map[string]any)
	if auth["postgresPassword"] != RedactedValue || auth["existingSecret"] != "db-credentials" {
		t.Errorf("redacted: %v", auth)
	}
	if again := credentialPaths(values, "", false); len(again) != 0 {
		t.Errorf("redacted values still reported: %v", again)
	}
}

func TestRedactCredentials(t *testing.T) {
	values := map[string]any{
		"auth": map[string]any{
			"postgresPassword": "hunter2",
			"existingSecret":   "db-credentials",
			"secretKeys":       map[string]any{"adminPasswordKey": "admin-password"},
			"pin":              map[string]any{"password": 1234},
		},
		"secrets": map[string]any{"DB_URL": "postgres://u:p@h/db", "ENCRYPTION_KEY": "k", "create": true, "name": "app",
			"mountPath": "/etc/secrets", "type": "Opaque", "refreshInterval": "1h"},
		"credentials": map[string]any{"accessKey": "AKIA", "key": "k2"},
		"env": []any{map[string]any{"name": "API_TOKEN", "value": "t0k"}, map[string]any{"name": "LOG_LEVEL", "value": "debug"},
			map[string]any{"name": "TOKEN_TTL", "value": 3600}},
		"image":             map[string]any{"tag": "1.2"},
		"externalDatabase":  map[string]any{"url": "postgres://app:s3cret@db:5432/app", "hosts": []any{"redis://:pw@cache:6379"}},
		"passwordMinLength": 8,
		"tokenTTL":          3600,
	}
	got := redactCredentials(values, "")
	want := []string{"auth.pin.password", "auth.postgresPassword", "credentials.accessKey", "credentials.key", "env[0].value",
		"externalDatabase.hosts[0]", "externalDatabase.url", "secrets.DB_URL", "secrets.ENCRYPTION_KEY"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("redacted %v, want %v", got, want)
	}
	auth := values["auth"].(map[string]any)
	if auth["existingSecret"] != "db-credentials" || auth["secretKeys"].(map[string]any)["adminPasswordKey"] != "admin-password" {
		t.Errorf("references were redacted: %v", auth)
	}
	if s := values["secrets"].(map[string]any); s["name"] != "app" || s["create"] != true || s["mountPath"] != "/etc/secrets" || s["type"] != "Opaque" {
		t.Errorf("secrets: %v", s)
	}
	if u := values["externalDatabase"].(map[string]any)["url"]; u != "postgres://app:<redacted>@db:5432/app" {
		t.Errorf("url: %v", u)
	}
	if values["passwordMinLength"] != 8 || values["tokenTTL"] != 3600 {
		t.Errorf("settings redacted: %v %v", values["passwordMinLength"], values["tokenTTL"])
	}
	if e := values["env"].([]any)[1].(map[string]any); e["value"] != "debug" {
		t.Errorf("env: %v", e)
	}
	if again := redactCredentials(values, ""); len(again) != 0 {
		t.Errorf("redacted twice: %v", again)
	}
}

// With ProtectSecrets, Secrets with encrypted values can't be moved by
// transforms; without, they can (the CLI).
func TestProtectSecretsScan(t *testing.T) {
	objs, err := discovery.ReadObjects([]string{fixture})
	if err != nil {
		t.Fatal(err)
	}
	for _, protect := range []bool{false, true} {
		cfg := &config.Config{}
		cfg.Secrets.Strategy = config.SecretsSOPS
		cfg.Secrets.SOPS.Age = []string{"age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"}
		cfg.Transforms = []config.Transform{{Type: "namespace", Map: map[string]string{"shop": "mine"}}}
		cfg.Default()
		res, err := Scan(context.Background(), Options{Config: cfg, Offline: deepCopies(objs), OfflineName: "clickops.yaml",
			Resolver: &helm.Resolver{}, Version: "test", ProtectSecrets: protect})
		if protect {
			if err == nil || !strings.Contains(err.Error(), "keeps its namespace and name") {
				t.Errorf("protected: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("unprotected: %v", err)
		}
		moved := false
		for _, o := range res.Inventory.Objects {
			if o.IsSecret() && o.Out != nil && o.Out.GetNamespace() == "mine" {
				moved = true
			}
		}
		if !moved {
			t.Error("no Secret moved without protection")
		}
	}
}
