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
	opts.log("rendering %d apps for %s", len(res.Inventory.Apps), cfg.Output.Tool)
	tree, err := render.Render(res.Inventory, render.Options{Config: cfg, Secrets: sres, Version: opts.Version})
	if err != nil {
		return err
	}
	res.Tree = tree
	return nil
}

func discover(ctx context.Context, opts *Options) (*model.Inventory, *discovery.Result, error) {
	cfg := opts.Config
	if opts.Offline != nil {
		opts.log("reading %d objects from %s", len(opts.Offline), opts.OfflineName)
		inv := &model.Inventory{Source: model.ClusterInfo{Context: "offline", Server: opts.OfflineName}}
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
	return credentialKey.MatchString(k) && !strings.Contains(l, "secretname") && !strings.HasSuffix(l, "ref") &&
		!strings.HasPrefix(l, "existing") && !strings.HasSuffix(l, "passwordkey") && !strings.Contains(l, "pullsecret") &&
		!strings.Contains(strings.ToLower(parent), "secretkeys")
}

// redactCredentials replaces values that look like credentials with
// RedactedValue and returns their paths: strings under credential keys,
// and the strings of maps and lists under them but names, references and
// settings (velero's credentials.secretContents, Airflow's
// extraSecrets.*.data, Argo CD's configs.secret.extra); numbers under keys
// whose last word is a password or PIN; the value of name/value pairs
// (env lists) whose name holds a credential word but isn't a setting
// (DB_PASSWORD, not TOKEN_TTL); and the password of URLs anywhere.
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
		for _, k := range keys {
			if skipSubtree(k) {
				continue
			}
			switch x := t[k].(type) {
			case string:
				if credentialName(k, parent) && redactable(x) {
					t[k] = RedactedValue
					out = append(out, join(k))
				} else if r, ok := redactURL(x); ok {
					t[k] = r
					out = append(out, join(k))
				}
			case map[string]any, []any:
				if credentialName(k, parent) {
					out = append(out, redactLeaves(x, join(k))...)
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
				if r, ok := redactURL(s); ok {
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

// redactLeaves redacts the strings in a map or list under a credential
// key, but names, references and settings.
func redactLeaves(v any, prefix string) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if skipSubtree(k) || reference(k) || settingKeys[strings.ToLower(k)] {
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
			if s, ok := c.(string); ok && redactable(s) {
				t[i] = RedactedValue
				out = append(out, fmt.Sprintf("%s[%d]", prefix, i))
				continue
			}
			out = append(out, redactLeaves(c, fmt.Sprintf("%s[%d]", prefix, i))...)
		}
	}
	return out
}

// skipSubtree reports keys whose values are never credentials: metadata,
// references to Secrets, image pull secrets.
func skipSubtree(k string) bool {
	l := strings.ToLower(k)
	return l == "annotations" || l == "labels" || l == "valuefrom" || strings.HasSuffix(l, "ref") || strings.Contains(l, "pullsecret")
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
	"refreshinterval": true, "namespace": true, "kind": true, "apiversion": true, "scope": true, "immutable": true}

// envCredential reports env variable names that hold a credential:
// DB_PASSWORD, SECRET_KEY_BASE, GCP_CREDENTIALS_JSON; not settings about
// one (TOKEN_TTL, PASSWORD_FILE, API_KEY_ID).
func envCredential(n string) bool {
	return credentialKey.MatchString(n) && !envSetting.MatchString(n)
}

var envSetting = regexp.MustCompile(`(?i)(_ttl|_file|_path|_length|_timeout|_enabled|_name|_key_id|_expiry|_expiration|_seconds|_minutes|_hours|_days|_size|_count|_header|_type|_mode)$`)

// numberCredential reports keys whose numbers are credentials: their last
// word is a password or a PIN (adminPin, PASSWORD; not spin or
// passwordMinLength).
func numberCredential(k string) bool {
	l := strings.ToLower(k)
	for _, w := range []string{"password", "passwd", "passphrase", "passcode", "pin"} {
		if !strings.HasSuffix(l, w) {
			continue
		}
		i := len(k) - len(w)
		if i == 0 || strings.ContainsRune("_.-", rune(k[i-1])) || (k[i] >= 'A' && k[i] <= 'Z' && k[i-1] >= 'a' && k[i-1] <= 'z') {
			return true
		}
	}
	return false
}

// redactURL replaces the password of a URL with <redacted>: the userinfo
// is what comes before the last @ of the authority (passwords may hold @
// and :), the password what follows its first colon. References to
// variables ($(DB_PASSWORD), ${DB_PASSWORD}) are left alone.
func redactURL(s string) (string, bool) {
	i := strings.Index(s, "://")
	if i <= 0 || !urlScheme.MatchString(s[:i]) {
		return s, false
	}
	rest := s[i+3:]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	at := strings.LastIndex(rest[:end], "@")
	if at < 0 {
		return s, false
	}
	userinfo := rest[:at]
	c := strings.Index(userinfo, ":")
	if c < 0 {
		return s, false
	}
	pw := userinfo[c+1:]
	if pw == "" || pw == "<redacted>" || ((strings.HasPrefix(pw, "$(") && strings.HasSuffix(pw, ")")) || (strings.HasPrefix(pw, "${") && strings.HasSuffix(pw, "}"))) {
		return s, false
	}
	return s[:i+3] + userinfo[:c+1] + "<redacted>" + rest[at:], true
}

var urlScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)

// redactable reports whether a value is a credential's value: a non-empty
// string or a number.
func redactable(v any) bool {
	switch x := v.(type) {
	case string:
		return x != "" && x != RedactedValue
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

// applyGlobalTransforms applies the transforms that hold for every
// environment to the objects, and namespace renames to Helm releases.
func applyGlobalTransforms(inv *model.Inventory, cfg *config.Config) error {
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
			if to, ok := t.Map[a.Namespace]; ok {
				a.Namespace = to
			}
			if a.Release != nil {
				if to, ok := t.Map[a.Release.Namespace]; ok {
					a.Release.Namespace = to
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
			data, err := os.ReadFile(f)
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
