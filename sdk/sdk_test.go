package sdk_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hayone1/gitopsify/sdk"
	"github.com/hayone1/gitopsify/sdk/sdktest"
)

type fake struct {
	checkErr error
	panic    bool
}

func (fake) Describe() sdk.Description {
	return sdk.Description{
		Name:      "fake",
		Title:     "Fake",
		Version:   "v0.0.1",
		Settings:  []sdk.Setting{{Name: "url", Description: "where", Env: "FAKE_URL", Required: true, Terraform: "url", Crossplane: "url"}},
		Partition: sdk.Noun{Singular: "org", Plural: "orgs"},
		Kinds: []sdk.Kind{
			{Name: "Folder", Description: "folders", Terraform: &sdk.TerraformType{Type: "fake_folder"},
				Crossplane: &sdk.CrossplaneType{APIVersion: "oss.fake.m.crossplane.io/v1alpha1", Kind: "Folder"}},
			{Name: "Dashboard", Description: "dashboards", Terraform: &sdk.TerraformType{Type: "fake_dashboard"},
				Crossplane: &sdk.CrossplaneType{APIVersion: "oss.fake.m.crossplane.io/v1alpha1", Kind: "Dashboard"}},
		},
		Terraform:  &sdk.TerraformProvider{Name: "fake", Source: "acme/fake", Version: "~> 1.0"},
		Crossplane: &sdk.CrossplaneProvider{Name: "provider-fake", Package: "xpkg.example.com/acme/provider-fake:v1.0.0", ProviderConfig: sdk.GVK{APIVersion: "fake.m.crossplane.io/v1beta1", Kind: "ClusterProviderConfig"}},
	}
}

func (f fake) Check(_ context.Context, req sdk.Request) (*sdk.CheckResult, error) {
	if f.checkErr != nil {
		return nil, f.checkErr
	}
	return &sdk.CheckResult{Target: "Fake at " + req.Settings["url"], Partitions: []sdk.Partition{{ID: "1", Name: "Main"}}}, nil
}

func (f fake) Discover(ctx context.Context, req sdk.Request, out sdk.Emitter) error {
	if f.panic {
		panic("boom")
	}
	sdk.Logger(ctx).Info("listing", "url", req.Settings["url"])
	if err := out.Partition(sdk.Partition{ID: "1", Name: "Main"}); err != nil {
		return err
	}
	if req.Scope.HasKind("Folder") {
		if err := out.Resource(sdk.Resource{Key: "folder/1/a", Kind: "Folder", Partition: "1", Name: "A", ImportID: "1:a",
			Attributes: map[string]any{"title": "A & B", "uid": "a", "org_id": json.Number("1")}}); err != nil {
			return err
		}
	}
	if err := out.Warn(sdk.Warning{Partition: "1", Kind: "Alert", Message: "forbidden"}); err != nil {
		return err
	}
	return out.Resource(sdk.Resource{Key: "dashboard/1/d", Kind: "Dashboard", Partition: "1", Name: "D <1>", ImportID: "1:d",
		Attributes: map[string]any{
			"folder":      sdk.Ref("folder/1/a", "uid", "a", "folderRef"),
			"config_json": sdk.File("dashboards/a/d.json", sdk.RawJSON(`{"title":"D \u003c1\u003e","panels":[{"b":1,"a":2}]}`)),
			"meta":        sdk.JSON(map[string]any{"x": 1}),
			"token":       sdk.Secret("token", "s3cret", "tokenSecretRef", "API token"),
			"password":    sdk.Secret("password", nil, "passwordSecretRef", ""),
		}})
}

func TestConformance(t *testing.T) {
	sdktest.Conformance(t, fake{}, sdk.Request{Settings: map[string]string{"url": "http://x"}})
}

