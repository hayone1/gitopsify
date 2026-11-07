package iac

import (
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/sdk"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func desc() *sdk.Description {
	xp := func(kind string) *sdk.CrossplaneType {
		return &sdk.CrossplaneType{APIVersion: "oss.grafana.m.crossplane.io/v1alpha1", Kind: kind}
	}
	return &sdk.Description{
		APIVersion: sdk.APIVersion, Name: "grafana", Title: "Grafana", Version: "v0.1.0", Protocols: []string{sdk.Protocol},
		Settings: []sdk.Setting{
			{Name: "url", Description: "URL", Env: "GRAFANA_URL", Required: true, Terraform: "url", Crossplane: "url"},
			{Name: "auth", Title: "credentials", Description: "token", Env: "GRAFANA_AUTH", Secret: true, Required: true, Terraform: "auth", Crossplane: "auth"},
		},
		Partition: sdk.Noun{Singular: "organization", Plural: "organizations"},
		Kinds: []sdk.Kind{
			{Name: "Folder", Description: "folders", Terraform: &sdk.TerraformType{Type: "grafana_folder"}, Crossplane: xp("Folder")},
			{Name: "Dashboard", Description: "dashboards", Terraform: &sdk.TerraformType{Type: "grafana_dashboard"}, Crossplane: xp("Dashboard")},
			{Name: "DataSource", Description: "data sources", Terraform: &sdk.TerraformType{Type: "grafana_data_source"}, Crossplane: xp("DataSource")},
			{Name: "ContactPoint", Description: "contact points", Terraform: &sdk.TerraformType{Type: "grafana_contact_point", Blocks: []string{"slack", "email"}},
				Crossplane: &sdk.CrossplaneType{APIVersion: "alerting.grafana.m.crossplane.io/v1alpha1", Kind: "ContactPoint"}},
		},
		Terraform:  &sdk.TerraformProvider{Name: "grafana", Source: "grafana/grafana", Version: "~> 4.53"},
		Crossplane: &sdk.CrossplaneProvider{Name: "provider-grafana", Package: "xpkg.upbound.io/grafana/provider-grafana:v2.17.0", ProviderConfig: sdk.GVK{APIVersion: "grafana.m.crossplane.io/v1beta1", Kind: "ClusterProviderConfig"}},
	}
}

func inventory() *sdk.Inventory {
	return &sdk.Inventory{
		Partitions: []sdk.Partition{
			{ID: "1", Name: "Main Org.", Terraform: map[string]any{"org_id": float64(1)}, Crossplane: map[string]any{"orgId": "1"}},
			{ID: "2", Name: "Team Ünicode", Terraform: map[string]any{"org_id": float64(2)}, Crossplane: map[string]any{"orgId": "2"}},
		},
		Resources: []sdk.Resource{
			{Key: "folder/1/sre", Kind: "Folder", Partition: "1", Name: "SRE", ImportID: "1:sre", Attributes: map[string]any{"title": "SRE", "uid": "sre"}},
			{Key: "folder/1/sre-k8s", Kind: "Folder", Partition: "1", Name: "Kubernetes", ImportID: "1:sre-k8s", Attributes: map[string]any{
				"title": "Kubernetes", "uid": "sre-k8s", "parent_folder_uid": sdk.Ref("folder/1/sre", "uid", "sre", "folderRef")}},
			{Key: "dashboard/1/k8s", Kind: "Dashboard", Partition: "1", Name: "Kubernetes / Overview", ImportID: "1:k8s", Attributes: map[string]any{
				"folder":      sdk.Ref("folder/1/sre-k8s", "uid", "sre-k8s", "folderRef"),
				"config_json": sdk.File("dashboards/kubernetes/overview.json", sdk.RawJSON(`{"uid":"k8s","title":"Kubernetes / Overview","templating":{"list":[{"query":"${DS}"}]}}`)),
			}},
			{Key: "dashboard/1/home", Kind: "Dashboard", Partition: "1", Name: "Home", ImportID: "1:home", Attributes: map[string]any{
				"config_json": sdk.File("dashboards/general/home.json", sdk.RawJSON(`{"uid":"home","title":"Home"}`)),
			}},
			{Key: "datasource/1/prom", Kind: "DataSource", Partition: "1", Name: "Prometheus", ImportID: "1:prom", Attributes: map[string]any{
				"name": "Prometheus", "type": "prometheus", "uid": "prom", "url": "http://prometheus:9090", "is_default": true,
				"json_data_encoded":        sdk.JSON(sdk.RawJSON(`{"httpMethod":"POST","timeInterval":"${interval}"}`)),
				"secure_json_data_encoded": sdk.Secret("secure_json_data", nil, "secureJsonDataEncodedSecretRef", "basicAuthPassword"),
			}},
			{Key: "contactpoint/1/oncall", Kind: "ContactPoint", Partition: "1", Name: "On call", ImportID: "1:On call", Attributes: map[string]any{
				"name": "On call",
				"slack": []any{map[string]any{
					"url":       sdk.Secret("url", "https://hooks.slack.com/x", "urlSecretRef", "Slack webhook URL"),
					"recipient": "#alerts",
					"text":      "{{ template \"x\" . }}\nline two\n",
				}},
				"email": []any{map[string]any{"addresses": []any{"a@example.com"}, "single_email": false}},
			}},
			{Key: "folder/2/apps", Kind: "Folder", Partition: "2", Name: "Апп", ImportID: "2:apps", Attributes: map[string]any{"title": "Апп", "uid": "apps"}},
		},
		Skipped:  []sdk.Skipped{{Kind: "Dashboard", Partition: "1", Name: "Node exporter", Reason: "provisioned from files"}},
		Warnings: nil,
	}
}

