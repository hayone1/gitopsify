package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/target"
)

func newMigrate(o *options) *cobra.Command {
	var g gitFlags
	var b bootstrapFlags
	var skipCheck, skipBootstrap bool
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Generate, push, check and bootstrap in one go",
		Long: `Runs the whole migration for one environment:
  1. scan the source cluster and write the repository (generate)
  2. commit, and push with --push
  3. preflight the target cluster (check); stops on failures
  4. install Argo CD or Flux and apply the root object (bootstrap)
It asks before changing the target unless --yes is given.`,
		Example: `  gitopsify migrate --context test --env uat=uat --tool argocd \
    --repo-url https://github.com/acme/gitops.git --push --git-token "$TOKEN"`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := o.load(cmd)
			if err != nil {
				return err
			}
			if len(cfg.Environments) != 1 {
				return fmt.Errorf("migrate works on one environment at a time; pass a single --env")
			}
			env := &cfg.Environments[0]
			u := o.ui()

			res, err := scanAndFinish(cmd.Context(), o, u, cfg)
			if err != nil {
				return err
			}
			if err := writeOutput(u, cfg, res, o.force); err != nil {
				return err
			}
			if g.commit || g.push || cfg.Output.Tool != config.ToolNone {
				g.commit = true
				if err := publish(u, cfg, g); err != nil {
					return err
				}
			}
			if cfg.Output.Tool == config.ToolNone {
				nextSteps(u, cfg, g.push)
				return nil
			}
			if !skipCheck {
				c, err := targetClient(cfg, env)
				if err != nil {
					return err
				}
				u.Step("checking %s", c.Context)
				findings, err := target.Check(cmd.Context(), res.Inventory, cfg, env, c)
				if err != nil {
					return err
				}
				errs := 0
				for _, f := range findings {
					switch f.Severity {
					case target.SeverityError:
						errs++
						u.Errorf("%s %s: %s", f.Check, f.Subject, f.Message)
					case target.SeverityWarning:
						u.Warn("%s %s: %s", f.Check, f.Subject, f.Message)
					}
				}
				if errs > 0 {
					return &exitError{code: 1, msg: fmt.Sprintf("%d preflight checks failed; fix them or rerun with --skip-check", errs)}
				}
			}
			if skipBootstrap {
				nextSteps(u, cfg, g.push)
				return nil
			}
			if !g.push {
				u.Warn("the repository was not pushed (--push); %s must already have this content for the sync to work", cfg.Output.Repo.URL)
			}
			return runBootstrap(cmd, u, cfg, env, b)
		},
	}
	o.addScope(cmd.Flags())
	o.addSecrets(cmd.Flags())
	o.addOutput(cmd.Flags())
	g.add(cmd)
	b.add(cmd)
	cmd.Flags().BoolVarP(&o.interactive, "interactive", "i", false, "choose the apps to migrate interactively")
	cmd.Flags().BoolVar(&skipCheck, "skip-check", false, "don't preflight the target")
	cmd.Flags().BoolVar(&skipBootstrap, "skip-bootstrap", false, "stop after generating and pushing")
	return cmd
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
