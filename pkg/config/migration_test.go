package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// richConfig has values that are easy to get wrong in one syntax or
// another: template sequences, multi-line text, keys that aren't
// identifiers or are HCL keywords (true, null, for), words YAML 1.1 reads as
// booleans, numbers and free-form values.
func richConfig() *Config {
	c := New()
	c.Metadata = ObjectMeta{Name: "shop.prod", Labels: map[string]string{"team": "a"}, Annotations: map[string]string{"example.com/note": "x"}}
	c.Source.Context = "kind-test"
	c.Scope.Namespaces = []string{"shop", "no", "on", "y", "Off", "true"}
	c.Helm.Repositories = map[string]string{"null": "https://charts.example.com", "my.repo": "oci://r.example.com/c", "true": "https://t.example.com", "for": "https://f.example.com", "yes": "n"}
	c.Ordering.Waves = map[string]int{"Namespace": -5, "for": 1}
	c.Transforms = []Transform{
		{Type: "replace", From: "${registry}", To: "%{if x}y%{endif}"},
		{Type: "replace", From: "off", To: "NO"},
		{Type: "set", Path: "spec.replicas", Value: float64(3)},
		{Type: "set", Path: "spec.enabled", Value: "yes"},
		{Type: "jsonPatch", Patch: []map[string]any{{"op": "add", "path": "/metadata/labels/a", "value": "line one\nline two\n"}}},
		{Type: "labels", Set: map[string]string{"app.kubernetes.io/part-of": "shop <&>", "enabled": "Yes", "Y": "1.0"}},
	}
	return c
}

// TestControlCharactersRoundTrip: JSON and HCL files are read through the
// YAML parser, which refuses some characters encoding/json writes raw.
func TestControlCharactersRoundTrip(t *testing.T) {
	want := New()
	want.Transforms = []Transform{{Type: "replace", From: "a\x7fb\u0085c\u009f", To: "d\uffffe"}}
	for _, name := range []string{"gitopsify.yaml", "gitopsify.json", "gitopsify.hcl"} {
		b, err := Encode(name, want.Document(), "", "")
		if err != nil {
			t.Fatal(err)
		}
		got, err := Load(write(t, name, string(b)))
		if err != nil {
			t.Fatalf("%s: %v\n%q", name, err, b)
		}
		got.Default()
		if got.Transforms[0].From != want.Transforms[0].From || got.Transforms[0].To != want.Transforms[0].To {
			t.Errorf("%s: %q %q", name, got.Transforms[0].From, got.Transforms[0].To)
		}
	}
}

func TestEmptyFilesMeanDefaults(t *testing.T) {
	for _, content := range []string{"", "# notes only\n", "---\n"} {
		c, err := Load(write(t, "gitopsify.yaml", content))
		if err != nil {
			t.Fatalf("%q: %v", content, err)
		}
		c.Default()
		if err := c.Validate(); err != nil {
			t.Errorf("%q: %v", content, err)
		}
	}
}

