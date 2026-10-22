package clean

import (
	"encoding/json"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hayone1/gitopsify/pkg/model"
)

// caInjectionAnnotations mark objects whose caBundle is filled in by a
// controller (cert-manager's cainjector, OpenShift's service CA operator).
var caInjectionAnnotations = []string{
	"cert-manager.io/inject-ca-from",
	"cert-manager.io/inject-ca-from-secret",
	"cert-manager.io/inject-apiserver-ca",
	"service.beta.openshift.io/inject-cabundle",
}

type kindContext struct {
	// managedFields is set when the object was pruned to the fields user
	// field managers own, which already dropped allocated values.
	managedFields bool
	// heuristic enables removal of known server-side defaults.
	heuristic bool
	// lastApplied is the kubectl.kubernetes.io/last-applied-configuration
	// of the object, evidence of values a user chose.
	lastApplied map[string]any
	// hpaTarget is set for workloads whose replicas an HPA controls.
	hpaTarget bool
}

func cleanKind(u *unstructured.Unstructured, o *model.Object, kc kindContext) {
	obj := u.Object
	h := kc.heuristic
	switch o.Ref.GroupKind() {
	case "Service":
		cleanService(obj, kc)
	case "PersistentVolumeClaim":
		del(obj, "spec", "volumeName")
		if h {
			delIfEq(obj, "Filesystem", "spec", "volumeMode")
		}
	case "PersistentVolume":
		if ref, found, _ := unstructured.NestedMap(obj, "spec", "claimRef"); found {
			keep := map[string]any{}
			for _, k := range []string{"namespace", "name"} {
				if v, ok := ref[k]; ok {
					keep[k] = v
				}
			}
			_ = unstructured.SetNestedMap(obj, keep, "spec", "claimRef")
		}
		if h {
			delIfEq(obj, "Filesystem", "spec", "volumeMode")
		}
	case "ServiceAccount":
		// Token Secrets listed here are generated per cluster, and so are
		// OpenShift's registry pull secrets.
		del(obj, "secrets")
		if ips := asSlice(nested(obj, "imagePullSecrets")); len(ips) > 0 {
			var keep []any
			for _, s := range ips {
				m, _ := s.(map[string]any)
				name, _ := m["name"].(string)
				if !model.GeneratedPullSecret(u.GetName(), name) {
					keep = append(keep, s)
				}
			}
			setOrDelete(obj, "imagePullSecrets", keep)
		}
	case "Secret":
		if t, _, _ := unstructured.NestedString(obj, "type"); t == "kubernetes.io/service-account-token" {
			// The token controller fills these in for the target cluster.
			for _, k := range []string{"token", "ca.crt", "namespace"} {
				del(obj, "data", k)
			}
			pruneEmpty(obj, "data")
		}
	case "Namespace":
		del(obj, "spec")
	case "ClusterRole.rbac.authorization.k8s.io":
		// Aggregated ClusterRoles get their rules from the aggregation controller.
		if _, found, _ := unstructured.NestedMap(obj, "aggregationRule"); found {
			del(obj, "rules")
		}
	case "CustomResourceDefinition.apiextensions.k8s.io":
		if caInjected(u) {
			del(obj, "spec", "conversion", "webhook", "clientConfig", "caBundle")
		}
		if h {
			delIfEq(obj, map[string]any{"strategy": "None"}, "spec", "conversion")
			if kind, _, _ := unstructured.NestedString(obj, "spec", "names", "kind"); kind != "" {
				delIfEq(obj, kind+"List", "spec", "names", "listKind")
			}
		}
	case "StorageClass.storage.k8s.io":
		if h {
			delIfEq(obj, "Delete", "reclaimPolicy")
			delIfEq(obj, "Immediate", "volumeBindingMode")
		}
	case "APIService.apiregistration.k8s.io":
		if caInjected(u) {
			del(obj, "spec", "caBundle")
		}
	case "ValidatingWebhookConfiguration.admissionregistration.k8s.io", "MutatingWebhookConfiguration.admissionregistration.k8s.io":
		injected := caInjected(u)
		forEach(obj, func(w map[string]any) {
			if injected {
				del(w, "clientConfig", "caBundle")
			}
			if h {
				delIfEq(w, "Equivalent", "matchPolicy")
				delIfEq(w, map[string]any{}, "namespaceSelector")
				delIfEq(w, map[string]any{}, "objectSelector")
				delIfEq(w, int64(10), "timeoutSeconds")
				delIfEq(w, "Fail", "failurePolicy")
				delIfEq(w, "Never", "reinvocationPolicy")
				delIfEq(w, int64(443), "clientConfig", "service", "port")
				forEach(w, func(r map[string]any) { delIfEq(r, "*", "scope") }, "rules")
			}
		}, "webhooks")
	case "Deployment.apps":
		if kc.hpaTarget {
			del(obj, "spec", "replicas")
		}
		if h {
			delIfEq(obj, int64(600), "spec", "progressDeadlineSeconds")
			delIfEq(obj, int64(10), "spec", "revisionHistoryLimit")
			delIfEq(obj, map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxSurge": "25%", "maxUnavailable": "25%"}}, "spec", "strategy")
		}
	case "StatefulSet.apps":
		if kc.hpaTarget {
			del(obj, "spec", "replicas")
		}
		forEach(obj, func(t map[string]any) {
			del(t, "status")
			del(t, "metadata", "creationTimestamp")
			if h {
				delIfEq(t, "v1", "apiVersion")
				delIfEq(t, "PersistentVolumeClaim", "kind")
				delIfEq(t, "Filesystem", "spec", "volumeMode")
			}
		}, "spec", "volumeClaimTemplates")
		if h {
			delIfEq(obj, "OrderedReady", "spec", "podManagementPolicy")
			delIfEq(obj, int64(10), "spec", "revisionHistoryLimit")
			delIfEq(obj, map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"partition": int64(0)}}, "spec", "updateStrategy")
			delIfEq(obj, map[string]any{"whenDeleted": "Retain", "whenScaled": "Retain"}, "spec", "persistentVolumeClaimRetentionPolicy")
		}
	case "DaemonSet.apps":
		if h {
			delIfEq(obj, int64(10), "spec", "revisionHistoryLimit")
			delIfEq(obj, map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxSurge": int64(0), "maxUnavailable": int64(1)}}, "spec", "updateStrategy")
		}
	case "Job.batch":
		cleanJobSpec(obj, h, "spec")
	case "CronJob.batch":
		del(obj, "spec", "jobTemplate", "metadata", "creationTimestamp")
		cleanJobSpec(obj, h, "spec", "jobTemplate", "spec")
		if h {
			delIfEq(obj, "Allow", "spec", "concurrencyPolicy")
			delIfEq(obj, int64(1), "spec", "failedJobsHistoryLimit")
			delIfEq(obj, int64(3), "spec", "successfulJobsHistoryLimit")
			delIfEq(obj, false, "spec", "suspend")
		}
	case "HorizontalPodAutoscaler.autoscaling":
		if h {
			delIfEq(obj, map[string]any{
				"scaleUp": map[string]any{
					"stabilizationWindowSeconds": int64(0),
					"selectPolicy":               "Max",
					"policies": []any{
						map[string]any{"type": "Pods", "value": int64(4), "periodSeconds": int64(15)},
						map[string]any{"type": "Percent", "value": int64(100), "periodSeconds": int64(15)},
					},
				},
				"scaleDown": map[string]any{
					"selectPolicy": "Max",
					"policies": []any{
						map[string]any{"type": "Percent", "value": int64(100), "periodSeconds": int64(15)},
					},
				},
			}, "spec", "behavior")
		}
	}
}

