package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hayone1/gitopsify/pkg/config"
)

const fixture = "../../testdata/clusters/clickops.yaml"

// run executes the command line and returns its exit code, stdout and
// stderr.
func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	ro, wo, _ := os.Pipe()
	re, we, _ := os.Pipe()
	os.Stdout, os.Stderr = wo, we
	outc, errc := make(chan string), make(chan string)
	go func() { b, _ := io.ReadAll(ro); outc <- string(b) }()
	go func() { b, _ := io.ReadAll(re); errc <- string(b) }()

	o := &options{}
	root := newRoot(o)
	root.SetArgs(append([]string{"--no-input", "--no-color"}, args...))
	code := 0
	if err := root.ExecuteContext(context.Background()); err != nil {
		code = 1
		if ee, ok := err.(*exitError); ok {
			code = ee.code
			if ee.msg != "" {
				o.ui().Errorf("%s", ee.msg)
			}
		} else {
			o.ui().Errorf("%v", err)
		}
	}
	wo.Close()
	we.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	return code, <-outc, <-errc
}

func chdir(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Chdir(dir)
	return abs
}

func TestHelpGroupsFlags(t *testing.T) {
	code, out, _ := run(t, "generate", "--help")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, section := range []string{"\nScope:\n", "\nHelm:\n", "\nSecrets:\n", "\nOutput:\n", "\nGit:\n", "\nTarget cluster access:\n", "\nSource cluster access:\n", "\nGlobal Flags:\n"} {
		if !strings.Contains(out, section) {
			t.Errorf("missing section %q", strings.TrimSpace(section))
		}
	}
	if strings.Contains(out, "--from-file kubectl") {
		t.Error("backquotes in a usage string turned into a value name")
	}
}

func TestScanJSON(t *testing.T) {
	dump := chdir(t)
	code, out, errOut := run(t, "scan", "--from-file", dump, "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var r scanReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(r.Apps) == 0 || r.ExcludedByRule["system-namespaces"] == 0 {
		t.Errorf("unexpected report: %d apps, %v", len(r.Apps), r.ExcludedByRule)
	}
}

func TestGenerateAndRegenerate(t *testing.T) {
	dump := chdir(t)
	args := []string{"generate", "--from-file", dump, "--tool", "flux", "--env", "uat", "--repo-url", "https://example.com/gitops.git", "-o", "out"}
	if code, _, errOut := run(t, args...); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, f := range []string{"out/gitopsify.yaml", "out/MIGRATION_REPORT.md", "out/clusters/uat/flux-system/gotk-sync.yaml"} {
		if _, err := os.Stat(f); err != nil {
			t.Error(err)
		}
	}
	// A file added by hand survives regeneration; the written configuration
	// regenerates the same tree.
	if err := os.WriteFile("out/NOTES.md", []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, "generate", "--from-file", dump, "-c", "out/gitopsify.yaml"); code != 0 {
		t.Fatalf("regenerate: exit %d: %s", code, errOut)
	}
	if _, err := os.Stat("out/NOTES.md"); err != nil {
		t.Error("hand-written file removed")
	}
	cfg, err := config.Load("out/gitopsify.yaml")
	if err != nil || cfg.Output.Tool != config.ToolFlux || cfg.Environments[0].Name != "uat" {
		t.Errorf("written configuration: %+v, %v", cfg, err)
	}
	// Another directory not made by gitopsify is refused without --force.
	if err := os.MkdirAll("other", 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile("other/x", nil, 0o644)
	if code, _, errOut := run(t, "generate", "--from-file", dump, "-o", "other"); code == 0 || !strings.Contains(errOut, "--force") {
		t.Errorf("expected a refusal, got %d: %s", code, errOut)
	}
}

func TestDryRunAndDuplicateEnv(t *testing.T) {
	dump := chdir(t)
	code, out, _ := run(t, "generate", "--from-file", dump, "--dry-run", "-o", "x")
	if code != 0 || !strings.Contains(out, "x/MIGRATION_REPORT.md") {
		t.Errorf("dry run: %d\n%s", code, out)
	}
	if _, err := os.Stat("x"); err == nil {
		t.Error("dry run wrote files")
	}
	if code, _, errOut := run(t, "generate", "--from-file", dump, "--env", "uat", "--env", "uat"); code == 0 || !strings.Contains(errOut, "given twice") {
		t.Errorf("duplicate --env: %d %s", code, errOut)
	}
}

func TestValidationErrors(t *testing.T) {
	dump := chdir(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"generate", "--from-file", dump, "--tool", "argocd"}, "output.repo.url"},
		{[]string{"generate", "--from-file", dump, "--secrets", "plain"}, "allowPlain"},
		{[]string{"scan", "--from-file", dump, "--disable-rule", "nope"}, "unknown rule"},
		{[]string{"scan", "--from-file", dump, "--server", "10.0.0.1:6443"}, "must be a URL"},
		{[]string{"scan", "--from-file", dump, "--wave", "x"}, "expected key=value"},
		{[]string{"generate", "--from-file", dump, "--env", "a", "--env", "b", "--target-context", "x"}, "exactly one environment"},
		{[]string{"check", "--from-file", dump, "--env", "uat"}, "no cluster to connect to"},
	} {
		code, _, errOut := run(t, c.args...)
		if code == 0 || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: exit %d, want %q in:\n%s", c.args, code, c.want, errOut)
		}
	}
}

