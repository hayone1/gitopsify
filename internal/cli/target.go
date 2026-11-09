package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hayone1/gitopsify/internal/ui"
	"github.com/hayone1/gitopsify/pkg/bootstrap"
	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/filter"
	"github.com/hayone1/gitopsify/pkg/kube"
	"github.com/hayone1/gitopsify/pkg/pipeline"
	"github.com/hayone1/gitopsify/pkg/target"
)

func filterRules() [][2]string {
	var out [][2]string
	for _, r := range filter.DefaultRules() {
		out = append(out, [2]string{r.Name, r.Description})
	}
	return out
}

// targetClient connects to an environment's cluster and checks it answers.
func targetClient(o *options, cfg *config.Config, env *config.Environment) (*kube.Client, error) {
	c, err := pipeline.EnvClient(cfg, env.Name, o.connectOptions())
	if errors.Is(err, pipeline.ErrNoEnvCluster) {
		return nil, fmt.Errorf("environment %s has no cluster to connect to: pass --env %s=<kube-context> (or --target-context, --target-kubeconfig, --target-server), or set environments[].cluster in gitopsify.yaml", env.Name, env.Name)
	}
	if err != nil {
		return nil, err
	}
	if _, err := c.Ping(); err != nil {
		return nil, fmt.Errorf("connecting to environment %s (%s): %w", env.Name, c.Context, err)
	}
	return c, nil
}

func newCheck(o *options) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Preflight the target cluster: APIs, classes, existing owners, Helm records",
		Long: `Checks an environment's cluster against what would be deployed to it:
  - every kind and API version is served (or its CRD is migrated)
  - referenced StorageClasses and IngressClasses exist
  - objects that already exist, and which tool owns them now
  - Helm release records that would go stale
  - controllers needed by SealedSecrets/ExternalSecrets
Exits with status 1 when a check fails.`,
		Example: "  gitopsify check --env uat\n  gitopsify check --env uat=kind-uat --format json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := o.load(cmd)
			if err != nil {
				return err
			}
			env, err := o.envName(cmd, cfg)
			if err != nil {
				return err
			}
			c, err := targetClient(o, cfg, env)
			if err != nil {
				return err
			}
			u := o.ui()
			popts, err := o.pipelineOptions(cfg, u.Step)
			if err != nil {
				return err
			}
			res, err := pipeline.Generate(cmd.Context(), popts)
			if err != nil {
				return err
			}
			u.Step("checking %s (%s)", c.Context, c.Server)
			findings, err := target.Check(cmd.Context(), res.Inventory, cfg, env, c)
			if err != nil {
				return err
			}
			if format != "table" {
				if err := printReport(u, findings, format); err != nil {
					return err
				}
			} else {
				var rows [][]string
				for _, f := range findings {
					rows = append(rows, []string{u.Colorize(string(f.Severity), string(f.Severity)), f.Check, f.Subject, f.Message})
				}
				u.Table([]string{"Severity", "Check", "Subject", "Message"}, rows)
			}
			errs := 0
			for _, f := range findings {
				if f.Severity == target.SeverityError {
					errs++
				}
			}
			if errs > 0 {
				return &exitError{code: 1, msg: fmt.Sprintf("%d checks failed", errs)}
			}
			u.Success("no blocking problems in %s", env.Name)
			return nil
		},
	}
	o.addScope(cmd.Flags())
	o.addSecrets(cmd.Flags())
	o.addEnv(cmd.Flags())
	o.addTarget(cmd.Flags())
	cmd.Flags().StringVar(&format, "format", "table", "output format: table, json or yaml")
	complete(cmd.Flags(), "format", "table", "json", "yaml")
	return cmd
}

