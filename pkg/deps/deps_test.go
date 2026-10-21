package deps

import (
	"sort"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/filter"
	"github.com/hayone1/gitopsify/pkg/model"
)

const cluster = `
- {apiVersion: v1, kind: Namespace, metadata: {name: shop}}
- {apiVersion: v1, kind: Namespace, metadata: {name: other}}
- apiVersion: apps/v1
  kind: Deployment
  metadata: {name: web, namespace: shop}
  spec:
    template:
      metadata: {labels: {app: web, tier: front}}
      spec:
        serviceAccountName: web
        priorityClassName: high
        imagePullSecrets: [{name: registry}]
        containers:
          - name: web
            envFrom: [{configMapRef: {name: env}}]
            env: [{name: P, valueFrom: {secretKeyRef: {name: db, key: password}}}]
        volumes:
          - {name: c, configMap: {name: files}}
          - {name: d, persistentVolumeClaim: {claimName: data}}
          - {name: p, projected: {sources: [{secret: {name: tls}}]}}
- {apiVersion: v1, kind: ConfigMap, metadata: {name: env, namespace: shop}}
- {apiVersion: v1, kind: ConfigMap, metadata: {name: files, namespace: shop}}
- {apiVersion: v1, kind: ConfigMap, metadata: {name: unrelated, namespace: shop}}
- {apiVersion: v1, kind: Secret, metadata: {name: db, namespace: shop}}
- {apiVersion: v1, kind: Secret, metadata: {name: tls, namespace: shop}}
- {apiVersion: v1, kind: Secret, metadata: {name: registry, namespace: shop}}
- {apiVersion: v1, kind: Secret, metadata: {name: ingress-tls, namespace: shop}}
- {apiVersion: v1, kind: PersistentVolumeClaim, metadata: {name: data, namespace: shop}, spec: {storageClassName: fast}}
- {apiVersion: storage.k8s.io/v1, kind: StorageClass, metadata: {name: fast}}
- {apiVersion: scheduling.k8s.io/v1, kind: PriorityClass, metadata: {name: high}}
- {apiVersion: v1, kind: ServiceAccount, metadata: {name: web, namespace: shop}}
- {apiVersion: rbac.authorization.k8s.io/v1, kind: Role, metadata: {name: reader, namespace: shop}}
- {apiVersion: rbac.authorization.k8s.io/v1, kind: RoleBinding, metadata: {name: web-reader, namespace: shop}, roleRef: {kind: Role, name: reader}, subjects: [{kind: ServiceAccount, name: web}]}
- {apiVersion: rbac.authorization.k8s.io/v1, kind: ClusterRole, metadata: {name: viewer}}
- {apiVersion: rbac.authorization.k8s.io/v1, kind: ClusterRoleBinding, metadata: {name: web-viewer}, roleRef: {kind: ClusterRole, name: viewer}, subjects: [{kind: ServiceAccount, name: web, namespace: shop}]}
- {apiVersion: v1, kind: Service, metadata: {name: web, namespace: shop}, spec: {selector: {app: web}}}
- {apiVersion: v1, kind: Service, metadata: {name: api, namespace: shop}, spec: {selector: {app: api}}}
- {apiVersion: networking.k8s.io/v1, kind: Ingress, metadata: {name: web, namespace: shop}, spec: {ingressClassName: nginx, tls: [{secretName: ingress-tls}], rules: [{http: {paths: [{backend: {service: {name: web}}}]}}]}}
- {apiVersion: networking.k8s.io/v1, kind: IngressClass, metadata: {name: nginx}}
- {apiVersion: gateway.networking.k8s.io/v1, kind: HTTPRoute, metadata: {name: web, namespace: shop}, spec: {rules: [{backendRefs: [{name: web}]}]}}
- {apiVersion: autoscaling/v2, kind: HorizontalPodAutoscaler, metadata: {name: web, namespace: shop}, spec: {scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: web}}}
- {apiVersion: policy/v1, kind: PodDisruptionBudget, metadata: {name: web, namespace: shop}, spec: {selector: {matchExpressions: [{key: tier, operator: In, values: [front]}]}}}
- {apiVersion: networking.k8s.io/v1, kind: NetworkPolicy, metadata: {name: web, namespace: shop}, spec: {podSelector: {matchLabels: {app: web}}}}
- {apiVersion: networking.k8s.io/v1, kind: NetworkPolicy, metadata: {name: api, namespace: shop}, spec: {podSelector: {matchLabels: {app: api}}}}
- {apiVersion: example.com/v1, kind: Widget, metadata: {name: w, namespace: other}}
- {apiVersion: apiextensions.k8s.io/v1, kind: CustomResourceDefinition, metadata: {name: widgets.example.com}}
`