// TestMetadataValuesConvert: metadata values convert to strings like the
// spec's, instead of failing before the resource is decoded.
func TestMetadataValuesConvert(t *testing.T) {
	c, err := Load(write(t, "gitopsify.yaml", "apiVersion: gitopsify.io/v1alpha1\nkind: Migration\nmetadata: {name: 123, labels: {tier: 1}}\nspec: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Metadata.Name != "123" || c.Metadata.Labels["tier"] != "1" {
		t.Errorf("metadata %+v", c.Metadata)
	}
	_, err = Load(write(t, "gitopsify.yaml", "apiVersion: gitopsify.io/v1alpha1\nkind: Migration\nmetadata: {name: a, labels: {tier: [1]}}\nspec: {}\n"))
	if err == nil || !strings.Contains(err.Error(), "labels") {
		t.Errorf("want an error naming labels, got %v", err)
	}
}

func TestMigrationIsWrittenAsAResource(t *testing.T) {
	c := richConfig()
	b, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !strings.HasPrefix(text, "apiVersion: gitopsify.io/v1alpha1\nkind: Migration\nmetadata:\n") || !strings.Contains(text, "\nspec:\n  source:\n    context: kind-test\n") {
		t.Fatalf("not in the resource form, in order:\n%s", text)
	}
	if strings.Contains(text, "\n  apiVersion:") || strings.Contains(text, "\n  kind:") {
		t.Errorf("apiVersion or kind repeated in the spec:\n%s", text)
	}
}

func TestMigrationRoundTripsInEverySyntax(t *testing.T) {
	want := richConfig()
	for _, name := range []string{"gitopsify.yaml", "gitopsify.yml", "gitopsify.json", "gitopsify.hcl"} {
		t.Run(name, func(t *testing.T) {
			b, err := Encode(name, want.Document(), SchemaHeader, "# a comment\n")
			if err != nil {
				t.Fatal(err)
			}
			got, err := Load(write(t, name, string(b)))
			if err != nil {
				t.Fatalf("%v\n%s", err, b)
			}
			got.Default()
			if err := got.Validate(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				g, _ := json.MarshalIndent(got.Document(), "", " ")
				w, _ := json.MarshalIndent(want.Document(), "", " ")
				t.Errorf("round trip changed the migration:\n%s\nwant:\n%s\nfile:\n%s", g, w, b)
			}
			text := string(b)
			switch {
			case strings.HasSuffix(name, ".json"):
				if !strings.HasPrefix(text, "{") {
					t.Errorf("JSON has no comments, so no header:\n%s", text)
				}
				if !strings.Contains(text, `"shop <&>"`) {
					t.Errorf("JSON escapes HTML characters:\n%s", text)
				}
			case strings.HasSuffix(name, ".hcl"):
				if !strings.HasPrefix(text, "# a comment\napiVersion = ") || strings.Contains(text, "yaml-language-server") {
					t.Errorf("HCL header:\n%s", text)
				}
			default:
				if !strings.HasPrefix(text, SchemaHeader+"# a comment\napiVersion: ") {
					t.Errorf("YAML header:\n%s", text)
				}
			}
		})
	}
}

func TestV01FilesAreRewrittenAsResources(t *testing.T) {
	c, err := Load(write(t, "gitopsify.yaml", "apiVersion: gitopsify.io/v1alpha1\nkind: Migration\nsource:\n  context: arn:aws:eks:eu-west-1:123:cluster/Prod\n"))
	if err != nil {
		t.Fatal(err)
	}
	c.Default()
	if c.Metadata.Name != "arn-aws-eks-eu-west-1-123-cluster-prod" {
		t.Errorf("name = %q", c.Metadata.Name)
	}
	b, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	again, err := Load(write(t, "gitopsify.yaml", string(b)))
	if err != nil || again.Source.Context != c.Source.Context || again.Metadata.Name != c.Metadata.Name {
		t.Fatalf("%+v %v\n%s", again, err, b)
	}
}

func TestMigrationRejects(t *testing.T) {
	for content, want := range map[string]string{
		"apiVersion: gitopsify.io/v1alpha1\nkind: Migration\nmetadata: {name: a}\nspec:\n  apiVersion: gitopsify.io/v1alpha1\n": "go at the top",
		"apiVersion: gitopsify.io/v1alpha1\nkind: Migration\nmetadata: {name: a}\nsource: {context: x}\n":                       "go under spec",
		"apiVersion: gitopsify.io/v1alpha1\nkind: Migration\nspec: {}\nsecrets: {strategy: sops}\n":                             "secrets goes under spec",
		"apiVersion: gitopsify.io/v1alpha1\nkind: Migration\nmetadata: {name: A_b}\nspec: {}\n":                                 "metadata.name",
		"apiVersion: gitopsify.io/v1alpha1\nkind: Migration\nmetadata: {name: a, uid: x}\nspec: {}\n":                           `unknown field "uid"`,
	} {
		c, err := Load(write(t, "gitopsify.yaml", content))
		if err == nil {
			c.Default()
			err = c.Validate()
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", content, want, err)
		}
	}
}

func TestNameFor(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "migration",
		"kind-test":                    "kind-test",
		"gke_proj_europe-west1_a":      "gke-proj-europe-west1-a",
		"--":                           "migration",
		"admin@Prod/ns":                "admin-prod-ns",
		strings.Repeat("a", 62) + "-b": strings.Repeat("a", 62),
	} {
		if got := NameFor(in); got != want {
			t.Errorf("NameFor(%q) = %q, want %q", in, got, want)
		}
		if err := (ObjectMeta{Name: NameFor(in)}).validate(); err != nil {
			t.Error(err)
		}
	}
}

