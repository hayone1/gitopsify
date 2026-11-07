// Package hclgen writes JSON values as HCL expressions, keeping the order of
// object keys. gitopsify uses it for Terraform output and for its own files
// written in HCL.
package hclgen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// Key writes an object key as an identifier when it is one, and as a quoted
// string otherwise. true, false and null would be read as values, and for
// would start a for expression.
func Key(k string) hclwrite.Tokens {
	if hclsyntax.ValidIdentifier(k) && k != "true" && k != "false" && k != "null" && k != "for" {
		return hclwrite.TokensForIdentifier(k)
	}
	return hclwrite.TokensForValue(cty.StringVal(k))
}

// JSON converts JSON text into an HCL expression, keeping the order of
// object keys. Strings are quoted with template sequences (${ and %{)
// escaped, so they read back as the same text.
func JSON(raw []byte) (hclwrite.Tokens, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	toks, err := value(d)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return toks, nil
}

// Attributes writes a JSON object as top-level HCL attributes (name = value),
// in the object's order. The file reads back as the same object with
// attribute-only HCL, as gitopsify's files are written.
func Attributes(raw []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return nil, fmt.Errorf("want a JSON object")
	}
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	for d.More() {
		kt, err := d.Token()
		if err != nil {
			return nil, err
		}
		k, _ := kt.(string)
		if !hclsyntax.ValidIdentifier(k) {
			return nil, fmt.Errorf("%q can't be an HCL attribute name", k)
		}
		v, err := value(d)
		if err != nil {
			return nil, err
		}
		body.SetAttributeRaw(k, v)
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return hclwrite.Format(f.Bytes()), nil
}

func value(d *json.Decoder) (hclwrite.Tokens, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch v := t.(type) {
	case json.Delim:
		switch v {
		case '{':
			var attrs []hclwrite.ObjectAttrTokens
			for d.More() {
				kt, err := d.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				val, err := value(d)
				if err != nil {
					return nil, err
				}
				attrs = append(attrs, hclwrite.ObjectAttrTokens{Name: Key(k), Value: val})
			}
			if _, err := d.Token(); err != nil {
				return nil, err
			}
			return hclwrite.TokensForObject(attrs), nil
		case '[':
			var elems []hclwrite.Tokens
			for d.More() {
				val, err := value(d)
				if err != nil {
					return nil, err
				}
				elems = append(elems, val)
			}
			if _, err := d.Token(); err != nil {
				return nil, err
			}
			return hclwrite.TokensForTuple(elems), nil
		}
	case string:
		return hclwrite.TokensForValue(cty.StringVal(v)), nil
	case json.Number:
		return hclwrite.Tokens{{Type: hclsyntax.TokenNumberLit, Bytes: []byte(v.String())}}, nil
	case bool:
		return hclwrite.TokensForValue(cty.BoolVal(v)), nil
	case nil:
		return hclwrite.TokensForIdentifier("null"), nil
	}
	return nil, fmt.Errorf("unexpected JSON token %v", t)
}
