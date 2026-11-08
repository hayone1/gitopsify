package iac

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/hayone1/gitopsify/internal/hclgen"
	"github.com/hayone1/gitopsify/sdk"
)

// hclValue renders attribute values as HCL expressions. Markers are resolved
// through the callbacks.
type hclValue struct {
	ref    func(r sdk.RefValue) (hclwrite.Tokens, bool)
	secret func(path string, s sdk.SecretValue) (hclwrite.Tokens, bool)
	file   func(path string, f sdk.FileValue) (hclwrite.Tokens, error)
}

// tokens returns the expression for v, or false when the attribute is left
// out (null, or a secret that can't be written).
func (h *hclValue) tokens(path string, v any) (hclwrite.Tokens, bool, error) {
	switch t := v.(type) {
	case nil:
		return nil, false, nil
	case string:
		return stringTokens(t), true, nil
	case bool:
		return hclwrite.TokensForValue(cty.BoolVal(t)), true, nil
	case json.Number:
		return hclwrite.Tokens{{Type: hclsyntax.TokenNumberLit, Bytes: []byte(t.String())}}, true, nil
	case sdk.RefValue:
		if h.ref != nil {
			if toks, ok := h.ref(t); ok {
				return toks, true, nil
			}
		}
		return h.tokens(path, t.Value)
	case sdk.SecretValue:
		if h.secret == nil {
			return nil, false, nil
		}
		toks, ok := h.secret(path, t)
		return toks, ok, nil
	case sdk.JSONValue:
		raw, err := sdk.CompactJSON(t.JSON)
		if err != nil {
			return nil, false, err
		}
		inner, err := hclgen.JSON(raw)
		if err != nil {
			return nil, false, err
		}
		return hclwrite.TokensForFunctionCall("jsonencode", inner), true, nil
	case sdk.FileValue:
		toks, err := h.file(path, t)
		return toks, err == nil, err
	case []any:
		var elems []hclwrite.Tokens
		for _, e := range t {
			toks, ok, err := h.tokens(path, e)
			if err != nil {
				return nil, false, err
			}
			if ok {
				elems = append(elems, toks)
			}
		}
		return hclwrite.TokensForTuple(elems), true, nil
	case map[string]any:
		keys := sortedKeys(t)
		var attrs []hclwrite.ObjectAttrTokens
		for _, k := range keys {
			toks, ok, err := h.tokens(path+"."+k, t[k])
			if err != nil {
				return nil, false, err
			}
			if ok {
				attrs = append(attrs, hclwrite.ObjectAttrTokens{Name: hclgen.Key(k), Value: toks})
			}
		}
		return hclwrite.TokensForObject(attrs), true, nil
	}
	return nil, false, fmt.Errorf("%s: unsupported value %T", path, v)
}

// stringTokens quotes a string, escaping template sequences (${ and %{).
// Strings that end with a newline become heredocs, which keep them readable
// and exact.
func stringTokens(s string) hclwrite.Tokens {
	if !strings.HasSuffix(s, "\n") || strings.ContainsAny(s, "\r") || strings.Count(s, "\n") < 2 {
		return hclwrite.TokensForValue(cty.StringVal(s))
	}
	// HCL ends a heredoc at any line that is the delimiter once trimmed, so
	// no line of the value may be the delimiter, however it is indented
	// (tool data could otherwise close the string and inject blocks).
	delim := "EOT"
	for i := 2; hasLine(s, delim); i++ {
		delim = fmt.Sprintf("EOT%d", i)
	}
	body := strings.NewReplacer("${", "$${", "%{", "%%{").Replace(s)
	return hclwrite.Tokens{
		{Type: hclsyntax.TokenOHeredoc, Bytes: []byte("<<" + delim + "\n")},
		{Type: hclsyntax.TokenStringLit, Bytes: []byte(body)},
		{Type: hclsyntax.TokenCHeredoc, Bytes: []byte(delim)},
	}
}

func hasLine(s, line string) bool {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

// rawTokens is an expression written as is, such as a reference.
func rawTokens(expr string) hclwrite.Tokens {
	return hclwrite.Tokens{{Type: hclsyntax.TokenIdent, Bytes: []byte(expr)}}
}

// fileCall returns file("${path.module}/<p>"); p is a sanitized relative path.
func fileCall(p string) hclwrite.Tokens {
	return rawTokens(`file("${path.module}/` + p + `")`)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func jsonNumber(s string) json.Number { return json.Number(s) }
