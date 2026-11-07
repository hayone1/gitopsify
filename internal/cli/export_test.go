package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/provider"
	"github.com/hayone1/gitopsify/sdk"
)

// The test binary acts as gitopsify-provider-fake when it runs under that
// name (a link on PATH).
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == provider.Prefix+"fake" {
		os.Exit(sdk.Run(context.Background(), fakeProvider{mode: os.Getenv("FAKE_MODE")}, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

type fakeProvider struct{ mode string }

func (fakeProvider) Describe() sdk.Description {
	return sdk.Description{
		Name: "fake", Title: "Fake", Version: "v1.0.0",
		Settings: []sdk.Setting{
			{Name: "url", Description: "URL", Env: "FAKE_URL", Required: true, Terraform: "url", Crossplane: "url"},
			{Name: "token", Description: "token", Env: "FAKE_TOKEN", Secret: true, Required: true, Crossplane: "token"},
		},
		Partition: sdk.Noun{Singular: "org", Plural: "orgs"},
		Kinds: []sdk.Kind{
			{Name: "Folder", Description: "folders", Terraform: &sdk.TerraformType{Type: "fake_folder"},
				Crossplane: &sdk.CrossplaneType{APIVersion: "fake.m.crossplane.io/v1alpha1", Kind: "Folder"}},
			{Name: "Dashboard", Description: "dashboards", Terraform: &sdk.TerraformType{Type: "fake_dashboard"},
				Crossplane: &sdk.CrossplaneType{APIVersion: "fake.m.crossplane.io/v1alpha1", Kind: "Dashboard"}},
			{Name: "Alert", Description: "alerts", Optional: true, Terraform: &sdk.TerraformType{Type: "fake_alert"}},
		},
		Terraform:  &sdk.TerraformProvider{Name: "fake", Source: "acme/fake", Version: "~> 1.0"},
		Crossplane: &sdk.CrossplaneProvider{Name: "provider-fake", Package: "xpkg.example.com/provider-fake:v1.0.0", ProviderConfig: sdk.GVK{APIVersion: "fake.m.crossplane.io/v1beta1", Kind: "ClusterProviderConfig"}},
	}
}

var fakeParts = []sdk.Partition{{ID: "1", Name: "One"}, {ID: "2", Name: "Two"}}

func (f fakeProvider) Check(_ context.Context, req sdk.Request) (*sdk.CheckResult, error) {
	if req.Settings["token"] != "t0ken" {
		return nil, sdk.Errorf(sdk.CodeUnauthenticated, "token refused").WithHint("check FAKE_TOKEN")
	}
	if strings.Contains(req.Settings["url"], "solo") {
		return &sdk.CheckResult{Target: "Fake at " + req.Settings["url"], Partitions: fakeParts[:1]}, nil
	}
	return &sdk.CheckResult{Target: "Fake at " + req.Settings["url"], Partitions: fakeParts}, nil
}

func (f fakeProvider) Discover(_ context.Context, req sdk.Request, out sdk.Emitter) error {
	for _, p := range fakeParts {
		if !req.Scope.HasPartition(p.ID) {
			continue
		}
		if err := out.Partition(p); err != nil {
			return err
		}
		emit := func(r sdk.Resource) error {
			if !req.Scope.HasKind(r.Kind) || !req.Scope.HasKey(r.Key) {
				return nil
			}
			return out.Resource(r)
		}
		if err := emit(sdk.Resource{Key: "folder/" + p.ID + "/ops", Kind: "Folder", Partition: p.ID, Name: "Ops", ImportID: p.ID + ":ops",
			Attributes: map[string]any{"title": "Ops"}}); err != nil {
			return err
		}
		if err := emit(sdk.Resource{Key: "dashboard/" + p.ID + "/home", Kind: "Dashboard", Partition: p.ID, Name: "Home", ImportID: p.ID + ":home",
			Attributes: map[string]any{"folder": sdk.Ref("folder/"+p.ID+"/ops", "uid", "ops", "folderRef"),
				"config_json": sdk.File("dashboards/home.json", sdk.RawJSON(`{"title":"Home"}`))}}); err != nil {
			return err
		}
		if f.mode == "partial" && p.ID == "2" {
			if err := out.Warn(sdk.Warning{Partition: p.ID, Kind: "Dashboard", Message: "HTTP 500"}); err != nil {
				return err
			}
		}
	}
	return nil
}

// fakeEnv puts gitopsify-provider-fake on PATH and works in a fresh
// directory.
func fakeEnv(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(bin, provider.Prefix+"fake")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(provider.EnvDir, filepath.Join(t.TempDir(), "providers"))
	t.Setenv("FAKE_URL", "http://fake")
	t.Setenv("FAKE_TOKEN", "t0ken")
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

func TestExportWritesAndReexports(t *testing.T) {
	dir := fakeEnv(t)
	code, out, errOut := run(t, "export", "fake", "-o", "out")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, p := range []string{"terraform/default/one/dashboards.tf", "terraform/default/two/folders.tf", "crossplane/default/one/folders/ops.yaml", "EXPORT_REPORT.md"} {
		if _, err := os.Stat(filepath.Join(dir, "out", p)); err != nil {
			t.Errorf("%s missing", p)
		}
	}
	if !strings.Contains(errOut, "wrote") || !strings.Contains(out, "terraform init") {
		t.Errorf("output:\n%s\n%s", out, errOut)
	}
	// Exporting again changes nothing and removes nothing.
	before, _ := os.ReadFile(filepath.Join(dir, "out/terraform/default/one/dashboards.tf"))
	code, _, errOut = run(t, "export", "fake", "-o", "out")
	after, _ := os.ReadFile(filepath.Join(dir, "out/terraform/default/one/dashboards.tf"))
	if code != 0 || string(before) != string(after) || strings.Contains(errOut, "removed") {
		t.Errorf("re-export: exit %d, changed %v\n%s", code, string(before) != string(after), errOut)
	}
}

func TestExportDryRunAndScope(t *testing.T) {
	dir := fakeEnv(t)
	code, out, errOut := run(t, "export", "fake", "-o", "out", "--dry-run", "--partition", "two", "--kind", "folder", "--to", "terraform")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "out")); err == nil {
		t.Error("dry run wrote files")
	}
	if !strings.Contains(out, "default / Two") || strings.Contains(out, "default / One") || strings.Contains(out, "Dashboard") {
		t.Errorf("summary:\n%s", out)
	}
	code, _, errOut = run(t, "export", "fake", "--kind", "Widgets")
	if code == 0 || !strings.Contains(errOut, "Folder, Dashboard, Alert") {
		t.Errorf("unknown kind: exit %d\n%s", code, errOut)
	}
	code, _, errOut = run(t, "export", "fake", "--partition", "three")
	if code == 0 || !strings.Contains(errOut, "one, two") {
		t.Errorf("unknown partition: exit %d\n%s", code, errOut)
	}
}

func TestExportPartialRead(t *testing.T) {
	fakeEnv(t)
	t.Setenv("FAKE_MODE", "partial")
	code, _, errOut := run(t, "export", "fake", "-o", "out")
	if code != 3 || !strings.Contains(errOut, "--allow-partial") || !strings.Contains(errOut, "HTTP 500") {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	code, _, errOut = run(t, "export", "fake", "-o", "out", "--allow-partial")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
}

func TestExportSaveAndReplay(t *testing.T) {
	dir := fakeEnv(t)
	code, _, errOut := run(t, "export", "fake", "-o", "out", "--set", "url=http://from-flag", "--to", "terraform", "--save")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	x, err := config.LoadExport(filepath.Join(dir, "gitopsify-fake.yaml"), "")
	if err != nil {
		t.Fatal(err)
	}
	in := x.Spec.Instances[0]
	if x.Kind != config.KindExport || x.Metadata.Name != "fake" || in.Settings["url"].Value != "http://from-flag" || in.Settings["token"].Env != "FAKE_TOKEN" {
		b, _ := json.Marshal(x)
		t.Errorf("saved %s", b)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "gitopsify-fake.yaml"))
	if strings.Contains(string(b), "t0ken") {
		t.Error("the token was saved")
	}
	// No provider argument: the only export file here is used.
	code, _, errOut = run(t, "export")
	if code != 0 || !strings.Contains(errOut, "using gitopsify-fake.yaml") {
		t.Fatalf("replay: exit %d\n%s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "out/crossplane")); err == nil {
		t.Error("replay ignored the saved formats")
	}
}

func TestExportSettingsErrors(t *testing.T) {
	dir := fakeEnv(t)
	t.Setenv("FAKE_TOKEN", "")
	code, _, errOut := run(t, "export", "fake")
	if code == 0 || !strings.Contains(errOut, "$FAKE_TOKEN") {
		t.Errorf("missing secret: exit %d\n%s", code, errOut)
	}
	t.Setenv("FAKE_TOKEN", "wrong")
	code, _, errOut = run(t, "export", "fake")
	if code == 0 || !strings.Contains(errOut, "token refused") || !strings.Contains(errOut, "check FAKE_TOKEN") {
		t.Errorf("refused: exit %d\n%s", code, errOut)
	}
	cfg := "apiVersion: gitopsify.io/v1alpha1\nkind: Export\nspec:\n  provider: {name: fake}\n  instances:\n  - name: a\n    settings: {token: plain}\n"
	if err := os.WriteFile(filepath.Join(dir, "gitopsify-fake.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = run(t, "export", "fake")
	if code == 0 || !strings.Contains(errOut, "is a secret") {
		t.Errorf("secret in file: exit %d\n%s", code, errOut)
	}
}

func TestProviderCommands(t *testing.T) {
	fakeEnv(t)
	code, out, _ := run(t, "provider", "list")
	if code != 0 || !strings.Contains(out, "fake") || !strings.Contains(out, "PATH") {
		t.Errorf("list: %d\n%s", code, out)
	}
	code, out, _ = run(t, "provider", "info", "fake")
	if code != 0 || !strings.Contains(out, "fake_dashboard") || !strings.Contains(out, "FAKE_TOKEN") {
		t.Errorf("info: %d\n%s", code, out)
	}
	code, _, errOut := run(t, "provider", "info", "nope")
	if code == 0 || !strings.Contains(errOut, "gitopsify provider install nope") {
		t.Errorf("info missing: %d\n%s", code, errOut)
	}
	code, out, _ = run(t, "provider", "search", "--index", writeIndex(t), "dash")
	if code != 0 || !strings.Contains(out, "acme/fake") {
		t.Errorf("search: %d\n%s", code, out)
	}
	code, out, _ = run(t, "export", "--help")
	if code != 0 || strings.Contains(out, "--kubeconfig") {
		t.Errorf("export --help shows cluster flags:\n%s", out)
	}
}

func writeIndex(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "index.yaml")
	if err := os.WriteFile(p, []byte("kind: ProviderIndex\nproviders:\n- name: fake\n  source: acme/fake\n  description: fake dashboards\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSaveWithAnInstanceLeftOut(t *testing.T) {
	dir := fakeEnv(t)
	cfg := "apiVersion: gitopsify.io/v1alpha1\nkind: Export\nmetadata: {name: fake}\nspec:\n  provider: {name: fake}\n  instances:\n" +
		"  - name: solo\n    settings: {url: http://solo}\n  - name: both\n    settings: {url: http://both}\n  output: {formats: [terraform]}\n"
	if err := os.WriteFile(filepath.Join(dir, "gitopsify-fake.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := run(t, "export", "fake", "--partition", "two", "--save")
	if code != 0 || !strings.Contains(errOut, "solo: no org matches") {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	x, err := config.LoadExport(filepath.Join(dir, "gitopsify-fake.yaml"), "")
	if err != nil {
		t.Fatal(err)
	}
	if x.Spec.Instances[0].Settings["url"].Value != "http://solo" || x.Spec.Instances[1].Settings["url"].Value != "http://both" {
		t.Errorf("saved settings: %+v", x.Spec.Instances)
	}
}

// TestSaveIntoASharedFile: saving an export kept in gitopsify.yaml with the
// migration and other exports changes only its own document.
func TestSaveIntoASharedFile(t *testing.T) {
	dir := fakeEnv(t)
	migration := "# yaml-language-server: $schema=x\napiVersion: gitopsify.io/v1alpha1\nkind: Migration\nmetadata: {name: m}  # mine\nspec:\n  source: {context: test}\n"
	other := "apiVersion: gitopsify.io/v1alpha1\nkind: Export\nmetadata: {name: other}\nspec:\n  provider: {name: other}\n"
	fake := "apiVersion: gitopsify.io/v1alpha1\nkind: Export\nmetadata: {name: fake}\nspec:\n  provider: {name: fake}\n  instances:\n  - name: both\n    settings: {url: http://both}\n  output: {formats: [terraform]}\n"
	shared := filepath.Join(dir, "gitopsify.yaml")
	if err := os.WriteFile(shared, []byte(migration+"---\n"+fake+"---\n"+other), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := run(t, "export", "fake", "--config", "gitopsify.yaml", "--save")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	b, _ := os.ReadFile(shared)
	if !strings.HasPrefix(string(b), migration+"---\n") || !strings.HasSuffix(string(b), "---\n"+other) {
		t.Fatalf("other documents changed:\n%s", b)
	}
	if _, err := config.Load(shared); err != nil {
		t.Error(err)
	}
	if x, err := config.LoadExport(shared, "fake"); err != nil || x.Spec.Output.Dir != "fake" {
		t.Errorf("saved export: %+v %v", x, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gitopsify-fake.yaml")); err == nil {
		t.Error("saved to another file as well")
	}
}

// TestSaveFromASharedJSONFile: a JSON file with several resources isn't
// rewritten; the export is saved on its own next to it (paths in it are
// relative to the file), with a warning.
func TestSaveFromASharedJSONFile(t *testing.T) {
	dir := fakeEnv(t)
	if err := os.Mkdir(filepath.Join(dir, "infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(dir, "infra", "gitopsify.json")
	content := `[{"apiVersion": "gitopsify.io/v1alpha1", "kind": "Migration", "metadata": {"name": "m"}, "spec": {}},
 {"apiVersion": "gitopsify.io/v1alpha1", "kind": "Export", "metadata": {"name": "fake"},
  "spec": {"provider": {"name": "fake"}, "instances": [{"name": "both", "settings": {"url": "http://both"}}], "output": {"formats": ["terraform"]}}}]`
	if err := os.WriteFile(shared, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := run(t, "export", "fake", "--config", "infra/gitopsify.json", "--save")
	if code != 0 || !strings.Contains(errOut, "can't be updated in place") {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if b, _ := os.ReadFile(shared); string(b) != content {
		t.Errorf("shared file rewritten:\n%s", b)
	}
	if x, err := config.LoadExport(filepath.Join(dir, "infra", "gitopsify-fake.yaml"), ""); err != nil || x.Spec.Instances[0].Settings["url"].Value != "http://both" {
		t.Errorf("saved export: %+v %v", x, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gitopsify-fake.yaml")); err == nil {
		t.Error("saved in the working directory, away from the file its paths are relative to")
	}
}
