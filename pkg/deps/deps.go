// Package deps finds what migrated objects need: the ConfigMaps, Secrets,
// ServiceAccounts, RBAC, PVCs, CRDs and Namespaces they reference, and the
// Services, Ingresses, routes, autoscalers, disruption budgets and network
// policies attached to them.
package deps

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/filter"
	"github.com/hayone1/gitopsify/pkg/model"
)

// builtinGroups are API groups that are not served by CRDs.
var builtinGroups = map[string]bool{
	"": true, "apps": true, "batch": true, "autoscaling": true, "policy": true,
	"networking.k8s.io": true, "rbac.authorization.k8s.io": true, "storage.k8s.io": true,
	"apiextensions.k8s.io": true, "apiregistration.k8s.io": true, "scheduling.k8s.io": true,
	"coordination.k8s.io": true, "discovery.k8s.io": true, "node.k8s.io": true,
	"admissionregistration.k8s.io": true, "certificates.k8s.io": true, "events.k8s.io": true,
	"flowcontrol.apiserver.k8s.io": true, "resource.k8s.io": true, "authentication.k8s.io": true,
	"authorization.k8s.io": true, "storagemigration.k8s.io": true, "internal.apiserver.k8s.io": true,
}

// IsCustomResource reports whether an object's group is served by a CRD.
func IsCustomResource(o *model.Object) bool {
	return !builtinGroups[o.Ref.Group]
}

// CRDName returns the CRD name that serves an object's kind.
func CRDName(o *model.Object) string {
	return o.GVR.Resource + "." + o.Ref.Group
}

// Resolve includes the dependencies of migrated objects that were excluded
// only by scope. In selection mode every dependency is pulled in; otherwise
// only cluster-scoped ones (Namespaces, CRDs, ClusterRoles, PVs, ...) are,
// since whole namespaces are already migrated.
func Resolve(inv *model.Inventory, cfg *config.Config) {
	if !config.True(cfg.Scope.IncludeDependencies, true) && filter.SelectionMode(cfg) {
		return
	}
	g := newGraph(inv)
	selection := filter.SelectionMode(cfg)
	// In selection mode the included objects are exactly the selected ones
	// and the objects of selected Helm releases.
	var queue []*model.Object
	for _, o := range inv.Objects {
		if !o.Excluded {
			queue = append(queue, o)
		}
	}
	seen := map[*model.Object]bool{}
	for len(queue) > 0 {
		o := queue[0]
		queue = queue[1:]
		if seen[o] {
			continue
		}
		seen[o] = true
		for _, d := range g.of(o) {
			if !d.Excluded {
				queue = append(queue, d)
				continue
			}
			if !filter.IsScopeExclusion(d) || (!selection && d.Namespaced) {
				continue
			}
			if d.HelmRelease != "" {
				o.Warn("depends on %s, which belongs to Helm release %s that is not migrated", d.Ref, d.HelmRelease)
				continue
			}
			d.Include()
			queue = append(queue, d)
		}
		if selection && config.True(cfg.Scope.IncludeDependencies, true) {
			for _, ref := range g.missing(o) {
				o.Warn("references %s, which does not exist in the source cluster", ref)
			}
		}
	}
}

type graph struct {
	inv *model.Inventory
	// byNS indexes objects by namespace for selector lookups.
	byNS map[string][]*model.Object
}

func newGraph(inv *model.Inventory) *graph {
	g := &graph{inv: inv, byNS: map[string][]*model.Object{}}
	for _, o := range inv.Objects {
		g.byNS[o.Ref.Namespace] = append(g.byNS[o.Ref.Namespace], o)
	}
	return g
}

// of returns the objects o depends on or that are attached to it.
func (g *graph) of(o *model.Object) []*model.Object {
	var out []*model.Object
	add := func(d *model.Object) {
		if d != nil && d != o {
			out = append(out, d)
		}
	}
	for _, r := range g.refs(o) {
		add(g.inv.Get(r))
	}
	for _, d := range g.attached(o) {
		add(d)
	}
	return out
}

// missing returns references of o that point to nothing.
func (g *graph) missing(o *model.Object) []model.Ref {
	var out []model.Ref
	for _, r := range g.refs(o) {
		if r.Kind == "ServiceAccount" && r.Name == "default" {
			continue
		}
		if g.inv.Get(r) == nil && r.Kind != "Namespace" {
			out = append(out, r)
		}
	}
	return out
}

