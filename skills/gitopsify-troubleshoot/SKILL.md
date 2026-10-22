---
name: gitopsify-troubleshoot
description: Diagnose and fix Argo CD Applications or Flux Kustomizations and HelmReleases that don't sync or aren't healthy after a gitopsify migration - missing CRDs, webhooks that aren't ready, absent namespaces, StorageClasses or Secrets, immutable fields, chart pull and Git authentication errors. Use when gitopsify status, bootstrap --wait or the GitOps tool's UI shows failures.
---

# Troubleshooting a migration after bootstrap

Start with what gitopsify sees, then go one level down:

`gitopsify status` names the resource that keeps an app from being healthy
(an HPA without metrics, an unbound PVC, an Ingress without an address, pods
not ready). `--wait-for synced` waits for Argo CD to apply everything without
requiring health, for clusters without an ingress controller or metrics
server; `--wait` stops early once apps stay Degraded.

```sh
gitopsify status --env uat                    # every app, with the first error
kubectl --context uat -n argocd get applications
kubectl --context uat -n argocd describe application <env>-<app>   # conditions, operation state
kubectl --context uat -n flux-system get kustomizations,helmreleases -A
kubectl --context uat -n flux-system describe kustomization <app>
kubectl --context uat get events -A --sort-by=.lastTimestamp | tail -30
```

Fix causes in `gitopsify.yaml` and regenerate when you can, rather than
editing generated files: the next `generate` would undo the edit (files the
person adds by hand are kept).

## Common failures

| Symptom | Cause | Fix |
|---|---|---|
| `no matches for kind X` / `the server could not find the requested resource` | The CRD isn't installed (yet) | If the operator is migrated, wait for its wave; Argo CD apps that need another app's CRDs get `SkipDryRunOnMissingResource`. If not migrated: install the operator, or include it (`-r helm/<ns>/<release>`) |
| `failed calling webhook ... connection refused` | The operator's webhook isn't ready | Usually resolves on retry (apps retry with backoff). If not, check the operator's pods; make sure it is in an earlier wave (`--wave operator=-5`) |
| `namespaces "x" not found` | The Namespace isn't migrated and the app doesn't create it | Include the namespace, or check that the namespaces app synced |
| `storageclass ... not found`, PVC `Pending` | Different StorageClass names | `--storage-class old=new` (per environment in `transforms`), or create the class |
| Pods `CreateContainerConfigError` / `ImagePullBackOff` | A Secret or pull secret wasn't created | The report's "Secrets that need action"; the `gitopsify-secrets` skill |
| `field is immutable` | Applying over an existing object with a different selector, Job template or PVC spec | For a new cluster: delete the conflicting object. For a takeover: make Git match the live value |
| `metadata.annotations: Too long` | Client-side apply of a large CRD | Keep `serverSideApply` on (the default) |
| Argo CD `ComparisonError ... authentication required` | Repository credentials | `gitopsify-git-remote` skill; check the `gitopsify-repo` Secret |
| Argo CD Helm app: `chart not found` / 401 from an OCI registry | Private chart repository | Register the repository with credentials in Argo CD (`argocd repo add ... --type helm [--enable-oci]`) |
| Flux HelmRelease `install retries exhausted` | The chart fails for these values | `kubectl describe helmrelease`; `helm template` the chart with `apps/<app>/.../values.yaml`; fix values or transforms |
| Flux `dependency 'flux-system/x' is not ready` | An earlier app failed | Fix that app first; dependents follow |
| App `OutOfSync` forever with no error | A controller or webhook rewrites a field Git sets | Add an ignore rule in the tool, or drop the field from Git with a `remove` transform |
| Workloads run but traffic fails | DNS, certificates, load balancer IPs, firewalls | Outside Kubernetes; check the cutover tasks in `MIGRATION_REPORT.md` |

## When to stop and ask

Deleting objects, force-syncing with replace, or turning pruning on can lose
data or cause outages. Explain what you would run and why, and wait for a yes.
