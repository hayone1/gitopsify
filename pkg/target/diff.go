package target

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/helm"
	"github.com/hayone1/gitopsify/pkg/kube"
	"github.com/hayone1/gitopsify/pkg/model"
	"github.com/hayone1/gitopsify/pkg/transform"
	"github.com/hayone1/gitopsify/pkg/yamlutil"
)

// Diff actions.
const (
	ActionCreate    = "create"
	ActionUpdate    = "update"
	ActionUnchanged = "unchanged"
	ActionError     = "error"
)

// Change is the effect the first sync would have on one object.
type Change struct {
	App     string `json:"app"`
	Object  string `json:"object"`
	Action  string `json:"action"`
	Diff    string `json:"diff,omitempty"`
	Message string `json:"message,omitempty"`
}

// DiffOptions tune Diff.
type DiffOptions struct {
	// HelmTemplate renders Helm apps with `helm template` and the generated
	// values. When false, or when helm isn't installed, the release's
	// recorded manifest is used.
	HelmTemplate bool
	// FieldManager used for the dry-run (default "gitopsify-diff").
	FieldManager string
}

// Diff server-side dry-runs every object of an environment against its
// cluster and reports what the first sync would create or change.
func Diff(ctx context.Context, inv *model.Inventory, cfg *config.Config, env *config.Environment, c *kube.Client, o DiffOptions) ([]Change, []string, error) {
	var notes []string
	objs, err := Objects(inv, cfg, env)
	if err != nil {
		return nil, nil, err
	}
	if o.HelmTemplate {
		if _, err := exec.LookPath("helm"); err != nil {
			notes = append(notes, "helm is not installed; Helm apps are compared using their recorded release manifests")
		} else {
			objs, notes = templateHelmApps(ctx, inv, cfg, env, objs, notes)
		}
	}
	fm := o.FieldManager
	if fm == "" {
		fm = "gitopsify-diff"
	}
	migrated := migratedKinds(inv)
	changes := make([]Change, len(objs))
	errs := make([]error, len(objs))
	parallel(ctx, len(objs), func(i int) {
		changes[i], errs[i] = diffObject(ctx, c, objs[i], fm, migrated)
	})
	for _, err := range errs {
		if err != nil {
			return nil, nil, err
		}
	}
	return changes, notes, nil
}

// diffObject dry-runs one object and compares the result with the live one.
func diffObject(ctx context.Context, c *kube.Client, eo EnvObject, fm string, migrated map[schema.GroupKind]string) (Change, error) {
	u := eo.Object.DeepCopy()
	ch := Change{App: eo.App.Name}
	if err := defaultNamespace(c, u, eo.DefaultNamespace); err != nil {
		ch.Object = refString(u)
		if meta.IsNoMatchError(err) {
			ch.Action = ActionCreate
			ch.Message = "kind not served yet"
			if app := migrated[u.GroupVersionKind().GroupKind()]; app != "" {
				ch.Message += "; its CRD is installed by app " + app
			}
		} else {
			ch.Action, ch.Message = ActionError, err.Error()
		}
		return ch, nil
	}
	ch.Object = refString(u)
	live, err := c.Get(ctx, u)
	if err != nil {
		ch.Action, ch.Message = ActionError, c.Explain(err).Error()
		return ch, nil
	}
	if live == nil {
		ch.Action = ActionCreate
		if _, err := c.Apply(ctx, u, kube.ApplyOptions{DryRun: true, FieldManager: fm, Force: true}); err != nil && !apierrors.IsNotFound(err) {
			ch.Message = "dry-run failed: " + err.Error()
		}
		return ch, nil
	}
	result, err := c.Apply(ctx, u, kube.ApplyOptions{DryRun: true, FieldManager: fm, Force: true})
	if err != nil {
		ch.Action, ch.Message = ActionError, "dry-run failed: "+err.Error()
		return ch, nil
	}
	d, err := unifiedDiff(live, result, ch.Object)
	if err != nil {
		return ch, err
	}
	if d == "" {
		ch.Action = ActionUnchanged
	} else {
		ch.Action, ch.Diff = ActionUpdate, d
	}
	return ch, nil
}

