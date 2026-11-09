package filter

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/match"
	"github.com/hayone1/gitopsify/pkg/model"
)

// Reason prefixes. Objects excluded only by scope can be pulled back in as
// dependencies of selected objects; rule exclusions are final.
const (
	ScopeRule = "scope"
	HelmRule  = "helm-release"
)

// IsScopeExclusion reports whether an object is excluded only by scope.
func IsScopeExclusion(o *model.Object) bool {
	return o.Excluded && strings.HasPrefix(o.Reason, ScopeRule+":")
}

// ResourceSpec is a parsed --resource value.
type ResourceSpec struct {
	Kind      string
	Namespace string
	Name      string
	// Helm is set for "helm/<namespace>/<name>", which selects a release.
	Helm bool
}

// ParseResourceSpec parses "kind[.group]/[namespace/]name" or
// "helm/namespace/name".
func ParseResourceSpec(s string) (ResourceSpec, error) {
	parts := strings.Split(s, "/")
	switch len(parts) {
	case 2:
		if parts[0] == "" || parts[1] == "" {
			break
		}
		return ResourceSpec{Kind: parts[0], Name: parts[1]}, nil
	case 3:
		if parts[0] == "" || parts[1] == "" || parts[2] == "" {
			break
		}
		k := strings.ToLower(parts[0])
		return ResourceSpec{Kind: parts[0], Namespace: parts[1], Name: parts[2], Helm: k == "helm" || k == "helmrelease"}, nil
	}
	return ResourceSpec{}, fmt.Errorf("resource %q must look like kind/name, kind/namespace/name or helm/namespace/name", s)
}

// SelectionMode reports whether the scope selects specific objects instead
// of whole namespaces.
func SelectionMode(cfg *config.Config) bool {
	return len(cfg.Scope.Resources) > 0 || cfg.Scope.LabelSelector != ""
}

// NarrowScope reports whether only part of the cluster is migrated, in which
// case cluster-scoped objects are kept only when something references them.
func NarrowScope(cfg *config.Config) bool {
	return SelectionMode(cfg) || len(cfg.Scope.Namespaces) > 0
}

// Options configure Apply.
type Options struct {
	Config *config.Config
	// KindInfo returns the kind information used to match kind patterns,
	// including short names. Defaults to match.InfoOf.
	KindInfo func(*model.Object) match.KindInfo
}

// Releases decides which Helm releases are migrated.
func Releases(inv *model.Inventory, cfg *config.Config) error {
	disabled := disabledSet(cfg)
	specs, err := resourceSpecs(cfg)
	if err != nil {
		return err
	}
	for _, r := range inv.Releases {
		ov := cfg.Helm.Releases[r.Key()]
		for _, s := range specs {
			if s.Helm && s.Namespace == r.Namespace && s.Name == r.Name {
				r.Selected = true
			}
		}
		switch {
		case ov.Skip:
			r.Skip, r.SkipReason = true, "skipped in helm.releases"
		case ov.Include:
		case !inNamespaceScope(cfg, r.Namespace):
			r.Skip, r.SkipReason = true, "namespace not in scope"
		case !disabled["system-namespaces"] && match.Glob(SystemNamespaces, r.Namespace):
			r.Skip, r.SkipReason = true, "installed in system namespace "+r.Namespace+` (set helm.releases["`+r.Key()+`"].include to migrate it)`
		case !disabled["gitops-namespaces"] && match.Glob(GitOpsNamespaces, r.Namespace):
			r.Skip, r.SkipReason = true, "GitOps tooling namespace "+r.Namespace
		case !cfg.Scope.IncludeAlreadyManaged && !disabled["already-gitops-managed"] && fluxManaged(inv, r):
			r.Skip, r.SkipReason = true, "managed by a Flux HelmRelease in the source cluster"
		}
	}
	return nil
}

// fluxManaged reports whether a release's objects carry helm-controller's
// labels.
func fluxManaged(inv *model.Inventory, r *model.HelmRelease) bool {
	for _, o := range inv.Objects {
		if o.HelmRelease == r.Key() && o.Live.GetLabels()["helm.toolkit.fluxcd.io/name"] != "" {
			return true
		}
	}
	return false
}

