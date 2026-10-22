package config

import (
	"encoding/json"
	"reflect"
	"strings"
)

// SchemaURL is where the JSON Schema of gitopsify.yaml is published.
const SchemaURL = "https://raw.githubusercontent.com/hayone1/gitopsify/main/docs/gitopsify.schema.json"

// SchemaHeader makes YAML editors validate and complete the file.
const SchemaHeader = "# yaml-language-server: $schema=" + SchemaURL + "\n"

// enums lists the allowed values of fields, by JSON path ("[]" for list items).
var enums = map[string][]string{
	"apiVersion":                        {APIVersion},
	"kind":                              {Kind},
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

// Schema returns the JSON Schema of the configuration file.
func Schema() ([]byte, error) {
	s := schemaOf(reflect.TypeOf(Config{}), "")
	s["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	s["$id"] = SchemaURL
	s["title"] = "gitopsify migration"
	s["description"] = "gitopsify.yaml: describes a migration of a Kubernetes cluster to GitOps. Reference: https://github.com/hayone1/gitopsify/blob/main/docs/configuration.md"
	b, err := json.MarshalIndent(s, "", "  ")
	return append(b, '\n'), err
}

func schemaOf(t reflect.Type, path string) map[string]any {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var s map[string]any
	switch t.Kind() {
	case reflect.Struct:
		props := map[string]any{}
		addFields(t, path, props)
		s = map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	case reflect.Slice:
		s = map[string]any{"type": "array", "items": schemaOf(t.Elem(), path+"[]")}
	case reflect.Map:
		s = map[string]any{"type": "object", "additionalProperties": schemaOf(t.Elem(), path+"{}")}
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
func addFields(t reflect.Type, path string, props map[string]any) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" || !f.IsExported() {
			continue
		}
		if name == "" && (f.Anonymous || strings.Contains(tag, "inline")) {
			addFields(f.Type, path, props)
			continue
		}
		if name == "" {
			name = f.Name
		}
		p := name
		if path != "" {
			p = path + "." + name
		}
		props[name] = schemaOf(f.Type, p)
	}
}
