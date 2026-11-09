package target

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/cached/memory"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/restmapper"
	clienttesting "k8s.io/client-go/testing"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/kube"
	"github.com/hayone1/gitopsify/pkg/model"
	"github.com/hayone1/gitopsify/pkg/render"
)

// liveTarget is a cluster that serves ConfigMaps and Widgets and holds objs.
func liveTarget(objs ...runtime.Object) *kube.Client {
	disc := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
	verbs := metav1.Verbs{"get", "list"}
	disc.Resources = []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: verbs},
			{Name: "secrets", Kind: "Secret", Namespaced: true, Verbs: verbs}}},
		{GroupVersion: "example.com/v1", APIResources: []metav1.APIResource{{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: verbs}}},
	}
	lists := map[schema.GroupVersionResource]string{
		{Version: "v1", Resource: "configmaps"}:                    "ConfigMapList",
		{Group: "example.com", Version: "v1", Resource: "widgets"}: "WidgetList",
	}
	return &kube.Client{Discovery: disc, Mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disc)),
		Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, objs...)}
}

func cm(name string, annotations map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": "shop"}, "data": map[string]any{"k": "v"}}}
	if annotations != nil {
		u.Object["metadata"].(map[string]any)["annotations"] = annotations
	}
	return u
}

// shopInventory is one app, shop, with objects a..d and a Widget.
func shopInventory() *model.Inventory {
	inv := &model.Inventory{}
	app := &model.App{Name: "shop", Namespace: "shop"}
	for _, u := range []*unstructured.Unstructured{cm("a", nil), cm("b", nil), cm("c", nil), cm("d", nil),
		{Object: map[string]any{"apiVersion": "example.com/v1", "kind": "Widget", "metadata": map[string]any{"name": "w", "namespace": "shop"}, "spec": map[string]any{}}}} {
		o := &model.Object{Ref: model.RefOf(u), Namespaced: true, Live: u, Out: u.DeepCopy(), App: "shop"}
		inv.Add(o)
		app.Objects = append(app.Objects, o)
	}
	inv.Apps = []*model.App{app}
	return inv
}

func TestMarkExisting(t *testing.T) {
	// In the target: a (another Argo CD app's), b (nobody's), d (this
	// migration's own app's, after its first sync), w (an operator's).
	ctrl := true
	w := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "example.com/v1", "kind": "Widget", "metadata": map[string]any{"name": "w", "namespace": "shop"}}}
	w.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "example.com/v1", Kind: "Platform", Name: "main", UID: "1", Controller: &ctrl}})
	c := liveTarget(cm("a", map[string]any{"argocd.argoproj.io/tracking-id": "platform:/ConfigMap:shop/a"}), cm("b", nil),
		cm("d", map[string]any{"argocd.argoproj.io/tracking-id": "uat-shop:/ConfigMap:shop/d"}), w)
	env := &config.Environment{Name: "uat"}
	skipped := func(mode string) []string {
		t.Helper()
		cfg := config.New()
		cfg.Output.Tool = config.ToolArgoCD
		cfg.Adopt.Existing = mode
		cfg.Default()
		inv := shopInventory()
		notes, err := MarkExisting(context.Background(), inv, cfg, env, c)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, o := range inv.Objects {
			if why, ok := o.Skip["uat"]; ok {
				out = append(out, o.Ref.Name+"="+why)
			}
		}
		if len(out) > 0 && (len(notes) == 0 || !strings.Contains(notes[0], "left out")) {
			t.Errorf("%s: notes %v", mode, notes)
		}
		return out
	}
	if got := strings.Join(skipped(config.ExistingSkipOwned), ","); got != "a=managed there by Argo CD application platform,w=managed there by Platform main" {
		t.Errorf("skip-owned: %s", got)
	}
	if got := strings.Join(skipped(config.ExistingSkip), ","); got != "a=managed there by Argo CD application platform,b=exists in the cluster,w=managed there by Platform main" {
		t.Errorf("skip (never its own d): %s", got)
	}
	if got := skipped(config.ExistingInclude); len(got) != 0 {
		t.Errorf("include: %v", got)
	}
}

