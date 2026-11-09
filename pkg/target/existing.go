package target

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/group"
	"github.com/hayone1/gitopsify/pkg/kube"
	"github.com/hayone1/gitopsify/pkg/model"
)

// MarkExisting leaves out of an environment the objects that already exist
// in its cluster, as cfg.Adopt.Existing says: all of them (skip), or those
// another tool manages there (skip-owned). It marks them in
// model.Object.Skip and returns what it did, for the report. Objects a
// Helm chart renders can't be left out one by one: they're reported.
func MarkExisting(ctx context.Context, inv *model.Inventory, cfg *config.Config, env *config.Environment, c *kube.Client) ([]string, error) {
	mode := cfg.Adopt.Existing
	if mode == "" || mode == config.ExistingInclude {
		return nil, nil
	}
	objs, err := Objects(inv, cfg, env)
	if err != nil {
		return nil, err
	}
	own := ownTools(inv, cfg, env)
	type found struct {
		exists bool
		owner  string
		mine   bool
		err    error
	}
	results := make([]found, len(objs))
	parallel(ctx, len(objs), func(i int) {
		u := objs[i].Object.DeepCopy()
		live, err := lookup(ctx, c, u, objs[i].DefaultNamespace)
		// A SealedSecret or ExternalSecret makes a Secret of the same name:
		// one that is already there counts as the object being there.
		if err == nil && live == nil && secretMaker(u) {
			twin := &unstructured.Unstructured{}
			twin.SetAPIVersion("v1")
			twin.SetKind("Secret")
			twin.SetNamespace(u.GetNamespace())
			twin.SetName(u.GetName())
			live, err = lookup(ctx, c, twin, objs[i].DefaultNamespace)
		}
		if err != nil {
			results[i].err = err
			return
		}
		if live != nil {
			owner, mine := ownership(live, own)
			results[i] = found{exists: true, owner: owner, mine: mine}
		}
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var skipped, chart []string
	for i, eo := range objs {
		r := results[i]
		if r.err != nil {
			return nil, fmt.Errorf("environment %s: reading %s: %w", env.Name, refString(eo.Object), r.err)
		}
		// This migration's own apps manage it (a regeneration after the
		// first sync): it stays, whatever the mode.
		if !r.exists || r.mine || (mode == config.ExistingSkipOwned && r.owner == "") {
			continue
		}
		why := "exists in the cluster"
		if r.owner != "" {
			why = "managed there by " + r.owner
		}
		if eo.FromChart || eo.Source == nil {
			chart = append(chart, fmt.Sprintf("%s (%s, rendered by chart %s)", refString(eo.Object), why, eo.App.Name))
			continue
		}
		if eo.Source.Skip == nil {
			eo.Source.Skip = map[string]string{}
		}
		eo.Source.Skip[env.Name] = why
		skipped = append(skipped, refString(eo.Object)+": "+why)
	}
	sort.Strings(skipped)
	var notes []string
	if len(skipped) > 0 {
		notes = append(notes, fmt.Sprintf("environment %s: left out %d objects that already exist in its cluster (adopt.existing: %s): %s",
			env.Name, len(skipped), mode, strings.Join(skipped, "; ")))
	}
	if len(chart) > 0 {
		sort.Strings(chart)
		notes = append(notes, fmt.Sprintf("environment %s: %d objects of Helm charts already exist in its cluster and stay in their charts (a chart's objects can't be left out one by one): %s",
			env.Name, len(chart), strings.Join(chart, "; ")))
	}
	return notes, nil
}

// ownTools are the owners that are this migration's own: the Argo CD
// Applications, Flux Kustomizations and HelmReleases it generates for the
// environment, and the Helm releases it migrates (a regeneration after a
// takeover finds its own apps managing the objects).
func ownTools(inv *model.Inventory, cfg *config.Config, env *config.Environment) map[string]bool {
	own := map[string]bool{}
	for _, a := range inv.Apps {
		own["Argo CD application "+group.Name(env.Name+"-"+a.Name)] = true
		own["Flux Kustomization "+cfg.Output.Flux.Namespace+"/"+a.Name] = true
		if a.Release != nil {
			own["Flux HelmRelease "+a.Release.Namespace+"/"+a.Name] = true
			own["Helm release "+a.Release.Key()] = true
		}
	}
	for _, r := range inv.Releases {
		own["Helm release "+r.Key()] = true
	}
	// The single pattern's one Application, Argo CD's root, and Flux's.
	own["Argo CD application "+group.Name(env.Name+"-all")] = true
	own["Argo CD application "+group.Name("gitopsify-"+env.Name)] = true
	own["Flux Kustomization "+cfg.Output.Flux.Namespace+"/flux-system"] = true
	return own
}

// lookup reads the live version of an object; nil when it isn't there,
// also when its kind isn't served yet (a CRD the migration installs).
func lookup(ctx context.Context, c *kube.Client, u *unstructured.Unstructured, def string) (*unstructured.Unstructured, error) {
	if err := defaultNamespace(c, u, def); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, err
	}
	live, err := c.Get(ctx, u)
	if meta.IsNoMatchError(err) {
		return nil, nil
	}
	return live, err
}

func secretMaker(u *unstructured.Unstructured) bool {
	switch u.GroupVersionKind().GroupKind().String() {
	case "SealedSecret.bitnami.com", "ExternalSecret.external-secrets.io":
		return true
	}
	return false
}

// ownership describes who manages a live object, and whether that is this
// migration itself: its Argo CD apps (tracked by annotation, by label, or
// in another namespace as ns_app), Flux objects or Helm releases.
func ownership(live *unstructured.Unstructured, own map[string]bool) (string, bool) {
	owner := ownerOf(live)
	if owner == "" {
		owner = otherOwner(live)
	}
	if app, ok := strings.CutPrefix(owner, "Argo CD application "); ok {
		if _, name, ok := strings.Cut(app, "_"); ok { // apps in any namespace
			app = name
		}
		if own["Argo CD application "+app] {
			return owner, true
		}
	}
	// Argo CD's label tracking (the default before 3.0) uses the
	// app.kubernetes.io/instance label, which charts set to the release.
	if inst := live.GetLabels()["app.kubernetes.io/instance"]; inst != "" && own["Argo CD application "+inst] {
		return "Argo CD application " + inst, true
	}
	return owner, own[owner]
}

// otherOwner describes managers ownerOf doesn't know: kapp, Config Sync,
// and controllers that own the object (operators).
func otherOwner(u *unstructured.Unstructured) string {
	a, l := u.GetAnnotations(), u.GetLabels()
	switch {
	case l["argocd.argoproj.io/instance"] != "": // Argo CD's label tracking
		return "Argo CD application " + l["argocd.argoproj.io/instance"]
	case l["kapp.k14s.io/app"] != "":
		return "kapp app " + l["kapp.k14s.io/app"]
	case a["configsync.gke.io/resource-id"] != "" || a["configmanagement.gke.io/managed"] == "enabled":
		return "Config Sync"
	}
	for _, ref := range u.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			return ref.Kind + " " + ref.Name
		}
	}
	return ""
}
