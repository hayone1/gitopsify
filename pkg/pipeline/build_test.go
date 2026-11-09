package pipeline

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"sigs.k8s.io/yaml"

	"github.com/hayone1/gitopsify/pkg/helm"
)

// TestGoldenKustomizationsBuild runs `kubectl kustomize` on every generated
// Kustomization that builds offline (no helmCharts, no remote resources).
func TestGoldenKustomizationsBuild(t *testing.T) {
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skip("kubectl not installed")
	}
	root := filepath.Join("..", "..", "testdata", "golden")
	built := 0
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "kustomization.yaml" {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dir := filepath.Dir(p)
		if bytes.Contains(data, []byte("https://")) || filepath.Base(dir) == "all" {
			return nil // remote resources, or aggregates of charts pulled from repositories
		}
		args := []string{"kustomize", dir}
		if bytes.Contains(data, []byte("helmCharts:")) {
			// Vendored charts render offline; charts from repositories need the network.
			if bytes.Contains(data, []byte("repo: ")) {
				return nil
			}
			if _, err := exec.LookPath("helm"); err != nil {
				return nil
			}
			args = append(args, "--enable-helm")
		}
		out, err := exec.Command(kubectl, args...).CombinedOutput()
		if err != nil && bytes.Contains(out, []byte("unknown shorthand flag: 'c'")) {
			// kustomize runs "helm version -c", which Helm 4 removed.
			t.Logf("skipping %s: this kubectl's kustomize can't drive the installed Helm 4", dir)
			return nil
		}
		if err != nil {
			t.Errorf("kubectl kustomize %s: %v\n%s", dir, err, out)
			return nil
		}
		built++
		// Patch directives kustomize doesn't apply end up in the objects.
		for _, d := range []string{"$setElementOrder", "$deleteFromPrimitiveList", "$retainKeys", "$patch"} {
			if bytes.Contains(out, []byte(d)) {
				t.Errorf("kubectl kustomize %s: the output has %s", dir, d)
			}
		}
		// Flux values ConfigMaps get hashed names; the HelmRelease must
		// reference the hashed names through the nameReference config.
		if bytes.Contains(out, []byte("kind: HelmRelease")) {
			var names []string
			for _, u := range helm.ManifestObjects(string(out)) {
				if u.GetKind() == "ConfigMap" {
					names = append(names, u.GetName())
				}
			}
			for _, u := range helm.ManifestObjects(string(out)) {
				if u.GetKind() != "HelmRelease" {
					continue
				}
				vf, _ := u.Object["spec"].(map[string]any)["valuesFrom"].([]any)
				for _, v := range vf {
					name := v.(map[string]any)["name"].(string)
					optional, _ := v.(map[string]any)["optional"].(bool)
					found := false
					for _, n := range names {
						found = found || n == name
					}
					if !found && !optional {
						t.Errorf("%s: HelmRelease references ConfigMap %s, built ConfigMaps are %v", dir, name, names)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if built < 20 {
		t.Errorf("only %d Kustomizations built", built)
	}
}

// TestApplicationSetTemplatePatch executes the ApplicationSet's
// templatePatch the way the ApplicationSet controller does, for every app
// type, and checks the result is a valid Application spec.
func TestApplicationSetTemplatePatch(t *testing.T) {
	for _, c := range []struct {
		golden    string
		automated bool
	}{{"argocd-applicationset", false}, {"argocd-applicationset-unordered", true}} {
		t.Run(c.golden, func(t *testing.T) {
			testTemplatePatch(t, filepath.Join("..", "..", "testdata", "golden", c.golden, "argocd", "uat"), c.automated)
		})
	}
}

func testTemplatePatch(t *testing.T, dir string, automated bool) {
	data, err := os.ReadFile(filepath.Join(dir, "apps", "applicationset.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var appset struct {
		Spec struct {
			TemplatePatch string `json:"templatePatch"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(data, &appset); err != nil {
		t.Fatal(err)
	}
	tmpl, err := template.New("patch").Option("missingkey=error").Parse(appset.Spec.TemplatePatch)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "config", "*.json"))
	types := map[string]bool{}
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		params := map[string]any{}
		if err := json.Unmarshal(raw, &params); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := tmpl.Execute(&out, params); err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		var patch struct {
			Spec struct {
				Source  map[string]any   `json:"source"`
				Sources []map[string]any `json:"sources"`
				Sync    struct {
					Automated   map[string]any `json:"automated"`
					SyncOptions []string       `json:"syncOptions"`
				} `json:"syncPolicy"`
			} `json:"spec"`
		}
		if err := yaml.UnmarshalStrict(out.Bytes(), &patch); err != nil {
			t.Errorf("%s: invalid YAML: %v\n%s", f, err, out.String())
			continue
		}
		typ := params["type"].(string)
		types[typ] = true
		switch typ {
		case "kustomize":
			if patch.Spec.Source["path"] != params["appPath"] {
				t.Errorf("%s: source = %v", f, patch.Spec.Source)
			}
		case "helm":
			if len(patch.Spec.Sources) != 2 || patch.Spec.Sources[0]["chart"] != params["chart"] || patch.Spec.Sources[1]["ref"] != "values" {
				t.Errorf("%s: sources = %v", f, patch.Spec.Sources)
			}
			vf := patch.Spec.Sources[0]["helm"].(map[string]any)["valueFiles"].([]any)
			if !strings.HasPrefix(vf[0].(string), "$values/") {
				t.Errorf("%s: valueFiles = %v", f, vf)
			}
		case "helm-git":
			if len(patch.Spec.Sources) != 2 || patch.Spec.Sources[0]["path"] != params["appPath"] {
				t.Errorf("%s: sources = %v", f, patch.Spec.Sources)
			}
		}
		if len(patch.Spec.Sync.SyncOptions) == 0 || patch.Spec.Sync.SyncOptions[0] != "ServerSideApply=true" {
			t.Errorf("%s: syncOptions = %v", f, patch.Spec.Sync.SyncOptions)
		}
		if (patch.Spec.Sync.Automated != nil) != automated || (automated && patch.Spec.Sync.Automated["selfHeal"] != true) {
			t.Errorf("%s: automated = %v", f, patch.Spec.Sync.Automated)
		}
	}
	for _, typ := range []string{"kustomize", "helm", "helm-git"} {
		if !types[typ] {
			t.Errorf("no app of type %s in the fixture", typ)
		}
	}
}
