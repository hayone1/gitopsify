package render

import (
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"sort"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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
		if err := r.writeObject(l.file(baseDir, ao.file), ao.o, ao.out); err != nil {
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
		if err := r.writeObject(l.file(dir, ao.file), ao.o, u); err != nil {
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
	full := false
	for _, ao := range objs {
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

	var resources []string
	var patches []any
	if full {
		for _, e := range list {
			if err := r.writeObject(l.file(dir, e.ao.file), e.ao.o, e.u); err != nil {
				return err
			}
			resources = append(resources, e.ao.file)
		}
	} else {
		resources = append(resources, relPath(dir, l.appBase(a.Name)))
		for _, e := range list {
			switch {
			case e.ao.out == nil:
				if err := r.writeObject(l.file(dir, e.ao.file), e.ao.o, e.u); err != nil {
					return err
				}
				resources = append(resources, e.ao.file)
			case e.changed:
				patch, err := mergePatch(e.ao.out, e.u)
				if err != nil {
					return fmt.Errorf("computing the %s patch of %s: %w", env.Name, e.ao.o.Ref, err)
				}
				name := "patch-" + e.ao.file
				if err := r.tree.AddYAML(l.file(dir, name), patch); err != nil {
					return err
				}
				patches = append(patches, map[string]any{"path": name})
			}
		}
	}
	extra := map[string]any{}
	if len(patches) > 0 {
		extra["patches"] = patches
	}
	return r.tree.AddYAML(l.file(dir, "kustomization.yaml"), kustomization(resources, extra))
}

// mergePatch returns a patch that turns base into env, identified by the
// object's apiVersion, kind, name and namespace. Kustomize applies it as a
// strategic merge patch for built-in kinds and as a JSON merge patch for
// custom resources; both treat this content the same way.
func mergePatch(base, env *unstructured.Unstructured) (map[string]any, error) {
	b, err := json.Marshal(base.Object)
	if err != nil {
		return nil, err
	}
	e, err := json.Marshal(env.Object)
	if err != nil {
		return nil, err
	}
	p, err := jsonpatch.CreateMergePatch(b, e)
	if err != nil {
		return nil, err
	}
	patch := map[string]any{}
	if err := json.Unmarshal(p, &patch); err != nil {
		return nil, err
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

// writeObject writes one object, encrypting it with SOPS when required.
func (r *renderer) writeObject(p string, o *model.Object, u *unstructured.Unstructured) error {
	b, err := yamlutil.Marshal(u.Object)
	if err != nil {
		return err
	}
	if o.Encrypt && u.GetKind() == "Secret" {
		if b, err = secrets.Encrypt(r.cfg.Secrets.SOPS, b); err != nil {
			return fmt.Errorf("encrypting %s: %w", o.Ref, err)
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
		names := make([]string, 0, len(files))
		for n := range files {
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
