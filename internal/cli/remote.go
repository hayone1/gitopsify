package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/hayone1/gitopsify/internal/remote"
	"github.com/hayone1/gitopsify/internal/server"
	"github.com/hayone1/gitopsify/internal/ui"
	"github.com/hayone1/gitopsify/pkg/config"
)

// checkFormat validates --format before anything happens on the server;
// text is accepted for table.
func checkRemoteFormat(format *string) error {
	switch *format {
	case "", "table", "text":
		*format = "table"
	case "json", "yaml":
	default:
		return fmt.Errorf("--format must be table, json or yaml, not %q", *format)
	}
	return nil
}

// noClusterFlags refuses the source-cluster flags on commands that talk to
// a gitopsify server: the server reaches clusters with its own settings.
func noLocalClusterFlags(cmd *cobra.Command) error {
	var used []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Changed && slices.Contains(f.Annotations[groupAnnotation], groupSource) {
			used = append(used, "--"+f.Name)
		}
	})
	if len(used) > 0 {
		return fmt.Errorf("%s: the server reaches clusters with its own settings; name the cluster with --cluster (run start), and the server's certificate with gitopsify login --remote-ca", strings.Join(used, ", "))
	}
	return nil
}

// remoteFlags choose the gitopsify server a command talks to.
type remoteFlags struct{ server string }

func (r *remoteFlags) add(fs *pflag.FlagSet) {
	fs.StringVar(&r.server, "remote", "", "gitopsify server: a name from gitopsify login, or an address (default: the current one, or $"+remote.EnvServer+")")
}

func (r *remoteFlags) client() (*remote.Client, error) {
	s, err := remote.Resolve(r.server)
	if err != nil {
		return nil, err
	}
	return remote.New(s, Version)
}

