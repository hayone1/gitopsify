// Package pipeline runs gitopsify's stages in order: discover, Helm, filter,
// dependencies, clean, group, transform, secrets, order and render.
package pipeline

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hayone1/gitopsify/pkg/clean"
	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/deps"
	"github.com/hayone1/gitopsify/pkg/discovery"
	"github.com/hayone1/gitopsify/pkg/filter"
	"github.com/hayone1/gitopsify/pkg/group"
	"github.com/hayone1/gitopsify/pkg/helm"
	"github.com/hayone1/gitopsify/pkg/kube"
	"github.com/hayone1/gitopsify/pkg/match"
	"github.com/hayone1/gitopsify/pkg/model"
	"github.com/hayone1/gitopsify/pkg/order"
	"github.com/hayone1/gitopsify/pkg/portability"
	"github.com/hayone1/gitopsify/pkg/render"
	"github.com/hayone1/gitopsify/pkg/secrets"
	"github.com/hayone1/gitopsify/pkg/target"
	"github.com/hayone1/gitopsify/pkg/transform"
)

// Options configure a run.
type Options struct {
	Config *config.Config
	// Source is the cluster to read; built from Config.Source when nil.
	Source *kube.Client
	// Offline replaces the source cluster with objects read from files.
	Offline []*unstructured.Unstructured
	// OfflineName describes where Offline objects came from.
	OfflineName string
	// EnvClient returns a client for an environment's cluster; built from
	// the environment's cluster settings when nil.
	EnvClient func(env string) (*kube.Client, error)
	// Resolver finds chart repositories; built from Config.Helm when nil.
	Resolver *helm.Resolver
	// Connect tunes connections to the source and environment clusters.
	Connect kube.ConnectOptions
	// Log receives progress messages.
	Log func(format string, args ...any)
	// Version of gitopsify, written into generated files.
	Version string
	// ProtectSecrets keeps Secret values from being learnt from the output
	// (the server sets it unless an admin allows Secret values): Secrets
	// written encrypted are Protected from transforms, and Helm values
	// that look like credentials are replaced with RedactedValue.
	ProtectSecrets bool
}

// RedactedValue replaces Helm values ProtectSecrets removes.
const RedactedValue = "<redacted by gitopsify: set from a Secret>"

// Result is the outcome of a run.
type Result struct {
	Inventory *model.Inventory
	Discovery *discovery.Result
	Secrets   *secrets.Result
	Tree      *render.Tree
}

func (o *Options) log(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// Scan reads the source cluster and decides what is migrated and how it is
// grouped and ordered, without rendering anything.
func Scan(ctx context.Context, opts Options) (*Result, error) {
	cfg := opts.Config
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := filter.ValidateRuleNames(cfg); err != nil {
		return nil, err
	}
	inv, dres, err := discover(ctx, &opts)
	if err != nil {
		return nil, err
	}
	inv.Add(dres.Objects...)
	inv.Warnings = append(inv.Warnings, dres.Warnings...)
	opts.log("found %d objects of %d kinds", len(dres.Objects), len(dres.Resources))

	scope := scopeIndex(dres.Resources)
	if err := helmStage(ctx, inv, cfg, scope, &opts); err != nil {
		return nil, err
	}
	if err := filter.Releases(inv, cfg); err != nil {
		return nil, err
	}
	info := func(o *model.Object) match.KindInfo {
		if r, ok := scope.byGVR[o.GVR.GroupResource().String()]; ok {
			return r.KindInfo()
		}
		return match.InfoOf(o)
	}
	if err := filter.Apply(inv, filter.Options{Config: cfg, KindInfo: info}); err != nil {
		return nil, err
	}
	deps.Resolve(inv, cfg)
	if err := resolveCharts(ctx, inv, cfg, scope, &opts); err != nil {
		return nil, err
	}
	clean.Run(inv, cfg)
	portability.Check(inv, cfg)
	if err := group.Run(inv, cfg); err != nil {
		return nil, err
	}
	ExcludeApps(inv, cfg.Scope.ExcludeApps)
	if opts.ProtectSecrets {
		protectSecrets(inv, cfg)
	}
	if err := applyGlobalTransforms(inv, cfg); err != nil {
		return nil, err
	}
	if err := order.Run(inv, cfg); err != nil {
		return nil, err
	}
	return &Result{Inventory: inv, Discovery: dres}, nil
}

// Generate runs Scan, then Finish.
func Generate(ctx context.Context, opts Options) (*Result, error) {
	res, err := Scan(ctx, opts)
	if err != nil {
		return nil, err
	}
	return res, Finish(ctx, opts, res)
}

// Finish handles Secrets and renders the repository of a scan result.
func Finish(ctx context.Context, opts Options, res *Result) error {
	cfg := opts.Config
	sres, err := secrets.Run(res.Inventory, secrets.Options{Config: cfg, Cert: certSource(ctx, &opts)})
	if err != nil {
		return err
	}
	res.Secrets = sres
	// Secrets may have become SealedSecrets/ExternalSecrets that depend on
	// their controllers; order again.
	if err := order.Run(res.Inventory, cfg); err != nil {
		return err
	}
	if err := markExisting(ctx, &opts, res.Inventory); err != nil {
		return err
	}
	opts.log("rendering %d apps for %s", len(res.Inventory.Apps), cfg.Output.Tool)
	tree, err := render.Render(res.Inventory, render.Options{Config: cfg, Secrets: sres, Version: opts.Version})
	if err != nil {
		return err
	}
	tree.SourceID = res.Inventory.Source.ID
	res.Tree = tree
	return nil
}

func discover(ctx context.Context, opts *Options) (*model.Inventory, *discovery.Result, error) {
	cfg := opts.Config
	if opts.Offline != nil {
		opts.log("reading %d objects from %s", len(opts.Offline), opts.OfflineName)
		inv := &model.Inventory{Source: model.ClusterInfo{Context: "offline", Server: opts.OfflineName, Offline: true}}
		return inv, discovery.FromObjects(opts.Offline), nil
	}
	src := opts.Source
	if src == nil {
		var err error
		if src, err = kube.Connect(cfg.Source.Access, opts.Connect); err != nil {
			return nil, nil, fmt.Errorf("source cluster: %w", err)
		}
	}
	version, err := src.Ping()
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to the source cluster %s: %w", src.Server, err)
	}
	inv := &model.Inventory{Source: model.ClusterInfo{Context: src.Context, Server: src.Server, Version: version}}
	// The cluster's identity, whatever address reaches it: kube-system's UID.
	if src.Kubernetes != nil {
		if ns, err := src.Kubernetes.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{}); err == nil {
			inv.Source.ID = string(ns.UID)
		}
	}
	dopts, err := discoveryOptions(cfg)
	if err != nil {
		return nil, nil, err
	}
	how := ""
	if src.Auth != "" {
		how = ", " + src.Auth
	}
	opts.log("discovering objects in %s (%s%s)", src.Context, src.Server, how)
	quarter := 0
	dopts.Progress = func(done, total int) {
		// Large clusters take a while; say how far along the listing is.
		if total >= 40 && done*4/total > quarter && done < total {
			quarter = done * 4 / total
			opts.log("listed %d of %d resource types", done, total)
		}
	}
	dres, err := discovery.Discover(ctx, src, dopts)
	return inv, dres, err
}

