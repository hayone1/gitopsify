// Package helm reads Helm releases from their storage Secrets/ConfigMaps,
// works out which objects they own and where their charts come from.
package helm

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/hayone1/gitopsify/pkg/model"
)

// Annotations Helm 3+ puts on every object it installs.
const (
	AnnotationReleaseName      = "meta.helm.sh/release-name"
	AnnotationReleaseNamespace = "meta.helm.sh/release-namespace"
	ReleaseSecretType          = "helm.sh/release.v1"
)

// rawRelease mirrors the JSON of helm.sh/helm/v4/pkg/release/v1.Release.
type rawRelease struct {
	Name string `json:"name"`
	Info *struct {
		Status string `json:"status"`
	} `json:"info"`
	Chart *struct {
		Metadata  *model.ChartMeta `json:"metadata"`
		Templates []model.File     `json:"templates"`
		Values    map[string]any   `json:"values"`
		Schema    []byte           `json:"schema"`
		Files     []model.File     `json:"files"`
	} `json:"chart"`
	Config    map[string]any   `json:"config"`
	Manifest  string           `json:"manifest"`
	Hooks     []model.HelmHook `json:"hooks"`
	Version   int              `json:"version"`
	Namespace string           `json:"namespace"`
}

// IsStorage reports whether an object is Helm release storage: a Secret of
// type helm.sh/release.v1 or a ConfigMap/Secret labelled owner=helm.
func IsStorage(u *unstructured.Unstructured) bool {
	if u.GetAPIVersion() != "v1" || (u.GetKind() != "Secret" && u.GetKind() != "ConfigMap") {
		return false
	}
	if t, _, _ := unstructured.NestedString(u.Object, "type"); t == ReleaseSecretType {
		return true
	}
	l := u.GetLabels()
	return l["owner"] == "helm" && l["name"] != "" && strings.HasPrefix(u.GetName(), "sh.helm.release.v1.")
}

// Decode decodes a release storage object.
func Decode(u *unstructured.Unstructured) (*model.HelmRelease, error) {
	data, _, _ := unstructured.NestedString(u.Object, "data", "release")
	if data == "" {
		return nil, fmt.Errorf("%s/%s has no release data", u.GetNamespace(), u.GetName())
	}
	if u.GetKind() == "Secret" {
		// Secret data is base64 on the wire, on top of Helm's own base64.
		b, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return nil, fmt.Errorf("decoding secret %s/%s: %w", u.GetNamespace(), u.GetName(), err)
		}
		data = string(b)
	}
	return decodeRelease(data)
}

func decodeRelease(data string) (*model.HelmRelease, error) {
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, fmt.Errorf("decoding release: %w", err)
	}
	// Releases stored before Helm compressed them are plain JSON.
	if len(b) > 3 && bytes.Equal(b[:3], []byte{0x1f, 0x8b, 0x08}) {
		r, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("decompressing release: %w", err)
		}
		defer r.Close()
		if b, err = io.ReadAll(r); err != nil {
			return nil, fmt.Errorf("decompressing release: %w", err)
		}
	}
	var raw rawRelease
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parsing release: %w", err)
	}
	rel := &model.HelmRelease{
		Name:      raw.Name,
		Namespace: raw.Namespace,
		Revision:  raw.Version,
		Values:    raw.Config,
		Manifest:  raw.Manifest,
		Hooks:     raw.Hooks,
	}
	if raw.Info != nil {
		rel.Status = raw.Info.Status
	}
	if raw.Chart != nil {
		if raw.Chart.Metadata != nil {
			rel.Chart = *raw.Chart.Metadata
		}
		rel.ChartValues = raw.Chart.Values
		rel.Templates = raw.Chart.Templates
		rel.Schema = raw.Chart.Schema
		for _, f := range raw.Chart.Files {
			rel.Files = append(rel.Files, f)
			if strings.HasPrefix(f.Name, "crds/") && isManifestFile(f.Name) {
				rel.CRDs = append(rel.CRDs, f)
			}
		}
	}
	if rel.Values == nil {
		rel.Values = map[string]any{}
	}
	return rel, nil
}

func isManifestFile(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".json")
}

