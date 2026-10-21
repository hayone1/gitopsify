# gitopsify design

gitopsify reads what is running in a Kubernetes cluster and produces a GitOps
repository that reproduces it in another cluster, or that lets Argo CD or Flux
take over the same cluster. This document records the use cases, the problems
people hit when they do this by hand, and how each part of the tool answers
them.

## 1. Use cases

| # | Use case | What the user has | What they want |
|---|----------|-------------------|----------------|
| UC1 | **ClickOps → GitOps across environments** | `test` built with `kubectl`, Helm CLI, dashboards | `uat` (and later `prod`) built from Git by Argo CD or Flux |
| UC2 | **In-place takeover** | A cluster nobody can rebuild | The same cluster managed from Git, with a no-op first sync |
| UC3 | **Selective migration** | One app (a Deployment, its config, Service, Ingress, HPA…) | Only that app and everything it needs |
| UC4 | **Whole-cluster migration** | A busy cluster | Everything except system and controller-generated objects |
| UC5 | **Helm CLI → GitOps Helm** | Releases installed with `helm install` | `HelmRelease`s or Argo CD Helm Applications with the same chart, version and values |
| UC6 | **Cluster rebuild / DR** | A live cluster, no repo | A repo that can rebuild the cluster from scratch |
| UC7 | **Multi-environment layout** | One source cluster | Overlays per environment, or one repo per environment |
| UC8 | **Inventory / audit** | A cluster | A report of what is not yet in Git and why |

Out of scope: copying persistent volume data (use Velero or
`crane transfer-pvc`), copying container images between registries (the
`image` transform rewrites references, it does not copy), and translating
between Argo CD and Flux (objects already managed by either are skipped by
default and reported).

## 2. What goes wrong when people do this by hand

The table lists the problems found in migration guides, issue trackers and
tool documentation (Argo CD, Flux, Konveyor Crane, kube-dump, katafygio,
kubectl-neat, argocd-autopilot), and how gitopsify handles each one.

