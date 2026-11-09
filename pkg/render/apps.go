package render

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"sort"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/helm"
	"github.com/hayone1/gitopsify/pkg/model"
	"github.com/hayone1/gitopsify/pkg/secrets"
	"github.com/hayone1/gitopsify/pkg/transform"
	"github.com/hayone1/gitopsify/pkg/yamlutil"
)

func (r *renderer) renderApp(l layout, a *model.App) error {
	if a.IsHelm() {
		return r.renderHelmApp(l, a)
	}
	objs := r.appObjects(a)
	if !l.overlays {
		return r.renderFlatApp(l, a, objs, l.envs[0])
	}
	baseDir := l.appBase(a.Name)
	var resources []string
	for _, ao := range objs {
		if ao.out == nil {
			continue // differs per environment; lives in the overlays
		}
		if err := r.writeObject(l.file(baseDir, ao.file), ao.o, ao.out, ""); err != nil {
			return err
		}
		resources = append(resources, ao.file)
	}
	if err := r.tree.AddYAML(l.file(baseDir, "kustomization.yaml"), kustomization(resources, nil)); err != nil {
		return err
	}
	for _, env := range l.envs {
		if err := r.renderOverlay(l, a, objs, env); err != nil {
			return err
		}
	}
	return nil
}

// renderFlatApp writes an app for a single environment, with that
// environment's transforms applied directly.
func (r *renderer) renderFlatApp(l layout, a *model.App, objs []*appObject, env config.Environment) error {
	dir := l.appEnv(a.Name, env.Name)
	var resources []string
	for _, ao := range objs {
		if _, skip := ao.o.Skip[env.Name]; skip {
			continue
		}
		u := ao.out
		if e, ok := ao.o.EnvOut[env.Name]; ok {
			u = r.decorate(e.DeepCopy(), a)
		} else if u != nil {
			u = u.DeepCopy()
			if _, err := transform.Object(u, ao.o, env.Transforms); err != nil {
				return err
			}
		}
		if u == nil {
			continue
		}
		if err := r.writeObject(l.file(dir, ao.file), ao.o, u, env.Name); err != nil {
			return err
		}
		resources = append(resources, ao.file)
	}
	return r.tree.AddYAML(l.file(dir, "kustomization.yaml"), kustomization(resources, nil))
}

