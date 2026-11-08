// Package transform applies the configured transforms to objects and Helm
// values: hostnames, registries, storage and ingress classes, namespaces and
// arbitrary fields that differ between environments.
package transform

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/deps"
	"github.com/hayone1/gitopsify/pkg/match"
	"github.com/hayone1/gitopsify/pkg/model"
)

// Object applies the transforms that target o to u, a copy of o's output,
// and reports whether anything changed.
func Object(u *unstructured.Unstructured, o *model.Object, ts []config.Transform) (bool, error) {
	before, _ := json.Marshal(u.Object)
	secret := u.GetAPIVersion() == "v1" && u.GetKind() == "Secret"
	// A protected Secret's values come out as they went in, whatever the
	// transforms do: a change that depends on the value (an overlay of
	// patches or of full copies, an error or none) would tell whether a
	// guess was right.
	var values map[string]any
	if secret && o.Protected {
		values = map[string]any{}
		for _, f := range []string{"data", "stringData"} {
			if v, ok := u.Object[f]; ok {
				values[f] = deepCopy(v)
			}
		}
	}
	for i, t := range ts {
		ok, err := match.SelectorOrAll(t.Target, o, o.App)
		if err != nil {
			return false, fmt.Errorf("transform %d (%s): %w", i, t.Type, err)
		}
		if !ok {
			continue
		}
		if secret && o.Protected {
			// Decided by the transform as written, never by its effect.
			if t.Type == "jsonPatch" {
				for _, op := range t.Patch {
					if name, _ := op["op"].(string); name != "add" && name != "remove" && name != "replace" {
						return false, fmt.Errorf("transform %d (jsonPatch) on %s: %q isn't allowed on a Secret whose values are protected", i, o.Ref, name)
					}
				}
			}
			if msg := fmt.Sprintf("transform %d (%s) doesn't change this Secret's values: they are protected (an admin can allow Secret values)", i, t.Type); touchesValues(t) && !slices.Contains(o.Warnings, msg) {
				o.Warnings = append(o.Warnings, msg)
			}
		}
		if err := apply(u, t, secret && o.Protected); err != nil {
			return false, fmt.Errorf("transform %d (%s) on %s: %w", i, t.Type, o.Ref, err)
		}
	}
	// A Secret that became another kind would skip the secrets strategy,
	// and its values would be written as they are.
	if secret && (u.GetAPIVersion() != "v1" || u.GetKind() != "Secret") {
		if o.Protected { // what it became could hold a value
			return false, fmt.Errorf("transforms change the kind of Secret %s: a Secret must stay a Secret", o.Ref)
		}
		return false, fmt.Errorf("transforms change Secret %s into %s %s: a Secret must stay a Secret", o.Ref, u.GetAPIVersion(), u.GetKind())
	}
	// Protected Secrets (and their SealedSecrets) keep their identity,
	// which sealing binds to.
	if o.Protected && (u.GetNamespace() != o.Ref.Namespace || u.GetName() != o.Ref.Name) {
		return false, fmt.Errorf("transforms move Secret %s to another namespace or name: its values are protected, so it keeps them (an admin can allow Secret values)", o.Ref)
	}
	if values != nil {
		for _, f := range []string{"data", "stringData"} {
			if v, ok := values[f]; ok {
				u.Object[f] = v
			} else {
				delete(u.Object, f)
			}
		}
	}
	after, _ := json.Marshal(u.Object)
	return string(before) != string(after), nil
}

// touchesValues reports whether a transform, as written, sets or removes a
// Secret's values (or reads them, for jsonPatch).
func touchesValues(t config.Transform) bool {
	under := func(p, sep string) bool {
		for _, f := range []string{"data", "stringData"} {
			if p == sep+f || strings.HasPrefix(p, sep+f+sep) || strings.HasPrefix(p, sep+f+"[") {
				return true
			}
		}
		return false
	}
	switch t.Type {
	case "set", "remove":
		p := strings.TrimPrefix(t.Path, ".")
		for _, f := range []string{"data", "stringData"} {
			if strings.HasPrefix(p, `["`+f+`"]`) || strings.HasPrefix(p, `['`+f+`']`) {
				return true
			}
		}
		return under("."+p, ".")
	case "jsonPatch":
		for _, op := range t.Patch {
			for _, k := range []string{"path", "from"} {
				if p, _ := op[k].(string); under(p, "/") {
					return true
				}
			}
		}
	}
	return false
}

