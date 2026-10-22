package skills

import (
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
)

var skillName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// TestSkillsFollowTheFormat checks the Agent Skills rules that agents enforce:
// a frontmatter name equal to the directory, a description of at most 1024
// characters, and links that resolve.
func TestSkillsFollowTheFormat(t *testing.T) {
	dirs, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	link := regexp.MustCompile(`\]\(([^)#]+\.md)\)`)
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		data, err := fs.ReadFile(FS, path.Join(d.Name(), "SKILL.md"))
		if err != nil {
			t.Errorf("%s: %v", d.Name(), err)
			continue
		}
		doc := string(data)
		if !strings.HasPrefix(doc, "---\n") {
			t.Errorf("%s: no frontmatter", d.Name())
			continue
		}
		head, body, _ := strings.Cut(doc[4:], "\n---\n")
		fields := map[string]string{}
		for _, l := range strings.Split(head, "\n") {
			k, v, _ := strings.Cut(l, ":")
			fields[k] = strings.TrimSpace(v)
		}
		if fields["name"] != d.Name() || len(d.Name()) > 64 || !skillName.MatchString(d.Name()) {
			t.Errorf("%s: name %q must equal the directory, lowercase with dashes, at most 64 characters", d.Name(), fields["name"])
		}
		if desc := fields["description"]; desc == "" || len(desc) > 1024 || strings.ContainsAny(desc, "<>") {
			t.Errorf("%s: description must be 1-1024 characters without angle brackets (%d)", d.Name(), len(desc))
		}
		for _, m := range link.FindAllStringSubmatch(body, -1) {
			if strings.HasPrefix(m[1], "http") {
				continue
			}
			if _, err := fs.Stat(FS, path.Join(d.Name(), m[1])); err != nil {
				t.Errorf("%s: broken link %s", d.Name(), m[1])
			}
		}
	}
}

// TestPluginVersionsMatch keeps the Claude Code plugin and the Gemini CLI
// extension at the same version.
func TestPluginVersionsMatch(t *testing.T) {
	version := func(p string) string {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		return m.Version
	}
	if a, b := version("../.claude-plugin/plugin.json"), version("../gemini-extension.json"); a == "" || a != b {
		t.Errorf("plugin.json has version %q, gemini-extension.json %q; run scripts/set-version.sh", a, b)
	}
}
