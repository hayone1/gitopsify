// Package target compares what gitopsify generates for an environment with
// that environment's cluster: preflight checks, diffs and takeover
// preparation.
package target

import (
	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/helm"
	"github.com/hayone1/gitopsify/pkg/model"
	"github.com/hayone1/gitopsify/pkg/transform"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// EnvObject is an object as it will be applied in an environment.
type EnvObject struct {
	App    *model.App
	Source *model.Object
	Object *unstructured.Unstructured
	// FromChart is set for objects rendered by a Helm chart, taken from the
	// release manifest.
	FromChart bool
	// DefaultNamespace is the namespace of namespaced objects that don't set
	// one: the release namespace for chart objects.
	DefaultNamespace string
}

// Objects returns the objects gitopsify generates for an environment: the
// manifests of every app with the environment's transforms, and the objects
// of Helm releases as recorded in the release.
func Objects(inv *model.Inventory, cfg *config.Config, env *config.Environment) ([]EnvObject, error) {
	var out []EnvObject
	for _, a := range inv.Apps {
		if a.IsHelm() {
			for _, u := range helm.ManifestObjects(a.Release.Manifest) {
				out = append(out, EnvObject{App: a, Object: u, FromChart: true, DefaultNamespace: a.Release.Namespace})
			}
			continue
		}
		for _, o := range a.Objects {
			var u *unstructured.Unstructured
			if e, ok := o.EnvOut[env.Name]; ok {
				u = e.DeepCopy()
			} else if o.Out != nil {
				u = o.Out.DeepCopy()
				if _, err := transform.Object(u, o, env.Transforms); err != nil {
					return nil, err
				}
			}
			if u != nil {
				out = append(out, EnvObject{App: a, Source: o, Object: u})
			}
		}
	}
	return out, nil
}