func discoveryOptions(cfg *config.Config) (discovery.Options, error) {
	// Cluster-scoped objects are always listed: even when they are not
	// migrated, they resolve dependencies (CRDs, Namespaces, ClusterRoles).
	o := discovery.Options{ClusterScoped: true}
	var namespaces []string
	exact := true
	for _, n := range cfg.Scope.Namespaces {
		if strings.ContainsAny(n, "*?[") {
			exact = false
		}
		namespaces = append(namespaces, n)
	}
	if len(cfg.Scope.Resources) > 0 && cfg.Scope.LabelSelector == "" && len(cfg.Scope.Namespaces) == 0 {
		set := map[string]bool{}
		for _, r := range cfg.Scope.Resources {
			s, err := filter.ParseResourceSpec(r)
			if err != nil {
				return o, err
			}
			if s.Namespace == "" {
				// A namespaced kind without a namespace could be anywhere.
				return o, nil
			}
			set[s.Namespace] = true
		}
		for n := range set {
			namespaces = append(namespaces, n)
		}
		sort.Strings(namespaces)
	}
	if exact {
		o.Namespaces = namespaces
	}
	return o, nil
}

type scopeInfo struct {
	byGVR      map[string]discovery.Resource
	namespaced map[string]bool
}

func scopeIndex(resources []discovery.Resource) *scopeInfo {
	s := &scopeInfo{byGVR: map[string]discovery.Resource{}, namespaced: map[string]bool{}}
	for _, r := range resources {
		s.byGVR[r.GVR.GroupResource().String()] = r
		s.namespaced[r.GVR.Group+"/"+r.Kind] = r.Namespaced
	}
	return s
}

func (s *scopeInfo) isNamespaced(group, kind string) bool {
	v, ok := s.namespaced[group+"/"+kind]
	if !ok {
		return true
	}
	return v
}

