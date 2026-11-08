package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// Default versions installed by bootstrap.
const (
	DefaultArgoCDVersion = "v3.5.6"
	DefaultFluxVersion   = "v2.9.6"
)

// DefaultArgoProject is the AppProject gitopsify creates; the environment is
// appended to it.
const DefaultArgoProject = "gitopsify"

// DefaultProtectKinds are objects whose deletion loses data or breaks other
// apps, so the GitOps tool is told never to prune or delete them.
var DefaultProtectKinds = []string{
	"PersistentVolumeClaim",
	"PersistentVolume",
	"Namespace",
	"CustomResourceDefinition.apiextensions.k8s.io",
}

// New returns a configuration with every default filled in.
func New() *Config {
	c := &Config{}
	c.Default()
	return c
}

func boolPtr(b bool) *bool { return &b }

// True reports whether an optional bool is set and true, using def when unset.
func True(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

// Default fills unset fields with their defaults.
func (c *Config) Default() {
	if c.APIVersion == "" {
		c.APIVersion = APIVersion
	}
	if c.Kind == "" {
		c.Kind = Kind
	}
	if c.Metadata.Name == "" {
		c.Metadata.Name = NameFor(c.Source.Context)
	}
	if c.Scope.IncludeDependencies == nil {
		c.Scope.IncludeDependencies = boolPtr(true)
	}
	if c.Scope.ClusterScoped == "" {
		c.Scope.ClusterScoped = "auto"
	}
	if c.Helm.Mode == "" {
		c.Helm.Mode = HelmAuto
	}
	if c.Helm.Resolvers == nil {
		c.Helm.Resolvers = []string{"local"}
	}
	if c.Clean.Strategy == "" {
		c.Clean.Strategy = CleanAuto
	}
	if c.Grouping.By == "" {
		c.Grouping.By = GroupByNamespace
	}
	if c.Grouping.By == GroupByLabel && c.Grouping.Label == "" {
		c.Grouping.Label = "app.kubernetes.io/part-of"
	}
	if c.Ordering.Enabled == nil {
		c.Ordering.Enabled = boolPtr(true)
	}
	if c.Ordering.InferDependencies == nil {
		c.Ordering.InferDependencies = boolPtr(true)
	}
	c.defaultSecrets()
	c.defaultOutput()
	if len(c.Environments) == 0 {
		c.Environments = []Environment{{Name: "default"}}
	}
	for i := range c.Environments {
		if c.Environments[i].Cluster.Server == "" && c.Environments[i].Cluster.Name == "" {
			c.Environments[i].Cluster.Server = "https://kubernetes.default.svc"
		}
	}
	if c.Adopt.ProtectKinds == nil {
		c.Adopt.ProtectKinds = append([]string(nil), DefaultProtectKinds...)
	}
}

func (c *Config) defaultSecrets() {
	s := &c.Secrets
	if s.Strategy == "" {
		s.Strategy = SecretsSkip
	}
	if s.SOPS.EncryptedRegex == "" {
		s.SOPS.EncryptedRegex = "^(data|stringData)$"
	}
	if s.SOPS.DecryptionSecret == "" {
		s.SOPS.DecryptionSecret = "sops-keys"
	}
	if s.SealedSecrets.ControllerNamespace == "" {
		s.SealedSecrets.ControllerNamespace = "kube-system"
	}
	if s.SealedSecrets.ControllerName == "" {
		s.SealedSecrets.ControllerName = "sealed-secrets-controller"
	}
	if s.SealedSecrets.Scope == "" {
		s.SealedSecrets.Scope = "strict"
	}
	e := &s.ExternalSecrets
	if e.APIVersion == "" {
		e.APIVersion = "external-secrets.io/v1"
	}
	if e.StoreKind == "" {
		e.StoreKind = "ClusterSecretStore"
	}
	if e.StoreName == "" {
		e.StoreName = "default"
	}
	if e.KeyTemplate == "" {
		e.KeyTemplate = "{{ .Namespace }}/{{ .Name }}"
	}
	if e.PropertyPerKey == nil {
		e.PropertyPerKey = boolPtr(true)
	}
	if e.RefreshInterval == "" {
		e.RefreshInterval = "1h"
	}
}

func (c *Config) defaultOutput() {
	o := &c.Output
	if o.Dir == "" {
		o.Dir = "gitops"
	}
	if o.Tool == "" {
		o.Tool = ToolNone
	}
	if o.Layout == "" {
		o.Layout = LayoutFolderPerEnv
	}
	if o.Repo.Revision == "" {
		o.Repo.Revision = "main"
	}
	o.Repo.Path = strings.Trim(o.Repo.Path, "/")
	a := &o.ArgoCD
	if a.Pattern == "" {
		a.Pattern = PatternAppOfApps
	}
	if a.Namespace == "" {
		a.Namespace = "argocd"
	}
	if a.Project == "" {
		a.Project = DefaultArgoProject
	}
	if a.CreateProject == nil {
		a.CreateProject = boolPtr(a.Project != "default")
	}
	if a.Version == "" {
		a.Version = DefaultArgoCDVersion
	}
	if a.AutoSync == nil {
		a.AutoSync = boolPtr(true)
	}
	if a.Prune == nil {
		// Pruning during a takeover deletes anything the export missed.
		a.Prune = boolPtr(!c.Adopt.Enabled)
	}
	if a.SelfHeal == nil {
		a.SelfHeal = boolPtr(true)
	}
	if a.ServerSideApply == nil {
		a.ServerSideApply = boolPtr(true)
	}
	f := &o.Flux
	if f.Namespace == "" {
		f.Namespace = "flux-system"
	}
	if f.Interval == "" {
		f.Interval = "10m"
	}
	if f.Version == "" {
		f.Version = DefaultFluxVersion
	}
	if f.Prune == nil {
		// Flux only prunes objects it applied itself, so this is safe for a
		// takeover too.
		f.Prune = boolPtr(true)
	}
	if f.SecretName == "" {
		f.SecretName = "flux-system"
	}
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Validate reports configuration errors.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.APIVersion != APIVersion || c.Kind != Kind {
		add("apiVersion/kind must be %s/%s, got %s/%s", APIVersion, Kind, c.APIVersion, c.Kind)
	}
	if err := c.Metadata.validate(); err != nil {
		errs = append(errs, err)
	}
	oneOf := func(field, v string, allowed ...string) {
		for _, a := range allowed {
			if v == a {
				return
			}
		}
		add("%s must be one of %s, got %q", field, strings.Join(allowed, ", "), v)
	}
	oneOf("scope.clusterScoped", c.Scope.ClusterScoped, "auto", "all", "none")
	oneOf("helm.mode", c.Helm.Mode, HelmAuto, HelmChart, HelmVendor, HelmManifests)
	for _, r := range c.Helm.Resolvers {
		oneOf("helm.resolvers[]", r, "local", "artifacthub")
	}
	for k, r := range c.Helm.Releases {
		if r.Mode != "" {
			oneOf("helm.releases["+k+"].mode", r.Mode, HelmAuto, HelmChart, HelmVendor, HelmManifests)
		}
	}
	oneOf("clean.strategy", c.Clean.Strategy, CleanAuto, CleanManagedFields, CleanHeuristic, CleanMinimal)
	oneOf("grouping.by", c.Grouping.By, GroupByNamespace, GroupByLabel, GroupSingle)
	for i, a := range c.Grouping.Apps {
		if !dnsLabel.MatchString(a.Name) {
			add("grouping.apps[%d].name %q must be a DNS label", i, a.Name)
		}
	}
	strategies := []string{SecretsSkip, SecretsLocal, SecretsPlain, SecretsSOPS, SecretsSealedSecrets, SecretsExternalSecrets}
	oneOf("secrets.strategy", c.Secrets.Strategy, strategies...)
	for i, o := range c.Secrets.Overrides {
		oneOf(fmt.Sprintf("secrets.overrides[%d].strategy", i), o.Strategy, strategies...)
	}
	if c.usesSecretStrategy(SecretsPlain) && !c.Secrets.AllowPlain {
		add("secrets strategy %q writes Secret data unencrypted into Git; set secrets.allowPlain (or --allow-plain-secrets) to confirm", SecretsPlain)
	}
	if c.usesSecretStrategy(SecretsSOPS) && len(c.Secrets.SOPS.Age)+len(c.Secrets.SOPS.PGP)+len(c.Secrets.SOPS.KMS)+len(c.Secrets.SOPS.GCPKMS)+len(c.Secrets.SOPS.AzureKV) == 0 {
		add("secrets strategy %q needs at least one recipient in secrets.sops (age, pgp, kms, gcpKms or azureKv)", SecretsSOPS)
	}
	oneOf("secrets.sealedSecrets.scope", c.Secrets.SealedSecrets.Scope, "strict", "namespace-wide", "cluster-wide")
	oneOf("secrets.externalSecrets.storeKind", c.Secrets.ExternalSecrets.StoreKind, "SecretStore", "ClusterSecretStore")
	for i, t := range c.Transforms {
		if err := t.Validate(); err != nil {
			add("transforms[%d]: %v", i, err)
		}
	}
	oneOf("output.tool", c.Output.Tool, ToolNone, ToolArgoCD, ToolFlux)
	oneOf("output.layout", c.Output.Layout, LayoutFolderPerEnv, LayoutRepoPerEnv)
	oneOf("output.argocd.pattern", c.Output.ArgoCD.Pattern, PatternAppOfApps, PatternApplicationSet, PatternSingle)
	if c.Output.Tool != ToolNone && c.Output.Repo.URL == "" && c.Output.Layout == LayoutFolderPerEnv {
		add("output.repo.url (or --repo-url) is required for --tool %s: the generated %s objects point at it", c.Output.Tool, c.Output.Tool)
	}
	if err := c.Source.Access.Validate(); err != nil {
		add("source: %v", err)
	}
	seen := map[string]bool{}
	for i, e := range c.Environments {
		if !dnsLabel.MatchString(e.Name) {
			add("environments[%d].name %q must be a DNS label", i, e.Name)
		}
		if err := e.Cluster.Access.Validate(); err != nil {
			add("environments[%d] (%s).cluster: %v", i, e.Name, err)
		}
		if seen[e.Name] {
			add("environment %q is defined twice", e.Name)
		}
		seen[e.Name] = true
		if c.Output.Tool != ToolNone && c.Output.Layout == LayoutRepoPerEnv && e.RepoURL == "" && c.Output.Repo.URL == "" {
			add("environments[%d] (%s) needs repoURL with the repo-per-env layout", i, e.Name)
		}
		for j, t := range e.Transforms {
			if err := t.Validate(); err != nil {
				add("environments[%d].transforms[%d]: %v", i, j, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (c *Config) usesSecretStrategy(s string) bool {
	if c.Secrets.Strategy == s {
		return true
	}
	for _, o := range c.Secrets.Overrides {
		if o.Strategy == s {
			return true
		}
	}
	return false
}

// Validate checks that a transform has the fields its type needs.
func (t Transform) Validate() error {
	switch t.Type {
	case "replace":
		if t.From == "" {
			return errors.New("replace needs from")
		}
		if t.Regex {
			if _, err := regexp.Compile(t.From); err != nil {
				return fmt.Errorf("invalid regex %q: %w", t.From, err)
			}
		}
	case "image":
		if t.From == "" || t.To == "" {
			return errors.New("image needs from and to (image prefixes)")
		}
	case "storageClass", "ingressClass", "namespace":
		if len(t.Map) == 0 {
			return fmt.Errorf("%s needs map", t.Type)
		}
	case "set":
		if t.Path == "" {
			return errors.New("set needs path")
		}
	case "remove":
		if t.Path == "" {
			return errors.New("remove needs path")
		}
	case "jsonPatch":
		if len(t.Patch) == 0 {
			return errors.New("jsonPatch needs patch")
		}
	case "labels", "annotations":
		if len(t.Set) == 0 && len(t.Remove) == 0 {
			return fmt.Errorf("%s needs set or remove", t.Type)
		}
	default:
		return fmt.Errorf("unknown transform type %q", t.Type)
	}
	return nil
}

// Load reads a configuration file. Unknown fields are errors so typos don't
// silently fall back to defaults.
func Load(path string) (*Config, error) {
	docs, err := ReadDocuments(path)
	if err != nil {
		return nil, err // names the file
	}
	return loadMigration(path, docs)
}

// ParseMigration reads the migration in data (YAML, or JSON when name ends in
// .json); name appears in errors.
func ParseMigration(name string, data []byte) (*Config, error) {
	docs, err := ParseDocuments(name, data)
	if err != nil {
		return nil, err
	}
	return loadMigration(name, docs)
}

func loadMigration(path string, docs []Document) (*Config, error) {
	if len(docs) == 0 {
		return &Config{}, nil // empty, or only comments: the defaults, as in v0.1
	}
	var migrations []Document
	for _, d := range docs {
		if d.Kind == Kind || d.Kind == "" {
			migrations = append(migrations, d)
		}
	}
	switch {
	case len(migrations) == 0 && len(Find(docs, KindExport, "")) > 0:
		return nil, fmt.Errorf("%s describes a provider export; run gitopsify export --config %s", path, path)
	case len(migrations) == 0:
		return nil, fmt.Errorf("%s holds no %s", path, Kind)
	case len(migrations) > 1:
		return nil, fmt.Errorf("%s holds several migrations; gitopsify reads one per file", path)
	}
	c, err := decodeMigration(migrations[0])
	if err != nil {
		if strings.Contains(err.Error(), `unknown field "token"`) {
			return nil, fmt.Errorf("parsing %s: tokens don't belong in gitopsify.yaml, which generate copies into the repository; use tokenFile, --token or a kubeconfig instead", path)
		}
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return c, nil
}

// migrationDocument is the Migration resource as written in files.
type migrationDocument struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata,omitzero"`
	Spec       Config     `json:"spec"`
}

// decodeMigration accepts both forms of a migration: the resource form
// (apiVersion, kind, metadata, spec) and the layout gitopsify v0.1 wrote,
// with the spec's fields next to apiVersion and kind and no metadata.
//
// Both decode into Config directly: sigs.k8s.io/yaml converts values to the
// fields' types (labelSelector: 4.5 → "4.5") only for fields it finds
// without following embedded structs.
func decodeMigration(doc Document) (*Config, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(doc.JSON, &probe); err != nil {
		return nil, err
	}
	if _, ok := probe["spec"]; ok {
		var top migrationDocument
		if err := decodeStrict(doc.Raw, &top); err != nil {
			var misplaced []string
			for k := range probe {
				if specField(k) {
					misplaced = append(misplaced, k)
				}
			}
			if len(misplaced) > 0 {
				sort.Strings(misplaced)
				verb := "goes"
				if len(misplaced) > 1 {
					verb = "go"
				}
				return nil, fmt.Errorf("%w (%s %s under spec)", err, strings.Join(misplaced, ", "), verb)
			}
			return nil, err
		}
		c := top.Spec
		if c.APIVersion != "" || c.Kind != "" {
			return nil, errors.New("spec: apiVersion and kind go at the top of the resource, not in spec")
		}
		c.APIVersion, c.Kind, c.Metadata = top.APIVersion, top.Kind, top.Metadata
		return &c, nil
	}
	c := &Config{}
	if err := decodeStrict(doc.Raw, c); err != nil {
		if _, ok := probe["metadata"]; ok {
			return nil, fmt.Errorf("%w (with metadata, the migration's fields go under spec)", err)
		}
		return nil, err
	}
	return c, nil
}

// ExpandHome replaces a leading "~/" with the user's home directory, in every
// element of a path list.
func ExpandHome(p string) string {
	if p == "" || !strings.Contains(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	parts := filepath.SplitList(p)
	for i, part := range parts {
		if part == "~" {
			parts[i] = home
		} else if strings.HasPrefix(part, "~/") {
			parts[i] = filepath.Join(home, part[2:])
		}
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

// specField reports whether name is a field of a migration's spec.
func specField(name string) bool {
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		tag, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if tag == name && tag != "apiVersion" && tag != "kind" {
			return true
		}
	}
	return false
}

// Document returns the Migration resource, ready to be encoded (see Encode).
func (c *Config) Document() any {
	spec := *c
	spec.APIVersion, spec.Kind = "", ""
	return migrationDocument{APIVersion: c.APIVersion, Kind: c.Kind, Metadata: c.Metadata, Spec: spec}
}

// Marshal renders the Migration resource as YAML, with the fields in their
// documented order.
func (c *Config) Marshal() ([]byte, error) { return marshalOrdered(c.Document()) }

// NameFor turns a kube context into a migration name: lowercase letters,
// digits and dashes, at most 63; "migration" when nothing is left.
func NameFor(context string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(context) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	name := strings.TrimRight(b.String(), "-")
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	if name == "" {
		return "migration"
	}
	return name
}

// Env returns the environment with the given name.
func (c *Config) Env(name string) (*Environment, error) {
	for i := range c.Environments {
		if c.Environments[i].Name == name {
			return &c.Environments[i], nil
		}
	}
	names := make([]string, 0, len(c.Environments))
	for _, e := range c.Environments {
		names = append(names, e.Name)
	}
	return nil, fmt.Errorf("environment %q is not defined (have: %s)", name, strings.Join(names, ", "))
}
