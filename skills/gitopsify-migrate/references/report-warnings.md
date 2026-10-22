# Working through MIGRATION_REPORT.md

The report lists, in order: a summary, the Helm releases and how each is
migrated, the apps and their waves, the Secrets that need action, warnings,
and every excluded object with the rule that excluded it. Go through each
section with the person. For every warning, either change `gitopsify.yaml` and
regenerate, or write it down as a manual task. Don't leave a warning
unexplained.

## Helm releases

| Report says | Meaning | What to do |
|---|---|---|
| Mode `chart`, resolved by `config`/`local`/`artifacthub` | The chart is pulled from that repository | Check the repository is the one the team trusts (a mirror, not a random Artifact Hub match). Pin it with `helm.repositories.<chart>` |
| `chart repository unknown; the chart is vendored from the release` | The chart was rebuilt from what Helm stored in the cluster and written to `apps/<app>/charts/` | Find the real repository (`Chart.yaml` `home`/`sources`, the team's docs, `helm repo list` on their machines) and set `helm.repositories.<chart>`; vendoring is a fallback for in-house charts |
| `... has subcharts, so it can't be rebuilt; migrated as plain manifests` | No repository and the chart can't be rebuilt | Set `helm.repositories.<chart>` to keep it a Helm release; otherwise accept manifests (the release's hooks are not migrated) |
| `differs from the chart's manifest at <fields>` | Someone edited the object after `helm install` (`kubectl edit`, `kubectl scale`, `kubectl set image`) | Decide with the person: keep the edit by moving it into the values (or a `set` transform), or accept that the GitOps tool resets it |
| `values key <path> looks like a credential` | A password or token sits in the Helm values, which go to Git | Move it to a Secret the chart can reference (`existingSecret`-style values), or encrypt it. Never commit it as is |
| `no deployed revision; using revision N (failed)` or `revision N (failed) is newer than the deployed revision` | The last upgrade didn't finish | Confirm which version should run; set `helm.releases.<ns>/<name>.version` if needed |
| `skipped: installed in system namespace kube-system` | Releases in system namespaces are platform parts | Leave them, unless the person installed it (metrics-server, an ingress controller); then `helm.releases["kube-system/<name>"].include: true` |

## Object warnings

| Warning | What to do |
|---|---|
| `references <object>, which does not exist in the source cluster` | A ConfigMap, Secret, ServiceAccount or PVC a workload names is missing. It may be created at runtime by an operator, or the workload is already broken. Ask; create it or note it |
| `depends on <object>, which belongs to Helm release <x> that is not migrated` | Include that release (`-r helm/<ns>/<name>`), or make sure the target has it |
| `is pinned to node <n>`, `selects node ... by hostname`, `requires specific nodes by name` | Node names don't carry over. Replace with node labels (a `set`/`remove` transform per environment, or change the source) |
| `mounts hostPath`, `is a hostPath/local volume` | The data lives on one node. Plan a data move (Velero, `crane transfer-pvc`, application backup) or use a StorageClass in the target |
| `is a statically provisioned <type> volume` | The PV points at existing storage (NFS export, cloud disk). Either the target can reach the same storage (fine for NFS), or drop the PV (`scope.rules` exclude) and let a StorageClass provision a new volume, then restore data |
| `is created from VolumeSnapshot <x>` | The snapshot must exist in the target first, or remove the `dataSource` |
| `runs image <x> from a registry local to the source cluster` | Push the image to a registry the target can pull from, then add an `image` transform (`--image-registry localhost:5000=ghcr.io/acme`) |
| `requests load balancer IP`, `has externalIPs` | Addresses belong to the source network. Change per environment with a `set` transform, or remove them |
| `is bound to an AWS IAM role (IRSA)` / `a Google service account` / `an Azure managed identity` | Create the identity in the target account, then add an `annotations` transform per environment with its ARN/email/client ID |
| `Secret data is written unencrypted` | Only with `--secrets plain --allow-plain-secrets`. Make sure that is really intended |

## Inventory warnings

| Warning | What to do |
|---|---|
| `not allowed to list <kind>` | The source credentials can't read that kind, so none were migrated. Use broader read access, or accept it knowingly |
| `listing <kind> failed, so none are migrated` | A custom API (often a broken conversion webhook or an unavailable aggregated API) failed. Fix it in the source or accept that kind is missing |
| `API group <x> is unavailable and was skipped` | An aggregated API (metrics-server, custom metrics) is down. Usually harmless |
| `app <x> is pinned to wave N but depends on apps in a later wave` | A `--wave` contradicts an inferred dependency; gitopsify used the later wave. Fix the pin |
| `ordering.dependsOn: ... unknown app` | Typo in `ordering.dependsOn`; app names are in the Apps table |

## Secrets that need action

Each line names a Secret, its type and keys, and what was done with it
(`skip`: nothing; `local`: written to `.secrets/`; `external-secrets`: values
to load). Follow the `gitopsify-secrets` skill. Registry pull secrets
(`kubernetes.io/dockerconfigjson`) matter most: without them nothing starts.

## Excluded objects

Grouped by rule; `gitopsify rules` explains each one. Things worth a second
look with the person:

- `scope`: outside the selected namespaces or resources, as intended?
- `already-gitops-managed`: already in Git through Argo CD, Flux or Config
  Sync. gitopsify doesn't convert between tools; their source is already Git.
- `platform-crds`, `system-namespaces`: owned by the distribution or cloud;
  the target platform brings its own.
- `completed-jobs`, `bare-pods`: one-off runs. A bare Pod someone depends on
  should become a Deployment or Job first.
- `default-storageclass`: the target provides its own default. Map classes
  with `--storage-class` when names differ.

A rule can be switched off (`--disable-rule <name>`) or overridden for some
objects with `scope.rules` (first match wins). Do it for a reason, and say why
in the configuration (`reason:`).
