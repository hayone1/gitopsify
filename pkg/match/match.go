// Package match implements the glob, kind and selector matching used by
// scope rules, transforms and overrides.
package match

import (
	"path"
	"strings"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/model"
)

// Glob reports whether s matches any of the shell-style patterns. An empty
// pattern list matches nothing.
func Glob(patterns []string, s string) bool {
	for _, p := range patterns {
		if p == s {
			return true
		}
		if ok, err := path.Match(p, s); err == nil && ok {
			return true
		}
	}
	return false
}

// GlobOrEmpty is Glob, except that an empty pattern list matches everything.
func GlobOrEmpty(patterns []string, s string) bool {
	return len(patterns) == 0 || Glob(patterns, s)
}

// KindInfo is what a kind pattern can match against.
type KindInfo struct {
	Group      string
	Kind       string
	Resource   string
	ShortNames []string
}

// shortNames are kubectl's short names of built-in kinds, used when discovery
// didn't provide them (offline sources).
var shortNames = map[string][]string{
	"Pod": {"po"}, "Service": {"svc"}, "ConfigMap": {"cm"}, "Namespace": {"ns"},
	"ServiceAccount": {"sa"}, "PersistentVolumeClaim": {"pvc"}, "PersistentVolume": {"pv"},
	"Endpoints": {"ep"}, "Node": {"no"}, "ReplicationController": {"rc"}, "LimitRange": {"limits"},
	"ResourceQuota": {"quota"}, "Deployment": {"deploy"}, "DaemonSet": {"ds"}, "ReplicaSet": {"rs"},
	"StatefulSet": {"sts"}, "CronJob": {"cj"}, "Ingress": {"ing"}, "HorizontalPodAutoscaler": {"hpa"},
	"PodDisruptionBudget": {"pdb"}, "NetworkPolicy": {"netpol"}, "CustomResourceDefinition": {"crd", "crds"},
	"StorageClass": {"sc"}, "PriorityClass": {"pc"}, "CertificateSigningRequest": {"csr"},
}

// Kind reports whether the object kind matches a pattern. A pattern is a kind,
// a plural resource or a short name, optionally followed by ".group", and is
// case-insensitive: "Deployment", "deployments", "deploy",
// "Ingress.networking.k8s.io".
func Kind(pattern string, k KindInfo) bool {
	p := strings.ToLower(pattern)
	name, group, hasGroup := strings.Cut(p, ".")
	if hasGroup && group != strings.ToLower(k.Group) {
		// "core" and "v1" both mean the core group.
		if !(k.Group == "" && (group == "core" || group == "v1")) {
			return false
		}
	}
	if name == strings.ToLower(k.Kind) || name == strings.ToLower(k.Resource) {
		return true
	}
	names := k.ShortNames
	if len(names) == 0 {
		names = shortNames[k.Kind]
	}
	for _, s := range names {
		if name == strings.ToLower(s) {
			return true
		}
	}
	return false
}

// AnyKind reports whether any pattern matches the kind.
func AnyKind(patterns []string, k KindInfo) bool {
	for _, p := range patterns {
		if Kind(p, k) {
			return true
		}
	}
	return false
}

// InfoOf returns the KindInfo of an object.
func InfoOf(o *model.Object) KindInfo {
	return KindInfo{Group: o.Ref.Group, Kind: o.Ref.Kind, Resource: o.GVR.Resource}
}

// Selector reports whether a selector matches an object. app is the name of
// the app the object is in, which may be empty before grouping.
func Selector(s config.Selector, o *model.Object, app string) (bool, error) {
	if len(s.Kinds) > 0 && !AnyKind(s.Kinds, InfoOf(o)) {
		return false, nil
	}
	if len(s.Namespaces) > 0 && !Glob(s.Namespaces, o.Ref.Namespace) {
		return false, nil
	}
	if len(s.Names) > 0 && !Glob(s.Names, o.Ref.Name) {
		return false, nil
	}
	if len(s.Apps) > 0 && !Glob(s.Apps, app) {
		return false, nil
	}
	if s.LabelSelector != "" {
		sel, err := labels.Parse(s.LabelSelector)
		if err != nil {
			return false, err
		}
		if !sel.Matches(labels.Set(o.Live.GetLabels())) {
			return false, nil
		}
	}
	return true, nil
}

// SelectorOrAll is Selector, but a nil selector matches everything.
func SelectorOrAll(s *config.Selector, o *model.Object, app string) (bool, error) {
	if s == nil {
		return true, nil
	}
	return Selector(*s, o, app)
}
