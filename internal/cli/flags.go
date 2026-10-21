package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/discovery"
	"github.com/hayone1/gitopsify/pkg/pipeline"
)

// DefaultConfigFile is read when --config is not given and it exists.
const DefaultConfigFile = "gitopsify.yaml"

// options holds every flag; each command registers the groups it uses.
type options struct {
	configFile string
	kubeconfig string
	context    string
	noInput    bool
	noColor    bool
	quiet      bool

	fromFiles []string

	// scope
	namespaces, excludeNamespaces []string
	selector                      string
	resources                     []string
	noDeps                        bool
	kinds, excludeKinds           []string
	clusterScoped                 string
	disableRules                  []string
	includeManaged                bool
	excludeApps                   []string

	// helm
	helmMode    string
	helmRepos   []string
	artifactHub bool

	// clean, group, order
	cleanStrategy string
	groupBy       string
	groupLabel    string
	noOrder       bool
	waves         []string
	dependsOn     []string

	// secrets
	secrets      string
	allowPlain   bool
	sopsAge      []string
	sealedCert   string
	esoStore     string
	esoStoreKind string

	// transforms
	replace, imageMap, storageClassMap, ingressClassMap, namespaceMap []string

	// output
	output      string
	tool        string
	pattern     string
	layout      string
	repoURL     string
	revision    string
	repoPath    string
	envs        []string
	adopt       bool
	force       bool
	argoVersion string
	fluxVersion string
	project     string
	noAutoSync  bool
	interactive bool
}

func (o *options) addGlobal(fs *pflag.FlagSet) {
	fs.StringVarP(&o.configFile, "config", "c", "", "configuration file (default ./gitopsify.yaml when it exists)")
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "kubeconfig of the source cluster")
	fs.StringVar(&o.context, "context", "", "kubeconfig context of the source cluster (default: current context)")
	fs.BoolVar(&o.noInput, "no-input", false, "never prompt; fail instead where a prompt would be needed")
	fs.BoolVar(&o.noColor, "no-color", false, "disable colors (also honors NO_COLOR)")
	fs.BoolVarP(&o.quiet, "quiet", "q", false, "only print results and errors")
}

func (o *options) addScope(fs *pflag.FlagSet) {
	fs.StringArrayVar(&o.fromFiles, "from-file", nil, "read the source objects from YAML/JSON files or directories instead of a cluster, e.g. `kubectl get -A -o yaml` output (\"-\" for stdin; repeatable)")
	fs.StringSliceVarP(&o.namespaces, "namespace", "n", nil, "migrate only these namespaces (globs, repeatable)")
	fs.StringSliceVar(&o.excludeNamespaces, "exclude-namespace", nil, "skip these namespaces (globs, repeatable)")
	fs.StringVarP(&o.selector, "selector", "l", "", "migrate objects matching this label selector, plus what they need")
	fs.StringArrayVarP(&o.resources, "resource", "r", nil, "migrate specific objects, plus what they need: kind[.group]/[namespace/]name or helm/namespace/release (repeatable)")
	fs.BoolVar(&o.noDeps, "no-deps", false, "with --resource/--selector, don't pull in dependencies")
	fs.StringSliceVar(&o.kinds, "kind", nil, "migrate only these kinds (Deployment, ingress.networking.k8s.io, svc...)")
	fs.StringSliceVar(&o.excludeKinds, "exclude-kind", nil, "never migrate these kinds")
	fs.StringVar(&o.clusterScoped, "cluster-scoped", "", "cluster-scoped objects: auto, all or none")
	fs.StringSliceVar(&o.disableRules, "disable-rule", nil, "disable a built-in exclusion rule (see `gitopsify rules`; \"*\" for all)")
	fs.BoolVar(&o.includeManaged, "include-managed", false, "also migrate objects already managed by Argo CD or Flux")
	fs.StringSliceVar(&o.excludeApps, "exclude-app", nil, "drop these apps (globs) after grouping")

	fs.StringVar(&o.helmMode, "helm-mode", "", "Helm releases: auto, chart, vendor or manifests")
	fs.StringArrayVar(&o.helmRepos, "helm-repo", nil, "repository of a chart: chart=https://... or chart=oci://... (repeatable)")
	fs.BoolVar(&o.artifactHub, "artifacthub", false, "look up unknown chart repositories on Artifact Hub")

	fs.StringVar(&o.cleanStrategy, "clean", "", "cleaning strategy: auto, managed-fields, heuristic or minimal")
	fs.StringVar(&o.groupBy, "group-by", "", "split objects into apps by namespace, label or single")
	fs.StringVar(&o.groupLabel, "group-label", "", "label used with --group-by label (default app.kubernetes.io/part-of)")
	fs.BoolVar(&o.noOrder, "no-order", false, "don't order apps (no sync waves or dependsOn)")
	fs.StringArrayVar(&o.waves, "wave", nil, "pin an app (glob) to a wave: app=N (repeatable)")
	fs.StringArrayVar(&o.dependsOn, "depends-on", nil, "make an app depend on others: app=dep1,dep2 (repeatable)")

	fs.StringArrayVar(&o.replace, "replace", nil, "replace a string everywhere, also in Helm values: from=to (repeatable)")
	fs.StringArrayVar(&o.imageMap, "image-registry", nil, "rewrite image prefixes: from=to, e.g. test.azurecr.io=uat.azurecr.io (repeatable)")
	fs.StringArrayVar(&o.storageClassMap, "storage-class", nil, "map StorageClasses: from=to (repeatable)")
	fs.StringArrayVar(&o.ingressClassMap, "ingress-class", nil, "map IngressClasses: from=to (repeatable)")
	fs.StringArrayVar(&o.namespaceMap, "namespace-map", nil, "rename namespaces: from=to (repeatable)")
}

