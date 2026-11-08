package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// KindExport is the kind of a provider export file.
const KindExport = "Export"

// Output formats of an export.
const (
	FormatTerraform  = "terraform"
	FormatCrossplane = "crossplane"
)

// What to do with secret values a provider can read.
const (
	SecretValuesSkip  = "skip"
	SecretValuesLocal = "local"
)

// Export describes how a provider's tool is turned into Terraform or
// Crossplane (gitopsify-<provider>.yaml). It is a resource in the
// Kubernetes style; see documents.go.
type Export struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata,omitzero"`
	Spec       ExportSpec `json:"spec"`
}

// ExportSpec is what an export reads and writes.
type ExportSpec struct {
	// Provider is the provider that reads the tool.
	Provider ProviderRef `json:"provider"`
	// Instances are the tool's instances (connections) to read. Their names
	// become directories when there is more than one.
	Instances []ExportInstance `json:"instances"`
	Select    ExportSelect     `json:"select,omitzero"`
	Output    ExportOutput     `json:"output,omitzero"`
}

// ProviderRef names a provider and where to install it from.
type ProviderRef struct {
	// Name is the provider's name, such as grafana.
	Name string `json:"name"`
	// Source is owner/name on GitHub; empty means the provider index.
	Source string `json:"source,omitempty"`
	// Version pins the provider version; gitopsify refuses another one.
	Version string `json:"version,omitempty"`
	// Checksums pins the SHA-256 of the provider's release archive per
	// platform (linux_amd64, darwin_arm64, ...), so CI installs exactly what
	// was reviewed.
	Checksums map[string]string `json:"checksums,omitempty"`
}

// ExportInstance is one instance of the tool.
type ExportInstance struct {
	// Name is a short name (lowercase letters, digits, dashes).
	Name string `json:"name"`
	// Settings are the provider's connection settings. Secret settings must
	// come from env or file, since this file is meant to be committed.
	Settings map[string]SettingValue `json:"settings,omitempty"`
}

// SettingValue is a setting's value, given directly (a plain string) or read
// from an environment variable or a file.
type SettingValue struct {
	Value string `json:"value,omitempty"`
	Env   string `json:"env,omitempty"`
	File  string `json:"file,omitempty"`
}

// UnmarshalJSON accepts a plain string or {value|env|file: ...}.
func (s *SettingValue) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &s.Value)
	}
	type plain SettingValue
	var p plain
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return fmt.Errorf("a setting is a string or one of {value: ...}, {env: VARIABLE}, {file: path}: %w", err)
	}
	*s = SettingValue(p)
	n := 0
	for _, v := range []string{s.Value, s.Env, s.File} {
		if v != "" {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("a setting needs exactly one of value, env and file")
	}
	return nil
}

// MarshalJSON writes plain values as strings.
func (s SettingValue) MarshalJSON() ([]byte, error) {
	if s.Env == "" && s.File == "" {
		return json.Marshal(s.Value)
	}
	type plain SettingValue
	return json.Marshal(plain(s))
}

// Literal reports whether the value is written in the file itself.
func (s SettingValue) Literal() bool { return s.Env == "" && s.File == "" }

// Resolve returns the value: from the environment or a file (both without
// surrounding whitespace, such as a trailing newline), or as written.
func (s SettingValue) Resolve() (string, error) {
	switch {
	case s.Env != "":
		v, ok := os.LookupEnv(s.Env)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", s.Env)
		}
		// Kubernetes Secrets made from files often end in a newline.
		return strings.TrimSpace(v), nil
	case s.File != "":
		b, err := os.ReadFile(ExpandHome(s.File))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return s.Value, nil
}

// ExportSelect narrows what is exported.
type ExportSelect struct {
	// Partitions are partition names or IDs (globs); empty means all.
	Partitions []string `json:"partitions,omitempty"`
	// Kinds are kind names; empty means the provider's default kinds.
	Kinds []string `json:"kinds,omitempty"`
	// Include keeps only resources matching these Kind/partition/name globs
	// (plus what they reference, unless noDeps).
	Include []string `json:"include,omitempty"`
	// Exclude drops resources matching these Kind/partition/name globs.
	Exclude []string `json:"exclude,omitempty"`
	NoDeps  bool     `json:"noDeps,omitempty"`
	// Prune stops managing objects exported before that the selection now
	// leaves out. Without it they stay in the configuration.
	Prune bool `json:"prune,omitempty"`
}

// ExportOutput says what is written.
type ExportOutput struct {
	// Dir is the output directory (default ./<provider>).
	Dir string `json:"dir,omitempty"`
	// Formats are terraform and/or crossplane (default both, as the provider
	// supports them).
	Formats []string `json:"formats,omitempty"`
	// Adopt manages the objects that were read (import blocks, external
	// names). false writes configuration that creates them elsewhere.
	Adopt *bool `json:"adopt,omitempty"`
	// SecretValues: skip (default) leaves secret values out; local writes
	// those the tool returns to git-ignored files under .secrets/.
	SecretValues string           `json:"secretValues,omitempty"`
	Terraform    TerraformOutput  `json:"terraform,omitzero"`
	Crossplane   CrossplaneOutput `json:"crossplane,omitzero"`
}

// TerraformOutput tunes the Terraform output.
type TerraformOutput struct {
	// Backend is written into every workspace; "{workspace}" in its settings
	// becomes the workspace's directory (such as default/main-org). Without
	// it, state is local.
	Backend *TerraformBackend `json:"backend,omitempty"`
}

// TerraformBackend is a Terraform backend block.
type TerraformBackend struct {
	// Type is the backend type: s3, gcs, azurerm, http, remote, ...
	Type string `json:"type"`
	// Settings are the backend's string arguments.
	Settings map[string]string `json:"settings,omitempty"`
}