func cleanService(obj map[string]any, kc kindContext) {
	if ip, _, _ := unstructured.NestedString(obj, "spec", "clusterIP"); ip != "None" && !kc.managedFields && !userSet(kc.lastApplied, "spec", "clusterIP") {
		del(obj, "spec", "clusterIP")
	}
	del(obj, "spec", "clusterIPs")
	del(obj, "spec", "healthCheckNodePort")
	// Node ports are allocated by the API server unless someone chose one.
	if !kc.managedFields {
		chosen := map[string]bool{}
		for _, p := range asSlice(nested(kc.lastApplied, "spec", "ports")) {
			pm, _ := p.(map[string]any)
			if pm["nodePort"] != nil {
				chosen[portKey(pm)] = true
			}
		}
		forEach(obj, func(p map[string]any) {
			if !chosen[portKey(p)] {
				delete(p, "nodePort")
			}
		}, "spec", "ports")
	}
	if kc.heuristic {
		delIfEq(obj, "None", "spec", "sessionAffinity")
		delIfEq(obj, "Cluster", "spec", "internalTrafficPolicy")
		delIfEq(obj, "Cluster", "spec", "externalTrafficPolicy")
		delIfEq(obj, true, "spec", "allocateLoadBalancerNodePorts")
		delIfEq(obj, "SingleStack", "spec", "ipFamilyPolicy")
		del(obj, "spec", "ipFamilies")
		forEach(obj, func(p map[string]any) {
			delIfEq(p, "TCP", "protocol")
			if jsonEqual(p["targetPort"], p["port"]) {
				delete(p, "targetPort")
			}
		}, "spec", "ports")
	}
}

