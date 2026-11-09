package iac

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/yamlutil"
	"github.com/hayone1/gitopsify/sdk"
)

// Labels and annotations on generated Crossplane objects.
const (
	LabelProvider      = "gitopsify.io/provider"
	AnnotationExternal = "crossplane.io/external-name"
	// CredentialsNamespace holds the ProviderConfigs' credential Secrets.
	CredentialsNamespace = "crossplane-system"
	// LargeObject is the size above which the report warns about a managed
	// resource (it is stored twice: spec and status.atProvider).
	LargeObject = 512 << 10
)

// Default management policies (see config.CrossplaneOutput).
var (
	PoliciesAdopt     = []string{"Observe", "Create", "Update"}
	PoliciesReplicate = []string{"Observe", "Create", "Update", "Delete"}
)

func renderCrossplane(res *Result, in Input, workspaces []*workspace, byKey map[string]*resource) error {
	d := in.Description
	xp := d.Crossplane
	policies := in.Options.ManagementPolicies
	if len(policies) == 0 {
		policies = PoliciesReplicate
		if in.Options.Adopt {
			policies = PoliciesAdopt
		}
	}
	rep := &res.Report

	for _, w := range workspaces {
		base := path.Join("crossplane", w.dir)
		if !w.unannounced {
			wu := res.unit(config.FormatCrossplane, w.id, "")
			wu.add(base+"/namespace.yaml", mustYAML(map[string]any{
				"apiVersion": "v1", "kind": "Namespace",
				"metadata": map[string]any{"name": w.namespace},
			}), false)
			spec := map[string]any{
				"credentials": map[string]any{
					"source":    "Secret",
					"secretRef": map[string]any{"namespace": CredentialsNamespace, "name": credentialsSecret(d, w.inst.Name), "key": "credentials"},
				},
			}
			if w.partition != nil {
				for k, v := range w.partition.Crossplane {
					spec[k] = v
				}
			}
			wu.add(base+"/providerconfig.yaml", mustYAML(map[string]any{
				"apiVersion": xp.ProviderConfig.APIVersion, "kind": xp.ProviderConfig.Kind,
				"metadata": map[string]any{"name": w.namespace},
				"spec":     spec,
			}), false)
		}
		secretNames := newNamer("-")
		for _, rs := range stateList(res.State) {
			if rs.Workspace == w.id {
				for _, n := range rs.Secrets {
					secretNames.reserve(n)
				}
			}
		}

		for i := range d.Kinds {
			kind := &d.Kinds[i]
			if kind.Crossplane == nil || w.isIncomplete(kind.Name) {
				continue
			}
			inScope := w.inst.hasKind(kind.Name) || len(w.gone[kind.Name]) > 0
			var rs []*resource
			for _, r := range w.resources {
				if r.kind == kind {
					rs = append(rs, r)
				}
			}
			if !inScope && len(rs) == 0 {
				continue
			}
			u := res.unit(config.FormatCrossplane, w.id, kind.Name)
			dir := base + "/" + kindDir(kind)
			for _, r := range rs {
				resolver := &xpResolver{w: w, r: r, byKey: byKey, keys: newNamer("-"), names: secretNames, prev: in.Previous}
				fp, omitted, err := sdk.ForProvider(kind, r.r, resolver)
				if err != nil {
					return err
				}
				for _, o := range omitted {
					reason := o.Reason
					if o.Secret.Value == nil {
						reason = secretNotReturned
					}
					rep.leaveOut(kind, w, r.r, o.Path, o.Secret, reason)
				}
				meta := map[string]any{
					"name": r.state.Crossplane, "namespace": w.namespace,
					"labels": map[string]any{LabelProvider: d.Name},
				}
				if in.Options.Adopt {
					ext := firstNonEmpty(r.r.ExternalName, r.r.ImportID)
					if ext != "" {
						meta["annotations"] = map[string]any{AnnotationExternal: ext}
					}
				}
				spec := map[string]any{
					"managementPolicies": toAny(policies),
					"providerConfigRef":  map[string]any{"kind": xp.ProviderConfig.Kind, "name": w.namespace},
					"forProvider":        fp,
				}
				// What Terraform must ignore goes to initProvider, which Upjet
				// uses when creating and ignores afterwards.
				if init := initProvider(kind, r.r.Attributes, fp); len(init) > 0 {
					spec["initProvider"] = init
				}
				obj := map[string]any{
					"apiVersion": kind.Crossplane.APIVersion, "kind": kind.Crossplane.Kind,
					"metadata": meta,
					"spec":     spec,
				}
				b := mustYAML(obj)
				if len(b) > LargeObject {
					rep.Large = append(rep.Large, fmt.Sprintf("%s %q (%s): %d KiB", kind.Name, r.r.Name, w.title(), len(b)>>10))
				}
				u.add(dir+"/"+r.state.Crossplane+".yaml", b, false)
				for _, s := range resolver.secrets {
					if in.Options.SecretValues == config.SecretValuesLocal {
						u.add(".secrets/"+base+"/"+s.name+".yaml", mustYAML(map[string]any{
							"apiVersion": "v1", "kind": "Secret",
							"metadata":   map[string]any{"name": s.name, "namespace": w.namespace},
							"stringData": s.data,
						}), true)
					} else {
						keys := make([]string, 0, len(s.data))
						for k := range s.data {
							keys = append(keys, k)
						}
						sort.Strings(keys)
						rep.Secrets = append(rep.Secrets, fmt.Sprintf("%s/%s (keys: %s), for %s %q", w.namespace, s.name, strings.Join(keys, ", "), kind.Name, r.r.Name))
					}
				}
			}
		}
	}

	// Credentials, per instance.
	for _, inst := range in.Instances {
		if in.Options.SecretValues != config.SecretValuesLocal {
			continue
		}
		creds := map[string]string{}
		for _, s := range d.Settings {
			if s.Crossplane != "" && inst.Settings[s.Name] != "" {
				creds[s.Crossplane] = inst.Settings[s.Name]
			}
		}
		b, _ := json.Marshal(creds)
		res.unit(config.FormatCrossplane, inst.Name+"/*", "").add(".secrets/crossplane/setup/"+credentialsSecret(d, inst.Name)+".yaml", mustYAML(map[string]any{
			"apiVersion": "v1", "kind": "Secret",
			"metadata":   map[string]any{"name": credentialsSecret(d, inst.Name), "namespace": CredentialsNamespace},
			"stringData": map[string]any{"credentials": string(b)},
		}), true)
	}
	return nil
}

