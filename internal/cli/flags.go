package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.opentelemetry.io/otel/trace"

	"github.com/hayone1/gitopsify/internal/telemetry"
	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/discovery"
	"github.com/hayone1/gitopsify/pkg/kube"
	"github.com/hayone1/gitopsify/pkg/pipeline"
)

// DefaultConfigFile is read when --config is not given and it exists.
const DefaultConfigFile = "gitopsify.yaml"

// Environment variables that hold bearer tokens, so they don't show up in
// process listings.
const (
	envSourceToken = "GITOPSIFY_TOKEN"
	envTargetToken = "GITOPSIFY_TARGET_TOKEN"
)

// options holds every flag; each command registers the groups it uses.
type options struct {
	span       trace.Span // the command's, when telemetry is on
	configFile string
	noInput    bool
	noColor    bool
	quiet      bool

	source accessFlags
	target accessFlags

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
	keep        []string
	existing    string
	edited      string
	unmanaged   string
	argoVersion string
	fluxVersion string
	project     string
	noAutoSync  bool
	interactive bool
}

// Flag groups shown as separate sections in --help.
const (
	groupScope     = "Scope"
	groupHelm      = "Helm"
	groupShape     = "Cleaning, grouping and ordering"
	groupTransform = "Environment transforms"
	groupSecrets   = "Secrets"
	groupOutput    = "Output"
	groupGit       = "Git"
	groupSource    = "Source cluster access"
	groupTarget    = "Target cluster access"
	groupBootstrap = "Bootstrap"
)

const groupAnnotation = "gitopsify_group"

// inGroup puts flags into a --help section.
func inGroup(fs *pflag.FlagSet, group string, names ...string) {
	for _, n := range names {
		_ = fs.SetAnnotation(n, groupAnnotation, []string{group})
	}
}

// accessFlags are the kubectl-style flags that say how to reach a cluster.
type accessFlags struct {
	prefix                       string
	kubeconfig, context          string
	server, token, tokenFile     string
	ca, cert, key, tlsServerName string
	proxyURL, as, asUID, timeout string
	asGroups                     []string
	insecure, inCluster          bool
	// tokenEnv holds a token when --token isn't given; what and group
	// describe the flags in --help.
	tokenEnv, what, group string
}

func (a *accessFlags) add(fs *pflag.FlagSet) {
	p, w := a.prefix, a.what
	fs.StringVar(&a.kubeconfig, p+"kubeconfig", "", "kubeconfig of the "+w+" (several files separated like in KUBECONFIG)")
	fs.StringVar(&a.context, p+"context", "", "kubeconfig context of the "+w+" (default: the current context)")
	fs.StringVar(&a.server, p+"server", "", "API server URL of the "+w+", overriding the kubeconfig's")
	fs.StringVar(&a.token, p+"token", "", "bearer token for the "+w+" (or $"+a.tokenEnv+"); never written to files")
	fs.StringVar(&a.tokenFile, p+"token-file", "", "file with a bearer token for the "+w)
	fs.StringVar(&a.cert, p+"client-certificate", "", "client certificate file (PEM) for the "+w)
	fs.StringVar(&a.key, p+"client-key", "", "client key file (PEM) for the "+w)
	fs.StringVar(&a.ca, p+"certificate-authority", "", "CA file (PEM) the "+w+"'s API server certificate is checked against")
	fs.BoolVar(&a.insecure, p+"insecure-skip-tls-verify", false, "don't verify the "+w+"'s API server certificate (test clusters only)")
	fs.StringVar(&a.tlsServerName, p+"tls-server-name", "", "server name to verify the "+w+"'s API server certificate against")
	fs.StringVar(&a.proxyURL, p+"proxy-url", "", "http(s) or socks5 proxy for the "+w+"'s API server")
	fs.StringVar(&a.as, p+"as", "", "impersonate this user on the "+w)
	fs.StringArrayVar(&a.asGroups, p+"as-group", nil, "impersonate this group on the "+w+" (repeatable)")
	fs.StringVar(&a.asUID, p+"as-uid", "", "impersonate this UID on the "+w)
	fs.BoolVar(&a.inCluster, p+"in-cluster", false, "use the ServiceAccount of the pod gitopsify runs in for the "+w)
	fs.StringVar(&a.timeout, p+"request-timeout", "", "timeout of each API request to the "+w+", e.g. 30s")
	inGroup(fs, a.group, a.names()...)
}

