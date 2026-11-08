// Package model holds the in-memory inventory that every gitopsify stage reads
// and annotates: discovered objects, Helm releases and the apps they are
// grouped into.
package model

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Ref identifies a Kubernetes object independently of its API version.
type Ref struct {
	Group     string `json:"group,omitempty"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// GroupKind returns "Kind" for the core group and "Kind.group" otherwise.
func (r Ref) GroupKind() string {
	if r.Group == "" {
		return r.Kind
	}
	return r.Kind + "." + r.Group
}

// String returns "Kind.group/namespace/name", omitting empty parts.
func (r Ref) String() string {
	if r.Namespace == "" {
		return r.GroupKind() + "/" + r.Name
	}
	return r.GroupKind() + "/" + r.Namespace + "/" + r.Name
}

// RefOf builds the Ref of an object.
func RefOf(u *unstructured.Unstructured) Ref {
	gvk := u.GroupVersionKind()
	return Ref{Group: gvk.Group, Kind: gvk.Kind, Namespace: u.GetNamespace(), Name: u.GetName()}
}

// Object is one discovered object and what gitopsify decided about it.
type Object struct {
	Ref        Ref
	GVR        schema.GroupVersionResource
	Namespaced bool
	// Live is the object as read from the source cluster. It is never mutated.
	Live *unstructured.Unstructured
	// Out is the object as it will be written: cleaned, transformed.
	Out *unstructured.Unstructured

	// HelmRelease is "<namespace>/<name>" when the object belongs to a release.
	HelmRelease string
	// Excluded objects are reported but not written.
	Excluded bool
	// Reason explains an exclusion as "<rule>: <detail>".
	Reason string
	// Selected marks objects pulled in explicitly or as a dependency of an
	// explicitly requested object.
	Selected bool
	// App is the name of the app the object is grouped into.
	App string
	// EnvOut holds per-environment versions of objects that differ by
	// environment, such as SealedSecrets sealed for each cluster. When set,
	// the object is written to the environment overlays instead of the base.
	EnvOut map[string]*unstructured.Unstructured
	// Encrypt marks Secrets that must be encrypted with SOPS when written.
	Encrypt  bool
	Warnings []string
}

// Exclude marks the object as excluded unless it already is.
func (o *Object) Exclude(rule, detail string) {
	if o.Excluded {
		return
	}
	o.Excluded = true
	o.Reason = rule
	if detail != "" {
		o.Reason += ": " + detail
	}
}

// Include clears an exclusion.
func (o *Object) Include() {
	o.Excluded = false
	o.Reason = ""
}

// Warn records a warning for the object.
func (o *Object) Warn(format string, args ...any) {
	o.Warnings = append(o.Warnings, fmt.Sprintf(format, args...))
}

// IsCRD reports whether the object is a CustomResourceDefinition.
func (o *Object) IsCRD() bool {
	return o.Ref.Group == "apiextensions.k8s.io" && o.Ref.Kind == "CustomResourceDefinition"
}

// IsSecret reports whether the object is a core Secret.
func (o *Object) IsSecret() bool {
	return o.Ref.Group == "" && o.Ref.Kind == "Secret"
}

// IsNamespace reports whether the object is a Namespace.
func (o *Object) IsNamespace() bool {
	return o.Ref.Group == "" && o.Ref.Kind == "Namespace"
}

// File is a file stored in a Helm chart.
type File struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// ChartDependency is a subchart declared in Chart.yaml.
type ChartDependency struct {
	Name       string `json:"name"`
	Version    string `json:"version,omitempty"`
	Repository string `json:"repository,omitempty"`
	Alias      string `json:"alias,omitempty"`
	Condition  string `json:"condition,omitempty"`
}

// ChartMeta is the Chart.yaml metadata stored in a release.
type ChartMeta struct {
	APIVersion   string            `json:"apiVersion,omitempty"`
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	AppVersion   string            `json:"appVersion,omitempty"`
	Description  string            `json:"description,omitempty"`
	Type         string            `json:"type,omitempty"`
	Home         string            `json:"home,omitempty"`
	Sources      []string          `json:"sources,omitempty"`
	Keywords     []string          `json:"keywords,omitempty"`
	KubeVersion  string            `json:"kubeVersion,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	Dependencies []ChartDependency `json:"dependencies,omitempty"`
}

