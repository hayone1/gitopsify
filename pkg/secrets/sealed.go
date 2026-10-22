package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
)

// ParseCert parses the PEM certificate published by the sealed-secrets
// controller.
func ParseCert(data []byte) (*rsa.PublicKey, error) {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil, errors.New("no certificate found in PEM data")
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		pub, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("certificate key is %T, not RSA", cert.PublicKey)
		}
		return pub, nil
	}
}

// FetchCert reads the controller's certificate through the API server's
// service proxy, as kubeseal does.
func FetchCert(ctx context.Context, kc kubernetes.Interface, namespace, name string) (*rsa.PublicKey, error) {
	data, err := kc.CoreV1().Services(namespace).ProxyGet("http", name, "", "/v1/cert.pem", nil).DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching the sealed-secrets certificate from service %s/%s through the API server: %w (is the controller installed there? Some clusters, such as private GKE ones, can't proxy to services: save it with `kubeseal --fetch-cert > cert.pem` and pass --sealed-secrets-cert cert.pem)", namespace, name, err)
	}
	return ParseCert(data)
}

// hybridEncrypt encrypts plaintext the way the sealed-secrets controller
// expects: a random AES-256-GCM session key, itself encrypted with RSA-OAEP
// (SHA-256) using label, prefixed with its 2-byte length.
func hybridEncrypt(rnd io.Reader, pub *rsa.PublicKey, plaintext, label []byte) ([]byte, error) {
	sessionKey := make([]byte, 32)
	if _, err := io.ReadFull(rnd, sessionKey); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(sessionKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	rsaCiphertext, err := rsa.EncryptOAEP(sha256.New(), rnd, pub, sessionKey, label)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 2, 2+len(rsaCiphertext)+len(plaintext)+aead.Overhead())
	binary.BigEndian.PutUint16(out, uint16(len(rsaCiphertext)))
	out = append(out, rsaCiphertext...)
	// The session key is used once, so a zero nonce is safe (and is what the
	// controller expects).
	return aead.Seal(out, make([]byte, aead.NonceSize()), plaintext, nil), nil
}

// Seal converts a Secret into a SealedSecret for the given scope: strict
// (bound to namespace and name), namespace-wide or cluster-wide.
func Seal(rnd io.Reader, pub *rsa.PublicKey, secret *unstructured.Unstructured, scope string) (*unstructured.Unstructured, error) {
	ns, name := secret.GetNamespace(), secret.GetName()
	var label []byte
	annotations := map[string]any{}
	switch scope {
	case "strict", "":
		label = []byte(ns + "/" + name)
	case "namespace-wide":
		label = []byte(ns)
		annotations["sealedsecrets.bitnami.com/namespace-wide"] = "true"
	case "cluster-wide":
		annotations["sealedsecrets.bitnami.com/cluster-wide"] = "true"
	default:
		return nil, fmt.Errorf("unknown sealed-secrets scope %q", scope)
	}

	values := map[string][]byte{}
	data, _, _ := unstructured.NestedStringMap(secret.Object, "data")
	for k, v := range data {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("secret %s/%s key %s: %w", ns, name, k, err)
		}
		values[k] = b
	}
	strData, _, _ := unstructured.NestedStringMap(secret.Object, "stringData")
	for k, v := range strData {
		values[k] = []byte(v)
	}
	encrypted := map[string]any{}
	for k, v := range values {
		ct, err := hybridEncrypt(rnd, pub, v, label)
		if err != nil {
			return nil, err
		}
		encrypted[k] = base64.StdEncoding.EncodeToString(ct)
	}

	tmplMeta := map[string]any{"name": name, "namespace": ns}
	if l := secret.GetLabels(); len(l) > 0 {
		tmplMeta["labels"] = toAny(l)
	}
	if a := secret.GetAnnotations(); len(a) > 0 {
		tmplMeta["annotations"] = toAny(a)
	}
	template := map[string]any{"metadata": tmplMeta}
	if t, _, _ := unstructured.NestedString(secret.Object, "type"); t != "" {
		template["type"] = t
	}
	if imm, found, _ := unstructured.NestedBool(secret.Object, "immutable"); found {
		template["immutable"] = imm
	}

	meta := map[string]any{"name": name, "namespace": ns}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "bitnami.com/v1alpha1",
		"kind":       "SealedSecret",
		"metadata":   meta,
		"spec": map[string]any{
			"encryptedData": encrypted,
			"template":      template,
		},
	}}, nil
}

func toAny(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
