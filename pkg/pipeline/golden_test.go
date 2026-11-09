package pipeline

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/discovery"
	"github.com/hayone1/gitopsify/pkg/helm"
	"github.com/hayone1/gitopsify/pkg/render"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

const fixture = "../../testdata/clusters/clickops.yaml"

// cases cover every tool, Argo CD pattern and layout on the same fixture.
var cases = map[string]func(c *config.Config){
	"none-folder-per-env": func(c *config.Config) {
		c.Environments = []config.Environment{
			{Name: "uat", Transforms: []config.Transform{{Type: "replace", From: "test.example.com", To: "uat.example.com"}}},
			{Name: "prod", Transforms: []config.Transform{
				{Type: "replace", From: "test.example.com", To: "example.com"},
				{Type: "set", Target: &config.Selector{Kinds: []string{"Deployment"}, Names: []string{"api"}}, Path: "spec.replicas", Value: 3},
				{Type: "set", Path: "values.replicaCount", Value: 2, Target: &config.Selector{Apps: []string{"podinfo"}}},
				{Type: "storageClass", Map: map[string]string{"fast": "premium"}},
			}},
		}
	},
	"argocd-app-of-apps": func(c *config.Config) {
		c.Output.Tool = config.ToolArgoCD
		c.Output.Repo.URL = "https://github.com/acme/gitops.git"
		c.Environments = []config.Environment{{Name: "uat", Cluster: config.Cluster{Server: "https://uat.example.com:6443"}}}
	},
	"argocd-applicationset": func(c *config.Config) {
		c.Output.Tool = config.ToolArgoCD
		c.Output.ArgoCD.Pattern = config.PatternApplicationSet
		c.Output.Repo.URL = "https://github.com/acme/gitops.git"
		c.Environments = []config.Environment{{Name: "uat"}}
	},
	// Unordered: Argo CD's automated sync instead of RollingSync.
	"argocd-applicationset-unordered": func(c *config.Config) {
		c.Output.Tool = config.ToolArgoCD
		c.Output.ArgoCD.Pattern = config.PatternApplicationSet
		c.Output.Repo.URL = "https://github.com/acme/gitops.git"
		off := false
		c.Ordering.Enabled = &off
		c.Environments = []config.Environment{{Name: "uat"}}
	},
	"argocd-single": func(c *config.Config) {
		c.Output.Tool = config.ToolArgoCD
		c.Output.ArgoCD.Pattern = config.PatternSingle
		c.Output.Repo.URL = "https://github.com/acme/gitops.git"
		c.Output.Repo.Path = "platform"
		c.Environments = []config.Environment{{Name: "uat"}}
	},
	"argocd-adopt": func(c *config.Config) {
		c.Output.Tool = config.ToolArgoCD
		c.Output.Repo.URL = "https://github.com/acme/gitops.git"
		c.Adopt.Enabled = true
		c.Environments = []config.Environment{{Name: "prod"}}
	},
	"flux-repo-per-env": func(c *config.Config) {
		c.Output.Tool = config.ToolFlux
		c.Output.Layout = config.LayoutRepoPerEnv
		c.Secrets.Strategy = config.SecretsExternalSecrets
		c.Secrets.ExternalSecrets.StoreName = "vault"
		c.Environments = []config.Environment{
			{Name: "uat", RepoURL: "git@github.com:acme/gitops-uat.git"},
			{Name: "prod", RepoURL: "git@github.com:acme/gitops-prod.git", Transforms: []config.Transform{{Type: "image", From: "nginx:", To: "registry.example.com/nginx:"}}},
		}
	},
	"selection": func(c *config.Config) {
		c.Scope.Resources = []string{"deployment/shop/web"}
		c.Environments = []config.Environment{{Name: "uat"}}
	},
}

func TestGolden(t *testing.T) {
	objs, err := discovery.ReadObjects([]string{fixture})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(cases))
	for n := range cases {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Helm.Repositories = map[string]string{"podinfo": "https://stefanprodan.github.io/podinfo"}
			cases[name](cfg)
			cfg.Default()
			res, err := Generate(context.Background(), Options{
				Config:      cfg,
				Offline:     deepCopies(objs),
				OfflineName: "clickops.yaml",
				Resolver:    &helm.Resolver{Repositories: cfg.Helm.Repositories},
				Version:     "test",
			})
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join("..", "..", "testdata", "golden", name)
			if *update {
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				if _, err := res.Tree.Write(dir, render.WriteOptions{Force: true}); err != nil {
					t.Fatal(err)
				}
				// Only the manifests: checksums would change with every file.
				_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
					if err == nil && filepath.Base(filepath.Dir(p)) == ".gitopsify" && d.Name() != "generated.txt" {
						_ = os.Remove(p)
					}
					return nil
				})
				return
			}
			want := map[string]bool{}
			_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					rel, _ := filepath.Rel(dir, p)
					want[filepath.ToSlash(rel)] = true
				}
				return nil
			})
			// Write's state (the output's, and each repository's).
			for p := range want {
				if p == ".gitopsify" || strings.HasPrefix(p, ".gitopsify/") || strings.Contains(p, "/.gitopsify/") {
					delete(want, p)
				}
			}
			for _, p := range res.Tree.Paths() {
				got, _ := res.Tree.Get(p)
				exp, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
				if err != nil {
					t.Errorf("%s: unexpected file (run go test ./pkg/pipeline -update)", p)
					continue
				}
				delete(want, p)
				if !bytes.Equal(got, exp) {
					t.Errorf("%s differs from the golden file (run go test ./pkg/pipeline -update):\n%s", p, firstDiff(string(exp), string(got)))
				}
			}
			for p := range want {
				t.Errorf("%s: missing from the output", p)
			}
		})
	}
}

func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return "line " + strconv.Itoa(i+1) + ":\n- " + x + "\n+ " + y
		}
	}
	return ""
}

// deepCopies copies objects, since the pipeline modifies them.
func deepCopies(objs []*unstructured.Unstructured) []*unstructured.Unstructured {
	out := make([]*unstructured.Unstructured, len(objs))
	for i, o := range objs {
		out[i] = o.DeepCopy()
	}
	return out
}