func (o *options) addSecrets(fs *pflag.FlagSet) {
	fs.StringVar(&o.secrets, "secrets", "", "Secret strategy: skip, local, plain, sops, sealed-secrets or external-secrets")
	fs.BoolVar(&o.allowPlain, "allow-plain-secrets", false, "allow --secrets plain, which writes Secret data unencrypted")
	fs.StringSliceVar(&o.sopsAge, "sops-age", nil, "age recipients for --secrets sops (repeatable)")
	fs.StringVar(&o.sealedCert, "sealed-secrets-cert", "", "sealed-secrets controller certificate (default: fetched from the environment's cluster)")
	fs.StringVar(&o.esoStore, "eso-store", "", "SecretStore name for --secrets external-secrets")
	fs.StringVar(&o.esoStoreKind, "eso-store-kind", "", "SecretStore or ClusterSecretStore")
}

func (o *options) addOutput(fs *pflag.FlagSet) {
	fs.StringVarP(&o.output, "output", "o", "", "output directory (default ./gitops)")
	fs.StringVar(&o.tool, "tool", "", "GitOps tool: none, argocd or flux")
	fs.StringVar(&o.pattern, "pattern", "", "Argo CD pattern: app-of-apps, applicationset or single")
	fs.StringVar(&o.layout, "layout", "", "environment layout: folder-per-env or repo-per-env")
	fs.StringVar(&o.repoURL, "repo-url", "", "Git repository the output is pushed to")
	fs.StringVar(&o.revision, "revision", "", "Git branch (default main)")
	fs.StringVar(&o.repoPath, "repo-path", "", "directory of the output inside the repository")
	fs.StringArrayVar(&o.envs, "env", nil, "target environment: name or name=kube-context (repeatable)")
	fs.BoolVar(&o.adopt, "adopt", false, "the target already runs these objects: generate takeover-safe sync settings")
	fs.BoolVar(&o.force, "force", false, "write into a non-empty directory not created by gitopsify")
	fs.StringVar(&o.argoVersion, "argocd-version", "", "Argo CD version to install (default "+config.DefaultArgoCDVersion+")")
	fs.StringVar(&o.fluxVersion, "flux-version", "", "Flux version to install (default "+config.DefaultFluxVersion+")")
	fs.StringVar(&o.project, "project", "", "Argo CD project (default gitopsify)")
	fs.BoolVar(&o.noAutoSync, "no-auto-sync", false, "don't enable automated sync on Argo CD applications")
}

// addEnv registers --env for commands that act on one environment.
func (o *options) addEnv(fs *pflag.FlagSet) {
	fs.StringArrayVar(&o.envs, "env", nil, "environment to act on (default: the first one)")
}

