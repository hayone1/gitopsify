package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrNotInPlace means a file holds other resources besides the export being
// saved, and the export's document can't be replaced in it safely.
var ErrNotInPlace = errors.New("the file holds other resources too")

// ExportID identifies an export in a file as it was read: its metadata.name
// and provider name before defaults.
type ExportID struct{ Name, Provider string }

// IDOf returns the identity of an export as read from its file.
func IDOf(x *Export) ExportID { return ExportID{x.Metadata.Name, x.Spec.Provider.Name} }

// SaveExport returns what path should hold after saving x, which was read
// from it as id (zero when it is new):
//
//   - x alone, when the file doesn't exist or holds nothing else;
//   - for YAML, the file with only that export's document replaced: the
//     other resources, separators and comments stay as they are;
//   - ErrNotInPlace otherwise (JSON and HCL files holding several resources,
//     or a YAML file whose documents can't be told apart by their
//     separators). Nothing is ever written that doesn't read back as before
//     with only the export changed.
func SaveExport(path string, x *Export, id ExportID, schemaHeader, comment string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return Encode(path, x, schemaHeader, comment)
	}
	if err != nil {
		return nil, err
	}
	docs, err := ParseDocuments(path, data)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 || (len(docs) == 1 && docs[0].Kind == KindExport) {
		return Encode(path, x, schemaHeader, comment)
	}
	if strings.EqualFold(filepath.Ext(path), ".hcl") || strings.EqualFold(filepath.Ext(path), ".json") {
		return nil, fmt.Errorf("%w (only YAML files are updated in place)", ErrNotInPlace)
	}
	out, err := replaceYAMLDocument(data, func(d Document) bool {
		if d.Kind != KindExport {
			return false
		}
		got := &Export{}
		return decodeStrict(d.Raw, got) == nil && IDOf(got) == id
	}, x)
	if err != nil {
		return nil, err
	}
	// Check: the same documents, in the same order, with only the export
	// changed, and the export as saved.
	after, err := ParseDocuments(path, out)
	if err != nil || len(after) != len(docs) {
		return nil, fmt.Errorf("%w (the documents couldn't be told apart)", ErrNotInPlace)
	}
	want, err := Encode(".json", x, "", "")
	if err != nil {
		return nil, err
	}
	replaced := 0
	for i := range docs {
		if bytes.Equal(after[i].JSON, docs[i].JSON) {
			continue
		}
		got := &Export{}
		if after[i].Kind != KindExport || decodeStrict(after[i].Raw, got) != nil {
			return nil, fmt.Errorf("%w (another document would change)", ErrNotInPlace)
		}
		if b, err := Encode(".json", got, "", ""); err != nil || !bytes.Equal(b, want) {
			return nil, fmt.Errorf("%w (the export wouldn't read back as saved)", ErrNotInPlace)
		}
		replaced++
	}
	if replaced > 1 {
		return nil, fmt.Errorf("%w (several documents would change)", ErrNotInPlace)
	}
	return out, nil
}

// docSeparator is a YAML document marker line. Such a line can't be part of
// a document's content (YAML forbids it even inside quoted strings), so the
// file splits on them.
var docSeparator = regexp.MustCompile(`^(---|\.\.\.)(\s.*)?$`)

// replaceYAMLDocument replaces the one document of data that match selects
// with x, keeping the comment lines around it (a note above the next
// document's separator often belongs to that document).
func replaceYAMLDocument(data []byte, match func(Document) bool, x *Export) ([]byte, error) {
	lines := strings.SplitAfter(string(data), "\n")
	type segment struct{ from, to int } // lines[from:to] between separators
	var segs []segment
	start := 0
	for i, l := range lines {
		if docSeparator.MatchString(strings.TrimRight(l, "\r\n")) {
			segs = append(segs, segment{start, i})
			start = i + 1
		}
	}
	segs = append(segs, segment{start, len(lines)})
	found := -1
	for i, s := range segs {
		docs, err := ParseDocuments(".yaml", []byte(strings.Join(lines[s.from:s.to], "")))
		if err != nil {
			return nil, fmt.Errorf("%w (the documents couldn't be told apart)", ErrNotInPlace)
		}
		if len(docs) != 1 || !match(docs[0]) {
			continue
		}
		if found >= 0 {
			return nil, fmt.Errorf("%w (the export appears twice)", ErrNotInPlace)
		}
		found = i
	}
	if found < 0 {
		return nil, fmt.Errorf("%w (the export to update isn't in it any more)", ErrNotInPlace)
	}
	body, err := marshalOrdered(x)
	if err != nil {
		return nil, err
	}
	s := segs[found]
	keep := s.from // comment and blank lines that head the document
	for keep < s.to {
		t := strings.TrimSpace(lines[keep])
		if t != "" && !strings.HasPrefix(t, "#") {
			break
		}
		keep++
	}
	tail := s.to // unindented comment and blank lines that end it
	for tail > keep {
		l := strings.TrimRight(lines[tail-1], "\r\n")
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "#") {
			break
		}
		tail--
	}
	var out strings.Builder
	out.WriteString(strings.Join(lines[:keep], ""))
	out.Write(body)
	out.WriteString(strings.Join(lines[tail:], ""))
	return []byte(out.String()), nil
}
