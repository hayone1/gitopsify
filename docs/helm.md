# Helm releases

Apps installed with `helm install` should stay Helm charts in Git, with the
same chart, version and values, not turn into a pile of rendered YAML. That's
what gitopsify does by default.

## What gitopsify reads

Helm keeps a record of every release revision in the cluster, in a Secret
(or a ConfigMap, with `HELM_DRIVER=configmap`). gitopsify decodes the newest
deployed revision and gets:

- the chart's name and version;
- the values you set (what `helm get values` shows), not the chart's defaults;
- the rendered manifest, to know which objects belong to the release;
- the chart's `crds/` files, which Helm installs but never lists in the
  manifest.

Objects of a release move with the release: if the release is migrated, its
objects are not exported separately.

## Where the chart comes from

Helm doesn't record which repository a chart was installed from, so gitopsify
looks for it, in this order:

1. your configuration: `helm.repositories` or `--helm-repo chart=url`
   (`https://` or `oci://`);
2. your local Helm repositories (what `helm repo add` set up on this machine):
   a repository that has this chart **at this version**;
3. Artifact Hub, when you pass `--artifacthub`.

`scan` and `MIGRATION_REPORT.md` show the repository each release was resolved
to and how. Check it's the one your team trusts; pinning it in
`gitopsify.yaml` makes the result independent of whoever runs gitopsify:

```yaml
spec:
  helm:
    repositories:
      ingress-nginx: https://kubernetes.github.io/ingress-nginx
      redis: oci://registry-1.docker.io/bitnamicharts
```

## How each release is written

| `helm.mode` | Result |
|---|---|
| `auto` (default) | `chart` when the repository is known, else `vendor`, else `manifests` |
| `chart` | Argo CD: a multi-source Application, the chart from its repository and values from Git. Flux: a HelmRelease with a HelmRepository or OCIRepository. Plain: a Kustomization with `helmCharts:` and an `apply_helm` line in the apply script |
| `vendor` | The chart, rebuilt from the release, is written to `apps/<app>/charts/` and deployed from Git. Good for in-house charts nobody published |
| `manifests` | The release's objects as plain manifests; Helm is gone |

Charts with subcharts can't be rebuilt from a release (Helm doesn't store the
subcharts), so without a known repository they fall back to manifests. Hooks
aren't migrated in manifests mode.

Set the mode per release, and override chart details when needed:

```yaml
spec:
  helm:
    mode: auto
    releases:
      tools/inhouse: {mode: vendor}
      shop/legacy: {skip: true}
      kube-system/metrics-server: {include: true}   # releases in system namespaces are skipped otherwise
      data/redis: {repo: "oci://registry.acme.io/charts", version: "19.6.4"}
```

Values go to `apps/<app>/base/values.yaml`; values that differ per environment
go to `apps/<app>/envs/<env>/values.yaml` (see [transforms](environments.md)).

## Drift

People sometimes change objects of a release by hand: `kubectl scale`, `kubectl
set env`, `kubectl edit`. gitopsify compares every object of a release with
the release's manifest and reports the fields that changed:

```text
Helm release podinfo/podinfo: Deployment.apps/podinfo/podinfo differs from the
chart's manifest at spec.template.spec.containers[name=podinfod].env; the GitOps
tool will reset it to the chart's value
```

Decide for each: if the change should stay, put it into the values (or a
`set` transform on `values.<path>`); otherwise the GitOps tool resets it on the
first sync.

## Credentials in values

Values whose keys look like credentials (`password`, `token`, `apiKey`, ...)
are reported, because values go to Git. Move them into a Secret the chart
can reference (most charts have an `existingSecret` value) and migrate that
Secret with a [Secret strategy](secrets.md).

## After the migration

- **Argo CD** renders charts itself and doesn't use Helm's release records. Run
  `gitopsify adopt --helm-records backup-delete` in a takeover so a later `helm
  upgrade` can't fight Argo CD.
- **Flux** HelmReleases upgrade the existing release in place and keep its
  history; nothing to clean up.
- `gitopsify diff` renders Helm apps with `helm template` and the generated
  values when Helm is installed, so the preview matches what will be applied.