// load builds the configuration: the file (if any), then flags that were set.
func (o *options) load(cmd *cobra.Command) (*config.Config, error) {
	cfg := &config.Config{}
	path := o.configFile
	if path == "" {
		if _, err := os.Stat(DefaultConfigFile); err == nil {
			path = DefaultConfigFile
		}
	}
	if path != "" {
		var err error
		if cfg, err = config.Load(path); err != nil {
			return nil, err
		}
	}
	if err := o.apply(cmd.Flags(), cfg); err != nil {
		return nil, err
	}
	cfg.Default()
	return cfg, nil
}

func (o *options) apply(fs *pflag.FlagSet, cfg *config.Config) error {
	set := func(name string) bool { f := fs.Lookup(name); return f != nil && f.Changed }
	if set("kubeconfig") {
		cfg.Source.Kubeconfig = o.kubeconfig
	}
	if set("context") {
		cfg.Source.Context = o.context
	}
	sc := &cfg.Scope
	if set("namespace") {
		sc.Namespaces = o.namespaces
	}
	if set("exclude-namespace") {
		sc.ExcludeNamespaces = o.excludeNamespaces
	}
	if set("selector") {
		sc.LabelSelector = o.selector
	}
	if set("resource") {
		sc.Resources = o.resources
	}
	if set("no-deps") {
		v := !o.noDeps
		sc.IncludeDependencies = &v
	}
	if set("kind") {
		sc.Kinds = o.kinds
	}
	if set("exclude-kind") {
		sc.ExcludeKinds = o.excludeKinds
	}
	if set("cluster-scoped") {
		sc.ClusterScoped = o.clusterScoped
	}
	if set("disable-rule") {
		sc.DisableDefaultRules = append(sc.DisableDefaultRules, o.disableRules...)
	}
	if set("include-managed") {
		sc.IncludeAlreadyManaged = o.includeManaged
	}
	if set("exclude-app") {
		sc.ExcludeApps = append(sc.ExcludeApps, o.excludeApps...)
	}
	if set("helm-mode") {
		cfg.Helm.Mode = o.helmMode
	}
	if set("helm-repo") {
		if cfg.Helm.Repositories == nil {
			cfg.Helm.Repositories = map[string]string{}
		}
		for _, kv := range o.helmRepos {
			k, v, err := pair("--helm-repo", kv)
			if err != nil {
				return err
			}
			cfg.Helm.Repositories[k] = v
		}
	}
	if set("artifacthub") && o.artifactHub {
		if cfg.Helm.Resolvers == nil {
			cfg.Helm.Resolvers = []string{"local"}
		}
		cfg.Helm.Resolvers = append(cfg.Helm.Resolvers, "artifacthub")
	}
	if set("clean") {
		cfg.Clean.Strategy = o.cleanStrategy
	}
	if set("group-by") {
		cfg.Grouping.By = o.groupBy
	}
	if set("group-label") {
		cfg.Grouping.Label = o.groupLabel
	}
	if set("no-order") {
		v := !o.noOrder
		cfg.Ordering.Enabled = &v
	}
	if set("wave") {
		if cfg.Ordering.Waves == nil {
			cfg.Ordering.Waves = map[string]int{}
		}
		for _, kv := range o.waves {
			k, v, err := pair("--wave", kv)
			if err != nil {
				return err
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("--wave %s: %q is not a number", kv, v)
			}
			cfg.Ordering.Waves[k] = n
		}
	}
	if set("depends-on") {
		if cfg.Ordering.DependsOn == nil {
			cfg.Ordering.DependsOn = map[string][]string{}
		}
		for _, kv := range o.dependsOn {
			k, v, err := pair("--depends-on", kv)
			if err != nil {
				return err
			}
			cfg.Ordering.DependsOn[k] = append(cfg.Ordering.DependsOn[k], strings.Split(v, ",")...)
		}
	}
	if err := o.applyTransforms(set, cfg); err != nil {
		return err
	}
	if set("secrets") {
		cfg.Secrets.Strategy = o.secrets
	}
	if set("allow-plain-secrets") {
		cfg.Secrets.AllowPlain = o.allowPlain
	}
	if set("sops-age") {
		cfg.Secrets.SOPS.Age = o.sopsAge
	}
	if set("sealed-secrets-cert") {
		cfg.Secrets.SealedSecrets.CertFile = o.sealedCert
	}
	if set("eso-store") {
		cfg.Secrets.ExternalSecrets.StoreName = o.esoStore
	}
	if set("eso-store-kind") {
		cfg.Secrets.ExternalSecrets.StoreKind = o.esoStoreKind
	}
	out := &cfg.Output
	if set("output") {
		out.Dir = o.output
	}
	if set("tool") {
		out.Tool = o.tool
	}
	if set("pattern") {
		out.ArgoCD.Pattern = o.pattern
	}
	if set("layout") {
		out.Layout = o.layout
	}
	if set("repo-url") {
		out.Repo.URL = o.repoURL
	}
	if set("revision") {
		out.Repo.Revision = o.revision
	}
	if set("repo-path") {
		out.Repo.Path = o.repoPath
	}
	if set("argocd-version") {
		out.ArgoCD.Version = o.argoVersion
	}
	if set("flux-version") {
		out.Flux.Version = o.fluxVersion
	}
	if set("project") {
		out.ArgoCD.Project = o.project
	}
	if set("no-auto-sync") {
		v := !o.noAutoSync
		out.ArgoCD.AutoSync = &v
	}
	if set("adopt") {
		cfg.Adopt.Enabled = o.adopt
	}
	// --env defines environments on commands that generate; elsewhere it
	// selects one (see envName).
	if set("env") && fs.Lookup("tool") != nil {
		var envs []config.Environment
		for _, e := range o.envs {
			name, ctx, _ := strings.Cut(e, "=")
			env := config.Environment{Name: name}
			if existing, err := cfg.Env(name); err == nil {
				env = *existing
			}
			if ctx != "" {
				env.Cluster.Context = ctx
			}
			envs = append(envs, env)
		}
		cfg.Environments = envs
	}
	return nil
}

