package filter

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/model"
)

func objects(t *testing.T, docs ...string) *model.Inventory {
	t.Helper()
	inv := &model.Inventory{}
	for _, d := range docs {
		m := map[string]any{}
		if err := yaml.Unmarshal([]byte(d), &m); err != nil {
			t.Fatal(err)
		}
		u := &unstructured.Unstructured{Object: m}
		inv.Add(&model.Object{Ref: model.RefOf(u), Namespaced: u.GetNamespace() != "", Live: u})
	}
	return inv
}

func reason(inv *model.Inventory, ref string) string {
	for _, o := range inv.Objects {
		if o.Ref.String() == ref {
			if !o.Excluded {
				return "included"
			}
			return o.Reason
		}
	}
	return "missing"
}

func run(t *testing.T, inv *model.Inventory, cfg *config.Config) {
	t.Helper()
	cfg.Default()
	if err := Releases(inv, cfg); err != nil {
		t.Fatal(err)
	}
	if err := Apply(inv, Options{Config: cfg}); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultRules(t *testing.T) {
	inv := objects(t,
		`{apiVersion: v1, kind: ConfigMap, metadata: {name: app, namespace: shop}}`,
		`{apiVersion: v1, kind: ConfigMap, metadata: {name: kube-root-ca.crt, namespace: shop}}`,
		`{apiVersion: v1, kind: ConfigMap, metadata: {name: coredns, namespace: kube-system}}`,
		`{apiVersion: v1, kind: Pod, metadata: {name: web-1, namespace: shop, ownerReferences: [{kind: ReplicaSet, name: web, controller: true}]}}`,
		`{apiVersion: v1, kind: Pod, metadata: {name: debug, namespace: shop}}`,
		`{apiVersion: batch/v1, kind: Job, metadata: {name: once, namespace: shop}, status: {completionTime: "2024-01-01T00:00:00Z"}}`,
		`{apiVersion: batch/v1, kind: Job, metadata: {name: running, namespace: shop}}`,
		`{apiVersion: v1, kind: ServiceAccount, metadata: {name: default, namespace: shop}}`,
		`{apiVersion: v1, kind: ServiceAccount, metadata: {name: default, namespace: other}, imagePullSecrets: [{name: reg}]}`,
		`{apiVersion: rbac.authorization.k8s.io/v1, kind: ClusterRole, metadata: {name: "system:aggregate-to-admin"}}`,
		`{apiVersion: rbac.authorization.k8s.io/v1, kind: ClusterRole, metadata: {name: cni}}`,
		`{apiVersion: rbac.authorization.k8s.io/v1, kind: ClusterRoleBinding, metadata: {name: cni}, roleRef: {kind: ClusterRole, name: cni}, subjects: [{kind: ServiceAccount, name: cni, namespace: kube-system}]}`,
		`{apiVersion: rbac.authorization.k8s.io/v1, kind: ClusterRole, metadata: {name: shared}}`,
		`{apiVersion: rbac.authorization.k8s.io/v1, kind: ClusterRoleBinding, metadata: {name: shared-sys}, roleRef: {kind: ClusterRole, name: shared}, subjects: [{kind: ServiceAccount, name: x, namespace: kube-system}]}`,
		`{apiVersion: rbac.authorization.k8s.io/v1, kind: RoleBinding, metadata: {name: shared-app, namespace: shop}, roleRef: {kind: ClusterRole, name: shared}, subjects: [{kind: ServiceAccount, name: web, namespace: shop}]}`,
		`{apiVersion: v1, kind: Secret, type: helm.sh/release.v1, metadata: {name: sh.helm.release.v1.x.v1, namespace: shop, labels: {owner: helm, name: x}}}`,
		`{apiVersion: v1, kind: Secret, metadata: {name: tls, namespace: shop, annotations: {cert-manager.io/certificate-name: web}}}`,
		`{apiVersion: v1, kind: Service, metadata: {name: web, namespace: shop}, spec: {selector: {app: web}}}`,
		`{apiVersion: v1, kind: Endpoints, metadata: {name: web, namespace: shop}}`,
		`{apiVersion: v1, kind: Service, metadata: {name: external, namespace: shop}, spec: {ports: [{port: 5432}]}}`,
		`{apiVersion: v1, kind: Endpoints, metadata: {name: external, namespace: shop}}`,
		`{apiVersion: discovery.k8s.io/v1, kind: EndpointSlice, metadata: {name: web-abc, namespace: shop, labels: {kubernetes.io/service-name: web}}}`,
		`{apiVersion: apps/v1, kind: Deployment, metadata: {name: argo-app, namespace: shop, annotations: {argocd.argoproj.io/tracking-id: "shop:apps/Deployment:shop/argo-app"}}}`,
		`{apiVersion: storage.k8s.io/v1, kind: StorageClass, metadata: {name: standard, annotations: {storageclass.kubernetes.io/is-default-class: "true"}}}`,
		`{apiVersion: v1, kind: PersistentVolume, metadata: {name: pvc-1, annotations: {pv.kubernetes.io/provisioned-by: ebs.csi.aws.com}}}`,
		`{apiVersion: v1, kind: PersistentVolume, metadata: {name: nfs}}`,
		`{apiVersion: v1, kind: Namespace, metadata: {name: default}}`,
		`{apiVersion: v1, kind: Namespace, metadata: {name: argocd}}`,
	)
	run(t, inv, config.New())
	for ref, want := range map[string]string{
		"ConfigMap/shop/app":              "included",
		"ConfigMap/shop/kube-root-ca.crt": "cluster-provided-config",
		"ConfigMap/kube-system/coredns":   "system-namespaces",
		"Pod/shop/web-1":                  "owned-by-controller",
		"Pod/shop/debug":                  "bare-pods",
		"Job.batch/shop/once":             "completed-jobs",
		"Job.batch/shop/running":          "included",
		"ServiceAccount/shop/default":     "default-serviceaccount",
		"ServiceAccount/other/default":    "included",
		"ClusterRole.rbac.authorization.k8s.io/system:aggregate-to-admin": "system-rbac",
		"ClusterRole.rbac.authorization.k8s.io/cni":                       "system-component-rbac",
		"ClusterRoleBinding.rbac.authorization.k8s.io/cni":                "system-component-rbac",
		"ClusterRole.rbac.authorization.k8s.io/shared":                    "included",
		"Secret/shop/sh.helm.release.v1.x.v1":                             "helm-release-storage",
		"Secret/shop/tls":                                                 "cert-manager-secrets",
		"Endpoints/shop/web":                                              "controller-endpoints",
		"Endpoints/shop/external":                                         "included",
		"EndpointSlice.discovery.k8s.io/shop/web-abc":                     "controller-endpoints",
		"Deployment.apps/shop/argo-app":                                   "already-gitops-managed",
		"StorageClass.storage.k8s.io/standard":                            "default-storageclass",
		"PersistentVolume/pvc-1":                                          "dynamic-persistentvolumes",
		"PersistentVolume/nfs":                                            "included",
		"Namespace/default":                                               "default-namespace",
		"Namespace/argocd":                                                "gitops-namespaces",
	} {
		got := reason(inv, ref)
		if rule, _, _ := strings.Cut(got, ":"); rule != want {
			t.Errorf("%s: %s, want %s", ref, got, want)
		}
	}
}

func TestScopeAndUserRules(t *testing.T) {
	docs := []string{
		`{apiVersion: v1, kind: ConfigMap, metadata: {name: a, namespace: shop}}`,
		`{apiVersion: v1, kind: ConfigMap, metadata: {name: legacy-a, namespace: shop}}`,
		`{apiVersion: v1, kind: Secret, metadata: {name: s, namespace: shop}}`,
		`{apiVersion: v1, kind: ConfigMap, metadata: {name: b, namespace: billing}}`,
		`{apiVersion: v1, kind: Namespace, metadata: {name: shop}}`,
		`{apiVersion: v1, kind: Namespace, metadata: {name: billing}}`,
		`{apiVersion: rbac.authorization.k8s.io/v1, kind: ClusterRole, metadata: {name: x}}`,
		`{apiVersion: v1, kind: ConfigMap, metadata: {name: coredns, namespace: kube-system}}`,
	}
	inv := objects(t, docs...)
	cfg := config.New()
	cfg.Scope.Namespaces = []string{"sh*"}
	cfg.Scope.ExcludeKinds = []string{"secret"}
	cfg.Scope.Rules = []config.Rule{
		{Name: "no-legacy", Action: "exclude", Selector: config.Selector{Names: []string{"legacy-*"}}, Reason: "deprecated"},
		{Action: "include", Selector: config.Selector{Namespaces: []string{"kube-system"}, Names: []string{"coredns"}}},
	}
	run(t, inv, cfg)
	for ref, want := range map[string]string{
		"ConfigMap/shop/a":                        "included",
		"ConfigMap/shop/legacy-a":                 "user rule no-legacy: deprecated",
		"Secret/shop/s":                           "scope: kind excluded",
		"ConfigMap/billing/b":                     "scope: namespace not selected",
		"Namespace/shop":                          "scope: cluster-scoped and not referenced by migrated objects",
		"Namespace/billing":                       "scope: namespace not selected",
		"ClusterRole.rbac.authorization.k8s.io/x": "scope: cluster-scoped and not referenced by migrated objects",
		"ConfigMap/kube-system/coredns":           "included",
	} {
		if got := reason(inv, ref); got != want {
			t.Errorf("%s: %q, want %q", ref, got, want)
		}
	}

	inv = objects(t, docs...)
	cfg = config.New()
	cfg.Scope.DisableDefaultRules = []string{"*"}
	run(t, inv, cfg)
	if got := reason(inv, "ConfigMap/kube-system/coredns"); got != "included" {
		t.Errorf("with every rule disabled coredns is %q", got)
	}
}

func TestSelectionAndHelm(t *testing.T) {
	inv := objects(t,
		`{apiVersion: apps/v1, kind: Deployment, metadata: {name: web, namespace: shop}}`,
		`{apiVersion: apps/v1, kind: Deployment, metadata: {name: api, namespace: shop}}`,
		`{apiVersion: apps/v1, kind: Deployment, metadata: {name: chart-web, namespace: tools, labels: {tier: front}}}`,
		`{apiVersion: v1, kind: Service, metadata: {name: chart-web, namespace: tools}}`,
		`{apiVersion: apps/v1, kind: Deployment, metadata: {name: other, namespace: tools}}`,
	)
	inv.Releases = []*model.HelmRelease{{Name: "chart", Namespace: "tools"}, {Name: "unused", Namespace: "tools"}}
	for _, o := range inv.Objects {
		if strings.HasPrefix(o.Ref.Name, "chart-") {
			o.HelmRelease = "tools/chart"
		}
	}
	cfg := config.New()
	cfg.Scope.Resources = []string{"deploy/shop/web"}
	cfg.Scope.LabelSelector = "tier=front"
	run(t, inv, cfg)
	for ref, want := range map[string]string{
		"Deployment.apps/shop/web":        "included",
		"Deployment.apps/shop/api":        "scope: not selected",
		"Deployment.apps/tools/chart-web": "included",
		"Service/tools/chart-web":         "included", // the selected object pulls in its whole release
		"Deployment.apps/tools/other":     "scope: not selected",
	} {
		if got := reason(inv, ref); got != want {
			t.Errorf("%s: %q, want %q", ref, got, want)
		}
	}
	if r := inv.Release("tools/unused"); !r.Skip || r.SkipReason != "not selected" {
		t.Errorf("unselected release: skip=%v reason=%q", r.Skip, r.SkipReason)
	}
}

// A cluster-scoped object named with --resource is migrated, though nothing
// selected references it.
func TestSelectClusterScoped(t *testing.T) {
	inv := objects(t,
		`{apiVersion: cert-manager.io/v1, kind: ClusterIssuer, metadata: {name: letsencrypt}}`,
		`{apiVersion: cert-manager.io/v1, kind: ClusterIssuer, metadata: {name: other}}`,
	)
	cfg := config.New()
	cfg.Scope.Resources = []string{"clusterissuer.cert-manager.io/letsencrypt"}
	run(t, inv, cfg)
	if got := reason(inv, "ClusterIssuer.cert-manager.io/letsencrypt"); got != "included" {
		t.Errorf("named ClusterIssuer: %q", got)
	}
	if got := reason(inv, "ClusterIssuer.cert-manager.io/other"); got == "included" {
		t.Error("an unnamed ClusterIssuer was included")
	}
}

func TestParseResourceSpec(t *testing.T) {
	for in, want := range map[string]ResourceSpec{
		"deploy/web":                         {Kind: "deploy", Name: "web"},
		"deployment/shop/web":                {Kind: "deployment", Namespace: "shop", Name: "web"},
		"helm/tools/chart":                   {Kind: "helm", Namespace: "tools", Name: "chart", Helm: true},
		"ingress.networking.k8s.io/shop/web": {Kind: "ingress.networking.k8s.io", Namespace: "shop", Name: "web"},
	} {
		got, err := ParseResourceSpec(in)
		if err != nil || got != want {
			t.Errorf("%s: %+v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"web", "a/b/c/d", "/web", "deploy/"} {
		if _, err := ParseResourceSpec(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPlatformRules(t *testing.T) {
	inv := objects(t,
		`{apiVersion: v1, kind: ConfigMap, metadata: {name: old, namespace: shop, deletionTimestamp: "2026-01-01T00:00:00Z"}}`,
		`{apiVersion: apiextensions.k8s.io/v1, kind: CustomResourceDefinition, metadata: {name: ciliumnetworkpolicies.cilium.io}, spec: {group: cilium.io}}`,
		`{apiVersion: apiextensions.k8s.io/v1, kind: CustomResourceDefinition, metadata: {name: addons.k3s.cattle.io}, spec: {group: k3s.cattle.io}}`,
		`{apiVersion: apiextensions.k8s.io/v1, kind: CustomResourceDefinition, metadata: {name: certificates.cert-manager.io}, spec: {group: cert-manager.io}}`,
		`{apiVersion: v1, kind: ResourceQuota, metadata: {name: gke-resource-quotas, namespace: shop}}`,
		`{apiVersion: v1, kind: ServiceAccount, metadata: {name: builder, namespace: shop, annotations: {openshift.io/internal-registry-pull-secret-ref: builder-dockercfg-abcde}}, imagePullSecrets: [{name: builder-dockercfg-abcde}]}`,
		`{apiVersion: v1, kind: ServiceAccount, metadata: {name: pipeline, namespace: shop, annotations: {eks.amazonaws.com/role-arn: arn}}, imagePullSecrets: [{name: pipeline-dockercfg-abcde}]}`,
		`{apiVersion: v1, kind: ServiceAccount, metadata: {name: default, namespace: shop}, imagePullSecrets: [{name: default-dockercfg-x1y2z}]}`,
		`{apiVersion: v1, kind: Secret, type: kubernetes.io/dockercfg, metadata: {name: builder-dockercfg-abcde, namespace: shop, annotations: {kubernetes.io/service-account.name: builder}}}`,
		`{apiVersion: v1, kind: ConfigMap, metadata: {name: synced, namespace: shop, annotations: {configmanagement.gke.io/managed: enabled}}}`,
		`{apiVersion: v1, kind: ConfigMap, metadata: {name: app, namespace: cattle-monitoring-system}}`,
		`{apiVersion: v1, kind: ConfigMap, metadata: {name: app, namespace: shop}}`,
		`{apiVersion: rbac.authorization.k8s.io/v1, kind: ClusterRole, metadata: {name: metrics-reader, annotations: {objectset.rio.cattle.io/owner-gvk: "k3s.cattle.io/v1, Kind=Addon", objectset.rio.cattle.io/owner-name: metrics-server, objectset.rio.cattle.io/owner-namespace: kube-system}}}`,
		`{apiVersion: apiregistration.k8s.io/v1, kind: APIService, metadata: {name: v1beta1.metrics.k8s.io}, spec: {service: {name: metrics-server, namespace: kube-system}}}`,
		`{apiVersion: apiregistration.k8s.io/v1, kind: APIService, metadata: {name: v1.acme.io}, spec: {service: {name: acme-api, namespace: acme}}}`,
		`{apiVersion: admissionregistration.k8s.io/v1, kind: ValidatingWebhookConfiguration, metadata: {name: cni}, webhooks: [{name: a.cni.io, clientConfig: {service: {name: cni, namespace: calico-system}}}]}`,
		`{apiVersion: admissionregistration.k8s.io/v1, kind: ValidatingWebhookConfiguration, metadata: {name: mixed}, webhooks: [{name: a.x.io, clientConfig: {service: {name: x, namespace: kube-system}}}, {name: b.x.io, clientConfig: {url: "https://x.example.com"}}]}`,
	)
	run(t, inv, &config.Config{})
	for ref, want := range map[string]string{
		"ClusterRole.rbac.authorization.k8s.io/metrics-reader":              "addon-managed:",
		"APIService.apiregistration.k8s.io/v1beta1.metrics.k8s.io":          "system-served-registrations:",
		"APIService.apiregistration.k8s.io/v1.acme.io":                      "included",
		"ValidatingWebhookConfiguration.admissionregistration.k8s.io/cni":   "system-served-registrations:",
		"ValidatingWebhookConfiguration.admissionregistration.k8s.io/mixed": "included",
		"ConfigMap/shop/old": "terminating:",
		"CustomResourceDefinition.apiextensions.k8s.io/ciliumnetworkpolicies.cilium.io": "platform-crds:",
		"CustomResourceDefinition.apiextensions.k8s.io/addons.k3s.cattle.io":            "platform-crds:",
		"CustomResourceDefinition.apiextensions.k8s.io/certificates.cert-manager.io":    "included",
		"ResourceQuota/shop/gke-resource-quotas":                                        "cluster-provided-config:",
		"ServiceAccount/shop/builder":                                                   "platform-serviceaccounts:",
		"ServiceAccount/shop/pipeline":                                                  "included", // customized
		"ServiceAccount/shop/default":                                                   "default-serviceaccount:",
		"Secret/shop/builder-dockercfg-abcde":                                           "platform-serviceaccounts:",
		"ConfigMap/shop/synced":                                                         "already-gitops-managed:",
		"ConfigMap/cattle-monitoring-system/app":                                        "system-namespaces:",
		"ConfigMap/shop/app":                                                            "included",
	} {
		if got := reason(inv, ref); !strings.HasPrefix(got, want) {
			t.Errorf("%s: got %q, want %q", ref, got, want)
		}
	}
}
