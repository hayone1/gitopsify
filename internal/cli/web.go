package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/hayone1/gitopsify/internal/server"
	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/web"
)

func newUI(o *options) *cobra.Command {
	var listen, dataDir, kubeconfig string
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Open the web UI on this machine",
		Long: `Starts the web UI on localhost for you alone and opens it in the browser. It uses
the clusters of your kubeconfig, as the CLI does, and keeps its runs in
--data-dir.

For a shared, multi-user server with single sign-on, roles and an audit log,
run gitopsify server (or the Helm chart) instead.`,
		Example: `  gitopsify ui
  gitopsify ui --listen 127.0.0.1:8080 --no-browser`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			host, _, err := net.SplitHostPort(listen)
			if err != nil {
				return fmt.Errorf("--listen: %w", err)
			}
			if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
				return errors.New("gitopsify ui only listens on localhost; for a shared server use gitopsify server")
			}
			if dataDir == "" {
				base, err := os.UserCacheDir()
				if err != nil {
					return err
				}
				dataDir = filepath.Join(base, "gitopsify", "ui")
			}
			ln, err := net.Listen("tcp", listen)
			if err != nil {
				return err
			}
			// One person, like the CLI: Secret values aren't kept from them.
			cfg := &config.Server{Spec: config.ServerSpec{Listen: ln.Addr().String(), DataDir: dataDir,
				ExternalURL: "http://" + ln.Addr().String(), Auth: config.ServerAuth{Mode: config.AuthLocal},
				Runs: config.ServerRuns{AllowSecretValues: true}}}
			cfg.Default()
			token := server.NewToken()
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			srv, err := server.New(ctx, server.Options{Config: cfg, Local: true, LocalToken: token, Kubeconfig: kubeconfig,
				Version: Version, Log: log, UI: web.FS()})
			if err != nil {
				_ = ln.Close()
				return err
			}
			url := fmt.Sprintf("http://%s/api/v1/auth/local?token=%s", ln.Addr(), token)
			u := o.ui()
			u.Success("gitopsify is running at http://%s (Ctrl-C stops it)", ln.Addr())
			u.Printf("  Open: %s\n", url)
			if !noBrowser {
				openBrowser(url)
			}
			return srv.ListenAndServe(ctx, ln)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:0", "address to listen on (localhost only; port 0 picks a free one)")
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "where runs are kept (default: the user cache directory)")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the address instead of opening the browser")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig whose contexts the UI offers (default: KUBECONFIG or ~/.kube/config)")
	return cmd
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func newServer(o *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the shared web server (single sign-on, roles, audit log)",
		Long: `Runs gitopsify's web UI and API for a team: people sign in with OpenID Connect
(Dex, Keycloak, Okta, Entra ID, Google...), get roles per cluster from their
groups, and every action lands in an audit log. Runs (scans and migrations)
work in the background and are kept in the data directory.

The configuration is a Server resource (--config, default
/etc/gitopsify/server.yaml); docs/web-ui.md describes it, and the Helm chart in
charts/gitopsify deploys it in a cluster.`,
		Example: `  gitopsify server --config server.yaml
  gitopsify server token --name ci --role operator`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := o.configFile
			if path == "" {
				path = firstNonEmptyEnv("GITOPSIFY_SERVER_CONFIG", "/etc/gitopsify/server.yaml")
			}
			cfg, err := config.LoadServer(path)
			if err != nil {
				return err
			}
			log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			srv, err := server.New(ctx, server.Options{Config: cfg, Version: Version, Log: log, UI: web.FS()})
			if err != nil {
				return err
			}
			ln, err := net.Listen("tcp", cfg.Spec.Listen)
			if err != nil {
				return err
			}
			log.Info("gitopsify server listening", "address", ln.Addr().String(), "version", Version, "auth", cfg.Spec.Auth.Mode,
				"ui", web.FS() != nil)
			return srv.ListenAndServe(ctx, ln)
		},
	}
	cmd.AddCommand(newServerToken())
	return cmd
}

func newServerToken() *cobra.Command {
	var name, role string
	var clusters, tools []string
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Make an API token for automation",
		Long: `Prints a new API token and the spec.auth.tokens entry that admits it. The server
stores only the token's SHA-256; keep the token itself in your CI's secrets and
send it as "Authorization: Bearer <token>".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if role != config.RoleViewer && role != config.RoleOperator && role != config.RoleAdmin {
				return fmt.Errorf("--role: want viewer, operator or admin")
			}
			t := server.NewToken()
			fmt.Fprintf(cmd.OutOrStdout(), "Token (shown once): %s\n\nAdd to the Server resource:\n\n  auth:\n    tokens:\n      - name: %s\n        role: %s\n        sha256: %s\n",
				t, name, role, server.TokenHash(t))
			if len(clusters) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "        clusters: [%s]\n", joinQuoted(clusters))
			}
			if len(tools) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "        tools: [%s]\n", joinQuoted(tools))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "automation", "the token's name, as the audit log shows it")
	cmd.Flags().StringVar(&role, "role", config.RoleOperator, "viewer, operator or admin")
	cmd.Flags().StringSliceVar(&clusters, "cluster", nil, "limit the token to these clusters (globs)")
	cmd.Flags().StringSliceVar(&tools, "tool", nil, "limit the token to these tools (globs)")
	return cmd
}

func joinQuoted(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%q", v)
	}
	return out
}

func firstNonEmptyEnv(env, def string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}