func TestExportNameIsChecked(t *testing.T) {
	x, err := LoadExport(write(t, "e.yaml", strings.Replace(exportYAML, "name: grafana-prod", "name: Grafana Prod", 1)), "")
	if err == nil {
		x.Default()
		err = x.Validate()
	}
	if err == nil || !strings.Contains(err.Error(), "metadata.name") {
		t.Errorf("got %v", err)
	}
}

func exportSpecField(name string) bool {
	t := reflect.TypeOf(ExportSpec{})
	for i := 0; i < t.NumField(); i++ {
		if tag, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); tag == name {
			return true
		}
	}
	return false
}

// TestDocumentedMigrationsLoad loads every migration shown in the docs and
// skills: complete files as they are, fragments (spec: ...) inside a
// Migration.
func TestDocumentedMigrationsLoad(t *testing.T) {
	var files []string
	for _, pattern := range []string{"../../docs/*.md", "../../skills/*/*.md", "../../skills/*/references/*.md", "../../README.md"} {
		m, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	fence := regexp.MustCompile("(?ms)^([ \\t]*)```ya?ml\\n(.*?)^[ \\t]*```")
	n := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range fence.FindAllStringSubmatch(string(data), -1) {
			var lines []string
			for _, l := range strings.Split(m[2], "\n") {
				lines = append(lines, strings.TrimPrefix(l, m[1]))
			}
			block := strings.Join(lines, "\n")
			first, _, _ := strings.Cut(strings.TrimLeft(block, "\n"), ":")
			switch {
			case strings.Contains(block, "kind: Migration"):
			case strings.HasPrefix(block, "spec:\n"):
				block = "apiVersion: gitopsify.io/v1alpha1\nkind: Migration\n" + block
			case specField(first) && !exportSpecField(first):
				t.Errorf("%s: a migration snippet with %s at the top: put it under spec:\n%s", filepath.Base(f), first, block)
				continue
			default:
				continue
			}
			n++
			c, err := Load(write(t, "gitopsify.yaml", block))
			if err == nil {
				c.Default()
				err = c.Validate()
			}
			if err != nil {
				t.Errorf("%s: %v\n%s", filepath.Base(f), err, block)
			}
		}
	}
	if n < 20 {
		t.Errorf("found only %d migrations in the docs", n)
	}
}

func TestExportRoundTripsInEverySyntax(t *testing.T) {
	want, err := LoadExport(write(t, "e.yaml", exportYAML), "")
	if err != nil {
		t.Fatal(err)
	}
	want.Default()
	want.Spec.Instances[0].Settings["ca_cert"] = SettingValue{File: "ca.pem"}
	want.Spec.Instances[0].Settings["note"] = SettingValue{Value: "${not.a.template}\nsecond line\n"}
	for _, name := range []string{"e.yaml", "e.json", "e.hcl"} {
		b, err := Encode(name, want, ExportSchemaHeader, "# export\n")
		if err != nil {
			t.Fatal(err)
		}
		got, err := LoadExport(write(t, name, string(b)), "")
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, b)
		}
		got.Default()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: round trip changed the export:\n%+v\nwant %+v\n%s", name, got, want, b)
		}
	}
}
