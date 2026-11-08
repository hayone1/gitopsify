// Package sdk is the protocol between gitopsify and its providers, and the
// helpers to write a provider in Go.
//
// A provider is an executable named gitopsify-provider-<name> that reads the
// live state of a tool (Grafana, Keycloak, ...) and describes it as Terraform
// resources, which gitopsify writes as Terraform or Crossplane configuration.
// gitopsify runs it once per call:
//
//	gitopsify-provider-<name> describe            # no input
//	gitopsify-provider-<name> check    < request  # connection, identity, partitions
//	gitopsify-provider-<name> discover < request  # partitions and resources
//
// The request is one JSON object on stdin. The provider answers on stdout with
// a stream of JSON messages (see [Message]) that ends with "done" or "error",
// and logs progress on stderr as JSON lines (log/slog). A Go provider
// implements [Provider] and calls [Serve] from main.
//
// This module only depends on the standard library, so providers don't inherit
// gitopsify's dependencies.
package sdk

import "encoding/json"

// APIVersion is the version of the protocol described here.
const APIVersion = "provider.gitopsify.io/v1alpha1"

// Protocol is the short name of APIVersion, listed in Description.Protocols.
const Protocol = "v1alpha1"

// Methods a provider answers.
const (
	MethodDescribe = "describe"
	MethodCheck    = "check"
	MethodDiscover = "discover"
)

// Description says what a provider reads and what it can produce. gitopsify
// builds its prompts, flags and output from it, without provider-specific
// code.
type Description struct {
	APIVersion string `json:"apiVersion"`
	// Name is the provider's short name: the part after gitopsify-provider-
	// in the executable's name. Lowercase letters, digits and dashes.
	Name string `json:"name"`
	// Title is the tool's display name, such as "Grafana".
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Homepage    string `json:"homepage,omitempty"`
	// Protocols lists the protocol versions the provider speaks.
	Protocols []string `json:"protocols"`
	// Settings are what gitopsify needs to connect to one instance of the tool.
	Settings []Setting `json:"settings,omitempty"`
	// Partition names the unit the tool divides its objects into (Grafana
	// organizations, Keycloak realms). Each partition gets its own Terraform
	// workspace and Kustomization.
	Partition Noun `json:"partition"`
	// Kinds are the types of objects the provider reads.
	Kinds []Kind `json:"kinds"`
	// Terraform is the upstream Terraform provider the output uses; nil when
	// the provider can't produce Terraform.
	Terraform *TerraformProvider `json:"terraform,omitempty"`
	// Crossplane is the upstream Crossplane provider the output uses; nil when
	// the provider can't produce Crossplane.
	Crossplane *CrossplaneProvider `json:"crossplane,omitempty"`
}

// Kind returns the kind with the given name.
func (d *Description) Kind(name string) (*Kind, bool) {
	for i := range d.Kinds {
		if d.Kinds[i].Name == name {
			return &d.Kinds[i], true
		}
	}
	return nil, false
}

// Setting is one connection setting, such as a URL or a token.
type Setting struct {
	// Name is the key in Request.Settings and in gitopsify's configuration.
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// Env is the environment variable gitopsify reads the value from when it
	// isn't configured. Use the upstream Terraform provider's variable
	// (GRAFANA_URL) so one environment serves both gitopsify and terraform.
	Env string `json:"env,omitempty"`
	// Secret values are masked in prompts, never written to tracked files and
	// never shown.
	Secret   bool   `json:"secret,omitempty"`
	Required bool   `json:"required,omitempty"`
	Default  string `json:"default,omitempty"`
	// Example is shown as a placeholder in prompts.
	Example string `json:"example,omitempty"`
	// Terraform is the attribute of the upstream provider block this setting
	// sets. Non-secret settings are written into the provider block; secret
	// ones are left to Env.
	Terraform string `json:"terraform,omitempty"`
	// Crossplane is the key of this setting in the JSON credentials of the
	// Crossplane ProviderConfig's Secret (Upjet providers use the Terraform
	// attribute names).
	Crossplane string `json:"crossplane,omitempty"`
}

// Noun is a word in singular and plural, for prompts and reports.
type Noun struct {
	Singular    string `json:"singular"`
	Plural      string `json:"plural"`
	Description string `json:"description,omitempty"`
}