// renderOverlay writes an environment overlay: the base plus patches for
// what the environment's transforms change. When a transform changes an
// object's identity (namespace, name) or an encrypted Secret, a patch cannot
// express it, so the overlay holds full copies of the app's objects instead.
func (r *renderer) renderOverlay(l layout, a *model.App, objs []*appObject, env config.Environment) error {
	dir := l.appEnv(a.Name, env.Name)
	type envObject struct {
		ao      *appObject
		u       *unstructured.Unstructured
		changed bool
	}
	var list []envObject
	var gone []*appObject // in the base, left out of this environment
	full := false
	for _, ao := range objs {
		if _, skip := ao.o.Skip[env.Name]; skip {
			if ao.out != nil {
				gone = append(gone, ao)
			}
			continue
		}
		if e, ok := ao.o.EnvOut[env.Name]; ok {
			list = append(list, envObject{ao: ao, u: r.decorate(e.DeepCopy(), a)})
			continue
		}
		if ao.out == nil {
			continue
		}
		u := ao.out.DeepCopy()
		changed, err := transform.Object(u, ao.o, env.Transforms)
		if err != nil {
			return err
		}
		if changed && (u.GetName() != ao.out.GetName() || u.GetNamespace() != ao.out.GetNamespace() || u.GetKind() != ao.out.GetKind() || ao.o.Encrypt) {
			full = true
		}
		list = append(list, envObject{ao: ao, u: u, changed: changed})
	}

	// Patches first: one kustomize can't express makes full copies.
	type envPatch struct {
		e     envObject
		patch map[string]any
	}
	var made []envPatch
	if !full {
		for _, e := range list {
			if e.ao.out == nil || !e.changed {
				continue
			}
			patch, err := mergePatch(e.ao.out, e.u)
			if err != nil {
				full = true // errNoPatch, or fields the typed object doesn't know
				break
			}
			made = append(made, envPatch{e: e, patch: patch})
		}
	}

	var resources []string
	var patches []any
	if full {
		for _, e := range list {
			if err := r.writeObject(l.file(dir, e.ao.file), e.ao.o, e.u, env.Name); err != nil {
				return err
			}
			resources = append(resources, e.ao.file)
		}
	} else {
		resources = append(resources, relPath(dir, l.appBase(a.Name)))
		for _, e := range list {
			switch {
			case e.ao.out == nil:
				if err := r.writeObject(l.file(dir, e.ao.file), e.ao.o, e.u, env.Name); err != nil {
					return err
				}
				resources = append(resources, e.ao.file)
			}
		}
		for _, m := range made {
			name := "patch-" + m.e.ao.file
			if err := r.tree.AddYAML(l.file(dir, name), m.patch); err != nil {
				return err
			}
			patches = append(patches, map[string]any{"path": name})
		}
		// Objects of the base left out here: kustomize deletes them.
		for _, ao := range gone {
			name := "delete-" + ao.file
			del := map[string]any{"$patch": "delete", "apiVersion": ao.out.GetAPIVersion(), "kind": ao.out.GetKind(),
				"metadata": deleteMeta(ao.out)}
			if err := r.tree.AddYAML(l.file(dir, name), del); err != nil {
				return err
			}
			patches = append(patches, map[string]any{"path": name})
		}
	}
	extra := map[string]any{}
	if len(patches) > 0 {
		extra["patches"] = patches
	}
	return r.tree.AddYAML(l.file(dir, "kustomization.yaml"), kustomization(resources, extra))
}

func deleteMeta(u *unstructured.Unstructured) map[string]any {
	m := map[string]any{"name": u.GetName()}
	if ns := u.GetNamespace(); ns != "" {
		m["namespace"] = ns
	}
	return m
}

