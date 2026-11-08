package config

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// SchemaURL is where the JSON Schema of gitopsify.yaml is published.
const SchemaURL = "https://raw.githubusercontent.com/hayone1/gitopsify/main/docs/gitopsify.schema.json"

// SchemaHeader makes YAML editors validate and complete the file.
const SchemaHeader = "# yaml-language-server: $schema=" + SchemaURL + "\n"

// enums lists the allowed values of a migration's fields, by JSON path in
// the spec ("[]" for list items, "{}" for map values).
var enums = map[string][]string{
	"scope.clusterScoped":               {"auto", "all", "none"},
	"scope.rules[].action":              {"include", "exclude"},
	"helm.mode":                         {HelmAuto, HelmChart, HelmVendor, HelmManifests},
	"helm.resolvers[]":                  {"local", "artifacthub"},
	"helm.releases{}.mode":              {HelmAuto, HelmChart, HelmVendor, HelmManifests},
	"clean.strategy":                    {CleanAuto, CleanManagedFields, CleanHeuristic, CleanMinimal},
	"grouping.by":                       {GroupByNamespace, GroupByLabel, GroupSingle},
	"secrets.strategy":                  {SecretsSkip, SecretsLocal, SecretsPlain, SecretsSOPS, SecretsSealedSecrets, SecretsExternalSecrets},
	"secrets.overrides[].strategy":      {SecretsSkip, SecretsLocal, SecretsPlain, SecretsSOPS, SecretsSealedSecrets, SecretsExternalSecrets},
	"secrets.sealedSecrets.scope":       {"strict", "namespace-wide", "cluster-wide"},
	"secrets.externalSecrets.storeKind": {"SecretStore", "ClusterSecretStore"},
	"transforms[].type":                 {"replace", "image", "storageClass", "ingressClass", "namespace", "set", "remove", "jsonPatch", "labels", "annotations"},
	"environments[].transforms[].type":  {"replace", "image", "storageClass", "ingressClass", "namespace", "set", "remove", "jsonPatch", "labels", "annotations"},
	"output.tool":                       {ToolNone, ToolArgoCD, ToolFlux},
	"output.layout":                     {LayoutFolderPerEnv, LayoutRepoPerEnv},
	"output.argocd.pattern":             {PatternAppOfApps, PatternApplicationSet, PatternSingle},
}

// ExportSchemaURL is where the JSON Schema of export files is published.
const ExportSchemaURL = "https://raw.githubusercontent.com/hayone1/gitopsify/main/docs/gitopsify-export.schema.json"

// ExportSchemaHeader makes YAML editors validate and complete export files.
const ExportSchemaHeader = "# yaml-language-server: $schema=" + ExportSchemaURL + "\n"

var exportEnums = map[string][]string{
	"apiVersion":               {APIVersion},
	"kind":                     {KindExport},
	"spec.output.formats[]":    {FormatTerraform, FormatCrossplane},
	"spec.output.secretValues": {SecretValuesSkip, SecretValuesLocal},
	"spec.output.crossplane.managementPolicies[]": {"Observe", "Create", "Update", "Delete", "LateInitialize", "*"},
}

