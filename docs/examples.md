# Examples

Recipes for common situations. Each one works on its own; combine them freely.
Flags can always move into `gitopsify.yaml` instead ([configuration](configuration.md)).

- [Looking around](#looking-around)
- [Choosing what to migrate](#choosing-what-to-migrate)
- [Argo CD](#argo-cd)
- [Flux](#flux)
- [Plain Kustomize, no GitOps tool](#plain-kustomize-no-gitops-tool)
- [Environments and transforms](#environments-and-transforms)
- [Helm releases](#helm-releases)
- [Secrets](#secrets)
- [Cluster access](#cluster-access)
- [Git hosts and pushing](#git-hosts-and-pushing)
- [Taking over a running cluster](#taking-over-a-running-cluster)
- [Ordering](#ordering)
- [CI and automation](#ci-and-automation)
- [Docker](#docker)
- [AI agents](#ai-agents)
- [Complete gitopsify.yaml files](#complete-gitopsifyyaml-files)

## Looking around

![scan](images/scan.gif)

```sh
gitopsify doctor                                   # can I reach everything?
gitopsify scan                                     # what would be migrated, from the current context
gitopsify scan --context test --show-excluded      # and every object left out, with the reason
gitopsify scan --show-objects                      # the objects of each app
gitopsify scan --format yaml > inventory.yaml      # for a closer look or a script
gitopsify scan --format json | jq '.apps[] | {name, wave, objects: (.objects | length)}'
gitopsify scan --format json | jq '.excludedByRule'   # how many objects each rule left out
gitopsify rules                                    # what each exclusion rule does
```

Without access to the cluster, from a dump someone sent you:

```sh
kubectl get "$(kubectl api-resources --verbs=list -o name | paste -sd, -)" -A -o yaml > dump.yaml
gitopsify scan --from-file dump.yaml
gitopsify generate --from-file dump.yaml --tool argocd --repo-url https://github.com/acme/gitops.git
cat a.yaml b.yaml | gitopsify scan --from-file -    # from stdin
gitopsify scan --from-file ./exports/               # every .yaml/.json in a directory
```

## Choosing what to migrate

```sh
gitopsify generate                                      # the whole cluster, minus system objects
gitopsify generate -n shop -n payments                  # some namespaces
gitopsify generate -n 'team-a-*'                        # namespaces by glob
gitopsify generate --exclude-namespace legacy --exclude-namespace 'tmp-*'
gitopsify generate -r deployment/shop/web               # one Deployment and everything it needs
gitopsify generate -r deployment/shop/web -r statefulset/shop/db
gitopsify generate -r helm/monitoring/kube-prometheus-stack   # one Helm release
gitopsify generate -r ingress.networking.k8s.io/shop/web      # kinds with groups
gitopsify generate -l app.kubernetes.io/part-of=shop    # by label, plus what it needs
gitopsify generate -r deployment/shop/web --no-deps     # exactly that, nothing else
gitopsify generate --kind Deployment,Service,ConfigMap  # only some kinds
gitopsify generate --exclude-kind CronJob,Job
gitopsify generate -n shop --cluster-scoped all         # also every cluster-wide object
gitopsify generate --cluster-scoped none                # namespaced objects only
gitopsify generate --exclude-app 'debug-*'              # drop whole apps after grouping
gitopsify generate -i                                   # pick apps from a list
```

![pick apps interactively](images/interactive.gif)

Rules of your own, first match wins:

```yaml
scope:
  rules:
    - {name: no-debug, action: exclude, kinds: [Deployment], names: ["debug-*"], reason: debugging leftovers}
    - {name: keep-metrics, action: include, namespaces: [kube-system], names: [metrics-server*]}
  disableDefaultRules: [default-storageclass]   # migrate the default StorageClass too
```

Grouping objects into apps differently:

```sh
gitopsify generate --group-by label --group-label app.kubernetes.io/part-of
gitopsify generate --group-by single                    # everything in one app
```

```yaml
grouping:
  apps:
    - {name: monitoring, kinds: [ServiceMonitor, PodMonitor, PrometheusRule]}
    - {name: edge, namespaces: [ingress-nginx, cert-manager]}
```

## Argo CD

![generate](images/generate.gif)

```sh
# app of apps (the default), one environment
gitopsify generate --tool argocd --env uat=uat-context --repo-url https://github.com/acme/gitops.git

# ApplicationSet, ordered with progressive syncs
gitopsify generate --tool argocd --pattern applicationset --env uat=uat --repo-url ...

# one Application for everything
gitopsify generate --tool argocd --pattern single --env uat=uat --repo-url ...

# your own AppProject, manual syncs, a pinned Argo CD version
gitopsify generate --tool argocd --project platform --no-auto-sync --argocd-version v3.5.6 --repo-url ...

# into a directory of an existing repository, on another branch
gitopsify generate --tool argocd --repo-url git@github.com:acme/platform.git \
  --repo-path clusters/uat --revision gitops -o ../platform/clusters/uat
```

Install Argo CD and let it sync, then follow along:

```sh
gitopsify bootstrap --env uat --git-token "$GITHUB_TOKEN" --wait
gitopsify bootstrap --env uat --ssh-key ./deploy-key --known-hosts known_hosts --wait
gitopsify bootstrap --env uat --wait --wait-for synced     # clusters without an ingress controller or metrics
gitopsify status --env uat
gitopsify status --env uat --format json | jq '.[] | select(.ready | not)'
```

A central Argo CD that deploys to registered clusters:

```yaml
environments:
  - name: prod
    cluster:
      name: prod-eu          # the cluster's name in Argo CD
      context: prod-eu       # how gitopsify reaches it for check/diff
```

## Flux

```sh
gitopsify generate --tool flux --env uat=uat --repo-url ssh://git@github.com/acme/gitops.git
gitopsify generate --tool flux --flux-version v2.9.6 --env uat --env prod --repo-url ...
gitopsify bootstrap --env uat --ssh-key deploy-key --known-hosts <(ssh-keyscan github.com) --wait
gitopsify bootstrap --env uat --git-username git --git-token "$TOKEN" --wait    # HTTPS
gitopsify status --env uat --wait --timeout 20m
```

`git@github.com:acme/gitops.git` works too: gitopsify writes it as an
`ssh://` URL, which Flux requires.

## Plain Kustomize, no GitOps tool

```sh
gitopsify generate                                  # --tool none is the default
./gitops/apply-default.sh --context uat --dry-run   # server-side dry run first
./gitops/apply-default.sh --context uat
./gitops/apply-uat.sh --kubeconfig ~/.kube/uat.yaml
kubectl kustomize gitops/apps/shop/envs/uat | less  # look at one app
kustomize build --enable-helm gitops/apps/podinfo/envs/uat   # a Helm app, rendered
```

## Environments and transforms

```sh
gitopsify generate --tool argocd --repo-url ... \
  --env uat=uat --env prod=prod \
  --replace test.acme.io=uat.acme.io \
  --image-registry test.azurecr.io/=acme.azurecr.io/ \
  --storage-class standard=premium-ssd \
  --ingress-class nginx=alb \
  --namespace-map shop-test=shop
```

Per environment, in `gitopsify.yaml`:

```yaml
environments:
  - name: uat
    cluster: {context: uat}
    transforms:
      - {type: replace, from: test.acme.io, to: uat.acme.io}
  - name: prod
    cluster: {context: prod}
    transforms:
      - {type: replace, from: test.acme.io, to: acme.io}
      - {type: replace, from: '(\w+)\.test\.svc', to: '$1.prod.svc', regex: true}
      - {type: set, target: {kinds: [Deployment], names: [api, web]}, path: spec.replicas, value: 3}
      - {type: set, target: {kinds: [Deployment], names: [api]}, path: 'spec.template.spec.containers[name=api].resources.requests.cpu', value: 500m}
      - {type: set, target: {apps: [redis]}, path: values.replica.replicaCount, value: 3}
      - {type: remove, target: {kinds: [Service], names: [lb]}, path: spec.loadBalancerIP}
      - {type: labels, set: {env: prod}, remove: ["test-*"]}
      - {type: annotations, target: {kinds: [ServiceAccount], names: [api]}, set: {eks.amazonaws.com/role-arn: "arn:aws:iam::222222222222:role/api"}}
      - type: jsonPatch
        target: {kinds: [Ingress], names: [web]}
        patch:
          - {op: add, path: /metadata/annotations/nginx.ingress.kubernetes.io~1proxy-body-size, value: 50m}
```

One repository per environment:

```yaml
output:
  tool: flux
  layout: repo-per-env
environments:
  - {name: uat, repoURL: https://github.com/acme/gitops-uat.git, cluster: {context: uat}}
  - {name: prod, repoURL: https://github.com/acme/gitops-prod.git, cluster: {context: prod}}
```

## Helm releases

```sh
gitopsify scan --format json | jq '.helmReleases[] | {release, chart, version, mode, repository}'
gitopsify generate --helm-repo ingress-nginx=https://kubernetes.github.io/ingress-nginx
gitopsify generate --helm-repo redis=oci://registry-1.docker.io/bitnamicharts
gitopsify generate --artifacthub                    # look up unknown charts on artifacthub.io
gitopsify generate --helm-mode manifests            # no Helm in Git, rendered objects only
gitopsify generate --helm-mode vendor               # charts copied into the repository
```

```yaml
helm:
  repositories:
    cert-manager: https://charts.jetstack.io
  releases:
    kube-system/metrics-server: {include: true}     # migrate a release from a system namespace
    tools/inhouse: {mode: vendor}
    shop/legacy: {skip: true}
    data/redis: {repo: oci://registry.acme.io/charts, version: 19.6.4}
```

## Secrets

```sh
gitopsify generate --secrets local                  # plain copies in git-ignored .secrets/
kubectl --context uat apply --server-side -f gitops/.secrets/shop/

gitopsify generate --secrets sealed-secrets --env uat=uat                 # sealed with uat's controller key
gitopsify generate --secrets sealed-secrets --sealed-secrets-cert uat.pem # with a fetched certificate

age-keygen -o age.key
gitopsify generate --tool flux --secrets sops --sops-age "$(age-keygen -y age.key)"
gitopsify bootstrap --env uat --sops-age-key age.key

gitopsify generate --secrets external-secrets --eso-store vault --eso-store-kind ClusterSecretStore
```

```yaml
secrets:
  strategy: sealed-secrets
  overrides:
    - {namespaces: [monitoring], strategy: local}
    - {names: ["*-tls"], strategy: skip}
  externalSecrets:
    keyTemplate: "{{ .Namespace }}-{{ .Name }}"     # stores that don't allow "/"
```

## Cluster access

```sh
gitopsify scan --context test
gitopsify scan --kubeconfig ~/.kube/test.yaml:~/.kube/uat.yaml --context test
gitopsify scan --server https://10.0.0.10:6443 --token-file reader.token --certificate-authority ca.pem
GITOPSIFY_TOKEN=$(kubectl -n kube-system create token gitopsify-reader) gitopsify scan --server https://test:6443 --insecure-skip-tls-verify
gitopsify scan --server https://203.0.113.10:6443 --tls-server-name kubernetes --certificate-authority ca.pem
gitopsify scan --client-certificate me.crt --client-key me.key --server https://test:6443 --certificate-authority ca.pem
gitopsify scan --as jane --as-group platform        # impersonation
gitopsify scan --proxy-url socks5://localhost:1080  # through an SSH tunnel: ssh -D 1080 bastion
gitopsify scan --in-cluster                         # from a pod
gitopsify check --env prod --target-kubeconfig ~/.kube/prod.yaml --target-context admin
gitopsify diff --env prod --target-server https://prod:6443 --target-token-file prod.token
```

Per platform:

```sh
aws eks update-kubeconfig --name test --region eu-west-1 && gitopsify scan --context arn:aws:eks:eu-west-1:111111111111:cluster/test
gcloud container clusters get-credentials test --region europe-west1 && gitopsify scan
az aks get-credentials -g rg -n test && kubelogin convert-kubeconfig -l azurecli && gitopsify scan
oc login https://api.test.example.com:6443 && gitopsify scan
tsh kube login test && gitopsify scan
```

## Git hosts and pushing

```sh
gitopsify generate --repo-url https://github.com/acme/gitops.git --push
gitopsify generate --repo-url https://github.com/acme/gitops.git --commit --message "Export test, take 2"
gh repo create acme/gitops --private && gitopsify generate --tool argocd --repo-url https://github.com/acme/gitops.git --push
glab repo create acme/gitops --private && gitopsify generate --repo-url git@gitlab.com:acme/gitops.git --push
az repos create --name gitops && gitopsify generate --repo-url https://dev.azure.com/acme/Platform/_git/gitops --push
```

Creating deploy keys, tokens and protected branches on each host is covered by
the `gitopsify-git-remote` [agent skill](ai-agents.md).

## Taking over a running cluster

```sh
gitopsify generate --tool argocd --adopt --env prod=prod --repo-url ... --push
gitopsify diff --env prod                         # should show only what you expect
gitopsify diff --env prod --format json | jq -r '.[] | select(.action == "update") | .diff'
gitopsify adopt --env prod --dry-run
gitopsify adopt --env prod --helm-records backup-delete
gitopsify bootstrap --env prod --wait
```

## Ordering

```sh
gitopsify scan --format json | jq -r '.apps[] | "\(.wave)\t\(.name)\t\(.dependsOn // [] | join(","))"' | sort -n
gitopsify generate --wave postgres=-3 --wave 'redis*'=-3
gitopsify generate --depends-on shop=postgres,redis
gitopsify generate --no-order                     # no waves, no dependsOn
```

## CI and automation

```sh
gitopsify doctor --no-input --format json | jq '.[] | select(.status != "ok")'
gitopsify diff --no-input --env prod --exit-code    # exit 2 when the cluster drifted from Git
gitopsify migrate --no-input --yes --context test --env uat=uat --tool argocd \
  --repo-url https://github.com/acme/gitops.git --push --git-token "$TOKEN" --wait
gitopsify status --no-input --env uat --wait --timeout 15m
```

A nightly drift report with GitHub Actions:

```yaml
on:
  schedule: [{cron: "0 3 * * *"}]
jobs:
  drift:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: curl -fsSL https://raw.githubusercontent.com/hayone1/gitopsify/main/install.sh | sh
      - run: gitopsify diff --no-input --env prod --exit-code
        env:
          GITOPSIFY_TOKEN: ${{ secrets.SOURCE_TOKEN }}
          GITOPSIFY_TARGET_TOKEN: ${{ secrets.PROD_TOKEN }}
```

## Docker

```sh
docker run --rm -v ~/.kube:/home/gitopsify/.kube:ro hayone/gitopsify scan
docker run --rm -v ~/.kube:/home/gitopsify/.kube:ro -v "$PWD:/work" hayone/gitopsify generate --tool flux --repo-url ...
docker run --rm -v "$PWD:/work" hayone/gitopsify scan --from-file dump.yaml
```

The image includes `git`, `ssh`, `helm` and `kubectl`. Credential plugins
(`aws`, `gke-gcloud-auth-plugin`, `kubelogin`) aren't in it: use a token or a
kubeconfig with embedded credentials, or build on top of the image.

## AI agents

```sh
gitopsify skills install                          # Codex, Gemini CLI, Cursor, Copilot, Claude Code
gitopsify skills install --agent claude --project # share them with your team through Git
gitopsify skills show gitopsify-git-remote
```

Then, in your agent:

> Move the shop namespace from the test cluster to uat with Flux. Our code is on GitLab in the platform group.

## Complete gitopsify.yaml files

Whole-cluster rebuild with Argo CD and Sealed Secrets:

```yaml
apiVersion: gitopsify.io/v1alpha1
kind: Migration
source: {context: prod-old}
scope:
  excludeNamespaces: [sandbox-*]
secrets: {strategy: sealed-secrets}
helm:
  resolvers: [local, artifacthub]
output:
  tool: argocd
  repo: {url: git@github.com:acme/prod-gitops.git}
environments:
  - name: prod
    cluster: {context: prod-new}
```

Two environments with Flux, SOPS and different sizes:

```yaml
apiVersion: gitopsify.io/v1alpha1
kind: Migration
source: {context: test}
scope: {namespaces: [shop, payments]}
secrets:
  strategy: sops
  sops: {age: [age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqs0x8eh7]}
transforms:
  - {type: image, from: registry.test.acme.io/, to: registry.acme.io/}
output:
  tool: flux
  repo: {url: ssh://git@github.com/acme/gitops.git}
environments:
  - name: uat
    cluster: {context: uat}
    transforms:
      - {type: replace, from: test.acme.io, to: uat.acme.io}
  - name: prod
    cluster: {context: prod}
    transforms:
      - {type: replace, from: test.acme.io, to: acme.io}
      - {type: set, target: {kinds: [Deployment]}, path: spec.replicas, value: 3}
```

One app as plain Kustomize, for a team that deploys with its own pipeline:

```yaml
apiVersion: gitopsify.io/v1alpha1
kind: Migration
source: {context: test}
scope:
  resources: [deployment/shop/web]
secrets: {strategy: local}
output: {dir: web-manifests}
```

Takeover of the same cluster by Argo CD:

```yaml
apiVersion: gitopsify.io/v1alpha1
kind: Migration
source: {context: prod}
adopt: {enabled: true}
output:
  tool: argocd
  repo: {url: https://github.com/acme/prod-gitops.git}
environments:
  - name: prod
    cluster: {context: prod}
```
