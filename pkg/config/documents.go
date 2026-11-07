package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	yamlv3 "go.yaml.in/yaml/v3"
	"sigs.k8s.io/yaml"

	"github.com/hayone1/gitopsify/internal/hclgen"
	"github.com/hayone1/gitopsify/pkg/yamlutil"
)

// gitopsify's files are resources in the Kubernetes style (apiVersion, kind,
// metadata, spec), like Grafana's dashboards outside Kubernetes. That makes
// them declarative, lets editors validate them with JSON Schema, and lets a
// controller reconcile them later. They can be written in YAML (several
// documents separated by ---), JSON (an object, an array, or a Kubernetes
// List) or HCL (attributes, as in the JSON form; "items = [...]" for
// several).

// ObjectMeta is the metadata of a gitopsify resource.
type ObjectMeta struct {
	// Name identifies the resource among others of its kind.
	Name        string            `json:"name,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

var resourceName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// validate checks the name the way Kubernetes checks most resources' (a DNS
// subdomain), so that a controller can reconcile the same files later.
func (m ObjectMeta) validate() error {
	if m.Name != "" && (len(m.Name) > 253 || !resourceName.MatchString(m.Name)) {
		return fmt.Errorf("metadata.name %q must be lowercase letters, digits, '-' and '.', starting and ending with a letter or digit", m.Name)
	}
	return nil
}

// Document is one resource read from a file.
type Document struct {
	APIVersion string
	Kind       string
	Name       string
	// JSON is the resource as JSON.
	JSON []byte
	// Raw is the resource as written (YAML, or JSON), which decoding uses so
	// that values convert to the fields' types and duplicate keys are errors,
	// as with sigs.k8s.io/yaml.
	Raw []byte
}

// head is what identifies a resource. It is read leniently (any scalar
// types, any metadata): the strict decoding of the resource reports type
// errors with the field's path.
type head struct {
	APIVersion any             `json:"apiVersion"`
	Kind       any             `json:"kind"`
	Metadata   json.RawMessage `json:"metadata"`
	Items      []json.RawMessage
}

func (h head) name() string {
	var m struct {
		Name any `json:"name"`
	}
	if json.Unmarshal(h.Metadata, &m) != nil {
		return ""
	}
	return scalarText(m.Name)
}

// scalarText formats a JSON scalar the way type-directed decoding would turn
// it into a string field.
func scalarText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

// ReadDocuments reads every resource in a file: YAML or JSON, or HCL when
// the name ends in .hcl.
func ReadDocuments(path string) ([]Document, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	return ParseDocuments(path, data)
}

// ParseDocuments parses the resources in data; name decides the syntax
// (.hcl) and appears in errors.
func ParseDocuments(name string, data []byte) ([]Document, error) {
	var raws [][]byte
	var err error
	if strings.EqualFold(filepath.Ext(name), ".hcl") {
		raws, err = hclDocuments(name, data) // errors carry the file and line
	} else if raws, err = yamlDocuments(data); err != nil {
		err = fmt.Errorf("%s: %w", name, err)
	}
	if err != nil {
		return nil, err
	}
	var out []Document
	for _, raw := range raws {
		j, err := yaml.YAMLToJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		docs, err := expand(j)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		// A single resource keeps its original text; items of a list are
		// decoded from their JSON.
		if len(docs) == 1 && bytes.Equal(docs[0].JSON, bytes.TrimSpace(j)) {
			docs[0].Raw = raw
		}
		out = append(out, docs...)
	}
	return out, nil
}

// expand turns a List (or a JSON array) into its items.
func expand(raw []byte) ([]Document, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, err
		}
		var out []Document
		for _, it := range items {
			docs, err := expand(it)
			if err != nil {
				return nil, err
			}
			out = append(out, docs...)
		}
		return out, nil
	}
	var h head
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, fmt.Errorf("a resource must be an object: %w", err)
	}
	kind := scalarText(h.Kind)
	if kind == "List" || (kind == "" && h.Items != nil) {
		var out []Document
		for _, it := range h.Items {
			docs, err := expand(it)
			if err != nil {
				return nil, err
			}
			out = append(out, docs...)
		}
		return out, nil
	}
	return []Document{{APIVersion: scalarText(h.APIVersion), Kind: kind, Name: h.name(), JSON: raw, Raw: raw}}, nil
}

func yamlDocuments(data []byte) ([][]byte, error) {
	dec := yamlv3.NewDecoder(bytes.NewReader(data))
	var out [][]byte
	for {
		var n yamlv3.Node
		if err := dec.Decode(&n); err != nil {
			if err == io.EOF {
				return out, nil
			}
			return nil, err
		}
		b, err := yamlv3.Marshal(&n)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
}

// hclDocuments reads HCL written with attributes only: the top-level
// attributes are one resource, or "items" holds several.
func hclDocuments(name string, data []byte) ([][]byte, error) {
	f, diags := hclsyntax.ParseConfig(data, name, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diags
	}
	body := f.Body.(*hclsyntax.Body)
	if len(body.Blocks) > 0 {
		b := body.Blocks[0]
		return nil, fmt.Errorf("%s: write %s as an attribute (%s = { ... }); gitopsify's HCL mirrors its JSON form", b.DefRange().String(), b.Type, b.Type)
	}
	obj := map[string]json.RawMessage{}
	for name, attr := range body.Attributes {
		v, diags := attr.Expr.Value(nil)
		if diags.HasErrors() {
			return nil, fmt.Errorf("%s: only literal values are allowed: %w", attr.SrcRange.String(), diags)
		}
		b, err := ctyjson.Marshal(v, v.Type())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", attr.SrcRange.String(), err)
		}
		obj[name] = b
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return [][]byte{escapeForYAML(b)}, nil
}

// escapeForYAML escapes the characters encoding/json leaves raw but the YAML
// reader refuses (DEL, C1 controls, U+FFFE and U+FFFF). In JSON text they
// can only appear inside strings, where \uXXXX means the same.
func escapeForYAML(b []byte) []byte {
	if !bytes.ContainsFunc(b, yamlRefuses) {
		return b
	}
	var out bytes.Buffer
	for _, r := range string(b) {
		if yamlRefuses(r) {
			fmt.Fprintf(&out, "\\u%04x", r)
		} else {
			out.WriteRune(r)
		}
	}
	return out.Bytes()
}

func yamlRefuses(r rune) bool {
	return r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == 0xfffe || r == 0xffff
}

// Find returns the documents of a kind; with name set, only that one.
func Find(docs []Document, kind, name string) []Document {
	var out []Document
	for _, d := range docs {
		if d.Kind == kind && (name == "" || d.Name == name) {
			out = append(out, d)
		}
	}
	return out
}

// decodeStrict decodes YAML or JSON into v like sigs.k8s.io/yaml: values
// convert to the fields' types, unknown fields and duplicate keys are errors.
func decodeStrict(data []byte, v any) error {
	return yaml.UnmarshalStrict(data, v)
}

// Encode renders a resource in the syntax of the file it goes to, which is
// the syntax ReadDocuments expects there: HCL attributes for .hcl, JSON for
// .json, YAML otherwise. Fields keep their documented order. comment
// ("# ..." lines) heads YAML and HCL files, schemaHeader (a modeline for YAML
// editors) YAML files only; JSON has no comments. HCL stores strings in
// Unicode normal form C, so decomposed accents come back composed.
func Encode(path string, v any, schemaHeader, comment string) ([]byte, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
		return escapeForYAML(buf.Bytes()), nil
	case ".hcl":
		j, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		b, err := hclgen.Attributes(j)
		if err != nil {
			return nil, err
		}
		return append([]byte(comment), b...), nil
	}
	b, err := marshalOrdered(v)
	if err != nil {
		return nil, err
	}
	return append([]byte(schemaHeader+comment), b...), nil
}

// marshalOrdered renders v as block-style YAML keeping the order of its JSON
// fields (struct order), which yaml.Marshal would sort.
func marshalOrdered(v any) ([]byte, error) {
	j, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var n yamlv3.Node
	if err := yamlv3.Unmarshal(escapeForYAML(j), &n); err != nil {
		return nil, err
	}
	var block func(*yamlv3.Node)
	block = func(n *yamlv3.Node) {
		n.Style &^= yamlv3.FlowStyle
		if n.Kind == yamlv3.ScalarNode && n.Tag == "!!str" {
			// Plain unless yaml.v3 (YAML 1.2) or the YAML 1.1 reader gitopsify
			// reads files with would take the text for another type.
			n.Style = 0
			if yamlutil.YAML11Bool(n.Value) || n.Value == "<<" { // << would merge maps
				n.Style = yamlv3.DoubleQuotedStyle
			}
		}
		for _, c := range n.Content {
			block(c)
		}
	}
	block(&n)
	var buf bytes.Buffer
	enc := yamlv3.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&n); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}
