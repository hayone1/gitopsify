# Writing a provider

A provider teaches gitopsify to read one tool. You write the part that knows
the tool's API and how its objects map to the tool's Terraform provider;
gitopsify does the rest: the wizard and CI flags, connection settings, layout
per partition, Terraform and Crossplane output, adoption, references, secrets
kept out of Git, safe re-exports and the report.
[DESIGN-providers.md](DESIGN-providers.md) explains why it works this way.

The reference provider is
[gitopsify-provider-grafana](https://github.com/hayone1/gitopsify-provider-grafana);
copy its layout.

## The contract

- A repository and an executable named **`gitopsify-provider-<name>`**
  (lowercase letters, digits, dashes), like Terraform's
  `terraform-provider-<name>`. Don't name it after the upstream Terraform or
  Crossplane provider: those names are taken by their repositories.
- gitopsify runs it once per call: `describe` (no input), `check` and
  `discover` (a JSON request on stdin). It answers on stdout with JSON messages
  that end with `{"done":{}}` or `{"error":{...}}`, and logs progress on stderr
  as JSON lines. The protocol is `provider.gitopsify.io/v1alpha1`; its types are
  in [`sdk/protocol.go`](../sdk/protocol.go).
- Any language works. In Go, the SDK does the protocol for you:

```sh
go get github.com/hayone1/gitopsify/sdk
```

The SDK is its own module with no dependencies outside the standard library.

## In Go

```go
package main

import "github.com/hayone1/gitopsify/sdk"

func main() { sdk.Serve(&provider{}) }

type provider struct{}

func (p *provider) Describe() sdk.Description { /* below */ }

// Check connects with req.Settings and lists the partitions the credentials see.
func (p *provider) Check(ctx context.Context, req sdk.Request) (*sdk.CheckResult, error)

// Discover sends partitions and resources as it finds them.
func (p *provider) Discover(ctx context.Context, req sdk.Request, out sdk.Emitter) error
```

`sdk.Serve` handles the command line, stdin/stdout, cancellation (Ctrl-C,
timeouts), panics, and points `os.Stdout` at stderr so a stray print can't
corrupt the answer. Log with `sdk.Logger(ctx).Info(...)`: gitopsify shows Info
messages as progress.

### Describe

```go
sdk.Description{
    Name: "keycloak", Title: "Keycloak", Version: version,
    Settings: []sdk.Setting{
        {Name: "url", Description: "the server URL", Env: "KEYCLOAK_URL", Required: true,
            Terraform: "url", Crossplane: "url"},
        {Name: "client_secret", Description: "...", Env: "KEYCLOAK_CLIENT_SECRET", Secret: true, Required: true,
            Terraform: "client_secret", Crossplane: "client_secret"},
    },
    Partition: sdk.Noun{Singular: "realm", Plural: "realms"},
    Kinds: []sdk.Kind{
        {Name: "Client", Plural: "Clients", Description: "OpenID Connect clients",
            Terraform:  &sdk.TerraformType{Type: "keycloak_openid_client", Blocks: []string{"authentication_flow_binding_overrides"}},
            Crossplane: &sdk.CrossplaneType{APIVersion: "openidclient.keycloak.m.crossplane.io/v1alpha1", Kind: "Client"}},
    },
    Terraform:  &sdk.TerraformProvider{Name: "keycloak", Source: "keycloak/keycloak", Version: "~> 5.0"},
    Crossplane: &sdk.CrossplaneProvider{Name: "provider-keycloak", Package: "xpkg.upbound.io/crossplane-contrib/provider-keycloak:v2.0.0",
        ProviderConfig: sdk.GVK{APIVersion: "keycloak.m.crossplane.io/v1beta1", Kind: "ClusterProviderConfig"}},
}
```

- **Settings** use the upstream Terraform provider's environment variables,
  so one environment serves gitopsify and `terraform plan`. `Terraform` and
  `Crossplane` name the provider block attribute and the key in the Crossplane
  credentials JSON (Upjet providers use the Terraform names).
- **Partition** is what the tool divides objects into; each partition becomes
  a workspace. Instance-wide objects have an empty partition.
- **Kinds** map to a Terraform resource type and a Crossplane GVK. `Blocks`
  lists the attribute paths that are nested blocks in the Terraform schema
  (`sdktest.TerraformBlocks` reads them from a schema dump). Kinds whose
  Crossplane resource isn't Upjet-generated set `Crossplane.Explicit` and give
  `Resource.Crossplane` themselves. Mark kinds most people don't want
  `Optional`.
- **Versions**: pin the upstream Terraform and Crossplane versions your
  output is tested with.

### Check

Connect, say what you're connected to and as whom, list the partitions, and
say per kind whether it can be read. Return `sdk.Errorf(code, ...)` with a
`WithHint(...)` saying what to do: gitopsify shows both. Use the codes in
[`sdk/errors.go`](../sdk/errors.go); `sdk.CodeForHTTPStatus` maps HTTP
statuses.

### Discover

For each partition in `req.Scope` send `out.Partition(...)`, then the
resources:

```go
out.Resource(sdk.Resource{
    Key:       "client/" + realm + "/" + c.ID,     // stable, unique per instance
    Kind:      "Client",
    Partition: realm,
    Name:      c.ClientID,                          // display name: file and resource names come from it
    ImportID:  realm + "/" + c.ID,                  // what `terraform import` takes
    ExternalName: c.ID,                             // only when Crossplane's external-name differs
    Attributes: map[string]any{                     // the Terraform arguments, by schema name
        "realm_id":  sdk.Ref("realm/"+realm, "id", realm, "realmIdRef"),
        "client_id": c.ClientID,
        "client_secret": sdk.Secret("client_secret", c.Secret, "clientSecretSecretRef", "client secret"),
        "extra_config":  map[string]any{"key": "value"},
    },
})
```

Attribute values are JSON values or markers:

| Marker | Use it for |
|---|---|
| `sdk.Ref(key, attr, value, crossplaneRef)` | An attribute that holds another object's ID. Terraform gets a reference when both are in the same workspace, Crossplane the `*Ref` field you name (except inside blocks, where resolving would rewrite the list and fight server-side apply); otherwise the literal value |
| `sdk.Secret(name, value, crossplaneSecretRef, description)` | Every attribute the Terraform schema marks sensitive. `value` is nil when the tool doesn't return it: the attribute is then left out and reported |
| `sdk.JSON(v)` | A string attribute holding JSON. Pass `sdk.RawJSON` to keep the key order |
| `sdk.File(path, content)` | A big document (a dashboard): written to its own file |

And:

- `out.Skip(sdk.Skipped{...})` for objects you leave out on purpose
  (provisioned from files, managed by another system, something you can't
  express without losing data). Say why.
- `out.Warn(sdk.Warning{Partition, Kind, Message})` when something can't be
  read. **Always set Partition and Kind**: gitopsify then keeps the previous
  export's files for that scope instead of treating its objects as deleted.
- Honour `req.Scope` (partitions, kinds, keys) to avoid reading what isn't
  needed, and `req.Detail == sdk.DetailSummary` (keys, names and `DependsOn`,
  no attributes) so people can pick objects quickly.

What makes the first plan clean:

- Emit every attribute whose value differs from the schema's default, with
  the schema's types (a string `"1"`, not the number 1).
- Leave out what the tool assigns (IDs, versions) and computed-only
  attributes.
- Never emit part of an object whose missing part the upstream provider would
  delete (a contact point with an integration you can't map): skip the whole
  object instead.
- Watch for authoritative attributes (member lists) and singletons that
  always exist (a default organization).

## Testing

1. **Conformance**, against a fake API (`httptest`):
   ```go
   sdktest.Conformance(t, provider.New(), sdk.Request{Settings: map[string]string{"url": srv.URL}})
   ```
2. **Upstream schemas**: keep `terraform providers schema -json` (trimmed to
   your types) and the Crossplane CRDs of the pinned versions in `testdata/`,
   and check every resource against them:
   ```go
   sdktest.ValidateTerraform(schema, d, inv)   // names, types, blocks, required, sensitive → Secret
   sdktest.ValidateCrossplane(crds, d, inv)    // derived fields, *Ref / *SecretRef names, types
   ```
3. **Real answers**: record what the real tool returns and replay it in unit
   tests (the Grafana provider records with `GITOPSIFY_GRAFANA_RECORD=dir`).
4. **Live**: the real tool in Docker, `gitopsify export`, `terraform plan`
   (imports only), `apply`, `plan` again (no changes), and Crossplane objects
   that become Ready and Synced while observing and while managing, without
   changing the tool. The Grafana provider's `test/live/crossplane.sh` does all
   of it on a throwaway machine.

The output is only as good as the mapping, and the live test is what proves it.

## Upstream code: modules, not submodules

You need the upstream provider's *schema*, not its source, so don't add it as a
Git submodule: submodules don't work with Go modules or `go install`, need
`--recursive` clones and manual bumps, and pull a large repository for a few
names. Pin the versions in `Describe`, keep the schema and CRDs as test data,
and validate. If you want to reuse the upstream provider's API client, depend
on it as a Go module.

## Releasing

Publish GitHub releases with archives named
`gitopsify-provider-<name>_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows)
and a checksums file: GoReleaser's defaults with `project_name:
gitopsify-provider-<name>`. The Grafana provider's `.goreleaser.yaml` is a
starting point. Build for linux, darwin and windows on amd64 and arm64.

Until the SDK has tagged releases, build against a checkout
(`replace github.com/hayone1/gitopsify/sdk => ../gitopsify/sdk`); require a
tagged `sdk/vX.Y.Z` before you release.

## Listing it in the index

`gitopsify provider install <name>` and `gitopsify provider search` read
[`providers/index.yaml`](../providers/index.yaml). Open a pull request that
adds your provider (sorted by name):

```yaml
  - name: keycloak
    source: acme/keycloak                  # github.com/acme/gitopsify-provider-keycloak
    description: "Keycloak: realms, clients, roles, groups, identity providers"
    homepage: https://github.com/acme/gitopsify-provider-keycloak
    maturity: alpha
    outputs: [terraform, crossplane]
    maintainers: [acme]
    repoId: 123456789                      # gh api repos/acme/gitopsify-provider-keycloak --jq .id
```

A listed provider has a public repository, a release with the archives above,
and passes `sdktest.Conformance`. Versions aren't listed: they come from your
releases. `repoId` makes sure the name keeps pointing at your repository even if
the account changes hands. Unlisted providers install by source
(`gitopsify provider install acme/keycloak`), and anything named
`gitopsify-provider-*` on `PATH` works too, which is how you test while
developing:

```sh
go build -o ~/go/bin/gitopsify-provider-keycloak . && gitopsify export keycloak
```
