package discovery

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/hayone1/gitopsify/pkg/model"
)

// builtin lists the resource name and scope of built-in kinds, which offline
// sources can't discover.
var builtin = map[string]struct {
	resource   string
	namespaced bool
}{
	"Pod": {"pods", true}, "Service": {"services", true}, "ConfigMap": {"configmaps", true},
	"Secret": {"secrets", true}, "ServiceAccount": {"serviceaccounts", true},
	"PersistentVolumeClaim": {"persistentvolumeclaims", true}, "PersistentVolume": {"persistentvolumes", false},
	"Namespace": {"namespaces", false}, "Node": {"nodes", false}, "Endpoints": {"endpoints", true},
	"LimitRange": {"limitranges", true}, "ResourceQuota": {"resourcequotas", true},
	"ReplicationController": {"replicationcontrollers", true},
	"Deployment.apps":       {"deployments", true}, "StatefulSet.apps": {"statefulsets", true},
	"DaemonSet.apps": {"daemonsets", true}, "ReplicaSet.apps": {"replicasets", true},
	"ControllerRevision.apps": {"controllerrevisions", true},
	"Job.batch":               {"jobs", true}, "CronJob.batch": {"cronjobs", true},
	"HorizontalPodAutoscaler.autoscaling": {"horizontalpodautoscalers", true},
	"PodDisruptionBudget.policy":          {"poddisruptionbudgets", true},
	"Ingress.networking.k8s.io":           {"ingresses", true}, "IngressClass.networking.k8s.io": {"ingressclasses", false},
	"NetworkPolicy.networking.k8s.io": {"networkpolicies", true},
	"Role.rbac.authorization.k8s.io":  {"roles", true}, "RoleBinding.rbac.authorization.k8s.io": {"rolebindings", true},
	"ClusterRole.rbac.authorization.k8s.io": {"clusterroles", false}, "ClusterRoleBinding.rbac.authorization.k8s.io": {"clusterrolebindings", false},
	"StorageClass.storage.k8s.io": {"storageclasses", false}, "CSIDriver.storage.k8s.io": {"csidrivers", false},
	"CSINode.storage.k8s.io": {"csinodes", false}, "VolumeAttachment.storage.k8s.io": {"volumeattachments", false},
	"CustomResourceDefinition.apiextensions.k8s.io":               {"customresourcedefinitions", false},
	"ValidatingWebhookConfiguration.admissionregistration.k8s.io": {"validatingwebhookconfigurations", false},
	"MutatingWebhookConfiguration.admissionregistration.k8s.io":   {"mutatingwebhookconfigurations", false},
	"PriorityClass.scheduling.k8s.io":                             {"priorityclasses", false}, "Lease.coordination.k8s.io": {"leases", true},
	"EndpointSlice.discovery.k8s.io": {"endpointslices", true}, "APIService.apiregistration.k8s.io": {"apiservices", false},
	"RuntimeClass.node.k8s.io":                      {"runtimeclasses", false},
	"CertificateSigningRequest.certificates.k8s.io": {"certificatesigningrequests", false},
}

// FromObjects builds a discovery result from objects read offline, for
// example from `kubectl get -A -o yaml` output. Resource names and scopes of
// custom resources come from the CRDs among the objects when present.
func FromObjects(objs []*unstructured.Unstructured) *Result {
	type info struct {
		resource   string
		namespaced bool
	}
	crds := map[string]info{}
	for _, u := range objs {
		if u.GetKind() != "CustomResourceDefinition" {
			continue
		}
		group, _, _ := unstructured.NestedString(u.Object, "spec", "group")
		kind, _, _ := unstructured.NestedString(u.Object, "spec", "names", "kind")
		plural, _, _ := unstructured.NestedString(u.Object, "spec", "names", "plural")
		scope, _, _ := unstructured.NestedString(u.Object, "spec", "scope")
		crds[kind+"."+group] = info{resource: plural, namespaced: scope == "Namespaced"}
	}
	res := &Result{}
	seen := map[schema.GroupVersionResource]bool{}
	for _, u := range objs {
		gvk := u.GroupVersionKind()
		gk := gvk.GroupKind().String()
		var in info
		if b, ok := builtin[gk]; ok {
			in = info(b)
		} else if c, ok := crds[gk]; ok {
			in = c
		} else {
			in = info{resource: plural(gvk.Kind), namespaced: u.GetNamespace() != ""}
		}
		gvr := gvk.GroupVersion().WithResource(in.resource)
		if !seen[gvr] {
			seen[gvr] = true
			res.Resources = append(res.Resources, Resource{GVR: gvr, Kind: gvk.Kind, Namespaced: in.namespaced})
		}
		if !in.namespaced {
			unstructured.RemoveNestedField(u.Object, "metadata", "namespace")
		}
		res.Objects = append(res.Objects, &model.Object{Ref: model.RefOf(u), GVR: gvr, Namespaced: in.namespaced, Live: u})
	}
	model.SortObjects(res.Objects)
	sort.Slice(res.Resources, func(i, j int) bool { return res.Resources[i].GVR.String() < res.Resources[j].GVR.String() })
	return res
}

// plural guesses the resource name of a kind the way most CRDs name theirs.
func plural(kind string) string {
	k := strings.ToLower(kind)
	switch {
	case strings.HasSuffix(k, "s"), strings.HasSuffix(k, "x"), strings.HasSuffix(k, "ch"), strings.HasSuffix(k, "sh"):
		return k + "es"
	case strings.HasSuffix(k, "y") && len(k) > 1 && !strings.ContainsRune("aeiou", rune(k[len(k)-2])):
		return k[:len(k)-1] + "ies"
	}
	return k + "s"
}

// ReadObjects reads objects from YAML or JSON files and directories ("-"
// reads stdin). List kinds (v1 List, ...List) are flattened.
func ReadObjects(paths []string) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	add := func(name string, data []byte) error {
		for _, doc := range splitYAML(string(data)) {
			m := map[string]any{}
			if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if len(m) == 0 {
				continue
			}
			u := &unstructured.Unstructured{Object: m}
			if u.IsList() {
				items, _, _ := unstructured.NestedSlice(m, "items")
				for _, it := range items {
					if im, ok := it.(map[string]any); ok {
						out = append(out, &unstructured.Unstructured{Object: im})
					}
				}
				continue
			}
			if u.GetKind() != "" && u.GetName() != "" {
				out = append(out, u)
			}
		}
		return nil
	}
	for _, p := range paths {
		if p == "-" {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return nil, err
			}
			if err := add("stdin", data); err != nil {
				return nil, err
			}
			continue
		}
		err := filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".yaml", ".yml", ".json":
			default:
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return add(path, data)
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func splitYAML(s string) []string {
	var docs []string
	var cur []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "---") {
			docs = append(docs, strings.Join(cur, "\n"))
			cur = nil
			continue
		}
		cur = append(cur, l)
	}
	return append(docs, strings.Join(cur, "\n"))
}