func newLogin(o *options) *cobra.Command {
	var tokenStdin, noBrowser, renew, insecure bool
	var name, caFile string
	cmd := &cobra.Command{
		Use:   "login [address | name]",
		Short: "Sign in to a gitopsify server",
		Long: `Signs in to a gitopsify server (gitopsify server, or its Helm chart) and makes it
the one the run commands talk to.

With single sign-on it opens your browser. Where the browser can't reach this
machine (SSH sessions, containers), --no-browser prints the address and asks for
the code the server's page shows. --token-stdin signs in with an API token from
gitopsify server token instead.

Sign-ins are saved, readable by you only, in servers.yaml in your configuration
directory ($` + remote.EnvFile + `). Logging in to a saved server whose sign-in
is still valid makes it the current one again; --renew signs in afresh.

In CI and CronJobs, skip login: set ` + remote.EnvServer + ` to the address and
` + remote.EnvToken + ` to an API token.`,
		Example: `  gitopsify login gitopsify.example.com
  gitopsify login gitopsify.example.com --no-browser       # over SSH
  gitopsify server token --name me --role operator          # (an admin) then:
  echo "$TOKEN" | gitopsify login gitopsify.example.com --token-stdin
  gitopsify login staging                                    # switch to a saved server`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := noLocalClusterFlags(cmd); err != nil {
				return err
			}
			u := o.ui()
			f, err := remote.Load()
			if err != nil {
				return err
			}
			want := f.Current
			if len(args) == 1 {
				want = args[0]
			}
			if want == "" {
				return errors.New("name the server: gitopsify login <address>")
			}
			var s remote.Server
			if saved := f.Find(want); saved != nil {
				s = *saved
				if name != "" && name != s.Name {
					f.Remove(s.Name) // renamed
				}
			} else {
				addr, err := remote.NormalizeURL(want)
				if err != nil {
					return err
				}
				s = remote.Server{Name: remote.NameOf(addr), URL: addr}
			}
			if name != "" {
				s.Name = name
			}
			if caFile != "" {
				abs, err := filepath.Abs(config.ExpandHome(caFile))
				if err != nil {
					return err
				}
				s.CAFile = abs
			}
			if strings.HasPrefix(s.URL, "http://") {
				u.Warn("%s is plain HTTP: your sign-in travels unencrypted", s.URL)
			}
			if cmd.Flags().Changed("remote-insecure-skip-tls-verify") {
				s.Insecure = insecure
			}
			c, err := remote.New(&s, Version)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			// A saved, valid sign-in: switch to it.
			if s.Token != "" && !s.Expired() && !renew && !tokenStdin {
				if me, err := c.Me(ctx); err == nil {
					f.Put(s)
					if err := f.Save(); err != nil {
						return err
					}
					u.Success("signed in to %s as %s%s; it's the current server (--renew signs in afresh)", s.Name, me.User.Username, until(s.Expires))
					return nil
				}
			}

			info, err := c.Info(ctx)
			if err != nil {
				return err
			}
			switch {
			case tokenStdin:
				if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
					fmt.Fprint(os.Stderr, "Paste the API token: ")
				}
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && line == "" {
					return errors.New("--token-stdin: no token on standard input")
				}
				s.Token, s.Expires = strings.TrimSpace(line), nil
			case info.Local:
				return fmt.Errorf("%s is a gitopsify ui for one person; open it in the browser instead", s.URL)
			case info.AuthMode != config.AuthOIDC:
				return fmt.Errorf("%s doesn't use single sign-on: sign in with an API token (gitopsify login %s --token-stdin)", s.URL, s.Name)
			default:
				paste := noBrowser || headless()
				if paste && o.noInput {
					return errors.New("--no-input: the sign-in needs a code pasted; use --token-stdin")
				}
				b := remote.BrowserLogin{
					Show: func(address string, paste bool, check string) {
						if paste {
							u.Step("open this address in a browser and sign in:")
						} else {
							u.Step("signing in with your browser; if it doesn't open, open:")
						}
						u.Printf("\n  %s\n\n", address)
						u.Step("the page asks you to confirm: check code %s", check)
						if !paste {
							u.Step("waiting for the browser (Ctrl-C stops)")
						}
					},
				}
				if paste {
					b.Paste = func() (string, error) {
						fmt.Fprint(os.Stderr, "Paste the code the page shows: ")
						line, err := bufio.NewReaderSize(cmd.InOrStdin(), 16<<10).ReadString('\n')
						if err != nil && line == "" {
							return "", errors.New("no code on standard input")
						}
						return line, nil
					}
				} else {
					b.Open = func(address string) error { openBrowser(address); return nil }
				}
				sess, err := c.Login(ctx, b)
				if err != nil {
					return err
				}
				s.Token, s.Expires = sess.Token, &sess.Expires
			}
			me, err := c.Me(ctx)
			if err != nil {
				return err
			}
			s.User = me.User.Username
			f.Put(s)
			if err := f.Save(); err != nil {
				return err
			}
			u.Success("signed in to %s as %s%s", s.Name, s.User, until(s.Expires))
			if me.Role == "" {
				u.Warn("you have no role on any cluster or tool yet; ask an admin to bind one")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read an API token from standard input instead of signing in with the browser")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the sign-in address and ask for the code the page shows (SSH sessions, containers)")
	cmd.Flags().BoolVar(&renew, "renew", false, "sign in again even if the saved sign-in is still valid")
	cmd.Flags().StringVar(&name, "name", "", "name to save the server under (default: its host)")
	cmd.Flags().StringVar(&caFile, "remote-ca", "", "CA file (PEM) the server's certificate is checked against")
	cmd.Flags().BoolVar(&insecure, "remote-insecure-skip-tls-verify", false, "don't verify the server's certificate (test servers only)")
	return cmd
}

// headless reports whether no browser can open here: Linux without a
// display (WSL opens the Windows one).
func headless() bool {
	if runtime.GOOS != "linux" || os.Getenv("WSL_DISTRO_NAME") != "" {
		return false
	}
	return os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == ""
}

func until(t *time.Time) string {
	if t == nil {
		return ""
	}
	return " until " + t.Local().Format("Mon 2 Jan 15:04")
}

