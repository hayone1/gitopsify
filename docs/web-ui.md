# Web UI

gitopsify has a web UI for everything the CLI does: scan a cluster, pick the
apps, choose the GitOps tool and how Secrets reach Git, review the result, and
push it. It also exports tools such as Grafana to Terraform or Crossplane
through providers. It runs in two ways:

| | `gitopsify ui` | `gitopsify server` (or the Helm chart) |
|---|---|---|
| For | you, on your machine | a team |
| Sign-in | a single-use link in your terminal | OpenID Connect: Keycloak, Dex, Okta, Entra ID, Google... |
| Clusters | your kubeconfig's contexts | the ones the server is configured with; optionally read as the signed-in person |
| Who may do what | you, everything | roles per cluster and tool, from groups |
| Audit log | yes | yes |

Every run produces the same configuration file the CLI uses, so what someone
clicked together runs in CI too (`gitopsify generate --config ...`,
`gitopsify export --config ... --no-input`). Clusters in it are named as the
server knows them: in CI, point `--context` (or the file's `context`) at your
kubeconfig's names.

## On your machine

```sh
gitopsify ui
```

It opens the browser, signed in. The sign-in link works once; start
`gitopsify ui` again for a new one. Runs are kept in your cache directory
(`--data-dir` to change it). It only listens on localhost. Like the CLI,
it doesn't keep Secret values from you (the server's protections below
are for teams).

## For a team

### With Helm

```sh
helm install gitopsify oci://ghcr.io/hayone1/charts/gitopsify \
  --namespace gitopsify --create-namespace -f values.yaml
# (or ./charts/gitopsify from this repository)
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
      - role: viewer
        groups: [developers]
secrets:
  oidcClientSecret: "..."          # or secrets.existingSecret
ingress:
  enabled: true
  className: nginx
  host: gitopsify.example.com
  tls: [{secretName: gitopsify-tls, hosts: [gitopsify.example.com]}]
```

Register `https://gitopsify.example.com/api/v1/auth/callback` as the client's
redirect URL at your identity provider.

The chart runs one replica as a non-root user, with a read-only root file
system and a PersistentVolumeClaim for runs and the audit log. The cluster it
runs in is available as `in-cluster` (`inCluster.*`), through a ServiceAccount
that may list everything and write nothing (`rbac.clusterReader`). Other
clusters are added under `server.clusters`, with their credentials mounted
through `extraVolumes`. Behind an ingress, set `server.trustedProxies` to
the ingress controller's pod CIDR so the audit log and rate limits see
clients' addresses.

### Without Kubernetes

```sh
gitopsify server --config server.yaml     # or GITOPSIFY_SERVER_CONFIG, default /etc/gitopsify/server.yaml
```

The container image has it too: `docker run -p 8080:8080 -v ./server.yaml:/etc/gitopsify/server.yaml:ro hayone/gitopsify server`.

## Configuration

The server's configuration is a `Server` resource, in YAML, JSON or HCL like
gitopsify's other files. Secrets in it are references: `{env: VARIABLE}` or
`{file: /path}`, never values (surrounding whitespace, such as the newline
of a Secret made from a file, is dropped). Its JSON Schema,
[gitopsify-server.schema.json](gitopsify-server.schema.json), gives editors
completion and checks (the first line below enables it in VS Code's YAML
extension).

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/hayone1/gitopsify/main/docs/gitopsify-server.schema.json
apiVersion: gitopsify.io/v1alpha1
kind: Server
metadata:
  name: gitopsify