func newDiff(o *options) *cobra.Command {
	var exitCode, showUnchanged, noHelmTemplate bool
	var format string
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Show what the first sync would change in the target cluster",
		Long: `Server-side dry-runs every generated object against an environment's cluster
(as a new field manager, like Argo CD and Flux) and compares the result with the
live object. Use it before a takeover: a clean diff means the first sync is a
no-op. Helm apps are rendered with helm template when helm is installed.`,
		Example: "  gitopsify diff --env prod\n  gitopsify diff --env prod --exit-code   # fail CI on changes",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := o.load(cmd)
			if err != nil {
				return err
			}
			env, err := o.envName(cmd, cfg)
			if err != nil {
				return err
			}
			c, err := targetClient(o, cfg, env)
			if err != nil {
				return err
			}
			u := o.ui()
			popts, err := o.pipelineOptions(cfg, u.Step)
			if err != nil {
				return err
			}
			res, err := pipeline.Generate(cmd.Context(), popts)
			if err != nil {
				return err
			}
			u.Step("dry-running against %s (%s)", c.Context, c.Server)
			changes, notes, err := target.Diff(cmd.Context(), res.Inventory, cfg, env, c, target.DiffOptions{HelmTemplate: !noHelmTemplate})
			if err != nil {
				return err
			}
			for _, n := range notes {
				u.Warn("%s", n)
			}
			if format != "table" {
				if err := printReport(u, changes, format); err != nil {
					return err
				}
			}
			counts := map[string]int{}
			for _, ch := range changes {
				counts[ch.Action]++
				if format != "table" {
					continue
				}
				switch ch.Action {
				case target.ActionUpdate:
					u.Printf("%s %s %s\n", u.Colorize("warning", "~"), ch.Object, u.Dim("("+ch.App+")"))
					for _, line := range strings.Split(strings.TrimRight(ch.Diff, "\n"), "\n") {
						switch {
						case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
							line = u.Colorize("ok", line)
						case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
							line = u.Colorize("error", line)
						}
						u.Printf("    %s\n", line)
					}
				case target.ActionCreate:
					msg := ""
					if ch.Message != "" {
						msg = " " + u.Dim("("+ch.Message+")")
					}
					u.Printf("%s %s %s%s\n", u.Colorize("ok", "+"), ch.Object, u.Dim("("+ch.App+")"), msg)
				case target.ActionError:
					u.Printf("%s %s: %s\n", u.Colorize("error", "✗"), ch.Object, ch.Message)
				case target.ActionUnchanged:
					if showUnchanged {
						u.Printf("  %s %s\n", ch.Object, u.Dim("unchanged"))
					}
				}
			}
			summary := fmt.Sprintf("%d to create, %d to change, %d unchanged, %d errors", counts[target.ActionCreate], counts[target.ActionUpdate], counts[target.ActionUnchanged], counts[target.ActionError])
			if format == "table" {
				u.Heading(summary)
			} else {
				u.Step("%s", summary)
			}
			if counts[target.ActionError] > 0 {
				return &exitError{code: 1}
			}
			if exitCode && counts[target.ActionCreate]+counts[target.ActionUpdate] > 0 {
				return &exitError{code: 2}
			}
			return nil
		},
	}
	o.addScope(cmd.Flags())
	o.addSecrets(cmd.Flags())
	o.addEnv(cmd.Flags())
	o.addTarget(cmd.Flags())
	cmd.Flags().BoolVar(&exitCode, "exit-code", false, "exit with status 2 when the sync would change anything")
	cmd.Flags().BoolVar(&showUnchanged, "show-unchanged", false, "also list unchanged objects")
	cmd.Flags().BoolVar(&noHelmTemplate, "no-helm-template", false, "compare Helm apps using their recorded release manifests")
	cmd.Flags().StringVar(&format, "format", "table", "output format: table, json or yaml")
	return cmd
}

