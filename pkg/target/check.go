package target

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/discovery"
	"github.com/hayone1/gitopsify/pkg/kube"
	"github.com/hayone1/gitopsify/pkg/model"
)

// Severity of a finding.
type Severity string

// Severities.
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

// Finding is one result of a preflight check.
type Finding struct {
	Severity Severity `json:"severity"`
	Check    string   `json:"check"`
	Subject  string   `json:"subject,omitempty"`
	Message  string   `json:"message"`
}

// Check runs preflight checks of an environment's cluster against what will
// be deployed to it.
func Check(ctx context.Context, inv *model.Inventory, cfg *config.Config, env *config.Environment, c *kube.Client) ([]Finding, error) {
	var out []Finding
	add := func(s Severity, check, subject, format string, args ...any) {
		out = append(out, Finding{Severity: s, Check: check, Subject: subject, Message: fmt.Sprintf(format, args...)})
	}

	version, err := c.ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", c.Server, err)
	}
	add(SeverityInfo, "cluster", c.Context, "Kubernetes %s at %s (source: %s)", version, c.Server, inv.Source.Version)
	if olderMinor(version, inv.Source.Version) {
		add(SeverityWarning, "cluster", c.Context, "the target runs an older Kubernetes version (%s) than the source (%s); APIs and defaults may differ", version, inv.Source.Version)
	}

	resources, warnings, err := discovery.APIResources(c)
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		add(SeverityWarning, "discovery", "", "%s", w)
	}
	served := map[schema.GroupVersionKind]bool{}
	kindVersions := map[schema.GroupKind][]string{}
	for _, r := range resources {
		served[r.GVR.GroupVersion().WithKind(r.Kind)] = true
	}
	// Preferred resources list one version per group; collect every served
	// version of each kind too.
	_, all, _ := c.Discovery.ServerGroupsAndResources()
	for _, l := range all {
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range l.APIResources {
			if strings.Contains(r.Name, "/") {
				continue
			}
			served[gv.WithKind(r.Kind)] = true
			gk := schema.GroupKind{Group: gv.Group, Kind: r.Kind}
			kindVersions[gk] = append(kindVersions[gk], gv.Version)
		}
	}

	objs, err := Objects(inv, cfg, env)
	if err != nil {
		return nil, err
	}
	migratedCRDs := migratedKinds(inv)

	// API availability.
	reported := map[schema.GroupVersionKind]bool{}
	for _, eo := range objs {
		gvk := eo.Object.GroupVersionKind()
		if served[gvk] || reported[gvk] {
			continue
		}
		reported[gvk] = true
		gk := gvk.GroupKind()
		switch {
		case migratedCRDs[gk] != "":
			add(SeverityInfo, "api", gvk.String(), "not served yet; its CRD is installed by app %s", migratedCRDs[gk])
		case len(kindVersions[gk]) > 0:
			add(SeverityError, "api", gvk.String(), "version %s is not served by the target, which serves %s; update the manifests (app %s)", gvk.Version, strings.Join(kindVersions[gk], ", "), eo.App.Name)
		default:
			add(SeverityError, "api", gvk.String(), "kind is not served by the target and no migrated app installs its CRD (app %s); install the operator first or exclude it", eo.App.Name)
		}
	}

	// Referenced cluster-level classes.
	migratedNames := map[string]bool{}
	for _, o := range inv.Included() {
		migratedNames[o.Ref.GroupKind()+"/"+o.Ref.Name] = true
	}
	// Chart objects come from the release's recorded manifest: transforms
	// map classes in the chart's values, which that manifest predates.
	mapped := map[string]bool{}
	for _, ts := range [][]config.Transform{cfg.Transforms, env.Transforms} {
		for _, t := range ts {
			if t.Type == "storageClass" || t.Type == "ingressClass" {
				for from := range t.Map {
					mapped[t.Type+"/"+from] = true
				}
			}
		}
	}
	var fromChart bool
	checkRef := func(check, group, version, resource, kind, name, user string) {
		if name == "" || migratedNames[kind+"."+group+"/"+name] || migratedNames[kind+"/"+name] {
			return
		}
		transform := strings.ToLower(kind[:1]) + kind[1:]
		if fromChart && mapped[transform+"/"+name] {
			return // the chart's values are mapped
		}
		_, err := c.Dynamic.Resource(schema.GroupVersionResource{Group: group, Version: version, Resource: resource}).Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
		case fromChart:
			add(SeverityWarning, check, user, "%s %q does not exist in the target, as the release's recorded manifest has it; if the chart's values don't set it otherwise, map it with a %s transform or create it (diff --helm-template renders the generated values)", kind, name, transform)
		default:
			add(SeverityError, check, user, "%s %q does not exist in the target (map it with a %s transform or create it)", kind, name, transform)
		}
	}
	for _, eo := range objs {
		u := eo.Object
		fromChart = eo.FromChart
		subject := refString(u)
		switch u.GroupVersionKind().GroupKind().String() {
		case "PersistentVolumeClaim":
			sc, _, _ := unstructured.NestedString(u.Object, "spec", "storageClassName")
			checkRef("storageclass", "storage.k8s.io", "v1", "storageclasses", "StorageClass", sc, subject)
		case "StatefulSet.apps":
			tpls, _, _ := unstructured.NestedSlice(u.Object, "spec", "volumeClaimTemplates")
			for _, t := range tpls {
				m, _ := t.(map[string]any)
				sc, _, _ := unstructured.NestedString(m, "spec", "storageClassName")
				checkRef("storageclass", "storage.k8s.io", "v1", "storageclasses", "StorageClass", sc, subject)
			}
		case "Ingress.networking.k8s.io":
			ic, _, _ := unstructured.NestedString(u.Object, "spec", "ingressClassName")
			checkRef("ingressclass", "networking.k8s.io", "v1", "ingressclasses", "IngressClass", ic, subject)
		}
	}

	// Existing objects and their owners.
	owners, refs := make([]string, len(objs)), make([]string, len(objs))
	exists := make([]bool, len(objs))
	parallel(ctx, len(objs), func(i int) {
		u := objs[i].Object.DeepCopy()
		if err := defaultNamespace(c, u, objs[i].DefaultNamespace); err != nil {
			return
		}
		live, err := c.Get(ctx, u)
		if err != nil || live == nil {
			return
		}
		exists[i], owners[i], refs[i] = true, ownerOf(live), refString(u)
	})
	existing := 0
	for i := range objs {
		if !exists[i] {
			continue
		}
		existing++
		if owners[i] != "" {
			add(SeverityWarning, "ownership", refs[i], "already exists and is managed by %s; the first sync takes it over", owners[i])
		}
	}
	if existing > 0 {
		msg := "%d of %d objects already exist in the target and will be adopted"
		if !cfg.Adopt.Enabled {
			add(SeverityWarning, "adoption", "", msg+"; consider --adopt for takeover-safe sync settings", existing, len(objs))
		} else {
			add(SeverityInfo, "adoption", "", msg, existing, len(objs))
		}
	}

	// Helm release records in the target.
	for _, a := range inv.Apps {
		if a.Release == nil {
			continue
		}
		secrets, err := c.Kubernetes.CoreV1().Secrets(a.Release.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "owner=helm,name=" + a.Release.Name})
		if err != nil || len(secrets.Items) == 0 {
			continue
		}
		switch {
		case cfg.Output.Tool == config.ToolFlux && a.IsHelm():
			add(SeverityInfo, "helm", a.Release.Key(), "release exists in the target; the HelmRelease upgrades it in place")
		default:
			add(SeverityWarning, "helm", a.Release.Key(), "release records exist in the target but the release will no longer be managed by Helm; run `gitopsify adopt` to back them up and remove them so `helm upgrade` can't fight the GitOps tool")
		}
	}

	// GitOps tool and secret controllers.
	switch cfg.Output.Tool {
	case config.ToolArgoCD:
		if !served[schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}] {
			add(SeverityInfo, "gitops", "", "Argo CD is not installed; `gitopsify bootstrap` installs it")
		}
	case config.ToolFlux:
		if !served[schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"}] {
			add(SeverityInfo, "gitops", "", "Flux is not installed; `gitopsify bootstrap` installs it")
		}
	}
	for _, need := range []struct{ group, kind, what string }{
		{"bitnami.com", "SealedSecret", "the sealed-secrets controller"},
		{"external-secrets.io", "ExternalSecret", "External Secrets Operator"},
	} {
		uses := false
		for _, eo := range objs {
			if eo.Object.GetKind() == need.kind && eo.Object.GroupVersionKind().Group == need.group {
				uses = true
				break
			}
		}
		gk := schema.GroupKind{Group: need.group, Kind: need.kind}
		if uses && len(kindVersions[gk]) == 0 && migratedCRDs[gk] == "" {
			add(SeverityError, "secrets", need.kind, "Secrets are written as %ss but %s is neither installed in the target nor migrated", need.kind, need.what)
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return rank(out[i].Severity) < rank(out[j].Severity) })
	return out, nil
}

