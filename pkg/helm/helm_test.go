package helm

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hayone1/gitopsify/pkg/model"
)

// ReleaseSecret builds a Helm release storage Secret the way Helm writes it:
// JSON, gzipped, base64 (Helm's), base64 again (Secret data).
func ReleaseSecret(t *testing.T, rel map[string]any, status string) *unstructured.Unstructured {
	t.Helper()
	raw, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(raw)
	_ = w.Close()
	helmEncoded := base64.StdEncoding.EncodeToString(buf.Bytes())
	name, ns := rel["name"].(string), rel["namespace"].(string)
	version := int(rel["version"].(float64))
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": ReleaseSecretType,
		"metadata": map[string]any{
			"name": "sh.helm.release.v1." + name + ".v" + itoa(version), "namespace": ns,
			"labels": map[string]any{"owner": "helm", "name": name, "status": status, "version": itoa(version)},
		},
		"data": map[string]any{"release": base64.StdEncoding.EncodeToString([]byte(helmEncoded))},
	}}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func release(version int, replicas int) map[string]any {
	return map[string]any{
		"name": "web", "namespace": "shop", "version": float64(version),
		"info":   map[string]any{"status": "deployed"},
		"config": map[string]any{"replicaCount": replicas},
		"chart": map[string]any{
			"metadata":  map[string]any{"name": "web", "version": "1.2.3", "appVersion": "2.0"},
			"values":    map[string]any{"replicaCount": 1, "image": "nginx"},
			"templates": []any{map[string]any{"name": "templates/deploy.yaml", "data": base64.StdEncoding.EncodeToString([]byte("kind: Deployment"))}},
			"files": []any{
				map[string]any{"name": "crds/widgets.yaml", "data": base64.StdEncoding.EncodeToString([]byte("apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: widgets.example.com\n"))},
				map[string]any{"name": "README.md", "data": base64.StdEncoding.EncodeToString([]byte("# web"))},
			},
		},
		"manifest": "---\n# Source: web/templates/deploy.yaml\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  replicas: 2\n  template:\n    spec:\n      containers:\n        - name: web\n          resources:\n            limits:\n              memory: 1024Mi\n---\napiVersion: v1\nkind: Service\nmetadata:\n  name: web\n  namespace: shop\n---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: web\n",
	}
}

func TestLatestPicksDeployedRevision(t *testing.T) {
	old := ReleaseSecret(t, release(1, 1), "superseded")
	cur := ReleaseSecret(t, release(2, 2), "deployed")
	failed := ReleaseSecret(t, release(3, 3), "failed")
	rels, warnings := Latest([]*unstructured.Unstructured{old, failed, cur})
	if len(warnings) > 0 || len(rels) != 1 {
		t.Fatalf("releases = %d, warnings = %v", len(rels), warnings)
	}
	r := rels[0]
	if r.Revision != 2 || r.Key() != "shop/web" || r.Chart.Name != "web" || r.Chart.Version != "1.2.3" {
		t.Errorf("release = %+v", r)
	}
	if !reflect.DeepEqual(r.Values, map[string]any{"replicaCount": float64(2)}) {
		t.Errorf("values = %v", r.Values)
	}
	if got := CRDNames(r); !reflect.DeepEqual(got, []string{"widgets.example.com"}) {
		t.Errorf("CRDNames = %v", got)
	}
	if !IsStorage(cur) {
		t.Error("IsStorage should recognize release secrets")
	}
}

func TestManifestRefs(t *testing.T) {
	rel := release(1, 1)
	namespaced := func(group, kind string) bool { return kind != "ClusterRole" }
	refs := ManifestRefs(rel["manifest"].(string), "shop", namespaced)
	want := []model.Ref{
		{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "web"},
		{Kind: "Service", Namespace: "shop", Name: "web"},
		{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "web"},
	}
	if !reflect.DeepEqual(refs, want) {
		t.Errorf("refs = %v", refs)
	}
}

