// Package secrets decides how each migrated Secret reaches the target
// without putting credentials into Git in plain text.
package secrets

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path"
	"sort"
	"strings"
	"text/template"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/match"
	"github.com/hayone1/gitopsify/pkg/model"
)

// LocalDir is the git-ignored directory, relative to the output root, that
// holds plain copies of Secrets for the local and external-secrets strategies.
const LocalDir = ".secrets"

// Options configure Run.
type Options struct {
	Config *config.Config
	// Cert returns the sealed-secrets public key of an environment.
	Cert func(env string) (*rsa.PublicKey, error)
	// Rand is the randomness source for sealing; crypto/rand by default.
	Rand io.Reader
}

// Result holds files that are written outside the GitOps tree.
type Result struct {
	// LocalFiles are relative to the output root and git-ignored.
	LocalFiles map[string][]byte
	// Strategies counts Secrets per strategy.
	Strategies map[string]int
}

// StrategyFor returns the strategy that applies to a Secret.
func StrategyFor(cfg *config.Config, ns, name string) string {
	for _, o := range cfg.Secrets.Overrides {
		if match.GlobOrEmpty(o.Namespaces, ns) && match.GlobOrEmpty(o.Names, name) {
			return o.Strategy
		}
	}
	return cfg.Secrets.Strategy
}

// Run applies the secret strategies to every included Secret.
func Run(inv *model.Inventory, opts Options) (*Result, error) {
	cfg := opts.Config
	rnd := opts.Rand
	if rnd == nil {
		rnd = rand.Reader
	}
	res := &Result{LocalFiles: map[string][]byte{}, Strategies: map[string]int{}}
	sopsChecked := false
	for _, o := range inv.Objects {
		if o.Excluded || !o.IsSecret() || o.Out == nil {
			continue
		}
		if o.HelmRelease != "" {
			if r := inv.Release(o.HelmRelease); r != nil && r.Mode != model.HelmModeManifests {
				continue // rendered by the chart
			}
		}
		secret := o.Out
		ns, name := secret.GetNamespace(), secret.GetName()
		strategy := StrategyFor(cfg, o.Ref.Namespace, o.Ref.Name)
		res.Strategies[strategy]++
		keys := secretKeys(secret)
		typ, _, _ := unstructured.NestedString(secret.Object, "type")
		if typ == "" {
			typ = "Opaque"
		}
		desc := fmt.Sprintf("%s/%s (%s, keys: %s)", ns, name, typ, strings.Join(keys, ", "))

		switch strategy {
		case config.SecretsSkip:
			o.Out = nil
			inv.SecretActions = append(inv.SecretActions, desc+": not migrated; create it in the target cluster")
		case config.SecretsLocal:
			b, err := yaml.Marshal(secret.Object)
			if err != nil {
				return nil, err
			}
			res.LocalFiles[path.Join(LocalDir, ns, name+".yaml")] = b
			o.Out = nil
			inv.SecretActions = append(inv.SecretActions, fmt.Sprintf("%s: written to %s/%s/%s.yaml (git-ignored); apply it to the target cluster", desc, LocalDir, ns, name))
		case config.SecretsPlain:
			o.Warn("Secret data is written unencrypted (secrets strategy %q)", strategy)
		case config.SecretsSOPS:
			if !sopsChecked {
				if _, err := exec.LookPath("sops"); err != nil {
					return nil, fmt.Errorf("secrets strategy %q needs the sops binary in PATH: %w", strategy, err)
				}
				sopsChecked = true
			}
			o.Encrypt = true
		case config.SecretsSealedSecrets:
			if opts.Cert == nil {
				return nil, fmt.Errorf("secrets strategy %q needs the sealed-secrets certificate (secrets.sealedSecrets.certFile or a reachable environment cluster)", strategy)
			}
			envs := cfg.Environments
			sealedFor := map[string]*unstructured.Unstructured{}
			for _, e := range envs {
				pub, err := opts.Cert(e.Name)
				if err != nil {
					return nil, err
				}
				sealed, err := Seal(rnd, pub, secret, cfg.Secrets.SealedSecrets.Scope)
				if err != nil {
					return nil, err
				}
				sealedFor[e.Name] = sealed
			}
			if len(envs) == 1 {
				o.Out = sealedFor[envs[0].Name]
			} else {
				// Each cluster has its own sealing key.
				o.Out = nil
				o.EnvOut = sealedFor
			}
		case config.SecretsExternalSecrets:
			es, remote, err := externalSecret(cfg, secret)
			if err != nil {
				return nil, err
			}
			o.Out = es
			b, err := json.MarshalIndent(remote, "", "  ")
			if err != nil {
				return nil, err
			}
			file := path.Join(LocalDir, "external-secrets", ns, name+".json")
			res.LocalFiles[file] = append(b, '\n')
			inv.SecretActions = append(inv.SecretActions, fmt.Sprintf("%s: load the values in %s (git-ignored) into %s %q under key %q",
				desc, file, cfg.Secrets.ExternalSecrets.StoreKind, cfg.Secrets.ExternalSecrets.StoreName, remoteKey(cfg, ns, name, "")))
		}
	}
	return res, nil
}