// refs returns the objects o references by name.
func (g *graph) refs(o *model.Object) []model.Ref {
	u := o.Live
	ns := o.Ref.Namespace
	var refs []model.Ref
	ref := func(group, kind, namespace, name string) {
		if name != "" {
			refs = append(refs, model.Ref{Group: group, Kind: kind, Namespace: namespace, Name: name})
		}
	}
	if ns != "" {
		ref("", "Namespace", "", ns)
	}
	if IsCustomResource(o) {
		ref("apiextensions.k8s.io", "CustomResourceDefinition", "", CRDName(o))
	}
	if spec := PodSpec(u); spec != nil {
		podSpecRefs(spec, ns, ref)
	}
	switch o.Ref.GroupKind() {
	case "StatefulSet.apps":
		s, _, _ := unstructured.NestedString(u.Object, "spec", "serviceName")
		ref("", "Service", ns, s)
		tpls, _, _ := unstructured.NestedSlice(u.Object, "spec", "volumeClaimTemplates")
		for _, t := range tpls {
			sc, _, _ := unstructured.NestedString(asMap(t), "spec", "storageClassName")
			ref("storage.k8s.io", "StorageClass", "", sc)
		}
	case "ServiceAccount":
		for _, s := range nameList(u.Object, "imagePullSecrets") {
			ref("", "Secret", ns, s)
		}
	case "RoleBinding.rbac.authorization.k8s.io", "ClusterRoleBinding.rbac.authorization.k8s.io":
		kind, _, _ := unstructured.NestedString(u.Object, "roleRef", "kind")
		name, _, _ := unstructured.NestedString(u.Object, "roleRef", "name")
		if kind == "Role" {
			ref("rbac.authorization.k8s.io", "Role", ns, name)
		} else {
			ref("rbac.authorization.k8s.io", "ClusterRole", "", name)
		}
	case "PersistentVolumeClaim":
		v, _, _ := unstructured.NestedString(u.Object, "spec", "volumeName")
		ref("", "PersistentVolume", "", v)
		sc, _, _ := unstructured.NestedString(u.Object, "spec", "storageClassName")
		ref("storage.k8s.io", "StorageClass", "", sc)
	case "Ingress.networking.k8s.io":
		for _, s := range IngressServices(u) {
			ref("", "Service", ns, s)
		}
		tls, _, _ := unstructured.NestedSlice(u.Object, "spec", "tls")
		for _, t := range tls {
			s, _, _ := unstructured.NestedString(asMap(t), "secretName")
			ref("", "Secret", ns, s)
		}
		ic, _, _ := unstructured.NestedString(u.Object, "spec", "ingressClassName")
		ref("networking.k8s.io", "IngressClass", "", ic)
	case "Secret":
		if t, _, _ := unstructured.NestedString(u.Object, "type"); t == "kubernetes.io/service-account-token" {
			ref("", "ServiceAccount", ns, u.GetAnnotations()["kubernetes.io/service-account.name"])
		}
	}
	// Autoscalers and similar point at their target.
	for _, path := range [][]string{{"spec", "scaleTargetRef"}, {"spec", "targetRef"}} {
		t, found, _ := unstructured.NestedMap(u.Object, path...)
		if !found {
			continue
		}
		kind, _ := t["kind"].(string)
		name, _ := t["name"].(string)
		group := ""
		if av, _ := t["apiVersion"].(string); strings.Contains(av, "/") {
			group = av[:strings.LastIndex(av, "/")]
		} else if kind == "Deployment" || kind == "StatefulSet" || kind == "ReplicaSet" || kind == "DaemonSet" {
			group = "apps"
		}
		ref(group, kind, ns, name)
	}
	return refs
}

func podSpecRefs(spec map[string]any, ns string, ref func(group, kind, namespace, name string)) {
	sa, _, _ := unstructured.NestedString(spec, "serviceAccountName")
	if sa == "" {
		sa = "default"
	}
	ref("", "ServiceAccount", ns, sa)
	for _, s := range nameList(spec, "imagePullSecrets") {
		ref("", "Secret", ns, s)
	}
	pc, _, _ := unstructured.NestedString(spec, "priorityClassName")
	ref("scheduling.k8s.io", "PriorityClass", "", pc)
	rc, _, _ := unstructured.NestedString(spec, "runtimeClassName")
	ref("node.k8s.io", "RuntimeClass", "", rc)

	vols, _, _ := unstructured.NestedSlice(spec, "volumes")
	for _, v := range vols {
		vm := asMap(v)
		cm, _, _ := unstructured.NestedString(vm, "configMap", "name")
		ref("", "ConfigMap", ns, cm)
		sec, _, _ := unstructured.NestedString(vm, "secret", "secretName")
		ref("", "Secret", ns, sec)
		pvc, _, _ := unstructured.NestedString(vm, "persistentVolumeClaim", "claimName")
		ref("", "PersistentVolumeClaim", ns, pvc)
		sources, _, _ := unstructured.NestedSlice(vm, "projected", "sources")
		for _, s := range sources {
			sm := asMap(s)
			cm, _, _ := unstructured.NestedString(sm, "configMap", "name")
			ref("", "ConfigMap", ns, cm)
			sec, _, _ := unstructured.NestedString(sm, "secret", "name")
			ref("", "Secret", ns, sec)
		}
	}
	for _, field := range []string{"containers", "initContainers", "ephemeralContainers"} {
		cs, _, _ := unstructured.NestedSlice(spec, field)
		for _, c := range cs {
			cm := asMap(c)
			envFrom, _, _ := unstructured.NestedSlice(cm, "envFrom")
			for _, e := range envFrom {
				em := asMap(e)
				n, _, _ := unstructured.NestedString(em, "configMapRef", "name")
				ref("", "ConfigMap", ns, n)
				n, _, _ = unstructured.NestedString(em, "secretRef", "name")
				ref("", "Secret", ns, n)
			}
			env, _, _ := unstructured.NestedSlice(cm, "env")
			for _, e := range env {
				em := asMap(e)
				n, _, _ := unstructured.NestedString(em, "valueFrom", "configMapKeyRef", "name")
				ref("", "ConfigMap", ns, n)
				n, _, _ = unstructured.NestedString(em, "valueFrom", "secretKeyRef", "name")
				ref("", "Secret", ns, n)
			}
		}
	}
}

