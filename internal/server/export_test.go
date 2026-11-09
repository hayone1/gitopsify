package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/sdk"
)

// The test binary is also a provider: TestMain serves fakeProvider when
// GITOPSIFY_TEST_PROVIDER is set, the way gitopsify runs providers.
func TestMain(m *testing.M) {
	if os.Getenv("GITOPSIFY_TEST_PROVIDER") == "1" {
		sdk.Serve(fakeProvider{})
		return
	}
	os.Exit(m.Run())
}

type fakeProvider struct{}

func (fakeProvider) Describe() sdk.Description {
	return sdk.Description{
		APIVersion: sdk.APIVersion, Name: "fake", Title: "Fake", Version: "v1.0.0", Protocols: []string{sdk.Protocol},
		Settings: []sdk.Setting{
			{Name: "url", Description: "URL", Env: "FAKE_URL", Required: true, Terraform: "url"},
			{Name: "token", Description: "token", Env: "FAKE_TOKEN", Secret: true, Required: true, Terraform: "token"},
		},
		Partition: sdk.Noun{Singular: "org", Plural: "orgs"},
		Kinds: []sdk.Kind{
			{Name: "Folder", Description: "folders", Terraform: &sdk.TerraformType{Type: "fake_folder"}},
			{Name: "Hook", Description: "hooks", Terraform: &sdk.TerraformType{Type: "fake_hook"}},
		},
		Terraform: &sdk.TerraformProvider{Name: "fake", Source: "acme/fake", Version: "~> 1.0"},
	}
}

func (fakeProvider) Check(_ context.Context, req sdk.Request) (*sdk.CheckResult, error) {
	if req.Settings["token"] != "t0ken" {
		return nil, sdk.Errorf(sdk.CodeUnauthenticated, "wrong token")
	}
	return &sdk.CheckResult{Target: "Fake at " + req.Settings["url"], Identity: "admin",
		Partitions: []sdk.Partition{{ID: "1", Name: "Main"}, {ID: "2", Name: "Other"}},
		Kinds:      []sdk.KindCheck{{Kind: "Folder", OK: true}, {Kind: "Hook", OK: true}}}, nil
}

func (fakeProvider) Discover(_ context.Context, req sdk.Request, out sdk.Emitter) error {
	for _, p := range []sdk.Partition{{ID: "1", Name: "Main"}, {ID: "2", Name: "Other"}} {
		if !req.Scope.HasPartition(p.ID) {
			continue
		}
		if err := out.Partition(p); err != nil {
			return err
		}
		if req.Scope.HasKind("Folder") {
			_ = out.Resource(sdk.Resource{Key: "folder/" + p.ID + "/a", Kind: "Folder", Partition: p.ID, Name: "A", ImportID: p.ID + ":a",
				Attributes: map[string]any{"title": "A"}})
		}
		if req.Scope.HasKind("Hook") {
			_ = out.Resource(sdk.Resource{Key: "hook/" + p.ID + "/h", Kind: "Hook", Partition: p.ID, Name: "H", ImportID: p.ID + ":h",
				Attributes: map[string]any{"url": sdk.Secret("url", "https://hooks/secret", "", "hook URL")}})
		}
	}
	return nil
}

// installFake puts a gitopsify-provider-fake on PATH that runs this binary
// as the fake provider.
func installFake(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nGITOPSIFY_TEST_PROVIDER=1 exec '" + exe + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gitopsify-provider-fake"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITOPSIFY_PROVIDERS_DIR", t.TempDir())
}