spec:
  listen: ":8080"
  externalURL: https://gitopsify.example.com
  dataDir: /var/lib/gitopsify
  storage:
    type: file                       # or postgres, for several replicas
    # postgres: {url: {env: DATABASE_URL}, maxConnections: 10}
  trustedProxies: [10.42.0.0/16]     # the ingress controller's pods only (see Protections)
  metrics:
    token: {env: METRICS_TOKEN}      # /metrics needs "Authorization: Bearer <token>"
  auth:
    mode: oidc                       # or local (gitopsify ui)
    oidc:
      issuer: https://keycloak.example.com/realms/acme
      clientID: gitopsify
      clientSecret: {env: OIDC_CLIENT_SECRET}
      scopes: [profile, email]       # Dex: [profile, email, groups]
      groupsClaim: groups
      usernameClaim: email           # default: email, then preferred_username, then sub
      allowedDomains: [acme.io]      # optional; verified emails only
      trustUnverifiedEmail: false    # emails count only with email_verified: true
    sessionKey: {env: SESSION_KEY}   # 32+ random bytes; keep it stable
    sessionTTL: 12h
    tokens:                          # API tokens for CI (see below)
      - {name: ci, role: operator, sha256: "…", clusters: [staging-*]}
  rbac:
    defaultRole: viewer              # for anyone who signs in; empty: no access
    bindings:
      - {role: admin, groups: [platform-team]}
      - {role: operator, groups: [shop-team], clusters: [staging-*]}
      - {role: operator, groups: [observability], tools: [grafana-*]}
  clusters:
    - name: prod-eu
      description: Production, Frankfurt
      impersonate: true              # read as the signed-in person
      impersonateUserPrefix: "oidc:" # optional, like --oidc-username-prefix
      impersonateGroupPrefix: "oidc:"
      impersonateGroups: [shop-*, platform-team]  # optional: only these groups
      sealedSecrets:                 # where its sealed-secrets controller runs (these are the defaults)
        controllerNamespace: kube-system
        controllerName: sealed-secrets-controller
      access:
        apiServer: https://prod-eu.example.com:6443
        certificateAuthority: /etc/gitopsify/prod-eu/ca.crt
      token: {file: /etc/gitopsify/prod-eu/token}
  tools:
    - name: grafana-prod
      provider: grafana
      settings:
        url: https://grafana.example.com
        auth: {env: GRAFANA_AUTH}
  providers:
    dir: /var/lib/gitopsify/providers
    disableInstall: false            # admins install providers from the UI
    env: [AWS_*]                     # server variables providers may see (globs; see Protections)
  git:
    repositories:                    # what runs may check out and push to
      - {url: "https://github.com/acme/shop-*", clusters: [shop-*]}
      - {url: "https://github.com/acme/grafana-iac", tools: [grafana-*]}
    credentials:
      - host: github.com
        token: {env: GITHUB_TOKEN}   # sent as an Authorization header, never in URLs
      - host: gitlab.example.com
        sshKeyFile: /etc/gitopsify/git/id_ed25519
        knownHostsFile: /etc/gitopsify/git/known_hosts
  runs:
    concurrency: 2
    timeout: 30m
    retention: 720h
    allowSecretValues: false         # refuse what can put Secret values in runs (see Security)
    sopsRecipients: [age1...]        # recipients SOPS may encrypt to (see Security)
    maxQueuedPerUser: 10
  notifications:
    - name: platform-slack
      url: {env: SLACK_WEBHOOK}
      format: slack                  # slack, teams or json (the URL is checked at startup, never logged)
      events: [run.succeeded, run.failed]
  audit:
    stdout: true
    retention: 2160h
```

### Identity providers

| Provider | issuer | Groups |
|---|---|---|
| Keycloak | `https://<host>/realms/<realm>` | add a "Group Membership" mapper (claim `groups`, full path off) to the client |
| Dex | Dex's issuer | add `groups` to `scopes` |
| Okta | `https://<org>.okta.com/oauth2/default` | add a `groups` claim to the authorization server |
| Microsoft Entra ID | `https://login.microsoftonline.com/<tenant>/v2.0` | `groupMembershipClaims` in the app manifest (groups arrive as object IDs: bind those); `usernameClaim: oid` (Entra sends no `email_verified`, and users can change `email`) |
| Google | `https://accounts.google.com` | none (bind users by email) |

The client is confidential (it has a secret) and uses the authorization code
flow with PKCE.

### Roles

| Role | May |
|---|---|
| viewer | see clusters, tools, runs, their logs and files, download them |
| operator | also scan, generate, export, cancel and push |
| admin | also delete anyone's runs; without cluster or tool limits, read the audit log and install providers |

A binding without `clusters` and `tools` applies everywhere. One with
`clusters` gives nothing on tools, and the other way round. The highest
matching role wins. `users` match the username (the verified email, unless
`usernameClaim` names another claim); names match case-insensitively, globs
(`team-*`) case-sensitively.

### Clusters and impersonation

With `impersonate: true` the server reads a cluster as the signed-in person
and their groups (Kubernetes impersonation), so what someone can migrate is
what Kubernetes lets them read. Usernames and groups starting with `system:`
are never impersonated (an identity provider group named `system:masters`
must not become cluster-admin), nor, without a group prefix, groups starting
with `kubeadm:` (kubeadm binds `kubeadm:cluster-admins` to cluster-admin).
`impersonateUserPrefix` and `impersonateGroupPrefix` add prefixes as the API
server's OIDC flags do, and `impersonateGroups` passes only the groups it
lists (globs). Set a prefix or the list whenever people can name their own
groups in the identity provider (self-service groups, Dex's GitHub
`org:team`): otherwise a group named like one your RBAC binds grants its
rights. The server's own identity needs the `impersonate` verb (the chart's
`rbac.impersonate`), which makes it highly privileged: protect it. API
tokens are impersonated as `token:<name>`, so bind that user in Kubernetes
for CI scans of such clusters.

