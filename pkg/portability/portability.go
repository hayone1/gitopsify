// Package portability warns about migrated objects that depend on the source
// environment: specific nodes, node-local data, in-cluster registries,
// allocated addresses, existing volumes and cloud identities.
package portability

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/deps"
	"github.com/hayone1/gitopsify/pkg/model"
)

// identityAnnotations bind ServiceAccounts to cloud identities.
var identityAnnotations = map[string]string{
	"eks.amazonaws.com/role-arn":        "an AWS IAM role (IRSA)",
	"iam.gke.io/gcp-service-account":    "a Google service account (Workload Identity)",
	"azure.workload.identity/client-id": "an Azure managed identity (Workload Identity)",
}

// Check adds a warning to every migrated object that won't work unchanged in
// another environment. A takeover keeps the environment, so it is skipped.
func Check(inv *model.Inventory, cfg *config.Config) {
	if cfg.Adopt.Enabled {
		return
	}
	for _, o := range inv.Objects {
		if o.Excluded || o.Out == nil {
			continue
		}
		u := o.Out
		if spec := deps.PodSpec(u); spec != nil {
			podSpec(o, spec)
		}
		switch o.Ref.GroupKind() {
		case "PersistentVolume":
			persistentVolume(o, u)
		case "PersistentVolumeClaim":
			for _, f := range []string{"dataSource", "dataSourceRef"} {
				if src, found, _ := unstructured.NestedMap(u.Object, "spec", f); found {
					o.Warn("is created from %s %v, which must exist in the target first", src["kind"], src["name"])
					break
				}
			}
		case "Service":
			if ip, _, _ := unstructured.NestedString(u.Object, "spec", "loadBalancerIP"); ip != "" {
				o.Warn("requests load balancer IP %s, which belongs to the source environment; change it per environment (a set or remove transform on spec.loadBalancerIP)", ip)
			}
			if ips, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "externalIPs"); len(ips) > 0 {
				o.Warn("has externalIPs %s of the source environment; change them per environment", strings.Join(ips, ", "))
			}
		case "ServiceAccount":
			for k, what := range identityAnnotations {
				if v := u.GetAnnotations()[k]; v != "" {
					o.Warn("is bound to %s, %s, of the source environment; point it at the target's with an annotations transform per environment", what, v)
				}
			}
		}
	}
}

func podSpec(o *model.Object, spec map[string]any) {
	if n, _ := spec["nodeName"].(string); n != "" {
		o.Warn("is pinned to node %s (spec.nodeName), which won't exist in the target; schedule with node labels instead", n)
	}
	if sel, ok := spec["nodeSelector"].(map[string]any); ok {
		if h, ok := sel["kubernetes.io/hostname"]; ok {
			o.Warn("selects node %v by hostname, which won't exist in the target; select node labels instead", h)
		}
	}
	terms, _, _ := unstructured.NestedSlice(spec, "affinity", "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms")
	for _, t := range terms {
		tm, _ := t.(map[string]any)
		for _, field := range []string{"matchExpressions", "matchFields"} {
			exprs, _ := tm[field].([]any)
			for _, e := range exprs {
				em, _ := e.(map[string]any)
				if k := em["key"]; k == "kubernetes.io/hostname" || k == "metadata.name" {
					o.Warn("requires specific nodes by name (node affinity on %v), which won't exist in the target", k)
				}
			}
		}
	}
	vols, _ := spec["volumes"].([]any)
	for _, v := range vols {
		vm, _ := v.(map[string]any)
		if hp, ok := vm["hostPath"].(map[string]any); ok {
			o.Warn("mounts hostPath %v (volume %v); its data stays on the source node", hp["path"], vm["name"])
		}
	}
	for _, field := range []string{"initContainers", "containers", "ephemeralContainers"} {
		cs, _ := spec[field].([]any)
		for _, c := range cs {
			cm, _ := c.(map[string]any)
			if img, _ := cm["image"].(string); localRegistry(img) {
				o.Warn("runs image %s from a registry local to the source cluster; push it to a registry the target can pull from and add an image transform", img)
			}
		}
	}
}

// localRegistry reports whether an image is pulled from a registry that only
// the source cluster can reach: localhost, an in-cluster Service, OpenShift's
// internal registry or a kind/k3d/minikube registry.
func localRegistry(image string) bool {
	host, _, found := strings.Cut(image, "/")
	if !found || !strings.ContainsAny(host, ".:") && host != "localhost" {
		return false // a Docker Hub image
	}
	h, _, _ := strings.Cut(host, ":")
	switch {
	case h == "localhost", h == "127.0.0.1", h == "::1", h == "kind-registry", h == "registry.localhost",
		strings.HasSuffix(h, ".svc"), strings.HasSuffix(h, ".svc.cluster.local"), strings.HasSuffix(h, ".cluster.local"),
		strings.HasSuffix(h, ".localhost"), strings.HasPrefix(h, "k3d-"):
		return true
	}
	return false
}

func persistentVolume(o *model.Object, u *unstructured.Unstructured) {
	spec, _, _ := unstructured.NestedMap(u.Object, "spec")
	for _, local := range []string{"hostPath", "local"} {
		if v, ok := spec[local].(map[string]any); ok {
			o.Warn("is a %s volume at %v; its data stays on the source node", local, v["path"])
			return
		}
	}
	for _, backend := range []string{"csi", "nfs", "awsElasticBlockStore", "gcePersistentDisk", "azureDisk", "azureFile", "iscsi", "fc", "cephfs", "rbd", "cinder", "vsphereVolume"} {
		if _, ok := spec[backend]; ok {
			o.Warn("is a statically provisioned %s volume; the target must reach the same storage, or drop the PV and let a StorageClass provision a new one", backend)
			return
		}
	}
}