func TestExport(t *testing.T) {
	installFake(t)
	t.Setenv("FAKE_TOKEN", "t0ken")
	e := newTestEnv(t, func(c *config.Server) {
		c.Spec.Tools = []config.ServerTool{{Name: "fake-prod", Provider: "fake", Settings: map[string]config.SettingValue{"url": {Value: "https://fake.acme.io"}}}}
		c.Spec.RBAC.Bindings = append(c.Spec.RBAC.Bindings, config.ServerBinding{Role: "operator", Groups: []string{"obs"}, Tools: []string{"fake-*"}})
	})
	obs := e.login(map[string]any{"sub": "9", "email": "olu@acme.io", "email_verified": true, "groups": []string{"obs"}})
	shop := e.login(ada) // operator on the dev cluster only: viewer on tools via the default role

	var tools []toolInfo
	obs.get("/api/v1/tools", &tools)
	if len(tools) != 1 || tools[0].Role != "operator" || !tools[0].Installed || tools[0].Title != "Fake" {
		t.Fatalf("tools: %+v", tools)
	}
	var tc toolCheck
	obs.get("/api/v1/tools/fake-prod/check", &tc)
	if tc.Error != "" || tc.Check == nil || len(tc.Check.Partitions) != 2 {
		t.Fatalf("check: %+v", tc)
	}
	exp := "apiVersion: gitopsify.io/v1alpha1\nkind: Export\nmetadata: {name: fake-prod}\nspec:\n  provider: {name: fake}\n  select:\n    partitions: [Main]\n  output:\n    formats: [terraform]\n"
	if code := shop.do(http.MethodPost, "/api/v1/runs", runRequest{Type: RunExport, Tool: "fake-prod", Export: exp}, nil); code != http.StatusForbidden {
		t.Errorf("a viewer of the tool exported: %d", code)
	}
	bad := exp + "  instances:\n    - name: x\n      settings: {url: http://evil}\n"
	if code := obs.do(http.MethodPost, "/api/v1/runs", runRequest{Type: RunExport, Tool: "fake-prod", Export: strings.Replace(bad, "  output:\n    formats: [terraform]\n", "", 1)}, nil); code != http.StatusBadRequest {
		t.Errorf("an export naming its own instances ran: %d", code)
	}
	var run Run
	if code := obs.do(http.MethodPost, "/api/v1/runs", runRequest{Type: RunExport, Tool: "fake-prod", Export: exp}, &run); code != http.StatusCreated {
		t.Fatalf("export: %d", code)
	}
	done := waitRun(t, e.s, run.ID)
	if done.Status != StatusSucceeded || done.Summary == nil || done.Summary.Objects != 2 || done.Tool != "fake-prod" || done.Cluster != "" {
		var lines []LogLine
		obs.get("/api/v1/runs/"+run.ID+"/log", &lines)
		t.Fatalf("run: %+v\nlog: %+v", done, lines)
	}
	var files []fileEntry
	obs.get("/api/v1/runs/"+run.ID+"/files", &files)
	all := ""
	for _, f := range files {
		all += f.Path + " "
	}
	if !strings.Contains(all, "terraform/fake-prod/main/") || strings.Contains(all, "other") || !strings.Contains(all, "gitopsify-fake.yaml") {
		t.Errorf("files: %s", all)
	}
	// The export file for CI names the environment, never the token.
	b, _ := os.ReadFile(filepath.Join(e.s.runs.outputDir(run.ID), "gitopsify-fake.yaml"))
	if !strings.Contains(string(b), "env: FAKE_TOKEN") || strings.Contains(string(b), "t0ken") || !strings.Contains(string(b), "https://fake.acme.io") {
		t.Errorf("export file:\n%s", b)
	}
	// Nothing in the output holds the secret value.
	_ = filepath.Walk(e.s.runs.outputDir(run.ID), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			if c, _ := os.ReadFile(p); strings.Contains(string(c), "hooks/secret") || strings.Contains(string(c), "t0ken") {
				t.Errorf("%s holds a secret", p)
			}
		}
		return nil
	})
	// The shop operator sees the run (viewer on tools) but can't push it.
	if code := shop.get("/api/v1/runs/"+run.ID, nil); code != 200 {
		t.Errorf("viewer reading the export: %d", code)
	}
	if code := shop.do(http.MethodPost, "/api/v1/runs/"+run.ID+"/publish", nil, nil); code != http.StatusForbidden {
		t.Errorf("viewer pushing the export: %d", code)
	}
}