// parallel calls fn for 0..n-1 with a few calls in flight, which keeps
// per-object API round trips fast against remote clusters.
func parallel(ctx context.Context, n int, fn func(i int)) {
	g, _ := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for i := 0; i < n; i++ {
		g.Go(func() error {
			if ctx.Err() == nil {
				fn(i)
			}
			return nil
		})
	}
	_ = g.Wait()
}

// normalizeForDiff drops fields that change on every write.
func normalizeForDiff(u *unstructured.Unstructured) map[string]any {
	c := u.DeepCopy()
	for _, f := range []string{"managedFields", "resourceVersion", "generation", "uid", "creationTimestamp", "selfLink"} {
		unstructured.RemoveNestedField(c.Object, "metadata", f)
	}
	delete(c.Object, "status")
	return c.Object
}

func unifiedDiff(live, desired *unstructured.Unstructured, name string) (string, error) {
	a, err := yamlutil.Marshal(normalizeForDiff(live))
	if err != nil {
		return "", err
	}
	b, err := yamlutil.Marshal(normalizeForDiff(desired))
	if err != nil {
		return "", err
	}
	if bytes.Equal(a, b) {
		return "", nil
	}
	return difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(a)),
		B:        difflib.SplitLines(string(b)),
		FromFile: "live/" + name,
		ToFile:   "git/" + name,
		Context:  3,
	})
}

// templateHelmApps replaces the release manifests of Helm apps with the
// output of `helm template` for the values gitopsify generated.
func templateHelmApps(ctx context.Context, inv *model.Inventory, cfg *config.Config, env *config.Environment, objs []EnvObject, notes []string) ([]EnvObject, []string) {
	rendered := map[string][]*unstructured.Unstructured{}
	for _, a := range inv.Apps {
		if !a.IsHelm() {
			continue
		}
		us, err := helmTemplate(ctx, a, cfg, env)
		if err != nil {
			notes = append(notes, fmt.Sprintf("app %s: helm template failed (%v); using the recorded release manifest", a.Name, err))
			continue
		}
		rendered[a.Name] = us
	}
	var out []EnvObject
	done := map[string]bool{}
	for _, eo := range objs {
		us, ok := rendered[eo.App.Name]
		if !eo.FromChart || !ok {
			out = append(out, eo)
			continue
		}
		if done[eo.App.Name] {
			continue
		}
		done[eo.App.Name] = true
		for _, u := range us {
			out = append(out, EnvObject{App: eo.App, Object: u, FromChart: true, DefaultNamespace: eo.DefaultNamespace})
		}
	}
	return out, notes
}

func helmTemplate(ctx context.Context, a *model.App, cfg *config.Config, env *config.Environment) ([]*unstructured.Unstructured, error) {
	rel := a.Release
	base, err := transform.Values(rel.Values, a, cfg.Transforms)
	if err != nil {
		return nil, err
	}
	values, err := transform.Values(base, a, env.Transforms)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "gitopsify-helm-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	vb, err := yamlutil.Marshal(values)
	if err != nil {
		return nil, err
	}
	valuesFile := filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(valuesFile, vb, 0o600); err != nil {
		return nil, err
	}
	args := []string{"template", rel.Name, "--namespace", rel.Namespace, "--include-crds", "--skip-tests", "-f", valuesFile}
	switch rel.Mode {
	case model.HelmModeVendor:
		files, err := helm.VendorFiles(rel)
		if err != nil {
			return nil, err
		}
		chartDir := filepath.Join(dir, "chart")
		for name, data := range files {
			p := filepath.Join(chartDir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(p, data, 0o644); err != nil {
				return nil, err
			}
		}
		args = append(args, chartDir)
	default:
		if rel.Repo.IsOCI() {
			args = append(args, strings.TrimSuffix(rel.Repo.URL, "/")+"/"+rel.Chart.Name, "--version", rel.Chart.Version)
		} else {
			args = append(args, rel.Chart.Name, "--repo", rel.Repo.URL, "--version", rel.Chart.Version)
		}
	}
	cmd := exec.CommandContext(ctx, "helm", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	// Hooks run during a sync and don't persist as objects to compare.
	var objs []*unstructured.Unstructured
	for _, u := range helm.ManifestObjects(string(out)) {
		if _, hook := u.GetAnnotations()["helm.sh/hook"]; !hook {
			objs = append(objs, u)
		}
	}
	return objs, nil
}