func newLogout(o *options) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "logout [name]",
		Short: "Forget the sign-in to a gitopsify server",
		Long: `Removes a saved sign-in (by default the current one) from this machine. A
session token stays valid on the server until it expires, and an API token until
an admin removes it from the Server resource.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := noLocalClusterFlags(cmd); err != nil {
				return err
			}
			u := o.ui()
			f, err := remote.Load()
			if err != nil {
				return err
			}
			var names []string
			switch {
			case all:
				for _, s := range f.Servers {
					names = append(names, s.Name)
				}
			case len(args) == 1:
				s := f.Find(args[0])
				if s == nil {
					return fmt.Errorf("not signed in to %s", args[0])
				}
				names = []string{s.Name}
			case f.Current != "":
				names = []string{f.Current}
			default:
				return errors.New("not signed in to any server")
			}
			for _, n := range names {
				f.Remove(n)
			}
			if err := f.Save(); err != nil {
				return err
			}
			for _, n := range names {
				u.Success("signed out of %s", n)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "forget every saved sign-in")
	return cmd
}

func newWhoami(o *options) *cobra.Command {
	var rf remoteFlags
	var all bool
	var format string
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show who you are on a gitopsify server, and your roles",
		Long: `Asks the current gitopsify server (or --remote) who you are signed in as and
which roles you have on its clusters and tools. --all lists the saved sign-ins
instead, without asking any server.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := noLocalClusterFlags(cmd); err != nil {
				return err
			}
			if err := checkRemoteFormat(&format); err != nil {
				return err
			}
			u := o.ui()
			if all {
				f, err := remote.Load()
				if err != nil {
					return err
				}
				if format != "table" {
					list := make([]remote.Server, len(f.Servers))
					for i, s := range f.Servers {
						s.Token = "" // never printed
						list[i] = s
					}
					return printStructured(u, format, map[string]any{"current": f.Current, "servers": list})
				}
				if len(f.Servers) == 0 {
					u.Step("not signed in to any server: gitopsify login <address>")
					return nil
				}
				rows := [][]string{}
				for _, s := range f.Servers {
					cur := ""
					if s.Name == f.Current {
						cur = "*"
					}
					exp := "never"
					if s.Expires != nil {
						exp = s.Expires.Local().Format("2006-01-02 15:04")
						if s.Expired() {
							exp += " (expired)"
						}
					}
					rows = append(rows, []string{cur, s.Name, s.URL, s.User, exp})
				}
				u.Table([]string{"", "NAME", "ADDRESS", "USER", "EXPIRES"}, rows)
				return nil
			}
			c, err := rf.client()
			if err != nil {
				return err
			}
			me, err := c.Me(cmd.Context())
			if err != nil {
				return err
			}
			if format != "table" {
				return printStructured(u, format, me)
			}
			u.Printf("%s %s (gitopsify %s)\n", u.Bold("Server:"), c.Server.URL, me.Version)
			who := me.User.Username
			if me.User.Name != "" && me.User.Name != who {
				who += " (" + me.User.Name + ")"
			}
			u.Printf("%s %s, signed in with %s%s\n", u.Bold("User:  "), remote.Clean(who), remote.Clean(me.User.Via), until(me.Expires))
			if len(me.User.Groups) > 0 {
				u.Printf("%s %s\n", u.Bold("Groups:"), remote.Clean(strings.Join(me.User.Groups, ", ")))
			}
			if me.ServerRole != "" {
				u.Printf("%s %s on the whole server\n", u.Bold("Role:  "), me.ServerRole)
			}
			var rows [][]string
			for _, n := range sortedKeys(me.Clusters) {
				rows = append(rows, []string{"cluster", remote.Clean(n), me.Clusters[n]})
			}
			for _, n := range sortedKeys(me.Tools) {
				rows = append(rows, []string{"tool", remote.Clean(n), me.Tools[n]})
			}
			if len(rows) == 0 {
				u.Warn("you have no role on any cluster or tool; ask an admin to bind one")
				return nil
			}
			u.Println()
			u.Table([]string{"KIND", "NAME", "ROLE"}, rows)
			return nil
		},
	}
	rf.add(cmd.Flags())
	cmd.Flags().BoolVar(&all, "all", false, "list the saved sign-ins")
	cmd.Flags().StringVar(&format, "format", "table", "output format: table, json or yaml")
	complete(cmd.Flags(), "format", "table", "json", "yaml")
	return cmd
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func newRun(o *options) *cobra.Command {
	rf := &remoteFlags{}
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Start and follow migrations and exports on a gitopsify server",
		Long: `Runs migrations, scans and exports on a gitopsify server instead of this machine:
the server reaches the clusters, tools and Git repositories with its own
credentials, checks your role, and keeps the log, the files and an audit trail,
as in its web UI.

Sign in first (gitopsify login), or set ` + remote.EnvServer + ` and ` + remote.EnvToken + `.`,
		Example: `  gitopsify run start -c gitopsify.yaml --follow
  gitopsify run start -c gitopsify.yaml --publish --branch gitopsify/drift --wait
  gitopsify run list --status failed
  gitopsify run logs -f 01JC...
  gitopsify run download 01JC... -o ./out`,
	}
	rf.add(cmd.PersistentFlags())
	cmd.AddCommand(newRunStart(o, rf), newRunList(o, rf), newRunGet(o, rf), newRunLogs(o, rf), newRunWait(o, rf),
		newRunCancel(o, rf), newRunDelete(o, rf), newRunDownload(o, rf), newRunPublish(o, rf))
	return cmd
}