| Problem | Why it hurts | gitopsify |
|---------|--------------|-----------|
| `kubectl get -o yaml` output is full of runtime fields (`uid`, `resourceVersion`, `managedFields`, `status`, `creationTimestamp`, `last-applied-configuration`) | Manifests are noisy, and some fields (resourceVersion) make apply fail | **clean** stage strips them for every object |
| Server-side defaults (`dnsPolicy: ClusterFirst`, `terminationMessagePath`, probe timings, `revisionHistoryLimit`…) end up in Git | Huge diffs, hides intent, drifts between Kubernetes versions | Two strategies: **managed-fields** (keep only fields owned by an `Apply` field manager, which excludes defaults) and **heuristic** (curated table of Kubernetes defaults per kind). `auto` picks per object |
| Cluster-allocated values (`clusterIP`, allocated `nodePort`, PVC `volumeName`, `pv.kubernetes.io/*`, ServiceAccount token secrets) | Fail to apply in a new cluster or bind PVCs to the wrong volumes | Stripped per kind; nodePorts are kept only when a client owns them in `managedFields` |
| Controller-generated children (ReplicaSets, Pods, Endpoints, EndpointSlices, Jobs from CronJobs, cert-manager `CertificateRequest`s, operator-created StatefulSets) | Two owners fight over the object; GitOps tool prunes it | Excluded when they have a controller `ownerReference`, plus named rules for well-known generated kinds |
| System objects (`kube-system`, `system:` RBAC, `kube-root-ca.crt`, default ServiceAccount, APF objects, aggregated APIServices, addon-manager objects, cloud-managed objects) | Conflicts with what the target cluster ships | Default exclusion rules, each with a name so it can be switched off |
| Helm does not record the chart repository | `helm get values` gives values but not where the chart came from | Resolution chain: config mapping → local Helm repo caches → Artifact Hub → **vendor** the chart from the release (templates, values, files are stored in the release) → fall back to the rendered **manifests** |
| Argo CD renders charts with `helm template` | Existing `sh.helm.release.v1` secrets go stale; `helm upgrade` later fights Argo CD | Release secrets are never exported; `adopt --helm-release-secrets=backup-delete` removes them after takeover |
| Charts' `crds/` folder is not tracked in the release manifest | CRDs silently missing or orphaned | CRDs are matched to the release through the chart's `crds/` files |
| CRDs must exist before CRs; webhooks must be ready before objects they validate | First sync fails with "no matches for kind" or webhook errors | **Ordering**: phases (crds → namespaces → cluster → platform → apps → custom resources), automatic dependency inference from CRD ownership, Argo CD sync waves + `SkipDryRunOnMissingResource`, Flux `dependsOn` + `wait` |
| Large CRDs exceed the 256 KiB `last-applied-configuration` limit | `metadata.annotations: Too long` | Argo CD apps use `ServerSideApply=true`; Flux always uses SSA |
| App-of-apps sync waves do nothing since Argo CD 1.8 (no health for `Application`) | Waves are ignored, children sync in parallel | The bootstrap patches `argocd-cm` with the `argoproj.io/Application` health check |
| Argo CD label tracking collides with charts' `app.kubernetes.io/instance` | Resources flap between apps | Bootstrap sets `application.resourceTrackingMethod: annotation` (the 3.x default) |
| First sync prunes or overwrites live state | Outage during takeover | Takeover mode: automated sync **without prune**, `Prune=false`/`Delete=false` (Argo CD) or `kustomize.toolkit.fluxcd.io/prune: disabled` (Flux) on PVCs, namespaces and CRDs, `diff` before sync |
| Client-side-apply `last-applied-configuration` from `kubectl apply` | A field missing from Git is deleted on first sync (for example `replicas` → back to 1) | Takeover uses SSA, and Argo CD apps set `ClientSideApplyMigration=false`, so fields owned by other managers stay put |
| HPA-managed `replicas` | GitOps tool and HPA fight | `spec.replicas` is stripped from workloads targeted by an HPA |
| Immutable fields (Job templates, Service `clusterIP`, PVC specs, Deployment selectors) | Sync fails on adoption | Stripped where cluster-allocated; completed Jobs excluded by default; `diff` reports immutable-field conflicts |
| Secrets end up in Git | Credential leak | Strategies: `skip` (default, reported), `local` (git-ignored files), `sops`, `sealed-secrets` (sealed in-process with the target cluster's certificate), `external-secrets` (ExternalSecret per Secret) |
| Environment-specific values (hostnames, registries, storage and ingress classes, namespaces) | Copying test settings into uat | **Transforms**, global or per environment; per-environment ones become Kustomize patches or Helm values overlays |
| APIs removed in the target Kubernetes version | Apply fails | `check` verifies every exported GVK, StorageClass and IngressClass against the target |
| `CreateNamespace=true` drops namespace labels/annotations (PSA, istio-injection) | Security and mesh settings lost | Namespace objects are exported and applied in their own early phase |
| Webhook and APIService `caBundle` injected by cert-manager | Permanent drift | `caBundle` stripped when an injection annotation is present |

## 3. Architecture

```
          source cluster                                  target cluster
               │                                                ▲
   ┌───────────▼───────────┐                                    │
   │ discover              │ API discovery, list every          │
   │                       │ listable resource (paged, parallel)│
   ├───────────────────────┤                                    │
   │ helm                  │ decode release secrets, map owned  │
   │                       │ objects, resolve chart repos       │
   ├───────────────────────┤                                    │
   │ select / filter       │ scope (namespaces, selectors,      │
   │                       │ specific objects + dependencies),  │
   │                       │ default + user rules with reasons  │
   ├───────────────────────┤                                    │
   │ clean                 │ managed-fields or heuristic        │
   ├───────────────────────┤                                    │
   │ group                 │ apps: Helm releases, namespaces,   │
   │                       │ labels; crds / namespaces / cluster│
   ├───────────────────────┤                                    │
   │ transform             │ global + per-environment           │
   ├───────────────────────┤                                    │
   │ secrets               │ skip/local/plain/sops/sealed/ESO   │
   ├───────────────────────┤                                    │
   │ order                 │ phases, inferred + explicit deps,  │
   │                       │ waves                              │
   ├───────────────────────┤                                    │
   │ render                │ manifests | argocd | flux          │
   │                       │ × folder-per-env | repo-per-env    │
   └───────────┬───────────┘                                    │
               ▼                                                │
         output directory ── git push ── check / diff / adopt / bootstrap
```

The source can also be a set of files (`--from-file`: `kubectl get -A -o
yaml` dumps or manifest directories), which replaces discovery; resource
scopes then come from a built-in table and the CRDs among the objects. The
golden tests use this to run the whole pipeline without a cluster.

Every stage works on one in-memory `Inventory`, so `scan` (stop after
grouping), `generate` (all stages) and `migrate` (generate + push + bootstrap)
share the same code path. Rendering produces an in-memory file tree, which
keeps renderers pure and testable with golden files.

## 4. Maturity levels

| Level | Command | What you get |
|-------|---------|--------------|
| 0. Inventory | `gitopsify scan` | Table/JSON/YAML of apps, objects, exclusions with reasons, warnings |
| 1. Manifests | `generate --tool none` | Clean Kustomize bases and overlays, Helm apps as `helmCharts:` Kustomizations. Wire any GitOps tool to it yourself |
| 2. GitOps config | `generate --tool argocd\|flux` | Level 1 plus Applications / ApplicationSets / AppProjects or Flux Kustomizations / HelmReleases / sources, ordering, bootstrap manifests and a README |
| 3. Bootstrapped | `bootstrap` or `migrate` | Level 2 pushed to Git, Argo CD or Flux installed in the target, repository credentials and the root object applied |
| 4. Takeover | `generate --adopt`, `check`, `diff`, `adopt` | Safe sync options for objects that already exist in the target, a pre-sync diff, Helm release secret clean-up |

## 5. Output layouts

### Argo CD patterns

* **app-of-apps** (default): `argocd/<env>/root.yaml` is applied once; it
  points at `argocd/<env>/apps/`, which holds one `Application` per app with a
  `sync-wave` annotation.
* **applicationset**: one `ApplicationSet` per environment with a Git **files**
  generator over `argocd/<env>/config/<app>.json`. Helm, vendored-chart and
  Kustomize apps share it through `templatePatch`. Ordering uses the progressive-sync `RollingSync`
  strategy keyed on the `gitopsify.io/wave` label (experimental in Argo CD; the
  bootstrap enables it).
* **single**: one `Application` over a Kustomization that lists every app, for
  small clusters.

Helm apps are multi-source Applications: the chart from its repository and
`$values/apps/<app>/{base,envs/<env>}/values.yaml` from Git.

### Flux

Mirrors `flux bootstrap` and the flux2-kustomize-helm-example layout:
`clusters/<env>/flux-system/` (`GitRepository` + root `Kustomization`),
`clusters/<env>/apps/<app>.yaml` (one `Kustomization` per app with `dependsOn`
and `wait`). Helm apps are a `HelmRelease` + `HelmRepository`/`OCIRepository`
with values in a `ConfigMap` generated from `values.yaml`, and an environment
values overlay passed as a second `valuesFrom`.

### Environment layouts

* **folder-per-env** (default): `apps/<app>/base` + `apps/<app>/envs/<env>`.
* **repo-per-env**: one self-contained tree per environment under
  `<out>/<env>/`, each meant to be its own repository.

Branch-per-environment is deliberately not offered: it is the pattern the Argo
CD and Flux maintainers advise against (merge drift, no single view of
differences).

## 6. Ordering model

1. **Phase** of each app: `crds` (-30), `namespaces` (-20), `cluster` (-10),
   `platform` (-5: apps that own CRDs, admission webhooks, APIServices,
   IngressClasses or StorageClasses), `apps` (0), `custom-resources` (10).
2. **Inferred dependencies**: an app that contains custom resources depends on
   the app that owns their CRD; apps that contain SealedSecrets or
   ExternalSecrets depend on the app that runs that controller.
3. **Explicit** `ordering.waves` (app → wave, globs allowed) and
   `ordering.dependsOn` (app → apps).
4. Waves are the longest-path layering of the dependency graph, starting from
   the phase wave. Cycles are reported as errors.

Argo CD gets waves (Applications) and `SkipDryRunOnMissingResource` where a CRD
comes from another app. Flux gets `dependsOn` to every app in the closest lower
wave plus explicit dependencies, and `wait: true` on crds/platform apps.

## 7. Takeover (adoption) model

| Concern | Argo CD | Flux |
|---------|---------|------|
| Ownership marker | `argocd.argoproj.io/tracking-id` written on first sync | `kustomize.toolkit.fluxcd.io/*` labels written on first apply |
| Apply method | `ServerSideApply=true`, `ClientSideApplyMigration=false` | SSA always |
| Pruning | `automated.prune: false` until the user turns it on | `prune: true` only prunes what Flux applied (inventory), so it is safe |
| Stateful objects | `Prune=false,Delete=false` on PVCs, Namespaces, CRDs | `kustomize.toolkit.fluxcd.io/prune: disabled` |
| Helm releases | Rendered by Argo CD; old release secrets backed up and deleted by `adopt` | `HelmRelease` with the same `releaseName`, `targetNamespace` and `storageNamespace` upgrades the existing release in place |
| Verification | `gitopsify diff` server-side dry-run of every generated object against the target | same |

## 8. Secrets

| Strategy | Output | Requirement in target |
|----------|--------|-----------------------|
| `skip` (default) | Nothing; listed in `MIGRATION_REPORT.md` | Create them yourself |
| `local` | `<out>/.secrets/<ns>/<name>.yaml`, git-ignored | `kubectl apply -f .secrets/` once |
| `plain` | Secret manifests in the app (needs `--allow-plain-secrets`) | none — never for real credentials |
| `sops` | Encrypted with the `sops` binary, `.sops.yaml` written | Flux: decryption secret; Argo CD: KSOPS or helm-secrets |
| `sealed-secrets` | `SealedSecret` sealed in-process (RSA-OAEP + AES-GCM, strict scope) with the target controller's certificate | sealed-secrets controller |
| `external-secrets` | `ExternalSecret` per Secret using a configured store | External Secrets Operator + the values in the backend (`local` copies are written to help load them) |

## 9. Commands

| Command | Purpose |
|---------|---------|
| `init` | Write `gitopsify.yaml`, interactively (Charm `huh`) or from flags |
| `scan` | Inventory of the source cluster; `--show-excluded` explains every exclusion |
| `generate` | Run the pipeline and write the repository |
| `check` | Preflight against the target: API versions, StorageClasses, IngressClasses, existing owners |
| `diff` | Server-side dry-run of the generated manifests against the target |
| `adopt` | Prepare a target for takeover (Helm release secrets backup/delete) |
| `bootstrap` | Install Argo CD or Flux, configure repository access, apply the root object |
| `migrate` | `generate` → git commit/push → `check` → `bootstrap`, with confirmations |

## 10. Prior art

| Project | What it does | Gap gitopsify fills |
|---------|--------------|---------------------|
| Konveyor Crane | export → transform (JSONPatch plugins) → apply, per namespace | No Helm awareness, no GitOps tool output, no ordering |
| kube-dump, katafygio | Dump cluster YAML to Git | Backups, not deployable GitOps config; no defaults removal |
| kubectl-neat | Clean one object | Single object, no grouping or GitOps |
| `flux export` | Exports Flux objects | Only for things already in Flux |
| argocd-autopilot | Opinionated Argo CD repo bootstrap | Starts from nothing; does not import existing workloads |
| helmify | Manifests → Helm chart | Different goal (chart authoring) |

## 11. Decisions that came from testing against real clusters

| Finding | Decision |
|---------|----------|
| The Deployment controller copies `meta.helm.sh/*` annotations onto ReplicaSets | Helm ownership is only assigned to objects without a controller `ownerReference` |
| CNI/CSI/kind components install cluster-wide RBAC outside system namespaces (`kindnet`, `local-path-provisioner`) | The `system-component-rbac` rule excludes bindings whose subjects are all system service accounts, and ClusterRoles only such bindings use |
| Applying gitopsify's `argocd-cm` keys with the field manager that applied the install removed the install's labels, and Argo CD ignores unlabelled ConfigMaps | Settings layered on upstream objects use their own field manager (`gitopsify-settings`) |
| Label values must start with an alphanumeric character | Waves are encoded as `w-30`, `w0`, … in labels (sync-wave annotations keep the number) |
| One Argo CD often serves several environments generated by separate runs; un-prefixed Application names collided | Application names are always `<env>-<app>`; the default AppProject is `gitopsify-<env>` |
| The Git files generator reserves `.path` in Go templates | ApplicationSet parameters are `appName`/`appPath` |
| `yaml.v3` writes YAML 1.2, kubectl reads YAML 1.1 (`n`, `on`, `yes` become booleans) | Those words are always quoted |
| Kubernetes 1.37 bumps a Deployment's `generation` on annotation changes | A takeover shows generation bumps from the tracking annotation without a rollout; the takeover test compares pod UIDs, not generations |
| Helm test hooks appear in `helm template` output but are never installed | `diff` renders charts with `--skip-tests` and ignores hook objects |

The end-to-end test (`test/e2e/run.sh`) checks the parts that unit tests can't:
the round trip (generated manifests dry-run against the source change nothing
but deliberate Helm drift), an Argo CD migration to a fresh cluster, and a Flux
migration with Secrets sealed in-process and unsealed by the real controller.
A takeover of the source cluster by Argo CD was verified by hand: no pod was
restarted or deleted, only the drifted Helm release was corrected.

## 12. Known limitations

* The heuristic cleaner removes the defaults it knows; unusual defaults from
  newer Kubernetes versions stay in the output until the table learns them.
  `managed-fields` mode does not have this problem for objects applied with SSA
  (`kubectl apply --server-side`, Helm 4, Argo CD/Flux).
* Helm charts with subcharts cannot be vendored from the release (subcharts are
  not stored); such releases fall back to rendered manifests when the
  repository cannot be resolved.
* Helm hooks are kept in `manifests` mode as annotated objects; Argo CD maps
  them to its own hooks, Flux applies them as plain objects.
