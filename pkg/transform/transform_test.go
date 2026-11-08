package transform

import (
	"encoding/base64"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/model"
)

func object(t *testing.T, y string) (*unstructured.Unstructured, *model.Object) {
	t.Helper()
	m := map[string]any{}
	if err := yaml.Unmarshal([]byte(y), &m); err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: m}
	return u, &model.Object{Ref: model.RefOf(u), Live: u.DeepCopy(), App: "shop"}
}

const workload = `
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop, labels: {env: test}}
spec:
  replicas: 1
  template:
    spec:
      containers:
        - name: web
          image: test.azurecr.io/shop/web:1.0
          env: [{name: URL, value: https://web.test.example.com}, {name: DB, value: db.shop.svc.cluster.local}]
        - name: sidecar
          image: docker.io/envoy:1
`

func TestObjectTransforms(t *testing.T) {
	u, o := object(t, workload)
	changed, err := Object(u, o, []config.Transform{
		{Type: "replace", From: "test.example.com", To: "uat.example.com"},
		{Type: "image", From: "test.azurecr.io/", To: "uat.azurecr.io/"},
		{Type: "set", Path: "spec.replicas", Value: 3},
		{Type: "set", Path: "spec.template.spec.containers[name=sidecar].image", Value: "docker.io/envoy:2"},
		{Type: "labels", Set: map[string]string{"env": "uat"}},
		{Type: "namespace", Map: map[string]string{"shop": "shop-uat"}},
		{Type: "replace", From: "never", To: "x", Target: &config.Selector{Kinds: []string{"Service"}}},
	})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	cs := u.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
	web, side := cs[0].(map[string]any), cs[1].(map[string]any)
	if web["image"] != "uat.azurecr.io/shop/web:1.0" || side["image"] != "docker.io/envoy:2" {
		t.Errorf("images = %v, %v", web["image"], side["image"])
	}
	env := web["env"].([]any)
	if env[0].(map[string]any)["value"] != "https://web.uat.example.com" || env[1].(map[string]any)["value"] != "db.shop-uat.svc.cluster.local" {
		t.Errorf("env = %v", env)
	}
	if r, _, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "replicas"); r != float64(3) && r != 3 {
		t.Errorf("replicas = %v", r)
	}
	if u.GetNamespace() != "shop-uat" || u.GetLabels()["env"] != "uat" {
		t.Errorf("namespace = %s labels = %v", u.GetNamespace(), u.GetLabels())
	}
	if u.GetName() != "web" {
		t.Error("replace must not rename objects")
	}
}

func TestSecretDataReplace(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString
	u, o := object(t, `
apiVersion: v1
kind: Secret
metadata: {name: s, namespace: shop}
data: {url: `+enc([]byte("https://db.test.example.com"))+`, bin: /wA=}
`)
	if _, err := Object(u, o, []config.Transform{{Type: "replace", From: "test.example.com", To: "uat.example.com"}}); err != nil {
		t.Fatal(err)
	}
	data, _, _ := unstructured.NestedStringMap(u.Object, "data")
	if data["url"] != enc([]byte("https://db.uat.example.com")) || data["bin"] != "/wA=" {
		t.Errorf("data = %v", data)
	}
}

