package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/strategicpatch"

	"github.com/hayone1/gitopsify/pkg/model"
)

func TestDiffValues(t *testing.T) {
	base := map[string]any{
		"replicas": 1,
		"image":    map[string]any{"tag": "1.0", "repository": "web"},
		"ingress":  map[string]any{"hosts": []any{"a"}},
		"gone":     true,
	}
	full := map[string]any{
		"replicas": 1,
		"image":    map[string]any{"tag": "1.1", "repository": "web"},
		"ingress":  map[string]any{"hosts": []any{"b"}},
		"new":      "x",
	}
	want := map[string]any{
		"image":   map[string]any{"tag": "1.1"},
		"ingress": map[string]any{"hosts": []any{"b"}},
		"gone":    nil,
		"new":     "x",
	}
	if got := DiffValues(base, full); !reflect.DeepEqual(got, want) {
		t.Errorf("DiffValues = %v, want %v", got, want)
	}
	if got := DiffValues(base, base); len(got) != 0 {
		t.Errorf("no differences expected, got %v", got)
	}
}

func TestRelPath(t *testing.T) {
	for _, c := range []struct{ dir, target, want string }{
		{"apps/web/envs/uat", "apps/web/base", "../../base"},
		{"apps/web/envs/uat", "apps/web/charts", "../../charts"},
		{"argocd/uat/all", "apps/web/envs/uat", "../../../apps/web/envs/uat"},
		{"a", "a", "."},
	} {
		if got := relPath(c.dir, c.target); got != c.want {
			t.Errorf("relPath(%s, %s) = %s, want %s", c.dir, c.target, got, c.want)
		}
	}
}

func TestMergePatch(t *testing.T) {
	base := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "web", "namespace": "shop", "labels": map[string]any{"a": "1"}},
		"spec":     map[string]any{"replicas": int64(1), "paused": true},
	}}
	env := base.DeepCopy()
	env.Object["spec"] = map[string]any{"replicas": int64(3)}
	p, err := mergePatch(base, env)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "web", "namespace": "shop"},
		"spec":     map[string]any{"replicas": float64(3), "paused": nil},
	}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("patch = %v, want %v", p, want)
	}
}

func TestFluxURL(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:acme/gitops.git":     "ssh://git@github.com/acme/gitops.git",
		"https://github.com/acme/gitops.git": "https://github.com/acme/gitops.git",
		"ssh://git@host:2222/a/b.git":        "ssh://git@host:2222/a/b.git",
	} {
		if got := FluxURL(in); got != want {
			t.Errorf("FluxURL(%s) = %s", in, got)
		}
	}
}

func TestTreeWriteStaysInside(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "victim-"+filepath.Base(dir))
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)
	if err := os.MkdirAll(filepath.Join(dir, ".gitopsify"), 0o755); err != nil {
		t.Fatal(err)
	}
	evil := "../" + filepath.Base(outside) + "\n" + outside + "\nold.yaml\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitopsify", "generated.txt"), []byte(evil), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old.yaml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := NewTree()
	tr.Add("new.yaml", []byte("y"))
	wr, err := tr.Write(dir, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	removed := wr.Removed
	if !reflect.DeepEqual(removed, []string{"old.yaml"}) {
		t.Errorf("removed %v", removed)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a file outside the output was removed: %v", err)
	}
}

func TestLongFileNames(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": strings.Repeat("a", 253), "namespace": "shop"}}}
	o := &model.Object{Ref: model.RefOf(u), Namespaced: true, Out: u}
	name := fileName(o, &model.App{Name: "shop"})
	if len(name) > 200 || !strings.HasSuffix(name, ".yaml") {
		t.Errorf("file name %d bytes: %s", len(name), name)
	}
	u2 := u.DeepCopy()
	u2.SetName(strings.Repeat("a", 252) + "b")
	if fileName(&model.Object{Ref: model.RefOf(u2), Namespaced: true, Out: u2}, &model.App{Name: "shop"}) == name {
		t.Error("different long names must get different files")
	}
}

// An overlay patch, applied the way kustomize applies it (a strategic merge
// patch for built-in kinds), gives the environment's object: list items the
// environment removed are removed.
func TestMergePatchLists(t *testing.T) {
	base := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "web", "namespace": "shop"},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{
			map[string]any{"name": "web", "image": "web:1",
				"env":       []any{map[string]any{"name": "DEBUG", "value": "1"}, map[string]any{"name": "HOST", "value": "test.example.com"}},
				"ports":     []any{map[string]any{"containerPort": int64(80)}, map[string]any{"containerPort": int64(9090)}},
				"resources": map[string]any{"limits": map[string]any{"cpu": "1"}},
			},
		}}}},
	}}
	env := base.DeepCopy()
	c := env.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	c["env"] = []any{map[string]any{"name": "HOST", "value": "example.com"}}
	c["ports"] = []any{map[string]any{"containerPort": int64(80)}}
	delete(c, "resources")
	patch, err := mergePatch(base, env)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(base.Object)
	p, _ := json.Marshal(patch)
	// kustomize would copy these into the object.
	if strings.Contains(string(p), "$setElementOrder") {
		t.Errorf("the patch has an order directive: %s", p)
	}
	got, err := strategicpatch.StrategicMergePatch(b, p, &appsv1.Deployment{})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(env.Object)
	var g, w any
	_ = json.Unmarshal(got, &g)
	_ = json.Unmarshal(want, &w)
	if !reflect.DeepEqual(g, w) {
		t.Errorf("kustomize would produce\n%s\nwant\n%s", got, want)
	}
}