// helmStage decodes Helm releases and marks the objects they own.
func helmStage(ctx context.Context, inv *model.Inventory, cfg *config.Config, scope *scopeInfo, opts *Options) error {
	var storage []*unstructured.Unstructured
	for _, o := range inv.Objects {
		if helm.IsStorage(o.Live) {
			storage = append(storage, o.Live)
		}
	}
	releases, warnings := helm.Latest(storage)
	inv.Releases = releases
	inv.Warnings = append(inv.Warnings, warnings...)
	if len(releases) > 0 {
		opts.log("found %d Helm releases", len(releases))
	}
	for _, rel := range releases {
		if ov, ok := cfg.Helm.Releases[rel.Key()]; ok {
			if ov.Chart != "" {
				rel.Chart.Name = ov.Chart
			}
			if ov.Version != "" {
				rel.Chart.Version = ov.Version
			}
		}
		owned := map[model.Ref]bool{}
		for _, ref := range helm.ManifestRefs(rel.Manifest, rel.Namespace, scope.isNamespaced) {
			owned[ref] = true
		}
		crds := map[string]bool{}
		for _, n := range helm.CRDNames(rel) {
			crds[n] = true
		}
		for _, o := range inv.Objects {
			// Controllers copy annotations to what they create (Deployments to
			// ReplicaSets), so Helm's annotations don't make those Helm's.
			if o.HelmRelease != "" || controlled(o.Live) {
				continue
			}
			switch {
			case helm.OwnerOf(o.Live) == rel.Key(), owned[o.Ref]:
				o.HelmRelease = rel.Key()
				rel.Owned = append(rel.Owned, o.Ref)
			case o.IsCRD() && crds[o.Ref.Name]:
				o.HelmRelease = rel.Key()
				rel.Owned = append(rel.Owned, o.Ref)
			}
		}
	}
	return nil
}

func controlled(u *unstructured.Unstructured) bool {
	for _, r := range u.GetOwnerReferences() {
		if r.Controller != nil && *r.Controller {
			return true
		}
	}
	return false
}

var credentialKey = regexp.MustCompile(`(?i)(password|passwd|secret|token|apikey|api_key|credential|privatekey|private_key)`)

// resolveCharts decides how each migrated release is represented and finds
// chart repositories.
func resolveCharts(ctx context.Context, inv *model.Inventory, cfg *config.Config, scope *scopeInfo, opts *Options) error {
	resolver := opts.Resolver
	if resolver == nil {
		resolver = &helm.Resolver{Repositories: cfg.Helm.Repositories, Order: cfg.Helm.Resolvers}
	}
	for _, rel := range inv.Releases {
		if rel.Skip {
			continue
		}
		ov := cfg.Helm.Releases[rel.Key()]
		mode := cfg.Helm.Mode
		if ov.Mode != "" {
			mode = ov.Mode
		}
		if ov.Repo != "" {
			rel.Repo = &model.ChartRepo{URL: strings.TrimSuffix(ov.Repo, "/"), Source: "config"}
		}
		if rel.Repo == nil && (mode == config.HelmAuto || mode == config.HelmChart) {
			repo, warnings, err := resolver.Resolve(ctx, rel.Chart.Name, rel.Chart.Version)
			if err != nil {
				return err
			}
			rel.Warnings = append(rel.Warnings, warnings...)
			rel.Repo = repo
		}
		switch mode {
		case config.HelmChart:
			if rel.Repo != nil {
				rel.Mode = model.HelmModeChart
				break
			}
			rel.Warnings = append(rel.Warnings, fmt.Sprintf("repository of chart %s %s not found (set helm.repositories.%s)", rel.Chart.Name, rel.Chart.Version, rel.Chart.Name))
			fallthrough
		case config.HelmAuto:
			switch {
			case rel.Repo != nil:
				rel.Mode = model.HelmModeChart
			case rel.Vendorable():
				rel.Mode = model.HelmModeVendor
				rel.Warnings = append(rel.Warnings, "chart repository unknown; the chart is vendored from the release. Set helm.repositories."+rel.Chart.Name+" to reference the upstream chart instead")
			default:
				rel.Mode = model.HelmModeManifests
				rel.Warnings = append(rel.Warnings, "chart repository unknown and the chart has subcharts, so it can't be rebuilt; its objects are migrated as plain manifests")
			}
		case config.HelmVendor:
			if rel.Vendorable() {
				rel.Mode = model.HelmModeVendor
			} else {
				rel.Mode = model.HelmModeManifests
				rel.Warnings = append(rel.Warnings, "the chart has subcharts, which Helm does not store in releases, so it can't be vendored; its objects are migrated as plain manifests")
			}
		case config.HelmManifests:
			rel.Mode = model.HelmModeManifests
		}

		if rel.Mode == model.HelmModeManifests {
			if len(rel.Hooks) > 0 {
				rel.Warnings = append(rel.Warnings, fmt.Sprintf("the release has %d hooks, which are not migrated in manifests mode", len(rel.Hooks)))
			}
			continue
		}
		live := func(r model.Ref) *unstructured.Unstructured {
			if o := inv.Get(r); o != nil {
				return o.Live
			}
			return nil
		}
		drift := helm.Drift(rel, live, scope.isNamespaced)
		refs := make([]model.Ref, 0, len(drift))
		for r := range drift {
			refs = append(refs, r)
		}
		sort.Slice(refs, func(i, j int) bool { return refs[i].String() < refs[j].String() })
		for _, r := range refs {
			rel.Warnings = append(rel.Warnings, fmt.Sprintf("%s differs from the chart's manifest at %s; the GitOps tool will reset it to the chart's value", r, strings.Join(drift[r], ", ")))
		}
		if opts.ProtectSecrets {
			if ps := redactCredentials(rel.Values, ""); len(ps) > 0 {
				rel.Warnings = append(rel.Warnings, fmt.Sprintf("values %s look like credentials and were redacted; set them from a Secret (Flux valuesFrom, an encrypted values file) before syncing", strings.Join(ps, ", ")))
			}
			if rel.Mode == model.HelmModeVendor {
				// The vendored chart's own values.yaml is written too.
				if ps := redactCredentials(rel.ChartValues, ""); len(ps) > 0 {
					rel.Warnings = append(rel.Warnings, fmt.Sprintf("the vendored chart's values %s look like credentials and were redacted", strings.Join(ps, ", ")))
				}
			}
		}
		for _, p := range credentialPaths(rel.Values, "", false) {
			rel.Warnings = append(rel.Warnings, fmt.Sprintf("values key %s looks like a credential and will be committed to Git; move it to a Secret (valuesFrom) or encrypt it", p))
		}
	}
	return nil
}

