package sdk

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Resolver tells ForProvider how other resources and secrets end up in
// Kubernetes.
type Resolver interface {
	// Ref returns the name of the managed resource for the resource with the
	// given key, and its namespace when it differs from the referencing
	// resource's (empty otherwise). ok is false when the target isn't
	// exported; the literal value is then used.
	Ref(key string) (name, namespace string, ok bool)
	// Secret returns the Kubernetes Secret (and key, for string values) that
	// holds the secret at an attribute path. ok is false to leave it out.
	Secret(path string, s SecretValue) (name, key string, ok bool)
}

// Omission is an attribute ForProvider leaves out, and why.
type Omission struct {
	Path   string
	Reason string
	// Secret is the secret left out.
	Secret SecretValue
}

// ForProvider returns spec.forProvider for a resource of kind k: its explicit
// Crossplane body when the kind is Explicit, otherwise its attributes with
// Upjet's conventions applied:
//
//   - field names become lowerCamelCase (org_id → orgId), unless renamed in
//     k.Crossplane.Rename; keys of map attributes are kept as they are;
//   - a $ref with a Crossplane field becomes that field ({name, namespace})
//     in place of the attribute when the target is exported; a list of such
//     refs becomes a list in a *Refs field; inside list items (blocks) refs
//     stay literal values, since resolving them would rewrite the list and
//     conflict with server-side apply;
//   - a $secret becomes its *SecretRef field ({name, key}, or {name} for a
//     map) or is left out;
//   - $json becomes JSON text and $file the file's text.
//
// Numbers become int64 or float64, so YAML encoders write them as numbers.
func ForProvider(k *Kind, r *Resource, res Resolver) (map[string]any, []Omission, error) {
	if k.Crossplane == nil {
		return nil, nil, fmt.Errorf("kind %s has no Crossplane type", k.Name)
	}
	d := &deriver{kind: k, res: res, blocks: map[string]bool{}}
	if k.Terraform != nil {
		for _, b := range k.Terraform.Blocks {
			d.blocks[b] = true
		}
	}
	src := r.Attributes
	if k.Crossplane.Explicit {
		if r.Crossplane == nil {
			return nil, nil, fmt.Errorf("resource %s: kind %s is explicit for Crossplane but has no Crossplane body", r.Key, k.Name)
		}
		src = r.Crossplane
		d.verbatim = true
	}
	out, err := d.object("", src, true)
	return out, d.omitted, err
}

type deriver struct {
	kind     *Kind
	res      Resolver
	blocks   map[string]bool
	verbatim bool // explicit bodies keep their field names
	omitted  []Omission
	// inList counts the lists being converted. References inside list items
	// stay literal values: Crossplane's reference resolver would rewrite the
	// whole (atomic) list, which conflicts with server-side apply by GitOps
	// tools.
	inList int
}

func (d *deriver) name(path, attr string) string {
	if d.verbatim {
		return attr
	}
	if n, ok := d.kind.Crossplane.Rename[path]; ok {
		return n
	}
	return LowerCamel(attr)
}

// object converts the fields of a resource body or of a block (renamed) or a
// map attribute (keys kept).
func (d *deriver) object(prefix string, in map[string]any, rename bool) (map[string]any, error) {
	out := map[string]any{}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, attr := range keys {
		path := join(prefix, attr)
		field := attr
		if rename {
			field = d.name(path, attr)
		}
		v := in[attr]
		switch m := v.(type) {
		case RefValue:
			if m.Crossplane != "" && d.res != nil && d.inList == 0 {
				if name, ns, ok := d.res.Ref(m.Ref); ok {
					out[m.Crossplane] = refObject(name, ns)
					continue
				}
			}
			out[field] = plainValue(m.Value)
			continue
		case SecretValue:
			if sr, ok := d.secret(path, m); ok {
				out[m.Crossplane] = sr
			}
			continue
		case []any:
			if refs, field, ok := d.refList(m); ok && d.inList == 0 {
				out[field] = refs
				continue
			}
		}
		conv, keep, err := d.value(path, v, rename)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if keep {
			out[field] = conv
		}
	}
	return out, nil
}

// refList turns a list whose items all reference exported resources through
// the same *Refs field into that field's value.
func (d *deriver) refList(items []any) ([]any, string, bool) {
	if len(items) == 0 || d.res == nil {
		return nil, "", false
	}
	var field string
	var out []any
	for _, it := range items {
		r, ok := it.(RefValue)
		if !ok || r.Crossplane == "" || (field != "" && r.Crossplane != field) {
			return nil, "", false
		}
		field = r.Crossplane
		name, ns, ok := d.res.Ref(r.Ref)
		if !ok {
			return nil, "", false
		}
		out = append(out, refObject(name, ns))
	}
	return out, field, true
}

func (d *deriver) secret(path string, s SecretValue) (map[string]any, bool) {
	switch {
	case s.Value == nil:
		d.omitted = append(d.omitted, Omission{Path: path, Reason: "the tool doesn't return this secret", Secret: s})
		return nil, false
	case s.Crossplane == "":
		d.omitted = append(d.omitted, Omission{Path: path, Reason: "the Crossplane resource has no secret reference for it", Secret: s})
		return nil, false
	case d.res == nil:
		return nil, false
	}
	name, key, ok := d.res.Secret(path, s)
	if !ok {
		return nil, false
	}
	if _, isMap := s.Value.(map[string]string); isMap || key == "" {
		return map[string]any{"name": name}, true
	}
	return map[string]any{"name": name, "key": key}, true
}

func (d *deriver) value(path string, v any, rename bool) (any, bool, error) {
	switch t := v.(type) {
	case nil:
		return nil, false, nil
	case RefValue:
		return plainValue(t.Value), true, nil
	case SecretValue:
		d.omitted = append(d.omitted, Omission{Path: path, Reason: "secrets inside lists or maps can't be referenced", Secret: t})
		return nil, false, nil
	case JSONValue:
		b, err := CompactJSON(t.JSON)
		return string(b), err == nil, err
	case FileValue:
		b, err := FileText(t.Content)
		return string(b), err == nil, err
	case map[string]any:
		// Blocks (and explicit bodies) have schema fields; other objects are
		// maps whose keys are data.
		inner := rename && (d.verbatim || d.blocks[path])
		o, err := d.object(path, t, inner)
		return o, true, err
	case []any:
		d.inList++
		defer func() { d.inList-- }()
		out := make([]any, 0, len(t))
		for _, e := range t {
			c, keep, err := d.value(path, e, rename)
			if err != nil {
				return nil, false, err
			}
			if keep {
				out = append(out, c)
			}
		}
		return out, true, nil
	default:
		return plainValue(v), true, nil
	}
}

func refObject(name, ns string) map[string]any {
	o := map[string]any{"name": name}
	if ns != "" {
		o["namespace"] = ns
	}
	return o
}

// plainValue converts json.Number to int64 or float64 and leaves other
// scalars alone.
func plainValue(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := strconv.ParseInt(string(t), 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(string(t), 64); err == nil {
			return f
		}
		return string(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = plainValue(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = plainValue(e)
		}
		return out
	}
	return v
}

func join(prefix, attr string) string {
	if prefix == "" {
		return attr
	}
	return prefix + "." + attr
}

// LowerCamel converts a Terraform attribute name to the field name Upjet
// generates: org_id → orgId, json_data_encoded → jsonDataEncoded.
func LowerCamel(s string) string {
	parts := strings.Split(s, "_")
	var b strings.Builder
	b.WriteString(parts[0])
	for _, p := range parts[1:] {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return b.String()
}