func input(inv *sdk.Inventory, prev *State, opts Options) Input {
	if opts.Formats == nil {
		opts.Formats = []string{config.FormatTerraform, config.FormatCrossplane}
	}
	return Input{
		Description: desc(),
		Instances: []*Instance{{Name: "default", Settings: map[string]string{"url": "http://grafana:3000", "auth": "admin:admin"},
			Target: "Grafana 12.1.0 at http://grafana:3000", Kinds: []string{"Folder", "Dashboard", "DataSource", "ContactPoint"}, Inventory: inv}},
		Options:  opts,
		Previous: prev,
	}
}

func export(t *testing.T, dir string, inv *sdk.Inventory, opts Options) (*Result, []string, []string) {
	t.Helper()
	prev, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Build(input(inv, prev, opts))
	if err != nil {
		t.Fatal(err)
	}
	written, removed, err := res.Write(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	return res, written, removed
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGolden(t *testing.T) {
	for name, opts := range map[string]Options{
		"adopt":     {Adopt: true},
		"replicate": {Adopt: false, SecretValues: config.SecretValuesLocal, Backend: &config.TerraformBackend{Type: "s3", Settings: map[string]string{"bucket": "state", "key": "grafana/{workspace}.tfstate"}}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			export(t, dir, inventory(), opts)
			golden := filepath.Join("testdata", "golden", name)
			if *update {
				_ = os.RemoveAll(golden)
				copyTree(t, dir, golden)
				return
			}
			compareTrees(t, golden, dir)
			checkTools(t, dir, opts.Adopt)
		})
	}
}

// checkTools runs terraform fmt and kustomize on the output when they're
// installed.
func checkTools(t *testing.T, dir string, adopt bool) {
	t.Helper()
	if tf, err := exec.LookPath("terraform"); err == nil {
		out, err := exec.Command(tf, "fmt", "-check", "-recursive", filepath.Join(dir, "terraform")).CombinedOutput()
		if err != nil {
			t.Errorf("terraform fmt -check: %v\n%s", err, out)
		}
	}
	build := func(d string) *exec.Cmd { return nil }
	if k, err := exec.LookPath("kustomize"); err == nil {
		build = func(d string) *exec.Cmd { return exec.Command(k, "build", d) }
	} else if k, err := exec.LookPath("kubectl"); err == nil {
		build = func(d string) *exec.Cmd { return exec.Command(k, "kustomize", d) }
	}
	if build("") != nil {
		for _, d := range []string{"crossplane", "crossplane/setup"} {
			out, err := build(filepath.Join(dir, d)).CombinedOutput()
			if err != nil {
				t.Errorf("kustomize build %s: %v\n%s", d, err, out)
			}
			if adopt && d == "crossplane" && strings.Contains(string(out), "- Create") {
				t.Errorf("observe-only component didn't apply:\n%s", out)
			}
		}
	}
}

func TestHCLEscapesTemplates(t *testing.T) {
	dir := t.TempDir()
	export(t, dir, inventory(), Options{Adopt: true})
	ds := read(t, filepath.Join(dir, "terraform/default/main-org/data_sources.tf"))
	if !strings.Contains(ds, `"$${interval}"`) {
		t.Errorf("template sequence not escaped:\n%s", ds)
	}
	cp := read(t, filepath.Join(dir, "terraform/default/main-org/contact_points.tf"))
	if !strings.Contains(cp, "<<EOT\n{{ template \"x\" . }}\nline two\nEOT") {
		t.Errorf("heredoc missing:\n%s", cp)
	}
	dash := read(t, filepath.Join(dir, "terraform/default/main-org/dashboards/kubernetes/overview.json"))
	if !strings.Contains(dash, `"query": "${DS}"`) {
		t.Errorf("dashboard JSON changed:\n%s", dash)
	}
}

func TestReexportKeepsNamesAndNeverDestroys(t *testing.T) {
	dir := t.TempDir()
	export(t, dir, inventory(), Options{Adopt: true})
	before := read(t, filepath.Join(dir, "terraform/default/main-org/dashboards.tf"))
	if !strings.Contains(before, `resource "grafana_dashboard" "kubernetes_overview"`) {
		t.Fatalf("unexpected name:\n%s", before)
	}

	// Rename a dashboard and delete another one in the tool.
	inv := inventory()
	var kept []sdk.Resource
	for _, r := range inv.Resources {
		if r.Key == "dashboard/1/home" {
			continue
		}
		if r.Key == "dashboard/1/k8s" {
			r.Name = "Cluster overview"
		}
		kept = append(kept, r)
	}
	inv.Resources = kept
	res, _, removed := export(t, dir, inv, Options{Adopt: true})
	after := read(t, filepath.Join(dir, "terraform/default/main-org/dashboards.tf"))
	if !strings.Contains(after, `resource "grafana_dashboard" "kubernetes_overview"`) {
		t.Errorf("rename changed the address:\n%s", after)
	}
	if !strings.Contains(after, "removed {\n  from = grafana_dashboard.home\n  lifecycle {\n    destroy = false\n  }\n}") {
		t.Errorf("no removed block for the deleted dashboard:\n%s", after)
	}
	if !contains(removed, "terraform/default/main-org/dashboards/general/home.json") || !contains(removed, "crossplane/default/main-org/dashboards/home.yaml") {
		t.Errorf("stale files not removed: %v", removed)
	}
	if res.Report.GoneTotal() != 1 {
		t.Errorf("gone = %d", res.Report.GoneTotal())
	}
	if _, err := os.Stat(filepath.Join(dir, "terraform/default/main-org/dashboards/kubernetes/overview.json")); err != nil {
		t.Errorf("renamed dashboard's file moved: %v", err)
	}

	// The removed block survives later exports.
	export(t, dir, inv, Options{Adopt: true})
	if !strings.Contains(read(t, filepath.Join(dir, "terraform/default/main-org/dashboards.tf")), "from = grafana_dashboard.home") {
		t.Error("removed block dropped by the next export")
	}
	// A new object can't take the old address.
	inv.Resources = append(inv.Resources, sdk.Resource{Key: "dashboard/1/home2", Kind: "Dashboard", Partition: "1", Name: "Home", ImportID: "1:home2",
		Attributes: map[string]any{"config_json": sdk.File("dashboards/general/home.json", "{}")}})
	export(t, dir, inv, Options{Adopt: true})
	got := read(t, filepath.Join(dir, "terraform/default/main-org/dashboards.tf"))
	if strings.Contains(got, `resource "grafana_dashboard" "home"`) || !strings.Contains(got, `resource "grafana_dashboard" "home_`) {
		t.Errorf("new dashboard reused a removed address:\n%s", got)
	}
}

func TestPartialReadKeepsPreviousFiles(t *testing.T) {
	dir := t.TempDir()
	export(t, dir, inventory(), Options{Adopt: true})
	cp := filepath.Join(dir, "terraform/default/main-org/contact_points.tf")
	before := read(t, cp)

	inv := inventory()
	var kept []sdk.Resource
	for _, r := range inv.Resources {
		if r.Kind != "ContactPoint" {
			kept = append(kept, r)
		}
	}
	inv.Resources = kept
	inv.Warnings = []sdk.Warning{{Partition: "1", Kind: "ContactPoint", Message: "HTTP 500"}}
	res, _, removed := export(t, dir, inv, Options{Adopt: true})
	if read(t, cp) != before {
		t.Error("contact points of an incomplete scope were rewritten")
	}
	for _, p := range removed {
		if strings.Contains(p, "contact") {
			t.Errorf("removed %s from an incomplete scope", p)
		}
	}
	if res.Report.GoneTotal() != 0 {
		t.Errorf("gone = %d, want 0", res.Report.GoneTotal())
	}
	if !strings.Contains(read(t, filepath.Join(dir, "crossplane/default/main-org/kustomization.yaml")), "contact-points/on-call.yaml") {
		t.Error("kept Crossplane files dropped from the kustomization")
	}
}

func TestNarrowScopeLeavesOtherWorkspaces(t *testing.T) {
	dir := t.TempDir()
	export(t, dir, inventory(), Options{Adopt: true})
	other := filepath.Join(dir, "terraform/default/team-unicode/folders.tf")
	before := read(t, other)

	// Only partition 1 and only Terraform this time.
	inv := inventory()
	inv.Partitions = inv.Partitions[:1]
	var kept []sdk.Resource
	for _, r := range inv.Resources {
		if r.Partition == "1" {
			kept = append(kept, r)
		}
	}
	inv.Resources = kept
	_, _, removed := export(t, dir, inv, Options{Adopt: true, Formats: []string{config.FormatTerraform}})
	if read(t, other) != before {
		t.Error("an out-of-scope workspace changed")
	}
	if len(removed) != 0 {
		t.Errorf("removed %v", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "crossplane/default/main-org/folders/sre.yaml")); err != nil {
		t.Error("the other format's files were removed")
	}
	if !strings.Contains(read(t, filepath.Join(dir, "crossplane/kustomization.yaml")), "team-unicode") {
		t.Error("crossplane root lost a workspace")
	}
}

func TestSecretsStayOutOfTrackedFiles(t *testing.T) {
	dir := t.TempDir()
	export(t, dir, inventory(), Options{Adopt: true, SecretValues: config.SecretValuesLocal})
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b, _ := os.ReadFile(p)
		ignored := strings.HasPrefix(rel, ".secrets") || strings.HasSuffix(rel, SecretTFVarsSuffix)
		if strings.Contains(string(b), "hooks.slack.com") || strings.Contains(string(b), "admin:admin") {
			if !ignored {
				t.Errorf("secret value in tracked file %s", rel)
			}
			if fi, _ := d.Info(); fi.Mode().Perm() != 0o600 {
				t.Errorf("%s has mode %v", rel, fi.Mode().Perm())
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "terraform/default/main-org/contact_points"+SecretTFVarsSuffix)); err != nil {
		t.Error("no tfvars for the Slack URL")
	}
}

func TestWriteRefusesForeignDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Build(input(inventory(), nil, Options{Adopt: true}))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := res.Write(dir, false); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestSelect(t *testing.T) {
	inv := inventory()
	sel := Select(inv, Selection{Include: []string{"Dashboard/main-org/kubernetes-overview"}})
	var keys []string
	for _, r := range sel.Resources {
		keys = append(keys, r.Key)
	}
	want := []string{"folder/1/sre", "folder/1/sre-k8s", "dashboard/1/k8s"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("selected %v, want %v", keys, want)
	}
	sel = Select(inv, Selection{Include: []string{"Dashboard/*/*"}, NoDeps: true, Exclude: []string{"*/*/home"}})
	if len(sel.Resources) != 1 || sel.Resources[0].Key != "dashboard/1/k8s" {
		t.Errorf("got %+v", sel.Resources)
	}
	ids, err := MatchPartitions(inv.Partitions, []string{"team-*"})
	if err != nil || len(ids) != 1 || ids[0] != "2" {
		t.Errorf("ids %v err %v", ids, err)
	}
	if _, err := MatchPartitions(inv.Partitions, []string{"nope"}); err == nil {
		t.Error("want an error for an unknown partition")
	}
}

func TestNames(t *testing.T) {
	for in, want := range map[string]string{"Kubernetes / Overview": "kubernetes_overview", "2024 Q1": "_2024_q1", "Апп": "r_", "Ça va": "ca_va"} {
		got := tfBase(in, "k")
		if !strings.HasPrefix(got, want) {
			t.Errorf("tfBase(%q) = %q, want prefix %q", in, got, want)
		}
	}
	if got := safePath("../../etc/passwd", "k"); got != "etc/passwd" {
		t.Errorf("safePath = %q", got)
	}
	if got := safePath("dashboards/CON.json", "k"); got != "dashboards/con-1.json" {
		t.Errorf("safePath = %q", got)
	}
	if got := snake("DataSources"); got != "data_sources" {
		t.Errorf("snake = %q", got)
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		b, _ := os.ReadFile(p)
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func listTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b, _ := os.ReadFile(p)
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	return out
}

func compareTrees(t *testing.T, want, got string) {
	t.Helper()
	w, g := listTree(t, want), listTree(t, got)
	var names []string
	for k := range w {
		names = append(names, k)
	}
	for k := range g {
		if _, ok := w[k]; !ok {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if w[n] != g[n] {
			t.Errorf("%s differs (run go test ./pkg/iac -run TestGolden -update):\n--- want\n%s\n--- got\n%s", n, w[n], g[n])
		}
	}
}

// Keep encoding/json imported for fixtures that need numbers.
var _ = json.Number("1")
