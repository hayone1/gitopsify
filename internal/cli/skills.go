package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hayone1/gitopsify/skills"
)

// agentDirs are where agents look for skills: relative to the home directory
// for user installs, to the project for project installs.
var agentDirs = map[string]struct{ user, project, who string }{
	"agents":  {".agents/skills", ".agents/skills", "Codex, Gemini CLI, Cursor, GitHub Copilot and other agents that read .agents/skills"},
	"claude":  {".claude/skills", ".claude/skills", "Claude Code (also read by Cursor and GitHub Copilot)"},
	"copilot": {".copilot/skills", ".github/skills", "GitHub Copilot"},
	"cursor":  {".cursor/skills", ".cursor/skills", "Cursor"},
	"gemini":  {".gemini/skills", ".gemini/skills", "Gemini CLI"},
}

// agentAliases name agents that read another agent's directory.
var agentAliases = map[string]string{"codex": "agents", "opencode": "agents", "antigravity": "agents"}

type skillInfo struct {
	name, description string
}

func bundledSkills() ([]skillInfo, error) {
	entries, err := fs.ReadDir(skills.FS, ".")
	if err != nil {
		return nil, err
	}
	var out []skillInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := fs.ReadFile(skills.FS, path.Join(e.Name(), "SKILL.md"))
		if err != nil {
			return nil, err
		}
		out = append(out, skillInfo{name: e.Name(), description: frontmatter(string(data), "description")})
	}
	return out, nil
}

// frontmatter returns a single-line field of a SKILL.md's YAML frontmatter.
func frontmatter(doc, key string) string {
	if !strings.HasPrefix(doc, "---\n") {
		return ""
	}
	head, _, _ := strings.Cut(doc[4:], "\n---")
	for _, l := range strings.Split(head, "\n") {
		if v, ok := strings.CutPrefix(l, key+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func newSkills(o *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Install the gitopsify skills for AI coding agents (Claude Code, Codex, Gemini CLI, Cursor, Copilot)",
		Long: `gitopsify ships agent skills: playbooks an AI coding agent follows to run a
migration with you, including the parts gitopsify leaves to people on purpose -
creating the remote repository on GitHub, GitLab, Azure DevOps or Bitbucket,
deploy keys, installing controllers, loading Secrets into a secret manager and
verifying the result.

The skills use the open Agent Skills format. Claude Code users can also install
them as a plugin (/plugin marketplace add hayone1/gitopsify), and Gemini CLI
users as an extension (gemini extensions install https://github.com/hayone1/gitopsify).`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newSkillsList(o), newSkillsShow(o), newSkillsInstall(o))
	return cmd
}

func newSkillsList(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the bundled skills",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			list, err := bundledSkills()
			if err != nil {
				return err
			}
			var rows [][]string
			for _, s := range list {
				d := s.description
				if i := strings.Index(d, ". "); i > 0 {
					d = d[:i+1]
				}
				rows = append(rows, []string{s.name, d})
			}
			o.ui().Table([]string{"Skill", "Description"}, rows)
			return nil
		},
	}
}

func newSkillsShow(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "show <skill>",
		Short: "Print a skill's SKILL.md",
		Args:  cobra.ExactArgs(1),
		ValidArgsFunction: func(*cobra.Command, []string, string) ([]cobra.Completion, cobra.ShellCompDirective) {
			list, _ := bundledSkills()
			var names []string
			for _, s := range list {
				names = append(names, s.name)
			}
			return names, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := fs.ReadFile(skills.FS, path.Join(args[0], "SKILL.md"))
			if err != nil {
				return fmt.Errorf("no skill %q (see gitopsify skills list)", args[0])
			}
			o.ui().Printf("%s", data)
			return nil
		},
	}
}

func newSkillsInstall(o *options) *cobra.Command {
	var agents []string
	var project, dryRun bool
	var dir string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Copy the skills to where your AI coding agents find them",
		Long: `Copies every gitopsify skill into the skills directory of the chosen agents,
replacing earlier versions of the gitopsify skills and leaving other skills
alone. By default they go to your home directory for the agents most people
use: ~/.agents/skills (Codex, Gemini CLI, Cursor, GitHub Copilot) and
~/.claude/skills (Claude Code). --project installs into the current project
instead, to share them through Git.

Agents: agents (also codex, opencode, antigravity), claude, copilot, cursor,
gemini, or all.`,
		Example: `  gitopsify skills install
  gitopsify skills install --agent claude --project
  gitopsify skills install --dir ~/my-agent/skills`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			u := o.ui()
			targets, err := skillTargets(agents, project, dir)
			if err != nil {
				return err
			}
			list, err := bundledSkills()
			if err != nil {
				return err
			}
			for _, t := range targets {
				for _, s := range list {
					dst := filepath.Join(t.dir, s.name)
					if dryRun {
						u.Println(dst)
						continue
					}
					if err := writeSkill(s.name, dst); err != nil {
						return err
					}
				}
				if !dryRun {
					u.Success("installed %d skills in %s (%s)", len(list), t.dir, t.who)
				}
			}
			if !dryRun {
				u.Step("start a new agent session, then ask it to migrate a cluster with gitopsify (or invoke gitopsify-migrate)")
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&agents, "agent", nil, "agents to install for: agents, claude, copilot, cursor, gemini, codex, opencode, antigravity or all (default agents,claude)")
	cmd.Flags().BoolVar(&project, "project", false, "install into the current project instead of your home directory")
	cmd.Flags().StringVar(&dir, "dir", "", "install into this skills directory instead")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print where the skills would go")
	complete(cmd.Flags(), "agent", "agents", "claude", "copilot", "cursor", "gemini", "codex", "opencode", "antigravity", "all")
	return cmd
}

type skillTarget struct{ dir, who string }

func skillTargets(agents []string, project bool, dir string) ([]skillTarget, error) {
	if dir != "" {
		return []skillTarget{{dir: dir, who: "custom directory"}}, nil
	}
	if len(agents) == 0 {
		agents = []string{"agents", "claude"}
	}
	base := "."
	if !project {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		base = home
	}
	seen := map[string]bool{}
	var names []string
	for _, a := range agents {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "all" {
			for n := range agentDirs {
				names = append(names, n)
			}
			continue
		}
		if alias, ok := agentAliases[a]; ok {
			a = alias
		}
		if _, ok := agentDirs[a]; !ok {
			return nil, fmt.Errorf("unknown agent %q (agents, claude, copilot, cursor, gemini, codex, opencode, antigravity, all)", a)
		}
		names = append(names, a)
	}
	sort.Strings(names)
	var out []skillTarget
	for _, n := range names {
		d := agentDirs[n]
		rel := d.user
		if project {
			rel = d.project
		}
		p := filepath.Join(base, filepath.FromSlash(rel))
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, skillTarget{dir: p, who: d.who})
	}
	return out, nil
}

// writeSkill replaces dst with the bundled skill.
func writeSkill(name, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return fs.WalkDir(skills.FS, name, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, name), "/")
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(skills.FS, p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
