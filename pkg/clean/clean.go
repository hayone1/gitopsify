// Package clean turns live objects into manifests: it removes runtime
// metadata, status, cluster-allocated values and server-side defaults.
package clean

import (
	"encoding/json"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/deps"
	"github.com/hayone1/gitopsify/pkg/match"
	"github.com/hayone1/gitopsify/pkg/model"
)

// stripAnnotations are runtime annotations removed from every object (globs).
var stripAnnotations = []string{
	"kubectl.kubernetes.io/last-applied-configuration",
	"deployment.kubernetes.io/revision",
	"kubernetes.io/change-cause",
	"meta.helm.sh/release-name",
	"meta.helm.sh/release-namespace",
	"argocd.argoproj.io/tracking-id",
	"endpoints.kubernetes.io/last-change-trigger-time",
	"control-plane.alpha.kubernetes.io/leader",
	"pv.kubernetes.io/*",
	"volume.beta.kubernetes.io/storage-provisioner",
	"volume.kubernetes.io/*",
	"kubernetes.io/service-account.uid",
	"autoscaling.alpha.kubernetes.io/*",
	"field.cattle.io/publicEndpoints",
	"cloud.google.com/neg-status",
	"batch.kubernetes.io/job-tracking",
	"deprecated.daemonset.template.generation",
	// OpenShift allocates UID and SELinux ranges per namespace and records
	// who requested a project and its generated pull secrets.
	"openshift.io/sa.scc.*",
	"openshift.io/requester",
	"openshift.io/internal-registry-pull-secret-ref",
	// Rancher's lifecycle bookkeeping and the project of the source cluster.
	"cattle.io/status",
	"lifecycle.cattle.io/*",
	"field.cattle.io/projectId",
	// GKE Autopilot's resource adjustments.
	"autopilot.gke.io/*",
	// k3s and Rancher's objectset apply bookkeeping.
	"objectset.rio.cattle.io/*",
	// Load balancers and addresses allocated in the source cluster.
	"kubernetes.digitalocean.com/load-balancer-id",
	"metallb.universe.tf/ip-allocated-from-pool",
	"metallb.io/ip-allocated-from-pool",
}

// stripLabels are runtime labels removed from object metadata (globs).
var stripLabels = []string{
	"kubernetes.io/metadata.name",
	"controller-uid",
	"batch.kubernetes.io/controller-uid",
	"job-name",
	"batch.kubernetes.io/job-name",
	"pod-template-hash",
	"controller-revision-hash",
	"statefulset.kubernetes.io/pod-name",
	"argocd.argoproj.io/instance",
	"kustomize.toolkit.fluxcd.io/*",
	"helm.toolkit.fluxcd.io/*",
	"field.cattle.io/projectId",
	"objectset.rio.cattle.io/*",
}

// templateAnnotations are removed from pod templates. They record a
// rollout restart, not configuration.
var templateAnnotations = []string{
	"kubectl.kubernetes.io/restartedAt",
	"cattle.io/timestamp",
}

// Cleaner cleans objects.
type Cleaner struct {
	Config *config.Config
	// hpaTargets holds "Kind/namespace/name" of workloads scaled by an HPA.
	hpaTargets map[string]bool
}

// Run cleans every included object of the inventory into Object.Out.
func Run(inv *model.Inventory, cfg *config.Config) {
	c := &Cleaner{Config: cfg, hpaTargets: map[string]bool{}}
	for _, o := range inv.Objects {
		if o.Excluded || o.Ref.GroupKind() != "HorizontalPodAutoscaler.autoscaling" {
			continue
		}
		kind, _, _ := unstructured.NestedString(o.Live.Object, "spec", "scaleTargetRef", "kind")
		name, _, _ := unstructured.NestedString(o.Live.Object, "spec", "scaleTargetRef", "name")
		c.hpaTargets[kind+"/"+o.Ref.Namespace+"/"+name] = true
	}
	for _, o := range inv.Objects {
		if !o.Excluded {
			o.Out = c.Clean(o)
		}
	}
}