Without impersonation, the server reads with its own credentials and
gitopsify's roles decide alone: an operator can migrate everything the
server can read.

A migration submitted to the server can't name kubeconfigs, token files or
other paths: clusters are picked by name, and the server fills in how to reach
them.

### API tokens

```sh
gitopsify server token --name ci --role operator --cluster 'staging-*'
```

prints a token and the `spec.auth.tokens` entry that admits it (only its
SHA-256 is stored). Send it as `Authorization: Bearer <token>`.

## API

Everything the UI does goes through `/api/v1`, with the session cookie (and an
`X-Gitopsify-CSRF` header on changes) or an API token:

| | |
|---|---|
| `GET /api/v1/me` | who you are, your roles |
| `GET /api/v1/clusters`, `GET /api/v1/clusters/{name}/check`, `.../namespaces` | clusters |
| `GET /api/v1/tools`, `GET /api/v1/tools/{name}/check`, `GET/POST /api/v1/providers` | tools and providers |
| `POST /api/v1/runs` | start a run: `{"type": "scan"\|"migrate", "cluster": "...", "migration": "<yaml>"}` or `{"type": "export", "tool": "...", "export": "<yaml>", "repo": {...}}`, optionally `"publish": {}` |
| `GET /api/v1/runs`, `GET /api/v1/runs/{id}` | runs |
| `GET /api/v1/runs/{id}/events` | the log and status, live (server-sent events) |
| `GET /api/v1/runs/{id}/files`, `.../files/{path}`, `.../archive` | the output |
| `POST /api/v1/runs/{id}/publish`, `.../cancel`, `DELETE /api/v1/runs/{id}` | actions |
| `GET /api/v1/audit` | the audit log (admins) |

`/healthz`, `/readyz` and `/metrics` (Prometheus) need no sign-in.

## Security

### Who can do what to whom

- **Operators** start runs with the server's access to clusters and tools,
  and its Git credentials. So:
  - `git.repositories` lists the repositories runs may check out and push to,
    per cluster and tool. Without it, an operator of one cluster could push
    to another team's GitOps repository with the server's token (and Argo CD
    or Flux would apply it). Leave it empty only on a server for one team.
  - Secret values reach runs only encrypted for keys an admin chose, unless
    an admin sets `runs.allowSecretValues`. Without it the server refuses:
    the `local` and `plain` strategies; jsonPatch `copy`, `move` and `test`
    operations (they could copy a Secret's data into an annotation, which
    neither sealing nor SOPS encrypts); transforms that turn a Secret into
    another kind, or move a Secret whose values are written encrypted to
    another namespace or name (sealing binds to them: the operator could
    pick a namespace they control in the target cluster); a custom
    `sops.encryptedRegex`; and sealing scopes wider than `strict`.
    Transforms leave such a Secret's values as they are, whatever they set,
    replace or remove (a result that depends on a value would tell whether
    a guess was right). Repositories that track `.secrets/` or
    `*.secrets.auto.tfvars` (where gitopsify writes values in clear) are
    refused before checking out results into them and before pushing. Sealed Secrets are sealed with the controller the server
    configures for the target cluster (`clusters[].sealedSecrets`), never one
    a migration names (it could be the operator's own service). SOPS needs
    recipients listed in `runs.sopsRecipients` (an operator's own key would
    let them decrypt every Secret), except on impersonating clusters, where
    people only read what Kubernetes lets them.
  - These protections keep values out of what runs produce and people
    download. They don't protect the target: whoever can push to a target
    cluster's GitOps repository (operators, through runs) can deploy
    something there that reads the Secrets migrated to it, and whoever can
    create SealedSecrets in a namespace there can unseal what was sealed
    for it. Protect target repositories with branch protection or reviews,
    and keep `git.repositories` narrow.
  - Helm values come from release Secrets and often hold passwords. Without
    `runs.allowSecretValues`, values that look like credentials are
    replaced with `<redacted by gitopsify: set from a Secret>` and the run
    warns about each:
    - strings under keys such as `password`, `token`, `secret`, `apiKey`,
      `fernetKey`, `bindpw` or `pass`, and the strings of maps (and of
      lists of maps) under keys whose last word is one (`secrets`,
      `extraSecrets`, `credentials`, `secureJsonData`), but names,
      references (`existingSecret`, `{name, key}`, GitLab's `{secret,
      key}`, `secretName`, `valueFrom`, pull secrets), settings
      (`mountPath`, `token_url`, `tokenIssuer`, `annotations`), templates
      (`{{ ... }}`) and switches (`on`, `off`);
    - numbers under keys ending in a credential word (`adminPin`,
      `apiKey`; not `passwordMinLength`, `tokenTTL` or `maxToken`);
    - the value of env pairs such as `{name: DB_PASSWORD, value: ...}`
      (not settings such as `TOKEN_TTL` or `TOKEN_URL`);
    - inline TLS private keys (`key` next to `cert`, `crt` or
      `certificate`, unless it's a file path) and PEM private keys
      anywhere;
    - the password of URLs in any string (`postgres://app:<redacted>@db/app`,
      `--db=...` arguments, lists of hosts; not `$(VAR)` references or
      templates).
    Vendored charts' own `values.yaml` too. Set them from a Secret (Flux
    `valuesFrom`, an encrypted values file) before syncing. This goes by
    names and shapes: secrets under other names, in query parameters
    (`?password=`) or in connection strings (`Password=...;`) are written
    as they are, and a value redacted by mistake shows in the warning.
    Releases in `helm.mode: manifests` write no values.
  - Repository URLs must be https or ssh (http with `git.allowHTTP`), with
    no options, local paths or passwords; git runs with only those
    protocols, without hooks or symbolic links, and stalls are aborted.
