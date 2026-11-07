package sdktest

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hayone1/gitopsify/sdk"
)

// Validating against the upstream schemas is how a provider proves its
// mapping without running Terraform or Crossplane: keep the output of
// `terraform providers schema -json` and the CRDs of the Crossplane package
// (for the pinned versions) as test data, and check the inventory against
// them.

type tfSchema struct {
	ProviderSchemas map[string]struct {
		ResourceSchemas map[string]struct {
			Block tfBlock `json:"block"`
		} `json:"resource_schemas"`
	} `json:"provider_schemas"`
}

type tfBlock struct {
	Attributes map[string]tfAttr `json:"attributes"`
	BlockTypes map[string]struct {
		NestingMode string  `json:"nesting_mode"`
		MinItems    int     `json:"min_items"`
		MaxItems    int     `json:"max_items"`
		Block       tfBlock `json:"block"`
	} `json:"block_types"`
}

type tfAttr struct {
	Type      json.RawMessage `json:"type"`
	Required  bool            `json:"required"`
	Optional  bool            `json:"optional"`
	Computed  bool            `json:"computed"`
	Sensitive bool            `json:"sensitive"`
}

// TerraformBlocks returns the block paths of a resource type in a schema
// dump, for TerraformType.Blocks.
func TerraformBlocks(schemaJSON []byte, source, typ string) ([]string, error) {
	b, err := resourceBlock(schemaJSON, source, typ)
	if err != nil {
		return nil, err
	}
	var out []string
	var walk func(prefix string, b tfBlock)
	walk = func(prefix string, b tfBlock) {
		for name, bt := range b.BlockTypes {
			p := join(prefix, name)
			out = append(out, p)
			walk(p, bt.Block)
		}
	}
	walk("", *b)
	sort.Strings(out)
	return out, nil
}

func resourceBlock(schemaJSON []byte, source, typ string) (*tfBlock, error) {
	var s tfSchema
	if err := json.Unmarshal(schemaJSON, &s); err != nil {
		return nil, fmt.Errorf("reading the schema: %w", err)
	}
	for addr, ps := range s.ProviderSchemas {
		if addr == source || strings.HasSuffix(addr, "/"+source) {
			rs, ok := ps.ResourceSchemas[typ]
			if !ok {
				return nil, fmt.Errorf("the schema of %s has no resource %s", source, typ)
			}
			return &rs.Block, nil
		}
	}
	return nil, fmt.Errorf("the schema has no provider %s", source)
}

// ValidateTerraform checks every resource against the Terraform schema dump
// (`terraform providers schema -json`) of the provider the description names.
func ValidateTerraform(schemaJSON []byte, d sdk.Description, inv sdk.Inventory) []string {
	var p problems
	if d.Terraform == nil {
		return nil
	}
	blocks := map[string]*tfBlock{}
	for _, k := range d.Kinds {
		if k.Terraform == nil {
			continue
		}
		b, err := resourceBlock(schemaJSON, d.Terraform.Source, k.Terraform.Type)
		if err != nil {
			p.add("kind %s: %v", k.Name, err)
			continue
		}
		blocks[k.Name] = b
		declared := map[string]bool{}
		for _, path := range k.Terraform.Blocks {
			declared[path] = true
			if !hasBlock(b, path) {
				p.add("kind %s: %q is listed as a block but the schema has no such block", k.Name, path)
			}
		}
		_ = declared
	}
	for _, r := range inv.Resources {
		k, ok := d.Kind(r.Kind)
		if !ok || k.Terraform == nil || blocks[r.Kind] == nil {
			continue
		}
		declared := map[string]bool{}
		for _, path := range k.Terraform.Blocks {
			declared[path] = true
		}
		checkTFBody(&p, "resource "+r.Key, "", r.Attributes, blocks[r.Kind], declared, r.Attributes != nil)
	}
	return p
}

func hasBlock(b *tfBlock, path string) bool {
	parts := strings.Split(path, ".")
	for _, part := range parts {
		bt, ok := b.BlockTypes[part]
		if !ok {
			return false
		}
		b = &bt.Block
	}
	return true
}