func (a *accessFlags) names() []string {
	var out []string
	for _, n := range []string{"kubeconfig", "context", "server", "token", "token-file", "client-certificate", "client-key",
		"certificate-authority", "insecure-skip-tls-verify", "tls-server-name", "proxy-url", "as", "as-group", "as-uid",
		"in-cluster", "request-timeout"} {
		out = append(out, a.prefix+n)
	}
	return out
}

// set reports whether any of the access flags was given.
func (a *accessFlags) set(fs *pflag.FlagSet) bool {
	for _, n := range a.names() {
		if f := fs.Lookup(n); f != nil && f.Changed {
			return true
		}
	}
	return false
}

// applyTo copies the given access flags over the configuration.
func (a *accessFlags) applyTo(fs *pflag.FlagSet, acc *config.Access) {
	set := func(n string) bool { f := fs.Lookup(a.prefix + n); return f != nil && f.Changed }
	str := func(n string, dst *string, v string) {
		if set(n) {
			*dst = v
		}
	}
	str("kubeconfig", &acc.Kubeconfig, a.kubeconfig)
	str("context", &acc.Context, a.context)
	str("server", &acc.APIServer, a.server)
	str("token-file", &acc.TokenFile, a.tokenFile)
	str("client-certificate", &acc.ClientCertificate, a.cert)
	str("client-key", &acc.ClientKey, a.key)
	str("certificate-authority", &acc.CertificateAuthority, a.ca)
	str("tls-server-name", &acc.TLSServerName, a.tlsServerName)
	str("proxy-url", &acc.ProxyURL, a.proxyURL)
	str("as", &acc.Impersonate, a.as)
	str("as-uid", &acc.ImpersonateUID, a.asUID)
	str("request-timeout", &acc.Timeout, a.timeout)
	if set("as-group") {
		acc.ImpersonateGroups = a.asGroups
	}
	if set("insecure-skip-tls-verify") {
		acc.InsecureSkipTLSVerify = a.insecure
	}
	if set("in-cluster") {
		acc.InCluster = a.inCluster
	}
	// A flag overrides the configuration, including the setting it can't
	// be combined with (the configuration's other half goes).
	if set("certificate-authority") {
		acc.InsecureSkipTLSVerify = false
	}
	if set("insecure-skip-tls-verify") && a.insecure {
		acc.CertificateAuthority = ""
	}
	if set("in-cluster") && a.inCluster {
		acc.Kubeconfig, acc.Context = "", ""
	}
	if (set("context") || set("kubeconfig")) && !set("in-cluster") {
		acc.InCluster = false
	}
	if set("token-file") {
		acc.Token = ""
	}
	// Flag, then environment, then configuration.
	switch {
	case set("token"):
		acc.Token, acc.TokenFile = a.token, ""
	case !set("token-file") && os.Getenv(a.tokenEnv) != "":
		acc.Token, acc.TokenFile = os.Getenv(a.tokenEnv), ""
	}
}

func (o *options) addGlobal(fs *pflag.FlagSet) {
	fs.StringVarP(&o.configFile, "config", "c", "", "configuration file (default ./gitopsify.yaml when it exists)")
	fs.BoolVar(&o.noInput, "no-input", false, "never prompt; fail instead where a prompt would be needed")
	fs.BoolVar(&o.noColor, "no-color", false, "disable colors (also honors NO_COLOR)")
	fs.BoolVarP(&o.quiet, "quiet", "q", false, "only print results and errors")
	o.source = accessFlags{what: "source cluster", group: groupSource, tokenEnv: envSourceToken}
	o.source.add(fs)
}

// addTarget registers the access flags of the environment a command acts on.
func (o *options) addTarget(fs *pflag.FlagSet) {
	o.target = accessFlags{prefix: "target-", what: "target cluster", group: groupTarget, tokenEnv: envTargetToken}
	o.target.add(fs)
}