// credentialName reports whether a values key names a credential (and not
// a reference to one, such as existingSecret or passwordKey, or image pull
// secrets).
func credentialName(k, parent string) bool {
	l := strings.ToLower(k)
	return (credentialKey.MatchString(k) || redactKey.MatchString(k)) && !strings.Contains(l, "secretname") &&
		!strings.HasSuffix(l, "ref") && !strings.Contains(l, "existing") && !strings.HasSuffix(l, "passwordkey") &&
		!strings.Contains(l, "pullsecret") && !secretNameKey(l) && !keySetting.MatchString(k) &&
		!strings.Contains(strings.ToLower(parent), "secretkeys") && !strings.EqualFold(parent, "keys") &&
		!strings.Contains(strings.ToLower(parent), "expiry")
}

// leafSetting is keySetting under a credential container, where URLs and
// files (SLACK_WEBHOOK_URL, keyfile) are values the user put there.
var leafSetting = regexp.MustCompile(`(?i)(path|issuer|ttl|expiry|expiration|selector|algorithm|rotationpolicy|duration|size|length|timeout|interval|enabled|type|mode|format|policy|provider|backend|audience)s?$`)

// privateKeyPEM reports PEM-encoded private keys, wherever they are.
func privateKeyPEM(s string) bool {
	return strings.Contains(s, "-----BEGIN") && strings.Contains(s, "PRIVATE KEY-----")
}

// keySetting matches keys about a credential rather than holding one
// (token_url, bearer_token_file, tokenIssuer, secretFieldSelector).
var keySetting = regexp.MustCompile(`(?i)(url|uri|file|path|issuer|ttl|expiry|expiration|selector|algorithm|rotationpolicy|duration|size|length|timeout|interval|enabled|type|mode|format|policy|provider|backend|endpoint|audience)s?$`)

// redactKey adds credentials known by other names (Airflow's fernetKey and
// connection pass, MongoDB's replicaSetKey, LDAP bind passwords, Grafana's
// secureJsonData, Argo CD's repository keys, alerting integration keys).
var redactKey = regexp.MustCompile(`(?i)(passphrase|fernet|bindpw|replicasetkey|securejsondata|tlsclientcertkey|tlsclientkey|serviceaccountkey|routing_key|service_key|user_key|integrationkey|(^|[_.-])pass$|dsn$|[_.-]key$)`)

// secretNameKey reports keys whose string is the name of a Secret.
func secretNameKey(l string) bool {
	for _, suffix := range []string{"fromsecret", "certificatessecret", "certificatesecret", "tlssecret", "certsecret", "secretnames",
		"privatekeysecret", "passwordsecretkey", "configsecretkey", "varssecret", "scriptssecret", "accountsecret",
		"accountsecretkey", "admintokensecret"} {
		if strings.HasSuffix(l, suffix) {
			return true
		}
	}
	return false
}

// container reports keys whose maps and lists hold credentials: their last
// word is one (secrets, extraSecrets, credentials; not passwordUpdateJob).
func container(k string) bool {
	l := strings.ToLower(k)
	if l == "securejsondata" || strings.HasSuffix(l, "securejsondata") {
		return true
	}
	switch strings.TrimSuffix(lastWord(k), "s") {
	case "secret", "credential", "password", "token", "key", "apikey":
		return !strings.Contains(l, "pullsecret")
	}
	return false
}

// lastWord returns the last word of a key, lowercased: camelCase,
// snake_case, kebab-case and dotted.
func lastWord(k string) string {
	i := len(k)
	for i > 0 {
		c := k[i-1]
		if c == '_' || c == '-' || c == '.' {
			break
		}
		if c >= 'A' && c <= 'Z' && i < len(k) && k[i] >= 'a' && k[i] <= 'z' {
			i--
			break
		}
		i--
	}
	return strings.ToLower(strings.TrimLeft(k[i:], "_-."))
}

