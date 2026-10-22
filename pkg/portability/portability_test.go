package portability

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/model"
)

func inventory(t *testing.T, docs ...string) *model.Inventory {
	t.Helper()
	inv := &model.Inventory{}
	for _, d := range docs {
		m := map[string]any{}
		if err := yaml.Unmarshal([]byte(d), &m); err != nil {
			t.Fatal(err)
		}
		u := &unstructured.Unstructured{Object: m}
		inv.Add(&model.Object{Ref: model.RefOf(u), Namespaced: u.GetNamespace() != "", Live: u, Out: u})
	}
	return inv
}

func TestCheck(t *testing.T) {
	docs := []string{
		`{apiVersion: apps/v1, kind: Deployment, metadata: {name: web, namespace: shop}, spec: {template: {spec: {
		  nodeSelector: {kubernetes.io/hostname: node-3},
		  volumes: [{name: cache, hostPath: {path: /var/cache/web}}],
		  containers: [{name: web, image: "localhost:5000/web:1"}, {name: side, image: "nginx:1.27"}, {name: reg, image: "image-registry.openshift-image-registry.svc:5000/shop/api:2"}]}}}}`,
		`{apiVersion: v1, kind: PersistentVolume, metadata: {name: data}, spec: {local: {path: /mnt/disk1}}}`,
		`{apiVersion: v1, kind: PersistentVolume, metadata: {name: nfs}, spec: {nfs: {server: 10.0.0.5, path: /export}}}`,
		`{apiVersion: v1, kind: PersistentVolumeClaim, metadata: {name: restored, namespace: shop}, spec: {dataSource: {kind: VolumeSnapshot, name: nightly}}}`,
		`{apiVersion: v1, kind: Service, metadata: {name: lb, namespace: shop}, spec: {type: LoadBalancer, loadBalancerIP: 203.0.113.7}}`,
		`{apiVersion: v1, kind: ServiceAccount, metadata: {name: api, namespace: shop, annotations: {eks.amazonaws.com/role-arn: "arn:aws:iam::111:role/api"}}}`,
		`{apiVersion: apps/v1, kind: Deployment, metadata: {name: ok, namespace: shop}, spec: {template: {spec: {containers: [{name: ok, image: "ghcr.io/acme/ok:1"}]}}}}`,
	}
	inv := inventory(t, docs...)
	Check(inv, config.New())
	got := map[string]string{}
	for _, o := range inv.Objects {
		got[o.Ref.String()] = strings.Join(o.Warnings, " | ")
	}
	for ref, want := range map[string][]string{
		"Deployment.apps/shop/web":            {"selects node node-3", "hostPath /var/cache/web", "localhost:5000/web:1", "openshift-image-registry"},
		"PersistentVolume/data":               {"local volume at /mnt/disk1"},
		"PersistentVolume/nfs":                {"statically provisioned nfs"},
		"PersistentVolumeClaim/shop/restored": {"VolumeSnapshot nightly"},
		"Service/shop/lb":                     {"203.0.113.7"},
		"ServiceAccount/shop/api":             {"AWS IAM role"},
	} {
		for _, w := range want {
			if !strings.Contains(got[ref], w) {
				t.Errorf("%s: missing %q in %q", ref, w, got[ref])
			}
		}
	}
	if got["Deployment.apps/shop/ok"] != "" || strings.Contains(got["Deployment.apps/shop/web"], "nginx") {
		t.Errorf("false positives: %q / %q", got["Deployment.apps/shop/ok"], got["Deployment.apps/shop/web"])
	}

	takeover := inventory(t, docs...)
	cfg := config.New()
	cfg.Adopt.Enabled = true
	Check(takeover, cfg)
	for _, o := range takeover.Objects {
		if len(o.Warnings) > 0 {
			t.Errorf("takeover keeps the environment; no warnings expected for %s: %v", o.Ref, o.Warnings)
		}
	}
}
