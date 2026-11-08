// Package config defines gitopsify.yaml, the file that describes a migration
// so it can be reviewed, committed and re-run.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"time"
)

// APIVersion and Kind of the configuration file.
const (
	APIVersion = "gitopsify.io/v1alpha1"
	Kind       = "Migration"
)

// Tools gitopsify can generate configuration for.
const (
	ToolNone   = "none"
	ToolArgoCD = "argocd"
	ToolFlux   = "flux"
)

// Argo CD patterns.
const (
	PatternAppOfApps      = "app-of-apps"
	PatternApplicationSet = "applicationset"
	PatternSingle         = "single"
)

// Environment layouts.
const (
	LayoutFolderPerEnv = "folder-per-env"
	LayoutRepoPerEnv   = "repo-per-env"
)

// Cleaning strategies.
const (
	CleanAuto          = "auto"
	CleanManagedFields = "managed-fields"
	CleanHeuristic     = "heuristic"
	CleanMinimal       = "minimal"
)

// Secret strategies.
const (
	SecretsSkip            = "skip"
	SecretsLocal           = "local"
	SecretsPlain           = "plain"
	SecretsSOPS            = "sops"
	SecretsSealedSecrets   = "sealed-secrets"
	SecretsExternalSecrets = "external-secrets"
)

// Grouping strategies.
const (
	GroupByNamespace = "namespace"
	GroupByLabel     = "label"
	GroupSingle      = "single"
)

// Helm modes as written in the configuration; "auto" picks per release.
const (
	HelmAuto      = "auto"
	HelmChart     = "chart"
	HelmVendor    = "vendor"
	HelmManifests = "manifests"
)

// Config is a migration: the spec of the Migration resource in
// gitopsify.yaml.
type Config struct {
	// APIVersion, Kind and Metadata are the resource's, written around the
	// spec. APIVersion and Kind are JSON fields only for v0.1's layout, which
	// had the spec's fields next to them (see decodeMigration).
	APIVersion   string        `json:"apiVersion,omitempty"`
	Kind         string        `json:"kind,omitempty"`
	Metadata     ObjectMeta    `json:"-"`
	Source       Source        `json:"source"`
	Scope        Scope         `json:"scope"`
	Helm         Helm          `json:"helm"`
	Clean        Clean         `json:"clean"`
	Grouping     Grouping      `json:"grouping"`
	Ordering     Ordering      `json:"ordering"`
	Secrets      Secrets       `json:"secrets"`
	Transforms   []Transform   `json:"transforms,omitempty"`
	Output       Output        `json:"output"`
	Environments []Environment `json:"environments"`
	Adopt        Adopt         `json:"adopt"`
}

// Source is the cluster gitopsify reads from.
type Source struct {
	Access `json:",inline"`
}

