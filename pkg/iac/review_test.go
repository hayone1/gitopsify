package iac

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/sdk"
)

// Regression tests for re-export safety and output hardening.

func exportWith(t *testing.T, dir string, inv *sdk.Inventory, opts Options, mutate func(*Instance)) *Result {
	t.Helper()
	prev, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	in := input(inv, prev, opts)
	if mutate != nil {
		mutate(in.Instances[0])
	}
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := res.Write(dir, false); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestKindOutOfScopeIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	export(t, dir, inventory(), Options{Adopt: true})
	folders := filepath.Join(dir, "terraform/default/main-org/folders.tf")
	before := read(t, folders)
	// Only dashboards this time, but the provider also sends one folder.
	exportWith(t, dir, inventory(), Options{Adopt: true}, func(in *Instance) { in.Kinds = []string{"Dashboard"} })
	if read(t, folders) != before {
		t.Error("folders.tf was rewritten by a dashboards-only export")
	}
}

func TestChangedKindGetsRemovedBlock(t *testing.T) {
	dir := t.TempDir()
	export(t, dir, inventory(), Options{Adopt: true})
	inv := inventory()
	for i := range inv.Resources {
		if inv.Resources[i].Key == "datasource/1/prom" {
			inv.Resources[i].Kind = "Folder" // stands in for Dashboard → DashboardV2
			inv.Resources[i].Attributes = map[string]any{"title": "Prometheus"}
		}
	}
	exportWith(t, dir, inv, Options{Adopt: true}, nil)
	ds := read(t, filepath.Join(dir, "terraform/default/main-org/data_sources.tf"))
	if !strings.Contains(ds, "from = grafana_data_source.prometheus") {
		t.Errorf("no removed block for the old address:\n%s", ds)
	}
}

func TestNarrowScopeKeepsCrossplaneWorkspaces(t *testing.T) {
	dir := t.TempDir()
	export(t, dir, inventory(), Options{Adopt: true})
	inv := inventory()
	inv.Partitions = inv.Partitions[:1]
	var kept []sdk.Resource
	for _, r := range inv.Resources {
		if r.Partition == "1" {
			kept = append(kept, r)
		}
	}
	inv.Resources = kept
	exportWith(t, dir, inv, Options{Adopt: true, Formats: []string{config.FormatCrossplane}}, nil)
	for _, p := range []string{"crossplane/default/team-unicode/namespace.yaml", "crossplane/default/team-unicode/providerconfig.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s was removed", p)
		}
	}
	k := read(t, filepath.Join(dir, "crossplane/default/team-unicode/kustomization.yaml"))
	if strings.Contains(k, "kustomization.yaml") {
		t.Errorf("kustomization lists itself:\n%s", k)
	}
}

func TestSelectionDoesNotDropExportedObjects(t *testing.T) {
	dir := t.TempDir()
	export(t, dir, inventory(), Options{Adopt: true})
	only := map[string]bool{"dashboard/1/home": true}
	res := exportWith(t, dir, inventory(), Options{Adopt: true}, func(in *Instance) { in.Selected = only })
	if res.Report.GoneTotal() != 0 {
		t.Errorf("a narrower selection made %d objects gone", res.Report.GoneTotal())
	}
	if !strings.Contains(read(t, filepath.Join(dir, "terraform/default/main-org/dashboards.tf")), "kubernetes_overview") {
		t.Error("an unselected, exported dashboard was dropped")
	}
	// With Prune they get removed blocks.
	res = exportWith(t, dir, inventory(), Options{Adopt: true, Prune: true}, func(in *Instance) { in.Selected = only })
	if res.Report.GoneTotal() == 0 || !strings.Contains(read(t, filepath.Join(dir, "terraform/default/main-org/dashboards.tf")), "from = grafana_dashboard.kubernetes_overview") {
		t.Error("prune didn't remove the unselected dashboard")
	}
	// A new export that never had them doesn't export unselected objects.
	fresh := t.TempDir()
	exportWith(t, fresh, inventory(), Options{Adopt: true}, func(in *Instance) { in.Selected = only })
	if strings.Contains(read(t, filepath.Join(fresh, "terraform/default/main-org/dashboards.tf")), "kubernetes_overview") {
		t.Error("an unselected dashboard was exported")
	}
}