// ExportSchema returns the JSON Schema of an export file.
func ExportSchema() ([]byte, error) {
	s := exportSchema()
	s["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	s["$id"] = ExportSchemaURL
	s["title"] = "gitopsify export"
	s["description"] = "gitopsify-<provider>.yaml: how gitopsify export turns a tool's state into Terraform or Crossplane. Reference: https://github.com/hayone1/gitopsify/blob/main/docs/providers.md"
	b, err := json.MarshalIndent(s, "", "  ")
	return append(b, '\n'), err
}

func exportSchema() map[string]any {
	s := schemaOf(reflect.TypeOf(Export{}), "", exportEnums)
	nameRule(s)
	return s
}

// ServerSchemaURL is where the JSON Schema of the server's configuration is
// published.
const ServerSchemaURL = "https://raw.githubusercontent.com/hayone1/gitopsify/main/docs/gitopsify-server.schema.json"

var serverEnums = map[string][]string{
	"apiVersion":                    {APIVersion},
	"kind":                          {KindServer},
	"spec.auth.mode":                {AuthOIDC, AuthLocal},
	"spec.rbac.defaultRole":         {RoleViewer, RoleOperator, RoleAdmin},
	"spec.rbac.bindings[].role":     {RoleViewer, RoleOperator, RoleAdmin},
	"spec.auth.tokens[].role":       {RoleViewer, RoleOperator, RoleAdmin},
	"spec.storage.type":             {"file", "postgres"},
	"spec.notifications[].format":   {"json", "slack", "teams"},
	"spec.notifications[].events[]": {"run.succeeded", "run.failed", "run.canceled"},
	"spec.notifications[].types[]":  {"scan", "migrate", "export"},
}

// ServerSchema returns the JSON Schema of the server's configuration (the
// Server resource of gitopsify server).
func ServerSchema() ([]byte, error) {
	s := schemaOf(reflect.TypeOf(Server{}), "", serverEnums)
	nameRule(s)
	s["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	s["$id"] = ServerSchemaURL
	s["title"] = "gitopsify server"
	s["description"] = "The configuration of gitopsify server: sign-in, roles, clusters, tools, Git, storage. Reference: https://github.com/hayone1/gitopsify/blob/main/docs/web-ui.md"
	b, err := json.MarshalIndent(s, "", "  ")
	return append(b, '\n'), err
}

// Schema returns the JSON Schema of gitopsify.yaml. It checks the Migration
// resource, and the layout gitopsify v0.1 wrote (the spec's fields at the
// top), which files written then still use and point to the same schema.
func Schema() ([]byte, error) {
	resourceEnums := map[string][]string{"apiVersion": {APIVersion}, "kind": {Kind}}
	for p, e := range enums {
		resourceEnums["spec."+p] = e
	}
	resource := schemaOf(reflect.TypeOf(migrationDocument{}), "", resourceEnums)
	nameRule(resource)
	spec := resource["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)
	delete(spec, "apiVersion")
	delete(spec, "kind")
	flat := schemaOf(reflect.TypeOf(Config{}), "", enums)
	props := flat["properties"].(map[string]any)
	props["apiVersion"] = map[string]any{"type": "string", "enum": []string{APIVersion}}
	props["kind"] = map[string]any{"type": "string", "enum": []string{Kind}}
	flat["description"] = "The layout gitopsify v0.1 wrote, with the fields at the top. Still read; gitopsify init now writes the resource form (apiVersion, kind, metadata, spec)."
	flat["deprecated"] = true
	// v0.1's layout is recognised by a spec field at the top, so a file being
	// typed, or with a misspelled key, is still checked as a resource.
	var v01 []any
	for name := range spec {
		v01 = append(v01, map[string]any{"required": []string{name}})
	}
	sort.Slice(v01, func(i, j int) bool {
		return v01[i].(map[string]any)["required"].([]string)[0] < v01[j].(map[string]any)["required"].([]string)[0]
	})
	s := map[string]any{
		"$schema":     "https://json-schema.org/draft/2020-12/schema",
		"$id":         SchemaURL,
		"title":       "gitopsify migration",
		"description": "gitopsify.yaml: describes a migration of a Kubernetes cluster to GitOps, and can hold provider exports too. Reference: https://github.com/hayone1/gitopsify/blob/main/docs/configuration.md",
		// The schema line covers every document of the file: exports in it
		// are checked against the export schema, embedded so that the
		// schema works offline and in validators that don't fetch.
		"$defs": map[string]any{"export": exportSchema()},
		"if":    map[string]any{"required": []string{"kind"}, "properties": map[string]any{"kind": map[string]any{"const": KindExport}}},
		"then":  map[string]any{"$ref": "#/$defs/export"},
		"else": map[string]any{
			"if":   map[string]any{"not": map[string]any{"anyOf": v01}},
			"then": resource,
			"else": flat,
		},
	}
	b, err := json.MarshalIndent(s, "", "  ")
	return append(b, '\n'), err
}

// nameRule adds the rule ObjectMeta.validate applies to metadata.name.
func nameRule(resource map[string]any) {
	meta := resource["properties"].(map[string]any)["metadata"].(map[string]any)["properties"].(map[string]any)
	name := meta["name"].(map[string]any)
	name["pattern"] = resourceName.String()
	name["maxLength"] = 253
}

func schemaOf(t reflect.Type, path string, enums map[string][]string) map[string]any {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeOf(SettingValue{}) {
		return map[string]any{"anyOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "object", "additionalProperties": false, "minProperties": 1, "maxProperties": 1, "properties": map[string]any{
				"value": map[string]any{"type": "string"}, "env": map[string]any{"type": "string"}, "file": map[string]any{"type": "string"}}},
		}}
	}
	var s map[string]any
	switch t.Kind() {
	case reflect.Struct:
		props := map[string]any{}
		addFields(t, path, props, enums)
		s = map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	case reflect.Slice:
		s = map[string]any{"type": "array", "items": schemaOf(t.Elem(), path+"[]", enums)}
	case reflect.Map:
		s = map[string]any{"type": "object", "additionalProperties": schemaOf(t.Elem(), path+"{}", enums)}
	case reflect.String:
		s = map[string]any{"type": "string"}
	case reflect.Bool:
		s = map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int64, reflect.Int32:
		s = map[string]any{"type": "integer"}
	default:
		s = map[string]any{} // any value
	}
	if e, ok := enums[path]; ok {
		s["enum"] = e
	}
	return s
}

// addFields adds the JSON properties of a struct, flattening embedded and
// inline structs the way encoding/json does.
func addFields(t reflect.Type, path string, props map[string]any, enums map[string][]string) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" || !f.IsExported() {
			continue
		}
		if name == "" && (f.Anonymous || strings.Contains(tag, "inline")) {
			addFields(f.Type, path, props, enums)
			continue
		}
		if name == "" {
			name = f.Name
		}
		p := name
		if path != "" {
			p = path + "." + name
		}
		props[name] = schemaOf(f.Type, p, enums)
	}
}