// HelmMode is how a Helm release is represented in the output.
type HelmMode string

// Helm modes.
const (
	// HelmModeChart references the chart in its repository plus values.
	HelmModeChart HelmMode = "chart"
	// HelmModeVendor writes the chart, rebuilt from the release, into Git.
	HelmModeVendor HelmMode = "vendor"
	// HelmModeManifests writes the release's live objects as plain manifests.
	HelmModeManifests HelmMode = "manifests"
)

// ChartRepo is where a chart can be pulled from.
type ChartRepo struct {
	// URL is an https:// Helm repository or an oci:// registry path that the
	// chart name is appended to.
	URL string `json:"url"`
	// Source records how the repository was resolved.
	Source string `json:"source"`
}

// IsOCI reports whether the repository is an OCI registry.
func (r ChartRepo) IsOCI() bool { return strings.HasPrefix(r.URL, "oci://") }

// HelmHook is a hook manifest stored in a release.
type HelmHook struct {
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Path     string   `json:"path"`
	Manifest string   `json:"manifest"`
	Events   []string `json:"events"`
}

// HelmRelease is the deployed revision of a Helm release.
type HelmRelease struct {
	Name        string
	Namespace   string
	Revision    int
	Status      string
	Chart       ChartMeta
	ChartValues map[string]any
	Values      map[string]any
	Manifest    string
	Hooks       []HelmHook
	Templates   []File
	Files       []File
	Schema      []byte
	// CRDs are the chart's crds/ files. Helm installs them but never tracks
	// them in the release manifest.
	CRDs []File

	// Owned lists the objects the release manifest declares.
	Owned []Ref
	Repo  *ChartRepo
	Mode  HelmMode
	// Skip excludes the release from the output, for SkipReason.
	Skip       bool
	SkipReason string
	// Selected marks releases requested explicitly or through one of
	// their objects.
	Selected bool
	Warnings []string
}

// Key returns "<namespace>/<name>".
func (r *HelmRelease) Key() string { return r.Namespace + "/" + r.Name }

// Vendorable reports whether the chart can be rebuilt from the release. Helm
// does not store subcharts in releases, so charts with dependencies cannot.
func (r *HelmRelease) Vendorable() bool {
	return len(r.Chart.Dependencies) == 0 && len(r.Templates) > 0
}

// Phase is the coarse reconciliation phase of an app.
type Phase string

// Phases in reconciliation order.
const (
	PhaseCRDs            Phase = "crds"
	PhaseNamespaces      Phase = "namespaces"
	PhaseCluster         Phase = "cluster"
	PhasePlatform        Phase = "platform"
	PhaseApps            Phase = "apps"
	PhaseCustomResources Phase = "custom-resources"
)

// PhaseWave is the default sync wave of each phase.
var PhaseWave = map[Phase]int{
	PhaseCRDs:            -30,
	PhaseNamespaces:      -20,
	PhaseCluster:         -10,
	PhasePlatform:        -5,
	PhaseApps:            0,
	PhaseCustomResources: 10,
}

// AppKind distinguishes plain manifest apps from Helm apps.
type AppKind string

// App kinds.
const (
	AppManifests AppKind = "manifests"
	AppHelm      AppKind = "helm"
)

// App is a unit of deployment: one Argo CD Application or Flux Kustomization.
type App struct {
	Name string
	Kind AppKind
	// Namespace is the app's main namespace; empty for cluster-scoped apps.
	Namespace string
	Phase     Phase
	Wave      int
	DependsOn []string
	Objects   []*Object
	Release   *HelmRelease
	// SkipDryRun is set when the app contains custom resources whose CRD is
	// installed by another app, so a dry run would fail on an empty cluster.
	SkipDryRun bool
	Notes      []string
}

// IsHelm reports whether the app deploys a Helm chart.
func (a *App) IsHelm() bool {
	return a.Kind == AppHelm && a.Release != nil && a.Release.Mode != HelmModeManifests
}