func TestTargetFlagsAndTokens(t *testing.T) {
	o := &options{}
	cmd := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
	o.addGlobal(cmd.Flags())
	o.addOutput(cmd.Flags())
	o.addTarget(cmd.Flags())
	t.Setenv(envSourceToken, "from-env")
	t.Setenv(envTargetToken, "target-env")
	t.Chdir(t.TempDir())
	if err := cmd.Flags().Parse([]string{"--env", "uat=uat-ctx", "--target-kubeconfig", "~/uat.yaml", "--target-as", "jane"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := o.load(cmd)
	if err != nil {
		t.Fatal(err)
	}
	src, env := cfg.Source.Access, cfg.Environments[0].Cluster.Access
	if src.Token != "from-env" {
		t.Errorf("source token %q", src.Token)
	}
	if env.Context != "uat-ctx" || env.Kubeconfig != "~/uat.yaml" || env.Impersonate != "jane" || env.Token != "target-env" {
		t.Errorf("target access %+v", env)
	}
	b, _ := cfg.Marshal()
	if bytes.Contains(b, []byte("from-env")) || bytes.Contains(b, []byte("target-env")) {
		t.Error("a token reached the written configuration")
	}
}

func TestDoctorOffline(t *testing.T) {
	dump := chdir(t)
	code, out, errOut := run(t, "doctor", "--from-file", dump)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "from-file") || !strings.Contains(out, "no cluster configured") {
		t.Errorf("doctor output:\n%s", out)
	}
	code, out, _ = run(t, "doctor", "--from-file", dump, "--tool", "flux", "--format", "json")
	var findings []finding
	if err := json.Unmarshal([]byte(out), &findings); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if code != 1 {
		t.Errorf("a missing --repo-url for flux must fail, got %d", code)
	}
}

func TestSkillsInstall(t *testing.T) {
	dir := t.TempDir()
	if code, _, errOut := run(t, "skills", "install", "--dir", dir); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "gitopsify-migrate", "references", "report-warnings.md")); err != nil {
		t.Error(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	if code, _, errOut := run(t, "skills", "install", "--agent", "codex,claude"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, d := range []string{".agents/skills/gitopsify-migrate/SKILL.md", ".claude/skills/gitopsify-git-remote/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(home, d)); err != nil {
			t.Error(err)
		}
	}
	if code, _, _ := run(t, "skills", "install", "--agent", "vim"); code == 0 {
		t.Error("unknown agent accepted")
	}
}

func TestVersionAndRules(t *testing.T) {
	if code, out, _ := run(t, "version"); code != 0 || !strings.HasPrefix(out, "gitopsify ") {
		t.Errorf("version: %d %q", code, out)
	}
	if code, out, _ := run(t, "rules"); code != 0 || !strings.Contains(out, "platform-crds") || !strings.Contains(out, "terminating") {
		t.Errorf("rules: %d\n%s", code, out)
	}
}

// TestCommandReferenceIsCurrent keeps docs/commands.md in step with the
// commands; run `make docs` after changing a command or flag.
func TestCommandReferenceIsCurrent(t *testing.T) {
	want := commandReference(newRoot(&options{}))
	got, err := os.ReadFile("../../docs/commands.md")
	if err != nil || string(got) != want {
		if os.Getenv("UPDATE_DOCS") != "" {
			if err := os.WriteFile("../../docs/commands.md", []byte(want), 0o644); err != nil {
				t.Fatal(err)
			}
			return
		}
		t.Error("docs/commands.md is out of date: run make docs")
	}
}