// TestSkippedObjectsLeaveOverlays: an object left out of one environment is
// deleted from its overlay by kustomize, and stays in the other's.
func TestSkippedObjectsLeaveOverlays(t *testing.T) {
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skip("no kubectl")
	}
	cfg := config.New()
	cfg.Environments = []config.Environment{{Name: "uat"}, {Name: "prod"}}
	cfg.Environments[1].Transforms = []config.Transform{{Type: "labels", Set: map[string]string{"env": "prod"}}}
	cfg.Default()
	inv := shopInventory()
	for _, o := range inv.Objects {
		if o.Ref.Name == "a" || o.Ref.Name == "w" {
			o.Skip = map[string]string{"uat": "exists in the cluster"}
		}
	}
	tree, err := render.Render(inv, render.Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := tree.Write(dir, render.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	build := func(env string) string {
		out, err := exec.Command(kubectl, "kustomize", filepath.Join(dir, "apps", "shop", "envs", env)).CombinedOutput()
		if err != nil {
			t.Fatalf("kustomize %s: %v\n%s", env, err, out)
		}
		return string(out)
	}
	uat, prod := build("uat"), build("prod")
	for _, name := range []string{"name: a", "name: w"} {
		if strings.Contains(uat, name+"\n") {
			t.Errorf("uat still has %s:\n%s", name, uat)
		}
		if !strings.Contains(prod, name+"\n") {
			t.Errorf("prod lost %s:\n%s", name, prod)
		}
	}
	if !strings.Contains(uat, "name: b\n") || strings.Contains(uat, "$patch") {
		t.Errorf("uat:\n%s", uat)
	}
	if _, err := os.Stat(filepath.Join(dir, "apps", "shop", "envs", "uat", "delete-configmap-a.yaml")); err != nil {
		t.Errorf("no delete patch: %v", err)
	}
}

// TestMarkExistingEdges: kinds the target doesn't serve yet are simply not
// there; Argo CD's label tracking and apps in other namespaces are
// recognized as this migration's own; a Secret a SealedSecret would make
// counts as the SealedSecret being there.
func TestMarkExistingEdges(t *testing.T) {
	inv := shopInventory()
	app := inv.Apps[0]
	add := func(u *unstructured.Unstructured, envOut *unstructured.Unstructured) {
		o := &model.Object{Ref: model.RefOf(u), Namespaced: true, Live: u, Out: u.DeepCopy(), App: "shop"}
		if envOut != nil {
			o.Out, o.EnvOut = nil, map[string]*unstructured.Unstructured{"uat": envOut}
		}
		inv.Add(o)
		app.Objects = append(app.Objects, o)
	}
	add(&unstructured.Unstructured{Object: map[string]any{"apiVersion": "example.com/v1", "kind": "Gadget", "metadata": map[string]any{"name": "g", "namespace": "shop"}}}, nil)
	add(&unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "db", "namespace": "shop"}}},
		&unstructured.Unstructured{Object: map[string]any{"apiVersion": "bitnami.com/v1alpha1", "kind": "SealedSecret", "metadata": map[string]any{"name": "db", "namespace": "shop"}}})
	byLabel := cm("a", nil)
	byLabel.SetLabels(map[string]string{"app.kubernetes.io/instance": "uat-shop"})
	elsewhere := cm("b", map[string]any{"argocd.argoproj.io/tracking-id": "apps_uat-shop:/ConfigMap:shop/b"})
	secret := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "db", "namespace": "shop"}}}
	c := liveTarget(byLabel, elsewhere, secret)
	cfg := config.New()
	cfg.Output.Tool = config.ToolArgoCD
	cfg.Adopt.Existing = config.ExistingSkip
	cfg.Default()
	if _, err := MarkExisting(context.Background(), inv, cfg, &config.Environment{Name: "uat"}, c); err != nil {
		t.Fatalf("an unserved kind failed the run: %v", err)
	}
	var got []string
	for _, o := range inv.Objects {
		if _, ok := o.Skip["uat"]; ok {
			got = append(got, o.Ref.Kind+"/"+o.Ref.Name)
		}
	}
	if strings.Join(got, ",") != "Secret/db" {
		t.Errorf("skipped %v; want only the Secret the sealed one would make", got)
	}
}
