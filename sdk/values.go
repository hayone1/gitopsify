package sdk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Attribute values are JSON values or one of these markers. A marker is a JSON
// object with one of the keys "$ref", "$secret", "$json" or "$file"; markers
// are recognised where an attribute value is expected (not inside the content
// of a $json or $file marker).

// RefValue refers to an attribute of another resource. In Terraform it becomes
// a reference expression when both resources are in the same workspace; in
// Crossplane it becomes the *Ref field named by Crossplane. When the target
// isn't exported (or lives elsewhere), the literal Value is written instead.
type RefValue struct {
	// Ref is the key of the target resource.
	Ref string `json:"$ref"`
	// Attr is the target's attribute, such as "uid" or "id".
	Attr string `json:"attr"`
	// Crossplane is the Crossplane reference field that replaces this field,
	// such as "folderRef". Empty means the literal value is always used.
	Crossplane string `json:"crossplane,omitempty"`
	// Value is the target attribute's current value.
	Value any `json:"value"`
}

// Ref returns a reference to the attribute attr of the resource with the given
// key, whose current value is value. crossplaneRef names the Crossplane
// reference field ("folderRef"), or is empty.
func Ref(key, attr string, value any, crossplaneRef string) RefValue {
	return RefValue{Ref: key, Attr: attr, Crossplane: crossplaneRef, Value: value}
}

// SecretValue is a sensitive value. In Terraform it becomes a sensitive
// variable, in Crossplane a Secret referenced by the field named by
// Crossplane. Values are only written to git-ignored files.
type SecretValue struct {
	// Secret names the value within its resource, such as "url" or
	// "basic_auth_password"; gitopsify makes variable and Secret names unique.
	Secret string `json:"$secret"`
	// Value is a string, or a map of strings for map attributes; nil when the
	// tool doesn't return the value (the attribute is then left out and
	// listed in the report).
	Value any `json:"value,omitempty"`
	// Crossplane is the *SecretRef field that replaces this field, such as
	// "urlSecretRef".
	Crossplane string `json:"crossplane,omitempty"`
	// Description is shown in the report and on the variable.
	Description string `json:"description,omitempty"`
}

// Secret returns a sensitive value. value is a string, a map[string]string,
// or nil when the tool doesn't return it.
func Secret(name string, value any, crossplaneRef, description string) SecretValue {
	return SecretValue{Secret: name, Value: value, Crossplane: crossplaneRef, Description: description}
}

// JSONValue is a string attribute that holds JSON, such as a data source's
// json_data_encoded. Terraform gets jsonencode({...}), Crossplane the JSON
// text.
type JSONValue struct {
	JSON any `json:"$json"`
}

// JSON returns a JSON-encoded string attribute. v may be RawJSON to keep the
// order of keys.
func JSON(v any) JSONValue { return JSONValue{JSON: v} }

// FileValue is a string attribute whose content goes into its own file, such
// as a dashboard's JSON. Terraform reads it with file(); Crossplane gets the
// content inline.
type FileValue struct {
	// File is the path of the file relative to the workspace, with forward
	// slashes, such as "dashboards/sre/kubernetes.json".
	File string `json:"$file"`
	// Content is a string (written as is) or a JSON value (written indented).
	Content any `json:"content"`
}

// File returns an attribute whose content is stored in a file at path.
func File(path string, content any) FileValue { return FileValue{File: path, Content: content} }

// Marker keys.
const (
	keyRef    = "$ref"
	keySecret = "$secret"
	keyJSON   = "$json"
	keyFile   = "$file"
)

