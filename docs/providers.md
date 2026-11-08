# Grafana and other tools: Terraform and Crossplane

Clusters aren't the only thing built by hand. `gitopsify export` reads the
live state of other tools through **providers** and writes it as Terraform (or
OpenTofu) and Crossplane configuration that manages the same objects:

```text
 Grafana, ... ──► provider ──► gitopsify export ──► terraform/  (one root module per organization)
                                                └─► crossplane/ (one Kustomization per organization)
```

The first plan imports everything and changes nothing; from then on the tool
is managed as code.

## Providers

A provider is a small program, `gitopsify-provider-<name>`, published in its
own repository. Find and install one:

```sh
gitopsify provider search                 # what the provider index lists
gitopsify provider install grafana        # latest release; or grafana@v0.1.0
gitopsify provider info grafana           # settings, kinds, outputs
gitopsify provider list                   # what's installed (and on your PATH)
```

`install` downloads the release for your machine from GitHub, checks it against
the release's checksums, makes sure it runs, and only then replaces the
installed version. It uses `GITHUB_TOKEN` (or `GH_TOKEN`) when set, for private
repositories and GitHub's rate limits. Providers not in the index install by
source: `gitopsify provider install acme/keycloak`. Providers run with your
rights, like any program you install: install the ones you trust.

