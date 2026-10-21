# gitopsify

Turn what runs in a Kubernetes cluster into a GitOps repository.

`test` was built by hand with `kubectl` and `helm`. You want `uat` built from
Git by Argo CD or Flux, or you want Argo CD or Flux to take over the cluster
that already exists. gitopsify reads the live cluster and writes the
repository:

- **Clean manifests.** Runtime metadata, status, server-side defaults and
  cluster-allocated values (`clusterIP`, allocated `nodePort`s, PVC
  `volumeName`, HPA-managed `replicas`, injected `caBundle`s) are stripped.
  Two strategies: one keeps only the fields that a client field manager owns
  (`managedFields`), the other removes known Kubernetes defaults.
- **Leaves out what shouldn't be in Git, and says why.** That covers system
  namespaces, controller-owned objects (ReplicaSets, Pods, Jobs of CronJobs,
  operator output), Kubernetes' own RBAC and APF objects, platform components'
  RBAC, Helm release records, finished Jobs and default ServiceAccounts. Every
  exclusion is reported with the rule that made it, and every rule can be
  turned off.
- **Keeps Helm releases as Helm.** gitopsify finds the chart's repository even
  though Helm doesn't record it, and keeps the chart version and the values
  you set. When the repository can't be found, it rebuilds the chart from the
  release, or falls back to plain manifests. It also reports objects edited
  since the install.
- **Orders reconciliation.** CRDs come before custom resources, and operators
  before what they serve. Dependencies are inferred from CRD ownership, and
  you can pin waves or add `dependsOn`. The result becomes Argo CD sync waves
  or Flux `dependsOn`.