func newRunStart(o *options, rf *remoteFlags) *cobra.Command {
	var cluster, tool, repoURL, repoBranch, branch, message, format string
	var scan, publish, wait, follow, allowPartial bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start a migration, scan or export on the server",
		Long: `Sends a Migration (gitopsify.yaml) or an Export resource (--config) to the
server and starts it there.

Migrations: the source cluster is --cluster, or the file's spec.source.context,
by the name the server registers it under; environments name registered clusters
in cluster.context. The server reaches them with its own credentials, so the
file's other access settings (kubeconfig, tokens) are left out. --scan only
inventories the cluster.

Exports: the tool is --tool, a tool registered on the server (default: the
file's only instance); --repo-url is where the output can be pushed.

--publish pushes the output when the run succeeds; with --branch, to that
branch instead of the one Argo CD or Flux follows, for a pull request: nothing
is pushed when the output doesn't differ, and a branch holding only gitopsify's
commits is replaced. --wait waits for the end and exits 1 when the run or the
push failed; --follow also prints the log.`,
		Example: `  gitopsify run start --follow
  gitopsify run start -c uat.yaml --cluster test --publish --wait
  # nightly, for a pull request (see docs/scheduled-sync.md)
  gitopsify run start --publish --branch gitopsify/drift --wait -q
  gitopsify run start -c gitopsify-grafana.yaml --tool grafana-prod --repo-url https://github.com/acme/grafana.git`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := noLocalClusterFlags(cmd); err != nil {
				return err
			}
			if err := checkRemoteFormat(&format); err != nil {
				return err
			}
			u := o.ui()
			if branch != "" && !publish {
				return errors.New("--branch needs --publish")
			}
			req, err := runRequestFrom(o.configFile, cluster, tool, scan)
			if err != nil {
				return err
			}
			if req.Type == server.RunExport {
				if repoURL != "" {
					req.Repo = &server.RunRepo{URL: repoURL, Branch: repoBranch}
				}
				req.AllowPartial = allowPartial
			} else if repoURL != "" || allowPartial {
				return errors.New("--repo-url and --allow-partial are for exports; a migration names its repository in spec.output.repo")
			}
			if publish {
				if req.Type == server.RunScan {
					return errors.New("--publish: a scan writes no files")
				}
				req.Publish = &remote.PublishRequest{Message: message, Branch: branch}
			}
			c, err := rf.client()
			if err != nil {
				return err
			}
			run, err := c.StartRun(cmd.Context(), *req)
			if err != nil {
				return err
			}
			if !wait && !follow {
				switch {
				case format != "table":
					return printStructured(u, format, run)
				case u.Quiet:
					u.Println(run.ID)
				default:
					u.Success("started %s %s on %s: run %s", run.Type, run.Name, firstOf(run.Cluster, run.Tool), run.ID)
					u.Step("follow it: gitopsify run logs -f %s", run.ID)
				}
				return nil
			}
			if !u.Quiet && format == "table" {
				u.Step("started %s %s on %s: run %s", run.Type, run.Name, firstOf(run.Cluster, run.Tool), run.ID)
			}
			return waitRun(cmd.Context(), u, c, run.ID, follow, timeout, format)
		},
	}
	cmd.Flags().StringVar(&cluster, "cluster", "", "source cluster, by its name on the server (default: the file's spec.source.context)")
	cmd.Flags().BoolVar(&scan, "scan", false, "only scan the cluster: what would be migrated, how, and what is left out")
	cmd.Flags().StringVar(&tool, "tool", "", "tool an export reads, by its name on the server (default: the file's only instance)")
	cmd.Flags().StringVar(&repoURL, "repo-url", "", "Git repository an export's output is pushed to")
	cmd.Flags().StringVar(&repoBranch, "repo-branch", "", "branch of --repo-url (default main)")
	cmd.Flags().BoolVar(&allowPartial, "allow-partial", false, "write an export even if the provider couldn't read everything")
	cmd.Flags().BoolVar(&publish, "publish", false, "push the output when the run succeeds")
	cmd.Flags().StringVar(&branch, "branch", "", "with --publish: push to this branch instead, for a pull request")
	cmd.Flags().StringVar(&message, "message", "", "commit message (default: names the run)")
	addWaitFlags(cmd.Flags(), &wait, &follow, &timeout, &format)
	return cmd
}

