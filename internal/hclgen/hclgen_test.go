package hclgen

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

func TestKey(t *testing.T) {
	for k, want := range map[string]string{
		"name": "name", "for": `"for"`, "true": `"true"`, "null": `"null"`, "in": "in",
		"a.b": `"a.b"`, "app.kubernetes.io/name": `"app.kubernetes.io/name"`, "1x": `"1x"`,
	} {
		if got := string(Key(k).Bytes()); got != want {
			t.Errorf("Key(%q) = %s, want %s", k, got, want)
		}
	}
}

// TestAttributesReadBack: what Attributes writes evaluates, without
// variables or functions, to the same JSON, in the same order.
func TestAttributesReadBack(t *testing.T) {
	in := `{"b":1,"a":{"for":"x","z":[true,null,"${t} %{u}"],"y":{}},"c":"line\nnext\n"}`
	out, err := Attributes([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "b = 1\na = {") {
		t.Errorf("order lost:\n%s", out)
	}
	f, diags := hclsyntax.ParseConfig(out, "x.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatalf("%v\n%s", diags, out)
	}
	attrs := f.Body.(*hclsyntax.Body).Attributes
	for name, want := range map[string]string{"b": `1`, "a": `{"for":"x","y":{},"z":[true,null,"${t} %{u}"]}`, "c": `"line\nnext\n"`} {
		v, diags := attrs[name].Expr.Value(nil)
		if diags.HasErrors() {
			t.Fatalf("%s: %v", name, diags)
		}
		got, err := ctyjson.Marshal(v, v.Type())
		if err != nil || string(got) != want {
			t.Errorf("%s = %s, want %s (%v)", name, got, want, err)
		}
	}
	for _, bad := range []string{`[1]`, `{"a.b":1}`, `{"a":1} 2`} {
		if _, err := Attributes([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
