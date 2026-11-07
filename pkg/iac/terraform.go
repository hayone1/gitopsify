package iac

import (
	"fmt"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/sdk"
)

// MinTerraformVersion is required for removed blocks.
const MinTerraformVersion = ">= 1.7.0"

func renderTerraform(res *Result, in Input, workspaces []*workspace, byKey map[string]*resource) error {
	d := in.Description
	for _, w := range workspaces {
		base := path.Join("terraform", w.dir)
		wu := res.unit(config.FormatTerraform, w.id, "")
		versions, err := versionsFile(d, in.Options.Backend, w)
		if err != nil {
			return err
		}
		wu.add(base+"/versions.tf", versions, false)
		wu.add(base+"/providers.tf", providersFile(d, w), false)

		for _, kind := range tfKinds(d, w) {
			if w.isIncomplete(kind.Name) {
				continue // keep what the last export wrote
			}
			u := res.unit(config.FormatTerraform, w.id, kind.Name)
			if err := renderTerraformKind(res, in, w, kind, byKey, u, base); err != nil {
				return err
			}
		}
	}
	res.unit(config.FormatTerraform, "", "").add("terraform/README.md", terraformReadme(res, in), false)
	return nil
}

// tfKinds are the kinds a workspace renders: those in scope, plus any with
// resources or removed blocks.
func tfKinds(d *sdk.Description, w *workspace) []*sdk.Kind {
	var out []*sdk.Kind
	for i := range d.Kinds {
		k := &d.Kinds[i]
		if k.Terraform == nil {
			continue
		}
		if w.inst.hasKind(k.Name) || len(w.gone[k.Name]) > 0 {
			out = append(out, k)
		}
	}
	return out
}

func versionsFile(d *sdk.Description, backend *config.TerraformBackend, w *workspace) ([]byte, error) {
	f := hclwrite.NewEmptyFile()
	tf := f.Body().AppendNewBlock("terraform", nil).Body()
	tf.SetAttributeValue("required_version", cty.StringVal(MinTerraformVersion))
	rp := tf.AppendNewBlock("required_providers", nil).Body()
	rp.SetAttributeRaw(d.Terraform.Name, hclwrite.TokensForObject([]hclwrite.ObjectAttrTokens{
		{Name: hclwrite.TokensForIdentifier("source"), Value: hclwrite.TokensForValue(cty.StringVal(d.Terraform.Source))},
		{Name: hclwrite.TokensForIdentifier("version"), Value: hclwrite.TokensForValue(cty.StringVal(d.Terraform.Version))},
	}))
	if backend != nil {
		tf.AppendNewline()
		b := tf.AppendNewBlock("backend", []string{backend.Type}).Body()
		keys := make([]string, 0, len(backend.Settings))
		for k := range backend.Settings {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !hclsyntax.ValidIdentifier(k) {
				return nil, fmt.Errorf("backend setting %q isn't a valid name", k)
			}
			b.SetAttributeValue(k, cty.StringVal(strings.ReplaceAll(backend.Settings[k], "{workspace}", w.dir)))
		}
	}
	return hclwrite.Format(f.Bytes()), nil
}

func providersFile(d *sdk.Description, w *workspace) []byte {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	var envs []string
	for _, s := range d.Settings {
		if s.Secret && s.Env != "" {
			envs = append(envs, fmt.Sprintf("%s (%s)", s.Env, firstNonEmpty(s.Title, s.Name)))
		}
	}
	comment := fmt.Sprintf("# %s: %s\n", w.inst.Name, firstNonEmpty(w.inst.Target, d.Title))
	if len(envs) > 0 {
		comment += "# Credentials come from the environment: " + strings.Join(envs, ", ") + ".\n"
	}
	body.AppendUnstructuredTokens(hclwrite.Tokens{{Type: hclsyntax.TokenComment, Bytes: []byte(comment)}})
	p := body.AppendNewBlock("provider", []string{d.Terraform.Name}).Body()
	attrs := map[string]any{}
	for _, s := range d.Settings {
		if s.Secret || s.Terraform == "" {
			continue
		}
		if v, ok := w.inst.Settings[s.Name]; ok && v != "" {
			attrs[s.Terraform] = v
		}
	}
	if w.partition != nil {
		for k, v := range w.partition.Terraform {
			attrs[k] = v
		}
	}
	h := &hclValue{}
	for _, k := range sortedKeys(attrs) {
		if toks, ok, err := h.tokens(k, scalar(attrs[k])); err == nil && ok {
			p.SetAttributeRaw(k, toks)
		}
	}
	return hclwrite.Format(f.Bytes())
}