func (o *options) addScope(fs *pflag.FlagSet) {
	fs.StringArrayVar(&o.fromFiles, "from-file", nil, "read the source objects from YAML/JSON files or directories instead of a cluster, e.g. kubectl get -A -o yaml output (\"-\" for stdin; repeatable)")
	fs.StringSliceVarP(&o.namespaces, "namespace", "n", nil, "migrate only these namespaces (globs, repeatable)")
	fs.StringSliceVar(&o.excludeNamespaces, "exclude-namespace", nil, "skip these namespaces (globs, repeatable)")
	fs.StringVarP(&o.selector, "selector", "l", "", "migrate objects matching this label selector, plus what they need")
	fs.StringArrayVarP(&o.resources, "resource", "r", nil, "migrate specific objects, plus what they need: kind[.group]/[namespace/]name or helm/namespace/release (repeatable)")
	fs.BoolVar(&o.noDeps, "no-deps", false, "with --resource/--selector, don't pull in dependencies")
	fs.StringSliceVar(&o.kinds, "kind", nil, "migrate only these kinds (Deployment, ingress.networking.k8s.io, svc...)")
	fs.StringSliceVar(&o.excludeKinds, "exclude-kind", nil, "never migrate these kinds")
	fs.StringVar(&o.clusterScoped, "cluster-scoped", "", "cluster-scoped objects: auto, all or none")
	fs.StringSliceVar(&o.disableRules, "disable-rule", nil, "disable a built-in exclusion rule (see \"gitopsify rules\"; \"*\" for all)")
	fs.BoolVar(&o.includeManaged, "include-managed", false, "also migrate objects already managed by Argo CD or Flux")
	fs.StringSliceVar(&o.excludeApps, "exclude-app", nil, "drop these apps (globs) after grouping")
	inGroup(fs, groupScope, "from-file", "namespace", "exclude-namespace", "selector", "resource", "no-deps", "kind",
		"exclude-kind", "cluster-scoped", "disable-rule", "include-managed", "exclude-app")

	fs.StringVar(&o.helmMode, "helm-mode", "", "Helm releases: auto, chart, vendor or manifests")
	fs.StringArrayVar(&o.helmRepos, "helm-repo", nil, "repository of a chart: chart=https://... or chart=oci://... (repeatable)")
	fs.BoolVar(&o.artifactHub, "artifacthub", false, "look up unknown chart repositories on Artifact Hub")
	inGroup(fs, groupHelm, "helm-mode", "helm-repo", "artifacthub")

	fs.StringVar(&o.cleanStrategy, "clean", "", "cleaning strategy: auto, managed-fields, heuristic or minimal")
	fs.StringVar(&o.groupBy, "group-by", "", "split objects into apps by namespace, label or single")
	fs.StringVar(&o.groupLabel, "group-label", "", "label used with --group-by label (default app.kubernetes.io/part-of)")
	fs.BoolVar(&o.noOrder, "no-order", false, "don't order apps (no sync waves or dependsOn)")
	fs.StringArrayVar(&o.waves, "wave", nil, "pin an app (glob) to a wave: app=N (repeatable)")
	fs.StringArrayVar(&o.dependsOn, "depends-on", nil, "make an app depend on others: app=dep1,dep2 (repeatable)")
	inGroup(fs, groupShape, "clean", "group-by", "group-label", "no-order", "wave", "depends-on")

	fs.StringArrayVar(&o.replace, "replace", nil, "replace a string everywhere, also in Helm values: from=to (repeatable)")
	fs.StringArrayVar(&o.imageMap, "image-registry", nil, "rewrite image prefixes: from=to, e.g. test.azurecr.io=uat.azurecr.io (repeatable)")
	fs.StringArrayVar(&o.storageClassMap, "storage-class", nil, "map StorageClasses: from=to (repeatable)")
	fs.StringArrayVar(&o.ingressClassMap, "ingress-class", nil, "map IngressClasses: from=to (repeatable)")
	fs.StringArrayVar(&o.namespaceMap, "namespace-map", nil, "rename namespaces: from=to (repeatable)")
	inGroup(fs, groupTransform, "replace", "image-registry", "storage-class", "ingress-class", "namespace-map")

	complete(fs, "cluster-scoped", "auto", "all", "none")
	complete(fs, "helm-mode", config.HelmAuto, config.HelmChart, config.HelmVendor, config.HelmManifests)
	complete(fs, "clean", config.CleanAuto, config.CleanManagedFields, config.CleanHeuristic, config.CleanMinimal)
	complete(fs, "group-by", config.GroupByNamespace, config.GroupByLabel, config.GroupSingle)
	complete(fs, "disable-rule", append(ruleNames(), "*")...)
}

