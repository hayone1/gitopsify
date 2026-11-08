# Web UI

gitopsify has a web UI for everything the CLI does: scan a cluster, pick the
apps, choose the GitOps tool and how Secrets reach Git, review the result, and
push it. It also exports tools such as Grafana to Terraform or Crossplane
through providers. It runs in two ways:

| | `gitopsify ui` | `gitopsify server` (or the Helm chart) |
|---|---|---|
| For | you, on your machine | a team |
| Sign-in | a one-time link in your terminal | OpenID Connect: Keycloak, Dex, Okta, Entra ID, Google... |
| Clusters | your kubeconfig's contexts | the ones the server is configured with; optionally read as the signed-in person |
| Who may do what | you, everything | roles per cluster and tool, from groups |
| Audit log | yes | yes |

Every run produces the same configuration file the CLI uses, so what someone
clicked together runs unchanged in CI (`gitopsify generate --config ...`,
`gitopsify export --config ... --no-input`).

## On your machine

```sh
gitopsify ui
```

It opens the browser, signed in. Runs are kept in your cache directory
(`--data-dir` to change it). It only listens on localhost.

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
that may read everything and write nothing (`rbac.clusterReader`). Other
clusters are added under `server.clusters`, with their credentials mounted
through `extraVolumes`.

### Without Kubernetes

```sh
gitopsify server --config server.yaml     # or GITOPSIFY_SERVER_CONFIG, default /etc/gitopsify/server.yaml
```

The container image has it too: `docker run -p 8080:8080 -v ./server.yaml:/etc/gitopsify/server.yaml:ro hayone/gitopsify server`.

## Configuration

The server's configuration is a `Server` resource, in YAML, JSON or HCL like
gitopsify's other files. Secrets in it are references: `{env: VARIABLE}` or
`{file: /path}`, never values.

```yaml
apiVersion: gitopsify.io/v1alpha1
kind: Server
metadata:
  name: gitopsify
spec:
  listen: ":8080"
  externalURL: https://gitopsify.example.com
  dataDir: /var/lib/gitopsify
  trustForwardedFor: true            # behind an ingress that sets X-Forwarded-For
  auth:
    mode: oidc                       # or local (gitopsify ui)
    oidc:
      issuer: https://keycloak.example.com/realms/acme
      clientID: gitopsify
      clientSecret: {env: OIDC_CLIENT_SECRET}
      scopes: [profile, email]       # Dex: [profile, email, groups]
      groupsClaim: groups
      usernameClaim: email           # default: email, then preferred_username, then sub
      allowedDomains: [acme.io]      # optional
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
  git:
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
    allowSecretValues: false         # refuse the local and plain Secret strategies
  notifications:
    - name: platform-slack
      url: {env: SLACK_WEBHOOK}
      format: slack                  # slack, teams or json
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
| Microsoft Entra ID | `https://login.microsoftonline.com/<tenant>/v2.0` | `groupMembershipClaims` in the app manifest; `usernameClaim: preferred_username` |
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
matching role wins. People and groups are matched case-insensitively; globs
work (`team-*`).

### Clusters and impersonation

With `impersonate: true` the server reads a cluster as the signed-in person
and their groups (Kubernetes impersonation), so what someone can migrate is
what Kubernetes lets them read. The server's own identity then needs the
`impersonate` verb (the chart's `rbac.impersonate`). Without it, the server
reads with its own credentials and gitopsify's roles decide alone.

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

- **Sessions** are AES-GCM encrypted, HttpOnly, SameSite=Lax cookies (Secure
  behind HTTPS); changes need a CSRF header only the UI knows. Sign-ins use
  PKCE, state and nonce checks; unverified emails don't identify anyone.
- **Secrets** stay out of runs: the strategies that store Secret values
  (`local`, `plain`) are refused unless `runs.allowSecretValues`, git-ignored
  secret files are never listed, served or archived, and exports never write
  the tool's credentials (their export file names environment variables).
- **The browser** gets a strict Content-Security-Policy (scripts only from the
  server, inline ones by hash), `X-Frame-Options: DENY` and HSTS behind HTTPS.
  Run logs and file contents are shown as text, never as HTML.
- **Requests** are rate limited per client (tighter for sign-ins), carry an
  `X-Request-Id`, and are logged as JSON.
- **The audit log** records sign-ins, runs, pushes, downloads, provider
  installs and refused requests, as JSON lines in `dataDir/audit` (and the
  server log with `audit.stdout`), for your SIEM.

## Operating it

- Back up `dataDir` (runs and the audit log); the configuration and its
  Secrets come from Git and your secret store.
- Metrics: `gitopsify_http_requests_total`,
  `gitopsify_http_request_duration_seconds`, `gitopsify_runs_total`,
  `gitopsify_runs_active`, `gitopsify_logins_total`.
- Runs that were working when the server stopped are marked failed when it
  starts again.
- The server runs as one replica (runs work in the pod that started them and
  its volume holds their output).

## Developing the UI

The UI is a Nuxt app (Vue, Nuxt UI) in `ui/`, built into `web/dist` and
embedded in the binary by `make ui build`. For development, run
`gitopsify ui --listen 127.0.0.1:8080` and `make ui-dev` (http://localhost:3000
proxies the API). The end-to-end test (Keycloak, kind, a Git server and a
browser) is `test/e2e/webui/run.sh`, run on a throwaway machine.
