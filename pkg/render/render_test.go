package render

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
