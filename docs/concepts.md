# How gitopsify works

gitopsify reads every object in a cluster, decides which ones belong in Git and
in what form, and writes them out for a GitOps tool. This page explains those
decisions, so the output doesn't surprise you.

```text
read ─► Helm releases ─► what to migrate ─► what it needs ─► clean ─► check portability
     ─► group into apps ─► transform ─► Secrets ─► order ─► write
```

## Reading the cluster

gitopsify asks the API server which kinds exist and lists every object of
every kind, in parallel, with paging. Kinds that are never configuration
(Events, metrics) aren't even listed. If a custom API is broken, for example a
CRD whose conversion webhook is down, that kind is skipped with a warning
instead of failing the whole run; transient errors are retried.

It only reads. Without cluster access, `--from-file` reads `kubectl get -A -o
yaml` output instead.

## Cleaning

What `kubectl get -o yaml` prints isn't what anyone wrote. gitopsify removes:

- runtime metadata: `uid`, `resourceVersion`, `managedFields`, `status`,
  `creationTimestamp`, `last-applied-configuration`, revision annotations;
- values the cluster allocated: Service `clusterIP`s and allocated
  `nodePort`s, PVC `volumeName`, injected `caBundle`s, the replica count of
  workloads an HPA scales, OpenShift's per-namespace UID ranges;
- defaults the API server filled in: `dnsPolicy: ClusterFirst`, probe timings,
  `terminationMessagePath`, `revisionHistoryLimit` and many more.

Defaults are removed in one of two ways, chosen per object (`clean.strategy:
auto`). When everything that wrote the object used server-side apply, its
`managedFields` say exactly which fields a person set, and gitopsify keeps only
those. Otherwise a curated table of Kubernetes defaults is removed. You can
force either with `--clean managed-fields` or `--clean heuristic`.

## What is left out, and why

Plenty of objects in a cluster shouldn't be in Git. Each built-in rule has a
name, a reason in the report, and can be switched off with `--disable-rule`:

| Rule | Leaves out |
|---|---|
| `system-namespaces` | `kube-system` and the namespaces of distributions and cloud platforms (GKE, AKS, OpenShift, Rancher, CNIs) |
| `gitops-namespaces` | The source cluster's own Argo CD and Flux |
| `owned-by-controller`, `generated-kinds` | ReplicaSets, Pods, Jobs of CronJobs, Leases, operator-created objects |
| `platform-crds` | CRDs of the distribution, cloud provider or CNI (k3s, EKS, GKE, Cilium, Calico, ...) |
| `helm-release-storage` | Helm's release records; releases are migrated as charts |
| `completed-jobs`, `bare-pods`, `terminating` | One-off runs and objects being deleted |
| `system-rbac`, `system-component-rbac` | Kubernetes' and platform components' RBAC |
| `addon-managed`, `system-served-registrations` | Objects the platform's addon manager applies (GKE, EKS, AKS, k3s add-ons), and APIServices and webhooks whose server runs in a system namespace |
| `default-serviceaccount`, `platform-serviceaccounts`, `legacy-token-secrets` | Accounts and tokens every cluster creates itself |
| `cert-manager-secrets` | TLS Secrets cert-manager will issue again |
| `helm-hooks` | Objects left by Helm hooks (`helm.sh/hook`); the chart runs them again |
| `already-gitops-managed` | Objects Argo CD, Flux or Config Sync already manage |

`gitopsify rules` lists all of them. `scope.rules` adds your own include and
exclude rules, which take precedence.

## Taking what an app needs

Select one app (`-r deployment/shop/web`), a label (`-l team=shop`) or
namespaces (`-n shop`), and gitopsify follows references: the ConfigMaps,
Secrets and ServiceAccount a Deployment uses, their Roles and bindings, PVCs
and StorageClasses, the Services that select its pods, Ingresses and Gateway
routes to those Services, HPAs, PDBs, NetworkPolicies, ServiceMonitors, the
Namespace, and the CRDs of custom resources. `--no-deps` turns this off.

## Portability checks

Some objects work in the source cluster only. gitopsify warns about workloads
pinned to node names, `hostPath` and local volumes, images from a registry only
the source can reach, statically provisioned PVs, fixed load-balancer IPs and
cloud workload identities (IRSA, GKE and Azure Workload Identity). Each warning
says what to change, usually with a [transform](environments.md). It also
warns about HPAs that scale on CPU or memory utilization of containers that
request none: they can't scale anywhere, and Argo CD reports them Degraded.

## Apps

Objects are grouped into **apps**. Each app becomes one Argo CD Application or
one Flux Kustomization:

- one app per Helm release;
- one app per namespace for everything else (or per label value with
  `--group-by label`, or one app for all with `--group-by single`);
- `crds`, `namespaces` (the Namespace objects themselves, so labels such as Pod
  Security levels are kept) and `cluster` (other cluster-wide objects).