func (o *options) applyTransforms(set func(string) bool, cfg *config.Config) error {
	for _, f := range []struct {
		flag, typ string
		values    []string
	}{
		{"replace", "replace", o.replace},
		{"image-registry", "image", o.imageMap},
		{"storage-class", "storageClass", o.storageClassMap},
		{"ingress-class", "ingressClass", o.ingressClassMap},
		{"namespace-map", "namespace", o.namespaceMap},
	} {
		if !set(f.flag) {
			continue
		}
		m := map[string]string{}
		for _, kv := range f.values {
			k, v, err := pair("--"+f.flag, kv)
			if err != nil {
				return err
			}
			switch f.typ {
			case "replace", "image":
				cfg.Transforms = append(cfg.Transforms, config.Transform{Type: f.typ, From: k, To: v})
			default:
				m[k] = v
			}
		}
		if len(m) > 0 {
			cfg.Transforms = append(cfg.Transforms, config.Transform{Type: f.typ, Map: m})
		}
	}
	return nil
}

func pair(flag, kv string) (string, string, error) {
	k, v, ok := strings.Cut(kv, "=")
	if !ok || k == "" {
		return "", "", fmt.Errorf("%s %q: expected key=value", flag, kv)
	}
	return k, v, nil
}

// pipelineOptions returns the pipeline options for the source: a cluster, or
// the --from-file objects.
func (o *options) pipelineOptions(cfg *config.Config, log func(string, ...any)) (pipeline.Options, error) {
	opts := pipeline.Options{Config: cfg, Log: log, Version: Version}
	if len(o.fromFiles) > 0 {
		objs, err := discovery.ReadObjects(o.fromFiles)
		if err != nil {
			return opts, err
		}
		opts.Offline = objs
		opts.OfflineName = strings.Join(o.fromFiles, ", ")
	}
	return opts, nil
}

// envName returns the environment a single-environment command acts on.
func (o *options) envName(cfg *config.Config) (*config.Environment, error) {
	if len(o.envs) > 1 {
		return nil, errors.New("--env can be given once for this command")
	}
	if len(o.envs) == 1 {
		name, ctx, _ := strings.Cut(o.envs[0], "=")
		e, err := cfg.Env(name)
		if err != nil {
			// Without configured environments, --env defines the one to use.
			if len(cfg.Environments) != 1 || cfg.Environments[0].Name != "default" {
				return nil, err
			}
			cfg.Environments[0].Name = name
			e = &cfg.Environments[0]
		}
		if ctx != "" {
			e.Cluster.Context = ctx
		}
		return e, nil
	}
	return &cfg.Environments[0], nil
}