func TestDrift(t *testing.T) {
	rels, _ := Latest([]*unstructured.Unstructured{ReleaseSecret(t, release(1, 1), "deployed")})
	live := map[model.Ref]*unstructured.Unstructured{
		{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "web"}: {Object: map[string]any{
			"spec": map[string]any{"replicas": int64(5), "template": map[string]any{"spec": map[string]any{"containers": []any{
				map[string]any{"name": "web", "resources": map[string]any{"limits": map[string]any{"memory": "1Gi"}}, "imagePullPolicy": "Always"},
			}}}},
		}},
		{Kind: "Service", Namespace: "shop", Name: "web"}: {Object: map[string]any{}},
	}
	drift := Drift(rels[0], func(r model.Ref) *unstructured.Unstructured { return live[r] }, func(_, kind string) bool { return kind != "ClusterRole" })
	dep := drift[model.Ref{Group: "apps", Kind: "Deployment", Namespace: "shop", Name: "web"}]
	if !reflect.DeepEqual(dep, []string{"spec.replicas"}) {
		t.Errorf("deployment drift = %v (quantities 1024Mi/1Gi are equal; extra live fields ignored)", dep)
	}
	if got := drift[model.Ref{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "web"}]; len(got) != 1 || !strings.Contains(got[0], "missing") {
		t.Errorf("missing object drift = %v", got)
	}
	if _, ok := drift[model.Ref{Kind: "Service", Namespace: "shop", Name: "web"}]; ok {
		t.Error("service has no drift")
	}
}

func TestVendorFiles(t *testing.T) {
	rels, _ := Latest([]*unstructured.Unstructured{ReleaseSecret(t, release(1, 1), "deployed")})
	files, err := VendorFiles(rels[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"Chart.yaml", "values.yaml", "templates/deploy.yaml", "crds/widgets.yaml", "README.md"} {
		if _, ok := files[f]; !ok {
			t.Errorf("missing %s", f)
		}
	}
	if !strings.Contains(string(files["Chart.yaml"]), "apiVersion: v2") {
		t.Errorf("Chart.yaml = %s", files["Chart.yaml"])
	}
	rels[0].Chart.Dependencies = []model.ChartDependency{{Name: "redis"}}
	if _, err := VendorFiles(rels[0]); err == nil {
		t.Error("charts with subcharts can't be vendored")
	}
}

func TestResolveLocalAndConfig(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "repositories.yaml")
	_ = os.WriteFile(cfgFile, []byte("repositories:\n- name: a\n  url: https://a.example.com/charts/\n- name: b\n  url: https://b.example.com\n- name: a2\n  url: https://a.example.com/charts\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "a-index.yaml"), []byte("entries:\n  web:\n  - version: 1.2.3\n  - version: 1.2.2\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "a2-index.yaml"), []byte("entries:\n  web:\n  - version: 1.2.3\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "b-index.yaml"), []byte("entries:\n  other:\n  - version: 1.2.3\n"), 0o600)
	r := &Resolver{Order: []string{"local"}, RepoConfig: cfgFile, RepoCache: dir, Repositories: map[string]string{"pinned": "oci://ghcr.io/acme/charts/"}}

	repo, _, err := r.Resolve(context.Background(), "web", "1.2.3")
	if err != nil || repo == nil || repo.URL != "https://a.example.com/charts" || strings.Contains(repo.Source, "also") {
		t.Errorf("web 1.2.3 = %+v, %v (duplicate URLs under two names count once)", repo, err)
	}
	if repo, _, _ := r.Resolve(context.Background(), "web", "9.9.9"); repo != nil {
		t.Errorf("unknown version resolved to %+v", repo)
	}
	repo, _, _ = r.Resolve(context.Background(), "pinned", "1.0.0")
	if repo == nil || repo.URL != "oci://ghcr.io/acme/charts" || !repo.IsOCI() || repo.Source != "config" {
		t.Errorf("pinned = %+v", repo)
	}
}

func TestResolveArtifactHub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/packages/search":
			_, _ = w.Write([]byte(`{"packages":[
				{"name":"web","repository":{"name":"community","url":"https://community.example.com"}},
				{"name":"web","repository":{"name":"official","url":"https://official.example.com","official":true}},
				{"name":"web-extra","repository":{"name":"x","url":"https://x.example.com"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/1.2.3"):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	r := &Resolver{Order: []string{"artifacthub"}, ArtifactHubURL: srv.URL, RepoConfig: filepath.Join(t.TempDir(), "none.yaml")}
	repo, warnings, err := r.Resolve(context.Background(), "web", "1.2.3")
	if err != nil || repo == nil || repo.URL != "https://official.example.com" {
		t.Fatalf("repo = %+v, err = %v", repo, err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "community.example.com") {
		t.Errorf("warnings = %v", warnings)
	}
}

func TestSplitDocsSkipsCommentOnlyDocs(t *testing.T) {
	docs := SplitDocs("---\n# Source: x\n---\nkind: A\n---\n\n---\nkind: B\n")
	if len(docs) != 2 {
		t.Errorf("docs = %q", docs)
	}
}