// Strategy returns the strategy used for an object under "auto".
func (c *Cleaner) Strategy(o *model.Object) string {
	s := c.Config.Clean.Strategy
	if s != config.CleanAuto {
		return s
	}
	if _, allApply, ok := UserFields(o.Live, c.Config.Clean.ControllerManagers); ok && allApply {
		return config.CleanManagedFields
	}
	return config.CleanHeuristic
}

// Clean returns the cleaned copy of an object.
func (c *Cleaner) Clean(o *model.Object) *unstructured.Unstructured {
	strategy := c.Strategy(o)
	userFields, _, haveFields := UserFields(o.Live, c.Config.Clean.ControllerManagers)

	out := o.Live.DeepCopy()
	if strategy == config.CleanManagedFields && haveFields {
		pruned, _ := Prune(out.Object, userFields).(map[string]any)
		// Identity always survives, whoever owns it.
		pruned["apiVersion"] = o.Live.GetAPIVersion()
		pruned["kind"] = o.Live.GetKind()
		meta, _ := pruned["metadata"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		meta["name"] = o.Live.GetName()
		if o.Live.GetNamespace() != "" {
			meta["namespace"] = o.Live.GetNamespace()
		}
		pruned["metadata"] = meta
		out.Object = pruned
	}

	var lastApplied map[string]any
	if la := o.Live.GetAnnotations()["kubectl.kubernetes.io/last-applied-configuration"]; la != "" {
		_ = json.Unmarshal([]byte(la), &lastApplied)
	}

	c.cleanMetadata(out)
	delete(out.Object, "status")
	// Known defaults are removed under every strategy but minimal: managed
	// fields still carry defaulted keys of list items (protocol: TCP).
	cleanKind(out, o, kindContext{
		managedFields: strategy == config.CleanManagedFields && haveFields,
		heuristic:     strategy != config.CleanMinimal,
		lastApplied:   lastApplied,
		hpaTarget:     c.hpaTargets[o.Ref.Kind+"/"+o.Ref.Namespace+"/"+o.Ref.Name],
	})
	if spec := deps.PodSpecPath(o.Ref.GroupKind()); spec != nil {
		cleanPodTemplate(out, spec, o.Ref.Kind == "Pod", strategy != config.CleanMinimal)
	}
	pruneEmpty(out.Object, "metadata", "annotations")
	pruneEmpty(out.Object, "metadata", "labels")
	return out
}

func (c *Cleaner) cleanMetadata(u *unstructured.Unstructured) {
	for _, f := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "deletionTimestamp",
		"deletionGracePeriodSeconds", "selfLink", "managedFields", "ownerReferences", "finalizers"} {
		unstructured.RemoveNestedField(u.Object, "metadata", f)
	}
	if u.GetName() != "" {
		unstructured.RemoveNestedField(u.Object, "metadata", "generateName")
	}
	u.SetAnnotations(dropKeys(u.GetAnnotations(), append(stripAnnotations, c.Config.Clean.StripAnnotations...)))
	u.SetLabels(dropKeys(u.GetLabels(), append(stripLabels, c.Config.Clean.StripLabels...)))
}

func dropKeys(m map[string]string, patterns []string) map[string]string {
	if len(m) == 0 {
		return m
	}
	out := map[string]string{}
	for k, v := range m {
		if !match.Glob(patterns, k) {
			out[k] = v
		}
	}
	return out
}

func pruneEmpty(obj map[string]any, path ...string) {
	v, found, _ := unstructured.NestedFieldNoCopy(obj, path...)
	if !found {
		return
	}
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			unstructured.RemoveNestedField(obj, path...)
		}
	case map[string]string:
		if len(t) == 0 {
			unstructured.RemoveNestedField(obj, path...)
		}
	case nil:
		unstructured.RemoveNestedField(obj, path...)
	}
}

// ImageDefaultPullPolicy returns the pull policy Kubernetes defaults to for an
// image: Always for ":latest" or untagged images, IfNotPresent otherwise.
func ImageDefaultPullPolicy(image string) string {
	if strings.Contains(image, "@") {
		return "IfNotPresent"
	}
	name := image
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	tag := ""
	if i := strings.LastIndex(name, ":"); i >= 0 {
		tag = name[i+1:]
	}
	if tag == "" || tag == "latest" {
		return "Always"
	}
	return "IfNotPresent"
}