// Access is how gitopsify reaches a cluster's API server. With nothing set it
// behaves like kubectl: KUBECONFIG or ~/.kube/config and its current context,
// including the credential plugins of EKS, GKE, AKS, OpenShift, Rancher,
// Teleport and OIDC logins. The other fields override parts of the
// kubeconfig, or replace it entirely (apiServer plus a token or a client
// certificate).
type Access struct {
	// Kubeconfig file. Several files are separated like in KUBECONFIG
	// (":" on Linux and macOS) and merged. Empty uses KUBECONFIG or
	// ~/.kube/config.
	Kubeconfig string `json:"kubeconfig,omitempty"`
	// Context in the kubeconfig; empty uses the current context.
	Context string `json:"context,omitempty"`
	// InCluster uses the ServiceAccount of the pod gitopsify runs in.
	InCluster bool `json:"inCluster,omitempty"`
	// APIServer is the API server URL, overriding the kubeconfig's.
	APIServer string `json:"apiServer,omitempty"`
	// TokenFile holds a bearer token: a ServiceAccount token, an OIDC ID
	// token, a cloud access token. It is re-read when it changes.
	TokenFile string `json:"tokenFile,omitempty"`
	// Token is a bearer token from the command line or the environment. It is
	// never written to files, so it can't end up in Git.
	Token string `json:"-"`
	// ClientCertificate and ClientKey are PEM files for TLS client
	// authentication.
	ClientCertificate string `json:"clientCertificate,omitempty"`
	ClientKey         string `json:"clientKey,omitempty"`
	// CertificateAuthority is a PEM file with the CA that signed the API
	// server's certificate.
	CertificateAuthority string `json:"certificateAuthority,omitempty"`
	// InsecureSkipTLSVerify doesn't verify the API server's certificate.
	// Only for test clusters.
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`
	// TLSServerName is the name to verify the API server's certificate
	// against, when it is reached by another address (an IP, a tunnel).
	TLSServerName string `json:"tlsServerName,omitempty"`
	// ProxyURL is an http(s) or socks5 proxy for the API server.
	ProxyURL string `json:"proxyURL,omitempty"`
	// Impersonate acts as another user, like kubectl --as.
	Impersonate       string   `json:"impersonate,omitempty"`
	ImpersonateGroups []string `json:"impersonateGroups,omitempty"`
	ImpersonateUID    string   `json:"impersonateUID,omitempty"`
	// Timeout of each API request, for example 30s. Empty means none.
	Timeout string `json:"timeout,omitempty"`
}

// Configured reports whether any connection setting is present.
func (a Access) Configured() bool {
	return a.Kubeconfig != "" || a.Context != "" || a.InCluster || a.APIServer != ""
}

// Validate reports contradictory connection settings.
func (a Access) Validate() error {
	var errs []error
	if a.InCluster && (a.Kubeconfig != "" || a.Context != "") {
		errs = append(errs, errors.New("inCluster can't be combined with kubeconfig or context"))
	}
	if (a.ClientCertificate == "") != (a.ClientKey == "") {
		errs = append(errs, errors.New("clientCertificate and clientKey go together"))
	}
	if a.InsecureSkipTLSVerify && a.CertificateAuthority != "" {
		errs = append(errs, errors.New("insecureSkipTLSVerify can't be combined with certificateAuthority"))
	}
	if a.Token != "" && a.TokenFile != "" {
		errs = append(errs, errors.New("a token and a tokenFile can't both be set"))
	}
	if a.APIServer != "" {
		if u, err := url.Parse(a.APIServer); err != nil || u.Scheme == "" || u.Host == "" {
			errs = append(errs, fmt.Errorf("apiServer %q must be a URL such as https://10.0.0.1:6443", a.APIServer))
		}
	}
	if a.ProxyURL != "" {
		if u, err := url.Parse(a.ProxyURL); err != nil || u.Scheme == "" || u.Host == "" {
			errs = append(errs, fmt.Errorf("proxyURL %q must be a URL such as http://proxy:3128 or socks5://localhost:1080", a.ProxyURL))
		}
	}
	if a.Timeout != "" {
		if d, err := time.ParseDuration(a.Timeout); err != nil || d < 0 {
			errs = append(errs, fmt.Errorf("timeout %q must be a duration such as 30s", a.Timeout))
		}
	}
	if len(a.ImpersonateGroups) > 0 && a.Impersonate == "" {
		errs = append(errs, errors.New("impersonateGroups needs impersonate"))
	}
	return errors.Join(errs...)
}

// Scope selects what to migrate.
type Scope struct {
	// Namespaces to migrate (globs). Empty means every namespace.
	Namespaces []string `json:"namespaces,omitempty"`
	// ExcludeNamespaces are skipped (globs).
	ExcludeNamespaces []string `json:"excludeNamespaces,omitempty"`
	// LabelSelector restricts namespaced objects.
	LabelSelector string `json:"labelSelector,omitempty"`
	// Resources migrates specific objects, written as
	// "kind[.group]/[namespace/]name", for example "deployment/shop/web".
	Resources []string `json:"resources,omitempty"`
	// IncludeDependencies pulls in what selected objects need: ConfigMaps,
	// Secrets, ServiceAccounts and RBAC, PVCs, Services, Ingresses, HPAs,
	// PDBs, NetworkPolicies, CRDs and Namespaces. Defaults to true.
	IncludeDependencies *bool `json:"includeDependencies,omitempty"`
	// Kinds restricts the migration to these kinds ("Deployment",
	// "deployments", "ingress.networking.k8s.io").
	Kinds []string `json:"kinds,omitempty"`
	// ExcludeKinds are never migrated.
	ExcludeKinds []string `json:"excludeKinds,omitempty"`
	// ClusterScoped controls cluster-scoped objects: "auto" (all of them for
	// a whole-cluster migration, only referenced ones otherwise), "all" or
	// "none".
	ClusterScoped string `json:"clusterScoped,omitempty"`
	// Rules include or exclude objects and take precedence over the default
	// rules.
	Rules []Rule `json:"rules,omitempty"`
	// DisableDefaultRules switches off built-in exclusion rules by name, or
	// all of them with "*". `gitopsify rules` lists them.
	DisableDefaultRules []string `json:"disableDefaultRules,omitempty"`
	// IncludeAlreadyManaged also migrates objects already managed by Argo CD
	// or Flux in the source cluster.
	IncludeAlreadyManaged bool `json:"includeAlreadyManaged,omitempty"`
	// ExcludeApps drops whole apps (globs) after grouping, for example the
	// ones deselected in `gitopsify generate --interactive`.
	ExcludeApps []string `json:"excludeApps,omitempty"`
}

// Rule includes or excludes the objects it matches.
type Rule struct {
	Name     string `json:"name,omitempty"`
	Action   string `json:"action"` // include | exclude
	Selector `json:",inline"`
	Reason   string `json:"reason,omitempty"`
}

// Selector matches objects. Empty fields match everything; every non-empty
// field must match.
type Selector struct {
	Kinds         []string `json:"kinds,omitempty"`
	Namespaces    []string `json:"namespaces,omitempty"`
	Names         []string `json:"names,omitempty"`
	LabelSelector string   `json:"labelSelector,omitempty"`
	Apps          []string `json:"apps,omitempty"`
}

// Helm configures how Helm releases are migrated.
type Helm struct {
	// Mode: auto (chart if the repository is known, else vendor, else
	// manifests), chart, vendor or manifests.
	Mode string `json:"mode,omitempty"`
	// Repositories maps chart names to repository URLs (https:// or oci://).
	Repositories map[string]string `json:"repositories,omitempty"`
	// Resolvers find repositories of charts not in Repositories, in order:
	// "local" (Helm's repository cache) and "artifacthub".
	Resolvers []string `json:"resolvers,omitempty"`
	// Releases overrides settings per release, keyed "<namespace>/<name>".
	Releases map[string]ReleaseOverride `json:"releases,omitempty"`
}

// ReleaseOverride overrides the settings of one release.
type ReleaseOverride struct {
	Mode    string `json:"mode,omitempty"`
	Repo    string `json:"repo,omitempty"`
	Chart   string `json:"chart,omitempty"`
	Version string `json:"version,omitempty"`
	Skip    bool   `json:"skip,omitempty"`
	// Include migrates the release even where the default rules would skip
	// it, for example a release installed in kube-system.
	Include bool `json:"include,omitempty"`
}

// Clean configures how live objects are turned into manifests.
type Clean struct {
	// Strategy: auto, managed-fields, heuristic or minimal.
	Strategy string `json:"strategy,omitempty"`
	// ControllerManagers are extra field managers whose fields are treated
	// as controller-owned and dropped by the managed-fields strategy.
	ControllerManagers []string `json:"controllerManagers,omitempty"`
	// StripAnnotations and StripLabels are removed everywhere (globs).
	StripAnnotations []string `json:"stripAnnotations,omitempty"`
	StripLabels      []string `json:"stripLabels,omitempty"`
}

// Grouping configures how objects are split into apps.
type Grouping struct {
	// By: namespace (default), label or single.
	By string `json:"by,omitempty"`
	// Label used when By is "label". Objects without it fall back to their
	// namespace.
	Label string `json:"label,omitempty"`
	// Apps assigns matching objects to a named app, before any other rule.
	Apps []AppAssignment `json:"apps,omitempty"`
}

// AppAssignment puts the objects a selector matches into an app.
type AppAssignment struct {
	Name     string `json:"name"`
	Selector `json:",inline"`
}

// Ordering configures the reconciliation order.
type Ordering struct {
	// Enabled turns ordering off entirely when false. Defaults to true.
	Enabled *bool `json:"enabled,omitempty"`
	// InferDependencies derives dependencies from CRD ownership and secret
	// controllers. Defaults to true.
	InferDependencies *bool `json:"inferDependencies,omitempty"`
	// Waves pins apps (globs) to a wave.
	Waves map[string]int `json:"waves,omitempty"`
	// DependsOn adds explicit dependencies between apps.
	DependsOn map[string][]string `json:"dependsOn,omitempty"`
}

// Secrets configures how Secret objects are migrated.
type Secrets struct {
	Strategy string `json:"strategy,omitempty"`
	// AllowPlain must be true for the "plain" strategy.
	AllowPlain      bool             `json:"allowPlain,omitempty"`
	SOPS            SOPS             `json:"sops,omitempty"`
	SealedSecrets   SealedSecrets    `json:"sealedSecrets,omitempty"`
	ExternalSecrets ExternalSecrets  `json:"externalSecrets,omitempty"`
	Overrides       []SecretOverride `json:"overrides,omitempty"`
}

// SOPS configures encryption with the sops binary.
type SOPS struct {
	Age            []string `json:"age,omitempty"`
	PGP            []string `json:"pgp,omitempty"`
	KMS            []string `json:"kms,omitempty"`
	GCPKMS         []string `json:"gcpKms,omitempty"`
	AzureKV        []string `json:"azureKv,omitempty"`
	EncryptedRegex string   `json:"encryptedRegex,omitempty"`
	// DecryptionSecret is the Flux decryption Secret name.
	DecryptionSecret string `json:"decryptionSecret,omitempty"`
}

// SealedSecrets configures sealing for the sealed-secrets controller.
type SealedSecrets struct {
	// CertFile is the controller's public certificate. When empty it is
	// fetched from the first environment's cluster.
	CertFile            string `json:"certFile,omitempty"`
	ControllerNamespace string `json:"controllerNamespace,omitempty"`
	ControllerName      string `json:"controllerName,omitempty"`
	// Scope: strict (default), namespace-wide or cluster-wide.
	Scope string `json:"scope,omitempty"`
}

// ExternalSecrets configures ExternalSecret generation.
type ExternalSecrets struct {
	APIVersion string `json:"apiVersion,omitempty"`
	StoreKind  string `json:"storeKind,omitempty"`
	StoreName  string `json:"storeName,omitempty"`
	// KeyTemplate is a Go template for the remote key, with .Namespace,
	// .Name and .Key.
	KeyTemplate     string `json:"keyTemplate,omitempty"`
	PropertyPerKey  *bool  `json:"propertyPerKey,omitempty"`
	RefreshInterval string `json:"refreshInterval,omitempty"`
}

// SecretOverride sets the strategy of matching secrets.
type SecretOverride struct {
	Namespaces []string `json:"namespaces,omitempty"`
	Names      []string `json:"names,omitempty"`
	Strategy   string   `json:"strategy"`
}

// Transform changes objects on their way to Git. See the README for the
// fields each type uses.
type Transform struct {
	// Type: replace, image, storageClass, ingressClass, namespace, set,
	// remove, jsonPatch, labels, annotations.
	Type   string    `json:"type"`
	Target *Selector `json:"target,omitempty"`
	From   string    `json:"from,omitempty"`
	To     string    `json:"to,omitempty"`
	Regex  bool      `json:"regex,omitempty"`
	// Map is used by storageClass, ingressClass and namespace.
	Map   map[string]string `json:"map,omitempty"`
	Path  string            `json:"path,omitempty"`
	Value any               `json:"value,omitempty"`
	Patch []map[string]any  `json:"patch,omitempty"`
	// Set and Remove are used by labels and annotations.
	Set    map[string]string `json:"set,omitempty"`
	Remove []string          `json:"remove,omitempty"`
	// HelmValues also applies replace and image to Helm values. Defaults
	// to true.
	HelmValues *bool `json:"helmValues,omitempty"`
}

// Output configures what gets written.
type Output struct {
	Dir    string `json:"dir,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Layout string `json:"layout,omitempty"`
	Repo   Repo   `json:"repo,omitempty"`
	ArgoCD ArgoCD `json:"argocd,omitempty"`
	Flux   Flux   `json:"flux,omitempty"`
}