// scalar turns numbers decoded by encoding/json (float64) into json.Number,
// which the HCL writer understands.
func scalar(v any) any {
	switch t := v.(type) {
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return jsonNumber(strconv.FormatInt(int64(t), 10))
		}
		return jsonNumber(strconv.FormatFloat(t, 'g', -1, 64))
	case int:
		return jsonNumber(strconv.Itoa(t))
	case int64:
		return jsonNumber(strconv.FormatInt(t, 10))
	case map[string]any:
		out := map[string]any{}
		for k, e := range t {
			out[k] = scalar(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = scalar(e)
		}
		return out
	}
	return v
}

func renderTerraformKind(res *Result, in Input, w *workspace, kind *sdk.Kind, byKey map[string]*resource, u *unit, base string) error {
	fileName := kindFileBase(kind) + ".tf"
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	blocks := map[string]bool{}
	for _, b := range kind.Terraform.Blocks {
		blocks[b] = true
	}
	varNames := newNamer("_")
	var tfvars []string
	var imports []*resource
	rep := &res.Report

	first := true
	for _, r := range w.resources {
		if r.kind != kind {
			continue
		}
		addr := r.state.Terraform
		typ, name, _ := strings.Cut(addr, ".")
		short := strings.TrimPrefix(typ, in.Description.Terraform.Name+"_")
		h := &hclValue{
			ref: func(ref sdk.RefValue) (hclwrite.Tokens, bool) {
				t, ok := byKey[stateKey(w.inst.Name, ref.Ref)]
				if !ok || t.ws != w || t.kind.Terraform == nil || t.state.Terraform == "" || !hclsyntax.ValidIdentifier(ref.Attr) {
					return nil, false
				}
				return rawTokens(t.state.Terraform + "." + ref.Attr), true
			},
			secret: func(p string, s sdk.SecretValue) (hclwrite.Tokens, bool) {
				if s.Value == nil {
					rep.MissingSecrets = append(rep.MissingSecrets, fmt.Sprintf("%s %q (%s): %s%s", kind.Name, r.r.Name, w.title(), p, describe(s.Description)))
					return nil, false
				}
				v := varNames.assign(truncate(short+"_"+name+"_"+s.Secret, 120, "_"), r.r.Key+p)
				typ := "string"
				if m, ok := s.Value.(map[string]string); ok {
					typ = "map(string)"
					if in.Options.SecretValues == config.SecretValuesLocal {
						tfvars = append(tfvars, tfvarMap(v, m))
					}
				} else if in.Options.SecretValues == config.SecretValuesLocal {
					tfvars = append(tfvars, tfvar(v, fmt.Sprint(s.Value)))
				}
				desc := fmt.Sprintf("%s of %s %q", firstNonEmpty(s.Description, p), kind.Name, r.r.Name)
				variableBlock(body, v, desc, typ)
				if in.Options.SecretValues != config.SecretValuesLocal {
					rep.Variables = append(rep.Variables, fmt.Sprintf("%s: TF_VAR_%s (%s)", w.dir, v, desc))
				}
				return rawTokens("var." + v), true
			},
			file: func(p string, fv sdk.FileValue) (hclwrite.Tokens, error) {
				sp, ok := r.state.Files[fv.File]
				if !ok {
					return nil, fmt.Errorf("%s: no name for file %s", r.r.Key, fv.File)
				}
				text, err := sdk.FileText(fv.Content)
				if err != nil {
					return nil, fmt.Errorf("%s: %s: %w", r.r.Key, p, err)
				}
				u.add(base+"/"+sp, text, false)
				return fileCall(sp), nil
			},
		}
		if !first {
			body.AppendNewline()
		}
		first = false
		rb := body.AppendNewBlock("resource", []string{typ, name}).Body()
		if err := writeBody(rb, "", r.r.Attributes, blocks, h); err != nil {
			return fmt.Errorf("%s %s: %w", kind.Name, r.r.Key, err)
		}
		if ic := kind.Terraform.IgnoreChanges; len(ic) > 0 {
			var elems []hclwrite.Tokens
			for _, a := range ic {
				if hclsyntax.ValidIdentifier(a) {
					elems = append(elems, rawTokens(a))
				}
			}
			rb.AppendNewline()
			rb.AppendNewBlock("lifecycle", nil).Body().SetAttributeRaw("ignore_changes", hclwrite.TokensForTuple(elems))
		}
		var deps []string
		for _, dep := range r.r.DependsOn {
			if t, ok := byKey[stateKey(w.inst.Name, dep)]; ok && t.ws == w && t.state.Terraform != "" && t.kind.Terraform != nil {
				deps = append(deps, t.state.Terraform)
			}
		}
		if len(deps) > 0 {
			sort.Strings(deps)
			var elems []hclwrite.Tokens
			for _, d := range deps {
				elems = append(elems, rawTokens(d))
			}
			rb.AppendNewline()
			rb.SetAttributeRaw("depends_on", hclwrite.TokensForTuple(elems))
		}
		if in.Options.Adopt && r.r.ImportID != "" {
			imports = append(imports, r)
		}
	}
	for _, r := range imports {
		body.AppendNewline()
		ib := body.AppendNewBlock("import", nil).Body()
		ib.SetAttributeRaw("to", rawTokens(r.state.Terraform))
		ib.SetAttributeValue("id", cty.StringVal(r.r.ImportID))
	}
	gone := w.gone[kind.Name]
	sort.Slice(gone, func(i, j int) bool { return gone[i].state.Terraform < gone[j].state.Terraform })
	for _, g := range gone {
		if g.state.Terraform == "" {
			continue
		}
		body.AppendNewline()
		body.AppendUnstructuredTokens(hclwrite.Tokens{{Type: hclsyntax.TokenComment,
			Bytes: []byte("# No longer exported (deleted, renamed away or out of the selection): Terraform forgets it but doesn't destroy it.\n")}})
		rb := body.AppendNewBlock("removed", nil).Body()
		rb.SetAttributeRaw("from", rawTokens(g.state.Terraform))
		rb.AppendNewBlock("lifecycle", nil).Body().SetAttributeValue("destroy", cty.False)
	}
	if len(f.Bytes()) > 0 {
		u.add(base+"/"+fileName, hclwrite.Format(f.Bytes()), false)
	}
	if len(tfvars) > 0 {
		u.add(base+"/"+kindFileBase(kind)+SecretTFVarsSuffix, []byte("# Secret values read from "+in.Description.Title+". Never commit this file.\n"+strings.Join(tfvars, "")), true)
	}
	return nil
}