// redactCredentials replaces values that look like credentials with
// RedactedValue and returns their paths: strings under credential keys,
// and the strings of maps under keys whose last word is a credential
// (secrets, credentials, secureJsonData) but names, references and
// settings; numbers under keys ending like a credential (adminPin,
// apiKey; not passwordMinLength or tokenTTL); the value of name/value
// pairs (env lists) whose name holds a credential but isn't a setting
// (DB_PASSWORD, FERNET_KEY; not TOKEN_TTL or TOKEN_URL); and the passwords
// of URLs anywhere in strings.
func redactCredentials(v any, prefix string) []string {
	var out []string
	join := func(k string) string {
		if prefix == "" {
			return k
		}
		return prefix + "." + k
	}
	switch t := v.(type) {
	case map[string]any:
		if referenceShape(t) {
			return nil
		}
		if n, ok := t["name"].(string); ok && envCredential(n) && redactable(t["value"]) {
			t["value"] = RedactedValue
			out = append(out, join("value"))
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parent := prefix[strings.LastIndex(prefix, ".")+1:]
		_, tlsPair := t["cert"]
		for _, c := range []string{"crt", "certificate", "ca"} {
			_, ok := t[c]
			tlsPair = tlsPair || ok
		}
		for _, k := range keys {
			if skipSubtree(k) {
				continue
			}
			switch x := t[k].(type) {
			case string:
				// An inline TLS private key: {cert, key} (not a file path).
				inlineKey := k == "key" && tlsPair && !strings.HasPrefix(x, "/")
				if (credentialName(k, parent) || inlineKey || privateKeyPEM(x)) && redactable(x) {
					t[k] = RedactedValue
					out = append(out, join(k))
				} else if r, ok := redactURLs(x); ok {
					t[k] = r
					out = append(out, join(k))
				}
			case map[string]any:
				if container(k) && credentialName(k, parent) {
					out = append(out, redactLeaves(x, join(k))...)
				}
				out = append(out, redactCredentials(x, join(k))...)
			case []any:
				if container(k) && credentialName(k, parent) {
					out = append(out, redactLeaves(x, join(k))...) // maps in it; strings are names
				}
				out = append(out, redactCredentials(x, join(k))...)
			default:
				if numberCredential(k) && redactable(x) {
					t[k] = RedactedValue
					out = append(out, join(k))
				}
			}
		}
	case []any:
		for i, c := range t {
			if s, ok := c.(string); ok {
				if privateKeyPEM(s) {
					t[i] = RedactedValue
					out = append(out, fmt.Sprintf("%s[%d]", prefix, i))
				} else if r, ok := redactURLs(s); ok {
					t[i] = r
					out = append(out, fmt.Sprintf("%s[%d]", prefix, i))
				}
				continue
			}
			out = append(out, redactCredentials(c, fmt.Sprintf("%s[%d]", prefix, i))...)
		}
	}
	return out
}

// redactLeaves redacts the strings in a map under a credential container,
// but names, references and settings. Lists of strings are left alone:
// under such keys they are names (Secrets to mount).
func redactLeaves(v any, prefix string) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		if referenceShape(t) {
			return nil
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if skipSubtree(k) || reference(k) || settingKeys[strings.ToLower(k)] || secretNameKey(strings.ToLower(k)) || leafSetting.MatchString(k) ||
				strings.EqualFold(k, "keys") || strings.Contains(strings.ToLower(k), "secretkeys") { // key names in a Secret
				continue
			}
			if s, ok := t[k].(string); ok {
				if redactable(s) {
					t[k] = RedactedValue
					out = append(out, prefix+"."+k)
				}
				continue
			}
			out = append(out, redactLeaves(t[k], prefix+"."+k)...)
		}
	case []any:
		for i, c := range t {
			if _, ok := c.(string); !ok {
				out = append(out, redactLeaves(c, fmt.Sprintf("%s[%d]", prefix, i))...)
			}
		}
	}
	return out
}

// referenceShape reports maps that point at a Secret ({name, key},
// GitLab's {secret, key}) rather than hold a value.
func referenceShape(m map[string]any) bool {
	if len(m) == 0 {
		return false
	}
	if _, ok := m["secretName"]; ok {
		// A Secret volume, a TLS block or an env entry names one; a map
		// with other keys (external-dns' {apiToken, secretName: ""}) holds
		// values next to it.
		for k := range m {
			switch k {
			case "secretName", "secretKey", "key", "envName", "items", "defaultMode", "optional", "hosts", "name", "mountPath", "subPath", "readOnly":
			default:
				return false
			}
		}
		return true
	}
	if _, key := m["key"]; key {
		if _, path := m["path"]; path { // a volume's items: {key, path, mode}
			for k := range m {
				if k != "key" && k != "path" && k != "mode" {
					return false
				}
			}
			return true
		}
	}
	for k, v := range m {
		switch k {
		case "name", "secret", "key", "optional", "secretName", "enabled":
		default:
			return false
		}
		if _, ok := v.(map[string]any); ok {
			return false
		}
	}
	_, named := m["name"]
	_, secret := m["secret"]
	_, key := m["key"]
	_, secretName := m["secretName"]
	return (named || secret || secretName) && (key || len(m) == 1 || secret || secretName)
}