func rank(s Severity) int {
	switch s {
	case SeverityError:
		return 0
	case SeverityWarning:
		return 1
	}
	return 2
}

// migratedKinds maps the kinds served by migrated CRDs to the app that
// installs them.
func migratedKinds(inv *model.Inventory) map[schema.GroupKind]string {
	out := map[schema.GroupKind]string{}
	add := func(u *unstructured.Unstructured, app string) {
		group, _, _ := unstructured.NestedString(u.Object, "spec", "group")
		kind, _, _ := unstructured.NestedString(u.Object, "spec", "names", "kind")
		if kind != "" {
			out[schema.GroupKind{Group: group, Kind: kind}] = app
		}
	}
	for _, a := range inv.Apps {
		for _, o := range a.Objects {
			if o.IsCRD() {
				add(o.Live, a.Name)
			}
		}
	}
	return out
}

func defaultNamespace(c *kube.Client, u *unstructured.Unstructured, def string) error {
	if u.GetNamespace() != "" {
		return nil
	}
	m, err := c.Mapping(u.GroupVersionKind())
	if err != nil {
		return err
	}
	if m.Scope.Name() == meta.RESTScopeNameNamespace {
		if def == "" {
			def = metav1.NamespaceDefault
		}
		u.SetNamespace(def)
	}
	return nil
}