| Provider | Reads | Repository |
|---|---|---|
| `grafana` | Organizations, folders, dashboards, library panels, data sources, teams, playlists, alerting (templates, mute timings, contact points, notification policies, rule groups) | [hayone1/gitopsify-provider-grafana](https://github.com/hayone1/gitopsify-provider-grafana) |

To write one: [writing-providers.md](writing-providers.md).

## A first export

On a terminal, `gitopsify export` asks what it needs:

```sh
gitopsify export
```

It picks (or installs) the provider, asks for the connection (taking what the
environment already has: `GRAFANA_URL`, `GRAFANA_AUTH`), checks it, then asks
which organizations, which kinds of objects, Terraform and/or Crossplane,
whether to **adopt** the objects or **replicate** them elsewhere, whether to
pick individual objects, and where to write. It shows what it will write,
writes it, and offers to save your answers to `gitopsify-grafana.yaml` so the
same export can run again in CI.

The same without questions:

```sh
export GRAFANA_URL=https://grafana.example.com
export GRAFANA_AUTH=admin:...          # a server admin, to export every organization
gitopsify export grafana --no-input --to terraform,crossplane -o grafana
```

Then read `grafana/EXPORT_REPORT.md`: what was exported, what was left out and
why (provisioned dashboards, integrations the provider doesn't map yet),
secrets you need to provide, and the next steps.

## What you get

```text
grafana/
  EXPORT_REPORT.md
  terraform/
    README.md
    default/main-org/        # <instance>/<organization>: one root module each
      versions.tf providers.tf
      folders.tf dashboards.tf data_sources.tf contact_points.tf ...
      dashboards/sre/kubernetes/kubernetes-overview.json
    default/team-a/
  crossplane/
    README.md
    setup/                   # the Provider package: apply first
    kustomization.yaml
    components/observe-only/
    default/main-org/        # namespace grafana-main-org, its ClusterProviderConfig, managed resources
  .gitopsify/export.json     # stable names; don't edit
  .gitignore                 # secrets, Terraform state
```

Each organization (Grafana's partitions; a Keycloak provider would use realms)
gets its own Terraform state and its own Kustomization and namespace. Objects
reference each other: a dashboard's `folder = grafana_folder.kubernetes.uid` in
Terraform, `folderRef: {name: kubernetes}` in Crossplane. Big JSON documents
(dashboards) are files that Terraform reads with `file()`.

### Terraform

```sh
cd grafana/terraform/default/main-org
terraform init
GRAFANA_AUTH=admin:... terraform plan   # only imports
terraform apply
```

Every `<kind>.tf` ends with `import` blocks for the objects that were read, so
the first apply adopts them. Needs Terraform 1.7 or OpenTofu 1.10, or later. State is
local unless you configure a backend (below).

### Crossplane

Crossplane 2.0 or later (namespaced managed resources):

```sh
kubectl apply -k grafana/crossplane/setup
kubectl wait provider.pkg.crossplane.io/provider-grafana --for=condition=Healthy --timeout=10m
kubectl wait crd --all --for=condition=Established --timeout=5m
kubectl -n crossplane-system create secret generic grafana-default-credentials \
  --from-literal=credentials='{"url":"https://grafana.example.com","auth":"'"$GRAFANA_AUTH"'"}'
kubectl apply --server-side -k grafana/crossplane
kubectl get managed -A
```

While adopting, `components/observe-only` makes every managed resource only
observe its object: Crossplane reads and changes nothing, which is how you
check the mapping without a plan step. When everything is Ready and Synced,
remove the component from `crossplane/kustomization.yaml` and apply again.
The managed resources never delete what they adopted (no `Delete` management
policy) unless you add it.

## Adopt or replicate

| | Adopt (default) | Replicate (`--no-adopt`) |
|---|---|---|
| Use it for | Managing the instance you read | Building the same objects in another instance |
| Terraform | `import` blocks | No imports: everything is created |
| Crossplane | `crossplane.io/external-name`, observe-only first, no `Delete` | Created, with `Delete` |

## Secrets

No secret value goes into a tracked file, ever:

- Values the tool returns (a Slack webhook URL, a webhook password) become
  sensitive Terraform variables and Kubernetes Secrets referenced by
  `*SecretRef` fields. With `--secret-values local` their values are written to
  git-ignored files (`*.secrets.auto.tfvars`, which Terraform loads by itself,
  and `.secrets/`); by default you set them yourself (`TF_VAR_...`, the report
  lists them).
- Values the tool never returns (data source passwords) are left out and listed
  in the report, so the first plan stays clean. Add them when you want the
  configuration to manage them.
- Grafana doesn't give Terraform secret settings back either, so contact points
  with secrets show an in-place update with the same values on the first plan.

## Choosing what to export

```sh
gitopsify export grafana --partition main-org --partition 'team-*'
gitopsify export grafana --kind Folder,Dashboard
gitopsify export grafana --include 'Dashboard/main-org/kubernetes-*'   # plus the folders they need
gitopsify export grafana --exclude 'Dashboard/*/scratch-*'
gitopsify export grafana --dry-run                                     # show, don't write
```

Objects are addressed as `<Kind>/<organization>/<name>`, with the names shown
by `--dry-run` (lowercase, dashes). `--include` pulls in what the objects
reference unless `--no-deps`.

A selection adds objects; it never takes away what an earlier export already
manages. Export one dashboard into a directory that already holds the rest and
the rest stays. To stop managing objects the selection leaves out, add
`--prune`: Terraform then forgets them (removed blocks) and their Crossplane
files go away.

## Exporting again

Run the same export again to pick up what changed in the tool; the
configuration follows. It's built to be safe to repeat:

- Names stay the same when objects are renamed (they're recorded in
  `.gitopsify/export.json`), so a renamed dashboard doesn't become a new
  resource.
- Objects that are no longer exported (deleted in the tool, or no longer
  selected) get a `removed { lifecycle { destroy = false } }` block: Terraform
  forgets them and never deletes them.
- An export only rewrites what it read: other organizations, kinds and formats
  stay as they are, and objects exported before stay exported until they're
  deleted in the tool or you `--prune`.
- If the provider can't read something (an API error, a missing permission), the
  export fails (exit code 3) instead of treating those objects as deleted.
  `--allow-partial` writes the rest and keeps the previous files of what
  couldn't be read.

Edits you make to generated files are overwritten by the next export, which
warns about every generated file that changed since it wrote it. Keep your own
changes in separate files in the same directories: gitopsify only touches the
files it wrote, and adds its lines to an existing `.gitignore` instead of
replacing it.

## CI and the export file

`gitopsify-<provider>.yaml` holds the choices; commit it, and run
`gitopsify export --no-input` anywhere. Like gitopsify's other files it's a
resource in the Kubernetes style (`apiVersion`, `kind`, `metadata`, `spec`),
validated by [a JSON Schema](gitopsify-export.schema.json):

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/hayone1/gitopsify/main/docs/gitopsify-export.schema.json
apiVersion: gitopsify.io/v1alpha1
kind: Export
metadata:
  name: grafana
spec:
  provider:
    name: grafana
    source: hayone1/grafana
    version: v0.1.0             # gitopsify refuses another version
    checksums:                  # the release archive per platform, checked on install
      linux_amd64: 3f5c...
  instances:
    - name: ops
      settings:
        url: https://grafana-ops.example.com
        auth: {env: GRAFANA_OPS_AUTH}      # secrets: env or file, never values
    - name: edge
      settings:
        url: https://grafana-edge.example.com
        auth: {file: /run/secrets/grafana-edge}
  select:
    partitions: ["*"]           # names, IDs or globs
    kinds: [Folder, Dashboard, DataSource, ContactPoint, NotificationPolicy, RuleGroup]
    exclude: ["Dashboard/*/scratch-*"]
  output:
    dir: grafana
    formats: [terraform, crossplane]
    adopt: true
    secretValues: skip          # or local
    terraform:
      backend:                  # one state per workspace; {workspace} is e.g. ops/main-org
        type: s3
        settings: {bucket: my-state, key: "grafana/{workspace}.tfstate", region: eu-central-1}
    crossplane:
      namespacePrefix: grafana
      managementPolicies: [Observe, Create, Update]
```

The same file can be JSON, or HCL (`gitopsify-grafana.hcl`, written with
attributes as in the JSON form):

```hcl
apiVersion = "gitopsify.io/v1alpha1"
kind       = "Export"
metadata   = { name = "grafana" }
spec = {
  provider  = { name = "grafana", version = "v0.1.0" }
  instances = [{ name = "ops", settings = { url = "https://grafana-ops.example.com", auth = { env = "GRAFANA_OPS_AUTH" } } }]
  output    = { formats = ["terraform"] }
}
```

One file can hold several resources (YAML documents separated by `---`, a JSON
array or `kind: List`, or `items = [...]` in HCL), for example the cluster
migration and an export per tool; `gitopsify export <provider>` picks the
export of that provider. Flags override the file. Saving (`--save`, or the
wizard's offer) in a YAML file that holds other resources replaces only that
export's document and leaves the rest as it was, comments included; a JSON or
HCL file that holds other resources isn't rewritten, and the export is saved
to `gitopsify-<provider>.yaml` instead, with a warning.

In a pipeline (GitHub Actions):

```yaml
- run: curl -fsSL https://raw.githubusercontent.com/hayone1/gitopsify/main/install.sh | sh
- run: gitopsify export --no-input --install      # installs the pinned provider
  env:
    GRAFANA_OPS_AUTH: ${{ secrets.GRAFANA_OPS_AUTH }}
- uses: peter-evans/create-pull-request@v7         # a PR with what changed in Grafana
  with:
    title: Grafana changes
```

Exit codes: 0 written, 1 error, 3 the provider couldn't read everything.

## Troubleshooting

| Message | What to do |
|---|---|
| `provider grafana isn't installed` | `gitopsify provider install grafana`, or `--install` with a pinned version |
| `token refused` / `Grafana refused the credentials` | Check `GRAFANA_AUTH`: `user:password` or a service account token |
| A token sees one organization | Use a server admin's `user:password` to export every organization, or one instance per organization with its own token |
| `couldn't read ... HTTP 403` | The credentials lack a permission (alerting needs an Admin); fix it or `--allow-partial` |
| `... is a secret; write it as {env: ...}` | Secrets in the export file are references, not values |
| `holds no earlier export; use --force` | The output directory has other files; pick an empty one or `--force` |
| The first plan updates a contact point | Grafana doesn't return its secret settings to Terraform; the update writes the same values |