// ClusterInfo describes the source cluster.
type ClusterInfo struct {
	Context string `json:"context,omitempty"`
	Server  string `json:"server,omitempty"`
	Version string `json:"version,omitempty"`
}

// String describes the cluster for people: "context (server, Kubernetes v)".
func (c ClusterInfo) String() string {
	s := c.Context
	switch {
	case c.Server != "" && c.Version != "":
		s += " (" + c.Server + ", Kubernetes " + c.Version + ")"
	case c.Server != "":
		s += " (" + c.Server + ")"
	}
	return s
}

// Inventory is everything gitopsify knows about the source cluster.
type Inventory struct {
	Source   ClusterInfo
	Objects  []*Object
	Releases []*HelmRelease
	Apps     []*App
	Warnings []string
	// Secrets that need action from the user, as "<namespace>/<name>: detail".
	SecretActions []string

	index map[Ref]*Object
}

// Add appends objects and indexes them.
func (inv *Inventory) Add(objs ...*Object) {
	if inv.index == nil {
		inv.index = map[Ref]*Object{}
	}
	for _, o := range objs {
		inv.Objects = append(inv.Objects, o)
		inv.index[o.Ref] = o
	}
}

// Get returns the object with the given ref.
func (inv *Inventory) Get(r Ref) *Object {
	if inv.index == nil {
		inv.Reindex()
	}
	return inv.index[r]
}

// Reindex rebuilds the ref index after objects were replaced.
func (inv *Inventory) Reindex() {
	inv.index = make(map[Ref]*Object, len(inv.Objects))
	for _, o := range inv.Objects {
		inv.index[o.Ref] = o
	}
}

// Included returns the objects that will be written, in a stable order.
func (inv *Inventory) Included() []*Object {
	var out []*Object
	for _, o := range inv.Objects {
		if !o.Excluded {
			out = append(out, o)
		}
	}
	SortObjects(out)
	return out
}

// Excluded returns the excluded objects, in a stable order.
func (inv *Inventory) Excluded() []*Object {
	var out []*Object
	for _, o := range inv.Objects {
		if o.Excluded {
			out = append(out, o)
		}
	}
	SortObjects(out)
	return out
}

// App returns the app with the given name.
func (inv *Inventory) App(name string) *App {
	for _, a := range inv.Apps {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// Release returns the release with the given "<namespace>/<name>" key.
func (inv *Inventory) Release(key string) *HelmRelease {
	for _, r := range inv.Releases {
		if r.Key() == key {
			return r
		}
	}
	return nil
}

// Warn records an inventory-wide warning.
func (inv *Inventory) Warn(format string, args ...any) {
	inv.Warnings = append(inv.Warnings, fmt.Sprintf(format, args...))
}

// SortObjects sorts objects by namespace, group, kind and name.
func SortObjects(objs []*Object) {
	sort.SliceStable(objs, func(i, j int) bool {
		a, b := objs[i].Ref, objs[j].Ref
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
}

var dockercfgSuffix = regexp.MustCompile(`-dockercfg-[a-z0-9]{5}$`)

// GeneratedPullSecret reports whether a ServiceAccount's image pull secret was
// generated for it by OpenShift (<serviceaccount>-dockercfg-xxxxx).
func GeneratedPullSecret(serviceAccount, secret string) bool {
	return strings.HasPrefix(secret, serviceAccount+"-dockercfg-") && dockercfgSuffix.MatchString(secret)
}

// AllWarnings returns the inventory's warnings with those of its Helm
// releases and included objects, as the migration report lists them.
func (inv *Inventory) AllWarnings() []string {
	out := append([]string(nil), inv.Warnings...)
	for _, rel := range inv.Releases {
		if rel.Skip {
			continue
		}
		for _, w := range rel.Warnings {
			out = append(out, "Helm release "+rel.Key()+": "+w)
		}
	}
	for _, o := range inv.Included() {
		for _, w := range o.Warnings {
			out = append(out, o.Ref.String()+": "+w)
		}
	}
	return out
}