// ownerOf describes who manages a live object, if anyone recognizable.
func ownerOf(u *unstructured.Unstructured) string {
	a, l := u.GetAnnotations(), u.GetLabels()
	switch {
	case a["argocd.argoproj.io/tracking-id"] != "":
		app, _, _ := strings.Cut(a["argocd.argoproj.io/tracking-id"], ":")
		return "Argo CD application " + app
	case l["kustomize.toolkit.fluxcd.io/name"] != "":
		return "Flux Kustomization " + l["kustomize.toolkit.fluxcd.io/namespace"] + "/" + l["kustomize.toolkit.fluxcd.io/name"]
	case l["helm.toolkit.fluxcd.io/name"] != "":
		return "Flux HelmRelease " + l["helm.toolkit.fluxcd.io/namespace"] + "/" + l["helm.toolkit.fluxcd.io/name"]
	case a["meta.helm.sh/release-name"] != "":
		return "Helm release " + a["meta.helm.sh/release-namespace"] + "/" + a["meta.helm.sh/release-name"]
	}
	return ""
}

func refString(u *unstructured.Unstructured) string {
	r := model.RefOf(u)
	return r.String()
}

// olderMinor reports whether version a is an older minor release than b.
func olderMinor(a, b string) bool {
	var amaj, amin, bmaj, bmin int
	if _, err := fmt.Sscanf(strings.TrimPrefix(a, "v"), "%d.%d", &amaj, &amin); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(strings.TrimPrefix(b, "v"), "%d.%d", &bmaj, &bmin); err != nil {
		return false
	}
	return amaj < bmaj || amaj == bmaj && amin < bmin
}