// mergePatch returns a patch that turns base into env, identified by the
// object's apiVersion, kind, name and namespace. Kustomize applies it as a
// strategic merge patch for built-in kinds, which merges lists such as
// containers and env by key (removals need its directives), and as a JSON
// merge patch for custom resources (lists replaced whole): the patch is
// made the same way.
func mergePatch(base, env *unstructured.Unstructured) (map[string]any, error) {
	b, err := json.Marshal(base.Object)
	if err != nil {
		return nil, err
	}
	e, err := json.Marshal(env.Object)
	if err != nil {
		return nil, err
	}
	var p []byte
	typed, terr := scheme.Scheme.New(base.GroupVersionKind())
	if terr == nil {
		p, err = strategicpatch.CreateTwoWayMergePatch(b, e, typed)
	} else {
		p, err = jsonpatch.CreateMergePatch(b, e)
	}
	if err != nil {
		return nil, err
	}
	patch := map[string]any{}
	if err := json.Unmarshal(p, &patch); err != nil {
		return nil, err
	}
	if terr == nil {
		// kustomize (kyaml) applies $patch: delete and nulls, not the other
		// directives: an order hint is dropped (the base's order stays),
		// and a patch that needs the rest is written as full copies.
		if !kustomizable(patch) {
			return nil, errNoPatch
		}
	}
	patch["apiVersion"] = base.GetAPIVersion()
	patch["kind"] = base.GetKind()
	meta, _ := patch["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["name"] = base.GetName()
	if base.GetNamespace() != "" {
		meta["namespace"] = base.GetNamespace()
	}
	patch["metadata"] = meta
	return patch, nil
}

// writeObject writes one object (env's, or the base's when env is empty),
// encrypting it with SOPS when required. Encrypted objects get a
// fingerprint, so an unchanged Secret keeps its ciphertext.
func (r *renderer) writeObject(p string, o *model.Object, u *unstructured.Unstructured, env string) error {
	b, err := yamlutil.Marshal(u.Object)
	if err != nil {
		return err
	}
	if _, sealed := o.EnvOut[env]; sealed && env != "" && o.Fingerprints[env] != "" {
		// What was sealed, and what is around it (sync options, waves).
		r.tree.Fingerprint(p, o.Fingerprints[env]+"-"+secrets.Shape(u))
	}
	if o.Encrypt && u.GetKind() == "Secret" {
		transforms := []any{r.cfg.Transforms}
		for _, e := range r.cfg.Environments {
			transforms = append(transforms, e.Transforms)
		}
		fp := secrets.Fingerprint(r.inv, o, u, "sops", transforms, r.cfg.Secrets.SOPS)
		if b, err = secrets.Encrypt(r.cfg.Secrets.SOPS, b); err != nil {
			return fmt.Errorf("encrypting %s: %w", o.Ref, err)
		}
		if fp != "" {
			r.tree.Fingerprint(p, fp)
		}
	}
	r.tree.Add(p, b)
	return nil
}

// valuesFor returns the release values for an environment: the release's
// values with the global transforms, then the environment's.
func (r *renderer) valuesFor(a *model.App, env *config.Environment) (base, full map[string]any, err error) {
	base, err = transform.Values(a.Release.Values, a, r.cfg.Transforms)
	if err != nil {
		return nil, nil, err
	}
	if env == nil {
		return base, base, nil
	}
	full, err = transform.Values(base, a, env.Transforms)
	return base, full, err
}

func (r *renderer) renderHelmApp(l layout, a *model.App) error {
	rel := a.Release
	if rel.Mode == model.HelmModeVendor {
		files, err := helm.VendorFiles(rel)
		if err != nil {
			return err
		}
		// The chart name and file names come from the release record, which
		// anyone who can create Secrets in the namespace can forge.
		if c := rel.Chart.Name; c == "" || c == "." || c == ".." || strings.ContainsAny(c, "/\\") {
			return fmt.Errorf("helm release %s: chart name %q isn't a valid directory name; exclude the release or use helm.mode: manifests", rel.Key(), c)
		}
		names := make([]string, 0, len(files))
		for n := range files {
			if treePath(n) != nil {
				return fmt.Errorf("helm release %s: chart file %q isn't a path inside the chart; exclude the release or use helm.mode: manifests", rel.Key(), n)
			}
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			r.tree.Add(l.file(l.chartHome(a.Name), rel.Chart.Name, n), files[n])
		}
	}
	switch {
	case r.cfg.Output.Tool == config.ToolFlux:
		return r.fluxHelmApp(l, a)
	case r.cfg.Output.Tool == config.ToolArgoCD && r.cfg.Output.ArgoCD.Pattern != config.PatternSingle:
		return r.argoHelmValues(l, a)
	default:
		return r.kustomizeHelmApp(l, a)
	}
}

// kustomizeHelmApp writes a self-contained Kustomization per environment
// that inflates the chart with `kustomize build --enable-helm`.
func (r *renderer) kustomizeHelmApp(l layout, a *model.App) error {
	rel := a.Release
	for i := range l.envs {
		env := &l.envs[i]
		dir := l.appEnv(a.Name, env.Name)
		_, values, err := r.valuesFor(a, env)
		if err != nil {
			return err
		}
		if err := r.tree.AddYAML(l.file(dir, "values.yaml"), values); err != nil {
			return err
		}
		chart := map[string]any{
			"name":        rel.Chart.Name,
			"version":     rel.Chart.Version,
			"releaseName": rel.Name,
			"namespace":   rel.Namespace,
			"valuesFile":  "values.yaml",
			"includeCRDs": true,
			// helm test hooks would run as plain Pods.
			"skipTests": true,
		}
		extra := map[string]any{"helmCharts": []any{chart}}
		if rel.Mode == model.HelmModeVendor {
			extra["helmGlobals"] = map[string]any{"chartHome": relPath(dir, l.chartHome(a.Name))}
		} else {
			chart["repo"] = rel.Repo.URL
		}
		if r.cfg.Output.Tool == config.ToolArgoCD && config.True(r.cfg.Ordering.Enabled, true) {
			// The single-Application pattern orders by resource sync waves.
			extra["patches"] = []any{map[string]any{
				"target": map[string]any{"name": ".*"},
				"patch":  fmt.Sprintf("apiVersion: v1\nkind: Any\nmetadata:\n  name: any\n  annotations:\n    %s: %q\n", annArgoSyncWave, fmt.Sprint(a.Wave)),
			}}
		}
		if err := r.tree.AddYAML(l.file(dir, "kustomization.yaml"), kustomization(nil, extra)); err != nil {
			return err
		}
	}
	return nil
}

// argoHelmValues writes the values files a multi-source Argo CD Application
// reads: the base values and a per-environment overlay with only the
// differences.
func (r *renderer) argoHelmValues(l layout, a *model.App) error {
	if !l.overlays {
		_, values, err := r.valuesFor(a, &l.envs[0])
		if err != nil {
			return err
		}
		return r.tree.AddYAML(l.file(l.appBase(a.Name), "values.yaml"), values)
	}
	base, _, err := r.valuesFor(a, nil)
	if err != nil {
		return err
	}
	if err := r.tree.AddYAML(l.file(l.appBase(a.Name), "values.yaml"), base); err != nil {
		return err
	}
	for i := range l.envs {
		env := &l.envs[i]
		_, full, err := r.valuesFor(a, env)
		if err != nil {
			return err
		}
		if err := r.tree.AddYAML(l.file(l.appEnv(a.Name, env.Name), "values.yaml"), DiffValues(base, full)); err != nil {
			return err
		}
	}
	return nil
}

// DiffValues returns the smallest values overlay that, merged over base the
// way Helm merges values files, yields full. Removed keys become null, which
// Helm treats as a deletion.
func DiffValues(base, full map[string]any) map[string]any {
	out := map[string]any{}
	for k, fv := range full {
		bv, ok := base[k]
		if !ok {
			out[k] = fv
			continue
		}
		bm, bIsMap := bv.(map[string]any)
		fm, fIsMap := fv.(map[string]any)
		if bIsMap && fIsMap {
			if d := DiffValues(bm, fm); len(d) > 0 {
				out[k] = d
			}
			continue
		}
		if !reflect.DeepEqual(normalize(bv), normalize(fv)) {
			out[k] = fv
		}
	}
	for k := range base {
		if _, ok := full[k]; !ok {
			out[k] = nil
		}
	}
	return out
}

func normalize(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

// chartRef describes where a Helm app's chart comes from, for the GitOps
// tool objects.
type chartRef struct {
	vendored bool
	oci      bool
	repoURL  string
	chart    string
	version  string
	// chartPath is the vendored chart directory relative to the layout root.
	chartPath string
}

func (r *renderer) chartRef(l layout, a *model.App) chartRef {
	rel := a.Release
	c := chartRef{chart: rel.Chart.Name, version: rel.Chart.Version}
	if rel.Mode == model.HelmModeVendor {
		c.vendored = true
		c.chartPath = path.Join(l.chartHome(a.Name), rel.Chart.Name)
		return c
	}
	c.repoURL = rel.Repo.URL
	c.oci = rel.Repo.IsOCI()
	return c
}

// errNoPatch is an environment change kustomize can't apply as a patch.
var errNoPatch = errors.New("not expressible as a kustomize patch")

// kustomizable drops $setElementOrder directives from a strategic merge
// patch (kustomize would copy them into the object) and reports whether
// the patch uses only what kustomize applies.
func kustomizable(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			switch {
			case strings.HasPrefix(k, "$setElementOrder/"):
				delete(t, k)
			case strings.HasPrefix(k, "$deleteFromPrimitiveList/"), k == "$retainKeys":
				return false
			case !kustomizable(c):
				return false
			}
		}
	case []any:
		for _, c := range t {
			if !kustomizable(c) {
				return false
			}
		}
	}
	return true
}
