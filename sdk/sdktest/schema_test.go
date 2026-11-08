package sdktest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hayone1/gitopsify/sdk"
)

const schemaJSON = `{"provider_schemas": {"registry.terraform.io/acme/fake": {"resource_schemas": {
  "fake_thing": {"block": {
    "attributes": {
      "name": {"type": "string", "required": true},
      "count": {"type": "number", "optional": true},
      "token": {"type": "string", "optional": true, "sensitive": true},
      "tags": {"type": ["list", "string"], "optional": true},
      "id": {"type": "string", "computed": true}
    },
    "block_types": {"rule": {"nesting_mode": "list", "block": {"attributes": {"x": {"type": "string", "required": true}}}}}
  }}
}}}}`

var crd = map[string]any{"spec": map[string]any{"group": "fake.m.crossplane.io", "names": map[string]any{"kind": "Thing"},
	"versions": []any{map[string]any{"name": "v1alpha1", "schema": map[string]any{"openAPIV3Schema": map[string]any{
		"properties": map[string]any{"spec": map[string]any{
			"x-kubernetes-validations": []any{map[string]any{"message": "spec.forProvider.name is a required parameter"}},
			"properties": map[string]any{"forProvider": map[string]any{"type": "object", "properties": map[string]any{
				"name":           map[string]any{"type": "string"},
				"count":          map[string]any{"type": "number"},
				"tokenSecretRef": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}, "key": map[string]any{"type": "string"}}},
				"rule":           map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}},
			}}},
		}},
	}}}},
}}

func desc() sdk.Description {
	return sdk.Description{Name: "fake", Terraform: &sdk.TerraformProvider{Name: "fake", Source: "acme/fake"},
		Kinds: []sdk.Kind{{Name: "Thing", Terraform: &sdk.TerraformType{Type: "fake_thing"},
			Crossplane: &sdk.CrossplaneType{APIVersion: "fake.m.crossplane.io/v1alpha1", Kind: "Thing"}}}}
}

func TestValidateTerraform(t *testing.T) {
	inv := sdk.Inventory{Resources: []sdk.Resource{{Key: "a", Kind: "Thing", Attributes: map[string]any{
		"count": "three", "token": "plain", "tags": []any{json.Number("1")}, "id": "x", "bogus": true,
		"rule": []any{map[string]any{}},
	}}}}
	got := strings.Join(ValidateTerraform([]byte(schemaJSON), desc(), inv), "\n")
	for _, want := range []string{"required attribute name", "want a number", "sensitive in the schema", "item: want a string",
		"read-only", "no attribute bogus", "list it in TerraformType.Blocks", "required attribute rule.x"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	ok := sdk.Inventory{Resources: []sdk.Resource{{Key: "b", Kind: "Thing", Attributes: map[string]any{
		"name": "n", "count": json.Number("3"), "token": sdk.Secret("token", "t", "tokenSecretRef", "")}}}}
	if p := ValidateTerraform([]byte(schemaJSON), desc(), ok); len(p) != 0 {
		t.Errorf("unexpected problems: %v", p)
	}
	blocks, err := TerraformBlocks([]byte(schemaJSON), "acme/fake", "fake_thing")
	if err != nil || strings.Join(blocks, ",") != "rule" {
		t.Errorf("blocks %v %v", blocks, err)
	}
}

func TestValidateCrossplane(t *testing.T) {
	inv := sdk.Inventory{Resources: []sdk.Resource{{Key: "a", Kind: "Thing", Attributes: map[string]any{
		"count": "x", "token": sdk.Secret("token", "t", "tokenRef", ""), "extra": "y",
	}}}}
	got := strings.Join(ValidateCrossplane([]map[string]any{crd}, desc(), inv), "\n")
	for _, want := range []string{"required parameter name", "count: want a number", "no field tokenRef", "no field extra"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	ok := sdk.Inventory{Resources: []sdk.Resource{{Key: "b", Kind: "Thing", Attributes: map[string]any{
		"name": "n", "count": json.Number("3"), "token": sdk.Secret("token", "t", "tokenSecretRef", ""),
		"rule": []any{map[string]any{"x": "1"}}}}}}
	k := desc()
	k.Kinds[0].Terraform.Blocks = []string{"rule"}
	if p := ValidateCrossplane([]map[string]any{crd}, k, ok); len(p) != 0 {
		t.Errorf("unexpected problems: %v", p)
	}
}
