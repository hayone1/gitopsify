# gitopsify.yaml recipes

Every field is optional; flags override the file. The full reference is
`docs/configuration.md` in the gitopsify repository.

## Scope

```yaml
spec:
  scope:
    namespaces: [shop, "payments-*"]        # globs
    excludeNamespaces: [legacy]
    resources: [deployment/shop/web, helm/tools/inhouse]   # one app and what it needs
    labelSelector: app.kubernetes.io/part-of=shop
    excludeKinds: [CronJob]
    disableDefaultRules: [default-storageclass]
    rules:
      - name: no-debug
        action: exclude
        kinds: [Deployment]
        names: ["debug-*"]
        reason: debugging leftovers
```

## Helm

```yaml
spec:
  helm:
    repositories:                     # chart name -> repository
      ingress-nginx: https://kubernetes.github.io/ingress-nginx
      redis: oci://registry-1.docker.io/bitnamicharts
    releases:
      kube-system/metrics-server: {include: true}
      tools/inhouse: {mode: vendor}
      shop/legacy: {skip: true}
```

## Environment differences

Global `transforms` change every environment (the base); environment
transforms become Kustomize patches or Helm values overlays.

```yaml
spec:
  transforms:
    - {type: image, from: test.azurecr.io/, to: acme.azurecr.io/}
  environments:
    - name: uat
      cluster: {context: uat}
      transforms:
        - {type: replace, from: test.acme.io, to: uat.acme.io}
    - name: prod
      cluster: {context: prod}
      transforms:
        - {type: replace, from: test.acme.io, to: acme.io}
        - {type: storageClass, map: {standard: premium-ssd}}
        - {type: ingressClass, map: {nginx: alb}}
        - {type: set, target: {kinds: [Deployment], names: [api]}, path: spec.replicas, value: 3}
        - {type: set, target: {apps: [redis]}, path: values.replica.replicaCount, value: 3}
        - type: annotations
          target: {kinds: [ServiceAccount], names: [api]}
          set: {eks.amazonaws.com/role-arn: "arn:aws:iam::222222222222:role/api-prod"}
        - {type: remove, target: {kinds: [Service], names: [lb]}, path: spec.loadBalancerIP}
```

Transform types: `replace`, `image`, `storageClass`, `ingressClass`,
`namespace`, `set`, `remove`, `jsonPatch`, `labels`, `annotations`. Paths:
`spec.template.spec.containers[name=web].image`, `metadata.annotations["a/b"]`,
`values.<path>` for Helm values.

## Grouping and ordering

```yaml
spec:
  grouping:
    by: label                          # namespace (default) | label | single
    label: app.kubernetes.io/part-of
    apps:
      - name: monitoring
        kinds: [ServiceMonitor, PrometheusRule]
  ordering:
    waves: {postgres: -2}
    dependsOn: {shop: [postgres, redis]}
```

## Output

```yaml
spec:
  output:
    dir: gitops
    tool: argocd                       # none | argocd | flux
    layout: folder-per-env             # or repo-per-env (then environments[].repoURL)
    repo: {url: git@github.com:acme/platform-gitops.git, revision: main, path: ""}
    argocd: {pattern: app-of-apps, project: gitopsify}
    flux: {interval: 10m}
```

## Cluster access

`source` and each `environments[].cluster` accept the same connection
settings. Tokens never go in this file (it is copied into the repository);
use `tokenFile`, `--token`/`--target-token` or `$GITOPSIFY_TOKEN` /
`$GITOPSIFY_TARGET_TOKEN`.

```yaml
spec:
  source:
    kubeconfig: ~/.kube/test.yaml
    context: test
  environments:
    - name: prod
      cluster:
        name: prod                     # Argo CD cluster name, for a hub Argo CD
        apiServer: https://prod.example.com:6443
        tokenFile: /var/run/secrets/prod/token
        certificateAuthority: prod-ca.pem
```

## Takeover

```yaml
spec:
  adopt:
    enabled: true                      # no pruning, protected PVC/PV/Namespace/CRD
```
