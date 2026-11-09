# gitopsify

[gitopsify](https://github.com/hayone1/gitopsify) turns what runs in a
Kubernetes cluster into a GitOps repository for Argo CD, Flux or plain
Kustomize. It can also turn other tools' live state (Grafana, ...) into
Terraform or Crossplane. This chart runs its web UI and API for a team:

- single sign-on (OpenID Connect), with viewer, operator and admin roles per
  cluster and tool;
- scans, migrations and exports that run in the background, with live logs;
- pushes to Git, an audit log, Prometheus metrics and OpenTelemetry;
- one replica on a volume, or several on PostgreSQL.

It reads clusters and writes Git repositories. It doesn't change the
clusters it reads, and it isn't an operator: it defines no custom resources.

## Install

```sh
helm install gitopsify oci://ghcr.io/hayone1/charts/gitopsify \
  --namespace gitopsify --create-namespace -f values.yaml
```

A minimal `values.yaml`:

```yaml
server:
  externalURL: https://gitopsify.example.com
  auth:
    oidc:
      issuer: https://keycloak.example.com/realms/acme
      clientID: gitopsify
  rbac:
    bindings:
      - role: admin
        groups: [platform-team]
      - role: operator
        groups: [shop-team]
        clusters: [in-cluster, staging-*]
secrets:
  oidcClientSecret: "..."          # or secrets.existingSecret
ingress:
  enabled: true
  className: nginx
  host: gitopsify.example.com
  tls: [{secretName: gitopsify-tls, hosts: [gitopsify.example.com]}]
```

Register `https://gitopsify.example.com/api/v1/auth/callback` as the
client's redirect URL at your identity provider.

## What it runs

- **Pod security:** one replica, non-root, with a read-only root file
  system, no capabilities and the RuntimeDefault seccomp profile.
- **Its own cluster:** available as `in-cluster`. Its ServiceAccount may
  list and watch every resource and write nothing (`rbac.clusterReader`).
  With `inCluster.impersonate`, it reads as the signed-in person instead.
- **Storage:** a PersistentVolumeClaim keeps runs and the audit log. With
  `storage.type: postgres`, `replicaCount` can be above 1.

## Main values

| Value | Default | What it does |
|---|---|---|
| `server.externalURL` | `""` | The address people reach gitopsify at (required with OIDC) |
| `server.auth.oidc.issuer`, `.clientID` | `""`, `gitopsify` | The identity provider |
| `server.rbac.bindings` | `[]` | Roles by group or user, per cluster and tool |
| `server.clusters` | `[]` | Other clusters, with credentials mounted through `extraVolumes` |
| `server.git.repositories`, `.credentials` | `[]` | The repositories runs may push to, and the tokens for them |
| `server.trustedProxies` | `[]` | The ingress controller's addresses, so the audit log sees clients |
| `secrets.sessionKey`, `.oidcClientSecret`, `.existingSecret` | generated, `""`, `""` | Session key and OIDC client secret (set the key with Argo CD or Flux) |
| `storage.type` | `file` | `file` (one replica) or `postgres` (high availability) |
| `storage.postgres.existingSecret`, `.url` | `""` | The database connection string |
| `replicaCount` | `1` | More than one needs `storage.type: postgres` |
| `inCluster.enabled`, `.impersonate` | `true`, `false` | This cluster as a source, read as the ServiceAccount or as each person |
| `ingress.enabled` | `false` | An Ingress for `ingress.host` |
| `metrics.serviceMonitor.enabled` | `false` | A Prometheus Operator ServiceMonitor |
| `opentelemetry.endpoint` | `""` | An OTLP/HTTP collector for traces, metrics and logs |
| `networkPolicy.enabled` | `false` | Ingress only from `networkPolicy.ingressNamespaces` |
| `persistence.size` | `10Gi` | The data volume |

`values.yaml` documents every value.

## Documentation

- [Web UI and server guide](https://github.com/hayone1/gitopsify/blob/main/docs/web-ui.md): configuration, roles, clusters, security, high availability
- [Observability](https://github.com/hayone1/gitopsify/blob/main/docs/observability.md)
- [gitopsify](https://github.com/hayone1/gitopsify#readme): the CLI and what it generates
