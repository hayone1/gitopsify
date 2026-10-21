package transform

import (
	"fmt"
	"strconv"
	"strings"
)

// segment is one step of a field path: a map key, a list index, or a list
// item selected by "key=value".
type segment struct {
	key      string
	index    int
	hasIndex bool
	selKey   string
	selValue string
}

// parsePath parses paths such as "spec.replicas",
// "spec.template.spec.containers[0].image" and
// "spec.template.spec.containers[name=web].image". Keys containing dots can
// be quoted: metadata.annotations["example.com/key"].
func parsePath(p string) ([]segment, error) {
	var out []segment
	i := 0
	for i < len(p) {
		switch p[i] {
		case '.':
			i++
		case '[':
			end := strings.IndexByte(p[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("path %q: unclosed [", p)
			}
			inner := p[i+1 : i+end]
			i += end + 1
			switch {
			case strings.HasPrefix(inner, `"`) && strings.HasSuffix(inner, `"`) && len(inner) >= 2:
				out = append(out, segment{key: inner[1 : len(inner)-1]})
			case strings.Contains(inner, "="):
				k, v, _ := strings.Cut(inner, "=")
				out = append(out, segment{selKey: k, selValue: v})
			default:
				n, err := strconv.Atoi(inner)
				if err != nil {
					return nil, fmt.Errorf("path %q: bad index %q", p, inner)
				}
				out = append(out, segment{index: n, hasIndex: true})
			}
		default:
			j := i
			for j < len(p) && p[j] != '.' && p[j] != '[' {
				j++
			}
			out = append(out, segment{key: p[i:j]})
			i = j
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty path")
	}
	return out, nil
}

// setPath sets the value at path, creating intermediate maps.
func setPath(root map[string]any, path []segment, value any) error {
	var cur any = root
	for i, s := range path {
		last := i == len(path)-1
		switch {
		case s.key != "":
			m, ok := cur.(map[string]any)
			if !ok {
				return fmt.Errorf("%s is not an object", s.key)
			}
			if last {
				m[s.key] = value
				return nil
			}
			next, ok := m[s.key]
			if !ok || next == nil {
				if path[i+1].key != "" {
					next = map[string]any{}
				} else {
					return fmt.Errorf("%s does not exist", s.key)
				}
				m[s.key] = next
			}
			cur = next
		default:
			l, ok := cur.([]any)
			if !ok {
				return fmt.Errorf("not a list")
			}
			idx := findIndex(l, s)
			if idx < 0 {
				return fmt.Errorf("no list item matches %s", describe(s))
			}
			if last {
				l[idx] = value
				return nil
			}
			cur = l[idx]
		}
	}
	return nil
}

// removePath removes the value at path; a missing path is not an error.
func removePath(root map[string]any, path []segment) {
	var cur any = root
	for i, s := range path {
		last := i == len(path)-1
		switch {
		case s.key != "":
			m, ok := cur.(map[string]any)
			if !ok {
				return
			}
			if last {
				delete(m, s.key)
				return
			}
			cur = m[s.key]
		default:
			l, ok := cur.([]any)
			if !ok {
				return
			}
			idx := findIndex(l, s)
			if idx < 0 {
				return
			}
			if last {
				// Lists can't shrink in place; blank the item instead and let
				// the caller compact. Removing list items is rare enough that
				// a set of the parent is the documented way.
				l[idx] = nil
				return
			}
			cur = l[idx]
		}
	}
}

func findIndex(l []any, s segment) int {
	if s.hasIndex {
		if s.index >= 0 && s.index < len(l) {
			return s.index
		}
		return -1
	}
	for i, item := range l {
		m, ok := item.(map[string]any)
		if ok && fmt.Sprint(m[s.selKey]) == s.selValue {
			return i
		}
	}
	return -1
}

func describe(s segment) string {
	if s.hasIndex {
		return fmt.Sprintf("[%d]", s.index)
	}
	return fmt.Sprintf("[%s=%s]", s.selKey, s.selValue)
}

// compactNils removes nil items left in lists by removePath.
func compactNils(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			t[k] = compactNils(c)
		}
		return t
	case []any:
		out := t[:0]
		for _, c := range t {
			if c != nil {
				out = append(out, compactNils(c))
			}
		}
		return out
	}
	return v
}