`grouping.apps` puts objects you choose into an app of your own.

## Order

A GitOps tool applying everything at once fails: custom resources before their
CRDs, webhooks that aren't running yet. Every app gets a phase and a wave:

| Phase | Wave | Apps |
|---|---|---|
| `crds` | -30 | CustomResourceDefinitions |
| `namespaces` | -20 | Namespaces |
| `cluster` | -10 | ClusterRoles, StorageClasses, ... |
| `platform` | -5 | Apps that install CRDs, webhooks, APIServices, IngressClasses, StorageClasses |
| `apps` | 0 | Everything else |
| `custom-resources` | 10 | Apps that only hold custom resources |

An app that contains custom resources also depends on the app that installs
their CRD, including the SealedSecrets and ExternalSecrets gitopsify writes in
place of Secrets. `--wave app=N` and `--depends-on app=a,b` add your own
constraints. Argo CD gets sync waves, Flux gets `dependsOn` and `wait`.

## Output

| `--tool` | You get |
|---|---|
| `none` | Kustomize apps (base and per-environment overlays), Helm values, and an `apply-<env>.sh` script that applies everything in order |
| `argocd` | The same, plus Applications (app of apps, ApplicationSet or a single Application), an AppProject and a pinned Argo CD install |
| `flux` | The same, plus Flux Kustomizations and HelmReleases with their sources, and a pinned Flux install |

Every output has a `MIGRATION_REPORT.md` and the `gitopsify.yaml` that
produced it. See [Argo CD, Flux or plain Kustomize](gitops-tools.md).

## Regenerating into an existing repository

gitopsify records, in `.gitopsify/`, the files it wrote and their checksums.
On every run it:

- rewrites the files it generated and removes those it no longer produces
  (deleting an object from the cluster deletes its file);
- leaves files you added alone;
- **hand edits** to files it generated are overwritten with a warning that
  lists them: `--edited keep` keeps them (and warns each time), `--edited
  fail` stops the run before writing anything;
- **a file of yours where it writes** stops the run: `--unmanaged overwrite`
  (or `--force`) replaces it, `--unmanaged keep` leaves it;
- **`--keep` paths** (globs: `apps/web/envs/prod`, `**/values.yaml`; `*`
  stays within a directory, `**/` is any directories) are never written or
  removed;
- an environment the run doesn't list stops it (`--force` removes it);
- an output made from another cluster stops it (gitopsify records the
  source's identity, the UID of its `kube-system` namespace; `--force`
  replaces it).

These are `spec.output.merge` in gitopsify.yaml. `--dry-run` shows the plan:
`+` new, `~` changed, `-` removed, `=` kept. The `gitopsify.yaml` the output
keeps is the next run's configuration (`gitopsify generate -c
gitops/gitopsify.yaml` writes into `gitops/`): edit it freely. It leaves out
what belongs to one machine: kubeconfig paths, tokens, `--in-cluster` and the
output directory. Into another repository, use `-o`
with that checkout and `--repo-path` for the directory it lives in. A run
that keeps a file gets nothing new into it: keep your own changes in files of
your own (an extra patch in an overlay, say) where you can, so the generated
ones stay current.

Sealed and SOPS-encrypted Secrets keep their ciphertext while the source
Secret doesn't change (gitopsify records a fingerprint of the Secret's
identity, resourceVersion and settings, never its values), so a regeneration
of an unchanged cluster changes nothing in Git. Secrets read from files
(`--from-file`) are encrypted afresh each time, and so is every sealed Secret
after the controller's key changes (every 30 days by default).

## Objects that already exist in the target

By default every object is generated and the first sync takes over the ones
already in the environment's cluster (`check` lists them, with who manages
them). `--existing` (`spec.adopt.existing`) reads each environment's cluster
while generating and leaves objects out of that environment instead:

| `--existing` | Left out of the environment |
|---|---|
| `include` (default) | nothing |
| `skip-owned` | objects another tool manages there: an Argo CD application, Flux, a Helm release, kapp, Config Sync, or an operator (a controller owner reference). A Secret already there counts for the SealedSecret or ExternalSecret that would make it |
| `skip` | every object that exists there, so Git only adds what is missing (`--tool argocd` or `flux`) |

Objects this migration's own Argo CD or Flux apps manage are never left out,
so regenerating after the first sync keeps them. The overlay of an
environment drops a left-out object with a delete patch; other environments
keep it. Objects a Helm chart renders can't be left out one by one: the report
lists them. `check` and `diff` apply the same rule.

## Maturity levels

You can stop at any level:

1. **Inventory**: `scan`, to know what isn't in Git yet.
2. **Manifests**: `generate --tool none`, clean files for any tool.
3. **GitOps configuration**: `generate --tool argocd|flux`.
4. **Bootstrapped**: `bootstrap` or `migrate`, the tool installed and syncing.
5. **Takeover**: `--adopt` for the cluster you exported from ([takeover](takeover.md)).
