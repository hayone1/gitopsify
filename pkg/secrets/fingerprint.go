package secrets

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hayone1/gitopsify/pkg/model"
)

// fingerprintScheme changes when what goes into a Secret's values changes,
// so every Secret is encrypted afresh once.
const fingerprintScheme = "gitopsify-secret-v1"

// Fingerprint describes what an encrypted Secret encrypts without its
// values: the source Secret's UID and resourceVersion (which change with
// every write of its data), the Secret with its values blanked, and the
// settings that shape the values and the encryption. Two runs with the same
// fingerprint encrypt the same plaintext, so the earlier ciphertext can
// stay and Git shows no change. It holds nothing that tells about the
// values. Empty, so a fresh ciphertext is made, when the Secret wasn't read
// from a cluster: files can change without a new resourceVersion.
func Fingerprint(inv *model.Inventory, o *model.Object, plain *unstructured.Unstructured, settings ...any) string {
	if inv.Source.Offline || o.Live == nil || o.Live.GetUID() == "" || o.Live.GetResourceVersion() == "" || plain == nil {
		return ""
	}
	blank := plain.DeepCopy()
	for _, field := range []string{"data", "stringData"} {
		if m, ok := blank.Object[field].(map[string]any); ok {
			for k := range m {
				m[k] = ""
			}
		}
	}
	// The UID names the cluster's object; the server's address would differ
	// between in-cluster runs and a kubeconfig. Protected Secrets keep their
	// values through transforms, others don't.
	b, err := json.Marshal([]any{fingerprintScheme, string(o.Live.GetUID()), o.Live.GetResourceVersion(), o.Protected, blank.Object, settings})
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// keyID identifies a sealing key without the key itself.
func keyID(pub *rsa.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(der)
	return hex.EncodeToString(h[:])
}

// Shape fingerprints an encrypted object as written, without its encrypted
// values (spec.encryptedData of a SealedSecret): its annotations, labels and
// template, which can change while the Secret doesn't.
func Shape(u *unstructured.Unstructured) string {
	c := u.DeepCopy()
	if m, ok, _ := unstructured.NestedMap(c.Object, "spec", "encryptedData"); ok {
		for k := range m {
			m[k] = ""
		}
		_ = unstructured.SetNestedMap(c.Object, m, "spec", "encryptedData")
	}
	b, err := json.Marshal(c.Object)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