func newAdopt(o *options) *cobra.Command {
	var helmRecords, backupDir string
	var dryRun, yes bool
	cmd := &cobra.Command{
		Use:   "adopt",
		Short: "Prepare the target for takeover: back up and remove stale Helm release records",
		Long: `When Argo CD (which renders charts itself) or plain manifests take over objects
installed with Helm, the release records left in the cluster go stale and a
later "helm upgrade" would fight the GitOps tool. adopt backs them up and, with
--helm-records backup-delete, removes them. Flux HelmReleases reuse the records,
so they are kept.

Pair it with "generate --adopt", which writes takeover-safe sync settings.`,
		Example: "  gitopsify adopt -o gitops --env prod --helm-records backup-delete",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := o.loadForBootstrap(cmd)
			if err != nil {
				return err
			}
			env, err := o.envName(cmd, cfg)
			if err != nil {
				return err
			}
			switch helmRecords {
			case target.HelmRecordsKeep, target.HelmRecordsBackup, target.HelmRecordsBackupDelete:
			default:
				return fmt.Errorf("--helm-records must be keep, backup or backup-delete")
			}
			if helmRecords == target.HelmRecordsBackupDelete {
				if o.configFile == "" && !fileExists(DefaultConfigFile) {
					// Flux HelmReleases keep using the records: deleting
					// them makes Flux install every release again.
					return fmt.Errorf("--helm-records backup-delete needs to know the GitOps tool that takes over: pass -o <the generated repository> or -c gitopsify.yaml")
				}
				if backupDir == "" {
					return fmt.Errorf("--helm-records backup-delete needs a --backup-dir: the records are deleted only after they are backed up")
				}
			}
			c, err := targetClient(o, cfg, env)
			if err != nil {
				return err
			}
			u := o.ui()
			if helmRecords == target.HelmRecordsBackupDelete && !dryRun && !yes {
				ok, err := u.Confirm(fmt.Sprintf("Delete stale Helm release records in %s?", c.Context), "They are backed up to "+backupDir+" first.", false)
				if err != nil {
					return err
				}
				if !ok {
					return &exitError{code: 1, msg: "not confirmed (pass --yes when running without a terminal)"}
				}
			}
			popts, err := o.pipelineOptions(cfg, u.Step)
			if err != nil {
				return err
			}
			res, err := pipeline.Scan(cmd.Context(), popts)
			if err != nil {
				return err
			}
			out, err := target.Adopt(cmd.Context(), res.Inventory, cfg, env, c, target.AdoptOptions{
				HelmRecords: helmRecords, BackupDir: backupDir, DryRun: dryRun, Log: u.Step,
			})
			if err != nil {
				return err
			}
			prefix := ""
			if dryRun {
				prefix = "(dry run) "
			}
			u.Success("%sbacked up %d, deleted %d Helm release records; kept the records of %d releases", prefix, len(out.BackedUp), len(out.Deleted), len(out.Kept))
			return nil
		},
	}
	o.addScope(cmd.Flags())
	o.addEnv(cmd.Flags())
	o.addTarget(cmd.Flags())
	cmd.Flags().StringVar(&helmRecords, "helm-records", target.HelmRecordsBackup, "keep, backup or backup-delete")
	cmd.Flags().StringVar(&backupDir, "backup-dir", filepath.Join(".gitopsify-backup", "helm-records"), "where backups are written")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would happen")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "directory of the generated repository, whose gitopsify.yaml names the GitOps tool")
	return cmd
}

type bootstrapFlags struct {
	gitUser     string
	gitToken    string
	sshKey      string
	knownHosts  string
	sopsAgeKey  string
	timeout     time.Duration
	yes         bool
	wait        bool
	waitTimeout time.Duration
	waitFor     string
}

func (b *bootstrapFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&b.gitUser, "git-username", "", "username for an HTTPS repository (default git)")
	cmd.Flags().StringVar(&b.gitToken, "git-token", "", "password or token for an HTTPS repository (or GITOPSIFY_GIT_TOKEN)")
	cmd.Flags().StringVar(&b.sshKey, "ssh-key", "", "private key file for an SSH repository")
	cmd.Flags().StringVar(&b.knownHosts, "known-hosts", "", "known_hosts file of the Git server, e.g. from ssh-keyscan (needed by Flux over SSH, and by Argo CD for hosts other than GitHub, GitLab, Bitbucket and Azure DevOps)")
	cmd.Flags().StringVar(&b.sopsAgeKey, "sops-age-key", "", "age key file loaded as Flux's SOPS decryption key")
	cmd.Flags().DurationVar(&b.timeout, "timeout", 15*time.Minute, "how long to wait for Argo CD or Flux to start (image pulls are the slow part)")
	cmd.Flags().BoolVarP(&b.yes, "yes", "y", false, "don't ask for confirmation")
	cmd.Flags().BoolVar(&b.wait, "wait", false, "after bootstrapping, wait until every app is synced and healthy")
	cmd.Flags().DurationVar(&b.waitTimeout, "wait-timeout", 20*time.Minute, "how long --wait waits")
	cmd.Flags().StringVar(&b.waitFor, "wait-for", "healthy", "what --wait waits for: healthy, or synced (applied, health aside)")
	inGroup(cmd.Flags(), groupBootstrap, "git-username", "git-token", "ssh-key", "known-hosts", "sops-age-key", "timeout", "yes", "wait", "wait-timeout", "wait-for")
	complete(cmd.Flags(), "wait-for", "healthy", "synced")
}

