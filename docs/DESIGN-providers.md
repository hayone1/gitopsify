# gitopsify providers: design

gitopsify started with one source, a Kubernetes cluster, and three outputs
(Kustomize, Argo CD, Flux). Providers add other sources: a provider reads the
live state of a tool (Grafana, Keycloak, ...) and gitopsify writes it as
Infrastructure as Code that manages the same objects, so the tool stops being
configured by hand. The first destinations are **Terraform/OpenTofu** and
**Crossplane** (Kustomize). This document records the use cases, the decisions
and why they were made. User and author guides: [providers.md](providers.md),
[writing-providers.md](writing-providers.md).

## 1. Use cases

| # | Use case | Example |
|---|----------|---------|
| P1 | **ClickOps → IaC, in place** | A Grafana with 3 organizations built by hand becomes one Terraform workspace (or one Kustomization) per organization that manages those same objects; the first `terraform plan` imports and changes nothing |
| P2 | **Several instances** | `ops`, `dev` and `edge` Grafanas: one export, one tree, one workspace per instance and organization |
| P3 | **Selected objects** | Only the dashboards in the `SRE` folder (and the folder they need) |
| P4 | **CI** | A nightly job re-exports with the same choices and opens a PR with what changed in the tool |
| P5 | **Replicate** | Read `grafana-test`, write IaC that creates the same objects in `grafana-uat` (no import) |
| P6 | **New provider** | Someone who knows Keycloak publishes `gitopsify-provider-keycloak` from their own repository, and gitopsify users install it by name |

