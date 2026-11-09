package cli

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hayone1/gitopsify/internal/ui"
	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/filter"
	"github.com/hayone1/gitopsify/pkg/kube"
	"github.com/hayone1/gitopsify/pkg/match"
)

func newInit(o *options) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Describe a migration in gitopsify.yaml (interactive on a terminal)",
		Long: `Writes gitopsify.yaml. On a terminal it asks for the source cluster, the
namespaces, the GitOps tool and pattern, the environments and the Secret
strategy; otherwise (or with --no-input) it uses the flags. Every other command
reads gitopsify.yaml from the current directory.

The file is a Kubernetes-style resource (apiVersion, kind, metadata, spec).
--file picks the syntax by extension: .json for JSON, .hcl for HCL, YAML
otherwise.`,
		Example: "  gitopsify init\n  gitopsify init --no-input --context test --tool flux --env uat=uat --repo-url git@github.com:acme/gitops.git",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			u := o.ui()
			if fileExists(file) && !o.force {
				return fmt.Errorf("%s exists; use --force to overwrite it", file)
			}
			cfg := &config.Config{}
			if err := o.apply(cmd.Flags(), cfg); err != nil {
				return err
			}
			if u.Interactive {
				if err := wizard(cmd.Context(), u, o, cfg); err != nil {
					return err
				}
			}
			cfg.Default()
			if err := cfg.Validate(); err != nil {
				return err
			}
			b, err := config.Encode(file, cfg.Document(), config.SchemaHeader, "# gitopsify migration. Reference: https://github.com/hayone1/gitopsify/blob/main/docs/configuration.md\n")
			if err != nil {
				return err
			}
			if err := os.WriteFile(file, b, 0o644); err != nil {
				return err
			}
			u.Success("wrote %s", file)
			u.Heading("Next steps")
			u.Println("  gitopsify scan        # see what would be migrated")
			u.Println("  gitopsify generate    # write the repository")
			return nil
		},
	}
	o.addScope(cmd.Flags())
	o.addSecrets(cmd.Flags())
	o.addOutput(cmd.Flags())
	cmd.Flags().StringVarP(&file, "file", "f", DefaultConfigFile, "file to write")
	return cmd
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// wizard fills the configuration interactively.
func wizard(ctx context.Context, u *ui.UI, o *options, cfg *config.Config) error {
	raw, err := kube.LoadingRules(cfg.Source.Kubeconfig).Load()
	if err != nil {
		return fmt.Errorf("reading kubeconfig: %w", err)
	}
	var contexts []ui.Option
	names := make([]string, 0, len(raw.Contexts))
	for name := range raw.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, n := range names {
		label := n
		if n == raw.CurrentContext {
			label += " (current)"
		}
		contexts = append(contexts, ui.Option{Label: label, Value: n})
	}
	if cfg.Source.Context == "" {
		cfg.Source.Context = raw.CurrentContext
	}
	if len(contexts) > 0 {
		if err := u.Select("Source cluster", "The cluster that runs what you want in Git", contexts, &cfg.Source.Context); err != nil {
			return err
		}
	}

	// Namespaces of the source cluster, without the system ones.
	if len(cfg.Scope.Namespaces) == 0 && len(cfg.Scope.Resources) == 0 {
		if c, err := kube.Connect(cfg.Source.Access, o.connectOptions()); err == nil {
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			list, err := c.Kubernetes.CoreV1().Namespaces().List(cctx, metav1.ListOptions{})
			cancel()
			if err == nil {
				var opts []ui.Option
				var all []string
				for _, ns := range list.Items {
					if match.Glob(filter.SystemNamespaces, ns.Name) || match.Glob(filter.GitOpsNamespaces, ns.Name) {
						continue
					}
					opts = append(opts, ui.Option{Label: ns.Name, Value: ns.Name})
					all = append(all, ns.Name)
				}
				selected := append([]string(nil), all...)
				if len(opts) > 0 {
					if err := u.MultiSelect("Namespaces to migrate", "Everything selected = the whole cluster, including cluster-scoped objects", opts, &selected); err != nil {
						return err
					}
					if len(selected) < len(all) {
						cfg.Scope.Namespaces = selected
					}
				}
			} else {
				u.Warn("couldn't list namespaces: %v", err)
			}
		} else {
			u.Warn("couldn't connect to %s: %v", cfg.Source.Context, err)
		}
	}

	if cfg.Output.Tool == "" {
		cfg.Output.Tool = config.ToolArgoCD
	}
	if err := u.Select("GitOps tool", "How far gitopsify takes you", []ui.Option{
		{Label: "Argo CD — Applications, AppProject, Argo CD bootstrap", Value: config.ToolArgoCD},
		{Label: "Flux — Kustomizations, HelmReleases, Flux bootstrap", Value: config.ToolFlux},
		{Label: "None — clean manifests and Helm values only; wire them up yourself", Value: config.ToolNone},
	}, &cfg.Output.Tool); err != nil {
		return err
	}
	if cfg.Output.Tool == config.ToolArgoCD {
		if cfg.Output.ArgoCD.Pattern == "" {
			cfg.Output.ArgoCD.Pattern = config.PatternAppOfApps
		}
		if err := u.Select("Argo CD pattern", "", []ui.Option{
			{Label: "App of apps — one Application per app, ordered with sync waves", Value: config.PatternAppOfApps},
			{Label: "ApplicationSet — one ApplicationSet per environment, Git files generator", Value: config.PatternApplicationSet},
			{Label: "Single Application — everything in one app, for small clusters", Value: config.PatternSingle},
		}, &cfg.Output.ArgoCD.Pattern); err != nil {
			return err
		}
	}

	envs := "uat"
	if len(cfg.Environments) > 0 {
		var names []string
		for _, e := range cfg.Environments {
			names = append(names, e.Name)
		}
		envs = strings.Join(names, ",")
	}
	if err := u.Input("Target environments", "Comma-separated, e.g. uat,prod", "uat", &envs, func(s string) error {
		for _, n := range strings.Split(s, ",") {
			if !dnsLabel.MatchString(strings.TrimSpace(n)) {
				return fmt.Errorf("%q is not a valid name (lowercase letters, digits, dashes)", n)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	// Start from what --env gave (name=context and the rest), so the
	// questions offer it rather than dropping it.
	given := map[string]config.Environment{}
	for _, e := range cfg.Environments {
		given[e.Name] = e
	}
	cfg.Environments = nil
	for _, n := range strings.Split(envs, ",") {
		n = strings.TrimSpace(n)
		e := config.Environment{Name: n}
		if g, ok := given[n]; ok {
			e = g
		}
		ctxName := e.Cluster.Context
		if len(contexts) > 0 {
			opts := append([]ui.Option{{Label: "(none: set it later)", Value: ""}}, contexts...)
			if err := u.Select("Cluster of "+n, "Used by check, diff, adopt and bootstrap", opts, &ctxName); err != nil {
				return err
			}
		}
		e.Cluster.Context = ctxName
		cfg.Environments = append(cfg.Environments, e)
	}
	if len(cfg.Environments) > 1 {
		if cfg.Output.Layout == "" {
			cfg.Output.Layout = config.LayoutFolderPerEnv
		}
		if err := u.Select("Environment layout", "", []ui.Option{
			{Label: "Folder per environment — one repository, base + overlays", Value: config.LayoutFolderPerEnv},
			{Label: "Repository per environment — one tree per environment", Value: config.LayoutRepoPerEnv},
		}, &cfg.Output.Layout); err != nil {
			return err
		}
	}
	if cfg.Output.Tool != config.ToolNone {
		if err := u.Input("Git repository URL", "Where the output will be pushed; the generated objects point at it", "https://github.com/acme/gitops.git", &cfg.Output.Repo.URL, func(s string) error {
			if strings.TrimSpace(s) == "" {
				return fmt.Errorf("required for %s", cfg.Output.Tool)
			}
			return nil
		}); err != nil {
			return err
		}
	}

	if cfg.Secrets.Strategy == "" {
		cfg.Secrets.Strategy = config.SecretsSkip
	}
	if err := u.Select("Secrets", "Secret data never goes into Git in plain text unless you say so", []ui.Option{
		{Label: "Skip — list them in the report; create them yourself", Value: config.SecretsSkip},
		{Label: "Local — write them to a git-ignored .secrets/ folder", Value: config.SecretsLocal},
		{Label: "Sealed Secrets — seal with the target controller's key", Value: config.SecretsSealedSecrets},
		{Label: "SOPS — encrypt with the sops binary (age, PGP, KMS)", Value: config.SecretsSOPS},
		{Label: "External Secrets — ExternalSecret per Secret", Value: config.SecretsExternalSecrets},
	}, &cfg.Secrets.Strategy); err != nil {
		return err
	}
	if cfg.Secrets.Strategy == config.SecretsSOPS && len(cfg.Secrets.SOPS.Age) == 0 {
		var age string
		if err := u.Input("age recipient", "Public key (age1...) the Secrets are encrypted for", "age1...", &age, nil); err != nil {
			return err
		}
		if age != "" {
			cfg.Secrets.SOPS.Age = []string{age}
		}
	}
	adopt, err := u.Confirm("Does the target already run these objects?", "Yes = takeover mode: no pruning, protected PVCs/Namespaces/CRDs, field-manager-safe sync", cfg.Adopt.Enabled)
	if err != nil {
		return err
	}
	cfg.Adopt.Enabled = adopt
	return nil
}