// apply applies one transform; protected leaves a Secret's values alone.
func apply(u *unstructured.Unstructured, t config.Transform, protected bool) error {
	obj := u.Object
	switch t.Type {
	case "replace":
		r, err := replacer(t)
		if err != nil {
			return err
		}
		isSecret := u.GetAPIVersion() == "v1" && u.GetKind() == "Secret"
		for k, v := range obj {
			switch {
			case k == "apiVersion" || k == "kind":
			case protected && (k == "data" || k == "stringData"):
			case k == "metadata":
				m, _ := v.(map[string]any)
				for mk, mv := range m {
					if mk == "labels" || mk == "annotations" {
						m[mk] = replaceStrings(mv, r)
					}
				}
			case isSecret && k == "data":
				m, _ := v.(map[string]any)
				for dk, dv := range m {
					s, _ := dv.(string)
					raw, err := base64.StdEncoding.DecodeString(s)
					if err != nil || !utf8.Valid(raw) {
						continue
					}
					m[dk] = base64.StdEncoding.EncodeToString([]byte(r(string(raw))))
				}
			default:
				obj[k] = replaceStrings(v, r)
			}
		}
	case "image":
		if spec := deps.PodSpecPath(u.GroupVersionKind().GroupKind().String()); spec != nil {
			podSpec, found, _ := unstructured.NestedMap(obj, spec...)
			if found {
				for _, field := range []string{"initContainers", "containers", "ephemeralContainers"} {
					cs, _ := podSpec[field].([]any)
					for _, c := range cs {
						cm, _ := c.(map[string]any)
						if img, ok := cm["image"].(string); ok {
							cm["image"] = rewriteImage(img, t.From, t.To)
						}
					}
				}
				_ = unstructured.SetNestedMap(obj, podSpec, spec...)
			}
		}
	case "storageClass":
		gk := u.GroupVersionKind().GroupKind().String()
		switch gk {
		case "PersistentVolumeClaim", "PersistentVolume":
			mapField(obj, t.Map, "spec", "storageClassName")
		case "StatefulSet.apps":
			tpls, _, _ := unstructured.NestedSlice(obj, "spec", "volumeClaimTemplates")
			for _, tpl := range tpls {
				if m, ok := tpl.(map[string]any); ok {
					mapField(m, t.Map, "spec", "storageClassName")
				}
			}
			_ = unstructured.SetNestedSlice(obj, tpls, "spec", "volumeClaimTemplates")
		}
	case "ingressClass":
		if u.GroupVersionKind().GroupKind().String() == "Ingress.networking.k8s.io" {
			mapField(obj, t.Map, "spec", "ingressClassName")
			a := u.GetAnnotations()
			if to, ok := t.Map[a["kubernetes.io/ingress.class"]]; ok {
				a["kubernetes.io/ingress.class"] = to
				u.SetAnnotations(a)
			}
		}
	case "namespace":
		Namespace(u, t.Map)
	case "set":
		path, err := parsePath(t.Path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(t.Path, "values.") {
			return nil // applies to Helm values only
		}
		return setPath(obj, path, deepCopy(t.Value))
	case "remove":
		if strings.HasPrefix(t.Path, "values.") {
			return nil
		}
		path, err := parsePath(t.Path)
		if err != nil {
			return err
		}
		removePath(obj, path)
		compactNils(obj)
	case "jsonPatch":
		patchJSON, err := json.Marshal(t.Patch)
		if err != nil {
			return err
		}
		p, err := jsonpatch.DecodePatch(patchJSON)
		if err != nil {
			return err
		}
		doc, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		out, err := p.Apply(doc)
		if err != nil {
			return err
		}
		m := map[string]any{}
		if err := json.Unmarshal(out, &m); err != nil {
			return err
		}
		u.Object = m
	case "labels":
		u.SetLabels(editMap(u.GetLabels(), t.Set, t.Remove))
	case "annotations":
		u.SetAnnotations(editMap(u.GetAnnotations(), t.Set, t.Remove))
	}
	return nil
}

// Namespace renames namespaces in an object: its own namespace (or name, for
// Namespace objects), RBAC subjects, webhook and APIService service
// references, and in-cluster DNS names such as "db.old.svc".
func Namespace(u *unstructured.Unstructured, m map[string]string) {
	if len(m) == 0 {
		return
	}
	if to, ok := m[u.GetNamespace()]; ok {
		u.SetNamespace(to)
	}
	gk := u.GroupVersionKind().GroupKind().String()
	if gk == "Namespace" {
		if to, ok := m[u.GetName()]; ok {
			u.SetName(to)
		}
	}
	obj := u.Object
	subjects, _, _ := unstructured.NestedSlice(obj, "subjects")
	for _, s := range subjects {
		if sm, ok := s.(map[string]any); ok {
			mapField(sm, m, "namespace")
		}
	}
	if subjects != nil {
		_ = unstructured.SetNestedSlice(obj, subjects, "subjects")
	}
	mapField(obj, m, "spec", "service", "namespace")
	mapField(obj, m, "spec", "conversion", "webhook", "clientConfig", "service", "namespace")
	webhooks, _, _ := unstructured.NestedSlice(obj, "webhooks")
	for _, w := range webhooks {
		if wm, ok := w.(map[string]any); ok {
			mapField(wm, m, "clientConfig", "service", "namespace")
		}
	}
	if webhooks != nil {
		_ = unstructured.SetNestedSlice(obj, webhooks, "webhooks")
	}
	for from, to := range m {
		re := regexp.MustCompile(`\.` + regexp.QuoteMeta(from) + `(\.svc(\.cluster\.local)?)\b`)
		r := func(s string) string { return re.ReplaceAllString(s, "."+to+"$1") }
		if spec, ok := obj["spec"]; ok {
			obj["spec"] = replaceStrings(spec, r)
		}
		if data, ok := obj["data"]; ok && gk == "ConfigMap" {
			obj["data"] = replaceStrings(data, r)
		}
	}
}

// Values applies the transforms that affect Helm values to a copy of values.
func Values(values map[string]any, app *model.App, ts []config.Transform) (map[string]any, error) {
	out, _ := deepCopy(values).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	for i, t := range ts {
		if !config.True(t.HelmValues, true) {
			continue
		}
		if t.Target != nil && len(t.Target.Apps) > 0 && !match.Glob(t.Target.Apps, app.Name) {
			continue
		}
		switch t.Type {
		case "replace":
			r, err := replacer(t)
			if err != nil {
				return nil, err
			}
			out = replaceStrings(out, r).(map[string]any)
		case "image":
			from, to := t.From, t.To
			out = replaceStrings(out, func(s string) string {
				if s == strings.TrimSuffix(from, "/") {
					return strings.TrimSuffix(to, "/")
				}
				return rewriteImage(s, from, to)
			}).(map[string]any)
		case "storageClass":
			mapKeys(out, t.Map, "storageClass", "storageClassName")
		case "ingressClass":
			mapKeys(out, t.Map, "ingressClassName", "className", "ingressClass")
		case "set", "remove":
			if !strings.HasPrefix(t.Path, "values.") {
				continue
			}
			path, err := parsePath(strings.TrimPrefix(t.Path, "values."))
			if err != nil {
				return nil, fmt.Errorf("transform %d: %w", i, err)
			}
			if t.Type == "set" {
				if err := setPath(out, path, deepCopy(t.Value)); err != nil {
					return nil, fmt.Errorf("transform %d on values of %s: %w", i, app.Name, err)
				}
			} else {
				removePath(out, path)
				compactNils(out)
			}
		}
	}
	return out, nil
}

func replacer(t config.Transform) (func(string) string, error) {
	if !t.Regex {
		return func(s string) string { return strings.ReplaceAll(s, t.From, t.To) }, nil
	}
	re, err := regexp.Compile(t.From)
	if err != nil {
		return nil, err
	}
	return func(s string) string { return re.ReplaceAllString(s, t.To) }, nil
}

func rewriteImage(image, from, to string) string {
	if strings.HasPrefix(image, from) {
		return to + strings.TrimPrefix(image, from)
	}
	return image
}

func replaceStrings(v any, r func(string) string) any {
	switch t := v.(type) {
	case string:
		return r(t)
	case map[string]any:
		for k, c := range t {
			t[k] = replaceStrings(c, r)
		}
		return t
	case []any:
		for i, c := range t {
			t[i] = replaceStrings(c, r)
		}
		return t
	}
	return v
}

func mapField(obj map[string]any, m map[string]string, path ...string) {
	v, found, _ := unstructured.NestedString(obj, path...)
	if !found {
		return
	}
	if to, ok := m[v]; ok {
		_ = unstructured.SetNestedField(obj, to, path...)
	}
}

// mapKeys renames values of the given keys anywhere in a values tree.
func mapKeys(v any, m map[string]string, keys ...string) {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			if s, ok := c.(string); ok {
				for _, key := range keys {
					if k == key {
						if to, ok := m[s]; ok {
							t[k] = to
						}
					}
				}
				continue
			}
			mapKeys(c, m, keys...)
		}
	case []any:
		for _, c := range t {
			mapKeys(c, m, keys...)
		}
	}
}

func editMap(m map[string]string, set map[string]string, remove []string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if !match.Glob(remove, k) {
			out[k] = v
		}
	}
	for k, v := range set {
		out[k] = v
	}
	return out
}

func deepCopy(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}
