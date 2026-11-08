package cli

import (
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/filter"
	"github.com/hayone1/gitopsify/pkg/kube"
)

// noClusterFlags marks commands whose help leaves out the cluster access
// flags.
const noClusterFlags = "gitopsify_no_cluster_flags"

// groupOrder is the order of the flag sections in --help.
var groupOrder = []string{groupScope, groupHelm, groupShape, groupTransform, groupSecrets, groupOutput, groupGit,
	groupBootstrap, groupTarget, groupSource}

// usageTemplate is cobra's, with the flags split into sections.
const usageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional Commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{flagSections .}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

func init() {
	cobra.AddTemplateFunc("flagSections", flagSections)
}

// flagSections renders the flags of a command: its own ungrouped flags, then
// one section per group, then the remaining global flags.
func flagSections(cmd *cobra.Command) string {
	sections := map[string]*pflag.FlagSet{}
	section := func(name string) *pflag.FlagSet {
		if sections[name] == nil {
			sections[name] = pflag.NewFlagSet(name, pflag.ContinueOnError)
		}
		return sections[name]
	}
	add := func(fs *pflag.FlagSet, fallback string) {
		fs.VisitAll(func(f *pflag.Flag) {
			if f.Hidden {
				return
			}
			name := fallback
			if g := f.Annotations[groupAnnotation]; len(g) > 0 {
				name = g[0]
			}
			section(name).AddFlag(f)
		})
	}
	add(cmd.LocalFlags(), "Flags")
	add(cmd.InheritedFlags(), "Global Flags")
	// Provider commands don't talk to clusters: leave the cluster flags out.
	if _, ok := cmd.Annotations[noClusterFlags]; ok {
		delete(sections, groupSource)
		delete(sections, groupTarget)
	}
	var b strings.Builder
	write := func(name string) {
		if fs := sections[name]; fs != nil && fs.HasAvailableFlags() {
			b.WriteString("\n\n" + name + ":\n")
			b.WriteString(strings.TrimRight(fs.FlagUsages(), " \n"))
		}
	}
	write("Flags")
	for _, g := range groupOrder {
		write(g)
	}
	write("Global Flags")
	return b.String()
}

func ruleNames() []string {
	var out []string
	for _, r := range filter.DefaultRules() {
		out = append(out, r.Name)
	}
	return out
}

// registerCompletions attaches shell completions to every command's flags:
// fixed values, kubeconfig contexts and the configured environments.
func registerCompletions(root *cobra.Command, o *options) {
	contexts := func(kubeconfigFlag string) cobra.CompletionFunc {
		return func(cmd *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
			kc := ""
			if f := cmd.Flags().Lookup(kubeconfigFlag); f != nil {
				kc = f.Value.String()
			}
			raw, err := kube.LoadingRules(kc).Load()
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			var out []string
			for n := range raw.Contexts {
				out = append(out, n)
			}
			sort.Strings(out)
			return out, cobra.ShellCompDirectiveNoFileComp
		}
	}
	envs := func(cmd *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		path := o.configFile
		if path == "" {
			path = DefaultConfigFile
		}
		cfg, err := config.Load(path)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var out []string
		for _, e := range cfg.Environments {
			out = append(out, e.Name)
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
	_ = root.RegisterFlagCompletionFunc("context", contexts("kubeconfig"))
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for name, values := range completions {
			if c.LocalNonPersistentFlags().Lookup(name) != nil {
				vals := values
				_ = c.RegisterFlagCompletionFunc(name, cobra.FixedCompletions(vals, cobra.ShellCompDirectiveNoFileComp))
			}
		}
		if c.LocalNonPersistentFlags().Lookup("target-context") != nil {
			_ = c.RegisterFlagCompletionFunc("target-context", contexts("target-kubeconfig"))
		}
		if c.LocalNonPersistentFlags().Lookup("env") != nil {
			_ = c.RegisterFlagCompletionFunc("env", envs)
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}
