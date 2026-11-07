# Grafana in Crossplane

Managed resources for provider-grafana (`xpkg.upbound.io/grafana/provider-grafana:v2.17.0`), one Kustomization and namespace per workspace (instance / organization). Needs Crossplane 2.0 or later (namespaced managed resources).

## 1. Install the provider

```sh
kubectl apply -k setup/
kubectl wait provider.pkg.crossplane.io/provider-grafana --for=condition=Healthy --timeout=10m
# The provider can report Healthy before all its CRDs are served:
kubectl wait crd --all --for=condition=Established --timeout=5m
```

`setup/` holds the Provider package. Apply it first: the ClusterProviderConfig of each workspace and the managed resources use kinds that exist only once the provider runs.

## 2. Credentials

Each ClusterProviderConfig reads its credentials from a Secret in `crossplane-system`:

```sh
kubectl -n crossplane-system create secret generic grafana-default-credentials \
  --from-literal=credentials='{"url":"<url>","auth":"'"$GRAFANA_AUTH"'"}'
```

Or apply the ones gitopsify wrote: `kubectl apply -f ../.secrets/crossplane/setup/`, and the Secrets of the managed resources: `kubectl apply -R -f ../.secrets/crossplane/`.

## 3. Apply

```sh
kubectl apply --server-side -k .
kubectl get managed -A
```

With Argo CD or Flux, point one application at `setup/` and a second one, which depends on it, at this directory, and use server-side apply.

Workspaces (each with its namespace, its ClusterProviderConfig and its managed resources):

- [`default/main-org`](default/main-org)
- [`default/team-unicode`](default/team-unicode)
