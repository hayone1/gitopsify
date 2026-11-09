# gitopsify.yaml reference

Every field is optional; the values shown are the defaults unless the comment
says otherwise. Flags override the file, and unknown fields are errors, so a
typo can't silently fall back to a default. `gitopsify generate` writes the
effective configuration, with every default filled in, to
`<output>/gitopsify.yaml`; `gitopsify generate -c <output>/gitopsify.yaml`
regenerates the same repository.

The file is a resource in the Kubernetes style: `apiVersion`, `kind:
Migration`, `metadata` and the fields below under `spec`. It can be YAML, JSON
or HCL (attributes, as in the JSON form): `gitopsify init -f gitopsify.json` or
`-f gitopsify.hcl` writes those, and other commands take them with `-c`. One
file can hold the migration and the provider exports
([providers.md](providers.md)) as several resources: YAML documents separated
by `---`, a JSON array or `kind: List`, or `items = [...]` in HCL.

Editors with the YAML language server (VS Code's YAML extension, JetBrains,
Neovim) complete and check the file from its JSON Schema,
[gitopsify.schema.json](gitopsify.schema.json); the files gitopsify writes
point at it in their first line. Every command also validates the file and
names the field that's wrong.

Files written by gitopsify v0.1 have the fields at the top, next to
`apiVersion` and `kind`, without `metadata` and `spec`. They are still read
and validated as they are. To move one to the resource form, indent the fields
under `spec:` and add `metadata: {name: ...}`; `gitopsify generate` already
writes its copy that way.

```yaml
apiVersion: gitopsify.io/v1alpha1
kind: Migration
metadata:
  name: shop                # lowercase letters, digits, - and .; gitopsify init derives it
                            # from the source context ("migration" without one)
  labels: {}
  annotations: {}
spec:
  source:                   # how to reach the source cluster; see docs/authentication.md
    kubeconfig: ""          # default: KUBECONFIG or ~/.kube/config; a:b merges files (--kubeconfig)
    context: ""             # default: the current context              (--context)
    inCluster: false        # the ServiceAccount of the pod gitopsify runs in (--in-cluster)
    apiServer: ""           # API server URL; alone, the kubeconfig is ignored (--server)
    tokenFile: ""           # bearer token file; tokens themselves never go here (--token-file;
                            # --token or $GITOPSIFY_TOKEN for the value)
    clientCertificate: ""   #                                            (--client-certificate)
    clientKey: ""           #                                            (--client-key)
    certificateAuthority: ""  #                                          (--certificate-authority)
    insecureSkipTLSVerify: false  #                                      (--insecure-skip-tls-verify)
    tlsServerName: ""       #                                            (--tls-server-name)
    proxyURL: ""            # http(s):// or socks5://                    (--proxy-url)
    impersonate: ""         #                                            (--as)
    impersonateGroups: []   #                                            (--as-group)
    impersonateUID: ""      #                                            (--as-uid)
    timeout: ""             # per request, e.g. 30s                      (--request-timeout)

  scope:
    namespaces: []          # globs; empty = every namespace            (-n)
    excludeNamespaces: []   # globs                                     (--exclude-namespace)
    labelSelector: ""       # select objects by label, plus what they need (-l)
    resources: []           # kind[.group]/[namespace/]name or helm/<ns>/<release> (-r)
    includeDependencies: true   # with resources/labelSelector         (--no-deps)
    kinds: []               # only these kinds: Deployment, deploy, ingress.networking.k8s.io (--kind)
    excludeKinds: []        #                                           (--exclude-kind)
    clusterScoped: auto     # auto | all | none. auto: all for a whole-cluster
                            # migration, only referenced ones otherwise (--cluster-scoped)
    disableDefaultRules: [] # rule names from `gitopsify rules`, or "*" (--disable-rule)
    includeAlreadyManaged: false  # objects Argo CD or Flux already manage (--include-managed)
    excludeApps: []         # drop whole apps (globs) after grouping    (--exclude-app, generate -i)
    rules:                  # first match wins; they override the default rules
      - name: no-debug
        action: exclude     # include | exclude
        reason: debugging leftovers
        kinds: [Deployment]
        namespaces: ["*"]
        names: ["debug-*"]
        labelSelector: ""

  helm:
    mode: auto              # auto | chart | vendor | manifests       (--helm-mode)
    resolvers: [local]      # local (Helm's repository cache), artifacthub (--artifacthub)
    repositories:           # chart name -> repository             (--helm-repo chart=url)
      ingress-nginx: https://kubernetes.github.io/ingress-nginx
      redis: oci://registry-1.docker.io/bitnamicharts
    releases:               # per release, keyed <namespace>/<name>
      kube-system/metrics-server:
        include: true       # migrate even though it's in a system namespace
      shop/legacy:
        skip: true
      tools/inhouse:
        mode: vendor
        repo: ""            # override the repository
        chart: ""           # override the chart name
        version: ""         # override the version

  clean:
    strategy: auto          # auto | managed-fields | heuristic | minimal (--clean)
                            # auto: managed-fields when every client field manager
                            # used server-side apply, heuristic otherwise
    controllerManagers: []  # extra field managers to treat as controllers
    stripAnnotations: []    # globs, removed everywhere
    stripLabels: []         # globs, removed from object metadata

  grouping:
    by: namespace           # namespace | label | single              (--group-by)
    label: app.kubernetes.io/part-of   # with by: label               (--group-label)
    apps:                   # explicit assignment, before everything else
      - name: monitoring
        kinds: [ServiceMonitor, PrometheusRule]

  ordering:
    enabled: true           #                                          (--no-order)
    inferDependencies: true # custom resources depend on the app installing their CRD
    waves:                  # app (glob) -> wave                       (--wave app=N)
      postgres: -2
    dependsOn:              # app (glob) -> apps                       (--depends-on app=a,b)
      shop: [postgres, redis]

  secrets:
    strategy: skip          # skip | local | plain | sops | sealed-secrets | external-secrets (--secrets)
    allowPlain: false       # required for plain                       (--allow-plain-secrets)
    overrides:
      - namespaces: [monitoring]
        names: ["*"]
        strategy: local
    sops:
      age: []               # recipients                               (--sops-age)
      pgp: []
      kms: []
      gcpKms: []
      azureKv: []
      encryptedRegex: ^(data|stringData)$
      decryptionSecret: sops-keys    # Flux decryption Secret name
    sealedSecrets:
      certFile: ""          # default: fetched from each environment's cluster (--sealed-secrets-cert)
      controllerNamespace: kube-system
      controllerName: sealed-secrets-controller
      scope: strict         # strict | namespace-wide | cluster-wide
    externalSecrets:
      apiVersion: external-secrets.io/v1
      storeKind: ClusterSecretStore   # SecretStore | ClusterSecretStore (--eso-store-kind)
      storeName: default              #                                  (--eso-store)
      keyTemplate: "{{ .Namespace }}/{{ .Name }}"   # also .Key
      propertyPerKey: true  # one remoteRef property per key; false = dataFrom.extract
      refreshInterval: 1h

  transforms: []            # applied for every environment; see below

  output:
    dir: gitops             #                                          (-o)
    tool: none              # none | argocd | flux                     (--tool)
    layout: folder-per-env  # folder-per-env | repo-per-env            (--layout)
    repo:
      url: ""               # required for argocd/flux                 (--repo-url)
      revision: main        #                                          (--revision)
      path: ""              # directory of the output inside the repository (--repo-path)
    argocd:
      pattern: app-of-apps  # app-of-apps | applicationset | single    (--pattern)
      namespace: argocd
      project: gitopsify    # "gitopsify" gets the environment appended (--project)
      createProject: true
      version: v3.5.6       # installed by bootstrap                   (--argocd-version)
      autoSync: true        #                                          (--no-auto-sync)
      prune: true           # false when adopt.enabled
      selfHeal: true
      serverSideApply: true
    flux:
      namespace: flux-system
      interval: 10m
      version: v2.9.6       #                                          (--flux-version)
      prune: true
      secretName: flux-system   # Git credentials of the GitRepository
    merge:                  # files already in the output directory
      edited: overwrite     # generated files changed by hand: overwrite | keep | fail (--edited)
      unmanaged: fail       # files gitopsify didn't write, where it writes: fail | overwrite | keep (--unmanaged; --force: overwrite)
      keep: []              # paths never written or removed; * within a directory, ** across (--keep)

  environments:             # default: one environment named "default"
    - name: uat             #                                          (--env uat=context)
      repoURL: ""           # with layout repo-per-env
      cluster:
        server: https://kubernetes.default.svc   # Argo CD destination, as Argo CD sees it
        name: ""            # Argo CD cluster name, instead of server
        # How gitopsify reaches the cluster for check/diff/adopt/status/bootstrap
        # and the sealed-secrets certificate: the same fields as source, set with
        # --env name=context or the --target-* flags.
        context: ""
        kubeconfig: ""
        apiServer: ""
        tokenFile: ""       # or --target-token / $GITOPSIFY_TARGET_TOKEN
      transforms: []        # this environment only

  adopt:
    enabled: false          # the target already runs these objects    (--adopt)
    protectKinds: [PersistentVolumeClaim, PersistentVolume, Namespace, CustomResourceDefinition.apiextensions.k8s.io]
    existing: include       # objects already in an environment's cluster (--existing):
                            #   include     generate them; the first sync takes them over
                            #   skip-owned  leave out those another tool manages there
                            #   skip        leave them all out (--tool argocd or flux only)
```

