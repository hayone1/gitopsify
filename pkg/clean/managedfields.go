package clean

import (
	"encoding/json"
	"path"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// deployers are field managers of tools that apply what a person wrote. Their
// fields are user intent even when the manager name looks like a controller.
var deployers = []string{
	"kubectl", "kubectl-*", "helm", "argocd-controller", "argocd-application-controller",
	"kustomize-controller", "helm-controller", "flux-client-side-apply", "before-first-apply",
	"gitopsify", "terraform*", "Terraform", "pulumi*", "kapp", "Mozilla", "dashboard", "rancher",
	"OpenLens", "Lens", "k9s", "skaffold", "tilt", "kpt", "crossplane-kubernetes-provider",
}

// controllers are field managers of controllers that mutate objects after
// they were created. Their fields are not user intent.
var controllers = []string{
	"kube-controller-manager", "kube-scheduler", "kubelet", "kube-apiserver", "kube-proxy",
	"*-controller", "*-controller-manager", "*-operator", "*operator*", "cainjector", "cert-manager*",
	"cluster-autoscaler", "vpa-*", "kyverno*", "gatekeeper*", "linkerd-*", "istio*",
	"metallb-*", "external-dns", "*-webhook",
}

func matchesAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, s); ok || p == s {
			return true
		}
	}
	return false
}

// IsController reports whether a field manager is a controller. Unknown
// managers count as users, which keeps their fields.
func IsController(manager string, extra []string) bool {
	if matchesAny(deployers, manager) {
		return false
	}
	return matchesAny(controllers, manager) || matchesAny(extra, manager)
}

type managedEntry struct {
	Manager     string         `json:"manager"`
	Operation   string         `json:"operation"`
	Subresource string         `json:"subresource"`
	FieldsV1    map[string]any `json:"fieldsV1"`
}

func managedEntries(u *unstructured.Unstructured) []managedEntry {
	raw, found, _ := unstructured.NestedSlice(u.Object, "metadata", "managedFields")
	if !found {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out []managedEntry
	_ = json.Unmarshal(data, &out)
	return out
}

// UserFields returns the union of the field sets owned by user (non-controller)
// field managers, and whether every one of those managers used server-side
// apply. Only Apply field sets exclude server-side defaults; Update field sets
// include whatever the object held after defaulting.
func UserFields(u *unstructured.Unstructured, extraControllers []string) (fields map[string]any, allApply bool, ok bool) {
	entries := managedEntries(u)
	if len(entries) == 0 {
		return nil, false, false
	}
	fields = map[string]any{}
	allApply = true
	found := false
	for _, e := range entries {
		if e.Subresource != "" || IsController(e.Manager, extraControllers) {
			continue
		}
		found = true
		if e.Operation != "Apply" {
			allApply = false
		}
		mergeFields(fields, e.FieldsV1)
	}
	return fields, allApply, found
}

// OwnsField reports whether a user field manager owns the field at path (map
// keys only, e.g. "spec", "ports").
func OwnsField(fields map[string]any, path ...string) bool {
	cur := fields
	for _, p := range path {
		next, ok := cur["f:"+p].(map[string]any)
		if !ok {
			return false
		}
		cur = next
	}
	return true
}

func mergeFields(dst, src map[string]any) {
	for k, v := range src {
		sv, sok := v.(map[string]any)
		dv, dok := dst[k].(map[string]any)
		switch {
		case sok && dok:
			mergeFields(dv, sv)
		case sok:
			cp := map[string]any{}
			mergeFields(cp, sv)
			dst[k] = cp
		default:
			dst[k] = v
		}
	}
}

// Prune keeps only the parts of v described by a FieldsV1 set.
func Prune(v any, fs map[string]any) any {
	if len(fs) == 0 {
		return v
	}
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, child := range fs {
			if !strings.HasPrefix(k, "f:") {
				continue
			}
			name := k[2:]
			val, exists := t[name]
			if !exists {
				continue
			}
			cfs, _ := child.(map[string]any)
			out[name] = Prune(val, cfs)
		}
		return out
	case []any:
		var out []any
		for i, item := range t {
			for k, child := range fs {
				if !listKeyMatches(k, i, item) {
					continue
				}
				cfs, _ := child.(map[string]any)
				pruned := Prune(item, cfs)
				// Keys of associative lists identify the item; keep them.
				if pm, ok := pruned.(map[string]any); ok && strings.HasPrefix(k, "k:") {
					if im, ok := item.(map[string]any); ok {
						var key map[string]any
						_ = json.Unmarshal([]byte(k[2:]), &key)
						for kk := range key {
							if _, has := pm[kk]; !has {
								pm[kk] = im[kk]
							}
						}
					}
				}
				out = append(out, pruned)
				break
			}
		}
		if out == nil {
			out = []any{}
		}
		return out
	default:
		return v
	}
}

func listKeyMatches(k string, index int, item any) bool {
	switch {
	case strings.HasPrefix(k, "k:"):
		var key map[string]any
		if err := json.Unmarshal([]byte(k[2:]), &key); err != nil {
			return false
		}
		im, ok := item.(map[string]any)
		if !ok {
			return false
		}
		for kk, kv := range key {
			if !jsonEqual(im[kk], kv) {
				return false
			}
		}
		return true
	case strings.HasPrefix(k, "v:"):
		var val any
		if err := json.Unmarshal([]byte(k[2:]), &val); err != nil {
			return false
		}
		return jsonEqual(item, val)
	case strings.HasPrefix(k, "i:"):
		return k[2:] == itoa(index)
	}
	return false
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}
