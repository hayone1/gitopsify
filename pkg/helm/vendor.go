package helm

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/hayone1/gitopsify/pkg/model"
)

// VendorFiles rebuilds the chart directory of a release: Chart.yaml,
// values.yaml, values.schema.json, templates and every other chart file.
// Paths are relative to the chart directory.
func VendorFiles(r *model.HelmRelease) (map[string][]byte, error) {
	if !r.Vendorable() {
		return nil, fmt.Errorf("chart %s of release %s cannot be rebuilt from the release (it has subcharts or no templates)", r.Chart.Name, r.Key())
	}
	files := map[string][]byte{}
	meta := r.Chart
	if meta.APIVersion == "" {
		meta.APIVersion = "v2"
	}
	chartYAML, err := yaml.Marshal(meta)
	if err != nil {
		return nil, err
	}
	files["Chart.yaml"] = chartYAML
	values := r.ChartValues
	if values == nil {
		values = map[string]any{}
	}
	if files["values.yaml"], err = yaml.Marshal(values); err != nil {
		return nil, err
	}
	if len(r.Schema) > 0 {
		files["values.schema.json"] = r.Schema
	}
	for _, t := range r.Templates {
		files[t.Name] = t.Data
	}
	for _, f := range r.Files {
		files[f.Name] = f.Data
	}
	return files, nil
}

// Drift compares the objects of a release manifest with the live objects and
// returns, per object, the fields whose live value differs from the chart's.
// Argo CD and Flux will revert those fields to the chart's values.
func Drift(r *model.HelmRelease, live func(model.Ref) *unstructured.Unstructured, namespaced func(group, kind string) bool) map[model.Ref][]string {
	out := map[model.Ref][]string{}
	for _, desired := range ManifestObjects(r.Manifest) {
		ref := model.RefOf(desired)
		if !namespaced(ref.Group, ref.Kind) {
			ref.Namespace = ""
		} else if ref.Namespace == "" {
			ref.Namespace = r.Namespace
		}
		l := live(ref)
		if l == nil {
			out[ref] = []string{"(missing in the cluster)"}
			continue
		}
		var diffs []string
		for _, top := range []string{"spec", "data", "stringData", "rules", "subjects", "roleRef"} {
			d, ok := desired.Object[top]
			if !ok {
				continue
			}
			compareSubset(d, l.Object[top], top, &diffs)
		}
		if len(diffs) > 0 {
			sort.Strings(diffs)
			out[ref] = diffs
		}
	}
	return out
}

// compareSubset records the paths where live differs from desired, for the
// fields desired sets. Fields added by the API server are ignored.
func compareSubset(desired, live any, path string, diffs *[]string) {
	if len(*diffs) >= 10 {
		return
	}
	switch d := desired.(type) {
	case map[string]any:
		l, ok := live.(map[string]any)
		if !ok {
			if len(d) > 0 {
				*diffs = append(*diffs, path)
			}
			return
		}
		keys := make([]string, 0, len(d))
		for k := range d {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			compareSubset(d[k], l[k], path+"."+k, diffs)
		}
	case []any:
		l, ok := live.([]any)
		if !ok || len(l) != len(d) {
			*diffs = append(*diffs, path)
			return
		}
		for i := range d {
			compareSubset(d[i], l[i], fmt.Sprintf("%s[%d]", path, i), diffs)
		}
	case nil:
		return
	default:
		if !scalarEqual(d, live, path) {
			*diffs = append(*diffs, path)
		}
	}
}

func scalarEqual(a, b any, path string) bool {
	as, bs := scalarString(a), scalarString(b)
	if as == bs {
		return true
	}
	// Resource quantities are normalized by the API server ("1024Mi" → "1Gi").
	if strings.Contains(path, ".resources.") || strings.HasSuffix(path, ".storage") {
		qa, errA := resource.ParseQuantity(as)
		qb, errB := resource.ParseQuantity(bs)
		return errA == nil && errB == nil && qa.Cmp(qb) == 0
	}
	return false
}

func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}