func TestDiscoverRoundTrip(t *testing.T) {
	inv := sdktest.Discover(t, fake{}, sdk.Request{Settings: map[string]string{"url": "http://x"}})
	if len(inv.Partitions) != 1 || len(inv.Resources) != 2 || len(inv.Warnings) != 1 {
		t.Fatalf("got %d partitions, %d resources, %d warnings", len(inv.Partitions), len(inv.Resources), len(inv.Warnings))
	}
	folder := inv.Resources[0].Attributes
	if folder["title"] != "A & B" {
		t.Errorf("title = %#v", folder["title"])
	}
	if n, ok := folder["org_id"].(json.Number); !ok || n.String() != "1" {
		t.Errorf("org_id = %#v, want json.Number 1", folder["org_id"])
	}
	a := inv.Resources[1].Attributes
	ref, ok := a["folder"].(sdk.RefValue)
	if !ok || ref.Ref != "folder/1/a" || ref.Attr != "uid" || ref.Value != "a" || ref.Crossplane != "folderRef" {
		t.Errorf("folder = %#v", a["folder"])
	}
	file, ok := a["config_json"].(sdk.FileValue)
	if !ok {
		t.Fatalf("config_json = %#v", a["config_json"])
	}
	text, err := sdk.FileText(file.Content)
	if err != nil {
		t.Fatal(err)
	}
	// Key order is kept and HTML escapes are undone.
	want := "{\n  \"title\": \"D <1>\",\n  \"panels\": [\n    {\n      \"b\": 1,\n      \"a\": 2\n    }\n  ]\n}\n"
	if string(text) != want {
		t.Errorf("file text:\n%s\nwant:\n%s", text, want)
	}
	if s, ok := a["token"].(sdk.SecretValue); !ok || s.Value != "s3cret" || s.Crossplane != "tokenSecretRef" {
		t.Errorf("token = %#v", a["token"])
	}
	if s, ok := a["password"].(sdk.SecretValue); !ok || s.Value != nil {
		t.Errorf("password = %#v", a["password"])
	}
	if j, ok := a["meta"].(sdk.JSONValue); !ok || string(j.JSON.(sdk.RawJSON)) != `{"x":1}` {
		t.Errorf("meta = %#v", a["meta"])
	}
}

func TestScope(t *testing.T) {
	inv := sdktest.Discover(t, fake{}, sdk.Request{Scope: sdk.Scope{Kinds: []string{"Dashboard"}}})
	if len(inv.Resources) != 1 || inv.Resources[0].Kind != "Dashboard" {
		t.Fatalf("resources = %+v", inv.Resources)
	}
}

func TestErrors(t *testing.T) {
	_, err := sdktest.CheckErr(t, fake{checkErr: sdk.Errorf(sdk.CodeUnauthenticated, "bad token").WithHint("set FAKE_TOKEN")}, sdk.Request{})
	var e *sdk.Error
	if !errors.As(err, &e) || e.Code != sdk.CodeUnauthenticated || e.Hint != "set FAKE_TOKEN" {
		t.Fatalf("err = %#v", err)
	}
	_, err = sdktest.CheckErr(t, fake{checkErr: errors.New("plain")}, sdk.Request{})
	if !errors.As(err, &e) || e.Code != sdk.CodeInternal || e.Message != "plain" {
		t.Fatalf("err = %#v", err)
	}
	_, err = sdktest.DiscoverErr(t, fake{panic: true}, sdk.Request{})
	if !errors.As(err, &e) || e.Code != sdk.CodeInternal || !strings.Contains(e.Message, "boom") {
		t.Fatalf("panic err = %#v", err)
	}
}

func TestRunRejectsOtherProtocols(t *testing.T) {
	var out bytes.Buffer
	code := sdk.Run(context.Background(), fake{}, []string{"check"}, strings.NewReader(`{"apiVersion":"provider.gitopsify.io/v9"}`), &out, &bytes.Buffer{})
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	_, err := sdk.ReadCheck(&out)
	var e *sdk.Error
	if !errors.As(err, &e) || e.Code != sdk.CodeUnsupported {
		t.Fatalf("err = %v", err)
	}
}

func TestRunCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	sdk.Run(ctx, fake{}, []string{"discover"}, strings.NewReader(`{}`), &out, &bytes.Buffer{})
	_, err := sdk.ReadInventory(&out, nil)
	var e *sdk.Error
	if !errors.As(err, &e) || e.Code != sdk.CodeCanceled {
		t.Fatalf("err = %v", err)
	}
}

func TestRunUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := sdk.Run(context.Background(), fake{}, nil, nil, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "gitopsify export fake") {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if code := sdk.Run(context.Background(), fake{}, []string{"--version"}, nil, &out, &errOut); code != 0 || !strings.Contains(out.String(), "v0.0.1") {
		t.Fatalf("exit %d, out %q", code, out.String())
	}
}

func TestReadIncomplete(t *testing.T) {
	_, err := sdk.ReadInventory(strings.NewReader(`{"partition":{"id":"1","name":"x"}}`+"\n"), nil)
	if !errors.Is(err, sdk.ErrIncomplete) {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeValueRejectsTwoMarkers(t *testing.T) {
	if _, err := sdk.DecodeValue([]byte(`{"$ref":"a","$json":{}}`)); err == nil {
		t.Fatal("want an error")
	}
}

func TestCompactJSONUnescapesOnlyRealEscapes(t *testing.T) {
	got, err := sdk.CompactJSON(sdk.RawJSON(`{"a": "x \u003c y", "b": "\\u003c stays"}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a":"x < y","b":"\\u003c stays"}`; string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestConformanceFindsProblems(t *testing.T) {
	d := fake{}.Describe()
	d.APIVersion = sdk.APIVersion
	d.Protocols = []string{sdk.Protocol}
	inv := sdk.Inventory{
		Partitions: []sdk.Partition{{ID: "1", Name: "Main"}},
		Resources: []sdk.Resource{
			{Key: "a", Kind: "Folder", Partition: "1", Name: "A", ImportID: "a", Attributes: map[string]any{"x": sdk.Secret("s", "v", "", "")}},
			{Key: "a", Kind: "Nope", Partition: "2", Name: "B"},
			{Key: "c", Kind: "Dashboard", Partition: "1", Name: "C", ImportID: "c", Attributes: map[string]any{"f": sdk.File("../x.json", "{}")}},
		},
	}
	got := strings.Join(sdktest.CheckInventory(d, inv), "\n")
	for _, want := range []string{"used twice", "isn't in the description", "must be relative"} {
		if !strings.Contains(got, want) {
			t.Errorf("problems don't mention %q:\n%s", want, got)
		}
	}
}

type resolver map[string][2]string

func (r resolver) Ref(key string) (string, string, bool) {
	v, ok := r[key]
	return v[0], v[1], ok
}

func (r resolver) Secret(path string, s sdk.SecretValue) (string, string, bool) {
	return "creds", s.Secret, true
}

func TestForProvider(t *testing.T) {
	k := &sdk.Kind{Name: "Policy", Terraform: &sdk.TerraformType{Type: "x_policy", Blocks: []string{"policy", "policy.matcher"}},
		Crossplane: &sdk.CrossplaneType{APIVersion: "a.b/v1", Kind: "Policy", Rename: map[string]string{"group_by": "groupBy2"}}}
	r := &sdk.Resource{Key: "p", Attributes: map[string]any{
		"org_id":   sdk.Ref("org/2", "id", "2", "organizationRef"),
		"folder":   sdk.Ref("folder/missing", "uid", "f1", "folderRef"),
		"group_by": []any{"alertname"},
		"labels":   map[string]any{"team_name": "sre", "app.kubernetes.io/name": "x"},
		"policy": []any{map[string]any{
			"contact_point": sdk.Ref("cp/a", "name", "a", "contactPointRef"),
			"matcher":       []any{map[string]any{"label_name": "team", "match_type": "="}},
			"group_wait":    "30s",
		}},
		"members":  []any{sdk.Ref("user/1", "email", "a@x", "memberRefs"), sdk.Ref("user/2", "email", "b@x", "memberRefs")},
		"token":    sdk.Secret("token", "t", "tokenSecretRef", ""),
		"headers":  sdk.Secret("headers", map[string]string{"X": "y"}, "httpHeadersSecretRef", ""),
		"password": sdk.Secret("password", nil, "passwordSecretRef", ""),
		"client":   sdk.Secret("client", "v", "", ""),
		"json":     sdk.JSON(sdk.RawJSON(`{"b": 1, "a": "<x>"}`)),
		"count":    json.Number("3"),
	}}
	got, omitted, err := sdk.ForProvider(k, r, resolver{"org/2": {"org-2", "grafana"}, "cp/a": {"a", ""}, "user/1": {"u1", ""}, "user/2": {"u2", ""}})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(got)
	gotJSON := bytes.TrimSpace(buf.Bytes())
	want := `{"count":3,"folder":"f1","groupBy2":["alertname"],"httpHeadersSecretRef":{"name":"creds"},"json":"{\"b\":1,\"a\":\"<x>\"}","labels":{"app.kubernetes.io/name":"x","team_name":"sre"},"memberRefs":[{"name":"u1"},{"name":"u2"}],"organizationRef":{"name":"org-2","namespace":"grafana"},"policy":[{"contactPoint":"a","groupWait":"30s","matcher":[{"labelName":"team","matchType":"="}]}],"tokenSecretRef":{"key":"token","name":"creds"}}`
	if string(gotJSON) != want {
		t.Errorf("got  %s\nwant %s", gotJSON, want)
	}
	if len(omitted) != 2 || omitted[0].Path != "client" || omitted[1].Path != "password" {
		t.Errorf("omitted %+v", omitted)
	}
}

func TestLowerCamel(t *testing.T) {
	for in, want := range map[string]string{"org_id": "orgId", "uid": "uid", "secure_json_data_encoded": "secureJsonDataEncoded", "x509_cert": "x509Cert"} {
		if got := sdk.LowerCamel(in); got != want {
			t.Errorf("LowerCamel(%q) = %q", in, got)
		}
	}
}

func TestUnknownMarkerIsAnError(t *testing.T) {
	if _, err := sdk.DecodeValue([]byte(`{"a": {"$secrett": "x"}}`)); err == nil || !strings.Contains(err.Error(), "unknown marker") {
		t.Fatalf("err = %v", err)
	}
}

func TestSummary(t *testing.T) {
	r := sdk.Resource{
		Key: "note/ops/n1", Kind: "Note", Name: "n1", ImportID: "ops/n1",
		DependsOn: []string{"webhook/ops/w1", "webhook/ops/w1"},
		Attributes: map[string]any{
			"folder_id": sdk.Ref("folder/ops/f2", "id", "f2", "folderIdRef"),
			"links":     []any{sdk.Ref("note/ops/n0", "id", "n0", ""), sdk.Ref("note/ops/n1", "id", "n1", "")},
			"block":     []any{map[string]any{"owner": sdk.Ref("folder/ops/f1", "id", "f1", "")}},
			// Typed Go containers serialize the same way.
			"typed": []map[string]any{{"user": sdk.Ref("user/1", "id", "1", "")}},
			"refs":  map[string]sdk.RefValue{"a": sdk.Ref("user/2", "id", "2", "")},
		},
		Crossplane: map[string]any{"extra": sdk.Ref("webhook/ops/w1", "id", "w1", "")},
	}
	s := r.Summary()
	if s.Attributes != nil || s.Crossplane != nil || s.Key != r.Key || s.ImportID != r.ImportID {
		t.Errorf("summary keeps the body or loses the rest: %+v", s)
	}
	if got := strings.Join(s.DependsOn, ","); got != "webhook/ops/w1,folder/ops/f1,folder/ops/f2,note/ops/n0,user/1,user/2" {
		t.Errorf("dependsOn = %s", got)
	}
	if r.Attributes == nil || len(r.DependsOn) != 2 {
		t.Errorf("Summary changed the original: %+v", r)
	}
	if s := (sdk.Resource{Key: "a"}).Summary(); s.DependsOn != nil {
		t.Errorf("no references: dependsOn = %#v", s.DependsOn)
	}
}
