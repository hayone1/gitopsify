// Package yamlutil renders Kubernetes objects as YAML the way people write
// them: apiVersion, kind and metadata first, multi-line strings as blocks.
package yamlutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	yamlv3 "go.yaml.in/yaml/v3"
)

// topOrder and metaOrder put the identifying fields first.
var (
	topOrder  = []string{"apiVersion", "kind", "metadata", "spec", "type", "immutable", "data", "stringData", "binaryData"}
	metaOrder = []string{"name", "generateName", "namespace", "labels", "annotations"}
)

// Marshal renders a value as YAML. Maps are emitted with keys sorted, except
// that Kubernetes object keys follow the conventional order.
func Marshal(v any) ([]byte, error) {
	// Normalize through JSON so structs, typed maps and numbers behave.
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	n := node(generic, "")
	var buf bytes.Buffer
	enc := yamlv3.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// MarshalDocs renders several values as one multi-document YAML stream.
func MarshalDocs(vs ...any) ([]byte, error) {
	var out bytes.Buffer
	for i, v := range vs {
		b, err := Marshal(v)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			out.WriteString("---\n")
		}
		out.Write(b)
	}
	return out.Bytes(), nil
}

func node(v any, key string) *yamlv3.Node {
	switch t := v.(type) {
	case map[string]any:
		n := &yamlv3.Node{Kind: yamlv3.MappingNode}
		for _, k := range orderedKeys(t, key) {
			n.Content = append(n.Content, scalar(k), node(t[k], k))
		}
		return n
	case []any:
		n := &yamlv3.Node{Kind: yamlv3.SequenceNode}
		for _, c := range t {
			n.Content = append(n.Content, node(c, ""))
		}
		return n
	case string:
		s := scalar(t)
		if strings.Contains(t, "\n") && !strings.ContainsAny(t, "\t\r") && !hasTrailingSpace(t) {
			s.Style = yamlv3.LiteralStyle
		}
		return s
	case json.Number:
		return &yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!" + numberTag(t), Value: t.String()}
	case bool:
		return &yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(t)}
	case nil:
		return &yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!null", Value: "null"}
	}
	return scalar(fmt.Sprint(v))
}

func numberTag(n json.Number) string {
	if _, err := n.Int64(); err == nil {
		return "int"
	}
	return "float"
}

// yaml11Bools are plain words YAML 1.1 parsers read as booleans. yaml.v3
// follows YAML 1.2 and leaves them unquoted, but kubectl and other tools built
// on sigs.k8s.io/yaml parse YAML 1.1, so a ConfigMap key "n" would become
// "false" (the "Norway problem").
var yaml11Bools = map[string]bool{
	"y": true, "Y": true, "yes": true, "Yes": true, "YES": true,
	"n": true, "N": true, "no": true, "No": true, "NO": true,
	"on": true, "On": true, "ON": true, "off": true, "Off": true, "OFF": true,
}

// YAML11Bool reports whether s is a plain word YAML 1.1 parsers (and so
// sigs.k8s.io/yaml) read as a boolean: such strings must be quoted.
func YAML11Bool(s string) bool { return yaml11Bools[s] }

// scalar returns a string node; yaml.v3 quotes it when it would otherwise
// read back as another type ("true", "1.0", "null").
func scalar(s string) *yamlv3.Node {
	n := &yamlv3.Node{}
	n.SetString(s)
	if yaml11Bools[s] {
		n.Style = yamlv3.DoubleQuotedStyle
	}
	return n
}

func hasTrailingSpace(s string) bool {
	for _, l := range strings.Split(s, "\n") {
		if strings.HasSuffix(l, " ") {
			return true
		}
	}
	return false
}

// orderedKeys sorts the keys of a map; key is the map's own key in its parent.
func orderedKeys(m map[string]any, key string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	var order []string
	_, isAV := m["apiVersion"].(string)
	_, isKind := m["kind"].(string)
	switch {
	case isAV && isKind:
		order = topOrder
	case key == "metadata":
		order = metaOrder
	case key == "":
		// List items read best with their identifying key first
		// (containers, ports, env, volumes).
		order = []string{"name"}
	}
	rank := map[string]int{}
	for i, k := range order {
		rank[k] = i + 1
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, rj := rank[keys[i]], rank[keys[j]]
		switch {
		case ri > 0 && rj > 0:
			return ri < rj
		case ri > 0:
			return true
		case rj > 0:
			return false
		}
		return keys[i] < keys[j]
	})
	return keys
}
