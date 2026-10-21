package render

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/model"
)

// FluxInstallURL returns the upstream install manifest of a Flux version.
func FluxInstallURL(version string) string {
	return "https://github.com/fluxcd/flux2/releases/download/" + version + "/install.yaml"
}

var scpLike = regexp.MustCompile(`^([^@/]+)@([^:/]+):(.+)$`)

// FluxURL converts scp-like Git URLs (git@host:owner/repo.git), which Flux
// does not accept, into ssh:// URLs.
func FluxURL(u string) string {
	if m := scpLike.FindStringSubmatch(u); m != nil {
		return "ssh://" + m[1] + "@" + m[2] + "/" + m[3]
	}
	return u
}

func (r *renderer) renderFlux(l layout) error {
	for _, env := range l.envs {
		if err := r.fluxEnv(l, env); err != nil {
			return fmt.Errorf("environment %s: %w", env.Name, err)
		}
	}
	return nil
}

func (r *renderer) fluxEnv(l layout, env config.Environment) error {
	f := r.cfg.Output.Flux
	clusterDir := path.Join("clusters", env.Name)
	sysDir := path.Join(clusterDir, "flux-system")

	git := map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "GitRepository",
		"metadata": map[string]any{"name": "flux-system", "namespace": f.Namespace},
		"spec": map[string]any{
			"interval":  "1m",
			"url":       FluxURL(l.repoURL),
			"ref":       map[string]any{"branch": r.cfg.Output.Repo.Revision},
			"secretRef": map[string]any{"name": f.SecretName},
		},
	}
	root := map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": "flux-system", "namespace": f.Namespace},
		"spec": map[string]any{
			"interval":  f.Interval,
			"path":      "./" + r.repoPath(clusterDir),
			"prune":     true,
			"sourceRef": map[string]any{"kind": "GitRepository", "name": "flux-system"},
		},
	}
	if err := r.tree.AddYAML(l.file(sysDir, "gotk-sync.yaml"), git, root); err != nil {
		return err
	}
	if err := r.tree.AddYAML(l.file(sysDir, "kustomization.yaml"), kustomization([]string{FluxInstallURL(f.Version), "gotk-sync.yaml"}, nil)); err != nil {
		return err
	}

	// Apps that others depend on are waited for, so dependents only start
	// once they are healthy.
	waited := map[string]bool{}
	deps := r.fluxDependsOn()
	for _, ds := range deps {
		for _, d := range ds {
			waited[d] = true
		}
	}
	for _, app := range r.inv.Apps {
		spec := map[string]any{
			"interval":      f.Interval,
			"retryInterval": "1m",
			"timeout":       "5m",
			"path":          "./" + r.repoPath(l.appEnv(app.Name, env.Name)),
			"prune":         config.True(f.Prune, true),
			"wait":          waited[app.Name],
			"sourceRef":     map[string]any{"kind": "GitRepository", "name": "flux-system", "namespace": f.Namespace},
		}
		if ds := deps[app.Name]; len(ds) > 0 {
			var list []any
			for _, d := range ds {
				list = append(list, map[string]any{"name": d})
			}
			spec["dependsOn"] = list
		}
		if appEncrypts(app) {
			spec["decryption"] = map[string]any{"provider": "sops", "secretRef": map[string]any{"name": r.cfg.Secrets.SOPS.DecryptionSecret}}
		}
		k := map[string]any{
			"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
			"metadata": map[string]any{
				"name": app.Name, "namespace": f.Namespace,
				"labels": map[string]any{LabelWave: WaveLabel(app.Wave)},
			},
			"spec": spec,
		}
		if err := r.tree.AddYAML(l.file(clusterDir, "apps", app.Name+".yaml"), k); err != nil {
			return err
		}
	}
	return nil
}

func appEncrypts(a *model.App) bool {
	for _, o := range a.Objects {
		if o.Encrypt {
			return true
		}
	}
	return false
}

// fluxDependsOn translates waves into Flux dependencies: every app depends on
// its explicit and inferred dependencies and on the apps of the closest
// lower wave, which keeps the order of the waves without listing everything.
func (r *renderer) fluxDependsOn() map[string][]string {
	out := map[string][]string{}
	if !config.True(r.cfg.Ordering.Enabled, true) {
		return out
	}
	byWave := map[int][]string{}
	var waves []int
	for _, a := range r.inv.Apps {
		if _, ok := byWave[a.Wave]; !ok {
			waves = append(waves, a.Wave)
		}
		byWave[a.Wave] = append(byWave[a.Wave], a.Name)
	}
	sort.Ints(waves)
	prev := map[int][]string{}
	for i := 1; i < len(waves); i++ {
		prev[waves[i]] = byWave[waves[i-1]]
	}
	for _, a := range r.inv.Apps {
		set := map[string]bool{}
		for _, d := range a.DependsOn {
			set[d] = true
		}
		for _, d := range prev[a.Wave] {
			set[d] = true
		}
		var list []string
		for d := range set {
			list = append(list, d)
		}
		sort.Strings(list)
		out[a.Name] = list
	}
	return out
}

const fluxKustomizeConfig = `nameReference:
  - kind: ConfigMap
    version: v1
    fieldSpecs:
      - path: spec/valuesFrom/name
        kind: HelmRelease
`