- **Viewers** see runs, logs and files of their clusters and tools.
- **Admins** without limits read the audit log and install providers
  (unless `providers.disableInstall`).

### Protections

- **Sessions** are AES-GCM encrypted, HttpOnly, SameSite=Lax cookies (Secure
  behind HTTPS); changes need a CSRF header only the UI knows. Sign-ins use
  PKCE, state and nonce checks; unverified emails don't identify anyone.
- **Secrets** stay out of runs (see above). The files gitopsify writes
  values to in clear (`.secrets/`, `*.secrets.auto.tfvars`, git-ignored)
  are never listed, served, stored in the database or pushed; other
  `*.secrets.*` files (a SOPS-encrypted values file a repository tracks)
  aren't listed or served, but are kept and pushed. Exports never write
  the tool's credentials (their export file names environment
  variables). Providers run with a minimal
  environment (`PATH`, `HOME`, proxies, certificates) plus the variables
  `providers.env` lists (such as `AWS_*` for the pod's cloud identity, or
  `KUBERNETES_SERVICE_*`, which would let a provider use the server's
  ServiceAccount), never the server's own secrets (the session key,
  client secret, database URL, Git and metrics tokens, webhook URLs, any
  `GITOPSIFY_*` variable) or another tool's settings. Tool settings reach
  them in the request. This is hygiene, not isolation: providers run as
  the server's user, so install only providers you trust.
- **The browser** gets a strict Content-Security-Policy (scripts only from the
  server, inline ones by hash), `X-Frame-Options: DENY` and HSTS behind HTTPS.
  Run logs and file contents are shown as text, never as HTML.
- **Requests** are rate limited per client (tighter for sign-ins), carry an
  `X-Request-Id`, and are logged as JSON. Behind a proxy, list it in
  `trustedProxies`: the client is then the last `X-Forwarded-For` entry that
  isn't a trusted proxy (entries before it are whatever the client sent).
  List the proxies as narrowly as you can (the ingress controller's pod
  CIDR): a range people connect from, such as a VPN's, would let them pick
  the address the audit log and rate limits see. Empty (the default), the
  client is the address that connected.
- **Sessions** last `sessionTTL` and can't be revoked one by one: rotate
  `sessionKey` to sign everyone out.
- **The audit log** records sign-ins, runs, pushes, downloads, provider
  installs and refused requests, as JSON lines in `dataDir/audit` (and the
  server log with `audit.stdout`), for your SIEM.

## Storage and high availability