// attached returns objects that select or route to o.
func (g *graph) attached(o *model.Object) []*model.Object {
	var out []*model.Object
	ns := o.Ref.Namespace
	switch {
	case PodSpec(o.Live) != nil && o.Ref.Kind != "Pod":
		podLabels := PodTemplateLabels(o.Live)
		sa := "default"
		if spec := PodSpec(o.Live); spec != nil {
			if s, _, _ := unstructured.NestedString(spec, "serviceAccountName"); s != "" {
				sa = s
			}
		}
		for _, c := range g.byNS[ns] {
			switch c.Ref.GroupKind() {
			case "Service":
				sel, _, _ := unstructured.NestedStringMap(c.Live.Object, "spec", "selector")
				if len(sel) > 0 && labels.SelectorFromSet(sel).Matches(labels.Set(podLabels)) {
					out = append(out, c)
				}
			case "PodDisruptionBudget.policy", "PodMonitor.monitoring.coreos.com":
				if selectorMatches(c.Live.Object, podLabels, "spec", "selector") {
					out = append(out, c)
				}
			case "NetworkPolicy.networking.k8s.io":
				if selectorMatches(c.Live.Object, podLabels, "spec", "podSelector") {
					out = append(out, c)
				}
			}
			if targets(c, o) {
				out = append(out, c)
			}
		}
		out = append(out, g.bindingsFor(ns, sa)...)
	case o.Ref.GroupKind() == "Service":
		svcLabels := o.Live.GetLabels()
		for _, c := range g.byNS[ns] {
			switch c.Ref.GroupKind() {
			case "Ingress.networking.k8s.io":
				for _, s := range IngressServices(c.Live) {
					if s == o.Ref.Name {
						out = append(out, c)
						break
					}
				}
			case "Endpoints", "EndpointSlice.discovery.k8s.io":
				if c.Ref.Name == o.Ref.Name || c.Live.GetLabels()["kubernetes.io/service-name"] == o.Ref.Name {
					out = append(out, c)
				}
			case "ServiceMonitor.monitoring.coreos.com":
				if selectorMatches(c.Live.Object, svcLabels, "spec", "selector") {
					out = append(out, c)
				}
			}
			if c.Ref.Group == "gateway.networking.k8s.io" && strings.HasSuffix(c.Ref.Kind, "Route") && routesTo(c.Live, o.Ref.Name) {
				out = append(out, c)
			}
		}
	case o.Ref.Group == "" && o.Ref.Kind == "ServiceAccount":
		out = append(out, g.bindingsFor(ns, o.Ref.Name)...)
	}
	return out
}

// bindingsFor returns the RoleBindings and ClusterRoleBindings that grant
// permissions to a ServiceAccount.
func (g *graph) bindingsFor(ns, sa string) []*model.Object {
	var out []*model.Object
	check := func(c *model.Object) {
		subjects, _, _ := unstructured.NestedSlice(c.Live.Object, "subjects")
		for _, s := range subjects {
			m := asMap(s)
			sns, _ := m["namespace"].(string)
			if sns == "" {
				sns = c.Ref.Namespace
			}
			if m["kind"] == "ServiceAccount" && m["name"] == sa && sns == ns {
				out = append(out, c)
				return
			}
		}
	}
	for _, c := range g.byNS[ns] {
		if c.Ref.GroupKind() == "RoleBinding.rbac.authorization.k8s.io" {
			check(c)
		}
	}
	for _, c := range g.byNS[""] {
		if c.Ref.GroupKind() == "ClusterRoleBinding.rbac.authorization.k8s.io" {
			check(c)
		}
	}
	return out
}