// skipSubtree reports keys whose values are never credentials: metadata,
// references to Secrets, image pull secrets.
func skipSubtree(k string) bool {
	l := strings.ToLower(k)
	return strings.HasSuffix(l, "annotations") || strings.HasSuffix(l, "labels") || l == "valuefrom" ||
		strings.HasSuffix(l, "ref") || strings.Contains(l, "pullsecret") || l == "envfrom"
}

// reference reports keys whose strings name or point at something.
func reference(k string) bool {
	l := strings.ToLower(k)
	return strings.HasSuffix(l, "name") || strings.HasPrefix(l, "existing") || strings.HasSuffix(l, "passwordkey") ||
		strings.HasSuffix(l, "tokenkey")
}

// settingKeys are settings of a Secret, not values, under credential keys.
var settingKeys = map[string]bool{"create": true, "enabled": true, "mountpath": true, "subpath": true, "path": true, "type": true,
	"mode": true, "defaultmode": true, "driver": true, "prefix": true, "provider": true, "format": true, "encoding": true,
	"refreshinterval": true, "namespace": true, "kind": true, "apiversion": true, "scope": true, "immutable": true,
	"resourcespreset": true, "image": true, "resources": true, "env": true, "algorithm": true, "rotationpolicy": true}

// envCredential reports env variable names that hold a credential:
// DB_PASSWORD, SECRET_KEY_BASE, FERNET_KEY, SENTRY_DSN; not settings about
// one (TOKEN_TTL, PASSWORD_FILE, API_KEY_ID, TOKEN_URL).
func envCredential(n string) bool {
	return (credentialKey.MatchString(n) || envCredentialEnd.MatchString(n)) && !envSetting.MatchString(n)
}

var envCredentialEnd = regexp.MustCompile(`(?i)(_key|passphrase|dsn|_pin|_pass)$`)

var envSetting = regexp.MustCompile(`(?i)(_ttl|_file|_path|_length|_timeout|_enabled|_name|_key_id|_expiry|_expiration|_seconds|_minutes|_hours|_days|_size|_count|_header|_type|_mode|_url|_uri|_endpoint|_issuer|_audience|_provider|_backend)$`)

// numberCredential reports keys whose numbers are credentials: they end
// in a credential word (adminPin, DBPassword, apiKey, TOKEN; not spin,
// passwordMinLength, tokenTTL or maxToken).
func numberCredential(k string) bool {
	l := strings.ToLower(k)
	if strings.HasPrefix(l, "max") || strings.HasPrefix(l, "min") {
		return false
	}
	for _, w := range []string{"password", "passwd", "passphrase", "passcode", "pin", "token", "secret", "apikey", "api_key", "secretkey", "secret_key", "accesskey", "access_key"} {
		if !strings.HasSuffix(l, w) {
			continue
		}
		i := len(k) - len(w)
		if i == 0 || strings.ContainsRune("_.-", rune(k[i-1])) {
			return true
		}
		upper := func(c byte) bool { return c >= 'A' && c <= 'Z' }
		if upper(k[i]) && (!upper(k[i-1]) || (i+1 < len(k) && !upper(k[i+1]))) {
			return true
		}
	}
	return false
}

// redactURLs redacts the passwords of the URLs in s (in arguments, lines,
// lists such as http://u:p@es1,http://u:p@es2), in one pass.
func redactURLs(s string) (string, bool) {
	if !strings.Contains(s, "://") {
		return s, false
	}
	sep := func(c byte) bool { return strings.IndexByte(" \t\r\n\"'", c) >= 0 }
	var b strings.Builder
	changed := false
	for i := 0; i < len(s); {
		j := i
		for j < len(s) && !sep(s[j]) {
			j++
		}
		piece := s[i:j]
		if r, ok := redactPiece(piece); ok {
			piece, changed = r, true
		}
		b.WriteString(piece)
		k := j
		for k < len(s) && sep(s[k]) {
			k++
		}
		b.WriteString(s[j:k])
		i = k
	}
	return b.String(), changed
}

