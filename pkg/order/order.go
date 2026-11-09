// Package order works out the reconciliation order of apps: sync waves for
// Argo CD and dependsOn for Flux.
package order

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/deps"
	"github.com/hayone1/gitopsify/pkg/helm"
	"github.com/hayone1/gitopsify/pkg/match"
	"github.com/hayone1/gitopsify/pkg/model"
)

// generatedCRDs maps kinds gitopsify writes in place of Secrets to the CRDs
// that serve them.
var generatedCRDs = map[string]string{
	"SealedSecret.bitnami.com":           "sealedsecrets.bitnami.com",
	"ExternalSecret.external-secrets.io": "externalsecrets.external-secrets.io",
}

// Run sets the wave and dependencies of every app.
func Run(inv *model.Inventory, cfg *config.Config) error {
	if !config.True(cfg.Ordering.Enabled, true) {
		for _, a := range inv.Apps {
			a.Wave, a.DependsOn = 0, nil
		}
		return nil
	}
	apps := map[string]*model.App{}
	for _, a := range inv.Apps {
		apps[a.Name] = a
	}
	edges := map[string]map[string]bool{}
	addEdge := func(from, to string) {
		if from == to {
			return
		}
		if edges[from] == nil {
			edges[from] = map[string]bool{}
		}
		edges[from][to] = true
	}

	dependers := make([]string, 0, len(cfg.Ordering.DependsOn))
	for app := range cfg.Ordering.DependsOn {
		dependers = append(dependers, app)
	}
	sort.Strings(dependers)
	for _, app := range dependers {
		ds := cfg.Ordering.DependsOn[app]
		matched := false
		for _, a := range inv.Apps {
			if !match.Glob([]string{app}, a.Name) {
				continue
			}
			matched = true
			for _, d := range ds {
				if apps[d] == nil {
					inv.Warn("ordering.dependsOn: %s depends on unknown app %q", a.Name, d)
					continue
				}
				addEdge(a.Name, d)
			}
		}
		if !matched {
			inv.Warn("ordering.dependsOn: no app matches %q", app)
		}
	}

	// Inferred after the explicit ones: an inferred edge that would close a
	// cycle (two apps using each other's custom resources) is left out, so
	// they sync in the same wave, without a dry run, rather than failing.
	if config.True(cfg.Ordering.InferDependencies, true) {
		owner := crdOwners(inv)
		for _, a := range inv.Apps {
			for _, crd := range customResourceCRDs(a) {
				o, ok := owner[crd]
				if !ok || o == a.Name {
					continue
				}
				a.SkipDryRun = true
				if reaches(edges, o, a.Name) {
					inv.Warn("apps %s and %s use each other's custom resources; neither is ordered after the other", a.Name, o)
					continue
				}
				addEdge(a.Name, o)
			}
		}
	}

	// The most specific pattern decides: an exact name, then longer ones
	// ("db-*" before "*"), then alphabetical; never map order.
	patterns := make([]string, 0, len(cfg.Ordering.Waves))
	for p := range cfg.Ordering.Waves {
		patterns = append(patterns, p)
	}
	sort.Slice(patterns, func(i, j int) bool {
		ei, ej := !strings.ContainsAny(patterns[i], "*?["), !strings.ContainsAny(patterns[j], "*?[")
		if ei != ej {
			return ei
		}
		if len(patterns[i]) != len(patterns[j]) {
			return len(patterns[i]) > len(patterns[j])
		}
		return patterns[i] < patterns[j]
	})
	base := map[string]int{}
	pinned := map[string]bool{}
	for _, a := range inv.Apps {
		base[a.Name] = model.PhaseWave[a.Phase]
		for _, pattern := range patterns {
			if match.Glob([]string{pattern}, a.Name) {
				base[a.Name] = cfg.Ordering.Waves[pattern]
				pinned[a.Name] = true
				break
			}
		}
	}

	waves, err := layer(inv.Apps, edges, base)
	if err != nil {
		return err
	}
	for _, a := range inv.Apps {
		a.Wave = waves[a.Name]
		if pinned[a.Name] && a.Wave != base[a.Name] {
			inv.Warn("app %s is pinned to wave %d but depends on apps in wave %d or later; using wave %d", a.Name, base[a.Name], a.Wave-1, a.Wave)
		}
		a.DependsOn = nil
		for d := range edges[a.Name] {
			a.DependsOn = append(a.DependsOn, d)
		}
		sort.Strings(a.DependsOn)
	}
	return nil
}

// layer assigns each app the longest-path wave: at least its base wave and
// one more than every app it depends on.
func layer(apps []*model.App, edges map[string]map[string]bool, base map[string]int) (map[string]int, error) {
	const (
		unvisited = iota
		visiting
		done
	)
	state := map[string]int{}
	waves := map[string]int{}
	var visit func(name string, stack []string) error
	visit = func(name string, stack []string) error {
		switch state[name] {
		case done:
			return nil
		case visiting:
			i := 0
			for i < len(stack) && stack[i] != name {
				i++
			}
			return fmt.Errorf("dependency cycle between apps: %s", strings.Join(append(stack[i:], name), " -> "))
		}
		state[name] = visiting
		w := base[name]
		deps := make([]string, 0, len(edges[name]))
		for d := range edges[name] {
			deps = append(deps, d)
		}
		sort.Strings(deps)
		for _, d := range deps {
			if err := visit(d, append(stack, name)); err != nil {
				return err
			}
			if waves[d]+1 > w {
				w = waves[d] + 1
			}
		}
		waves[name] = w
		state[name] = done
		return nil
	}
	for _, a := range apps {
		if err := visit(a.Name, nil); err != nil {
			return nil, err
		}
	}
	return waves, nil
}

// crdOwners maps CRD names to the app that installs them.
func crdOwners(inv *model.Inventory) map[string]string {
	owner := map[string]string{}
	for _, a := range inv.Apps {
		for _, o := range a.Objects {
			if o.IsCRD() {
				owner[o.Ref.Name] = a.Name
			}
		}
		if a.Release != nil {
			for _, n := range helm.CRDNames(a.Release) {
				owner[n] = a.Name
			}
			for _, u := range helm.ManifestObjects(a.Release.Manifest) {
				if u.GetKind() == "CustomResourceDefinition" {
					owner[u.GetName()] = a.Name
				}
			}
		}
	}
	return owner
}

// customResourceCRDs returns the CRDs that serve the custom resources an app
// writes, including SealedSecrets and ExternalSecrets generated from Secrets.
func customResourceCRDs(a *model.App) []string {
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, o := range a.Objects {
		outs := []*unstructured.Unstructured{o.Out}
		for _, e := range o.EnvOut {
			outs = append(outs, e)
		}
		for _, u := range outs {
			if u == nil {
				continue
			}
			gk := u.GroupVersionKind().GroupKind().String()
			if crd, ok := generatedCRDs[gk]; ok {
				add(crd)
			} else if gk == o.Ref.GroupKind() && deps.IsCustomResource(o) {
				add(deps.CRDName(o))
			}
		}
	}
	sort.Strings(out)
	return out
}

// reaches reports whether from depends on to, directly or through others.
func reaches(edges map[string]map[string]bool, from, to string) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(n string) bool {
		if n == to {
			return true
		}
		if seen[n] {
			return false
		}
		seen[n] = true
		for d := range edges[n] {
			if walk(d) {
				return true
			}
		}
		return false
	}
	return walk(from)
}