func portKey(p map[string]any) string {
	b, _ := json.Marshal([]any{p["name"], p["port"]})
	return string(b)
}

func userSet(lastApplied map[string]any, path ...string) bool {
	if lastApplied == nil {
		return false
	}
	_, found, _ := unstructured.NestedFieldNoCopy(lastApplied, path...)
	return found
}

func cleanJobSpec(obj map[string]any, heuristic bool, path ...string) {
	manual, _, _ := unstructured.NestedBool(obj, append(path, "manualSelector")...)
	if !manual {
		del(obj, append(path, "selector")...)
		tl := append(append([]string(nil), path...), "template", "metadata", "labels")
		labels, _, _ := unstructured.NestedStringMap(obj, tl...)
		if labels != nil {
			for _, k := range []string{"controller-uid", "batch.kubernetes.io/controller-uid", "job-name", "batch.kubernetes.io/job-name"} {
				delete(labels, k)
			}
			if len(labels) == 0 {
				del(obj, tl...)
			} else {
				_ = unstructured.SetNestedStringMap(obj, labels, tl...)
			}
		}
	}
	if heuristic {
		delIfEq(obj, int64(6), append(path, "backoffLimit")...)
		delIfEq(obj, "NonIndexed", append(path, "completionMode")...)
		delIfEq(obj, int64(1), append(path, "completions")...)
		delIfEq(obj, int64(1), append(path, "parallelism")...)
		delIfEq(obj, false, append(path, "suspend")...)
		delIfEq(obj, "TerminatingOrFailed", append(path, "podReplacementPolicy")...)
	}
}

// cleanPodTemplate cleans the pod template (or pod) at specPath.
func cleanPodTemplate(u *unstructured.Unstructured, specPath []string, isPod, heuristic bool) {
	obj := u.Object
	spec, found, _ := unstructured.NestedMap(obj, specPath...)
	if !found {
		return
	}
	if !isPod {
		meta := append(append([]string(nil), specPath[:len(specPath)-1]...), "metadata")
		del(obj, append(meta, "creationTimestamp")...)
		defer pruneEmpty(obj, meta...)
		ann, _, _ := unstructured.NestedStringMap(obj, append(meta, "annotations")...)
		if ann != nil {
			for _, k := range templateAnnotations {
				delete(ann, k)
			}
			if len(ann) == 0 {
				del(obj, append(meta, "annotations")...)
			} else {
				_ = unstructured.SetNestedStringMap(obj, ann, append(meta, "annotations")...)
			}
		}
	}

	// serviceAccount is the deprecated alias of serviceAccountName.
	if jsonEqual(spec["serviceAccount"], spec["serviceAccountName"]) {
		delete(spec, "serviceAccount")
	}
	if isPod {
		delete(spec, "nodeName")
		delete(spec, "priority")
		// Service account token volumes are injected by admission.
		var vols []any
		injected := map[string]bool{}
		for _, v := range asSlice(spec["volumes"]) {
			vm, _ := v.(map[string]any)
			if n, _ := vm["name"].(string); len(n) > 15 && n[:15] == "kube-api-access" {
				injected[n] = true
				continue
			}
			vols = append(vols, v)
		}
		setOrDelete(spec, "volumes", vols)
		eachContainer(spec, func(c map[string]any) {
			var mounts []any
			for _, m := range asSlice(c["volumeMounts"]) {
				mm, _ := m.(map[string]any)
				if n, _ := mm["name"].(string); !injected[n] {
					mounts = append(mounts, m)
				}
			}
			setOrDelete(c, "volumeMounts", mounts)
		})
		var tols []any
		for _, t := range asSlice(spec["tolerations"]) {
			tm, _ := t.(map[string]any)
			k, _ := tm["key"].(string)
			if (k == "node.kubernetes.io/not-ready" || k == "node.kubernetes.io/unreachable") && jsonEqual(tm["tolerationSeconds"], int64(300)) {
				continue
			}
			tols = append(tols, t)
		}
		setOrDelete(spec, "tolerations", tols)
	}

	if heuristic {
		delIfEq(spec, "ClusterFirst", "dnsPolicy")
		if !isJob(u) {
			delIfEq(spec, "Always", "restartPolicy")
		}
		delIfEq(spec, "default-scheduler", "schedulerName")
		delIfEq(spec, map[string]any{}, "securityContext")
		delIfEq(spec, int64(30), "terminationGracePeriodSeconds")
		delIfEq(spec, true, "enableServiceLinks")
		delIfEq(spec, "PreemptLowerPriority", "preemptionPolicy")
		for _, v := range asSlice(spec["volumes"]) {
			vm, _ := v.(map[string]any)
			for _, src := range []string{"configMap", "secret", "projected", "downwardAPI"} {
				delIfEq(vm, int64(420), src, "defaultMode")
			}
			delIfEq(vm, "", "hostPath", "type")
			for _, item := range asSlice(nested(vm, "downwardAPI", "items")) {
				im, _ := item.(map[string]any)
				delIfEq(im, "v1", "fieldRef", "apiVersion")
			}
		}
		eachContainer(spec, func(c map[string]any) {
			delIfEq(c, "/dev/termination-log", "terminationMessagePath")
			delIfEq(c, "File", "terminationMessagePolicy")
			if img, _ := c["image"].(string); img != "" {
				delIfEq(c, ImageDefaultPullPolicy(img), "imagePullPolicy")
			}
			delIfEq(c, map[string]any{}, "resources")
			for _, p := range asSlice(c["ports"]) {
				pm, _ := p.(map[string]any)
				delIfEq(pm, "TCP", "protocol")
			}
			for _, probe := range []string{"livenessProbe", "readinessProbe", "startupProbe"} {
				pr, ok := c[probe].(map[string]any)
				if !ok {
					continue
				}
				delIfEq(pr, int64(1), "timeoutSeconds")
				delIfEq(pr, int64(10), "periodSeconds")
				delIfEq(pr, int64(1), "successThreshold")
				delIfEq(pr, int64(3), "failureThreshold")
				delIfEq(pr, "HTTP", "httpGet", "scheme")
			}
			for _, e := range asSlice(c["env"]) {
				em, _ := e.(map[string]any)
				delIfEq(em, "v1", "valueFrom", "fieldRef", "apiVersion")
			}
		})
	}
	_ = unstructured.SetNestedMap(obj, spec, specPath...)
}