func checkTFBody(p *problems, at, prefix string, attrs map[string]any, b *tfBlock, declared map[string]bool, full bool) {
	if full {
		for name, a := range b.Attributes {
			if a.Required {
				if _, ok := attrs[name]; !ok {
					p.add("%s: required attribute %s is missing", at, join(prefix, name))
				}
			}
		}
		for name, bt := range b.BlockTypes {
			if bt.MinItems > 0 {
				if _, ok := attrs[name]; !ok {
					p.add("%s: required block %s is missing", at, join(prefix, name))
				}
			}
		}
	}
	for name, v := range attrs {
		path := join(prefix, name)
		if bt, ok := b.BlockTypes[name]; ok {
			if !declared[path] {
				p.add("%s: %s is a block in the schema; list it in TerraformType.Blocks", at, path)
			}
			var items []any
			switch t := v.(type) {
			case []any:
				items = t
			case map[string]any:
				items = []any{t}
			default:
				p.add("%s: block %s must be an object or a list of objects, not %T", at, path, v)
				continue
			}
			if bt.MaxItems > 0 && len(items) > bt.MaxItems {
				p.add("%s: block %s allows at most %d, got %d", at, path, bt.MaxItems, len(items))
			}
			for i, it := range items {
				m, ok := it.(map[string]any)
				if !ok {
					p.add("%s: %s[%d] must be an object", at, path, i)
					continue
				}
				checkTFBody(p, at, path, m, &bt.Block, declared, true)
			}
			continue
		}
		a, ok := b.Attributes[name]
		if !ok {
			p.add("%s: the schema has no attribute %s", at, path)
			continue
		}
		if a.Computed && !a.Optional && !a.Required {
			p.add("%s: %s is read-only (computed) and can't be set", at, path)
		}
		if a.Sensitive {
			if _, isSecret := v.(sdk.SecretValue); !isSecret && v != nil {
				p.add("%s: %s is sensitive in the schema; give it with sdk.Secret so it stays out of Git", at, path)
			}
		}
		if msg := tfTypeMismatch(a.Type, v); msg != "" {
			p.add("%s: %s: %s", at, path, msg)
		}
	}
}

// tfTypeMismatch says why v can't be a value of the Terraform type t.
func tfTypeMismatch(t json.RawMessage, v any) string {
	switch v.(type) {
	case nil, sdk.RefValue, sdk.SecretValue:
		return ""
	}
	var simple string
	if json.Unmarshal(t, &simple) == nil {
		switch simple {
		case "string":
			switch v.(type) {
			case string, sdk.JSONValue, sdk.FileValue:
				return ""
			}
		case "number":
			if _, ok := v.(json.Number); ok {
				return ""
			}
		case "bool":
			if _, ok := v.(bool); ok {
				return ""
			}
		case "dynamic":
			return ""
		}
		return fmt.Sprintf("want a %s, got %T", simple, v)
	}
	var complex []json.RawMessage
	if json.Unmarshal(t, &complex) != nil || len(complex) != 2 {
		return ""
	}
	var kind string
	_ = json.Unmarshal(complex[0], &kind)
	switch kind {
	case "list", "set":
		items, ok := v.([]any)
		if !ok {
			return fmt.Sprintf("want a %s, got %T", kind, v)
		}
		for _, it := range items {
			if msg := tfTypeMismatch(complex[1], it); msg != "" {
				return "item: " + msg
			}
		}
	case "map":
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Sprintf("want a map, got %T", v)
		}
		for _, it := range m {
			if msg := tfTypeMismatch(complex[1], it); msg != "" {
				return "value: " + msg
			}
		}
	case "object":
		if _, ok := v.(map[string]any); !ok {
			return fmt.Sprintf("want an object, got %T", v)
		}
	}
	return ""
}

