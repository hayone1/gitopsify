package pipeline

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

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
			"replicaSetKey":    "rs",
			"fernetKey":        "fk",
		},
		"secrets": map[string]any{"DB_URL": "postgres://u:p@h/db", "ENCRYPTION_KEY": "k", "create": true, "name": "app",
			"mountPath": "/etc/secrets", "type": "Opaque", "refreshInterval": "1h", "github": "ghp_x", "annotations": map[string]any{"a/b": "c"},
			"API_TOKEN": 12345678},
		"credentials":  map[string]any{"accessKey": "AKIA", "key": "k2", "secretContents": map[string]any{"cloud": "[default]\naws_access_key_id=x"}},
		"extraSecrets": map[string]any{"conn": map[string]any{"data": "Y29ubg==", "stringData": "x"}},
		"configs":      map[string]any{"secret": map[string]any{"extra": map[string]any{"ldap.bindPW": "pw"}}},
		"data":         map[string]any{"metadataConnection": map[string]any{"user": "airflow", "pass": "conn-pw", "host": "db"}},
		"ldap":         map[string]any{"bindpw": "ldap-pw", "binddn": "cn=admin"},
		"datasources":  []any{map[string]any{"name": "loki", "secureJsonData": map[string]any{"httpHeaderValue1": "Bearer x", "tlsClientKey": "k"}}},
		"env": []any{map[string]any{"name": "API_TOKEN", "value": "t0k"}, map[string]any{"name": "LOG_LEVEL", "value": "debug"},
			map[string]any{"name": "TOKEN_TTL", "value": 3600}, map[string]any{"name": "SECRET_KEY_BASE", "value": "base"},
			map[string]any{"name": "DB_PASS", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "db", "key": "password"}}},
			map[string]any{"name": "AIRFLOW__CORE__FERNET_KEY", "value": "f"}, map[string]any{"name": "SENTRY_DSN", "value": "https://k@sentry.io/1"},
			map[string]any{"name": "TOKEN_URL", "value": "https://idp/token"}, map[string]any{"name": "ADMIN_PIN", "value": "0000"}},
		"image":            map[string]any{"tag": "1.2"},
		"imagePullSecrets": []any{"regcred"},
		"externalDatabase": map[string]any{"url": "postgres://app:s3cret@db:5432/app", "hosts": []any{"redis://:pw@cache:6379"}},
		"args":             []any{"--db=postgres://u:p@h/db --log=info"},
		// References and settings stay.
		"global": map[string]any{"psql": map[string]any{"password": map[string]any{"secret": "gitlab-pg", "key": "password"}},
			"redis": map[string]any{"auth": map[string]any{"enabled": true, "secret": "gitlab-redis", "key": "secret"}}},
		"backupStorageLocation": []any{map[string]any{"credential": map[string]any{"name": "cloud-creds", "key": "cloud"}}},
		"remoteWrite":           []any{map[string]any{"basicAuth": map[string]any{"password": map[string]any{"name": "rw", "key": "pw"}}}},
		"secret":                []any{map[string]any{"envName": "X", "secretName": "airflow-x", "secretKey": "x"}},
		"envFromSecret":         "grafana-env",
		"tls":                   map[string]any{"certificatesSecret": "redis-tls"},
		"alertmanagerSpec":      map[string]any{"secrets": []any{"am-config"}},
		"externalRedis":         map[string]any{"secretAnnotations": map[string]any{"helm.sh/resource-policy": "keep"}},
		"passwordUpdateJob":     map[string]any{"resourcesPreset": "micro"},
		"tolerations":           []any{map[string]any{"key": "dedicated", "operator": "Equal", "value": "db"}},
		"affinity":              map[string]any{"matchExpressions": []any{map[string]any{"key": "zone", "operator": "In"}}},
		"extraVolumes":          []any{map[string]any{"secret": map[string]any{"secretName": "x", "items": []any{map[string]any{"key": "k", "path": "p"}}}}},
		"grafana.ini":           map[string]any{"auth.generic_oauth": map[string]any{"token_url": "https://idp/token", "client_secret": "cs"}},
		"expiry":                map[string]any{"idTokens": "24h"},
		"nginx":                 map[string]any{"server-tokens": "off"},
		"basicAuth":             map[string]any{"htpasswd": "{{ htpasswd .Values.user .Values.pass }}"},
		"moreSecrets":           []any{map[string]any{"name": "github", "value": "ghp_y"}},
		"es":                    map[string]any{"hosts": "http://u:p@es1:9200,http://u:p2@es2:9200"},
		"es2":                   map[string]any{"hosts": "http://es1:9200,http://u:p3@es2:9200"},
		"cloudflare":            map[string]any{"apiToken": "cf", "secretName": ""},
		"alibabacloud":          map[string]any{"accessKeySecret": "ali"},
		"certificateSecret":     map[string]any{"crt": "-----BEGIN CERTIFICATE-----", "key": "inline-key"},
		"traefikTLS":            map[string]any{"cert": "/certs/tls.crt", "key": "/certs/tls.key"},
		"pem":                   []any{"-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----"},
		"primary":               map[string]any{"extraEnvVarsSecret": "pg-env", "initdb": map[string]any{"scriptsSecret": "pg-init"}},
		"ambientCredentials":    map[string]any{"keys": map[string]any{"clientSecret": "client-secret"}},
		"passwordMinLength":     8,
		"tokenTTL":              3600,
		"maxToken":              4096,
		"spin":                  3,
		"adminPin":              1234,
		"DBPassword":            1234,
		"apiKey":                987,
	}
	got := redactCredentials(values, "")
	want := []string{"DBPassword", "adminPin", "apiKey", "args[0]", "es.hosts", "es2.hosts", "cloudflare.apiToken", "alibabacloud.accessKeySecret", "certificateSecret.key", "pem[0]", "moreSecrets[0].value", "grafana.ini.auth.generic_oauth.client_secret", "auth.fernetKey", "auth.pin.password", "auth.postgresPassword", "auth.replicaSetKey",
		"configs.secret.extra.ldap.bindPW", "credentials.accessKey", "credentials.key", "credentials.secretContents.cloud",
		"data.metadataConnection.pass", "datasources[0].secureJsonData.httpHeaderValue1", "datasources[0].secureJsonData.tlsClientKey",
		"env[0].value", "env[3].value", "env[5].value", "env[6].value", "env[8].value",
		"externalDatabase.hosts[0]", "externalDatabase.url", "extraSecrets.conn.data", "extraSecrets.conn.stringData", "ldap.bindpw",
		"secrets.API_TOKEN", "secrets.DB_URL", "secrets.ENCRYPTION_KEY", "secrets.github"}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("redacted %v\nwant     %v", got, want)
	}
	auth := values["auth"].(map[string]any)
	if auth["existingSecret"] != "db-credentials" || auth["secretKeys"].(map[string]any)["adminPasswordKey"] != "admin-password" {
		t.Errorf("references were redacted: %v", auth)
	}
	if s := values["secrets"].(map[string]any); s["name"] != "app" || s["create"] != true || s["mountPath"] != "/etc/secrets" ||
		s["type"] != "Opaque" || s["refreshInterval"] != "1h" || s["annotations"].(map[string]any)["a/b"] != "c" {
		t.Errorf("secrets: %v", s)
	}
	if u := values["externalDatabase"].(map[string]any)["url"]; u != "postgres://app:<redacted>@db:5432/app" {
		t.Errorf("url: %v", u)
	}
	if a := values["args"].([]any)[0]; a != "--db=postgres://u:<redacted>@h/db --log=info" {
		t.Errorf("args: %v", a)
	}
	if h := values["es"].(map[string]any)["hosts"]; h != "http://u:<redacted>@es1:9200,http://u:<redacted>@es2:9200" {
		t.Errorf("hosts: %v", h)
	}
	if h := values["es2"].(map[string]any)["hosts"]; h != "http://es1:9200,http://u:<redacted>@es2:9200" {
		t.Errorf("hosts: %v", h)
	}
	if k := values["traefikTLS"].(map[string]any)["key"]; k != "/certs/tls.key" {
		t.Errorf("a key file path was redacted: %v", k)
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
		"postgresql://u:pw?q@h/db":                            "postgresql://u:<redacted>@h/db",
		"postgresql://u:pw#x@h/db":                            "postgresql://u:<redacted>@h/db",
		"postgresql://u:a/b+c=@h/db":                          "postgresql://u:<redacted>@h/db",
		"redis://:pw@cache:6379":                              "redis://:<redacted>@cache:6379",
		"http://[::1]:8080?a=b@c":                             "http://[::1]:8080?a=b@c",
		"http://h:8080/a@b":                                   "http://h:8080/a@b",
		"https://user@example.com/x":                          "https://user@example.com/x",
		"https://user:pw@registry/v2/@scope/pkg":              "https://user:<redacted>@registry/v2/@scope/pkg",
		"postgres://app:$(DB_PASSWORD)@db/app":                "postgres://app:$(DB_PASSWORD)@db/app",
		"postgres://app:${DB_PASSWORD}@db/app":                "postgres://app:${DB_PASSWORD}@db/app",
		"postgres://app:${A}hunter2${B}@db/app":               "postgres://app:<redacted>@db/app",
		"postgres://app:{{ .Values.pass }}@db/app":            "postgres://app:{{ .Values.pass }}@db/app",
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

// Many URLs in one piece are scanned once each.
func TestRedactURLsLinear(t *testing.T) {
	s := strings.Repeat("http://h/x,", 16000) + "http://u:pw@h/x"
	start := time.Now()
	got, ok := redactURLs(s)
	if !ok || !strings.HasSuffix(got, "http://u:<redacted>@h/x") {
		t.Fatalf("not redacted: %v", ok)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %s", d)
	}
	if _, ok := redactURLs(strings.Repeat("a://", 16000)); ok {
		t.Error("redacted schemes only")
	}
}

func FuzzRedactURLs(f *testing.F) {
	for _, s := range []string{"postgres://u:p@h/db", "http://u:p@es1,http://u:p2@es2", "--db=a://x:y@z w://:p@q", "a://", "http://[::1]:8080?a=b@c"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		once, _ := redactURLs(s)
		if twice, changed := redactURLs(once); changed || twice != once {
			t.Errorf("not idempotent: %q -> %q -> %q", s, once, twice)
		}
	})
}
