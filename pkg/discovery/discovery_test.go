package discovery

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/hayone1/gitopsify/pkg/kube"
)

func TestListRetriesAndTolerance(t *testing.T) {
	Backoff = func(int) time.Duration { return 0 }
	cms := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	widgets := schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}
	cm := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a", "namespace": "shop"}}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{cms: "ConfigMapList", widgets: "WidgetList"}, cm)
	calls := 0
	dyn.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls < 3 {
			return true, nil, apierrors.NewServiceUnavailable("etcd leader changed")
		}
		return false, nil, nil
	})
	dyn.PrependReactor("list", "widgets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errString("conversion webhook for example.com/v1 failed"))
	})
	c := &kube.Client{Dynamic: dyn}
	objs, err := list(context.Background(), c, Resource{GVR: cms, Kind: "ConfigMap", Namespaced: true}, "")
	if err != nil || len(objs) != 1 || calls != 3 {
		t.Fatalf("configmaps after retries: %d objects, %d calls, %v", len(objs), calls, err)
	}
	if _, err := list(context.Background(), c, Resource{GVR: widgets, Kind: "Widget", Namespaced: true}, ""); err == nil {
		t.Fatal("expected the widgets error")
	}
	if !builtinGroup("") || !builtinGroup("apps") || !builtinGroup("networking.k8s.io") || builtinGroup("example.com") || builtinGroup("cluster.x-k8s.io") {
		t.Error("builtinGroup")
	}
	if !retryable(apierrors.NewTooManyRequests("slow down", 1)) || retryable(apierrors.NewForbidden(cms.GroupResource(), "", errString("no"))) {
		t.Error("retryable")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestReadObjectsLists(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/dump.yaml"
	data := "apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: ConfigMap\n  metadata: {name: a, namespace: shop}\n---\r\napiVersion: v1\r\nkind: Secret\r\nmetadata: {name: b, namespace: shop}\r\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	objs, err := ReadObjects([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, o := range objs {
		names = append(names, o.GetKind()+"/"+o.GetName())
	}
	if strings.Join(names, ",") != "ConfigMap/a,Secret/b" {
		t.Errorf("objects: %v", names)
	}
}