// Apply decides, for every object, whether it is migrated. It must run after
// Releases.
func Apply(inv *model.Inventory, opts Options) error {
	cfg := opts.Config
	info := opts.KindInfo
	if info == nil {
		info = match.InfoOf
	}
	disabled := disabledSet(cfg)
	rules := DefaultRules()
	specs, err := resourceSpecs(cfg)
	if err != nil {
		return err
	}
	var sel labels.Selector
	if cfg.Scope.LabelSelector != "" {
		if sel, err = labels.Parse(cfg.Scope.LabelSelector); err != nil {
			return fmt.Errorf("scope.labelSelector: %w", err)
		}
	}
	selection := SelectionMode(cfg)

	for _, o := range inv.Objects {
		o.Include()

		// 1. Objects of a Helm release follow the release.
		if o.HelmRelease != "" {
			if r := inv.Release(o.HelmRelease); r != nil {
				if r.Skip {
					o.Exclude(HelmRule, "release "+r.Key()+" not migrated ("+r.SkipReason+")")
				} else if selection && !r.Selected {
					o.Exclude(ScopeRule, "not selected (part of Helm release "+r.Key()+")")
				}
				continue
			}
		}

		// 2. User rules, first match wins.
		decided := false
		for i, r := range cfg.Scope.Rules {
			ok, err := match.Selector(r.Selector, o, "")
			if err != nil {
				return fmt.Errorf("scope.rules[%d]: %w", i, err)
			}
			if !ok {
				continue
			}
			decided = true
			if r.Action == "exclude" {
				name := r.Name
				if name == "" {
					name = fmt.Sprintf("rules[%d]", i)
				}
				o.Exclude("user rule "+name, r.Reason)
			}
			break
		}
		if decided {
			continue
		}

		// 3. Built-in rules.
		for _, r := range rules {
			if disabled[r.Name] || (r.Name == "already-gitops-managed" && cfg.Scope.IncludeAlreadyManaged) {
				continue
			}
			if ok, why := r.Match(o, inv); ok {
				o.Exclude(r.Name, why)
				break
			}
		}
		if o.Excluded {
			continue
		}

		// 4. Scope.
		k := info(o)
		ns := o.Ref.Namespace
		if o.IsNamespace() {
			ns = o.Ref.Name
		}
		switch {
		case len(cfg.Scope.Kinds) > 0 && !match.AnyKind(cfg.Scope.Kinds, k):
			o.Exclude(ScopeRule, "kind not selected")
		case match.AnyKind(cfg.Scope.ExcludeKinds, k):
			o.Exclude(ScopeRule, "kind excluded")
		case ns != "" && !inNamespaceScope(cfg, ns):
			o.Exclude(ScopeRule, "namespace not selected")
		case !o.Namespaced && !o.IsNamespace() && cfg.Scope.ClusterScoped == "none":
			o.Exclude(ScopeRule, "cluster-scoped objects disabled")
		case selection && matchesSpecs(o, specs, sel, k):
			o.Selected = true // named explicitly: cluster-scoped ones too
		case !o.Namespaced && cfg.Scope.ClusterScoped == "auto" && NarrowScope(cfg):
			o.Exclude(ScopeRule, "cluster-scoped and not referenced by migrated objects")
		case selection:
			o.Exclude(ScopeRule, "not selected")
		}
	}

	// In selection mode, a selected object that belongs to a release selects
	// the release; re-evaluate the release's objects.
	if selection {
		changed := false
		for _, o := range inv.Objects {
			if o.HelmRelease == "" || !matchesSpecs(o, specs, sel, info(o)) {
				continue
			}
			if r := inv.Release(o.HelmRelease); r != nil && !r.Skip && !r.Selected {
				r.Selected, changed = true, true
			}
		}
		if changed {
			for _, o := range inv.Objects {
				if r := inv.Release(o.HelmRelease); r != nil && r.Selected && !r.Skip {
					o.Include()
				}
			}
		}
		for _, r := range inv.Releases {
			if !r.Selected && !r.Skip {
				r.Skip, r.SkipReason = true, "not selected"
			}
		}
	}
	return nil
}

func matchesSpecs(o *model.Object, specs []ResourceSpec, sel labels.Selector, k match.KindInfo) bool {
	if sel != nil && o.Namespaced && sel.Matches(labels.Set(o.Live.GetLabels())) {
		return true
	}
	for _, s := range specs {
		if s.Helm || !match.Kind(s.Kind, k) || s.Name != o.Ref.Name {
			continue
		}
		if s.Namespace == "" || s.Namespace == o.Ref.Namespace {
			return true
		}
	}
	return false
}

func inNamespaceScope(cfg *config.Config, ns string) bool {
	if len(cfg.Scope.Namespaces) > 0 && !match.Glob(cfg.Scope.Namespaces, ns) {
		return false
	}
	return !match.Glob(cfg.Scope.ExcludeNamespaces, ns)
}

func disabledSet(cfg *config.Config) map[string]bool {
	d := map[string]bool{}
	for _, n := range cfg.Scope.DisableDefaultRules {
		if n == "*" {
			for _, r := range DefaultRules() {
				d[r.Name] = true
			}
			continue
		}
		d[n] = true
	}
	return d
}

func resourceSpecs(cfg *config.Config) ([]ResourceSpec, error) {
	var out []ResourceSpec
	for _, r := range cfg.Scope.Resources {
		s, err := ParseResourceSpec(r)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// ValidateRuleNames reports unknown names in scope.disableDefaultRules.
func ValidateRuleNames(cfg *config.Config) error {
	known := map[string]bool{"*": true}
	for _, r := range DefaultRules() {
		known[r.Name] = true
	}
	for _, n := range cfg.Scope.DisableDefaultRules {
		if !known[n] {
			return fmt.Errorf("scope.disableDefaultRules: unknown rule %q (see `gitopsify rules`)", n)
		}
	}
	return nil
}