func inventory(t *testing.T) *model.Inventory {
	t.Helper()
	var docs []map[string]any
	if err := yaml.Unmarshal([]byte(cluster), &docs); err != nil {
		t.Fatal(err)
	}
	inv := &model.Inventory{}
	for _, d := range docs {
		u := &unstructured.Unstructured{Object: d}
		gvk := u.GroupVersionKind()
		res := "widgets"
		if gvk.Kind != "Widget" {
			res = "x"
		}
		inv.Add(&model.Object{Ref: model.RefOf(u), GVR: schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: res}, Namespaced: u.GetNamespace() != "", Live: u})
	}
	return inv
}

func included(inv *model.Inventory) []string {
	var out []string
	for _, o := range inv.Included() {
		out = append(out, o.Ref.String())
	}
	sort.Strings(out)
	return out
}

func TestSelectionClosure(t *testing.T) {
	inv := inventory(t)
	cfg := config.New()
	cfg.Scope.Resources = []string{"deployment/shop/web"}
	cfg.Scope.DisableDefaultRules = []string{"*"}
	if err := filter.Releases(inv, cfg); err != nil {
		t.Fatal(err)
	}
	if err := filter.Apply(inv, filter.Options{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	Resolve(inv, cfg)
	want := []string{
		"ClusterRole.rbac.authorization.k8s.io/viewer",
		"ClusterRoleBinding.rbac.authorization.k8s.io/web-viewer",
		"ConfigMap/shop/env",
		"ConfigMap/shop/files",
		"Deployment.apps/shop/web",
		"HTTPRoute.gateway.networking.k8s.io/shop/web",
		"HorizontalPodAutoscaler.autoscaling/shop/web",
		"Ingress.networking.k8s.io/shop/web",
		"IngressClass.networking.k8s.io/nginx",
		"Namespace/shop",
		"NetworkPolicy.networking.k8s.io/shop/web",
		"PersistentVolumeClaim/shop/data",
		"PodDisruptionBudget.policy/shop/web",
		"PriorityClass.scheduling.k8s.io/high",
		"Role.rbac.authorization.k8s.io/shop/reader",
		"RoleBinding.rbac.authorization.k8s.io/shop/web-reader",
		"Secret/shop/db",
		"Secret/shop/ingress-tls",
		"Secret/shop/registry",
		"Secret/shop/tls",
		"Service/shop/web",
		"ServiceAccount/shop/web",
		"StorageClass.storage.k8s.io/fast",
	}
	got := included(inv)
	if len(got) != len(want) {
		t.Fatalf("included %d objects:\n%v\nwant %d:\n%v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("included[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestNamespaceScopePullsClusterObjects(t *testing.T) {
	inv := inventory(t)
	cfg := config.New()
	cfg.Scope.Namespaces = []string{"other"}
	cfg.Scope.DisableDefaultRules = []string{"*"}
	_ = filter.Releases(inv, cfg)
	if err := filter.Apply(inv, filter.Options{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	Resolve(inv, cfg)
	got := included(inv)
	want := []string{"CustomResourceDefinition.apiextensions.k8s.io/widgets.example.com", "Namespace/other", "Widget.example.com/other/w"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("included = %v, want %v", got, want)
	}
}