## Transforms

[Environments and transforms](environments.md) explains them with examples.
Every transform can have a `target` selector (`kinds`, `namespaces`,
`names`, `labelSelector`, `apps`); without one it applies to everything.
Global transforms change the base. Environment transforms become Kustomize
patches in `envs/<env>/`, or Helm values overrides; when one changes a
name or namespace the overlay holds full copies instead.

| type | fields | effect |
|---|---|---|
| `replace` | `from`, `to`, `regex` | Replace in every string value (not names), Secret data and Helm values |
| `image` | `from`, `to` | Rewrite image prefixes in pod specs and Helm values (`registry:` keys too) |
| `storageClass` | `map` | PVCs, PVs, StatefulSet `volumeClaimTemplates`, Helm `storageClass`/`storageClassName` |
| `ingressClass` | `map` | `ingressClassName`, the `kubernetes.io/ingress.class` annotation, Helm `className`/`ingressClassName` |
| `namespace` | `map` | Object namespaces, Namespace names, RBAC subjects, webhook/APIService service refs, `*.ns.svc` DNS names, Helm release namespaces |
| `set` | `path`, `value` | Set a field: `spec.replicas`, `spec.template.spec.containers[name=web].image`, `metadata.annotations["a/b"]`. `values.<path>` sets Helm values |
| `remove` | `path` | Remove a field (`values.<path>` for Helm values) |
| `jsonPatch` | `patch` | RFC 6902 operations |
| `labels` / `annotations` | `set`, `remove` (globs) | Edit metadata |

`helmValues: false` keeps `replace`/`image` away from Helm values. A Secret
must stay a Secret: transforms that change its `apiVersion` or `kind` are
refused (its values would skip the secrets strategy). Settings read from
`{env: ...}` and `{file: ...}` lose surrounding whitespace, such as a
trailing newline.