// redactPiece redacts the URLs in a piece of text without whitespace: each
// runs from its scheme to the next URL's scheme.
func redactPiece(p string) (string, bool) {
	var starts []int
	for from := 0; ; {
		i := strings.Index(p[from:], "://")
		if i < 0 {
			break
		}
		i += from
		st := i
		for st > from && (isAlnum(p[st-1]) || strings.IndexByte("+.-", p[st-1]) >= 0) {
			st--
		}
		if st < i {
			starts = append(starts, st)
		}
		from = i + 3
	}
	if len(starts) == 0 {
		return p, false
	}
	var b strings.Builder
	b.WriteString(p[:starts[0]])
	changed := false
	for n, st := range starts {
		end := len(p)
		if n+1 < len(starts) {
			end = starts[n+1]
		}
		seg := p[st:end]
		if r, ok := redactURL(seg); ok {
			seg, changed = r, true
		}
		b.WriteString(seg)
	}
	return b.String(), changed
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// redactURL replaces the password of a URL with <redacted>, failing
// closed: the password runs from the first colon of the authority (not a
// port) to the last @ before the first / that follows an @ (a password may
// hold @, /, ? or #). References ($(DB_PASSWORD), ${DB_PASSWORD}, Helm
// templates) are left alone.
func redactURL(s string) (string, bool) {
	i := strings.Index(s, "://")
	if i <= 0 || !urlScheme.MatchString(s[:i]) {
		return s, false
	}
	rest := s[i+3:]
	c := strings.Index(rest, ":")
	if c < 0 || strings.ContainsAny(rest[:c], "/?#[") {
		return s, false
	}
	if port := rest[c+1:]; len(port) > 0 {
		j := 0
		for j < len(port) && port[j] >= '0' && port[j] <= '9' {
			j++
		}
		if j > 0 && (j == len(port) || strings.ContainsRune("/?#", rune(port[j]))) {
			return s, false // host:port
		}
	}
	first := strings.Index(rest[c+1:], "@")
	if first < 0 {
		return s, false
	}
	first += c + 1
	limit := len(rest)
	if slash := strings.Index(rest[first:], "/"); slash >= 0 {
		limit = first + slash
	}
	at := strings.LastIndex(rest[:limit], "@")
	pw := rest[c+1 : at]
	if pw == "" || pw == "<redacted>" || varRef.MatchString(pw) || strings.Contains(pw, "{{") {
		return s, false
	}
	return s[:i+3] + rest[:c+1] + "<redacted>" + rest[at:], true
}

var (
	urlScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)
	varRef    = regexp.MustCompile(`^(\$\([A-Za-z_][A-Za-z0-9_]*\)|\$\{[A-Za-z_][A-Za-z0-9_]*\})$`)
)

// redactable reports whether a value is a credential's value: a non-empty
// string or a number.
func redactable(v any) bool {
	switch x := v.(type) {
	case string:
		switch strings.ToLower(x) {
		case "", "true", "false", "on", "off", "yes", "no", "enabled", "disabled":
			return false
		}
		return x != RedactedValue && !strings.Contains(x, "{{")
	case int, int64, float64, json.Number:
		return true
	}
	return false
}

// credentialPaths lists the values that look like credentials, replacing
// them with RedactedValue when redact is set.
func credentialPaths(v any, prefix string, redact bool) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			if s, ok := t[k].(string); ok && s != "" && s != RedactedValue && credentialKey.MatchString(k) && !strings.Contains(strings.ToLower(k), "secretname") && !strings.HasSuffix(strings.ToLower(k), "ref") && !strings.HasPrefix(strings.ToLower(k), "existing") {
				out = append(out, p)
				if redact {
					t[k] = RedactedValue
				}
				continue
			}
			out = append(out, credentialPaths(t[k], p, redact)...)
		}
	case []any:
		for i, c := range t {
			out = append(out, credentialPaths(c, fmt.Sprintf("%s[%d]", prefix, i), redact)...)
		}
	}
	return out
}

// ExcludeApps drops the apps matching the patterns, with their objects and
// releases.
func ExcludeApps(inv *model.Inventory, patterns []string) {
	if len(patterns) == 0 {
		return
	}
	kept := inv.Apps[:0]
	for _, a := range inv.Apps {
		if !match.Glob(patterns, a.Name) {
			kept = append(kept, a)
			continue
		}
		for _, o := range a.Objects {
			o.Exclude("excluded-app", "app "+a.Name+" excluded by scope.excludeApps")
		}
		if a.Release != nil {
			a.Release.Skip, a.Release.SkipReason = true, "app excluded by scope.excludeApps"
		}
	}
	inv.Apps = kept
}

// protectSecrets marks the Secrets gitopsify writes with their values
// (encrypted) Protected; those a chart renders are never written.
func protectSecrets(inv *model.Inventory, cfg *config.Config) {
	for _, o := range inv.Objects {
		if !o.IsSecret() {
			continue
		}
		if o.HelmRelease != "" {
			if r := inv.Release(o.HelmRelease); r != nil && r.Mode != model.HelmModeManifests {
				continue
			}
		}
		switch secrets.StrategyFor(cfg, o.Ref.Namespace, o.Ref.Name) {
		case config.SecretsSealedSecrets, config.SecretsSOPS, config.SecretsPlain, config.SecretsLocal:
			o.Protected = true
		}
	}
}

// chartTransformWarnings tells which transforms don't reach objects that
// Helm charts render (chart and vendor modes): only chart values change.
func chartTransformWarnings(inv *model.Inventory, cfg *config.Config) {
	check := func(where string, ts []config.Transform, env bool) {
		for i, t := range ts {
			objectOnly := false
			switch t.Type {
			case "labels", "annotations", "jsonPatch", "remove":
				objectOnly = true
			case "set":
				objectOnly = !strings.HasPrefix(t.Path, "values.")
			case "namespace":
				objectOnly = env // global ones rename releases too
			}
			if !objectOnly {
				continue
			}
			var apps []string
			for _, a := range inv.Apps {
				if a.Release == nil || a.Release.Mode == model.HelmModeManifests {
					continue
				}
				for _, o := range a.Objects {
					if ok, _ := match.SelectorOrAll(t.Target, o, o.App); ok {
						apps = append(apps, a.Name)
						break
					}
				}
			}
			if len(apps) > 0 {
				inv.Warn("%stransform %d (%s) doesn't change the objects of Helm apps %s, which their charts render: set their values instead (values.<path>), or use helm.mode: manifests",
					where, i, t.Type, strings.Join(apps, ", "))
			}
		}
	}
	check("", cfg.Transforms, false)
	for _, e := range cfg.Environments {
		check("environment "+e.Name+": ", e.Transforms, true)
	}
}