// fluxHelmApp writes a HelmRelease, its chart source and values ConfigMaps
// generated from values.yaml files.
func (r *renderer) fluxHelmApp(l layout, a *model.App) error {
	f := r.cfg.Output.Flux
	rel := a.Release
	c := r.chartRef(l, a)
	baseDir := l.appBase(a.Name)
	valuesName := a.Name + "-values"
	envValuesName := a.Name + "-env-values"

	var resources []string
	spec := map[string]any{
		"interval":    f.Interval,
		"releaseName": rel.Name,
		"install": map[string]any{
			"crds":            "CreateReplace",
			"createNamespace": r.needsCreateNamespace(a),
			"remediation":     map[string]any{"retries": int64(3)},
		},
		"upgrade": map[string]any{
			"crds":        "CreateReplace",
			"remediation": map[string]any{"retries": int64(3)},
		},
	}
	drift := "enabled"
	if r.cfg.Adopt.Enabled {
		drift = "warn"
	}
	spec["driftDetection"] = map[string]any{"mode": drift}
	valuesFrom := []any{map[string]any{"kind": "ConfigMap", "name": valuesName}}
	if l.overlays {
		valuesFrom = append(valuesFrom, map[string]any{"kind": "ConfigMap", "name": envValuesName, "optional": true})
	}
	spec["valuesFrom"] = valuesFrom

	switch {
	case c.vendored:
		spec["chart"] = map[string]any{"spec": map[string]any{
			"chart":             "./" + r.repoPath(c.chartPath),
			"reconcileStrategy": "Revision",
			"sourceRef":         map[string]any{"kind": "GitRepository", "name": "flux-system", "namespace": f.Namespace},
		}}
	case c.oci:
		src := map[string]any{
			"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "OCIRepository",
			"metadata": map[string]any{"name": a.Name, "namespace": rel.Namespace},
			"spec": map[string]any{
				"interval": "1h",
				"url":      strings.TrimSuffix(c.repoURL, "/") + "/" + c.chart,
				"ref":      map[string]any{"tag": c.version},
				"layerSelector": map[string]any{
					"mediaType": "application/vnd.cncf.helm.chart.content.v1.tar+gzip",
					"operation": "copy",
				},
			},
		}
		if err := r.tree.AddYAML(l.file(baseDir, "ocirepository.yaml"), src); err != nil {
			return err
		}
		resources = append(resources, "ocirepository.yaml")
		spec["chartRef"] = map[string]any{"kind": "OCIRepository", "name": a.Name, "namespace": rel.Namespace}
	default:
		src := map[string]any{
			"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "HelmRepository",
			"metadata": map[string]any{"name": a.Name, "namespace": rel.Namespace},
			"spec":     map[string]any{"interval": "1h", "url": c.repoURL},
		}
		if err := r.tree.AddYAML(l.file(baseDir, "helmrepository.yaml"), src); err != nil {
			return err
		}
		resources = append(resources, "helmrepository.yaml")
		spec["chart"] = map[string]any{"spec": map[string]any{
			"chart":     c.chart,
			"version":   c.version,
			"sourceRef": map[string]any{"kind": "HelmRepository", "name": a.Name},
		}}
	}
	hr := map[string]any{
		"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
		"metadata": map[string]any{"name": a.Name, "namespace": rel.Namespace},
		"spec":     spec,
	}
	if err := r.tree.AddYAML(l.file(baseDir, "helmrelease.yaml"), hr); err != nil {
		return err
	}
	resources = append(resources, "helmrelease.yaml")

	var baseValues map[string]any
	var err error
	if l.overlays {
		baseValues, _, err = r.valuesFor(a, nil)
	} else {
		_, baseValues, err = r.valuesFor(a, &l.envs[0])
	}
	if err != nil {
		return err
	}
	if err := r.tree.AddYAML(l.file(baseDir, "values.yaml"), baseValues); err != nil {
		return err
	}
	r.tree.Add(l.file(baseDir, "kustomizeconfig.yaml"), []byte(fluxKustomizeConfig))
	generator := func(name string) []any {
		return []any{map[string]any{
			"name":      name,
			"namespace": rel.Namespace,
			"files":     []any{"values.yaml=values.yaml"},
		}}
	}
	if err := r.tree.AddYAML(l.file(baseDir, "kustomization.yaml"), kustomization(resources, map[string]any{
		"configMapGenerator": generator(valuesName),
		"configurations":     []any{"kustomizeconfig.yaml"},
	})); err != nil {
		return err
	}
	if !l.overlays {
		return nil
	}
	for i := range l.envs {
		env := &l.envs[i]
		dir := l.appEnv(a.Name, env.Name)
		_, full, err := r.valuesFor(a, env)
		if err != nil {
			return err
		}
		if err := r.tree.AddYAML(l.file(dir, "values.yaml"), DiffValues(baseValues, full)); err != nil {
			return err
		}
		r.tree.Add(l.file(dir, "kustomizeconfig.yaml"), []byte(fluxKustomizeConfig))
		if err := r.tree.AddYAML(l.file(dir, "kustomization.yaml"), kustomization([]string{relPath(dir, baseDir)}, map[string]any{
			"configMapGenerator": generator(envValuesName),
			"configurations":     []any{"kustomizeconfig.yaml"},
		})); err != nil {
			return err
		}
	}
	return nil
}