// finishCrossplane writes the files that list other files: Kustomizations,
// the setup and the README. It runs after every unit is known, so files kept
// from earlier exports are listed too.
func finishCrossplane(res *Result, in Input) {
	d := in.Description
	xp := d.Crossplane
	owned := res.ownedAfter(config.FormatCrossplane)

	// Workspaces this export rendered get their kustomization rewritten;
	// others keep theirs (and every file in them).
	rendered := map[string]bool{}
	for k := range res.units {
		if k.format == config.FormatCrossplane {
			rendered[k.ws] = true
		}
	}
	var wsDirs []string
	for _, ws := range sortedMapKeys(owned) {
		if ws == "" || strings.HasSuffix(ws, "/*") {
			continue
		}
		wst := res.State.Workspaces[ws]
		if wst == nil {
			continue
		}
		var files []string
		for _, paths := range owned[ws] {
			for _, p := range paths {
				rel, ok := strings.CutPrefix(p, "crossplane/"+wst.Dir+"/")
				if ok && strings.HasSuffix(rel, ".yaml") && rel != "kustomization.yaml" {
					files = append(files, rel)
				}
			}
		}
		if len(files) == 0 {
			continue
		}
		wsDirs = append(wsDirs, wst.Dir)
		if !rendered[ws] {
			continue
		}
		sort.Strings(files)
		res.unit(config.FormatCrossplane, ws, "").add("crossplane/"+wst.Dir+"/kustomization.yaml", mustYAML(map[string]any{
			"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization",
			"resources": toAny(files),
		}), false)
	}
	sort.Strings(wsDirs)
	root := res.unit(config.FormatCrossplane, "", "")
	k := map[string]any{"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "resources": toAny(wsDirs)}
	if in.Options.Adopt {
		k["components"] = []any{"components/observe-only"}
		root.add("crossplane/components/observe-only/kustomization.yaml", []byte(`# While this component is listed in ../../kustomization.yaml, every managed
# resource only observes: Crossplane reads the objects it adopts and changes
# nothing. When they are all Ready and Synced (kubectl get managed -A), remove
# the component from the list to let Crossplane manage them.
apiVersion: kustomize.config.k8s.io/v1alpha1
kind: Component
patches:
  - target:
      labelSelector: `+LabelProvider+`=`+d.Name+`
    patch: |-
      - op: replace
        path: /spec/managementPolicies
        value: ["Observe"]
`), false)
	}
	root.add("crossplane/kustomization.yaml", mustYAML(k), false)
	root.add("crossplane/setup/provider.yaml", mustYAML(map[string]any{
		"apiVersion": "pkg.crossplane.io/v1", "kind": "Provider",
		"metadata": map[string]any{"name": xp.Name},
		"spec":     map[string]any{"package": xp.Package},
	}), false)
	root.add("crossplane/setup/kustomization.yaml", mustYAML(map[string]any{
		"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization",
		"resources": []any{"provider.yaml"},
	}), false)
	root.add("crossplane/README.md", crossplaneReadme(res, in, wsDirs), false)
}

// initProvider moves the fields of a kind's Terraform IgnoreChanges out of
// forProvider: the attribute's own field, or the *Ref / *SecretRef field it
// became.
func initProvider(kind *sdk.Kind, attrs, fp map[string]any) map[string]any {
	if kind.Terraform == nil || kind.Crossplane.Explicit {
		return nil
	}
	init := map[string]any{}
	move := func(field string) {
		if v, ok := fp[field]; ok && field != "" {
			init[field] = v
			delete(fp, field)
		}
	}
	for _, attr := range kind.Terraform.IgnoreChanges {
		field := sdk.LowerCamel(attr)
		if n, ok := kind.Crossplane.Rename[attr]; ok {
			field = n
		}
		move(field)
		switch v := attrs[attr].(type) {
		case sdk.RefValue:
			move(v.Crossplane)
		case sdk.SecretValue:
			move(v.Crossplane)
		}
	}
	return init
}

func credentialsSecret(d *sdk.Description, inst string) string {
	return truncate(d.Name+"-"+inst+"-credentials", 63, "-")
}

func kindDir(k *sdk.Kind) string {
	return strings.ReplaceAll(kindFileBase(k), "_", "-")
}

type xpSecret struct {
	name string
	data map[string]any
}

// xpResolver resolves references and secrets of one managed resource.
type xpResolver struct {
	w     *workspace
	r     *resource
	byKey map[string]*resource
	keys  *namer
	// names hands out Secret names, unique in the workspace's namespace.
	names   *namer
	strName string
	secrets []*xpSecret
	// prev is the last export's state: references only to what is output.
	prev *State
}

func (x *xpResolver) Ref(key string) (string, string, bool) {
	t, ok := x.byKey[stateKey(x.w.inst.Name, key)]
	if !ok || t.kind.Crossplane == nil || t.state.Crossplane == "" || !declared(t, x.prev) {
		return "", "", false
	}
	ns := ""
	if t.ws.namespace != x.w.namespace {
		ns = t.ws.namespace
	}
	return t.state.Crossplane, ns, true
}

func (x *xpResolver) Secret(p string, s sdk.SecretValue) (string, string, bool) {
	base := truncate(strings.ToLower(x.r.kind.Crossplane.Kind)+"-"+x.r.state.Crossplane, 50, "-")
	if x.r.state.Secrets == nil {
		x.r.state.Secrets = map[string]string{}
	}
	if m, ok := s.Value.(map[string]string); ok {
		name, ok := x.r.state.Secrets[p]
		if !ok {
			name = x.names.assign(truncate(base+"-"+k8sBase(s.Secret, p), 56, "-"), x.r.r.Key+"\x00"+p)
			x.r.state.Secrets[p] = name
		}
		data := map[string]any{}
		for k, v := range m {
			data[k] = v
		}
		x.secrets = append(x.secrets, &xpSecret{name: name, data: data})
		return name, "", true
	}
	if x.strName == "" {
		if n, ok := x.r.state.Secrets[""]; ok {
			x.strName = n
		} else {
			x.strName = x.names.assign(base, x.r.r.Key)
			x.r.state.Secrets[""] = x.strName
		}
	}
	name := x.strName
	var sec *xpSecret
	for _, e := range x.secrets {
		if e.name == name {
			sec = e
		}
	}
	if sec == nil {
		sec = &xpSecret{name: name, data: map[string]any{}}
		x.secrets = append(x.secrets, sec)
	}
	key := x.keys.assign(k8sBase(s.Secret, p), p)
	sec.data[key] = fmt.Sprint(s.Value)
	return name, key, true
}

func mustYAML(v any) []byte {
	b, err := yamlutil.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
