package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hayone1/gitopsify/internal/ui"
	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/kube"
)

// finding is one line of the doctor's report.
type finding struct {
	Status  string `json:"status"` // ok, warning, error
	Area    string `json:"area"`
	Check   string `json:"check"`
	Message string `json:"message"`
}

type doctor struct {
	o        *options
	cfg      *config.Config
	findings []finding
}

func (d *doctor) add(status, area, check, format string, args ...any) {
	d.findings = append(d.findings, finding{Status: status, Area: area, Check: check, Message: fmt.Sprintf(format, args...)})
}

func newDoctor(o *options) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that gitopsify can reach everything a migration needs",
		Long: `Checks, without changing anything:
  - the configuration file
  - the tools gitopsify calls: git, helm, kubectl or kustomize, sops
  - the source cluster: connection, how you authenticate, who you are and
    whether you may read everything
  - each environment's cluster: the same, and whether you may install Argo CD
    or Flux there (cluster-admin), and the sealed-secrets controller
  - the Git repositories: whether they can be reached with your credentials
Exits with status 1 when a check fails.`,
		Example: "  gitopsify doctor\n  gitopsify doctor --env uat --target-kubeconfig ~/.kube/uat.yaml",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			u := o.ui()
			d := &doctor{o: o}
			cfg, err := o.load(cmd)
			if err != nil {
				d.add("error", "config", "load", "%v", err)
				return d.report(u, format)
			}
			d.cfg = cfg
			d.config()
			d.tools()
			ctx := cmd.Context()
			if len(o.fromFiles) > 0 {
				d.add("ok", "source", "from-file", "reading objects from %s; no source cluster needed", strings.Join(o.fromFiles, ", "))
			} else {
				d.source(ctx)
			}
			envs := cfg.Environments
			if len(o.envs) > 0 {
				env, err := o.envName(cmd, cfg)
				if err != nil {
					d.add("error", "target", "env", "%v", err)
					return d.report(u, format)
				}
				envs = []config.Environment{*env}
			}
			for i := range envs {
				d.target(ctx, &envs[i])
			}
			d.git(ctx, envs)
			return d.report(u, format)
		},
	}
	o.addScope(cmd.Flags())
	o.addSecrets(cmd.Flags())
	o.addOutput(cmd.Flags())
	o.addTarget(cmd.Flags())
	cmd.Flags().StringVar(&format, "format", "table", "output format: table, json or yaml")
	complete(cmd.Flags(), "format", "table", "json", "yaml")
	return cmd
}

func (d *doctor) report(u *ui.UI, format string) error {
	errs := 0
	for _, f := range d.findings {
		if f.Status == "error" {
			errs++
		}
	}
	if format != "table" {
		if err := printReport(u, d.findings, format); err != nil {
			return err
		}
	} else {
		var rows [][]string
		for _, f := range d.findings {
			mark := map[string]string{"ok": "✓", "warning": "!", "error": "✗"}[f.Status]
			rows = append(rows, []string{u.Colorize(f.Status, mark), f.Area, f.Check, f.Message})
		}
		u.Table([]string{"", "Area", "Check", "Result"}, rows)
	}
	if errs > 0 {
		return &exitError{code: 1, msg: fmt.Sprintf("%d checks failed", errs)}
	}
	u.Success("ready to migrate")
	return nil
}

func (d *doctor) config() {
	path := d.o.configFile
	if path == "" && fileExists(DefaultConfigFile) {
		path = DefaultConfigFile
	}
	if err := d.cfg.Validate(); err != nil {
		d.add("error", "config", "validate", "%v", err)
		return
	}
	if path == "" {
		d.add("ok", "config", "file", "no gitopsify.yaml; using flags and defaults (gitopsify init writes one)")
		return
	}
	d.add("ok", "config", "file", "%s is valid", path)
}

