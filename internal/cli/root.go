// Package cli implements the gitopsify command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/hayone1/gitopsify/internal/ui"
)

// Version is set at build time with -ldflags "-X .../internal/cli.Version=...".
var Version = "dev"

// exitError ends the process with a code, after its message (if any) was
// printed.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// Execute runs the command line and returns the process exit code.
func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	o := &options{}
	root := newRoot(o)
	if err := root.ExecuteContext(ctx); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			if ee.msg != "" {
				o.ui().Errorf("%s", ee.msg)
			}
			return ee.code
		}
		if errors.Is(err, ui.ErrAborted) {
			o.ui().Warn("aborted")
			return 130
		}
		o.ui().Errorf("%v", err)
		return 1
	}
	return 0
}

func (o *options) ui() *ui.UI { return ui.New(o.noInput, o.noColor, o.quiet) }

func newRoot(o *options) *cobra.Command {
	root := &cobra.Command{
		Use:   "gitopsify",
		Short: "Turn what runs in a Kubernetes cluster into a GitOps repository",
		Long: `gitopsify reads the objects and Helm releases of a running cluster and writes a
GitOps repository that rebuilds them in another cluster, or lets Argo CD or Flux
take over the same cluster.

It strips runtime fields and defaults, leaves out system and controller-generated
objects (and says why), keeps Helm releases as charts, orders CRDs, operators and
workloads, keeps Secrets out of Git in plain text, and can install Argo CD or Flux
and point it at the result.

Typical flow:
  gitopsify init                       # describe the migration in gitopsify.yaml
  gitopsify scan                       # see what would be migrated and what's left out
  gitopsify generate                   # write the repository
  gitopsify check --env uat            # preflight against the target cluster
  gitopsify diff --env uat             # what the first sync would change
  gitopsify bootstrap --env uat        # install Argo CD/Flux and apply the root object

or all at once: gitopsify migrate`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}
	o.addGlobal(root.PersistentFlags())
	root.AddGroup(
		&cobra.Group{ID: "migrate", Title: "Migration:"},
		&cobra.Group{ID: "target", Title: "Target cluster:"},
	)
	for _, c := range []*cobra.Command{newInit(o), newScan(o), newGenerate(o), newMigrate(o)} {
		c.GroupID = "migrate"
		root.AddCommand(c)
	}
	for _, c := range []*cobra.Command{newCheck(o), newDiff(o), newAdopt(o), newBootstrap(o), newStatus(o)} {
		c.GroupID = "target"
		root.AddCommand(c)
	}
	root.AddCommand(newRules(o), newDoctor(o), newVersion())
	root.SetUsageTemplate(usageTemplate)
	registerCompletions(root, o)
	return root
}

func newVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), "gitopsify", Version)
		},
	}
}
