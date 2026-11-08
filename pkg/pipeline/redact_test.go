package pipeline

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/discovery"
	"github.com/hayone1/gitopsify/pkg/helm"
	"github.com/hayone1/gitopsify/pkg/model"
)

func TestCredentialPathsRedact(t *testing.T) {
	values := map[string]any{
		"auth": map[string]any{
			"postgresPassword": "fixture-value",
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
			"postgresPassword": "fixture-value",
			"existingSecret":   "db-credentials",
			"secretKeys":       map[string]any{"adminPasswordKey": "admin-password"},
			"pin":              map[string]any{"password": 1234},
		},
		"secrets": map[string]any{"DB_URL": "postgres://u:p@h/db", "ENCRYPTION_KEY": "k", "create": true, "name": "app",
			"mountPath": "/etc/secrets", "type": "Opaque", "refreshInterval": "1h", "github": "ghp_x", "annotations": map[string]any{"a/b": "c"}},
		"credentials":  map[string]any{"accessKey": "AKIA", "key": "k2", "secretContents": map[string]any{"cloud": "[default]\naws_access_key_id=x"}},
		"extraSecrets": map[string]any{"conn": map[string]any{"data": "Y29ubg==", "stringData": "x"}},
		"configs":      map[string]any{"secret": map[string]any{"extra": map[string]any{"ldap.bindPW": "pw"}}},
		"env": []any{map[string]any{"name": "API_TOKEN", "value": "t0k"}, map[string]any{"name": "LOG_LEVEL", "value": "debug"},
			map[string]any{"name": "TOKEN_TTL", "value": 3600}, map[string]any{"name": "SECRET_KEY_BASE", "value": "base"},
			map[string]any{"name": "DB_PASS", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "db", "key": "password"}}}},
		"image":             map[string]any{"tag": "1.2"},
		"imagePullSecrets":  []any{"regcred"},
		"externalDatabase":  map[string]any{"url": "postgres://app:s3cret@db:5432/app", "hosts": []any{"redis://:pw@cache:6379"}},
		"passwordMinLength": 8,
		"tokenTTL":          3600,
		"spin":              3,
		"adminPin":          1234,
	}
	got := redactCredentials(values, "")
	want := []string{"adminPin", "auth.pin.password", "auth.postgresPassword", "configs.secret.extra.ldap.bindPW",
		"credentials.accessKey", "credentials.key", "credentials.secretContents.cloud", "env[0].value", "env[3].value",
		"externalDatabase.hosts[0]", "externalDatabase.url", "extraSecrets.conn.data", "extraSecrets.conn.stringData",
		"secrets.DB_URL", "secrets.ENCRYPTION_KEY", "secrets.github"}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("redacted %v\nwant %v", got, want)
	}
	auth := values["auth"].(map[string]any)
	if auth["existingSecret"] != "db-credentials" || auth["secretKeys"].(map[string]any)["adminPasswordKey"] != "admin-password" {
		t.Errorf("references were redacted: %v", auth)
	}
	if s := values["secrets"].(map[string]any); s["name"] != "app" || s["create"] != true || s["mountPath"] != "/etc/secrets" ||
		s["type"] != "Opaque" || s["refreshInterval"] != "1h" || s["annotations"].(map[string]any)["a/b"] != "c" {
		t.Errorf("secrets: %v", s)
	}
	if e := values["env"].([]any); e[1].(map[string]any)["value"] != "debug" || e[2].(map[string]any)["value"] != 3600 ||
		e[4].(map[string]any)["valueFrom"].(map[string]any)["secretKeyRef"].(map[string]any)["key"] != "password" {
		t.Errorf("env: %v", e)
	}
	if u := values["externalDatabase"].(map[string]any)["url"]; u != "postgres://app:<redacted>@db:5432/app" {
		t.Errorf("url: %v", u)
	}
	if values["passwordMinLength"] != 8 || values["tokenTTL"] != 3600 || values["spin"] != 3 || values["imagePullSecrets"].([]any)[0] != "regcred" {
		t.Errorf("settings redacted: %v", values)
	}
	if again := redactCredentials(values, ""); len(again) != 0 {
		t.Errorf("redacted twice: %v", again)
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"postgres://app:s3cret@db/app":                        "postgres://app:<redacted>@db/app",
		"smtp://alerts@example.com:pw@smtp.example.com:587":   "smtp://alerts@example.com:<redacted>@smtp.example.com:587",
		"postgres://u:p@ss@h/db":                              "postgres://u:<redacted>@h/db",
		"redis://:pw@cache:6379":                              "redis://:<redacted>@cache:6379",
		"http://[::1]:8080?a=b@c":                             "http://[::1]:8080?a=b@c",
		"https://user@example.com/x":                          "https://user@example.com/x",
		"postgres://app:$(DB_PASSWORD)@db/app":                "postgres://app:$(DB_PASSWORD)@db/app",
		"postgres://app:${DB_PASSWORD}@db/app":                "postgres://app:${DB_PASSWORD}@db/app",
		"registry.example.com/app@sha256:abc":                 "registry.example.com/app@sha256:abc",
		"postgres://app:<redacted>@db/app":                    "postgres://app:<redacted>@db/app",
		"amqp://guest:gu%40st@[2001:db8::1]:5672/vhost?x=y@z": "amqp://guest:<redacted>@[2001:db8::1]:5672/vhost?x=y@z",
	} {
		if got, _ := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
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
			if err == nil || !strings.Contains(err.Error(), "to another namespace or name") {
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

// Secrets a chart renders aren't written, so they aren't protected (and
// don't stop a namespace rename of their release); the others are.
func TestProtectSecretsMarks(t *testing.T) {
	cfg := &config.Config{}
	cfg.Secrets.Strategy = config.SecretsSOPS
	cfg.Secrets.Overrides = []config.SecretOverride{{Names: []string{"ext"}, Strategy: config.SecretsExternalSecrets}}
	cfg.Default()
	obj := func(ns, name, release string) *model.Object {
		return &model.Object{Ref: model.Ref{Kind: "Secret", Namespace: ns, Name: name}, HelmRelease: release}
	}
	charted, vendored, manifests, plain, ext := obj("a", "c", "a/chart"), obj("a", "v", "a/vendor"), obj("a", "m", "a/manifests"), obj("a", "p", ""), obj("a", "ext", "")
	inv := &model.Inventory{
		Objects: []*model.Object{charted, vendored, manifests, plain, ext},
		Releases: []*model.HelmRelease{{Name: "chart", Namespace: "a", Mode: model.HelmModeChart},
			{Name: "vendor", Namespace: "a", Mode: model.HelmModeVendor}, {Name: "manifests", Namespace: "a", Mode: model.HelmModeManifests}},
	}
	protectSecrets(inv, cfg)
	for o, want := range map[*model.Object]bool{charted: false, vendored: false, manifests: true, plain: true, ext: false} {
		if o.Protected != want {
			t.Errorf("%s protected %v, want %v", o.Ref, o.Protected, want)
		}
	}
}