func addWaitFlags(fs *pflag.FlagSet, wait, follow *bool, timeout *time.Duration, format *string) {
	fs.BoolVar(wait, "wait", false, "wait for the run to end; exit 1 when it or its push failed")
	fs.BoolVarP(follow, "follow", "f", false, "print the run's log until it ends (implies --wait)")
	fs.DurationVar(timeout, "timeout", 2*time.Hour, "with --wait: give up after this long (the run goes on)")
	fs.StringVar(format, "format", "table", "output format: table (for people), json or yaml (the run)")
	complete(fs, "format", "table", "json", "yaml")
}

// runRequestFrom reads the Migration or Export resource of a file into a
// run request, leaving out the local access settings the server replaces.
func runRequestFrom(path, cluster, tool string, scan bool) (*remote.RunRequest, error) {
	if path == "" {
		if _, err := os.Stat(DefaultConfigFile); err != nil {
			return nil, fmt.Errorf("no %s here: name the Migration or Export file with --config", DefaultConfigFile)
		}
		path = DefaultConfigFile
	}
	docs, err := config.ReadDocuments(path)
	if err != nil {
		return nil, err
	}
	migrations, exports := config.Find(docs, config.Kind, ""), config.Find(docs, config.KindExport, "")
	switch {
	case len(migrations)+len(exports) == 0:
		return nil, fmt.Errorf("%s holds no Migration or Export resource", path)
	case len(migrations)+len(exports) > 1:
		return nil, fmt.Errorf("%s holds several resources; put the one to run in a file of its own", path)
	case len(exports) == 1:
		if scan {
			return nil, errors.New("--scan is for migrations")
		}
		var x map[string]any
		if err := json.Unmarshal(exports[0].JSON, &x); err != nil {
			return nil, err
		}
		spec, _ := x["spec"].(map[string]any)
		if insts, _ := spec["instances"].([]any); len(insts) > 0 {
			if tool == "" {
				if len(insts) > 1 {
					return nil, fmt.Errorf("%s reads %d instances: pick the tool with --tool", path, len(insts))
				}
				inst, _ := insts[0].(map[string]any)
				tool, _ = inst["name"].(string)
			}
			delete(spec, "instances") // the server's tool settings replace them
		}
		if tool == "" {
			return nil, errors.New("name the tool to export with --tool")
		}
		b, err := json.Marshal(x)
		if err != nil {
			return nil, err
		}
		return &remote.RunRequest{Type: server.RunExport, Tool: tool, Export: string(b)}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(migrations[0].JSON, &m); err != nil {
		return nil, err
	}
	spec, _ := m["spec"].(map[string]any)
	if src, ok := spec["source"].(map[string]any); ok {
		if cluster == "" {
			cluster, _ = src["context"].(string)
		}
		delete(spec, "source")
	}
	if cluster == "" {
		return nil, fmt.Errorf("name the source cluster: --cluster, or spec.source.context in %s", path)
	}
	envs, _ := spec["environments"].([]any)
	for _, e := range envs {
		env, _ := e.(map[string]any)
		if c, ok := env["cluster"].(map[string]any); ok {
			for _, k := range accessKeys() {
				if k != "context" {
					delete(c, k)
				}
			}
		}
	}
	if out, ok := spec["output"].(map[string]any); ok {
		delete(out, "dir") // the server's own
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	typ := server.RunMigrate
	if scan {
		typ = server.RunScan
	}
	return &remote.RunRequest{Type: typ, Cluster: cluster, Migration: string(b)}, nil
}

// accessKeys are the JSON names of the cluster access settings.
func accessKeys() []string {
	var keys []string
	t := reflect.TypeFor[config.Access]()
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			keys = append(keys, name)
		}
	}
	return keys
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// waitRun follows a run to its end, printing its log when asked, and fails
// when the run or a push failed.
func waitRun(ctx context.Context, u *ui.UI, c *remote.Client, id string, printLog bool, timeout time.Duration, format string) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var onLog func(server.LogLine)
	if printLog {
		// With --format json or yaml, stdout is the run alone.
		lu := u
		if format != "table" {
			toErr := *u
			toErr.Out, toErr.OutColor = u.Err, u.Color
			lu = &toErr
		}
		onLog = func(l server.LogLine) { printLogLine(lu, l) }
	}
	run, err := c.Follow(ctx, id, onLog, nil)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("run %s didn't end within %s; it goes on: gitopsify run wait %s", id, timeout, id)
		}
		return err
	}
	if run == nil {
		if run, err = c.Run(ctx, id); err != nil {
			return err
		}
	}
	if format != "table" {
		if err := printStructured(u, format, run); err != nil {
			return err
		}
	} else {
		printRunEnd(u, run)
	}
	return runOutcome(run)
}