func secretKeys(u *unstructured.Unstructured) []string {
	var keys []string
	for _, f := range []string{"data", "stringData"} {
		m, _, _ := unstructured.NestedMap(u.Object, f)
		for k := range m {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func remoteKey(cfg *config.Config, ns, name, key string) string {
	t, err := template.New("key").Parse(cfg.Secrets.ExternalSecrets.KeyTemplate)
	if err != nil {
		return ns + "/" + name
	}
	var b bytes.Buffer
	if err := t.Execute(&b, map[string]string{"Namespace": ns, "Name": name, "Key": key}); err != nil {
		return ns + "/" + name
	}
	return b.String()
}

// externalSecret builds the ExternalSecret that recreates a Secret from the
// configured store, and the decoded values to load into that store.
func externalSecret(cfg *config.Config, secret *unstructured.Unstructured) (*unstructured.Unstructured, map[string]string, error) {
	eso := cfg.Secrets.ExternalSecrets
	ns, name := secret.GetNamespace(), secret.GetName()
	values := map[string]string{}
	data, _, _ := unstructured.NestedStringMap(secret.Object, "data")
	for k, v := range data {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, nil, fmt.Errorf("secret %s/%s key %s: %w", ns, name, k, err)
		}
		values[k] = string(b)
	}
	sd, _, _ := unstructured.NestedStringMap(secret.Object, "stringData")
	for k, v := range sd {
		values[k] = v
	}

	tmpl := map[string]any{}
	if t, _, _ := unstructured.NestedString(secret.Object, "type"); t != "" && t != "Opaque" {
		tmpl["type"] = t
	}
	tmplMeta := map[string]any{}
	if l := secret.GetLabels(); len(l) > 0 {
		tmplMeta["labels"] = toAny(l)
	}
	if a := secret.GetAnnotations(); len(a) > 0 {
		tmplMeta["annotations"] = toAny(a)
	}
	if len(tmplMeta) > 0 {
		tmpl["metadata"] = tmplMeta
	}
	target := map[string]any{"name": name, "creationPolicy": "Owner"}
	if len(tmpl) > 0 {
		tmpl["engineVersion"] = "v2"
		target["template"] = tmpl
	}
	spec := map[string]any{
		"refreshInterval": eso.RefreshInterval,
		"secretStoreRef":  map[string]any{"kind": eso.StoreKind, "name": eso.StoreName},
		"target":          target,
	}
	if config.True(eso.PropertyPerKey, true) {
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var items []any
		for _, k := range keys {
			items = append(items, map[string]any{
				"secretKey": k,
				"remoteRef": map[string]any{"key": remoteKey(cfg, ns, name, k), "property": k},
			})
		}
		spec["data"] = items
	} else {
		spec["dataFrom"] = []any{map[string]any{"extract": map[string]any{"key": remoteKey(cfg, ns, name, "")}}}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": eso.APIVersion,
		"kind":       "ExternalSecret",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec":       spec,
	}}, values, nil
}

// SOPSArgs returns the sops command-line arguments that encrypt YAML from
// stdin for the configured recipients.
func SOPSArgs(s config.SOPS) []string {
	args := []string{"--encrypt", "--input-type", "yaml", "--output-type", "yaml", "--encrypted-regex", s.EncryptedRegex}
	if len(s.Age) > 0 {
		args = append(args, "--age", strings.Join(s.Age, ","))
	}
	if len(s.PGP) > 0 {
		args = append(args, "--pgp", strings.Join(s.PGP, ","))
	}
	if len(s.KMS) > 0 {
		args = append(args, "--kms", strings.Join(s.KMS, ","))
	}
	if len(s.GCPKMS) > 0 {
		args = append(args, "--gcp-kms", strings.Join(s.GCPKMS, ","))
	}
	if len(s.AzureKV) > 0 {
		args = append(args, "--azure-kv", strings.Join(s.AzureKV, ","))
	}
	return append(args, "/dev/stdin")
}

// Encrypt encrypts a YAML document with sops.
func Encrypt(s config.SOPS, plain []byte) ([]byte, error) {
	cmd := exec.Command("sops", SOPSArgs(s)...)
	cmd.Stdin = bytes.NewReader(plain)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("sops: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// SOPSConfig renders a .sops.yaml so people can edit the encrypted Secrets
// with plain `sops <file>`.
func SOPSConfig(s config.SOPS) ([]byte, error) {
	rule := map[string]any{
		"path_regex":      `.*secret-.*\.yaml$`,
		"encrypted_regex": s.EncryptedRegex,
	}
	if len(s.Age) > 0 {
		rule["age"] = strings.Join(s.Age, ",")
	}
	if len(s.PGP) > 0 {
		rule["pgp"] = strings.Join(s.PGP, ",")
	}
	if len(s.KMS) > 0 {
		rule["kms"] = strings.Join(s.KMS, ",")
	}
	if len(s.GCPKMS) > 0 {
		rule["gcp_kms"] = strings.Join(s.GCPKMS, ",")
	}
	if len(s.AzureKV) > 0 {
		rule["azure_keyvault"] = strings.Join(s.AzureKV, ",")
	}
	return yaml.Marshal(map[string]any{"creation_rules": []any{rule}})
}