func (d *doctor) tools() {
	cfg := d.cfg
	version := func(name string, args ...string) (string, bool) {
		p, err := exec.LookPath(name)
		if err != nil {
			return "", false
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, _ := exec.CommandContext(ctx, p, args...).CombinedOutput()
		line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		return line, true
	}
	if v, ok := version("git", "--version"); ok {
		d.add("ok", "tools", "git", "%s", v)
	} else {
		d.add("warning", "tools", "git", "not found: needed for --commit, --push and migrate")
	}
	if v, ok := version("helm", "version", "--short"); ok {
		d.add("ok", "tools", "helm", "%s", v)
	} else {
		d.add("warning", "tools", "helm", "not found: diff compares Helm apps using their recorded manifests instead of rendering the chart")
	}
	if cfg.Output.Tool == config.ToolNone {
		k, okK := version("kubectl", "version", "--client")
		z, okZ := version("kustomize", "version")
		switch {
		case okK:
			d.add("ok", "tools", "kubectl", "%s (runs the generated apply script)", k)
		case okZ:
			d.add("warning", "tools", "kubectl", "not found; kustomize %s builds the apps, but the apply script also needs kubectl", z)
		default:
			d.add("warning", "tools", "kubectl", "not found: the generated apply-<env>.sh needs it")
		}
	}
	if usesStrategy(cfg, config.SecretsSOPS) {
		if v, ok := version("sops", "--version"); ok {
			d.add("ok", "tools", "sops", "%s", v)
		} else {
			d.add("error", "tools", "sops", "not found, but the sops secrets strategy needs it (https://github.com/getsops/sops/releases)")
		}
	}
}

func usesStrategy(cfg *config.Config, s string) bool {
	if cfg.Secrets.Strategy == s {
		return true
	}
	for _, o := range cfg.Secrets.Overrides {
		if o.Strategy == s {
			return true
		}
	}
	return false
}

// connect connects and reports the cluster, how the client authenticates and
// who it is.
func (d *doctor) connect(ctx context.Context, area string, a config.Access) *kube.Client {
	c, err := kube.Connect(a, d.o.connectOptions())
	if err != nil {
		d.add("error", area, "connect", "%v", err)
		return nil
	}
	version, err := c.Ping()
	if err != nil {
		d.add("error", area, "connect", "%s: %v", c.Context, err)
		return nil
	}
	d.add("ok", area, "connect", "%s (%s), Kubernetes %s", c.Context, c.Server, version)
	who := "unknown user (SelfSubjectReview needs Kubernetes 1.28+)"
	if id, err := c.WhoAmI(ctx); err == nil {
		who = id.Username
		if len(id.Groups) > 0 {
			who += " (groups: " + strings.Join(id.Groups, ", ") + ")"
		}
	}
	d.add("ok", area, "identity", "%s via %s", who, c.Auth)
	return c
}

func (d *doctor) source(ctx context.Context) {
	c := d.connect(ctx, "source", d.cfg.Source.Access)
	if c == nil {
		return
	}
	namespaces := []string{""}
	if len(d.cfg.Scope.Namespaces) > 0 {
		namespaces = nil
		for _, n := range d.cfg.Scope.Namespaces {
			if !strings.ContainsAny(n, "*?[") {
				namespaces = append(namespaces, n)
			}
		}
		if len(namespaces) == 0 {
			namespaces = []string{""}
		}
	}
	if ok, err := c.CanI(ctx, kube.Permission{Verb: "list", Group: "*", Resource: "*"}); err == nil && ok {
		d.add("ok", "source", "permissions", "may list every kind in every namespace")
		return
	}
	var missing []string
	for _, ns := range namespaces {
		for _, p := range []kube.Permission{
			{Verb: "list", Resource: "configmaps"},
			{Verb: "list", Resource: "secrets"},
			{Verb: "list", Group: "apps", Resource: "deployments"},
			{Verb: "list", Group: "networking.k8s.io", Resource: "ingresses"},
		} {
			p.Namespace = ns
			if ok, err := c.CanI(ctx, p); err == nil && !ok {
				missing = append(missing, p.String())
			}
		}
	}
	for _, p := range []kube.Permission{
		{Verb: "list", Resource: "namespaces"},
		{Verb: "list", Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"},
	} {
		if ok, err := c.CanI(ctx, p); err == nil && !ok {
			missing = append(missing, p.String())
		}
	}
	if len(missing) > 0 {
		d.add("warning", "source", "permissions", "may not %s; those objects are left out (and listed as warnings by scan)", strings.Join(missing, ", "))
		return
	}
	d.add("ok", "source", "permissions", "may read the common kinds; kinds it may not list are reported by scan")
}

func (d *doctor) target(ctx context.Context, env *config.Environment) {
	area := "env " + env.Name
	if !env.Cluster.Access.Configured() {
		d.add("warning", area, "connect", "no cluster configured; only check, diff, adopt, status and bootstrap need one (--env %s=<context> or environments[].cluster)", env.Name)
		return
	}
	c := d.connect(ctx, area, env.Cluster.Access)
	if c == nil {
		return
	}
	if ok, err := c.CanI(ctx, kube.Permission{Verb: "*", Group: "*", Resource: "*"}); err == nil {
		if ok {
			d.add("ok", area, "permissions", "cluster-admin: bootstrap can install %s", toolName(d.cfg))
		} else {
			d.add("warning", area, "permissions", "not cluster-admin: bootstrap needs it to install CRDs and ClusterRoles; check and diff only need read access and dry-run patch")
		}
	}
	if usesStrategy(d.cfg, config.SecretsSealedSecrets) && d.cfg.Secrets.SealedSecrets.CertFile == "" {
		s := d.cfg.Secrets.SealedSecrets
		if _, err := c.Kubernetes.CoreV1().Services(s.ControllerNamespace).Get(ctx, s.ControllerName, metav1.GetOptions{}); err != nil {
			d.add("error", area, "sealed-secrets", "service %s/%s not found: install the sealed-secrets controller first, or pass --sealed-secrets-cert", s.ControllerNamespace, s.ControllerName)
		} else {
			d.add("ok", area, "sealed-secrets", "controller found; Secrets are sealed with its certificate")
		}
	}
}

func toolName(cfg *config.Config) string {
	switch cfg.Output.Tool {
	case config.ToolArgoCD:
		return "Argo CD"
	case config.ToolFlux:
		return "Flux"
	}
	return "Argo CD or Flux"
}

// git checks that the repositories the GitOps tool will pull from answer to
// the user's Git credentials.
func (d *doctor) git(ctx context.Context, envs []config.Environment) {
	urls := map[string]bool{}
	if d.cfg.Output.Layout == config.LayoutRepoPerEnv {
		for _, e := range envs {
			if u := repoURLOf(d.cfg, &e); u != "" {
				urls[u] = true
			}
		}
	} else if d.cfg.Output.Repo.URL != "" {
		urls[d.cfg.Output.Repo.URL] = true
	}
	if len(urls) == 0 {
		if d.cfg.Output.Tool != config.ToolNone {
			d.add("error", "git", "repository", "no repository URL (--repo-url); %s needs one to sync from", toolName(d.cfg))
		}
		return
	}
	if _, err := exec.LookPath("git"); err != nil {
		return
	}
	for u := range urls {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		cmd := exec.CommandContext(cctx, "git", "ls-remote", "--heads", u)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o BatchMode=yes -o ConnectTimeout=15")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		cancel()
		switch {
		case err != nil:
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = err.Error()
			}
			d.add("warning", "git", "repository", "%s: %s (generate --push and the GitOps tool need access; the tool uses bootstrap's --git-token or --ssh-key)", u, firstLine(msg))
		case strings.TrimSpace(string(out)) == "":
			d.add("ok", "git", "repository", "%s is reachable and empty", u)
		default:
			d.add("ok", "git", "repository", "%s is reachable", u)
		}
	}
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}