func TestCrossplaneSecretNamesDontCollide(t *testing.T) {
	inv := &sdk.Inventory{Partitions: []sdk.Partition{{ID: "1", Name: "Main"}}, Resources: []sdk.Resource{
		{Key: "a", Kind: "DataSource", Partition: "1", Name: "foo", ImportID: "a", Attributes: map[string]any{
			"name": "foo", "http_headers": sdk.Secret("bar", map[string]string{"X": "1"}, "httpHeadersSecretRef", "")}},
		{Key: "b", Kind: "DataSource", Partition: "1", Name: "foo bar", ImportID: "b", Attributes: map[string]any{
			"name": "foo bar", "secure_json_data_encoded": sdk.Secret("secure", "pw", "secureJsonDataEncodedSecretRef", "")}},
	}}
	dir := t.TempDir()
	exportWith(t, dir, inv, Options{Adopt: true, SecretValues: config.SecretValuesLocal, Formats: []string{config.FormatCrossplane}}, nil)
	a := read(t, filepath.Join(dir, "crossplane/default/main/data-sources/foo.yaml"))
	b := read(t, filepath.Join(dir, "crossplane/default/main/data-sources/foo-bar.yaml"))
	nameOf := func(y, field string) string {
		i := strings.Index(y, field+":")
		rest := y[i:]
		j := strings.Index(rest, "name: ")
		return strings.Fields(rest[j+6:])[0]
	}
	if nameOf(a, "httpHeadersSecretRef") == nameOf(b, "secureJsonDataEncodedSecretRef") {
		t.Errorf("both reference Secret %s", nameOf(a, "httpHeadersSecretRef"))
	}
}

func TestGitignoreIsMerged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/\n.secrets/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Build(input(inventory(), nil, Options{Adopt: true}))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := res.Write(dir, true); err != nil {
		t.Fatal(err)
	}
	g := read(t, filepath.Join(dir, ".gitignore"))
	if !strings.HasPrefix(g, "node_modules/\n") || strings.Count(g, ".secrets/") != 1 || !strings.Contains(g, ".terraform/") {
		t.Errorf("merged .gitignore:\n%s", g)
	}
}

func TestSecretsRefuseSymlinks(t *testing.T) {
	dir := t.TempDir()
	tracked := filepath.Join(dir, "tracked")
	if err := os.MkdirAll(tracked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tracked", filepath.Join(dir, ".secrets")); err != nil {
		t.Fatal(err)
	}
	res, err := Build(input(inventory(), nil, Options{Adopt: true, SecretValues: config.SecretValuesLocal}))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := res.Write(dir, true); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v", err)
	}
	entries, _ := os.ReadDir(tracked)
	if len(entries) != 0 {
		t.Errorf("secrets written through the link: %v", entries)
	}
}