// ValidateCrossplane derives every resource's spec.forProvider and checks it
// against the CRDs (decoded YAML or JSON documents) of the Crossplane
// provider: field names (references and secret references included), types
// and required parameters.
func ValidateCrossplane(crds []map[string]any, d sdk.Description, inv sdk.Inventory) []string {
	var p problems
	schemas := map[string]map[string]any{}
	for _, crd := range crds {
		spec, _ := crd["spec"].(map[string]any)
		group, _ := spec["group"].(string)
		names, _ := spec["names"].(map[string]any)
		kind, _ := names["kind"].(string)
		versions, _ := spec["versions"].([]any)
		for _, v := range versions {
			vm, _ := v.(map[string]any)
			name, _ := vm["name"].(string)
			schema, _ := vm["schema"].(map[string]any)
			if s, ok := schema["openAPIV3Schema"].(map[string]any); ok {
				schemas[group+"/"+name+"/"+kind] = s
			}
		}
	}
	for _, k := range d.Kinds {
		if k.Crossplane != nil {
			if _, ok := schemas[k.Crossplane.APIVersion+"/"+k.Crossplane.Kind]; !ok {
				p.add("kind %s: no CRD for %s %s", k.Name, k.Crossplane.APIVersion, k.Crossplane.Kind)
			}
		}
	}
	for _, r := range inv.Resources {
		k, ok := d.Kind(r.Kind)
		if !ok || k.Crossplane == nil || r.Attributes == nil {
			continue
		}
		root, ok := schemas[k.Crossplane.APIVersion+"/"+k.Crossplane.Kind]
		if !ok {
			continue
		}
		fp, _, err := sdk.ForProvider(k, &r, everything{})
		if err != nil {
			p.add("resource %s: %v", r.Key, err)
			continue
		}
		spec := prop(root, "spec")
		fps := prop(spec, "forProvider")
		at := "resource " + r.Key + ": spec.forProvider"
		checkOpenAPI(&p, at, fp, fps)
		if vals, ok := spec["x-kubernetes-validations"].([]any); ok {
			for _, v := range vals {
				msg, _ := v.(map[string]any)["message"].(string)
				field, ok := strings.CutPrefix(msg, "spec.forProvider.")
				if !ok {
					continue
				}
				field, ok = strings.CutSuffix(field, " is a required parameter")
				if !ok {
					continue
				}
				if _, has := fp[field]; !has {
					p.add("%s: required parameter %s is missing", at, field)
				}
			}
		}
	}
	return p
}

func prop(s map[string]any, name string) map[string]any {
	props, _ := s["properties"].(map[string]any)
	out, _ := props[name].(map[string]any)
	return out
}

func checkOpenAPI(p *problems, at string, v any, s map[string]any) {
	if s == nil {
		return
	}
	if pres, _ := s["x-kubernetes-preserve-unknown-fields"].(bool); pres {
		return
	}
	switch s["type"] {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			p.add("%s: want an object, got %T", at, v)
			return
		}
		props, hasProps := s["properties"].(map[string]any)
		extra, _ := s["additionalProperties"].(map[string]any)
		for k, e := range m {
			if ps, ok := props[k].(map[string]any); ok {
				checkOpenAPI(p, at+"."+k, e, ps)
				continue
			}
			if extra != nil {
				checkOpenAPI(p, at+"."+k, e, extra)
				continue
			}
			if hasProps {
				p.add("%s: the CRD has no field %s", at, k)
			}
		}
	case "array":
		items, ok := v.([]any)
		if !ok {
			p.add("%s: want an array, got %T", at, v)
			return
		}
		is, _ := s["items"].(map[string]any)
		for i, it := range items {
			checkOpenAPI(p, fmt.Sprintf("%s[%d]", at, i), it, is)
		}
	case "string":
		if _, ok := v.(string); !ok {
			p.add("%s: want a string, got %T", at, v)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			p.add("%s: want a boolean, got %T", at, v)
		}
	case "integer":
		if _, ok := v.(int64); !ok {
			p.add("%s: want an integer, got %T", at, v)
		}
	case "number":
		switch v.(type) {
		case int64, float64:
		default:
			p.add("%s: want a number, got %T", at, v)
		}
	}
}

func join(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}