func runBootstrap(cmd *cobra.Command, o *options, u *ui.UI, cfg *config.Config, env *config.Environment, b bootstrapFlags) error {
	c, err := targetClient(o, cfg, env)
	if err != nil {
		return err
	}
	if cfg.Output.Tool == config.ToolNone {
		return fmt.Errorf("--tool none has nothing to bootstrap; apply %s", filepath.Join(envDir(cfg, env.Name), "apply-"+env.Name+".sh"))
	}
	version := cfg.Output.ArgoCD.Version
	if cfg.Output.Tool == config.ToolFlux {
		version = cfg.Output.Flux.Version
	}
	if !b.yes {
		ok, err := u.Confirm(fmt.Sprintf("Install %s %s into %s and sync %s?", cfg.Output.Tool, version, c.Context, repoURLOf(cfg, env)), "Context "+c.Context+" at "+c.Server, true)
		if err != nil {
			return err
		}
		if !ok || !u.Interactive {
			return &exitError{code: 1, msg: "not confirmed (pass --yes when running without a terminal)"}
		}
	}
	token := b.gitToken
	if token == "" {
		token = envOr("GITOPSIFY_GIT_TOKEN", "")
	}
	err = bootstrap.Run(cmd.Context(), bootstrap.Options{
		Config: cfg, Env: env, Client: c, Dir: envDir(cfg, env.Name),
		Git:            bootstrap.GitCredentials{Username: b.gitUser, Password: token, SSHKeyFile: b.sshKey, KnownHostsFile: b.knownHosts},
		SOPSAgeKeyFile: b.sopsAgeKey,
		Timeout:        b.timeout,
		Log:            u.Step,
	})
	if err != nil {
		return err
	}
	u.Success("%s is syncing %s into %s", cfg.Output.Tool, repoURLOf(cfg, env), c.Context)
	if !b.wait {
		u.Step("follow it with: gitopsify status --env %s", env.Name)
		return nil
	}
	st, err := waitSynced(cmd.Context(), u, cfg, env, c, envDir(cfg, env.Name), b.waitTimeout, b.waitFor)
	if err != nil {
		printStatus(u, st)
		return &exitError{code: 1, msg: err.Error()}
	}
	u.Success("%d objects %s in %s", len(st), map[string]string{"synced": "synced", "healthy": "synced and healthy"}[b.waitFor], env.Name)
	return nil
}

// repoURLOf returns the repository an environment syncs from.
func repoURLOf(cfg *config.Config, env *config.Environment) string {
	if cfg.Output.Layout == config.LayoutRepoPerEnv && env.RepoURL != "" {
		return env.RepoURL
	}
	return cfg.Output.Repo.URL
}

func newBootstrap(o *options) *cobra.Command {
	var b bootstrapFlags
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Install Argo CD or Flux in the target and point it at the generated repository",
		Long: `Installs the pinned Argo CD or Flux release in an environment's cluster, applies
the settings the generated apps rely on, registers the repository credentials and
applies the root object (argocd/<env>/root.yaml or clusters/<env>/flux-system).
Push the generated repository first. Doesn't need the source cluster.`,
		Example: `  gitopsify bootstrap --env uat --git-token "$GITHUB_TOKEN"
  gitopsify bootstrap --env uat --ssh-key ~/.ssh/deploy --known-hosts known_hosts`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := o.loadForBootstrap(cmd)
			if err != nil {
				return err
			}
			env, err := o.envName(cmd, cfg)
			if err != nil {
				return err
			}
			return runBootstrap(cmd, o, o.ui(), cfg, env, b)
		},
	}
	o.addEnv(cmd.Flags())
	o.addTarget(cmd.Flags())
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "directory of the generated repository (default from the configuration)")
	b.add(cmd)
	return cmd
}

// loadForBootstrap reads the configuration, falling back to the copy
// generate wrote into the output directory.
func (o *options) loadForBootstrap(cmd *cobra.Command) (*config.Config, error) {
	if o.configFile == "" && o.output != "" {
		if p := filepath.Join(o.output, DefaultConfigFile); fileExists(p) {
			o.configFile = p
		}
	}
	cfg, err := o.load(cmd)
	if err != nil {
		return nil, err
	}
	if o.output != "" {
		cfg.Output.Dir = o.output
	}
	return cfg, nil
}
