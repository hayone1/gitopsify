package order

import (
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/model"
)

func crd(name string) *model.Object {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]any{"name": name}}}
	return &model.Object{Ref: model.RefOf(u), Live: u, Out: u}
}

func cr(group, kind, resource, ns, name string) *model.Object {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": group + "/v1", "kind": kind, "metadata": map[string]any{"name": name, "namespace": ns}}}
	return &model.Object{Ref: model.RefOf(u), GVR: schema.GroupVersionResource{Group: group, Version: "v1", Resource: resource}, Namespaced: true, Live: u, Out: u}
}

func deploy(ns string) *model.Object {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "d", "namespace": ns}}}
	return &model.Object{Ref: model.RefOf(u), GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, Namespaced: true, Live: u, Out: u}
}

func apps() *model.Inventory {
	return &model.Inventory{Apps: []*model.App{
		{Name: "crds", Phase: model.PhaseCRDs, Objects: []*model.Object{crd("widgets.example.com")}},
		{Name: "namespaces", Phase: model.PhaseNamespaces},
		{Name: "cert-manager", Phase: model.PhasePlatform, Objects: []*model.Object{crd("certificates.cert-manager.io")}},
		{Name: "shop", Phase: model.PhaseApps, Objects: []*model.Object{deploy("shop"), cr("example.com", "Widget", "widgets", "shop", "w"), cr("cert-manager.io", "Certificate", "certificates", "shop", "c")}},
		{Name: "db", Phase: model.PhaseApps, Objects: []*model.Object{deploy("db")}},
	}}
}

func waves(inv *model.Inventory) map[string]int {
	out := map[string]int{}
	for _, a := range inv.Apps {
		out[a.Name] = a.Wave
	}
	return out
}

func TestInferredAndExplicit(t *testing.T) {
	inv := apps()
	cfg := config.New()
	cfg.Ordering.DependsOn = map[string][]string{"shop": {"db"}}
	if err := Run(inv, cfg); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"crds": -30, "namespaces": -20, "cert-manager": -5, "db": 0, "shop": 1}
	if got := waves(inv); !reflect.DeepEqual(got, want) {
		t.Errorf("waves = %v, want %v", got, want)
	}
	shop := inv.App("shop")
	if !reflect.DeepEqual(shop.DependsOn, []string{"cert-manager", "crds", "db"}) || !shop.SkipDryRun {
		t.Errorf("shop dependsOn = %v skipDryRun = %v", shop.DependsOn, shop.SkipDryRun)
	}
	if inv.App("db").SkipDryRun {
		t.Error("db has no custom resources")
	}
}

func TestPinnedWaveAndCycles(t *testing.T) {
	inv := apps()
	cfg := config.New()
	cfg.Ordering.Waves = map[string]int{"d*": 7, "shop": -50}
	if err := Run(inv, cfg); err != nil {
		t.Fatal(err)
	}
	if w := waves(inv); w["db"] != 7 || w["shop"] != -4 {
		t.Errorf("waves = %v (shop is pinned before its CRD owners and gets moved after them)", w)
	}
	if len(inv.Warnings) != 1 || !strings.Contains(inv.Warnings[0], "shop is pinned") {
		t.Errorf("warnings = %v", inv.Warnings)
	}

	inv = apps()
	cfg = config.New()
	cfg.Ordering.DependsOn = map[string][]string{"db": {"shop"}, "shop": {"db"}}
	if err := Run(inv, cfg); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("cycle not detected: %v", err)
	}
}

func TestSealedSecretsDependOnController(t *testing.T) {
	inv := apps()
	inv.Apps = append(inv.Apps, &model.App{Name: "sealed-secrets", Phase: model.PhasePlatform, Objects: []*model.Object{crd("sealedsecrets.bitnami.com")}})
	sec := cr("", "Secret", "secrets", "db", "pw")
	sec.Out = &unstructured.Unstructured{Object: map[string]any{"apiVersion": "bitnami.com/v1alpha1", "kind": "SealedSecret", "metadata": map[string]any{"name": "pw"}}}
	inv.App("db").Objects = append(inv.App("db").Objects, sec)
	if err := Run(inv, config.New()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inv.App("db").DependsOn, []string{"sealed-secrets"}) {
		t.Errorf("db dependsOn = %v", inv.App("db").DependsOn)
	}
}

func TestDisabled(t *testing.T) {
	inv := apps()
	cfg := config.New()
	f := false
	cfg.Ordering.Enabled = &f
	if err := Run(inv, cfg); err != nil {
		t.Fatal(err)
	}
	for _, a := range inv.Apps {
		if a.Wave != 0 || a.DependsOn != nil {
			t.Errorf("%s: wave %d deps %v", a.Name, a.Wave, a.DependsOn)
		}
	}
}

// Overlapping wave patterns: the most specific decides, every time.
func TestWavePatternsDeterministic(t *testing.T) {
	for range 20 {
		inv := apps()
		cfg := config.New()
		cfg.Ordering.Waves = map[string]int{"*": 5, "d*": -1, "db": 3}
		if err := Run(inv, cfg); err != nil {
			t.Fatal(err)
		}
		if w := waves(inv); w["db"] != 3 {
			t.Fatalf("db is in wave %d, want 3 (its exact name)", w["db"])
		}
	}
}

// Two apps using each other's custom resources don't fail the scan.
func TestMutualCRDs(t *testing.T) {
	inv := &model.Inventory{Apps: []*model.App{
		{Name: "a", Phase: model.PhaseApps, Objects: []*model.Object{crd("foos.a.io"), cr("b.io", "Bar", "bars", "x", "b")}},
		{Name: "b", Phase: model.PhaseApps, Objects: []*model.Object{crd("bars.b.io"), cr("a.io", "Foo", "foos", "x", "f")}},
	}}
	if err := Run(inv, config.New()); err != nil {
		t.Fatal(err)
	}
	if len(inv.Warnings) != 1 || !strings.Contains(inv.Warnings[0], "each other's custom resources") {
		t.Errorf("warnings: %v", inv.Warnings)
	}
	if !inv.App("a").SkipDryRun || !inv.App("b").SkipDryRun {
		t.Error("both apps need SkipDryRun")
	}
}
