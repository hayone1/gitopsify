# Argo CD, Flux or plain Kustomize

`--tool` decides how far gitopsify takes you. All three write the same clean
apps under `apps/`; they differ in what drives them.

## Plain Kustomize (`--tool none`)

For any deployment tool, or for a first rollout by hand. Each app is a
Kustomization (`apps/<app>/envs/<env>`), Helm apps carry their values and a
`helmCharts:` entry, and `apply-<env>.sh` applies everything in order:

```sh
./gitops/apply-uat.sh --context uat --dry-run   # server-side dry run first
./gitops/apply-uat.sh --context uat
```

The script server-side applies manifests with `kubectl`, installs Helm apps
with `helm upgrade --install` (Helm 3 or 4), waits for CRDs and for operators
and webhooks to be available before the apps that need them, and passes
`--context`/`--kubeconfig` to both tools. A dry run on an empty cluster reports
the waves that need earlier ones and carries on.

## Argo CD (`--tool argocd`)

```text
argocd/bootstrap/        the pinned Argo CD install, plus the settings the apps rely on
argocd/<env>/root.yaml   the one Application you apply
argocd/<env>/apps/       what root.yaml syncs: an AppProject and the apps
```

Three patterns (`--pattern`):

| Pattern | Generates | Good for |
|---|---|---|
| `app-of-apps` (default) | One Application per app, ordered with sync waves | Most migrations |
| `applicationset` | One ApplicationSet per environment over `argocd/<env>/config/*.json`, ordered with progressive syncs | Many similar apps, central control |
| `single` | One Application over every app | Small clusters |

Helm apps are multi-source Applications: the chart from its repository, values
from Git. Applications are named `<env>-<app>` so one Argo CD can serve several
environments; the AppProject is `gitopsify-<env>` unless you set `--project`.
Sync uses server-side apply, automated sync with self-heal, retries with
backoff, and `CreateNamespace` only where the Namespace isn't migrated.

The bootstrap settings matter: Argo CD dropped health checks for
`Application` objects in 1.8, which makes app-of-apps sync waves meaningless,
so gitopsify restores that health check; it also selects annotation-based
resource tracking, so charts' `app.kubernetes.io/instance` labels don't
confuse Argo CD.

## Flux (`--tool flux`)

```text
clusters/<env>/flux-system/   the pinned Flux install, GitRepository and root Kustomization
clusters/<env>/apps/          one Kustomization per app, with dependsOn
apps/<app>/base/              for Helm apps: HelmRelease, HelmRepository/OCIRepository, values
```

Ordering becomes `dependsOn` (an app depends on its inferred dependencies and
on the apps of the previous wave) with `wait: true` on apps others depend on.
Helm values are stored in ConfigMaps generated from `values.yaml`, plus an
optional per-environment overlay.

## Bootstrapping

```sh
gitopsify bootstrap --env uat --git-token "$TOKEN" --wait              # HTTPS
gitopsify bootstrap --env uat --ssh-key deploy --known-hosts known_hosts --wait   # SSH
```

`bootstrap` installs the pinned version (`--argocd-version`,
`--flux-version`), applies gitopsify's settings next to the upstream install
without taking over its fields, stores the repository credentials, applies the
root object and, with `--wait`, waits until every app is synced and healthy.
It doesn't need the source cluster. `--known-hosts` is required for Flux over
SSH; Argo CD already knows GitHub, GitLab, Bitbucket and Azure DevOps and gets
any other host from `--known-hosts`.

Already running Argo CD or Flux? Don't bootstrap a second one. Add the
repository to your installation and apply `argocd/<env>/root.yaml`, or the
`clusters/<env>/flux-system/gotk-sync.yaml` objects, yourself.

## Status

```sh
gitopsify status --env uat
gitopsify status --env uat --wait --timeout 15m   # in CI
```

`status` lists every Application, or every Kustomization and HelmRelease,
that the generated repository declares, with its sync state, health and the
first error, and reports the ones that don't exist yet as Missing. For an
unhealthy Argo CD app it names the resource that holds it back: an HPA without
metrics, an unbound PVC, an Ingress without an address, pods that aren't
ready.

`--wait` (on `status` and `bootstrap`) waits for every app to be synced and
healthy, and stops early when everything is synced but some apps stay
Degraded. On clusters where some resources can't become healthy, for example
without an ingress controller, `--wait-for synced` waits for Argo CD to apply
everything and reports health without waiting for it.

## Regenerating

Run `generate` again whenever the source changed or you changed
`gitopsify.yaml`. Files gitopsify generated before and no longer produces are
removed; files you added are kept. Commit the result like any other change,
ideally through a pull request.