// DecodeValue decodes one attribute value as gitopsify sees it: nil, bool,
// json.Number, string, []any, map[string]any, RefValue, SecretValue,
// JSONValue or FileValue. The content of JSONValue and FileValue stays
// RawJSON, so key order is kept.
func DecodeValue(data []byte) (any, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	switch data[0] {
	case '{':
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return nil, err
		}
		if v, ok, err := decodeMarker(fields); ok || err != nil {
			return v, err
		}
		out := make(map[string]any, len(fields))
		for k, raw := range fields {
			v, err := DecodeValue(raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = v
		}
		return out, nil
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, err
		}
		out := make([]any, 0, len(items))
		for i, raw := range items {
			v, err := DecodeValue(raw)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out = append(out, v)
		}
		return out, nil
	default:
		d := json.NewDecoder(bytes.NewReader(data))
		d.UseNumber()
		var v any
		err := d.Decode(&v)
		return v, err
	}
}

func decodeMarker(fields map[string]json.RawMessage) (any, bool, error) {
	str := func(k string) (string, error) {
		raw, ok := fields[k]
		if !ok {
			return "", nil
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("%s must be a string", k)
		}
		return s, nil
	}
	plain := func(k string) (any, error) {
		raw, ok := fields[k]
		if !ok {
			return nil, nil
		}
		return DecodeValue(raw)
	}
	var markers []string
	for k := range fields {
		if !strings.HasPrefix(k, "$") {
			continue
		}
		switch k {
		case keyRef, keySecret, keyJSON, keyFile:
			markers = append(markers, k)
		default:
			// A marker from a newer SDK (or a typo) must not be written out as
			// a plain value: it might be a secret.
			return nil, true, fmt.Errorf("unknown marker %q (keys starting with $ are reserved for markers)", k)
		}
	}
	sort.Strings(markers)
	switch len(markers) {
	case 0:
		return nil, false, nil
	case 1:
	default:
		return nil, true, fmt.Errorf("value has several markers: %s", strings.Join(markers, ", "))
	}
	switch markers[0] {
	case keyRef:
		var r RefValue
		var err error
		if r.Ref, err = str(keyRef); err != nil {
			return nil, true, err
		}
		if r.Attr, err = str("attr"); err != nil {
			return nil, true, err
		}
		if r.Crossplane, err = str("crossplane"); err != nil {
			return nil, true, err
		}
		if r.Value, err = plain("value"); err != nil {
			return nil, true, err
		}
		return r, true, nil
	case keySecret:
		var s SecretValue
		var err error
		if s.Secret, err = str(keySecret); err != nil {
			return nil, true, err
		}
		if s.Crossplane, err = str("crossplane"); err != nil {
			return nil, true, err
		}
		if s.Description, err = str("description"); err != nil {
			return nil, true, err
		}
		if raw, ok := fields["value"]; ok && string(bytes.TrimSpace(raw)) != "null" {
			var str string
			var m map[string]string
			switch {
			case json.Unmarshal(raw, &str) == nil:
				s.Value = str
			case json.Unmarshal(raw, &m) == nil:
				s.Value = m
			default:
				return nil, true, fmt.Errorf("$secret value must be a string or an object of strings")
			}
		}
		return s, true, nil
	case keyJSON:
		return JSONValue{JSON: RawJSON(bytes.TrimSpace(fields[keyJSON]))}, true, nil
	default:
		var f FileValue
		var err error
		if f.File, err = str(keyFile); err != nil {
			return nil, true, err
		}
		if raw, ok := fields["content"]; ok {
			f.Content = RawJSON(bytes.TrimSpace(raw))
		}
		return f, true, nil
	}
}