- **Keeps Secrets out of Git in plain text.** Choose skip, local git-ignored
  files, SOPS, Sealed Secrets (sealed in-process with the target's key) or
  External Secrets.
- **Handles environments.** Use per-environment overlays, or one repository
  per environment, with transforms for hostnames, registries, storage and
  ingress classes, namespaces, Helm values and arbitrary fields.
- **Goes as far as you want.** Get plain Kustomize apps, or Argo CD (app of
  apps, ApplicationSet or a single Application), or Flux (Kustomizations +
  HelmReleases). It can also install Argo CD or Flux in the target and point it
  at the repository.
- **Supports takeover.** `generate --adopt` writes sync settings that are safe
  when the objects already exist. `diff` server-side dry-runs everything
  against the target first, and `adopt` retires the Helm release records.

## Install

```sh
go install github.com/hayone1/gitopsify/cmd/gitopsify@latest
```

or build from a checkout with `make build` (binary in `bin/`).

## Quick start

```sh
# 1. Describe the migration (interactive on a terminal; flags otherwise).
gitopsify init

# 2. See what would be migrated, how, and what is left out.
gitopsify scan --context test

# 3. Write the repository.
gitopsify generate --context test --tool argocd --env uat=uat-cluster \
  --repo-url https://github.com/acme/platform-gitops.git

# 4. Check the target, preview the first sync, then hand it to Argo CD.
gitopsify check --env uat
gitopsify diff --env uat
gitopsify bootstrap --env uat --git-token "$GITHUB_TOKEN"
```

Or in one go: `gitopsify migrate --context test --env uat=uat-cluster --tool argocd --repo-url ... --push`.

Every command reads `./gitopsify.yaml` when it exists; flags override it.
`generate` writes the effective configuration into the output, so
`gitopsify generate -c gitopsify.yaml` regenerates the same repository. Files
you add by hand survive regeneration.

## Common migrations

| You want to… | Command |
|---|---|
| Build `uat` from Git with Argo CD | `generate --tool argocd --env uat=uat-ctx --repo-url …` |
| …with Flux | `generate --tool flux --env uat=uat-ctx --repo-url …` |
| Hand Git to people who run their own tooling | `generate` (`--tool none`: Kustomize apps, Helm values, `apply-<env>.sh`) |
| Have Argo CD take over the cluster it was exported from | `generate --tool argocd --adopt --env prod=prod-ctx …`, `diff`, `adopt`, `bootstrap` |
| Move one app and what it needs | `generate -r deployment/shop/web` (or `-l app=web`) |
| Move some namespaces | `generate -n shop -n 'payments-*'` |
| Move a whole cluster except some things | `generate --exclude-namespace legacy --exclude-kind CronJob` |
| Several environments from one source | `--env uat=uat-ctx --env prod=prod-ctx`, per-environment `transforms` in `gitopsify.yaml` |
| One repository per environment | `--layout repo-per-env` (with `environments[].repoURL`) |
| Work from a dump, without access to the cluster | `generate --from-file dump.yaml` (`kubectl get … -A -o yaml` output) |
| Pick apps interactively | `generate -i` |

## Maturity levels

| Level | You get |
|---|---|
| `scan` | Inventory: apps, Helm releases, exclusions with reasons, warnings |
| `generate --tool none` | Clean Kustomize bases and per-environment overlays; Helm apps as `helmCharts:` Kustomizations; an `apply-<env>.sh` that applies everything in order. Wire it into any tool |
| `generate --tool argocd` | Plus Applications (`--pattern app-of-apps`, `applicationset` or `single`), an AppProject, OCI repository registrations and a pinned Argo CD install with the settings the apps need |
| `generate --tool flux` | Plus Flux `Kustomization`s with `dependsOn`, `HelmRelease`s with `HelmRepository`/`OCIRepository` sources and values from files, and a pinned Flux install |
| `bootstrap` / `migrate` | Argo CD or Flux installed in the target, repository credentials registered, the root object applied |

Example output of every mode, generated from [a real cluster](testdata/clusters/clickops.yaml), is in [testdata/golden](testdata/golden):
[plain Kustomize](testdata/golden/none-folder-per-env),
[Argo CD app of apps](testdata/golden/argocd-app-of-apps),
[ApplicationSet](testdata/golden/argocd-applicationset),
[single Application](testdata/golden/argocd-single),
[takeover](testdata/golden/argocd-adopt),
[Flux, repository per environment](testdata/golden/flux-repo-per-env),
[one app and its dependencies](testdata/golden/selection).

## What gets migrated

`gitopsify scan --show-excluded` lists every object that is left out, with the
rule and the reason; `gitopsify rules` lists the rules. Turn one off with
`--disable-rule <name>` (or all with `"*"`), or override with `scope.rules`.

Objects are grouped into **apps** (one Argo CD Application or Flux
Kustomization each):

- one app per Helm release
- one per namespace (or per label value with `--group-by label`, or a single
  app with `--group-by single`; `grouping.apps` assigns objects explicitly)
- `crds`, `namespaces` (Namespace objects, so their labels, such as Pod
  Security or istio-injection, are kept) and `cluster` (other cluster-scoped
  objects)

When you select namespaces (`-n`) or objects (`-r`, `-l`), dependencies come
along: ConfigMaps, Secrets, ServiceAccounts and their (Cluster)Role(Binding)s,
PVCs and StorageClasses, PriorityClasses, Services selecting the pods,
Ingresses, Gateway API routes, HPAs/KEDA/VPA targeting the workload,
PodDisruptionBudgets, NetworkPolicies, ServiceMonitors, the Namespace and
the CRDs of custom resources. `--no-deps` turns this off.

## Helm releases

gitopsify decodes Helm's release records and gets the chart, version, the
values you set (what `helm get values` shows), the manifest and the chart's
CRDs. Helm doesn't record the chart's repository, so it is looked up:

1. `helm.repositories` / `--helm-repo chart=url` (also `oci://`)
2. your local Helm repository cache (`helm repo add` repositories)
3. Artifact Hub, with `--artifacthub`

| `helm.mode` | Result |
|---|---|
| `auto` (default) | `chart` when the repository is found, else `vendor`, else `manifests` |
| `chart` | Argo CD multi-source Application (chart + values from Git) or Flux HelmRelease |
| `vendor` | The chart, rebuilt from the release, is written to `apps/<app>/charts/` and deployed from Git (charts with subcharts can't be rebuilt; Helm doesn't store them) |
| `manifests` | The release's objects as plain manifests |

The objects of a release are compared with the release's manifest. Fields
someone changed since (`kubectl edit`, `kubectl set env`) are reported, since
the GitOps tool will set them back. Values whose keys look like credentials
are reported too.

## Ordering

Apps get a phase: `crds` (-30), `namespaces` (-20), `cluster` (-10),
`platform` (-5: apps with CRDs, webhooks, APIServices, IngressClasses,
StorageClasses), `apps` (0), `custom-resources` (10). An app that contains
custom resources depends on the app that installs their CRD, including the
SealedSecrets and ExternalSecrets gitopsify writes. `--wave app=N` and
`--depends-on app=a,b` add your own constraints; cycles are errors.

- **Argo CD** gets `sync-wave` annotations, the `argoproj.io/Application`
  health check that app-of-apps waves need (removed from Argo CD in 1.8), and
  `SkipDryRunOnMissingResource` where a CRD comes from another app. The
  ApplicationSet uses `RollingSync` steps, and the bootstrap turns on
  progressive syncs.
- **Flux** gets `dependsOn` (dependencies plus the closest lower wave), and
  `wait: true` on the apps others depend on.

## Secrets

| `--secrets` | Result | Needs in the target |
|---|---|---|
| `skip` (default) | Nothing written; listed in `MIGRATION_REPORT.md` | Create them yourself |
| `local` | `.secrets/<ns>/<name>.yaml`, git-ignored | `kubectl apply -f .secrets/` |
| `sealed-secrets` | `SealedSecret`s sealed with the target controller's certificate (fetched through the API server, or `--sealed-secrets-cert`), one per environment | sealed-secrets controller |
| `sops` | Secrets encrypted with `sops` (`--sops-age …`), `.sops.yaml`, Flux `decryption` | Flux: the decryption key (`bootstrap --sops-age-key`); Argo CD: KSOPS or helm-secrets |
| `external-secrets` | An `ExternalSecret` per Secret; the values to load into your store in `.secrets/external-secrets/` | External Secrets Operator |
| `plain` | Secret manifests as they are (`--allow-plain-secrets`) | — |

`secrets.overrides` picks a strategy per namespace or name.

## Environments and transforms

`--env uat=uat-ctx --env prod=prod-ctx` defines the environments (the kube
context is used by `check`, `diff`, `adopt`, `bootstrap` and to fetch the
sealing certificate). With `folder-per-env` each app gets
`base/` and `envs/<env>/`; environment transforms become Kustomize patches,
and Helm values overrides become `envs/<env>/values.yaml`. With
`repo-per-env` each environment gets its own self-contained tree.

```yaml
transforms:                       # every environment
  - type: image
    from: test.azurecr.io/
    to: platform.azurecr.io/
environments:
  - name: uat
    cluster: {context: uat}
    transforms:
      - {type: replace, from: test.example.com, to: uat.example.com}
  - name: prod
    cluster: {context: prod, server: https://prod.example.com:6443}
    transforms:
      - {type: replace, from: test.example.com, to: example.com}
      - {type: storageClass, map: {standard: premium-ssd}}
      - {type: set, target: {kinds: [Deployment], names: [api]}, path: spec.replicas, value: 3}
      - {type: set, target: {apps: [redis]}, path: values.replica.replicaCount, value: 3}
```

Transform types: `replace`, `image`, `storageClass`, `ingressClass`,
`namespace`, `set`, `remove`, `jsonPatch`, `labels`, `annotations`. See
[docs/configuration.md](docs/configuration.md).

## Taking over a running cluster

```sh
gitopsify generate --tool argocd --adopt --env prod=prod --repo-url … --push
gitopsify diff --env prod          # should list only changes you expect
gitopsify adopt --env prod --helm-records backup-delete
gitopsify bootstrap --env prod
```

`--adopt` sets automated sync without pruning (Flux only prunes what it
applied, so it keeps `prune: true`), server-side apply with
`ClientSideApplyMigration=false`, so fields owned by `kubectl` and other
managers aren't deleted on the first sync, and Helm drift detection in
`warn` mode. PVCs, PersistentVolumes, Namespaces and CRDs are never pruned or
deleted (`adopt.protectKinds`). `adopt` backs up and removes Helm release
records that would go stale, because Argo CD renders charts itself. Flux
HelmReleases reuse them, so they are kept.

## Commands

| Command | |
|---|---|
| `init` | Write `gitopsify.yaml`; a wizard on a terminal |
| `scan` | Inventory; `--show-excluded`, `--show-objects`, `--format json\|yaml` |
| `generate` | Write the repository; `-i` to pick apps, `--commit`/`--push`, `--dry-run` |
| `check` | Preflight the target: API versions served, StorageClasses/IngressClasses, existing owners, Helm records, secret controllers |
| `diff` | Server-side dry-run against the target; `--exit-code` for CI |
| `adopt` | Back up / delete stale Helm release records in the target |
| `bootstrap` | Install the pinned Argo CD/Flux, register the repository, apply the root object |
| `migrate` | `generate` → commit/push → `check` → `bootstrap` |
| `rules` | List the exclusion rules |

`--no-input` never prompts (CI); `--no-color` or `NO_COLOR` disables colors.

## Development

```sh
make test     # unit and golden tests (needs kubectl for the Kustomize build checks)
make golden   # regenerate testdata/golden after an intended output change
make e2e      # kind clusters, Argo CD and Flux end to end (~15 minutes)
```

The design, the problems it addresses and how they map to the code are in
[docs/DESIGN.md](docs/DESIGN.md).

## Limitations

- Persistent volume data and container images are not copied; use Velero or
  `crane transfer-pvc`, and your registry's replication.
- The heuristic cleaner removes the defaults it knows. Objects applied with
  server-side apply (`kubectl apply --server-side`, Helm 4, Argo CD, Flux) are
  pruned precisely with managed fields instead.
- Charts with subcharts can't be rebuilt from a release; without a known
  repository they are migrated as manifests.
- Objects already managed by Argo CD or Flux are left out by default
  (`--include-managed` to change that); gitopsify doesn't convert between the
  two tools.