// CrossplaneOutput tunes the Crossplane output.
type CrossplaneOutput struct {
	// NamespacePrefix starts each workspace's namespace (default the
	// provider's name).
	NamespacePrefix string `json:"namespacePrefix,omitempty"`
	// ManagementPolicies of every managed resource. Default with adopt:
	// Observe, Create, Update (never delete what was adopted); without:
	// Observe, Create, Update, Delete. LateInitialize is left out by default
	// because it rewrites spec and fights GitOps tools.
	ManagementPolicies []string `json:"managementPolicies,omitempty"`
}

// AdoptOrDefault returns Adopt, true when unset.
func (o ExportOutput) AdoptOrDefault() bool { return o.Adopt == nil || *o.Adopt }

var instanceName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidInstanceName reports whether s can name an instance.
func ValidInstanceName(s string) bool { return len(s) <= 40 && instanceName.MatchString(s) }

// DefaultExportFile is the export file of a provider.
func DefaultExportFile(provider string) string { return "gitopsify-" + provider + ".yaml" }

// Default fills unset fields.
func (x *Export) Default() {
	if x.APIVersion == "" {
		x.APIVersion = APIVersion
	}
	if x.Kind == "" {
		x.Kind = KindExport
	}
	e := &x.Spec
	if x.Metadata.Name == "" {
		x.Metadata.Name = e.Provider.Name
	}
	if len(e.Instances) == 0 {
		e.Instances = []ExportInstance{{Name: "default"}}
	}
	if e.Output.Dir == "" && e.Provider.Name != "" {
		e.Output.Dir = e.Provider.Name
	}
	if e.Output.SecretValues == "" {
		e.Output.SecretValues = SecretValuesSkip
	}
}

// Validate checks what can be checked without the provider.
func (x *Export) Validate() error {
	if x.Kind != KindExport {
		return fmt.Errorf("kind is %q, want %s", x.Kind, KindExport)
	}
	if x.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion is %q, want %s", x.APIVersion, APIVersion)
	}
	if err := x.Metadata.validate(); err != nil {
		return err
	}
	e := &x.Spec
	if e.Provider.Name == "" {
		return fmt.Errorf("spec.provider.name is required")
	}
	seen := map[string]bool{}
	for i, in := range e.Instances {
		if !ValidInstanceName(in.Name) {
			return fmt.Errorf("spec.instances[%d]: name %q must be lowercase letters, digits and dashes (at most 40)", i, in.Name)
		}
		if seen[in.Name] {
			return fmt.Errorf("spec.instances[%d]: %s is listed twice", i, in.Name)
		}
		seen[in.Name] = true
	}
	for _, f := range e.Output.Formats {
		if f != FormatTerraform && f != FormatCrossplane {
			return fmt.Errorf("spec.output.formats: %q isn't terraform or crossplane", f)
		}
	}
	switch e.Output.SecretValues {
	case "", SecretValuesSkip, SecretValuesLocal:
	default:
		return fmt.Errorf("spec.output.secretValues: %q isn't skip or local", e.Output.SecretValues)
	}
	if b := e.Output.Terraform.Backend; b != nil && b.Type == "" {
		return fmt.Errorf("spec.output.terraform.backend.type is required")
	}
	for _, p := range e.Output.Crossplane.ManagementPolicies {
		switch p {
		case "Observe", "Create", "Update", "Delete", "LateInitialize", "*":
		default:
			return fmt.Errorf("spec.output.crossplane.managementPolicies: %q isn't Observe, Create, Update, Delete, LateInitialize or *", p)
		}
	}
	return nil
}

// LoadExport reads the Export resource from a file (YAML, JSON or HCL).
// When the file holds several, name picks one (by metadata.name or provider
// name).
func LoadExport(path, name string) (*Export, error) {
	docs, err := ReadDocuments(path)
	if err != nil {
		return nil, err
	}
	return findExport(path, name, docs)
}

// ParseExport reads the export in data, as LoadExport does a file's; name
// appears in errors.
func ParseExport(name string, data []byte) (*Export, error) {
	docs, err := ParseDocuments(name, data)
	if err != nil {
		return nil, err
	}
	return findExport(name, "", docs)
}

func findExport(path, name string, docs []Document) (*Export, error) {
	exports := Find(docs, KindExport, "")
	if len(exports) == 0 {
		var kinds []string
		for _, d := range docs {
			kinds = append(kinds, d.Kind)
		}
		return nil, fmt.Errorf("%s holds no %s (it has: %s)", path, KindExport, strings.Join(kinds, ", "))
	}
	var picked []*Export
	var names []string
	for _, d := range exports {
		x := &Export{}
		if err := decodeStrict(d.Raw, x); err != nil {
			return nil, fmt.Errorf("%s: %s %s: %w", path, KindExport, d.Name, err)
		}
		id := firstNonEmptyString(x.Metadata.Name, x.Spec.Provider.Name)
		names = append(names, id)
		if name == "" || name == x.Metadata.Name || name == x.Spec.Provider.Name {
			picked = append(picked, x)
		}
	}
	switch len(picked) {
	case 1:
		return picked[0], nil
	case 0:
		return nil, fmt.Errorf("%s has no %s named %s (it has: %s)", path, KindExport, name, strings.Join(names, ", "))
	default:
		return nil, fmt.Errorf("%s has several exports (%s); name one", path, strings.Join(names, ", "))
	}
}

// Marshal renders the export as YAML, with the fields in their documented
// order.
func (x *Export) Marshal() ([]byte, error) { return marshalOrdered(x) }

func firstNonEmptyString(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