func TestHandEditsAreDetected(t *testing.T) {
	dir := t.TempDir()
	export(t, dir, inventory(), Options{Adopt: true})
	f := filepath.Join(dir, "terraform/default/main-org/folders.tf")
	if err := os.WriteFile(f, []byte(read(t, f)+"# my note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inv := inventory()
	inv.Resources[0].Attributes["title"] = "SRE team"
	prev, _ := LoadState(dir)
	res, err := Build(input(inv, prev, Options{Adopt: true}))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.HandEdited(dir); len(got) != 1 || got[0] != "terraform/default/main-org/folders.tf" {
		t.Errorf("hand edited: %v", got)
	}
}

func TestStateIsValidated(t *testing.T) {
	dir := t.TempDir()
	export(t, dir, inventory(), Options{Adopt: true})
	sf := filepath.Join(dir, StateFile)
	b := read(t, sf)
	bad := strings.Replace(b, `"terraform": "grafana_folder.sre"`, `"terraform": "grafana_folder.sre\n}\ndata \"x\" \"y\" {"`, 1)
	if bad == b {
		t.Fatal("fixture didn't change")
	}
	if err := os.WriteFile(sf, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(dir); err == nil || !strings.Contains(err.Error(), "invalid Terraform address") {
		t.Fatalf("err = %v", err)
	}
}

func TestIgnoreChangesInBothOutputs(t *testing.T) {
	inv := &sdk.Inventory{Partitions: []sdk.Partition{{ID: "1", Name: "Main"}}, Resources: []sdk.Resource{
		{Key: "a", Kind: "ContactPoint", Partition: "1", Name: "ops", ImportID: "1:ops", Attributes: map[string]any{
			"name": "ops", "disable_provenance": true}}}}
	in := input(inv, nil, Options{Adopt: true})
	in.Description.Kinds[3].Terraform.IgnoreChanges = []string{"disable_provenance"}
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, _, err := res.Write(dir, false); err != nil {
		t.Fatal(err)
	}
	tf := read(t, filepath.Join(dir, "terraform/default/main/contact_points.tf"))
	if !strings.Contains(tf, "lifecycle {\n    ignore_changes = [disable_provenance]\n  }") {
		t.Errorf("no ignore_changes:\n%s", tf)
	}
	xp := read(t, filepath.Join(dir, "crossplane/default/main/contact-points/ops.yaml"))
	if !strings.Contains(xp, "initProvider:\n    disableProvenance: true") || strings.Contains(xp, "forProvider:\n    disableProvenance") {
		t.Errorf("disableProvenance not in initProvider:\n%s", xp)
	}
}

func TestHandEditWithUnchangedToolIsDetected(t *testing.T) {
	dir := t.TempDir()
	export(t, dir, inventory(), Options{Adopt: true})
	f := filepath.Join(dir, "terraform/default/main-org/folders.tf")
	if err := os.WriteFile(f, []byte(read(t, f)+"# my note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev, _ := LoadState(dir)
	res, err := Build(input(inventory(), prev, Options{Adopt: true}))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.HandEdited(dir); len(got) != 1 || got[0] != "terraform/default/main-org/folders.tf" {
		t.Errorf("hand edited: %v", got)
	}
}

func TestSameTypeKindChangeKeepsAddress(t *testing.T) {
	dir := t.TempDir()
	withV2 := func(in *Instance) {}
	_ = withV2
	export(t, dir, inventory(), Options{Adopt: true})
	inv := inventory()
	for i := range inv.Resources {
		if inv.Resources[i].Key == "folder/1/sre" {
			inv.Resources[i].Kind = "FolderV2"
		}
	}
	prev, _ := LoadState(dir)
	in := input(inv, prev, Options{Adopt: true})
	in.Description.Kinds = append(in.Description.Kinds, sdk.Kind{Name: "FolderV2", Description: "folders",
		Terraform: &sdk.TerraformType{Type: "grafana_folder"}})
	in.Instances[0].Kinds = append(in.Instances[0].Kinds, "FolderV2")
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := res.Write(dir, false); err != nil {
		t.Fatal(err)
	}
	tf := read(t, filepath.Join(dir, "terraform/default/main-org/folder_v2s.tf"))
	if !strings.Contains(tf, `resource "grafana_folder" "sre"`) {
		t.Errorf("the address changed:\n%s", tf)
	}
	if strings.Contains(read(t, filepath.Join(dir, "terraform/default/main-org/folders.tf")), "from = grafana_folder.sre") {
		t.Error("a removed block for an address still in use")
	}
}

func TestWarningForUnannouncedPartitionKeepsFiles(t *testing.T) {
	dir := t.TempDir()
	export(t, dir, inventory(), Options{Adopt: true})
	before := read(t, filepath.Join(dir, "terraform/default/team-unicode/folders.tf"))
	inv := inventory()
	inv.Partitions = inv.Partitions[:1]
	var kept []sdk.Resource
	for _, r := range inv.Resources {
		if r.Partition == "1" {
			kept = append(kept, r)
		}
	}
	inv.Resources = kept
	inv.Warnings = []sdk.Warning{{Partition: "2", Kind: "Dashboard", Message: "forbidden"}}
	res := exportWith(t, dir, inv, Options{Adopt: true}, nil)
	if read(t, filepath.Join(dir, "terraform/default/team-unicode/folders.tf")) != before || res.Report.GoneTotal() != 0 {
		t.Errorf("an unannounced partition's other kinds were treated as deleted (gone %d)", res.Report.GoneTotal())
	}
}

func TestCrossplaneSecretNamesAreStable(t *testing.T) {
	mk := func(keys ...string) *sdk.Inventory {
		inv := &sdk.Inventory{Partitions: []sdk.Partition{{ID: "1", Name: "Main"}}}
		for _, k := range keys {
			inv.Resources = append(inv.Resources, sdk.Resource{Key: k, Kind: "DataSource", Partition: "1", Name: "same", ImportID: k,
				Attributes: map[string]any{"name": k, "secure_json_data_encoded": sdk.Secret("secure", "pw-"+k, "secureJsonDataEncodedSecretRef", "")}})
		}
		return inv
	}
	opts := Options{Adopt: true, SecretValues: config.SecretValuesLocal, Formats: []string{config.FormatCrossplane}}
	dir := t.TempDir()
	exportWith(t, dir, mk("a", "b"), opts, nil)
	st, _ := LoadState(dir)
	want := st.Resources["default/b"].Secrets[""]
	exportWith(t, dir, mk("b"), opts, nil)
	st, _ = LoadState(dir)
	if got := st.Resources["default/b"].Secrets[""]; got != want || got == "" {
		t.Errorf("b's Secret moved from %q to %q", want, got)
	}
}
