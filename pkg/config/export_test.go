package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const exportYAML = `apiVersion: gitopsify.io/v1alpha1
kind: Export
metadata:
  name: grafana-prod
spec:
  provider:
    name: grafana
    version: v0.1.0
  instances:
    - name: ops
      settings:
        url: https://grafana.example.com
        auth: {env: GRAFANA_AUTH}
  select:
    kinds: [Folder, Dashboard]
  output:
    formats: [terraform]
`

const exportHCL = `apiVersion = "gitopsify.io/v1alpha1"
kind       = "Export"
metadata   = { name = "grafana-prod" }
spec = {
  provider  = { name = "grafana", version = "v0.1.0" }
  instances = [{
    name     = "ops"
    settings = { url = "https://grafana.example.com", auth = { env = "GRAFANA_AUTH" } }
  }]
  select = { kinds = ["Folder", "Dashboard"] }
  output = { formats = ["terraform"] }
}
`

const exportJSONList = `{"apiVersion": "v1", "kind": "List", "items": [
  {"apiVersion": "gitopsify.io/v1alpha1", "kind": "Export", "metadata": {"name": "grafana-prod"},
   "spec": {"provider": {"name": "grafana", "version": "v0.1.0"},
            "instances": [{"name": "ops", "settings": {"url": "https://grafana.example.com", "auth": {"env": "GRAFANA_AUTH"}}}],
            "select": {"kinds": ["Folder", "Dashboard"]}, "output": {"formats": ["terraform"]}}}
]}`

func write(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadExportInEverySyntax(t *testing.T) {
	for name, content := range map[string]string{"export.yaml": exportYAML, "export.hcl": exportHCL, "export.json": exportJSONList} {
		t.Run(name, func(t *testing.T) {
			x, err := LoadExport(write(t, name, content), "")
			if err != nil {
				t.Fatal(err)
			}
			x.Default()
			if err := x.Validate(); err != nil {
				t.Fatal(err)
			}
			in := x.Spec.Instances[0]
			if x.Metadata.Name != "grafana-prod" || in.Name != "ops" || in.Settings["auth"].Env != "GRAFANA_AUTH" ||
				in.Settings["url"].Value != "https://grafana.example.com" || strings.Join(x.Spec.Select.Kinds, ",") != "Folder,Dashboard" {
				t.Errorf("got %+v", x)
			}
		})
	}
}

func TestOneFileForEverything(t *testing.T) {
	multi := `apiVersion: gitopsify.io/v1alpha1
kind: Migration
metadata: {name: test-to-uat}
spec:
  source: {context: test}
---
` + exportYAML + `---
apiVersion: gitopsify.io/v1alpha1
kind: Export
metadata: {name: keycloak}
spec:
  provider: {name: keycloak}
`
	p := write(t, "gitopsify.yaml", multi)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Source.Context != "test" || c.Kind != Kind {
		t.Errorf("migration %+v", c)
	}
	if _, err := LoadExport(p, ""); err == nil || !strings.Contains(err.Error(), "several exports") {
		t.Errorf("want several exports, got %v", err)
	}
	x, err := LoadExport(p, "keycloak")
	if err != nil || x.Spec.Provider.Name != "keycloak" {
		t.Errorf("%+v %v", x, err)
	}
	x, err = LoadExport(p, "grafana")
	if err != nil || x.Metadata.Name != "grafana-prod" {
		t.Errorf("%+v %v", x, err)
	}
}

func TestMigrationKeepsItsFlatForm(t *testing.T) {
	c, err := Load(write(t, "gitopsify.yaml", "apiVersion: gitopsify.io/v1alpha1\nkind: Migration\nsource:\n  context: test\n"))
	if err != nil || c.Source.Context != "test" {
		t.Fatalf("%+v %v", c, err)
	}
	_, err = Load(write(t, "gitopsify.yaml", "kind: Migration\nsource:\n  token: abc\n"))
	if err == nil || !strings.Contains(err.Error(), "tokens don't belong") {
		t.Fatalf("err = %v", err)
	}
	_, err = Load(write(t, "x.yaml", exportYAML))
	if err == nil || !strings.Contains(err.Error(), "provider export") {
		t.Fatalf("err = %v", err)
	}
}

func TestExportRejects(t *testing.T) {
	for content, want := range map[string]string{
		strings.Replace(exportYAML, "auth: {env: GRAFANA_AUTH}", "auth: {env: A, file: b}", 1): "exactly one",
		strings.Replace(exportYAML, "formats: [terraform]", "formats: [pulumi]", 1):            "pulumi",
		strings.Replace(exportYAML, "  select:", "  bogus: 1\n  select:", 1):                   "unknown field",
	} {
		x, err := LoadExport(write(t, "e.yaml", content), "")
		if err == nil {
			x.Default()
			err = x.Validate()
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
	_, err := LoadExport(write(t, "e.hcl", "spec {\n}\n"), "")
	if err == nil || !strings.Contains(err.Error(), "as an attribute") {
		t.Errorf("HCL blocks: %v", err)
	}
	_, err = LoadExport(write(t, "e.hcl", "kind = var.x\n"), "")
	if err == nil || !strings.Contains(err.Error(), "literal") {
		t.Errorf("HCL variables: %v", err)
	}
}

// TestExportSchemaIsCurrent keeps docs/gitopsify-export.schema.json in step.
func TestExportSchemaIsCurrent(t *testing.T) {
	want, err := ExportSchema()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../../docs/gitopsify-export.schema.json")
	if err != nil || string(got) != string(want) {
		if os.Getenv("UPDATE_DOCS") != "" {
			if err := os.WriteFile("../../docs/gitopsify-export.schema.json", want, 0o644); err != nil {
				t.Fatal(err)
			}
			return
		}
		t.Error("docs/gitopsify-export.schema.json is out of date: run make docs")
	}
}

// TestMigrationDecodingStaysCompatible: values convert to the fields' types
// and duplicate keys are errors, as before resources could be written in
// several syntaxes.
func TestMigrationDecodingStaysCompatible(t *testing.T) {
	c, err := Load(write(t, "gitopsify.yaml", "kind: Migration\nscope:\n  labelSelector: 4.5\n"))
	if err != nil || c.Scope.LabelSelector != "4.5" {
		t.Fatalf("%+v %v", c, err)
	}
	_, err = Load(write(t, "gitopsify.yaml", "kind: Migration\nsource:\n  context: a\n  context: b\n"))
	if err == nil {
		t.Fatal("duplicate keys were accepted")
	}
	c, err = Load(write(t, "gitopsify.yaml", "apiVersion: gitopsify.io/v1alpha1\nkind: Migration\nmetadata: {name: x}\nspec:\n  scope:\n    labelSelector: 2.5\n"))
	if err != nil || c.Scope.LabelSelector != "2.5" {
		t.Fatalf("resource form: %+v %v", c, err)
	}
}