// UnmarshalJSON decodes a resource, decoding attribute values with
// DecodeValue.
func (r *Resource) UnmarshalJSON(data []byte) error {
	type plain Resource
	var aux struct {
		plain
		Attributes json.RawMessage `json:"attributes"`
		Crossplane json.RawMessage `json:"crossplane"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*r = Resource(aux.plain)
	var err error
	if r.Attributes, err = decodeObject(aux.Attributes); err != nil {
		return fmt.Errorf("resource %s: attributes: %w", r.Key, err)
	}
	if r.Crossplane, err = decodeObject(aux.Crossplane); err != nil {
		return fmt.Errorf("resource %s: crossplane: %w", r.Key, err)
	}
	return nil
}

func decodeObject(raw json.RawMessage) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	v, err := DecodeValue(raw)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("must be an object")
	}
	return m, nil
}

// FileText returns the text to write for a file's content: strings as they
// are, JSON values indented by two spaces with a final newline.
func FileText(content any) ([]byte, error) {
	raw, err := toRaw(content)
	if err != nil {
		return nil, err
	}
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []byte(s), nil
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return unescapeHTML(buf.Bytes()), nil
}

// CompactJSON returns v (RawJSON or any Go value) as compact JSON text, with
// <, > and & unescaped.
func CompactJSON(v any) ([]byte, error) {
	raw, err := toRaw(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, err
	}
	return unescapeHTML(buf.Bytes()), nil
}

func toRaw(v any) ([]byte, error) {
	if raw, ok := v.(RawJSON); ok {
		return bytes.TrimSpace(raw), nil
	}
	return marshal(v)
}

// marshal encodes like json.Marshal without escaping <, > and &.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

// unescapeHTML turns the <, > and & escapes that Go's JSON
// encoder writes (and many APIs return) back into <, > and &, so files stay
// readable. Escaped backslashes are respected.
func unescapeHTML(b []byte) []byte {
	if !bytes.Contains(b, []byte(`\u00`)) {
		return b
	}
	out := make([]byte, 0, len(b))
	inString := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			out = append(out, c)
			continue
		}
		switch c {
		case '"':
			inString = false
			out = append(out, c)
		case '\\':
			if i+5 < len(b) && b[i+1] == 'u' {
				switch strings.ToLower(string(b[i+2 : i+6])) {
				case "003c":
					out = append(out, '<')
					i += 5
					continue
				case "003e":
					out = append(out, '>')
					i += 5
					continue
				case "0026":
					out = append(out, '&')
					i += 5
					continue
				}
			}
			out = append(out, c)
			if i+1 < len(b) {
				out = append(out, b[i+1])
				i++
			}
		default:
			out = append(out, c)
		}
	}
	return out
}

// Summary returns r as discover sends it when the request asks for
// DetailSummary: without attributes and Crossplane body, but with the keys of
// the resources they reference added to DependsOn, so gitopsify still pulls in
// what a picked object needs.
func (r Resource) Summary() Resource {
	seen := map[string]bool{}
	deps := make([]string, 0, len(r.DependsOn))
	for _, d := range r.DependsOn {
		if !seen[d] {
			seen[d] = true
			deps = append(deps, d)
		}
	}
	var refs []string
	for _, body := range []map[string]any{r.Attributes, r.Crossplane} {
		collectRefs(asSent(body), &refs)
	}
	sort.Strings(refs)
	for _, k := range refs {
		if !seen[k] && k != r.Key {
			seen[k] = true
			deps = append(deps, k)
		}
	}
	if len(deps) == 0 {
		deps = nil
	}
	r.Attributes, r.Crossplane, r.DependsOn = nil, nil, deps
	return r
}

// asSent returns v as gitopsify decodes it, so markers inside typed Go
// values ([]map[string]any, map[string]sdk.RefValue, structs) are found too.
func asSent(v map[string]any) any {
	if v == nil {
		return nil
	}
	b, err := marshal(v)
	if err != nil {
		return v
	}
	d, err := DecodeValue(b)
	if err != nil {
		return v
	}
	return d
}

// collectRefs appends the keys of the $ref markers in v.
func collectRefs(v any, out *[]string) {
	switch t := v.(type) {
	case RefValue:
		*out = append(*out, t.Ref)
	case map[string]any:
		for _, e := range t {
			collectRefs(e, out)
		}
	case []any:
		for _, e := range t {
			collectRefs(e, out)
		}
	}
}
