package config

import (
	"errors"
	"os"
	"strings"
	"testing"
)

const migrationDoc = `# yaml-language-server: $schema=x
# Our migration: keep this comment.
apiVersion: gitopsify.io/v1alpha1
kind: Migration
metadata: {name: test-to-uat}   # flow style stays
spec:
  source: {context: test}
`

const otherExport = `apiVersion: gitopsify.io/v1alpha1
kind: Export
metadata: {name: keycloak}
spec:
  provider: {name: keycloak}
`

func TestSaveExportReplacesOnlyItsDocument(t *testing.T) {
	file := migrationDoc + "---\n# Grafana, saved by the wizard\n" + exportYAML + "# about keycloak, below\n\n---\n" + otherExport
	p := write(t, "gitopsify.yaml", file)
	x, err := LoadExport(p, "grafana")
	if err != nil {
		t.Fatal(err)
	}
	id := IDOf(x)
	x.Default()
	x.Spec.Select.Kinds = []string{"Dashboard"}
	b, err := SaveExport(p, x, id, ExportSchemaHeader, "# header\n")
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.HasPrefix(got, migrationDoc+"---\n# Grafana, saved by the wizard\napiVersion: gitopsify.io/v1alpha1\n") || !strings.HasSuffix(got, "# about keycloak, below\n\n---\n"+otherExport) {
		t.Fatalf("other documents or comments changed:\n%s", got)
	}
	if strings.Contains(got, "# header") || strings.Count(got, "yaml-language-server") != 1 {
		t.Errorf("a whole-file header was added:\n%s", got)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := LoadExport(p, "grafana")
	if err != nil || strings.Join(again.Spec.Select.Kinds, ",") != "Dashboard" {
		t.Fatalf("%+v %v", again, err)
	}
	if c, err := Load(p); err != nil || c.Metadata.Name != "test-to-uat" {
		t.Errorf("migration: %+v %v", c, err)
	}

	// The same file saved again changes nothing more.
	b2, err := SaveExport(p, again, IDOf(again), ExportSchemaHeader, "")
	if err != nil || string(b2) != string(b) {
		t.Errorf("second save differs (%v):\n%s", err, b2)
	}
}

func TestSaveExportAlone(t *testing.T) {
	x, err := LoadExport(write(t, "e.yaml", exportYAML), "")
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"missing": "", "only-export": exportYAML} {
		p := t.TempDir() + "/gitopsify-grafana.yaml"
		if content != "" {
			p = write(t, "gitopsify-grafana.yaml", content)
		}
		b, err := SaveExport(p, x, IDOf(x), ExportSchemaHeader, "# header\n")
		if err != nil || !strings.HasPrefix(string(b), ExportSchemaHeader+"# header\napiVersion:") {
			t.Errorf("%s: %v\n%s", name, err, b)
		}
	}
}

func TestSaveExportRefusesToRewriteOthers(t *testing.T) {
	jsonList := `{"apiVersion": "v1", "kind": "List", "items": [
  {"apiVersion": "gitopsify.io/v1alpha1", "kind": "Migration", "metadata": {"name": "m"}, "spec": {}},
  {"apiVersion": "gitopsify.io/v1alpha1", "kind": "Export", "metadata": {"name": "grafana-prod"}, "spec": {"provider": {"name": "grafana"}}}]}`
	for name, content := range map[string]string{
		"x.json": jsonList,
		// A document that starts on its separator line can't be cut out.
		"x.yaml": migrationDoc + "--- {apiVersion: gitopsify.io/v1alpha1, kind: Export, metadata: {name: grafana-prod}, spec: {provider: {name: grafana}}}\n---\n" + otherExport,
	} {
		p := write(t, name, content)
		x, err := LoadExport(p, "grafana")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := SaveExport(p, x, IDOf(x), ExportSchemaHeader, ""); !errors.Is(err, ErrNotInPlace) {
			t.Errorf("%s: want ErrNotInPlace, got %v", name, err)
		}
	}
}