// Latest picks the revision that describes each release: the newest
// "deployed" revision, or the newest revision when none is deployed.
func Latest(storage []*unstructured.Unstructured) ([]*model.HelmRelease, []string) {
	type candidate struct {
		u       *unstructured.Unstructured
		version int
		status  string
	}
	byRelease := map[string][]candidate{}
	for _, u := range storage {
		l := u.GetLabels()
		v, _ := strconv.Atoi(l["version"])
		key := u.GetNamespace() + "/" + l["name"]
		byRelease[key] = append(byRelease[key], candidate{u: u, version: v, status: l["status"]})
	}
	var out []*model.HelmRelease
	var warnings []string
	keys := make([]string, 0, len(byRelease))
	for k := range byRelease {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cs := byRelease[k]
		sort.Slice(cs, func(i, j int) bool { return cs[i].version > cs[j].version })
		pick := cs[0]
		for _, c := range cs {
			if c.status == "deployed" {
				pick = c
				break
			}
		}
		rel, err := Decode(pick.u)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Helm release %s: %v", k, err))
			continue
		}
		if rel.Namespace == "" {
			rel.Namespace = pick.u.GetNamespace()
		}
		if rel.Status != "deployed" {
			rel.Warnings = append(rel.Warnings, fmt.Sprintf("no deployed revision; using revision %d (%s)", rel.Revision, rel.Status))
		} else if newest := cs[0]; newest.version > pick.version {
			// A failed or interrupted upgrade may have changed some objects.
			rel.Warnings = append(rel.Warnings, fmt.Sprintf("revision %d (%s) is newer than the deployed revision %d that is migrated; objects changed by the unfinished upgrade are set back to revision %d", newest.version, orUnknown(newest.status), pick.version, pick.version))
		}
		out = append(out, rel)
	}
	return out, warnings
}

// ManifestRefs parses a multi-document manifest into object refs. namespaced
// reports whether a kind is namespaced; objects without a namespace get
// defaultNS when they are.
func ManifestRefs(manifest, defaultNS string, namespaced func(group, kind string) bool) []model.Ref {
	var refs []model.Ref
	for _, doc := range SplitDocs(manifest) {
		var m struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil || m.Kind == "" || m.Metadata.Name == "" {
			continue
		}
		group := ""
		if i := strings.LastIndex(m.APIVersion, "/"); i > 0 {
			group = m.APIVersion[:i]
		}
		ns := m.Metadata.Namespace
		if ns == "" && namespaced(group, m.Kind) {
			ns = defaultNS
		}
		if !namespaced(group, m.Kind) {
			ns = ""
		}
		refs = append(refs, model.Ref{Group: group, Kind: m.Kind, Namespace: ns, Name: m.Metadata.Name})
	}
	return refs
}

// ManifestObjects parses a multi-document manifest into objects.
func ManifestObjects(manifest string) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	for _, doc := range SplitDocs(manifest) {
		m := map[string]any{}
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil || len(m) == 0 {
			continue
		}
		// Charts render unset values as null (ingressClassName: with an empty
		// value). Helm drops them when it installs; applying them would remove
		// what the cluster defaulted.
		dropNulls(m)
		u := &unstructured.Unstructured{Object: m}
		if u.GetKind() == "" {
			continue
		}
		out = append(out, u)
	}
	return out
}

// SplitDocs splits a YAML stream into documents, dropping empty ones.
func SplitDocs(s string) []string {
	var docs []string
	var cur []string
	flush := func() {
		d := strings.TrimSpace(strings.Join(cur, "\n"))
		cur = cur[:0]
		if d == "" {
			return
		}
		// Skip documents that only hold comments, such as Helm's "# Source:" lines.
		for _, l := range strings.Split(d, "\n") {
			if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
				docs = append(docs, d)
				return
			}
		}
	}
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "---") {
			flush()
			continue
		}
		cur = append(cur, l)
	}
	flush()
	return docs
}

// CRDNames returns the names of the CRDs in the release's crds/ files.
func CRDNames(r *model.HelmRelease) []string {
	var names []string
	for _, f := range r.CRDs {
		for _, u := range ManifestObjects(string(f.Data)) {
			if u.GetKind() == "CustomResourceDefinition" {
				names = append(names, u.GetName())
			}
		}
	}
	return names
}

// OwnerOf returns the "<namespace>/<name>" release key of an object from its
// Helm annotations.
func OwnerOf(u *unstructured.Unstructured) string {
	a := u.GetAnnotations()
	name, ns := a[AnnotationReleaseName], a[AnnotationReleaseNamespace]
	if name == "" {
		return ""
	}
	if ns == "" {
		ns = u.GetNamespace()
	}
	return ns + "/" + name
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown status"
	}
	return s
}

// dropNulls removes null values from maps, recursively.
func dropNulls(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			if c == nil {
				delete(t, k)
				continue
			}
			dropNulls(c)
		}
	case []any:
		for _, c := range t {
			dropNulls(c)
		}
	}
}