| `storage.type` | Keeps runs, logs, outputs, the audit log in | Replicas |
|---|---|---|
| `file` (default) | `dataDir` (the chart's PersistentVolumeClaim) | one |
| `postgres` | a PostgreSQL database (tables `gitopsify_*`, created on start) | several |

With PostgreSQL, any replica serves any run: a run works on the replica that
started it, the others follow its log from the database, cancel it through a
flag the working replica picks up, and unpack its output (archived in the
database when it finishes) to serve files or push. Pushes of a run take a
lock in the database (a two-minute lease renewed while pushing, which
holds no connection; a replica that can't renew it for a minute stops its
push and doesn't store the output), so two replicas never push it at
once, deleting and pruning wait for it, and each push starts from the
last one's commit (copies older than the stored output are unpacked
again; unused copies are removed after an hour). The files gitopsify
writes values to in clear stay out of the database. If a replica dies
while working, another fails its runs
within about three and a half minutes (two minutes without a heartbeat,
by the database's clock, plus the next check) and notifies about them; a
replica that comes back stops working on runs it no longer owns, and a
run whose final update failed is failed by the next check. Sessions
work on every replica because they're sealed with the shared `sessionKey`.

Use `sslmode=verify-full` in the URL (outputs read from the database get
pushed to Git). Behind PgBouncer, use session pooling: transaction pooling
breaks the lock gitopsify takes while creating its tables at startup (and
pgx's statement cache, unless the URL sets
`default_query_exec_mode=simple_protocol`). `maxConnections` defaults to 4
or the number of CPUs, whichever is more.

In the chart:

```yaml
storage:
  type: postgres
  postgres:
    existingSecret: gitopsify-db     # a Secret with key "url": postgres://...?sslmode=verify-full
replicaCount: 3                      # adds a PodDisruptionBudget
```

With more than one replica, providers installed from the UI would land on
one pod only, so the chart sets `server.providers.disableInstall: true`
unless you set it: bring providers in the image, or on a shared volume
(`server.providers.dir`). Without `existingSecret`, the chart keeps
`storage.postgres.url` in a Secret of its own (`<release>-postgres`), which
also works next to `secrets.existingSecret`.

## Operating it

- Back up `dataDir` with file storage, or the database with PostgreSQL; the
  configuration and its Secrets come from Git and your secret store.
- Behind an ingress, set `server.trustedProxies` (otherwise every client
  shares the ingress controller's rate limits) and `server.metrics.token`
  (the ingress serves `/metrics` too); `metrics.serviceMonitor.tokenSecret`
  names the Secret Prometheus reads the token from.
- Metrics: `gitopsify_http_requests_total`,
  `gitopsify_http_request_duration_seconds`, `gitopsify_runs_total`,
  `gitopsify_runs_active`, `gitopsify_logins_total`.
- OpenTelemetry traces (a span per API request and per run, Kubernetes
  calls), metrics and logs go to a collector when `OTEL_*` variables or the
  chart's `opentelemetry.endpoint` ask for it: [observability](observability.md).
- Stopping the server (a restart, a rolling update) fails the runs it was
  working on as "interrupted" (start them again); `/readyz` fails while it
  stops, so new requests go to other replicas. Runs whose server died are
  failed when it starts again (file storage) or by another replica.
- With file storage the server runs as one replica (the chart refuses more).
- The chart's ClusterRole lists and watches every resource and never gets
  single objects: `get` on `"*"` would also cover subresources such as
  `nodes/proxy` (the kubelet), `pods/log` and `services/proxy`. A Role in
  the sealed-secrets controller's namespace allows fetching its certificate
  through that one service's proxy (`inCluster.sealedSecrets`).

### Known limits

- Sessions can't be revoked one by one (rotate `sessionKey`).
- Without `git.repositories`, runs may use any https or ssh remote,
  internal addresses included, and Git's errors tell whether one answers.
  The server warns at startup when roles are limited to clusters or tools
  but repositories aren't.
- Without a verified email, the username is `preferred_username` (or the
  claim `usernameClaim` names): make sure people can't choose it in the
  identity provider, or bindings by email could match it.
- Run logs are written to the store a line at a time; `maxQueuedPerUser`
  is checked before a run is created, so simultaneous requests can pass it
  by a few.

## Developing the UI

The UI is a Nuxt app (Vue, Nuxt UI) in `ui/`, built into `web/dist` and
embedded in the binary by `make ui build`. For development, run
`gitopsify ui --listen 127.0.0.1:8080` and `make ui-dev` (http://localhost:3000
proxies the API). The end-to-end test (Keycloak, kind, a Git server and a
browser) is `test/e2e/webui/run.sh`, run on a throwaway machine; it saves
every screenshot in the light and the dark theme.

The look follows IBM's [Carbon Design System](https://carbondesignsystem.com)
(Apache-2.0): its palettes as Nuxt UI colors and its White and Gray 100
themes as Nuxt UI's variables (`ui/app/assets/css/main.css`,
`ui/app/app.config.ts`), square corners and IBM Plex (SIL OFL 1.1; its
license travels in the font files), bundled so nothing is fetched. Text keeps
4.5:1 contrast in both themes, tinted badges included. The color mode (System, Light, Dark) is in the sidebar and on the
sign-in page; it follows the system until someone picks one, and the browser
remembers it.