// SecretTFVarsSuffix ends the files that hold secret variable values; the
// output's .gitignore ignores them and Terraform loads them automatically.
const SecretTFVarsSuffix = ".secrets.auto.tfvars"

// writeBody writes attributes (sorted) and then nested blocks (sorted).
func writeBody(b *hclwrite.Body, prefix string, attrs map[string]any, blocks map[string]bool, h *hclValue) error {
	var plain, nested []string
	for _, k := range sortedKeys(attrs) {
		if blocks[join(prefix, k)] {
			nested = append(nested, k)
		} else {
			plain = append(plain, k)
		}
	}
	for _, k := range plain {
		toks, ok, err := h.tokens(join(prefix, k), attrs[k])
		if err != nil {
			return err
		}
		if ok {
			b.SetAttributeRaw(k, toks)
		}
	}
	for _, k := range nested {
		p := join(prefix, k)
		var items []map[string]any
		switch t := attrs[k].(type) {
		case nil:
		case map[string]any:
			items = []map[string]any{t}
		case []any:
			for i, e := range t {
				m, ok := e.(map[string]any)
				if !ok {
					return fmt.Errorf("%s[%d]: a block must be an object, not %T", p, i, e)
				}
				items = append(items, m)
			}
		default:
			return fmt.Errorf("%s: a block must be an object or a list of objects, not %T", p, t)
		}
		for _, item := range items {
			b.AppendNewline()
			if err := writeBody(b.AppendNewBlock(k, nil).Body(), p, item, blocks, h); err != nil {
				return err
			}
		}
	}
	return nil
}

func join(prefix, attr string) string {
	if prefix == "" {
		return attr
	}
	return prefix + "." + attr
}

func variableBlock(body *hclwrite.Body, name, desc, typ string) {
	body.AppendNewline()
	vb := body.AppendNewBlock("variable", []string{name}).Body()
	vb.SetAttributeValue("description", cty.StringVal(desc))
	vb.SetAttributeRaw("type", rawTokens(typ))
	vb.SetAttributeValue("sensitive", cty.True)
}

func tfvar(name, value string) string {
	f := hclwrite.NewEmptyFile()
	f.Body().SetAttributeRaw(name, stringTokens(value))
	return string(hclwrite.Format(f.Bytes()))
}

func tfvarMap(name string, m map[string]string) string {
	obj := map[string]any{}
	for k, v := range m {
		obj[k] = v
	}
	toks, _, _ := (&hclValue{}).tokens(name, obj)
	f := hclwrite.NewEmptyFile()
	f.Body().SetAttributeRaw(name, toks)
	return string(hclwrite.Format(f.Bytes()))
}

// kindFileBase is a kind's file name: DataSource → data_sources.
func kindFileBase(k *sdk.Kind) string {
	p := k.Plural
	if p == "" {
		p = k.Name + "s"
	}
	return snake(p)
}

func snake(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i, r := range rs {
		if unicode.IsUpper(r) {
			if i > 0 && (unicode.IsLower(rs[i-1]) || unicode.IsDigit(rs[i-1]) || (i+1 < len(rs) && unicode.IsLower(rs[i+1]) && unicode.IsUpper(rs[i-1]))) {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return slug(b.String(), "_")
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func describe(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}
