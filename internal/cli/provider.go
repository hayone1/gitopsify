package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/hayone1/gitopsify/internal/ui"
	"github.com/hayone1/gitopsify/pkg/provider"
	"github.com/hayone1/gitopsify/sdk"
)

func newProvider(o *options) *cobra.Command {
	var index string
	cmd := &cobra.Command{
		Use:   "provider",
		Short: "Find, install and inspect providers (Grafana, ...) for gitopsify export",
		Long: `Providers read the state of a tool, such as Grafana, so "gitopsify export" can
write it as Terraform or Crossplane configuration. Each provider is a separate
program, gitopsify-provider-<name>, published in its own repository.

Install one by name from the provider index, or by its GitHub source
(owner/name) when it isn't listed. Executables named gitopsify-provider-<name>
on your PATH work too, which is handy when you develop one.`,
		Example: `  gitopsify provider search
  gitopsify provider install grafana
  gitopsify provider info grafana
  gitopsify export grafana`,
		Annotations: map[string]string{noClusterFlags: ""},
	}
	cmd.PersistentFlags().StringVar(&index, "index", "", "provider index: a URL or a file (default: the published index, or $"+provider.EnvIndex+")")
	cmd.AddCommand(newProviderList(o), newProviderSearch(o, &index), newProviderInstall(o, &index), newProviderRemove(o), newProviderInfo(o))
	for _, c := range cmd.Commands() {
		c.Annotations = map[string]string{noClusterFlags: ""}
	}
	return cmd
}

func newProviderList(o *options) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the providers gitopsify can run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			u := o.ui()
			list, err := provider.List()
			if err != nil {
				return err
			}
			if format != "table" {
				return printStructured(u, format, list)
			}
			if len(list) == 0 {
				u.Println("No providers installed. Find one with: gitopsify provider search")
				return nil
			}
			var rows [][]string
			for _, p := range list {
				version := p.Version
				if version == "" {
					version = "-"
				}
				rows = append(rows, []string{p.Name, version, firstNonEmpty(p.Source, "-"), p.Origin, p.Path})
			}
			u.Table([]string{"NAME", "VERSION", "SOURCE", "FROM", "PATH"}, rows)
			for _, p := range list {
				for _, s := range p.Shadowed {
					u.Warn("%s: %s is not used; %s comes first", p.Name, s, p.Path)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "table", "output format: table, json or yaml")
	return cmd
}

func newProviderSearch(o *options, index *string) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "search [term]",
		Short: "Search the provider index",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			u := o.ui()
			idx, err := provider.LoadIndex(cmd.Context(), *index)
			if err != nil {
				return err
			}
			term := ""
			if len(args) == 1 {
				term = args[0]
			}
			found := idx.Search(term)
			if format != "table" {
				return printStructured(u, format, found)
			}
			if len(found) == 0 {
				u.Println("No provider matches. Anyone can publish one: https://github.com/hayone1/gitopsify/blob/main/docs/writing-providers.md")
				return nil
			}
			installed := map[string]bool{}
			if list, err := provider.List(); err == nil {
				for _, p := range list {
					installed[p.Name] = true
				}
			}
			var rows [][]string
			for _, e := range found {
				name := e.Name
				if installed[e.Name] {
					name += " ✓"
				}
				rows = append(rows, []string{name, e.Source, firstNonEmpty(e.Maturity, "-"), strings.Join(e.Outputs, ", "), e.Description})
			}
			u.Table([]string{"NAME", "SOURCE", "MATURITY", "OUTPUTS", "DESCRIPTION"}, rows)
			if idx.Location == "built-in" {
				u.Step("the published index couldn't be fetched; showing the copy built into gitopsify")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "table", "output format: table, json or yaml")
	return cmd
}

func newProviderInstall(o *options, index *string) *cobra.Command {
	return &cobra.Command{
		Use:   "install <name|owner/name>[@version]",
		Short: "Install or upgrade a provider from its GitHub releases",
		Long: `Downloads a provider release for this machine, checks it against the release's
checksums, makes sure it runs, and installs it into the providers directory
($` + provider.EnvDir + ` to change it). Without a version it installs the latest
release. GITHUB_TOKEN (or GH_TOKEN) is used when set: for private repositories
and to avoid GitHub's rate limits.`,
		Example: `  gitopsify provider install grafana
  gitopsify provider install grafana@v0.1.0
  gitopsify provider install acme/keycloak`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := installProvider(cmd.Context(), o.ui(), args[0], *index, nil)
			return err
		},
	}
}