// Repo is the Git repository the output is pushed to.
type Repo struct {
	URL      string `json:"url,omitempty"`
	Revision string `json:"revision,omitempty"`
	// Path is the directory inside the repository that holds the output.
	Path string `json:"path,omitempty"`
}

// ArgoCD configures the Argo CD output.
type ArgoCD struct {
	Pattern   string `json:"pattern,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Project   string `json:"project,omitempty"`
	// CreateProject writes an AppProject. Defaults to true.
	CreateProject *bool  `json:"createProject,omitempty"`
	Version       string `json:"version,omitempty"`
	// SelfManage adds an Application that manages Argo CD itself.
	SelfManage      bool  `json:"selfManage,omitempty"`
	AutoSync        *bool `json:"autoSync,omitempty"`
	Prune           *bool `json:"prune,omitempty"`
	SelfHeal        *bool `json:"selfHeal,omitempty"`
	ServerSideApply *bool `json:"serverSideApply,omitempty"`
}

// Flux configures the Flux output.
type Flux struct {
	Namespace string `json:"namespace,omitempty"`
	Interval  string `json:"interval,omitempty"`
	Version   string `json:"version,omitempty"`
	Prune     *bool  `json:"prune,omitempty"`
	// SecretName is the Git credentials Secret of the GitRepository.
	SecretName string `json:"secretName,omitempty"`
}

// Environment is a target environment.
type Environment struct {
	Name    string  `json:"name"`
	Cluster Cluster `json:"cluster,omitempty"`
	// RepoURL is the environment's repository when the layout is
	// repo-per-env.
	RepoURL    string      `json:"repoURL,omitempty"`
	Transforms []Transform `json:"transforms,omitempty"`
}

// Cluster is where an environment runs.
type Cluster struct {
	// Server is the API server URL Argo CD deploys to, as Argo CD sees it:
	// https://kubernetes.default.svc when Argo CD runs in the environment's
	// own cluster.
	Server string `json:"server,omitempty"`
	// Name is the Argo CD cluster name; used instead of Server when set.
	Name string `json:"name,omitempty"`
	// Access is how gitopsify connects to the cluster for check, diff,
	// adopt, status and bootstrap, and to fetch the sealed-secrets
	// certificate.
	Access `json:",inline"`
}

// Adopt configures taking over objects that already exist in the target.
type Adopt struct {
	// Enabled generates safe sync settings for a target that already runs
	// the objects (in-place takeover).
	Enabled bool `json:"enabled,omitempty"`
	// ProtectKinds are never pruned or deleted by the GitOps tool.
	ProtectKinds []string `json:"protectKinds,omitempty"`
}