// Kind is a type of object a provider reads.
type Kind struct {
	// Name is the kind as people select it: Dashboard, Folder, DataSource.
	Name        string `json:"name"`
	Plural      string `json:"plural,omitempty"`
	Description string `json:"description,omitempty"`
	// Optional kinds are left out unless they are asked for.
	Optional   bool            `json:"optional,omitempty"`
	Terraform  *TerraformType  `json:"terraform,omitempty"`
	Crossplane *CrossplaneType `json:"crossplane,omitempty"`
}

// TerraformType is the Terraform resource a kind becomes.
type TerraformType struct {
	// Type is the resource type, such as grafana_dashboard.
	Type string `json:"type"`
	// Blocks lists the attribute paths that are nested blocks in the
	// resource's schema ("rule", "rule.notification_settings"), so they are
	// written as blocks rather than as arguments. Paths ignore list indexes.
	Blocks []string `json:"blocks,omitempty"`
	// IgnoreChanges lists top-level attributes Terraform must not plan
	// changes for (lifecycle ignore_changes): attributes the upstream
	// provider can't read back after an import and would otherwise replace
	// the object over.
	IgnoreChanges []string `json:"ignoreChanges,omitempty"`
}

// CrossplaneType is the Crossplane managed resource a kind becomes.
type CrossplaneType struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	// Rename maps attribute paths ("secure_json_data_encoded") to the field
	// name Crossplane uses where it isn't the lowerCamelCase of the Terraform
	// name.
	Rename map[string]string `json:"rename,omitempty"`
	// Explicit means the provider gives spec.forProvider itself in
	// Resource.Crossplane instead of having it derived from the attributes
	// with Upjet's conventions.
	Explicit bool `json:"explicit,omitempty"`
}

// TerraformProvider is the upstream Terraform provider.
type TerraformProvider struct {
	// Name is the local name in required_providers, such as grafana.
	Name string `json:"name"`
	// Source is the registry address, such as grafana/grafana.
	Source string `json:"source"`
	// Version is a version constraint, such as "~> 4.53".
	Version string `json:"version"`
}

// CrossplaneProvider is the upstream Crossplane provider.
type CrossplaneProvider struct {
	// Name is the name of the pkg.crossplane.io Provider object.
	Name string `json:"name"`
	// Package is the provider's package, with its version.
	Package string `json:"package"`
	// ProviderConfig is the API version and kind of the cluster-wide provider
	// configuration that managed resources reference.
	ProviderConfig GVK `json:"providerConfig"`
}

// GVK is an API version and kind.
type GVK struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

// Request is what gitopsify sends to check and discover.
type Request struct {
	APIVersion string `json:"apiVersion"`
	// Instance is the name the user gave this connection, for messages.
	Instance string `json:"instance,omitempty"`
	// Settings holds the connection settings by Setting.Name. gitopsify has
	// already read them from flags, configuration, the environment or prompts;
	// providers use only these values.
	Settings map[string]string `json:"settings,omitempty"`
	// Scope narrows discover. Empty lists mean everything.
	Scope Scope `json:"scope,omitempty"`
	// Detail is DetailFull (the default) or DetailSummary.
	Detail string `json:"detail,omitempty"`
}

// Detail levels of discover.
const (
	// DetailFull asks for resources with their attributes.
	DetailFull = "full"
	// DetailSummary asks for keys, kinds, partitions, names and DependsOn
	// (including the keys of referenced resources), without attributes, so
	// people can pick objects quickly. Providers may answer with full
	// resources instead.
	DetailSummary = "summary"
)

// Scope narrows what discover reads. Providers should skip what is out of
// scope (gitopsify filters again either way).
type Scope struct {
	// Partitions holds partition IDs.
	Partitions []string `json:"partitions,omitempty"`
	// Kinds holds kind names.
	Kinds []string `json:"kinds,omitempty"`
	// Keys holds resource keys (from a summary). When set, only these
	// resources are needed.
	Keys []string `json:"keys,omitempty"`
}

// HasPartition reports whether the scope includes a partition.
func (s Scope) HasPartition(id string) bool {
	return len(s.Partitions) == 0 || contains(s.Partitions, id)
}

// HasKind reports whether the scope includes a kind.
func (s Scope) HasKind(name string) bool { return len(s.Kinds) == 0 || contains(s.Kinds, name) }

// HasKey reports whether the scope includes a resource key.
func (s Scope) HasKey(key string) bool { return len(s.Keys) == 0 || contains(s.Keys, key) }

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// CheckResult says what a provider is connected to and what it can read.
type CheckResult struct {
	// Target describes the instance: product, version and address.
	Target string `json:"target"`
	// Identity is who the provider is connected as, and with what rights.
	Identity string `json:"identity,omitempty"`
	// Partitions lists every partition the identity can see.
	Partitions []Partition `json:"partitions"`
	// Kinds says, per kind, whether it can be read.
	Kinds    []KindCheck `json:"kinds,omitempty"`
	Warnings []string    `json:"warnings,omitempty"`
}

