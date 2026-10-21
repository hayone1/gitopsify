package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/hayone1/gitopsify/internal/ui"
	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/filter"
	"github.com/hayone1/gitopsify/pkg/model"
	"github.com/hayone1/gitopsify/pkg/pipeline"
)

func newScan(o *options) *cobra.Command {
	var showExcluded, showObjects bool
	var format string
	cmd := &cobra.Command{
		Use:     "scan",
		Aliases: []string{"discover"},
		Short:   "Inventory the source cluster: what would be migrated, how, and what is left out",
		Example: `  gitopsify scan
  gitopsify scan -n shop -n payments --show-objects
  gitopsify scan -r deployment/shop/web            # one Deployment and what it needs
  gitopsify scan --show-excluded --format yaml`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := o.load(cmd)
			if err != nil {
				return err
			}
			u := o.ui()
			popts, err := o.pipelineOptions(cfg, u.Step)
			if err != nil {
				return err
			}
			res, err := pipeline.Scan(cmd.Context(), popts)
			if err != nil {
				return err
			}
			switch format {
			case "json", "yaml":
				return printReport(u, scanReportOf(res.Inventory, showExcluded), format)
			case "table":
				printScan(u, res, cfg, showExcluded, showObjects)
				return nil
			}
			return fmt.Errorf("--format must be table, json or yaml")
		},
	}
	o.addScope(cmd.Flags())
	cmd.Flags().BoolVar(&showExcluded, "show-excluded", false, "list every excluded object and why")
	cmd.Flags().BoolVar(&showObjects, "show-objects", false, "list the objects of every app")
	cmd.Flags().StringVar(&format, "format", "table", "output format: table, json or yaml")
	return cmd
}

type scanReport struct {
	Source   model.ClusterInfo `json:"source"`
	Apps     []appReport       `json:"apps"`
	Releases []releaseReport   `json:"helmReleases,omitempty"`
	Excluded []excludedReport  `json:"excluded,omitempty"`
	// ExcludedByRule counts exclusions per rule.
	ExcludedByRule map[string]int `json:"excludedByRule,omitempty"`
	Warnings       []string       `json:"warnings,omitempty"`
}

type appReport struct {
	Name      string   `json:"name"`
	Phase     string   `json:"phase"`
	Wave      int      `json:"wave"`
	Namespace string   `json:"namespace,omitempty"`
	DependsOn []string `json:"dependsOn,omitempty"`
	Helm      string   `json:"helmRelease,omitempty"`
	Objects   []string `json:"objects"`
}