// applyGlobalTransforms applies the transforms that hold for every
// environment to the objects, and namespace renames to Helm releases.
func applyGlobalTransforms(inv *model.Inventory, cfg *config.Config) error {
	chartTransformWarnings(inv, cfg)
	for _, o := range inv.Objects {
		if o.Excluded || o.Out == nil {
			continue
		}
		if _, err := transform.Object(o.Out, o, cfg.Transforms); err != nil {
			return err
		}
	}
	for _, t := range cfg.Transforms {
		if t.Type != "namespace" {
			continue
		}
		for _, a := range inv.Apps {
			if t.Target != nil && len(t.Target.Apps) > 0 && !match.Glob(t.Target.Apps, a.Name) {
				continue
			}
			if to, ok := t.Map[a.Namespace]; ok {
				a.Namespace = to
			}
			if a.Release != nil {
				if to, ok := t.Map[a.Release.Namespace]; ok {
					// Objects find their release by key (namespace/name).
					old := a.Release.Key()
					a.Release.Namespace = to
					for _, o := range inv.Objects {
						if o.HelmRelease == old {
							o.HelmRelease = a.Release.Key()
						}
					}
				}
			}
		}
	}
	return nil
}

// certSource returns the sealed-secrets certificate of each environment.
func certSource(ctx context.Context, opts *Options) func(env string) (*rsa.PublicKey, error) {
	cfg := opts.Config
	var mu sync.Mutex
	cache := map[string]*rsa.PublicKey{}
	return func(env string) (*rsa.PublicKey, error) {
		mu.Lock()
		defer mu.Unlock()
		if k, ok := cache[env]; ok {
			return k, nil
		}
		var key *rsa.PublicKey
		if f := cfg.Secrets.SealedSecrets.CertFile; f != "" {
			data, err := os.ReadFile(config.ExpandHome(f))
			if err != nil {
				return nil, err
			}
			if key, err = secrets.ParseCert(data); err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
		} else {
			c, err := envClient(opts, env)
			if err != nil {
				return nil, fmt.Errorf("sealing secrets for environment %s: %w (or set secrets.sealedSecrets.certFile)", env, err)
			}
			s := cfg.Secrets.SealedSecrets
			if key, err = secrets.FetchCert(ctx, c.Kubernetes, s.ControllerNamespace, s.ControllerName); err != nil {
				return nil, fmt.Errorf("environment %s (%s): %w", env, c.Context, c.Explain(err))
			}
		}
		cache[env] = key
		return key, nil
	}
}

// markExisting leaves out of each environment the objects its cluster
// already has, as adopt.existing says.
func markExisting(ctx context.Context, opts *Options, inv *model.Inventory) error {
	cfg := opts.Config
	if cfg.Adopt.Existing == "" || cfg.Adopt.Existing == config.ExistingInclude {
		return nil
	}
	for i := range cfg.Environments {
		env := &cfg.Environments[i]
		c, err := envClient(opts, env.Name)
		if err != nil {
			return fmt.Errorf("adopt.existing: %s needs to read each environment's cluster: %w", cfg.Adopt.Existing, err)
		}
		opts.log("looking for objects that already exist in %s", env.Name)
		notes, err := target.MarkExisting(ctx, inv, cfg, env, c)
		if err != nil {
			return err
		}
		inv.Warnings = append(inv.Warnings, notes...)
	}
	return nil
}

// ErrNoEnvCluster is returned when an environment has no cluster to connect to.
var ErrNoEnvCluster = errors.New("the environment has no cluster to connect to (cluster.context, cluster.kubeconfig, cluster.apiServer or cluster.inCluster)")

func envClient(opts *Options, env string) (*kube.Client, error) {
	if opts.EnvClient != nil {
		return opts.EnvClient(env)
	}
	return EnvClient(opts.Config, env, opts.Connect)
}

// EnvClient connects to an environment's cluster.
func EnvClient(cfg *config.Config, env string, o kube.ConnectOptions) (*kube.Client, error) {
	e, err := cfg.Env(env)
	if err != nil {
		return nil, err
	}
	if !e.Cluster.Access.Configured() {
		return nil, fmt.Errorf("environment %s: %w", env, ErrNoEnvCluster)
	}
	c, err := kube.Connect(e.Cluster.Access, o)
	if err != nil {
		return nil, fmt.Errorf("environment %s: %w", env, err)
	}
	return c, nil
}
