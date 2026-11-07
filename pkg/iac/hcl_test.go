package iac

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
)

// TestStringsRoundTrip writes hostile strings as HCL, parses them back and
// checks that the file holds exactly one attribute with the same value.
func TestStringsRoundTrip(t *testing.T) {
	cases := []string{
		"plain",
		"${var.x} and %{ if true }x%{ endif }",
		"two\nlines\n",
		"x\n  EOT\n}\n}\ndata \"external\" \"pwn\" {program=[\"true\"]}\nresource \"x\" \"y\" {\n  a = <<EOT\n",
		"EOT\nEOT2\n\tEOT3 \n",
		"quote \" backslash \\ tab \t",
		"trailing\n",
		"no trailing\nnewline",
		"$${already} %%{escaped}\n\n",
	}
	for _, s := range cases {
		f := hclwrite.NewEmptyFile()
		f.Body().SetAttributeRaw("v", stringTokens(s))
		src := hclwrite.Format(f.Bytes())
		parsed, diags := hclsyntax.ParseConfig(src, "t.tf", hcl.InitialPos)
		if diags.HasErrors() {
			t.Errorf("%q: doesn't parse: %v\n%s", s, diags, src)
			continue
		}
		body := parsed.Body.(*hclsyntax.Body)
		if len(body.Blocks) != 0 || len(body.Attributes) != 1 {
			t.Errorf("%q: injected structure:\n%s", s, src)
			continue
		}
		v, diags := body.Attributes["v"].Expr.Value(nil)
		if diags.HasErrors() {
			t.Errorf("%q: %v", s, diags)
			continue
		}
		if v.AsString() != s {
			t.Errorf("round trip changed %q into %q\n%s", s, v.AsString(), src)
		}
	}
}

func TestFileCallPathsAreSafe(t *testing.T) {
	for _, p := range []string{`x.j"s`, "a/${x}.json", "b/%{x}.json", `c\d.json`, "e.${x}"} {
		sp := safePath(p, "k")
		if strings.ContainsAny(sp, `"\$%{}`) {
			t.Errorf("safePath(%q) = %q", p, sp)
		}
	}
}
