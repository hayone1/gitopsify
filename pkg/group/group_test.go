package group

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/model"
)

func TestName(t *testing.T) {
	a := Name(strings.Repeat("x", 60) + "-alpha")
	b := Name(strings.Repeat("x", 60) + "-beta")
	if len(a) > 63 || len(b) > 63 || a == b {
		t.Errorf("long names: %q, %q", a, b)
	}
	if Name("Shop_App") != "shop-app" || Name("--") != "app" {
		t.Errorf("sanitizing: %q %q", Name("Shop_App"), Name("--"))
	}
}

func TestReservedNames(t *testing.T) {
	inv := &model.Inventory{}
	for _, ns := range []string{"cluster", "crds", "shop"} {
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "c", "namespace": ns}}}
		inv.Add(&model.Object{Ref: model.RefOf(u), Namespaced: true, Live: u})
	}
	crd := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]any{"name": "a.b"}}}
	inv.Add(&model.Object{Ref: model.RefOf(crd), Live: crd})
	cfg := config.New()
	if err := Run(inv, cfg); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range inv.Apps {
		names = append(names, a.Name+"="+string(a.Phase))
	}
	if got := strings.Join(names, " "); got != "cluster-ns=apps crds=crds crds-ns=apps shop=apps" {
		t.Errorf("apps: %s", got)
	}
}