func (o *options) addSecrets(fs *pflag.FlagSet) {
	fs.StringVar(&o.secrets, "secrets", "", "Secret strategy: skip, local, plain, sops, sealed-secrets or external-secrets")
	fs.BoolVar(&o.allowPlain, "allow-plain-secrets", false, "allow --secrets plain, which writes Secret data unencrypted")
	fs.StringSliceVar(&o.sopsAge, "sops-age", nil, "age recipients for --secrets sops (repeatable)")
	fs.StringVar(&o.sealedCert, "sealed-secrets-cert", "", "sealed-secrets controller certificate (default: fetched from the environment's cluster)")
	fs.StringVar(&o.esoStore, "eso-store", "", "SecretStore name for --secrets external-secrets")
	fs.StringVar(&o.esoStoreKind, "eso-store-kind", "", "SecretStore or ClusterSecretStore")
	inGroup(fs, groupSecrets, "secrets", "allow-plain-secrets", "sops-age", "sealed-secrets-cert", "eso-store", "eso-store-kind")
	complete(fs, "secrets", config.SecretsSkip, config.SecretsLocal, config.SecretsPlain, config.SecretsSOPS, config.SecretsSealedSecrets, config.SecretsExternalSecrets)
	complete(fs, "eso-store-kind", "SecretStore", "ClusterSecretStore")
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
	fs.StringVar(&o.existing, "existing", "", "objects already in an environment's cluster: include (default), skip-owned (leave out those another tool manages there) or skip (leave them all out)")
	fs.BoolVar(&o.force, "force", false, "write into a non-empty directory not created by gitopsify (overwriting its files where gitopsify writes), or remove environments this run doesn't generate")
	fs.StringArrayVar(&o.keep, "keep", nil, "never write or remove these paths of the output (globs: * within a directory, ** across; repeatable)")
	fs.StringVar(&o.edited, "edited", "", "generated files changed by hand since gitopsify wrote them: overwrite (default, with a warning), keep or fail")
	fs.StringVar(&o.unmanaged, "unmanaged", "", "files gitopsify didn't write where it writes: fail (default; overwrite with --force), overwrite or keep")
	fs.StringVar(&o.argoVersion, "argocd-version", "", "Argo CD version to install (default "+config.DefaultArgoCDVersion+")")
	fs.StringVar(&o.fluxVersion, "flux-version", "", "Flux version to install (default "+config.DefaultFluxVersion+")")
	fs.StringVar(&o.project, "project", "", "Argo CD project (default gitopsify)")
	fs.BoolVar(&o.noAutoSync, "no-auto-sync", false, "don't enable automated sync on Argo CD applications")
	inGroup(fs, groupOutput, "output", "tool", "pattern", "layout", "repo-url", "revision", "repo-path", "env", "adopt",
		"existing", "force", "keep", "edited", "unmanaged", "argocd-version", "flux-version", "project", "no-auto-sync")
	complete(fs, "edited", config.MergeOverwrite, config.MergeKeep, config.MergeFail)
	complete(fs, "existing", config.ExistingInclude, config.ExistingSkipOwned, config.ExistingSkip)
	complete(fs, "unmanaged", config.MergeFail, config.MergeOverwrite, config.MergeKeep)
	complete(fs, "tool", config.ToolNone, config.ToolArgoCD, config.ToolFlux)
	complete(fs, "pattern", config.PatternAppOfApps, config.PatternApplicationSet, config.PatternSingle)
	complete(fs, "layout", config.LayoutFolderPerEnv, config.LayoutRepoPerEnv)
}

