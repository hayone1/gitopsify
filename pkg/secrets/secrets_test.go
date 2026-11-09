package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/model"
)

// unseal is the controller side of hybridEncrypt.
func unseal(t *testing.T, key *rsa.PrivateKey, ciphertext, label []byte) string {
	t.Helper()
	n := int(binary.BigEndian.Uint16(ciphertext))
	sessionKey, err := rsa.DecryptOAEP(sha256.New(), nil, key, ciphertext[2:2+n], label)
	if err != nil {
		t.Fatalf("RSA-OAEP: %v", err)
	}
	block, _ := aes.NewCipher(sessionKey)
	aead, _ := cipher.NewGCM(block)
	plain, err := aead.Open(nil, make([]byte, aead.NonceSize()), ciphertext[2+n:], nil)
	if err != nil {
		t.Fatalf("AES-GCM: %v", err)
	}
	return string(plain)
}

func secret() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/basic-auth",
		"metadata":   map[string]any{"name": "db", "namespace": "shop", "labels": map[string]any{"app": "db"}},
		"data":       map[string]any{"password": base64.StdEncoding.EncodeToString([]byte("s3cr3t"))},
		"stringData": map[string]any{"username": "admin"},
	}}
}

func TestSealRoundTrip(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for scope, label := range map[string]string{"strict": "shop/db", "namespace-wide": "shop", "cluster-wide": ""} {
		sealed, err := Seal(rand.Reader, &key.PublicKey, secret(), scope)
		if err != nil {
			t.Fatal(err)
		}
		enc, _, _ := unstructured.NestedStringMap(sealed.Object, "spec", "encryptedData")
		for k, want := range map[string]string{"password": "s3cr3t", "username": "admin"} {
			ct, _ := base64.StdEncoding.DecodeString(enc[k])
			if got := unseal(t, key, ct, []byte(label)); got != want {
				t.Errorf("%s %s = %q, want %q", scope, k, got, want)
			}
		}
		if typ, _, _ := unstructured.NestedString(sealed.Object, "spec", "template", "type"); typ != "kubernetes.io/basic-auth" {
			t.Errorf("template type = %q", typ)
		}
		if scope != "strict" && sealed.GetAnnotations()["sealedsecrets.bitnami.com/"+scope] != "true" {
			t.Errorf("%s annotation missing: %v", scope, sealed.GetAnnotations())
		}
	}
}

func TestParseCert(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "sealed-secret"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pub, err := ParseCert(append([]byte("junk\n"), pemData...))
	if err != nil || pub.N.Cmp(key.PublicKey.N) != 0 {
		t.Fatalf("ParseCert: %v", err)
	}
}

func inventory() *model.Inventory {
	inv := &model.Inventory{}
	u := secret()
	inv.Add(&model.Object{Ref: model.RefOf(u), Namespaced: true, Live: u, Out: u.DeepCopy()})
	return inv
}