// KindCheck says whether a kind can be read.
type KindCheck struct {
	Kind string `json:"kind"`
	OK   bool   `json:"ok"`
	// Message explains a failure and how to fix it.
	Message string `json:"message,omitempty"`
}

// Partition is one organization, realm, ... of an instance. Resources that
// belong to no partition (instance-wide objects) have an empty Partition and
// go to their own workspace.
type Partition struct {
	// ID is stable and unique within the instance.
	ID string `json:"id"`
	// Name is the display name.
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Terraform holds attributes added to the provider block of this
	// partition's workspace, such as Grafana's {"org_id": 2}.
	Terraform map[string]any `json:"terraform,omitempty"`
	// Crossplane holds fields added to the spec of this partition's
	// ProviderConfig, such as Grafana's {"orgId": "2"}.
	Crossplane map[string]any `json:"crossplane,omitempty"`
}

// Resource is one object of the tool, as a Terraform resource.
type Resource struct {
	// Key identifies the resource within the instance, stably across runs;
	// references use it. For example "dashboard/2/k8s-overview".
	Key string `json:"key"`
	// Kind is one of Description.Kinds.
	Kind string `json:"kind"`
	// Partition is the ID of the partition the resource belongs to; empty
	// for instance-wide objects.
	Partition string `json:"partition,omitempty"`
	// Name is the display name, used for file and resource names.
	Name string `json:"name"`
	// ImportID is the ID `terraform import` takes. Required to adopt the
	// resource.
	ImportID string `json:"importId,omitempty"`
	// ExternalName is the crossplane.io/external-name annotation; empty means
	// ImportID (true for Upjet providers that use the Terraform ID).
	ExternalName string `json:"externalName,omitempty"`
	// Attributes are the Terraform resource's arguments, by their schema
	// names. Values are JSON values or markers made with Ref, Secret, JSON
	// and File.
	Attributes map[string]any `json:"attributes"`
	// Crossplane is spec.forProvider, for kinds whose CrossplaneType is
	// Explicit. It may contain markers too.
	Crossplane map[string]any `json:"crossplane,omitempty"`
	// DependsOn lists keys of resources this one needs beyond its references.
	DependsOn []string `json:"dependsOn,omitempty"`
	// Notes are shown in the export report, such as "the password can't be
	// read back".
	Notes []string `json:"notes,omitempty"`
}

// Skipped is an object the provider leaves out on purpose, and why: it is
// managed elsewhere (provisioned from files, read-only) or can't be expressed.
// gitopsify lists it in the report.
type Skipped struct {
	Kind      string `json:"kind"`
	Partition string `json:"partition,omitempty"`
	Name      string `json:"name"`
	Reason    string `json:"reason"`
}

// Warning reports something the provider couldn't read; discover goes on.
// With a Partition and a Kind (or only a Partition) it marks that scope as
// incomplete: gitopsify keeps the previous output of incomplete scopes
// instead of treating their objects as gone. Set them whenever a read fails.
type Warning struct {
	Partition string `json:"partition,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Message   string `json:"message"`
}

// Message is one JSON object a provider writes to stdout. Exactly one field
// is set. describe answers with Description, check with Check; discover
// streams Partition, Resource, Skipped and Warning messages. Every answer ends with
// Done or Error, so gitopsify can tell a finished answer from a provider that
// died half way.
type Message struct {
	Description *Description `json:"description,omitempty"`
	Check       *CheckResult `json:"check,omitempty"`
	Partition   *Partition   `json:"partition,omitempty"`
	Resource    *Resource    `json:"resource,omitempty"`
	Skipped     *Skipped     `json:"skipped,omitempty"`
	Warning     *Warning     `json:"warning,omitempty"`
	Error       *Error       `json:"error,omitempty"`
	Done        *struct{}    `json:"done,omitempty"`
}

// Inventory is the whole answer of discover, as gitopsify collects it.
type Inventory struct {
	Partitions []Partition `json:"partitions"`
	Resources  []Resource  `json:"resources"`
	Skipped    []Skipped   `json:"skipped,omitempty"`
	Warnings   []Warning   `json:"warnings,omitempty"`
}

// RawJSON is JSON text kept as is, so the order of keys (a dashboard's
// layout, for example) survives.
type RawJSON = json.RawMessage
