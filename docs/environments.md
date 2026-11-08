# Environments and transforms

One source cluster often becomes several environments: `uat` and `prod` built
from the same Git repository, each with its own hostnames, registries, storage
and sizes. gitopsify writes what they share once and what differs per
environment.

## Defining environments

```sh
gitopsify generate --tool argocd --env uat=uat-context --env prod=prod-context --repo-url ...
```

or in `gitopsify.yaml`:

```yaml
spec:
  environments:
    - name: uat
      cluster: {context: uat}
    - name: prod
      cluster:
        context: prod
        server: https://kubernetes.default.svc   # where Argo CD deploys, as Argo CD sees it
```

The `cluster` settings are how gitopsify reaches the environment (see [Cluster
access](authentication.md)) for `check`, `diff`, `adopt`, `status`,
`bootstrap`, and for fetching the Sealed Secrets certificate. `server` (or
`name`) is the Argo CD destination: the default, `https://kubernetes.default.svc`,
means Argo CD runs in the environment's own cluster. For a central Argo CD
managing several clusters, register them with Argo CD and set `cluster.name`.

Without `--env`, there is one environment called `default`.

## Layouts

**Folder per environment** (default): one repository.

```text
apps/web/base/                   shared manifests
apps/web/envs/uat/               a Kustomize overlay: the base plus uat's patches
apps/web/envs/prod/
apps/podinfo/base/values.yaml    Helm values every environment shares
apps/podinfo/envs/prod/values.yaml   only what prod changes
```

**Repository per environment** (`--layout repo-per-env`): one self-contained
tree per environment, each meant to be its own repository, with
`environments[].repoURL`. Use it when environments have different owners or
access rules.

Branches per environment aren't offered: Argo CD and Flux maintainers advise
against them (merges drift, and there's no single view of the differences).

## Transforms

Transforms change objects on their way to Git. Global `transforms` apply to
every environment and change the base; an environment's `transforms` become its
Kustomize patches or Helm values overlay.

```yaml
spec:
  transforms:
    - {type: image, from: test.azurecr.io/, to: acme.azurecr.io/}
  environments:
    - name: prod
      transforms:
        - {type: replace, from: test.acme.io, to: acme.io}
        - {type: storageClass, map: {standard: premium-ssd}}
        - {type: ingressClass, map: {nginx: alb}}
        - {type: set, target: {kinds: [Deployment], names: [api]}, path: spec.replicas, value: 3}
        - {type: set, target: {apps: [redis]}, path: values.replica.replicaCount, value: 3}
```

| Type | Fields | What it does |
|---|---|---|
| `replace` | `from`, `to`, `regex` | Replaces text in every string value (not names), in Secret data and in Helm values |
| `image` | `from`, `to` | Rewrites image prefixes in pod specs and Helm values (`registry:` keys too) |
| `storageClass` | `map` | PVCs, PVs, StatefulSet volume claim templates, Helm `storageClass`/`storageClassName` |
| `ingressClass` | `map` | `ingressClassName`, the `kubernetes.io/ingress.class` annotation, Helm `className`/`ingressClassName` |
| `namespace` | `map` | Object namespaces, Namespace names, RBAC subjects, webhook and APIService references, `*.<ns>.svc` names, Helm release namespaces |
| `set` | `path`, `value` | Sets a field: `spec.replicas`, `spec.template.spec.containers[name=web].image`, `metadata.annotations["a/b"]`, or `values.<path>` for Helm values |
| `remove` | `path` | Removes a field |
| `jsonPatch` | `patch` | RFC 6902 operations |
| `labels`, `annotations` | `set`, `remove` | Edits metadata; `remove` takes globs |

Every transform can have a `target` (`kinds`, `namespaces`, `names`,
`labelSelector`, `apps`); without one it applies everywhere. `helmValues:
false` keeps `replace` and `image` out of Helm values. The common ones have
flags too: `--replace`, `--image-registry`, `--storage-class`,
`--ingress-class`, `--namespace-map`.

When an environment transform changes an object's name or namespace, a patch
can't express it; that environment's overlay then holds full copies of the
app's objects instead.

## What usually differs

The [portability warnings](concepts.md#portability-checks) point at most of
these. A checklist:

- hostnames in Ingresses, routes, config and Helm values: `replace`;
- image registries: `image`;
- StorageClass and IngressClass names: `storageClass`, `ingressClass`;
- replica counts and resources: `set`;
- cloud identities (`eks.amazonaws.com/role-arn`,
  `iam.gke.io/gcp-service-account`, `azure.workload.identity/client-id`):
  `annotations`;
- fixed load-balancer IPs: `remove` or `set` on `spec.loadBalancerIP`.
