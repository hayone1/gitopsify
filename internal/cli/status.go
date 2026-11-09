package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/hayone1/gitopsify/internal/ui"
	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/kube"
	"github.com/hayone1/gitopsify/pkg/target"
)

func newStatus(o *options) *cobra.Command {
	var format, waitFor string
	var wait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether Argo CD or Flux has synced every app of an environment",
		Long: `Reads the Argo CD Applications, or the Flux Kustomizations and HelmReleases, that
gitopsify generated for an environment and reports their sync and health. Objects
the generated repository declares but the cluster doesn't have yet are reported as
Missing. Doesn't need the source cluster.

Exits with status 1 when something isn't ready (or, with --wait, isn't ready in
time).`,
		Example: `  gitopsify status --env uat
  gitopsify status --env uat --wait --timeout 15m   # in CI, after bootstrap`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(format); err != nil {
				return err
			}
			cfg, err := o.loadForBootstrap(cmd)
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
			dir := envDir(cfg, env.Name)
			var st []target.AppStatus
			if wait {
				st, err = waitSynced(cmd.Context(), u, cfg, env, c, dir, timeout, waitFor)
			} else {
				st, err = target.Status(cmd.Context(), cfg, env, c, dir)
			}
			if st == nil && err != nil {
				return err
			}
			if format != "table" {
				if perr := printReport(u, st, format); perr != nil {
					return perr
				}
			} else {
				printStatus(u, st)
			}
			if err != nil {
				return &exitError{code: 1, msg: err.Error()}
			}
			if !target.AllReady(st) || len(st) == 0 {
				return &exitError{code: 1, msg: fmt.Sprintf("%d of %d not ready in %s", len(st)-ready(st), len(st), env.Name)}
			}
			what := "synced and healthy"
			if wait && waitFor == "synced" {
				what = "synced"
			}
			u.Success("%d objects %s in %s", len(st), what, env.Name)
			return nil
		},
	}
	o.addEnv(cmd.Flags())
	o.addTarget(cmd.Flags())
	cmd.Flags().StringVarP(&o.output, "output", "o", "", "directory of the generated repository (default from the configuration)")
	cmd.Flags().StringVar(&format, "format", "table", "output format: table, json or yaml")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until everything is synced and healthy")
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Minute, "how long --wait waits")
	cmd.Flags().StringVar(&waitFor, "wait-for", "healthy", "what --wait waits for: healthy, or synced (Argo CD applied everything, health aside)")
	complete(cmd.Flags(), "format", "table", "json", "yaml")
	complete(cmd.Flags(), "wait-for", "healthy", "synced")
	return cmd
}

func ready(st []target.AppStatus) int {
	n := 0
	for _, s := range st {
		if s.Ready {
			n++
		}
	}
	return n
}

func printStatus(u *ui.UI, st []target.AppStatus) {
	var rows [][]string
	for _, s := range st {
		sev := "warning"
		switch {
		case s.Ready:
			sev = "ok"
		case s.Health == "Degraded" || s.Health == "NotReady" || s.Health == "Missing":
			sev = "error"
		}
		rows = append(rows, []string{s.Kind, s.Name, orDash(s.Sync), u.Colorize(sev, s.Health), s.Message})
	}
	u.Table([]string{"Kind", "Name", "Sync", "Health", "Message"}, rows)
}

// waitSynced waits for an environment's GitOps objects, reporting progress
// whenever the number of ready objects changes.
func waitSynced(ctx context.Context, u *ui.UI, cfg *config.Config, env *config.Environment, c *kube.Client, dir string, timeout time.Duration, waitFor string) ([]target.AppStatus, error) {
	switch waitFor {
	case "healthy", "synced":
	default:
		return nil, fmt.Errorf("--wait-for must be healthy or synced, not %q", waitFor)
	}
	last := -1
	u.Step("waiting up to %s for %s to sync %s (%s)", timeout, cfg.Output.Tool, env.Name, waitFor)
	return target.Wait(ctx, cfg, env, c, dir, target.WaitOptions{
		Timeout:    timeout,
		SyncedOnly: waitFor == "synced",
		Progress: func(st []target.AppStatus) {
			if n := ready(st); n != last {
				last = n
				u.Step("%d of %d ready", n, len(st))
			}
		},
	})
}