// addEnv registers --env for commands that act on one environment.
func (o *options) addEnv(fs *pflag.FlagSet) {
	fs.StringArrayVar(&o.envs, "env", nil, "environment to act on: name or name=kube-context (default: the first one)")
	inGroup(fs, groupTarget, "env")
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
		// The gitopsify.yaml an output keeps describes that output: it
		// writes into the folder it sits in (older copies name a folder
		// relative to wherever they were made).
		if dir := filepath.Dir(path); fileExists(filepath.Join(dir, ".gitopsify", "generated.txt")) {
			cfg.Output.Dir = dir
		}
	}
	if err := o.apply(cmd.Flags(), cfg); err != nil {
		return nil, err
	}
	cfg.Default()
	if err := o.applyTarget(cmd.Flags(), cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (o *options) apply(fs *pflag.FlagSet, cfg *config.Config) error {
	set := func(name string) bool { f := fs.Lookup(name); return f != nil && f.Changed }
	o.source.applyTo(fs, &cfg.Source.Access)
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
		out.Dir = config.ExpandHome(o.output)
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
	if set("existing") {
		cfg.Adopt.Existing = o.existing
	}
	if set("keep") || set("edited") || set("unmanaged") {
		if out.Merge == nil {
			out.Merge = &config.Merge{}
		}
		if set("keep") {
			for _, k := range o.keep {
				if !slices.Contains(out.Merge.Keep, k) {
					out.Merge.Keep = append(out.Merge.Keep, k)
				}
			}
		}
		if set("edited") {
			out.Merge.Edited = o.edited
		}
		if set("unmanaged") {
			out.Merge.Unmanaged = o.unmanaged
		}
	}
	// --env defines environments on commands that generate; elsewhere it
	// selects one (see envName).
	if set("env") && fs.Lookup("tool") != nil {
		var envs []config.Environment
		seen := map[string]bool{}
		for _, e := range o.envs {
			name, ctx, _ := strings.Cut(e, "=")
			if seen[name] {
				return fmt.Errorf("--env %s is given twice", name)
			}
			seen[name] = true
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

// applyTarget copies the --target-* flags of generating commands onto their
// only environment. Commands acting on one environment apply them in envName.
func (o *options) applyTarget(fs *pflag.FlagSet, cfg *config.Config) error {
	if o.target.prefix == "" || fs.Lookup("tool") == nil {
		return nil
	}
	if len(cfg.Environments) != 1 {
		if o.target.set(fs) {
			return errors.New("--target-* flags need exactly one environment; set the others' access in gitopsify.yaml (environments[].cluster)")
		}
		return nil
	}
	o.target.applyTo(fs, &cfg.Environments[0].Cluster.Access)
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

// connectOptions are the options of every cluster connection.
func (o *options) connectOptions() kube.ConnectOptions {
	return kube.ConnectOptions{NoInput: !o.ui().Interactive, WrapTransport: telemetry.Transport}
}

// pipelineOptions returns the pipeline options for the source: a cluster, or
// the --from-file objects.
func (o *options) pipelineOptions(cfg *config.Config, log func(string, ...any)) (pipeline.Options, error) {
	opts := pipeline.Options{Config: cfg, Log: log, Version: Version, Connect: o.connectOptions()}
	if len(o.fromFiles) > 0 {
		objs, err := discovery.ReadObjects(o.fromFiles)
		if err != nil {
			return opts, err
		}
		if len(objs) == 0 {
			return opts, fmt.Errorf("no Kubernetes objects found in %s", strings.Join(o.fromFiles, ", "))
		}
		opts.Offline = objs
		opts.OfflineName = strings.Join(o.fromFiles, ", ")
	}
	return opts, nil
}

// envName returns the environment a single-environment command acts on, with
// the --target-* flags applied.
func (o *options) envName(cmd *cobra.Command, cfg *config.Config) (*config.Environment, error) {
	if len(o.envs) > 1 {
		return nil, errors.New("--env can be given once for this command")
	}
	e := &cfg.Environments[0]
	if len(o.envs) == 1 {
		name, ctx, _ := strings.Cut(o.envs[0], "=")
		var err error
		if e, err = cfg.Env(name); err != nil {
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
	}
	if o.target.prefix != "" {
		o.target.applyTo(cmd.Flags(), &e.Cluster.Access)
	}
	if err := e.Cluster.Access.Validate(); err != nil {
		return nil, fmt.Errorf("environment %s: %w", e.Name, err)
	}
	return e, nil
}

// complete records the fixed shell completions of a flag of a flag set;
// registerCompletions attaches them to the command that has the set. (Flags
// of the same name differ between commands: --format, --tool.)
func complete(fs *pflag.FlagSet, name string, values ...string) {
	if completions[fs] == nil {
		completions[fs] = map[string][]string{}
	}
	completions[fs][name] = values
}

var completions = map[*pflag.FlagSet]map[string][]string{}