func printLogLine(u *ui.UI, l server.LogLine) {
	ts := l.Time.Local().Format("15:04:05")
	msg := remote.Clean(l.Message)
	switch strings.ToLower(l.Level) {
	case "warn", "warning":
		msg = u.Colorize("warning", msg)
	case "error":
		msg = u.Colorize("error", msg)
	}
	u.Printf("%s %s\n", u.Dim(ts), msg)
}

func printRunEnd(u *ui.UI, r *server.Run) {
	what := remote.Clean(fmt.Sprintf("%s %s on %s", r.Type, r.Name, firstOf(r.Cluster, r.Tool)))
	switch r.Status {
	case server.StatusSucceeded:
		if u.Quiet {
			u.Println(r.ID)
		} else {
			u.Success("%s succeeded (run %s)%s", what, r.ID, summaryText(r.Summary))
		}
	case server.StatusCanceled:
		u.Warn("%s was canceled (run %s)", what, r.ID)
	default:
		u.Errorf("%s %s (run %s): %s", what, r.Status, r.ID, remote.Clean(r.Error))
	}
	for _, p := range r.Publishes {
		printPublish(u, p)
	}
}

func summaryText(s *server.RunSummary) string {
	if s == nil {
		return ""
	}
	parts := []string{fmt.Sprintf("%d apps", s.Apps), fmt.Sprintf("%d objects", s.Objects)}
	if s.Files > 0 {
		parts = append(parts, fmt.Sprintf("%d files (%d differ from the repository)", s.Files, s.Changed))
	}
	if s.Warnings > 0 {
		parts = append(parts, fmt.Sprintf("%d warnings", s.Warnings))
	}
	if s.SecretActions > 0 {
		parts = append(parts, fmt.Sprintf("%d Secrets need action", s.SecretActions))
	}
	return ": " + strings.Join(parts, ", ")
}

func printPublish(u *ui.UI, p server.Publish) {
	p.Remote, p.Branch, p.Base, p.Error = remote.Clean(p.Remote), remote.Clean(p.Branch), remote.Clean(p.Base), remote.Clean(p.Error)
	switch {
	case p.Error != "":
		u.Errorf("push to %s (%s) failed: %s", p.Remote, p.Branch, p.Error)
	case p.Unchanged:
		u.Step("%s (%s) already holds this output: nothing pushed", p.Remote, firstOf(p.Base, p.Branch))
	case p.UpToDate:
		u.Step("%s (%s) already holds this output: nothing pushed", p.Remote, p.Branch)
	case p.Base != "":
		u.Success("pushed %s to %s branch %s, for a pull request into %s", p.Commit, p.Remote, p.Branch, p.Base)
	default:
		u.Success("pushed %s to %s (%s)", p.Commit, p.Remote, p.Branch)
	}
}