Out of scope for now: keeping the IaC in sync with later changes made in the
tool while respecting edits already made to the generated files (§11), and data
(dashboard versions, alert state, users' passwords).

## 2. Prior art

| Project | What it does | What we take / avoid |
|---------|--------------|----------------------|
| Terraform `import` blocks + `plan -generate-config-out` (TF ≥ 1.5, OpenTofu ≥ 1.6) | Generates HCL for resources you list by ID | Import blocks are our adoption mechanism. Generated config is every attribute, inline JSON, no references, and it needs a Terraform run with credentials |
| Grafana's generator (`terraform-provider-grafana/pkg/generate`) | Lists a Grafana's resources, runs `generate-config-out`, strips defaults, extracts dashboards; basic Crossplane output | Shows the value (extracted dashboards, `org_id` handling). One flat directory, one ProviderConfig, no organization split, needs Terraform |
| Terraformer | Many cloud providers in one binary, HCL output | In-tree providers make the core the bottleneck; we keep providers out of tree |
| external-dns | In-tree providers, then out-of-tree **webhook** providers listed in its README | A list of known providers is enough registration; a narrow protocol lets providers live elsewhere |
| cert-manager | External issuers implement a small contract (a CRD) and are listed in the docs | Same: a contract plus a list |
| Crossplane providers and functions | OCI packages; functions are gRPC servers with a versioned request/response | A versioned request/response protocol; packages and gRPC are too heavy for a CLI |
| OpenTofu registry | A Git repository of small JSON files (one per provider) added by PR; versions read from the provider's GitHub releases (`terraform-provider-<name>`) | Our index works the same way: a reviewed list in Git, versions from releases, a naming convention |
| gh extensions, kubectl plugins (krew) | Executables named `gh-<x>` / `kubectl-<x>`; install from a repository or a curated index | Executables found by name, installed from releases |

## 3. Decisions

| # | Decision | Why |
|---|----------|-----|
| D1 | A provider is an **executable** named `gitopsify-provider-<name>`, in its own repository, that speaks **JSON over stdin/stdout**, one request per process | Any language works (Go is easiest with the SDK); nothing to keep running; debuggable by hand (`gitopsify-provider-grafana describe \| jq`); credentials go over stdin, never argv. go-plugin (gRPC) fits long sessions with many calls (Terraform's plan/apply); we make one or two. Go plugins break across versions and on Windows; WASM can't open network connections portably; containers would require Docker |
| D2 | **Protocol** `provider.gitopsify.io/v1alpha1`: `describe`, `check`, `discover` (with a *summary* detail level and a key scope for interactive selection). Answers are **streams of JSON messages** ending in `done` or `error`; progress is JSON log lines on stderr (`log/slog`); errors carry a code and a hint; the Go SDK points `os.Stdout` at stderr so a stray print can't corrupt the stream | `describe` drives the CLI and the wizard with no provider-specific code in gitopsify; `check` is the provider's `doctor` and lists partitions cheaply; a summary `discover` lets people pick objects without downloading every dashboard; streaming keeps memory flat and shows progress; a missing `done` means the provider died half way |
| D3 | The inventory is **Terraform-shaped**: each resource has a kind (mapped to a Terraform type and a Crossplane GVK) and snake_case attributes, plus markers for references, secrets, JSON values and files. The provider names the Crossplane `*Ref` and `*SecretRef` fields and the attribute paths that are Terraform blocks; everything else is **derived** with Upjet's conventions (lowerCamelCase field names, blocks keep their shape). Kinds whose Crossplane resource isn't Upjet-generated give `spec.forProvider` explicitly. A provider can declare one kind per upstream API version (Dashboard, DashboardV2) and pick per resource | Most Crossplane providers for SaaS tools are generated by Upjet from the Terraform provider, so one mapping gives both outputs. What can't be derived (reference field names, sensitive fields, blocks vs. attributes) is stated, not guessed |
| D4 | **Instances, partitions, workspaces.** An instance is one connection with one credential. A partition (Grafana organization, Keycloak realm) becomes a workspace: one Terraform root module and one Kustomization per instance × partition, and objects that belong to no partition go to `_instance`. A partition can carry provider settings: attributes for the Terraform provider block and fields for the Crossplane ProviderConfig (Grafana with basic auth: `org_id`). Org-scoped credentials (tokens) are one instance per organization | Separate state and blast radius per organization; the same split works for both outputs. Grafana's provider refuses `org_id` with tokens, and instance-wide objects need server-admin credentials: the credential decides what one connection can see |
| D5 | **Adopt by default.** Terraform: `import` blocks. Crossplane: `crossplane.io/external-name`, management policies without `Delete` and `LateInitialize`, and a first sync that only **observes** (a Kustomize component the user removes after checking that the objects are Ready). `--no-adopt` replicates instead | The objects exist (we just read them). The first plan/sync must change nothing; deleting the IaC must not delete the tool's objects until the user says so. Crossplane has no plan step, so Observe-only is its plan |
| D6 | **No secret in a tracked file.** Secret values the tool returns become sensitive variables (Terraform) or Secrets referenced by `*SecretRef` (Crossplane), written only to git-ignored files (`.secrets/`, `*.secrets.auto.tfvars`) when `secretValues: local`. Values the tool never returns, and secrets the Crossplane resource has no `*SecretRef` for, are left out and listed in the report | Same rule as the cluster migration. Leaving an unreadable secret out keeps the first plan clean |
| D7 | **Install from GitHub releases by convention, find by an index.** `owner/name` → `github.com/<owner>/gitopsify-provider-<name>`, asset `gitopsify-provider-<name>_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows), checked against the release's checksums file and, when the export file pins them, against per-platform checksums. A short name is looked up in `providers/index.yaml` in this repository (embedded in the binary for offline use), which also records the repository's numeric ID so a renamed or re-registered account can't serve the name. `PATH` works for development | OpenTofu-style: no registry service, registration is a reviewed PR, versions come from releases. Pinned checksums make CI reproducible and catch a release that changed. Private indexes: `--index`/`GITOPSIFY_PROVIDER_INDEX` |
| D8 | **Go SDK** as a separate module, `github.com/hayone1/gitopsify/sdk`, standard library only, with `sdk/sdktest`: conformance checks and validation of the output against the upstream Terraform schema (`terraform providers schema -json`) and the Crossplane CRDs | Providers must not inherit gitopsify's dependencies or Go version. An unknown marker is an error on both sides, so a newer SDK can't silently write a secret as plain text |
| D9 | Names: **`gitopsify-provider-<name>`**, not `{terraform,crossplane}-provider-<name>` | It follows Terraform's `<tool>-provider-<name>` pattern. `terraform-provider-grafana` and `crossplane-provider-grafana` are Grafana's own repositories; reusing the names would collide on GitHub, in Go module paths and in people's heads. One gitopsify provider produces both outputs |
| D10 | **No Git submodules** of the upstream Terraform or Crossplane provider | A provider needs the upstream *schema*, not its source. Submodules don't work with Go modules or `go install`, need `--recursive` clones and manual bumps, and pull a large repository for a few names. Instead: pin the upstream versions in `describe` (they end up in `required_providers` and the Crossplane `Provider`), keep the upstream schema and CRDs as test data, validate against them with `sdktest`, and prove the output with the real thing (a clean `terraform plan`, Crossplane objects that become Ready). A Go provider *may* depend on the upstream provider as a Go module to reuse its API client or readers; the reference provider doesn't, to stay small and independent of a large dependency graph |
| D11 | gitopsify's own files are **resources in the Kubernetes style** (`apiVersion`, `kind`, `metadata`, `spec`), like Grafana's dashboards outside Kubernetes, in **YAML, JSON or HCL** (HCL's attribute syntax mirrors the JSON form; several resources per file as YAML documents, a JSON array or `kind: List`, or `items = [...]`). Exports are `kind: Export` (`gitopsify-<provider>.yaml`, [schema](gitopsify-export.schema.json)); the wizard writes one so CI replays the same choices with `--no-input`. `kind: Migration` (`gitopsify.yaml`) is written in the resource form by `init` and `generate`, in the syntax of the file's extension; the layout v0.1 wrote (fields at the top) is still read, and its schema accepts both | Declarative CI with existing tools (editors validate with JSON Schema; `kubectl`-style Lists; the HCL library instead of a parser of our own). The same resources can later be reconciled continuously, by a `gitopsify sync` loop or as CRDs watched by a controller. Secrets in them are references (`env:`, `file:`), never values |
| D12 | **Re-exports are safe.** Names are stable per resource key and recorded in `.gitopsify/export.json` (Terraform address, Kubernetes name, file paths, workspace directory), so renaming a dashboard doesn't rename its address. A resource exported before and missing now gets `removed { lifecycle { destroy = false } }` (Terraform ≥ 1.7, OpenTofu ≥ 1.10). Terraform files are self-contained per kind (resources, imports, variables, removed blocks), and a run only rewrites the workspaces, formats and kinds it read completely. A partial read (the provider reports a warning for a partition or kind) fails unless `--allow-partial`, which keeps the previous files of the incomplete scopes. A selection only adds: objects exported before stay until they're deleted in the tool or `--prune` drops them. An object whose kind, partition or Terraform type changes keeps a removed block for its old address. `.gitopsify/export.json` also records a hash of every file written except the git-ignored secret files (a hash of one would let anyone who can read the repository check guesses of its secrets offline; they're rewritten from the tool every time), so hand edits are detected (and, later, merged) | Without this, a renamed dashboard, an alerting API that returned 500 in a nightly run, or a narrower selection would delete live objects on the next apply |
| D13 | **SDK in this repository, providers start from a template.** The SDK stays a nested module (`sdk/`, tagged `sdk/vX.Y.Z`); [gitopsify-provider-template](https://github.com/hayone1/gitopsify-provider-template) is a GitHub template repository with a working provider for a made-up tool, a rename tool (`go run ./tools/prepare`), `make upstream` (fetches the pinned Terraform schema and the Crossplane package's CRDs for the tests) and `make record`, CI and release workflows, and `AGENTS.md` | The same split as Terraform (terraform-plugin-framework + terraform-provider-scaffolding-framework) and Crossplane (crossplane-runtime/Upjet + upjet-provider-template). The SDK and its client (`pkg/provider`) change together in one pull request and are tested together; Go fetches only the `sdk/` directory, so providers don't download gitopsify. A template is copied once and owned by the author; generators would need maintaining for every change. Helpers every provider needs (`Resource.Summary`) go into the SDK, not the template |

## 4. Protocol (v1alpha1)

```text
gitopsify ──stdin: Request (JSON)──► gitopsify-provider-<name> <method>
          ◄─stdout: stream of Messages, ending with done or error
          ◄─stderr: JSON log lines (progress), or plain text
```

* `describe` (no stdin) → `Description`: name, version, protocols, connection
  **settings** (name, environment variable, secret, required, default, the
  Terraform provider attribute and Crossplane credential key it maps to), the
  **partition** noun, the **kinds** (Terraform type and blocks, Crossplane GVK
  and renames, optional), the upstream **Terraform provider** (source, version)
  and **Crossplane provider** (package, ProviderConfig kind).
* `check` → `CheckResult`: target (product, version, address), identity, the
  partitions the identity can see, and per kind whether it can be read.
* `discover` → messages: `partition`, `resource`, `skipped` (an object left out
  on purpose, with the reason: provisioned from files, read-only, ...) and
  `warning` (something that couldn't be read; with a partition and a kind, that
  scope is incomplete). The request's scope narrows partitions, kinds and keys;
  `detail: summary` asks for keys, names and dependencies without attributes.
* Failure: exit code ≠ 0 and `{"error": {"code", "message", "hint"}}`. Codes:
  `invalid-settings`, `unauthenticated`, `forbidden`, `unreachable`,
  `unsupported`, `canceled`, `internal`.

A resource:

```json
{
  "key": "dashboard/2/k8s-overview",
  "kind": "Dashboard",
  "partition": "2",
  "name": "Kubernetes overview",
  "importId": "2:k8s-overview",
  "attributes": {
    "folder": {"$ref": "folder/2/sre", "attr": "uid", "crossplane": "folderRef", "value": "sre"},
    "config_json": {"$file": "dashboards/sre/kubernetes-overview.json", "content": {"title": "Kubernetes overview"}}
  }
}
```

| Marker | Terraform | Crossplane |
|--------|-----------|------------|
| `{"$ref": key, "attr", "crossplane", "value"}` | `grafana_folder.sre.uid` when the target is in the same workspace; otherwise the literal `value` | `folderRef: {name, namespace}` replacing the field when the target is exported; otherwise, and inside list items (Crossplane's resolver would rewrite the atomic list and conflict with server-side apply), the literal `value` |
| `{"$secret": name, "value"?, "crossplane"?}` | `var.<name>` (sensitive), value in a git-ignored `<kind>.secrets.auto.tfvars` next to it | `<field>SecretRef: {name, key}` (`{name}` for maps), Secret under `.secrets/crossplane/` |
| `{"$json": any}` | `jsonencode({...})` | the JSON text |
| `{"$file": path, "content"}` | `file("${path.module}/<path>")`, content written to the file | the content inline (a YAML block scalar) |

Crossplane gets file content inline, not through a Kustomize generator: a
generator plus `replacements` works but needs a placeholder that a renamed
target silently keeps, and it grew to 35 s for 2000 dashboards in testing.

## 5. Layout

```text
<out>/
  EXPORT_REPORT.md                 # what was exported, skipped, needs secrets; next steps
  .gitignore                       # .secrets/, .terraform/, state files
  .gitopsify/export.json           # stable names and owned files (D12)
  .secrets/                        # git-ignored: tfvars and Secrets with values
  terraform/
    README.md
    <instance>/<partition>/        # one root module per workspace
      versions.tf  providers.tf    # required_providers, provider block (+ partition settings), backend
      <kind>.tf ...                # resources, imports, variables and removed blocks of one kind
      dashboards/<folder>/<name>.json
  crossplane/
    README.md
    setup/                         # the Provider package: apply first
    kustomization.yaml             # every workspace (+ the observe-only component while adopting)
    components/observe-only/
    <instance>/<partition>/        # namespace, ClusterProviderConfig, managed resources, kustomization
```

Names come from display names (lowercase, `_` or `-` separated, never starting
with a digit, `x` + hash when nothing is left), are unique per kind and
workspace (case-insensitively), avoid Windows reserved file names, and stay as
recorded in `.gitopsify/export.json`. Namespaces are at most 63 characters.

## 6. CLI

```text
gitopsify provider list                     # installed and on PATH
gitopsify provider search [term]            # the index
gitopsify provider install grafana[@v0.1.0] # or owner/name
gitopsify provider remove grafana
gitopsify provider info grafana             # settings, kinds, outputs (describe)
gitopsify export [provider]                 # wizard on a terminal
gitopsify export grafana --to terraform --partition main-org --kind Dashboard,Folder --no-input
gitopsify export grafana --dry-run          # what would be written
```

Wizard: provider (install from the index if needed) → connection (settings from
the environment first, secrets masked) → `check` → partitions → kinds → all
objects or pick some (summary `discover`) → outputs → adopt or replicate →
output directory → summary and confirmation → write → next steps. It offers to
save the answers to `gitopsify-<provider>.yaml`, which CI replays.

## 7. Selection

Resources are addressed as `<Kind>/<partition>/<name>` with the slugs shown by
`--dry-run` (titles can contain `/`), matched with globs (`--include`,
`--exclude`) after `--partition` and `--kind`. Selecting an object pulls in what
it references (a dashboard's folder, a folder's parent) unless `--no-deps`.

## 8. Testing

* SDK: protocol round trips; `sdktest.Conformance`; `sdktest` validation against
  a Terraform schema dump and CRDs.
* gitopsify: golden trees from a fixture provider (the test binary acting as a
  provider), `terraform validate` and `kustomize build` on the golden output,
  re-export safety (renames, partial reads, narrowed scopes).
* Providers: unit tests against a fake HTTP API replaying recorded answers,
  validated against the pinned upstream schema and CRDs (`make record`,
  `make upstream` in the template); a live test against the real tool in
  Docker: export, `terraform plan` shows imports only, Crossplane objects
  become Ready and Synced without changing the tool.

## 9. Requirements of the output

* Terraform ≥ 1.7 or OpenTofu ≥ 1.10 (`removed` blocks with `lifecycle`); state is local unless
  `output.terraform.backend` is set (a backend block per workspace, with
  `{workspace}` in its settings).
* Crossplane ≥ 2.0: namespaced managed resources (`*.m.crossplane.io`).

## 10. Known limitations

Found in review and testing, and left as they are on purpose or for later:

* **Release integrity.** Checksums prove a download matches its release, and
  pinned checksums (all platforms, from the export file) prove it matches what
  was reviewed; neither proves who built it. Verifying GitHub artifact
  attestations is future work. The installed binary's hash is kept next to it,
  so it catches corruption, not someone who can write to the providers
  directory. Providers run with your user's rights.
* **OpenTofu 1.7-1.9** accept `required_version = ">= 1.7.0"` but not
  `lifecycle` inside `removed` blocks: they fail with a parse error (nothing is
  destroyed). Use OpenTofu 1.10 or later.
* **Secrets the tool won't return** can't make a clean first plan when they're
  set: Grafana doesn't give Terraform contact point secrets back, so adopting
  rewrites them once, with the same values. Secrets it never returns (data
  source passwords) are left out and reported.
* **The mapping is the provider's.** It is proven by its live test, not by
  gitopsify. Fields the upstream schema doesn't mark sensitive can't be
  referenced from a Crossplane Secret. Objects a provider can't express
  completely are skipped whole, with the reason (for Grafana: contact points
  with unmapped integrations or settings, notification policies with legacy
  matchers, dashboards stored with Grafana 13's new layouts, teams whose
  preferences Terraform refuses).
* **Re-exports keep history.** Objects that are gone keep their state entry and
  their `removed` block (dropping either could make a later apply destroy
  them), so both accumulate until you delete them by hand. Import blocks come
  back on every export (they do nothing once imported).
* **Large Crossplane objects:** a dashboard is stored in `spec` and again in
  `status.atProvider`; the report warns above 512 KiB (etcd's limit is
  1.5 MiB). Apply with server-side apply.
* **A kind change with the old kind out of scope:** when an object moves to
  another kind of the same Terraform type and the export doesn't include the
  old kind, both kinds' files declare the address and Terraform stops with
  "Duplicate resource" (nothing is destroyed). Export both kinds once.
* **Small ones:** two concurrent `gitopsify provider install` runs of the same
  provider can race; YAML error line numbers in multi-document files may not
  match the file; a summary listing (the wizard's object picker) can show
  objects the full read then skips.

## 11. Next: continuous use

Re-exports are safe (D12) but authoritative: hand edits to generated files are
overwritten. Respecting them needs a three-way merge between the last export,
the current export and the files in Git, keyed by the stable resource keys and
names in `.gitopsify/export.json`. Because exports (and migrations) are
declarative resources (D11), continuous use can then be a loop (`gitopsify
sync`, in CI or a CronJob) or a controller reconciling `Export` objects in a
cluster, reporting in their `status` what changed in the tool. That is the next
design.
