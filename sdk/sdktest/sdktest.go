// Package sdktest tests gitopsify providers: it runs a provider in-process the
// way gitopsify runs it (through JSON), and checks its description and
// inventory against the rules gitopsify relies on.
//
//	func TestConformance(t *testing.T) {
//		srv := fakeAPI(t) // an httptest.Server that answers like the tool
//		sdktest.Conformance(t, provider.New(), sdk.Request{Settings: map[string]string{"url": srv.URL}})
//	}
package sdktest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/hayone1/gitopsify/sdk"
)

// Describe runs describe and returns the description gitopsify would see.
func Describe(t testing.TB, p sdk.Provider) sdk.Description {
	t.Helper()
	out := run(t, p, sdk.MethodDescribe, nil)
	d, err := sdk.ReadDescription(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	return *d
}

// Check runs check and returns its result. It fails t when check fails.
func Check(t testing.TB, p sdk.Provider, req sdk.Request) sdk.CheckResult {
	t.Helper()
	c, err := CheckErr(t, p, req)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	return c
}

// CheckErr runs check and returns its result or the provider's error.
func CheckErr(t testing.TB, p sdk.Provider, req sdk.Request) (sdk.CheckResult, error) {
	t.Helper()
	out := run(t, p, sdk.MethodCheck, &req)
	c, err := sdk.ReadCheck(bytes.NewReader(out))
	if err != nil {
		return sdk.CheckResult{}, err
	}
	return *c, nil
}

// Discover runs discover and returns the inventory gitopsify would see. It
// fails t when discover fails.
func Discover(t testing.TB, p sdk.Provider, req sdk.Request) sdk.Inventory {
	t.Helper()
	inv, err := DiscoverErr(t, p, req)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	return inv
}

// DiscoverErr runs discover and returns the inventory or the provider's error.
func DiscoverErr(t testing.TB, p sdk.Provider, req sdk.Request) (sdk.Inventory, error) {
	t.Helper()
	out := run(t, p, sdk.MethodDiscover, &req)
	inv, err := sdk.ReadInventory(bytes.NewReader(out), nil)
	if err != nil {
		return sdk.Inventory{}, err
	}
	return *inv, nil
}

func run(t testing.TB, p sdk.Provider, method string, req *sdk.Request) []byte {
	t.Helper()
	var in, out, errOut bytes.Buffer
	if req != nil {
		if req.APIVersion == "" {
			req.APIVersion = sdk.APIVersion
		}
		if err := json.NewEncoder(&in).Encode(req); err != nil {
			t.Fatalf("encoding the request: %v", err)
		}
	}
	sdk.Run(context.Background(), p, []string{method}, &in, &out, &errOut)
	if testing.Verbose() && errOut.Len() > 0 {
		t.Logf("%s stderr:\n%s", method, errOut.String())
	}
	return out.Bytes()
}

// Conformance runs describe and discover and fails t for every rule the
// description or the inventory breaks.
func Conformance(t *testing.T, p sdk.Provider, req sdk.Request) {
	t.Helper()
	d := Describe(t, p)
	for _, problem := range CheckDescription(d) {
		t.Errorf("description: %s", problem)
	}
	inv := Discover(t, p, req)
	if len(inv.Resources) == 0 {
		t.Errorf("discover found no resources; give the conformance test a fixture with some")
	}
	for _, problem := range CheckInventory(d, inv) {
		t.Errorf("inventory: %s", problem)
	}
}

var (
	providerName  = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$`)
	settingName   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	envName       = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	kindName      = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	tfName        = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	tfSource      = regexp.MustCompile(`^([a-z0-9.-]+/)?[a-z0-9-]+/[a-z0-9-]+$`)
	secretName    = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	crossplaneRef = regexp.MustCompile(`^[a-z][A-Za-z0-9]*Refs?$`)
	secretRef     = regexp.MustCompile(`^[a-z][A-Za-z0-9]*SecretRef$`)
)

// CheckDescription returns what is wrong with a description.
func CheckDescription(d sdk.Description) []string {
	var p problems
	if d.APIVersion != sdk.APIVersion {
		p.add("apiVersion is %q, want %q", d.APIVersion, sdk.APIVersion)
	}
	if !providerName.MatchString(d.Name) {
		p.add("name %q must be lowercase letters, digits and dashes", d.Name)
	}
	if d.Title == "" {
		p.add("title is empty")
	}
	if d.Version == "" {
		p.add("version is empty")
	}
	if !contains(d.Protocols, sdk.Protocol) {
		p.add("protocols %v don't include %s", d.Protocols, sdk.Protocol)
	}
	seen := map[string]bool{}
	for _, s := range d.Settings {
		if !settingName.MatchString(s.Name) {
			p.add("setting %q: name must be lowercase letters, digits and underscores", s.Name)
		}
		if seen[s.Name] {
			p.add("setting %q is listed twice", s.Name)
		}
		seen[s.Name] = true
		if s.Env != "" && !envName.MatchString(s.Env) {
			p.add("setting %q: env %q isn't an environment variable name", s.Name, s.Env)
		}
		if s.Description == "" {
			p.add("setting %q has no description", s.Name)
		}
	}
	if d.Partition.Singular == "" || d.Partition.Plural == "" {
		p.add("partition needs a singular and a plural")
	}
	if len(d.Kinds) == 0 {
		p.add("no kinds")
	}
	if d.Terraform == nil && d.Crossplane == nil {
		p.add("neither terraform nor crossplane is set: the provider can't produce anything")
	}
	if tf := d.Terraform; tf != nil {
		if !tfName.MatchString(tf.Name) {
			p.add("terraform.name %q isn't a provider local name", tf.Name)
		}
		if !tfSource.MatchString(tf.Source) {
			p.add("terraform.source %q isn't a registry address such as grafana/grafana", tf.Source)
		}
		if tf.Version == "" {
			p.add("terraform.version is empty; pin the provider version the output was tested with")
		}
	}
	if xp := d.Crossplane; xp != nil {
		if xp.Name == "" || xp.Package == "" {
			p.add("crossplane needs a name and a package")
		}
		if !strings.Contains(xp.Package, ":") && !strings.Contains(xp.Package, "@") {
			p.add("crossplane.package %q has no version", xp.Package)
		}
		if !strings.Contains(xp.ProviderConfig.APIVersion, "/") || xp.ProviderConfig.Kind == "" {
			p.add("crossplane.providerConfig needs an apiVersion (group/version) and a kind")
		}
	}
	kinds := map[string]bool{}
	for _, k := range d.Kinds {
		if !kindName.MatchString(k.Name) {
			p.add("kind %q: name must be UpperCamelCase", k.Name)
		}
		if kinds[k.Name] {
			p.add("kind %q is listed twice", k.Name)
		}
		kinds[k.Name] = true
		if k.Description == "" {
			p.add("kind %q has no description", k.Name)
		}
		if k.Terraform != nil {
			if d.Terraform == nil {
				p.add("kind %q has a terraform type but the provider has no terraform provider", k.Name)
			} else if !strings.HasPrefix(k.Terraform.Type, d.Terraform.Name+"_") || !tfName.MatchString(k.Terraform.Type) {
				p.add("kind %q: terraform type %q must start with %q", k.Name, k.Terraform.Type, d.Terraform.Name+"_")
			}
			for _, a := range k.Terraform.IgnoreChanges {
				if !tfName.MatchString(a) {
					p.add("kind %q: ignoreChanges %q isn't an attribute name", k.Name, a)
				}
			}
		}
		if k.Crossplane != nil {
			if d.Crossplane == nil {
				p.add("kind %q has a crossplane type but the provider has no crossplane provider", k.Name)
			}
			if !strings.Contains(k.Crossplane.APIVersion, "/") || !kindName.MatchString(k.Crossplane.Kind) {
				p.add("kind %q: crossplane needs an apiVersion (group/version) and a kind", k.Name)
			}
		}
		if k.Terraform == nil && k.Crossplane == nil {
			p.add("kind %q has neither a terraform nor a crossplane type", k.Name)
		}
	}
	return p
}

// CheckInventory returns what is wrong with an inventory.
func CheckInventory(d sdk.Description, inv sdk.Inventory) []string {
	var p problems
	partitions := map[string]bool{}
	for _, part := range inv.Partitions {
		if part.ID == "" || part.Name == "" {
			p.add("partition %+v needs an ID and a name", part)
		}
		if partitions[part.ID] {
			p.add("partition %q is listed twice", part.ID)
		}
		partitions[part.ID] = true
	}
	keys := map[string]bool{}
	for _, r := range inv.Resources {
		if keys[r.Key] {
			p.add("resource key %q is used twice", r.Key)
		}
		keys[r.Key] = true
	}
	files := map[string]string{}
	for _, r := range inv.Resources {
		at := "resource " + r.Key
		k, ok := d.Kind(r.Kind)
		if !ok {
			p.add("%s: kind %q isn't in the description", at, r.Kind)
			continue
		}
		if r.Partition != "" && !partitions[r.Partition] {
			p.add("%s: partition %q wasn't sent", at, r.Partition)
		}
		if r.Name == "" {
			p.add("%s has no name", at)
		}
		if r.ImportID == "" {
			p.add("%s has no importId, so it can't be adopted", at)
		}
		explicit := k.Crossplane != nil && k.Crossplane.Explicit
		if explicit && r.Crossplane == nil {
			p.add("%s: kind %s is explicit for crossplane but the resource has no crossplane body", at, r.Kind)
		}
		if k.Crossplane != nil {
			if _, _, err := sdk.ForProvider(k, &r, everything{}); err != nil {
				p.add("%s: deriving the Crossplane resource: %v", at, err)
			}
		}
		for _, dep := range r.DependsOn {
			if !keys[dep] {
				p.add("%s depends on %q, which isn't in the inventory", at, dep)
			}
		}
		check := func(where string, v any) {
			walk(where, v, func(where string, v any) {
				switch m := v.(type) {
				case sdk.RefValue:
					if m.Ref == "" || m.Attr == "" {
						p.add("%s: $ref needs a key and an attr", where)
					}
					if m.Value == nil {
						p.add("%s: $ref to %q has no value to fall back on", where, m.Ref)
					}
					if m.Crossplane != "" && !crossplaneRef.MatchString(m.Crossplane) {
						p.add("%s: crossplane reference field %q should end in Ref or Refs", where, m.Crossplane)
					}
				case sdk.SecretValue:
					if !secretName.MatchString(m.Secret) {
						p.add("%s: secret name %q must be lowercase letters, digits and underscores", where, m.Secret)
					}
					if m.Crossplane != "" && !secretRef.MatchString(m.Crossplane) {
						p.add("%s: crossplane secret field %q should end in SecretRef", where, m.Crossplane)
					}
				case sdk.FileValue:
					clean := path.Clean(m.File)
					if m.File == "" || clean != m.File || path.IsAbs(m.File) || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." || strings.Contains(m.File, `\`) {
						p.add("%s: file path %q must be relative, clean and use forward slashes", where, m.File)
					}
					if m.Content == nil {
						p.add("%s: file %q has no content", where, m.File)
					}
					fk := r.Partition + "\x00" + m.File
					if other, dup := files[fk]; dup {
						p.add("%s: file %q is also used by %s", where, m.File, other)
					}
					files[fk] = r.Key
				case sdk.JSONValue:
					if raw, ok := m.JSON.(sdk.RawJSON); ok && !json.Valid(raw) {
						p.add("%s: $json content isn't valid JSON", where)
					}
				}
			})
		}
		check(at+": attributes", r.Attributes)
		check(at+": crossplane", r.Crossplane)
		if len(r.Attributes) == 0 && k.Terraform != nil {
			p.add("%s has no attributes", at)
		}
	}
	for _, sk := range inv.Skipped {
		if sk.Kind == "" || sk.Name == "" || sk.Reason == "" {
			p.add("skipped %+v needs a kind, a name and a reason", sk)
		}
	}
	return p
}

// everything resolves every reference and secret, to exercise ForProvider.
type everything struct{}

func (everything) Ref(key string) (string, string, bool) { return "ref", "", true }
func (everything) Secret(path string, s sdk.SecretValue) (string, string, bool) {
	return "secret", s.Secret, true
}

// walk calls fn for every value under v, markers included (but not their
// content).
func walk(where string, v any, fn func(string, any)) {
	fn(where, v)
	switch t := v.(type) {
	case map[string]any:
		names := make([]string, 0, len(t))
		for k := range t {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			walk(where+"."+k, t[k], fn)
		}
	case []any:
		for i, e := range t {
			walk(fmt.Sprintf("%s[%d]", where, i), e, fn)
		}
	}
}

type problems []string

func (p *problems) add(format string, args ...any) { *p = append(*p, fmt.Sprintf(format, args...)) }

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