// installProvider resolves and installs a provider, printing progress.
func installProvider(ctx context.Context, u *ui.UI, ref, index string, checksums map[string]string) (*provider.Installed, error) {
	src, version, err := provider.Resolve(ctx, ref, index)
	if err != nil {
		return nil, err
	}
	what := src.String()
	if version != "" {
		what += "@" + version
	}
	u.Step("installing %s from github.com/%s/%s", what, src.Owner, src.Repo)
	inst := &provider.Installer{Progress: func(s string) { u.Step("%s", s) }, Checksums: checksums}
	in, err := inst.Install(ctx, src, version)
	if err != nil {
		return nil, err
	}
	u.Success("installed %s %s (%s)", src.Name, in.Version, in.Path)
	return in, nil
}

func newProviderRemove(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Uninstall a provider",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			u := o.ui()
			if err := provider.Remove(provider.NameOf(args[0])); err != nil {
				if errors.Is(err, provider.ErrNotFound) {
					if p, ferr := provider.Find(args[0]); ferr == nil && p.Origin == provider.OriginPath {
						return fmt.Errorf("%s comes from your PATH (%s); gitopsify only removes providers it installed", args[0], p.Path)
					}
				}
				return err
			}
			u.Success("removed %s", args[0])
			return nil
		},
	}
}

func newProviderInfo(o *options) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "info <name>",
		Short: "Show what a provider reads and what it needs to connect",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			u := o.ui()
			p, err := provider.Find(args[0])
			if err != nil {
				return notInstalled(args[0], err)
			}
			d, err := (&provider.Client{Name: p.Name, Path: p.Path}).Describe(cmd.Context())
			if err != nil {
				return err
			}
			if format != "table" {
				return printStructured(u, format, d)
			}
			printDescription(u, p, d)
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "table", "output format: table, json or yaml")
	return cmd
}

func printDescription(u *ui.UI, p *provider.Installed, d *sdk.Description) {
	u.Println(u.Bold(d.Title) + " provider " + d.Version + " (" + p.Path + ")")
	if d.Description != "" {
		u.Println(d.Description)
	}
	if d.Homepage != "" {
		u.Println(d.Homepage)
	}
	u.Heading("Connection settings")
	var rows [][]string
	for _, s := range d.Settings {
		flags := []string{}
		if s.Required {
			flags = append(flags, "required")
		}
		if s.Secret {
			flags = append(flags, "secret")
		}
		rows = append(rows, []string{s.Name, firstNonEmpty(s.Env, "-"), strings.Join(flags, ", "), s.Description})
	}
	u.Table([]string{"SETTING", "ENVIRONMENT", "", "DESCRIPTION"}, rows)
	u.Heading("Kinds (one workspace per " + d.Partition.Singular + ")")
	rows = nil
	for _, k := range d.Kinds {
		tf, xp := "-", "-"
		if k.Terraform != nil {
			tf = k.Terraform.Type
		}
		if k.Crossplane != nil {
			xp = k.Crossplane.Kind + "." + strings.SplitN(k.Crossplane.APIVersion, "/", 2)[0]
		}
		def := "yes"
		if k.Optional {
			def = "no"
		}
		rows = append(rows, []string{k.Name, def, tf, xp, k.Description})
	}
	u.Table([]string{"KIND", "DEFAULT", "TERRAFORM", "CROSSPLANE", "DESCRIPTION"}, rows)
	u.Heading("Outputs")
	if d.Terraform != nil {
		u.Printf("  terraform   %s %s\n", d.Terraform.Source, d.Terraform.Version)
	}
	if d.Crossplane != nil {
		u.Printf("  crossplane  %s\n", d.Crossplane.Package)
	}
}

func notInstalled(ref string, err error) error {
	if errors.Is(err, provider.ErrNotFound) {
		name := provider.NameOf(ref)
		return &exitError{code: 1, msg: fmt.Sprintf("provider %s isn't installed. Install it with:\n  gitopsify provider install %s", name, ref)}
	}
	return err
}

func printStructured(u *ui.UI, format string, v any) error {
	switch format {
	case "json":
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		u.Println(string(b))
	case "yaml":
		b, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		u.Printf("%s", b)
	default:
		return fmt.Errorf("--format must be table, json or yaml")
	}
	return nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