// runOutcome is the exit status of a finished run: 1 when it or one of its
// pushes failed.
func runOutcome(r *server.Run) error {
	if r.Status != server.StatusSucceeded {
		return &exitError{code: 1}
	}
	return pushOutcome(r.Publishes)
}

func pushOutcome(pubs []server.Publish) error {
	for _, p := range pubs {
		if p.Error != "" {
			return &exitError{code: 1}
		}
	}
	return nil
}

func newRunList(o *options, rf *remoteFlags) *cobra.Command {
	var f remote.RunFilter
	var format string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List runs, newest first",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := noLocalClusterFlags(cmd); err != nil {
				return err
			}
			if err := checkRemoteFormat(&format); err != nil {
				return err
			}
			u := o.ui()
			c, err := rf.client()
			if err != nil {
				return err
			}
			runs, err := c.ListRuns(cmd.Context(), f)
			if err != nil {
				return err
			}
			if format != "table" {
				return printStructured(u, format, runs)
			}
			if len(runs) == 0 {
				u.Step("no runs")
				return nil
			}
			rows := make([][]string, 0, len(runs))
			for _, r := range runs {
				rows = append(rows, []string{remote.Clean(r.ID), r.Type, remote.Clean(r.Name), remote.Clean(firstOf(r.Cluster, r.Tool)),
					u.Colorize(statusSeverity(r.Status), remote.Clean(r.Status)), remote.Clean(r.CreatedBy), ago(r.CreatedAt)})
			}
			u.Table([]string{"ID", "TYPE", "NAME", "CLUSTER/TOOL", "STATUS", "BY", "CREATED"}, rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&f.Status, "status", "", "only runs with this status: queued, running, succeeded, failed or canceled")
	cmd.Flags().StringVar(&f.Type, "type", "", "only this type: scan, migrate or export")
	cmd.Flags().StringVar(&f.Cluster, "cluster", "", "only runs on this cluster")
	cmd.Flags().StringVar(&f.CreatedBy, "created-by", "", "only runs started by this user")
	cmd.Flags().IntVar(&f.Limit, "limit", 30, "at most this many (up to 500)")
	cmd.Flags().StringVar(&format, "format", "table", "output format: table, json or yaml")
	complete(cmd.Flags(), "format", "table", "json", "yaml")
	complete(cmd.Flags(), "status", server.StatusQueued, server.StatusRunning, server.StatusSucceeded, server.StatusFailed, server.StatusCanceled)
	complete(cmd.Flags(), "type", server.RunScan, server.RunMigrate, server.RunExport)
	return cmd
}

func statusSeverity(s string) string {
	switch s {
	case server.StatusSucceeded:
		return "ok"
	case server.StatusFailed:
		return "error"
	case server.StatusCanceled:
		return "warning"
	}
	return "info"
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Local().Format("2006-01-02")
}

func newRunGet(o *options, rf *remoteFlags) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Show a run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := noLocalClusterFlags(cmd); err != nil {
				return err
			}
			if err := checkRemoteFormat(&format); err != nil {
				return err
			}
			u := o.ui()
			c, err := rf.client()
			if err != nil {
				return err
			}
			r, err := c.Run(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if format != "table" {
				return printStructured(u, format, r)
			}
			field := func(k, v string) {
				if v != "" {
					u.Printf("%s %s\n", u.Bold(fmt.Sprintf("%-9s", k+":")), remote.Clean(v))
				}
			}
			field("Run", r.ID)
			field("Type", r.Type)
			field("Name", r.Name)
			field("Cluster", r.Cluster)
			field("Tool", r.Tool)
			field("Status", u.Colorize(statusSeverity(r.Status), r.Status))
			field("By", r.CreatedBy)
			field("Created", r.CreatedAt.Local().Format(time.DateTime))
			if r.FinishedAt != nil {
				field("Finished", r.FinishedAt.Local().Format(time.DateTime))
			}
			field("Error", r.Error)
			if s := summaryText(r.Summary); s != "" {
				field("Summary", strings.TrimPrefix(s, ": "))
			}
			for _, p := range r.Publishes {
				printPublish(u, p)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "table", "output format: table, json or yaml")
	complete(cmd.Flags(), "format", "table", "json", "yaml")
	return cmd
}