func TestStrategies(t *testing.T) {
	cfg := config.New()
	cfg.Secrets.Strategy = config.SecretsSkip
	inv := inventory()
	if _, err := Run(inv, Options{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	if inv.Objects[0].Out != nil || len(inv.SecretActions) != 1 || !strings.Contains(inv.SecretActions[0], "keys: password, username") {
		t.Errorf("skip: out=%v actions=%v", inv.Objects[0].Out, inv.SecretActions)
	}

	cfg.Secrets.Strategy = config.SecretsLocal
	inv = inventory()
	res, err := Run(inv, Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.LocalFiles[".secrets/shop/db.yaml"]; !ok || inv.Objects[0].Out != nil {
		t.Errorf("local: files=%v", res.LocalFiles)
	}

	cfg.Secrets.Strategy = config.SecretsExternalSecrets
	cfg.Secrets.ExternalSecrets.KeyTemplate = "apps/{{ .Namespace }}/{{ .Name }}"
	inv = inventory()
	res, err = Run(inv, Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	es := inv.Objects[0].Out
	if es.GetKind() != "ExternalSecret" {
		t.Fatalf("kind = %s", es.GetKind())
	}
	data, _, _ := unstructured.NestedSlice(es.Object, "spec", "data")
	first := data[0].(map[string]any)
	if first["secretKey"] != "password" || first["remoteRef"].(map[string]any)["key"] != "apps/shop/db" {
		t.Errorf("data = %v", data)
	}
	if !strings.Contains(string(res.LocalFiles[".secrets/external-secrets/shop/db.json"]), `"password": "s3cr3t"`) {
		t.Errorf("values file = %s", res.LocalFiles[".secrets/external-secrets/shop/db.json"])
	}

	// Overrides pick a strategy per Secret.
	cfg.Secrets.Overrides = []config.SecretOverride{{Namespaces: []string{"shop"}, Names: []string{"d*"}, Strategy: config.SecretsSkip}}
	if s := StrategyFor(cfg, "shop", "db"); s != config.SecretsSkip {
		t.Errorf("override strategy = %s", s)
	}
	if s := StrategyFor(cfg, "other", "db"); s != config.SecretsExternalSecrets {
		t.Errorf("default strategy = %s", s)
	}
}

func TestSealedPerEnvironment(t *testing.T) {
	keys := map[string]*rsa.PrivateKey{}
	for _, e := range []string{"uat", "prod"} {
		keys[e], _ = rsa.GenerateKey(rand.Reader, 2048)
	}
	cfg := config.New()
	cfg.Secrets.Strategy = config.SecretsSealedSecrets
	cfg.Environments = []config.Environment{{Name: "uat"}, {Name: "prod"}}
	inv := inventory()
	_, err := Run(inv, Options{Config: cfg, Cert: func(env string) (*rsa.PublicKey, error) { return &keys[env].PublicKey, nil }})
	if err != nil {
		t.Fatal(err)
	}
	o := inv.Objects[0]
	if o.Out != nil || len(o.EnvOut) != 2 {
		t.Fatalf("sealed secrets must be per environment: out=%v envOut=%d", o.Out, len(o.EnvOut))
	}
	for env, key := range keys {
		enc, _, _ := unstructured.NestedStringMap(o.EnvOut[env].Object, "spec", "encryptedData")
		ct, _ := base64.StdEncoding.DecodeString(enc["password"])
		if got := unseal(t, key, ct, []byte("shop/db")); got != "s3cr3t" {
			t.Errorf("%s: %q", env, got)
		}
	}

	// An environment's transforms apply before sealing: prod's Secret has
	// prod's value, sealed for prod's namespace.
	cfg.Environments[1].Transforms = []config.Transform{
		{Type: "replace", From: "s3cr3t", To: "pr0d"},
		{Type: "namespace", Map: map[string]string{"shop": "shop-prod"}},
	}
	inv = inventory()
	if _, err := Run(inv, Options{Config: cfg, Cert: func(env string) (*rsa.PublicKey, error) { return &keys[env].PublicKey, nil }}); err != nil {
		t.Fatal(err)
	}
	prod := inv.Objects[0].EnvOut["prod"]
	enc, _, _ := unstructured.NestedStringMap(prod.Object, "spec", "encryptedData")
	ct, _ := base64.StdEncoding.DecodeString(enc["password"])
	if got := unseal(t, keys["prod"], ct, []byte("shop-prod/db")); got != "pr0d" || prod.GetNamespace() != "shop-prod" {
		t.Errorf("prod: %q in %s", got, prod.GetNamespace())
	}
}

func TestSOPSArgs(t *testing.T) {
	args := strings.Join(SOPSArgs(config.SOPS{Age: []string{"age1a", "age1b"}, EncryptedRegex: "^(data|stringData)$"}), " ")
	if !strings.Contains(args, "--age age1a,age1b") || !strings.Contains(args, "--encrypted-regex ^(data|stringData)$") {
		t.Errorf("args = %s", args)
	}
}

// Binary values (keystores) reach the store base64-encoded, and the
// ExternalSecret decodes them; a Secret without keys stays as it is.
func TestExternalSecretsBinaryAndEmpty(t *testing.T) {
	cfg := config.New()
	cfg.Secrets.Strategy = config.SecretsExternalSecrets
	inv := inventory()
	ks := []byte{0xfe, 0xed, 0xfe, 0xed, 0x00, 0x02, 0xff}
	_ = unstructured.SetNestedField(inv.Objects[0].Out.Object, base64.StdEncoding.EncodeToString(ks), "data", "keystore.jks")
	empty := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "empty", "namespace": "shop"}}}
	inv.Add(&model.Object{Ref: model.RefOf(empty), Namespaced: true, Live: empty, Out: empty.DeepCopy()})
	res, err := Run(inv, Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	data, _, _ := unstructured.NestedSlice(inv.Objects[0].Out.Object, "spec", "data")
	strategy := map[string]any{}
	for _, d := range data {
		m := d.(map[string]any)
		strategy[m["secretKey"].(string)] = m["remoteRef"].(map[string]any)["decodingStrategy"]
	}
	if strategy["keystore.jks"] != "Base64" || strategy["password"] != nil {
		t.Errorf("decoding strategies: %v", strategy)
	}
	var values map[string]string
	if err := json.Unmarshal(res.LocalFiles[".secrets/external-secrets/shop/db.json"], &values); err != nil {
		t.Fatal(err)
	}
	if b, _ := base64.StdEncoding.DecodeString(values["keystore.jks"]); !bytes.Equal(b, ks) || values["password"] != "s3cr3t" {
		t.Errorf("values: %v", values)
	}
	for _, o := range inv.Objects {
		if o.Ref.Name == "empty" && (o.Out == nil || o.Out.GetKind() != "Secret") {
			t.Errorf("the empty Secret became %v", o.Out)
		}
	}
}

// TestFingerprints: a sealed Secret's fingerprint is stable while the
// source Secret, the settings and the key don't change, changes with each
// of them, never holds the value, and is empty for Secrets read from files.
func TestFingerprints(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	cfg := config.New()
	cfg.Secrets.Strategy = config.SecretsSealedSecrets
	cfg.Environments = []config.Environment{{Name: "uat"}}
	run := func(mutate func(inv *model.Inventory), pub *rsa.PublicKey) string {
		t.Helper()
		inv := inventory()
		inv.Objects[0].Live.SetUID("uid-1")
		inv.Objects[0].Live.SetResourceVersion("41")
		if mutate != nil {
			mutate(inv)
		}
		if _, err := Run(inv, Options{Config: cfg, Cert: func(string) (*rsa.PublicKey, error) { return pub, nil }}); err != nil {
			t.Fatal(err)
		}
		return inv.Objects[0].Fingerprints["uat"]
	}
	base := run(nil, &key.PublicKey)
	if base == "" || run(nil, &key.PublicKey) != base {
		t.Fatalf("not stable: %q", base)
	}
	for name, fp := range map[string]string{
		"new resourceVersion": run(func(inv *model.Inventory) { inv.Objects[0].Live.SetResourceVersion("42") }, &key.PublicKey),
		"another key":         run(nil, &other.PublicKey),
		"a label":             run(func(inv *model.Inventory) { inv.Objects[0].Out.SetLabels(map[string]string{"x": "y"}) }, &key.PublicKey),
	} {
		if fp == base || fp == "" {
			t.Errorf("%s: fingerprint %q", name, fp)
		}
	}
	cfg.Environments[0].Transforms = []config.Transform{{Type: "replace", From: "s3cr3t", To: "other"}}
	if run(nil, &key.PublicKey) == base {
		t.Error("a transform didn't change the fingerprint")
	}
	cfg.Environments[0].Transforms = nil
	if fp := run(func(inv *model.Inventory) { inv.Source.Offline = true }, &key.PublicKey); fp != "" {
		t.Errorf("read from files: %q", fp)
	}
	// The value doesn't go in: changing it without a new resourceVersion
	// (impossible in a cluster) keeps the fingerprint.
	if fp := run(func(inv *model.Inventory) {
		_ = unstructured.SetNestedField(inv.Objects[0].Out.Object, base64.StdEncoding.EncodeToString([]byte("changed")), "data", "password")
	}, &key.PublicKey); fp != base {
		t.Error("the value is part of the fingerprint")
	}
}