func TestRemoveAndJSONPatch(t *testing.T) {
	u, o := object(t, workload)
	_, err := Object(u, o, []config.Transform{
		{Type: "remove", Path: "spec.template.spec.containers[1]"},
		{Type: "jsonPatch", Patch: []map[string]any{{"op": "add", "path": "/metadata/annotations", "value": map[string]any{"a": "b"}}}},
		{Type: "annotations", Remove: []string{"a"}, Set: map[string]string{"c": "d"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cs, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
	if len(cs) != 1 {
		t.Errorf("containers = %d", len(cs))
	}
	if !reflect.DeepEqual(u.GetAnnotations(), map[string]string{"c": "d"}) {
		t.Errorf("annotations = %v", u.GetAnnotations())
	}
}

func TestStorageAndIngressClass(t *testing.T) {
	u, o := object(t, `
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db, namespace: shop}
spec:
  volumeClaimTemplates: [{metadata: {name: data}, spec: {storageClassName: gp2}}]
`)
	if _, err := Object(u, o, []config.Transform{{Type: "storageClass", Map: map[string]string{"gp2": "premium"}}}); err != nil {
		t.Fatal(err)
	}
	tpls, _, _ := unstructured.NestedSlice(u.Object, "spec", "volumeClaimTemplates")
	if sc := tpls[0].(map[string]any)["spec"].(map[string]any)["storageClassName"]; sc != "premium" {
		t.Errorf("storageClassName = %v", sc)
	}
	ing, io := object(t, `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: web, namespace: shop, annotations: {kubernetes.io/ingress.class: nginx}}
spec: {ingressClassName: nginx}
`)
	if _, err := Object(ing, io, []config.Transform{{Type: "ingressClass", Map: map[string]string{"nginx": "traefik"}}}); err != nil {
		t.Fatal(err)
	}
	if c, _, _ := unstructured.NestedString(ing.Object, "spec", "ingressClassName"); c != "traefik" || ing.GetAnnotations()["kubernetes.io/ingress.class"] != "traefik" {
		t.Errorf("ingress class = %s / %v", c, ing.GetAnnotations())
	}
}

func TestValues(t *testing.T) {
	values := map[string]any{
		"image":       map[string]any{"registry": "test.azurecr.io", "repository": "shop/web"},
		"ingress":     map[string]any{"className": "nginx", "hosts": []any{"web.test.example.com"}},
		"persistence": map[string]any{"storageClass": "gp2"},
		"replicas":    1,
	}
	app := &model.App{Name: "web"}
	out, err := Values(values, app, []config.Transform{
		{Type: "replace", From: "test.example.com", To: "uat.example.com"},
		{Type: "image", From: "test.azurecr.io/", To: "uat.azurecr.io/"},
		{Type: "ingressClass", Map: map[string]string{"nginx": "traefik"}},
		{Type: "storageClass", Map: map[string]string{"gp2": "premium"}},
		{Type: "set", Path: "values.replicas", Value: 3},
		{Type: "set", Path: "values.replicas", Value: 9, Target: &config.Selector{Apps: []string{"other"}}},
		{Type: "set", Path: "spec.replicas", Value: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"image":       map[string]any{"registry": "uat.azurecr.io", "repository": "shop/web"},
		"ingress":     map[string]any{"className": "traefik", "hosts": []any{"web.uat.example.com"}},
		"persistence": map[string]any{"storageClass": "premium"},
		"replicas":    3,
	}
	if !jsonEq(out, want) {
		t.Errorf("values = %v", out)
	}
	if values["replicas"] != 1 {
		t.Error("Values must not modify its input")
	}
}

func TestParsePath(t *testing.T) {
	segs, err := parsePath(`metadata.annotations["example.com/key"].x[2][name=a]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 6 || segs[2].key != "example.com/key" || segs[3].key != "x" || !segs[4].hasIndex || segs[4].index != 2 || segs[5].selKey != "name" || segs[5].selValue != "a" {
		t.Errorf("segments = %+v", segs)
	}
	if _, err := parsePath("a[1"); err == nil {
		t.Error("unclosed bracket accepted")
	}
}

func jsonEq(a, b any) bool { return reflect.DeepEqual(normalize(a), normalize(b)) }

func normalize(v any) any {
	b, _ := yaml.Marshal(v)
	var out any
	_ = yaml.Unmarshal(b, &out)
	return out
}

// A Secret turned into another kind would skip the secrets strategy.
func TestSecretKeepsKind(t *testing.T) {
	u, o := object(t, "apiVersion: v1\nkind: Secret\nmetadata: {name: db, namespace: shop}\ndata: {password: aHVudGVyMg==}\n")
	_, err := Object(u, o, []config.Transform{{Type: "set", Path: "kind", Value: "ConfigMap"}})
	if err == nil {
		t.Fatal("a Secret became a ConfigMap")
	}
	u, o = object(t, "apiVersion: v1\nkind: Secret\nmetadata: {name: db, namespace: shop}\ndata: {password: aHVudGVyMg==}\n")
	if _, err := Object(u, o, []config.Transform{{Type: "namespace", Map: map[string]string{"shop": "store"}}}); err != nil || u.GetNamespace() != "store" {
		t.Fatalf("namespace: %v %s", err, u.GetNamespace())
	}
}

// Protected Secrets keep their namespace, name and data.
func TestProtectedSecret(t *testing.T) {
	const secret = "apiVersion: v1\nkind: Secret\nmetadata: {name: db, namespace: shop, labels: {host: a.example.com}}\ndata: {password: aHVudGVyMg==}\nstringData: {url: https://a.example.com}\n"
	u, o := object(t, secret)
	o.Protected = true
	if _, err := Object(u, o, []config.Transform{{Type: "replace", From: "^h.*", To: "", Regex: true}, {Type: "replace", From: "a.example.com", To: "b.example.com"}}); err != nil {
		t.Fatal(err)
	}
	if d, _, _ := unstructured.NestedString(u.Object, "data", "password"); d != "aHVudGVyMg==" {
		t.Errorf("data replaced: %s", d)
	}
	if s, _, _ := unstructured.NestedString(u.Object, "stringData", "url"); s != "https://a.example.com" {
		t.Errorf("stringData replaced: %s", s)
	}
	if u.GetLabels()["host"] != "b.example.com" {
		t.Errorf("labels: %v", u.GetLabels())
	}
	for _, tr := range []config.Transform{
		{Type: "namespace", Map: map[string]string{"shop": "mine"}},
		{Type: "set", Path: "metadata.name", Value: "other"},
		{Type: "jsonPatch", Patch: []map[string]any{{"op": "replace", "path": "/metadata/namespace", "value": "mine"}}},
	} {
		u, o := object(t, secret)
		o.Protected = true
		if _, err := Object(u, o, []config.Transform{tr}); err == nil {
			t.Errorf("%s moved a protected Secret", tr.Type)
		}
	}
	// Unprotected (the CLI): namespaces move and data is replaced.
	u, o = object(t, secret)
	if _, err := Object(u, o, []config.Transform{{Type: "namespace", Map: map[string]string{"shop": "mine"}}, {Type: "replace", From: "hunter2", To: "x"}}); err != nil || u.GetNamespace() != "mine" {
		t.Errorf("unprotected: %v %s", err, u.GetNamespace())
	}
}
