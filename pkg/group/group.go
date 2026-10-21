// Package group splits the migrated objects into apps: one Argo CD
// Application or Flux Kustomization each.
package group

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/deps"
	"github.com/hayone1/gitopsify/pkg/match"
	"github.com/hayone1/gitopsify/pkg/model"
)

// Names of the apps that hold cluster-wide objects.
const (
	AppCRDs       = "crds"
	AppNamespaces = "namespaces"
	AppCluster    = "cluster"
)

// platformKinds make the app that contains them a platform app: other apps
// rely on what they provide.
var platformKinds = map[string]bool{
	"CustomResourceDefinition.apiextensions.k8s.io":               true,
	"ValidatingWebhookConfiguration.admissionregistration.k8s.io": true,
	"MutatingWebhookConfiguration.admissionregistration.k8s.io":   true,
	"APIService.apiregistration.k8s.io":                           true,
	"IngressClass.networking.k8s.io":                              true,
	"StorageClass.storage.k8s.io":                                 true,
	"GatewayClass.gateway.networking.k8s.io":                      true,
}

var invalidName = regexp.MustCompile(`[^a-z0-9-]+`)

// Name turns s into a DNS label usable as an app name.
func Name(s string) string {
	n := invalidName.ReplaceAllString(strings.ToLower(s), "-")
	n = strings.Trim(n, "-")
	if len(n) > 63 {
		n = strings.TrimRight(n[:63], "-")
	}
	if n == "" {
		n = "app"
	}
	return n
}

// Run groups the included objects of the inventory into apps.
func Run(inv *model.Inventory, cfg *config.Config) error {
	apps := map[string]*model.App{}
	get := func(name string, kind model.AppKind, ns string) *model.App {
		a, ok := apps[name]
		if !ok {
			a = &model.App{Name: name, Kind: kind, Namespace: ns}
			apps[name] = a
		}
		return a
	}

	// Helm releases first: their names win conflicts.
	releaseApp := map[string]string{}
	for _, r := range inv.Releases {
		if r.Skip {
			continue
		}
		name := Name(r.Name)
		if a, taken := apps[name]; taken && a.Release != nil {
			name = Name(r.Name + "-" + r.Namespace)
		}
		a := get(name, model.AppHelm, r.Namespace)
		a.Release = r
		releaseApp[r.Key()] = name
	}

	for _, o := range inv.Objects {
		if o.Excluded {
			continue
		}
		name, kind, ns, err := assign(o, cfg, releaseApp)
		if err != nil {
			return err
		}
		if a, taken := apps[name]; taken && a.Kind == model.AppHelm && o.HelmRelease == "" {
			// A namespace named like a release: keep the extra objects apart.
			name = Name(name + "-config")
		}
		a := get(name, kind, ns)
		o.App = a.Name
		a.Objects = append(a.Objects, o)
	}

	inv.Apps = inv.Apps[:0]
	for _, a := range apps {
		if a.Kind == model.AppHelm && a.Release == nil {
			a.Kind = model.AppManifests
		}
		model.SortObjects(a.Objects)
		if a.Kind == model.AppManifests {
			a.Namespace = commonNamespace(a.Objects)
		}
		a.Phase = phase(a)
		inv.Apps = append(inv.Apps, a)
	}
	sort.Slice(inv.Apps, func(i, j int) bool { return inv.Apps[i].Name < inv.Apps[j].Name })
	return nil
}

func assign(o *model.Object, cfg *config.Config, releaseApp map[string]string) (string, model.AppKind, string, error) {
	if o.HelmRelease != "" {
		if name, ok := releaseApp[o.HelmRelease]; ok {
			ns, _, _ := strings.Cut(o.HelmRelease, "/")
			return name, model.AppHelm, ns, nil
		}
	}
	for i, a := range cfg.Grouping.Apps {
		ok, err := match.Selector(a.Selector, o, "")
		if err != nil {
			return "", "", "", fmt.Errorf("grouping.apps[%d]: %w", i, err)
		}
		if ok {
			return a.Name, model.AppManifests, "", nil
		}
	}
	switch {
	case o.IsCRD():
		return AppCRDs, model.AppManifests, "", nil
	case o.IsNamespace():
		return AppNamespaces, model.AppManifests, "", nil
	case !o.Namespaced:
		return AppCluster, model.AppManifests, "", nil
	}
	switch cfg.Grouping.By {
	case config.GroupSingle:
		return "workloads", model.AppManifests, "", nil
	case config.GroupByLabel:
		if v := o.Live.GetLabels()[cfg.Grouping.Label]; v != "" {
			return Name(v), model.AppManifests, "", nil
		}
	}
	return Name(o.Ref.Namespace), model.AppManifests, o.Ref.Namespace, nil
}

// commonNamespace returns the namespace shared by all namespaced objects, or
// "" when they span several.
func commonNamespace(objs []*model.Object) string {
	ns := ""
	for _, o := range objs {
		if !o.Namespaced {
			continue
		}
		if ns != "" && ns != o.Ref.Namespace {
			return ""
		}
		ns = o.Ref.Namespace
	}
	return ns
}

func phase(a *model.App) model.Phase {
	switch a.Name {
	case AppCRDs:
		return model.PhaseCRDs
	case AppNamespaces:
		return model.PhaseNamespaces
	case AppCluster:
		return model.PhaseCluster
	}
	if a.Release != nil && len(a.Release.CRDs) > 0 {
		return model.PhasePlatform
	}
	onlyCRs := len(a.Objects) > 0
	for _, o := range a.Objects {
		if platformKinds[o.Ref.GroupKind()] {
			return model.PhasePlatform
		}
		if !deps.IsCustomResource(o) {
			onlyCRs = false
		}
	}
	if onlyCRs && a.Release == nil {
		return model.PhaseCustomResources
	}
	return model.PhaseApps
}