type releaseReport struct {
	Release    string   `json:"release"`
	Chart      string   `json:"chart"`
	Version    string   `json:"version"`
	Mode       string   `json:"mode,omitempty"`
	Repository string   `json:"repository,omitempty"`
	ResolvedBy string   `json:"resolvedBy,omitempty"`
	Skipped    string   `json:"skipped,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

type excludedReport struct {
	Object string `json:"object"`
	Reason string `json:"reason"`
}

func scanReportOf(inv *model.Inventory, withExcluded bool) scanReport {
	r := scanReport{Source: inv.Source, ExcludedByRule: map[string]int{}}
	for _, a := range inv.Apps {
		ar := appReport{Name: a.Name, Phase: string(a.Phase), Wave: a.Wave, Namespace: a.Namespace, DependsOn: a.DependsOn}
		if a.Release != nil {
			ar.Helm = a.Release.Key()
		}
		for _, o := range a.Objects {
			ar.Objects = append(ar.Objects, o.Ref.String())
		}
		r.Apps = append(r.Apps, ar)
	}
	for _, rel := range inv.Releases {
		rr := releaseReport{Release: rel.Key(), Chart: rel.Chart.Name, Version: rel.Chart.Version, Mode: string(rel.Mode), Warnings: rel.Warnings}
		if rel.Repo != nil {
			rr.Repository, rr.ResolvedBy = rel.Repo.URL, rel.Repo.Source
		}
		if rel.Skip {
			rr.Skipped, rr.Mode = rel.SkipReason, ""
		}
		r.Releases = append(r.Releases, rr)
	}
	for _, o := range inv.Excluded() {
		rule, _, _ := strings.Cut(o.Reason, ":")
		r.ExcludedByRule[rule]++
		if withExcluded {
			r.Excluded = append(r.Excluded, excludedReport{Object: o.Ref.String(), Reason: o.Reason})
		}
	}
	r.Warnings = allWarnings(inv)
	return r
}

func allWarnings(inv *model.Inventory) []string {
	out := append([]string(nil), inv.Warnings...)
	for _, rel := range inv.Releases {
		if rel.Skip {
			continue
		}
		for _, w := range rel.Warnings {
			out = append(out, "Helm release "+rel.Key()+": "+w)
		}
	}
	for _, o := range inv.Included() {
		for _, w := range o.Warnings {
			out = append(out, o.Ref.String()+": "+w)
		}
	}
	return out
}

func printReport(u *ui.UI, v any, format string) error {
	var b []byte
	var err error
	if format == "json" {
		b, err = json.MarshalIndent(v, "", "  ")
		b = append(b, '\n')
	} else {
		b, err = yaml.Marshal(v)
	}
	if err != nil {
		return err
	}
	u.Printf("%s", b)
	return nil
}

func printScan(u *ui.UI, res *pipeline.Result, cfg *config.Config, showExcluded, showObjects bool) {
	inv := res.Inventory
	u.Printf("Source: %s\n", inv.Source)
	u.Printf("Objects: %d discovered, %s migrated, %d excluded\n", len(inv.Objects), u.Bold(fmt.Sprint(len(inv.Included()))), len(inv.Excluded()))

	u.Heading(fmt.Sprintf("Apps (%d)", len(inv.Apps)))
	apps := append([]*model.App(nil), inv.Apps...)
	sort.SliceStable(apps, func(i, j int) bool {
		if apps[i].Wave != apps[j].Wave {
			return apps[i].Wave < apps[j].Wave
		}
		return apps[i].Name < apps[j].Name
	})
	var rows [][]string
	for _, a := range apps {
		typ := "manifests"
		if a.Release != nil {
			typ = fmt.Sprintf("helm: %s %s (%s)", a.Release.Chart.Name, a.Release.Chart.Version, a.Release.Mode)
		}
		rows = append(rows, []string{fmt.Sprint(a.Wave), a.Name, typ, orDash(a.Namespace), fmt.Sprint(len(a.Objects)), orDash(strings.Join(a.DependsOn, ", "))})
	}
	u.Table([]string{"Wave", "App", "Type", "Namespace", "Objects", "Depends on"}, rows)
	if showObjects {
		for _, a := range apps {
			u.Printf("\n%s\n", u.Bold(a.Name))
			for _, o := range a.Objects {
				u.Printf("  %s\n", o.Ref)
			}
		}
	}

	if len(inv.Releases) > 0 {
		u.Heading(fmt.Sprintf("Helm releases (%d)", len(inv.Releases)))
		rows = nil
		for _, rel := range inv.Releases {
			mode, repo := string(rel.Mode), "—"
			if rel.Repo != nil {
				repo = rel.Repo.URL + " " + u.Dim("("+rel.Repo.Source+")")
			}
			if rel.Skip {
				mode = u.Dim("skipped: " + rel.SkipReason)
			}
			rows = append(rows, []string{rel.Key(), rel.Chart.Name, rel.Chart.Version, mode, repo})
		}
		u.Table([]string{"Release", "Chart", "Version", "Mode", "Repository"}, rows)
	}

	excluded := inv.Excluded()
	if len(excluded) > 0 {
		u.Heading(fmt.Sprintf("Excluded (%d)", len(excluded)))
		counts := map[string]int{}
		for _, o := range excluded {
			rule, _, _ := strings.Cut(o.Reason, ":")
			counts[rule]++
		}
		rules := make([]string, 0, len(counts))
		for r := range counts {
			rules = append(rules, r)
		}
		sort.Slice(rules, func(i, j int) bool {
			return counts[rules[i]] > counts[rules[j]] || counts[rules[i]] == counts[rules[j]] && rules[i] < rules[j]
		})
		rows = nil
		for _, r := range rules {
			rows = append(rows, []string{r, fmt.Sprint(counts[r])})
		}
		u.Table([]string{"Rule", "Objects"}, rows)
		if showExcluded {
			rows = nil
			for _, o := range excluded {
				rows = append(rows, []string{o.Ref.String(), o.Reason})
			}
			u.Table([]string{"Object", "Reason"}, rows)
		} else {
			u.Println(u.Dim("  --show-excluded lists every object; `gitopsify rules` explains the rules"))
		}
	}

	if filter.SelectionMode(cfg) {
		var selected []string
		for _, o := range inv.Objects {
			if o.Selected {
				selected = append(selected, o.Ref.String())
			}
		}
		u.Heading("Selected")
		for _, s := range selected {
			u.Printf("  %s\n", s)
		}
		if len(selected) == 0 {
			u.Warn("nothing matched --resource/--selector")
		}
	}

	warnings := allWarnings(inv)
	if len(warnings) > 0 {
		u.Heading(fmt.Sprintf("Warnings (%d)", len(warnings)))
		for i, w := range warnings {
			if i == 15 {
				u.Printf("  … %d more (see MIGRATION_REPORT.md after generate, or --format yaml)\n", len(warnings)-i)
				break
			}
			u.Printf("  %s %s\n", u.Colorize("warning", "!"), w)
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