func isJob(u *unstructured.Unstructured) bool {
	k := u.GetKind()
	return k == "Job" || k == "CronJob"
}

func caInjected(u *unstructured.Unstructured) bool {
	a := u.GetAnnotations()
	for _, k := range caInjectionAnnotations {
		if _, ok := a[k]; ok {
			return true
		}
	}
	return false
}

func eachContainer(spec map[string]any, fn func(map[string]any)) {
	for _, field := range []string{"initContainers", "containers", "ephemeralContainers"} {
		for _, c := range asSlice(spec[field]) {
			if cm, ok := c.(map[string]any); ok {
				fn(cm)
			}
		}
	}
}

// forEach calls fn for every map in the list at path.
func forEach(obj map[string]any, fn func(map[string]any), path ...string) {
	for _, item := range asSlice(nested(obj, path...)) {
		if m, ok := item.(map[string]any); ok {
			fn(m)
		}
	}
}

func nested(obj map[string]any, path ...string) any {
	v, _, _ := unstructured.NestedFieldNoCopy(obj, path...)
	return v
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func setOrDelete(m map[string]any, key string, v []any) {
	if len(v) == 0 {
		delete(m, key)
		return
	}
	m[key] = v
}

func del(obj map[string]any, path ...string) {
	if obj != nil {
		unstructured.RemoveNestedField(obj, path...)
	}
}

// delIfEq removes the field at path when its value equals def, and then
// removes parents left empty.
func delIfEq(obj map[string]any, def any, path ...string) {
	if obj == nil {
		return
	}
	v, found, _ := unstructured.NestedFieldNoCopy(obj, path...)
	if !found || !jsonEqual(v, def) {
		return
	}
	unstructured.RemoveNestedField(obj, path...)
	for i := len(path) - 1; i > 0; i-- {
		parent, found, _ := unstructured.NestedFieldNoCopy(obj, path[:i]...)
		if pm, ok := parent.(map[string]any); found && ok && len(pm) == 0 {
			unstructured.RemoveNestedField(obj, path[:i]...)
			continue
		}
		break
	}
}

// Equal reports whether two values are equal after JSON round-tripping, so
// int64 and float64 representations of the same number compare equal.
func Equal(a, b any) bool { return jsonEqual(a, b) }
