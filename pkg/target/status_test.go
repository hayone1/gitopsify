package target

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/kube"
)

func obj(apiVersion, kind, ns, name string, labels map[string]any, status map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": labels, "generation": int64(1)},
		"status":   status,
	}}
}

func fakeClient(objs ...runtime.Object) *kube.Client {
	lists := map[schema.GroupVersionResource]string{
		gvrApplication: "ApplicationList", gvrKustomization: "KustomizationList", gvrHelmRelease: "HelmReleaseList",
	}
	return &kube.Client{Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, objs...)}
}

func TestArgoStatus(t *testing.T) {
	dir := t.TempDir()
	apps := filepath.Join(dir, "argocd", "uat", "apps")
	if err := os.MkdirAll(apps, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "argocd", "uat", "root.yaml"), []byte("apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata: {name: gitopsify-uat}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(apps, "shop.yaml"), []byte("apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata: {name: uat-shop}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(apps, "web.yaml"), []byte("apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata: {name: uat-web}\n"), 0o644)
	env := map[string]any{"gitopsify.io/env": "uat"}
	c := fakeClient(
		obj("argoproj.io/v1alpha1", "Application", "argocd", "gitopsify-uat", env, map[string]any{"sync": map[string]any{"status": "Synced"}, "health": map[string]any{"status": "Healthy"}}),
		obj("argoproj.io/v1alpha1", "Application", "argocd", "uat-shop", env, map[string]any{
			"sync": map[string]any{"status": "OutOfSync"}, "health": map[string]any{"status": "Degraded"},
			"operationState": map[string]any{"phase": "Failed", "message": "one or more objects failed to apply"},
		}),
		obj("argoproj.io/v1alpha1", "Application", "argocd", "prod-shop", map[string]any{"gitopsify.io/env": "prod"}, nil),
	)
	cfg := config.New()
	cfg.Output.Tool = config.ToolArgoCD
	cfg.Environments[0].Name = "uat"
	st, err := Status(context.Background(), cfg, &cfg.Environments[0], c, dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]AppStatus{}
	for _, s := range st {
		got[s.Name] = s
	}
	if len(st) != 3 || !got["argocd/gitopsify-uat"].Ready || got["argocd/uat-shop"].Ready || got["argocd/uat-web"].Health != "Missing" {
		t.Errorf("status: %+v", st)
	}
	if got["argocd/uat-shop"].Message != "one or more objects failed to apply" {
		t.Errorf("message: %q", got["argocd/uat-shop"].Message)
	}
	if AllReady(st) {
		t.Error("not all ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Wait(ctx, cfg, &cfg.Environments[0], c, dir, time.Second, nil); err == nil {
		t.Error("Wait must fail while apps are not ready")
	}
}

func TestFluxStatus(t *testing.T) {
	dir := t.TempDir()
	cfg := config.New()
	cfg.Output.Tool = config.ToolFlux
	cfg.Environments[0].Name = "uat"
	ready := func(status string, observed int64) map[string]any {
		return map[string]any{"observedGeneration": observed, "lastAppliedRevision": "main@sha1:abc",
			"conditions": []any{map[string]any{"type": "Ready", "status": status, "message": "msg"}}}
	}
	wave := map[string]any{"gitopsify.io/wave": "w0"}
	c := fakeClient(
		obj("kustomize.toolkit.fluxcd.io/v1", "Kustomization", "flux-system", "flux-system", nil, ready("True", 1)),
		obj("kustomize.toolkit.fluxcd.io/v1", "Kustomization", "flux-system", "shop", wave, ready("True", 0)),
		obj("kustomize.toolkit.fluxcd.io/v1", "Kustomization", "flux-system", "redis", wave, ready("False", 1)),
		obj("helm.toolkit.fluxcd.io/v2", "HelmRelease", "data", "redis", map[string]any{"kustomize.toolkit.fluxcd.io/name": "redis", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}, ready("True", 1)),
	)
	st, err := Status(context.Background(), cfg, &cfg.Environments[0], c, dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]AppStatus{}
	for _, s := range st {
		got[s.Kind+" "+s.Name] = s
	}
	if !got["Kustomization flux-system/flux-system"].Ready || got["Kustomization flux-system/shop"].Health != "Progressing" ||
		got["Kustomization flux-system/redis"].Health != "NotReady" || !got["HelmRelease data/redis"].Ready {
		t.Errorf("status: %+v", st)
	}
}