func newRunLogs(o *options, rf *remoteFlags) *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs <id>",
		Short: "Print a run's log",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := noLocalClusterFlags(cmd); err != nil {
				return err
			}
			u := o.ui()
			c, err := rf.client()
			if err != nil {
				return err
			}
			if follow {
				_, err := c.Follow(cmd.Context(), args[0], func(l server.LogLine) { printLogLine(u, l) }, nil)
				return err
			}
			lines, err := c.Log(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			for _, l := range lines {
				printLogLine(u, l)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing until the run ends")
	return cmd
}

func newRunWait(o *options, rf *remoteFlags) *cobra.Command {
	var timeout time.Duration
	var format string
	cmd := &cobra.Command{
		Use:   "wait <id>",
		Short: "Wait for a run to end; exit 1 when it or its push failed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := noLocalClusterFlags(cmd); err != nil {
				return err
			}
			if err := checkRemoteFormat(&format); err != nil {
				return err
			}
			c, err := rf.client()
			if err != nil {
				return err
			}
			return waitRun(cmd.Context(), o.ui(), c, args[0], false, timeout, format)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Hour, "give up after this long (the run goes on)")
	cmd.Flags().StringVar(&format, "format", "table", "output format: table (for people), json or yaml (the run)")
	complete(cmd.Flags(), "format", "table", "json", "yaml")
	return cmd
}

func newRunCancel(o *options, rf *remoteFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <id>",
		Short: "Stop a queued or working run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := noLocalClusterFlags(cmd); err != nil {
				return err
			}
			c, err := rf.client()
			if err != nil {
				return err
			}
			if err := c.CancelRun(cmd.Context(), args[0]); err != nil {
				return err
			}
			o.ui().Success("canceling run %s", args[0])
			return nil
		},
	}
}

func newRunDelete(o *options, rf *remoteFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a finished run and its files",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := noLocalClusterFlags(cmd); err != nil {
				return err
			}
			c, err := rf.client()
			if err != nil {
				return err
			}
			if err := c.DeleteRun(cmd.Context(), args[0]); err != nil {
				return err
			}
			o.ui().Success("deleted run %s", args[0])
			return nil
		},
	}
}

func newRunPublish(o *options, rf *remoteFlags) *cobra.Command {
	var branch, message, format string
	cmd := &cobra.Command{
		Use:   "publish <id>",
		Short: "Push a run's output to its Git repository",
		Long: `Commits a succeeded migration's or export's files and pushes them to the
repository the run names. --branch pushes to that branch instead, for a pull
request: nothing is pushed when the output doesn't differ from the run's branch,
and a branch holding only gitopsify's commits is replaced. Exits 1 when a push
failed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := noLocalClusterFlags(cmd); err != nil {
				return err
			}
			if err := checkRemoteFormat(&format); err != nil {
				return err
			}
			u := o.ui()
			c, err := rf.client()
			if err != nil {
				return err
			}
			before := 0
			if r, err := c.Run(cmd.Context(), args[0]); err == nil {
				before = len(r.Publishes)
			}
			r, err := c.Publish(cmd.Context(), args[0], remote.PublishRequest{Branch: branch, Message: message})
			if err != nil {
				return err
			}
			if format != "table" {
				return printStructured(u, format, r)
			}
			if before > len(r.Publishes) {
				before = 0
			}
			for _, p := range r.Publishes[before:] {
				printPublish(u, p)
			}
			return pushOutcome(r.Publishes[before:])
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "push to this branch instead, for a pull request")
	cmd.Flags().StringVar(&message, "message", "", "commit message (default: names the run)")
	cmd.Flags().StringVar(&format, "format", "table", "output format: table (for people), json or yaml (the run)")
	complete(cmd.Flags(), "format", "table", "json", "yaml")
	return cmd
}