func targets(c, o *model.Object) bool {
	for _, path := range [][]string{{"spec", "scaleTargetRef"}, {"spec", "targetRef"}} {
		t, found, _ := unstructured.NestedMap(c.Live.Object, path...)
		if found && t["kind"] == o.Ref.Kind && t["name"] == o.Ref.Name {
			return true
		}
	}
	return false
}

func routesTo(u *unstructured.Unstructured, svc string) bool {
	rules, _, _ := unstructured.NestedSlice(u.Object, "spec", "rules")
	for _, r := range rules {
		backends, _, _ := unstructured.NestedSlice(asMap(r), "backendRefs")
		for _, b := range backends {
			bm := asMap(b)
			kind, _ := bm["kind"].(string)
			if (kind == "" || kind == "Service") && bm["name"] == svc {
				return true
			}
		}
	}
	return false
}

func selectorMatches(obj map[string]any, l map[string]string, path ...string) bool {
	sel, found, _ := unstructured.NestedMap(obj, path...)
	if !found {
		return false
	}
	ls, err := labelSelector(sel)
	if err != nil {
		return false
	}
	return ls.Matches(labels.Set(l))
}

func labelSelector(sel map[string]any) (labels.Selector, error) {
	s := labels.NewSelector()
	ml, _, _ := unstructured.NestedStringMap(sel, "matchLabels")
	for k, v := range ml {
		r, err := labels.NewRequirement(k, "=", []string{v})
		if err != nil {
			return nil, err
		}
		s = s.Add(*r)
	}
	exprs, _, _ := unstructured.NestedSlice(sel, "matchExpressions")
	for _, e := range exprs {
		em := asMap(e)
		key, _ := em["key"].(string)
		op, _ := em["operator"].(string)
		vals, _, _ := unstructured.NestedStringSlice(em, "values")
		var lop selection.Operator
		switch op {
		case "In":
			lop = selection.In
		case "NotIn":
			lop = selection.NotIn
		case "Exists":
			lop = selection.Exists
		case "DoesNotExist":
			lop = selection.DoesNotExist
		default:
			return nil, fmt.Errorf("unknown operator %q", op)
		}
		r, err := labels.NewRequirement(key, lop, vals)
		if err != nil {
			return nil, err
		}
		s = s.Add(*r)
	}
	return s, nil
}

// PodSpec returns the pod spec of a workload, or nil.
func PodSpec(u *unstructured.Unstructured) map[string]any {
	path := PodSpecPath(u.GroupVersionKind().GroupKind().String())
	if path == nil {
		return nil
	}
	m, found, _ := unstructured.NestedMap(u.Object, path...)
	if !found {
		return nil
	}
	return m
}

// PodSpecPath returns the path of the pod spec of a workload kind ("Kind" or
// "Kind.group"), or nil.
func PodSpecPath(groupKind string) []string {
	switch groupKind {
	case "Pod":
		return []string{"spec"}
	case "Deployment.apps", "ReplicaSet.apps", "StatefulSet.apps", "DaemonSet.apps", "Job.batch", "ReplicationController":
		return []string{"spec", "template", "spec"}
	case "CronJob.batch":
		return []string{"spec", "jobTemplate", "spec", "template", "spec"}
	}
	return nil
}

// PodTemplateLabels returns the pod template labels of a workload.
func PodTemplateLabels(u *unstructured.Unstructured) map[string]string {
	path := PodSpecPath(u.GroupVersionKind().GroupKind().String())
	if len(path) < 2 {
		return u.GetLabels()
	}
	meta := append(append([]string(nil), path[:len(path)-1]...), "metadata", "labels")
	l, _, _ := unstructured.NestedStringMap(u.Object, meta...)
	return l
}

// IngressServices returns the Services an Ingress routes to.
func IngressServices(u *unstructured.Unstructured) []string {
	var out []string
	if s, _, _ := unstructured.NestedString(u.Object, "spec", "defaultBackend", "service", "name"); s != "" {
		out = append(out, s)
	}
	rules, _, _ := unstructured.NestedSlice(u.Object, "spec", "rules")
	for _, r := range rules {
		paths, _, _ := unstructured.NestedSlice(asMap(r), "http", "paths")
		for _, p := range paths {
			if s, _, _ := unstructured.NestedString(asMap(p), "backend", "service", "name"); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func nameList(obj map[string]any, field string) []string {
	items, _, _ := unstructured.NestedSlice(obj, field)
	var out []string
	for _, i := range items {
		if n, _ := asMap(i)["name"].(string); n != "" {
			out = append(out, n)
		}
	}
	return out
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
