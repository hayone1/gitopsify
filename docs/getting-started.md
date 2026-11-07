# Getting started

This guide takes one realistic case from start to finish: a `test` cluster that
people built by hand, and a new `uat` cluster that should be built from Git by
Argo CD. It takes about half an hour. Nothing changes in any cluster until step
7, and gitopsify never changes the cluster it reads from.

Swap in your own names as you go. For Flux, replace `--tool argocd` with
`--tool flux`; everything else is the same.

## 1. Install and check access

```sh
curl -fsSL https://raw.githubusercontent.com/hayone1/gitopsify/main/install.sh | sh
gitopsify doctor --context test --env uat=uat
```

`doctor` connects to both clusters and tells you how you authenticate, who the
clusters think you are, and whether you have enough permissions: read access
to everything in `test`, cluster-admin in `uat` for installing Argo CD later.
If something is red, [Cluster access](authentication.md) explains every
platform and error.

## 2. Describe the migration

You can pass everything as flags, but a small `gitopsify.yaml` is easier to
review and re-run. `gitopsify init` asks a few questions and writes it:

![gitopsify init](images/init.gif)

Or write it yourself:

```yaml
apiVersion: gitopsify.io/v1alpha1
kind: Migration
metadata:
  name: shop
spec:
  source:
    context: test
  scope:
    namespaces: [shop, payments]      # leave out to take the whole cluster
  secrets:
    strategy: sealed-secrets
  output:
    tool: argocd
    repo:
      url: https://github.com/acme/platform-gitops.git
  environments:
    - name: uat
      cluster: {context: uat}
      transforms:
        - {type: replace, from: test.acme.io, to: uat.acme.io}
```

Every command reads `gitopsify.yaml` from the current directory, and flags
override it. [Configuration](configuration.md) lists every field.

## 3. See what would be migrated

```sh
gitopsify scan --show-excluded
```

`scan` reads the cluster and shows:

- **Apps**: the units Argo CD will sync, one per namespace and one per Helm
  release by default, with their sync wave.
- **Helm releases**: the chart, version, and where the chart is pulled from.
- **Excluded objects**: everything left out, with the rule and the reason.
  Pods owned by ReplicaSets, `kube-system`, finished Jobs, the default
  ServiceAccount and so on. `gitopsify rules` explains each rule.
- **Warnings**: things to look at, such as an object someone edited after
  `helm install`, or a workload pinned to a node that won't exist in `uat`.

Adjust `gitopsify.yaml` and scan again until the result looks like your
system. [How it works](concepts.md) explains the decisions behind the output.

## 4. Generate the repository

```sh
gitopsify generate
```

This writes `./gitops`:

```text
gitops/
├── MIGRATION_REPORT.md        what happened, and what needs your attention
├── README.md                  how to use this repository
├── gitopsify.yaml             the configuration, to regenerate it
├── apps/
│   ├── shop/base/…            clean manifests
│   ├── shop/envs/uat/…        what differs in uat
│   └── podinfo/base/values.yaml   a Helm release as chart values
└── argocd/
    ├── bootstrap/             the pinned Argo CD install and its settings
    └── uat/
        ├── root.yaml          the one object you apply
        └── apps/*.yaml        one Application per app, in sync waves
```

**Read `MIGRATION_REPORT.md` now.** It lists the Secrets you still need to
create, every warning, and every excluded object. Fix what you can in
`gitopsify.yaml` and generate again; generating is cheap, and files you add to
`gitops/` by hand survive.

## 5. Push it

Create an empty repository on your Git host, then:

```sh
gitopsify generate --push
```

gitopsify pushes with your own Git credentials. Creating the repository,
protecting `main` and creating a read-only deploy key are up to you; the
`gitopsify-git-remote` [agent skill](ai-agents.md) has the commands for GitHub,
GitLab, Azure DevOps, Bitbucket and others.

## 6. Check the target

```sh
gitopsify check --env uat
gitopsify diff --env uat
```

`check` compares what will be deployed with what `uat` offers: API versions,
StorageClasses and IngressClasses, controllers for Sealed Secrets or External
Secrets, objects that already exist. Fix every error before going on.

`diff` dry-runs every object against `uat` and shows what the first sync would
create or change. On a new cluster it is mostly `+`; on a cluster that already
runs these apps it should be nearly empty.

## 7. Bootstrap

```sh
gitopsify bootstrap --env uat --ssh-key deploy-key --known-hosts known_hosts --wait
```

This installs the pinned Argo CD version in `uat`, registers your repository
with the key, applies `argocd/uat/root.yaml` and waits until every Application
is synced and healthy. For an HTTPS repository use `--git-token` instead of
`--ssh-key`. If your organization already runs Argo CD, skip `bootstrap` and
apply `root.yaml` to it yourself.

## 8. Watch it

```sh
gitopsify status --env uat
```

From now on, changes go through Git. To add `prod`, add it to `environments`,
generate again, and repeat steps 6 to 8.

## Shortcuts

- `gitopsify migrate` runs generate, push, check and bootstrap in one go.
- `gitopsify generate -i` lets you pick apps from a list.
- `--tool none` stops at clean Kustomize apps with an `apply-<env>.sh` script,
  for any other deployment tool.
- `gitopsify scan --from-file dump.yaml` works on `kubectl get -A -o yaml`
  output when you can't reach the cluster.
