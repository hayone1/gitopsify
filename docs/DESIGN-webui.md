# Design: web UI and server

Status: built (branch `webui`). User guide: [web-ui.md](web-ui.md).

## Goal

A browser front end for what the CLI does, for one person (`gitopsify ui`)
and for teams (`gitopsify server`, the Helm chart), that an enterprise can
adopt: single sign-on, roles per cluster and tool, an audit log, high
availability, and nothing that lets one team reach another's clusters,
repositories or Secrets.

## Decisions

| # | Decision | Why |
|---|---|---|
| W1 | **One binary.** The server is `internal/server` in the gitopsify binary; the UI (Nuxt 4, Nuxt UI 4, `ui/`) is a static single-page app built into `web/dist` and embedded. No Node at runtime | Same release, image and version as the CLI; the API is the only interface |
| W2 | **The CLI's engine, the CLI's files.** Runs call `pkg/pipeline`, `pkg/iac`, `pkg/provider`; what someone configures is a `Migration` or `Export` resource the CLI and CI run unchanged | The UI can't drift from the CLI, and a click-built migration becomes code |
| W3 | **Runs are background jobs** (scan, migrate, export) with a log streamed over server-sent events, persisted output, download and push | Scans and Helm resolution take minutes; people come back later |
| W4 | **Sign-in**: OIDC code flow with PKCE, state and nonce; sessions are AES-GCM sealed cookies (no session store); a CSRF header on changes; API tokens stored as SHA-256 for CI; local mode is a single-use link on localhost | Works with every identity provider and across replicas without shared session storage |
| W5 | **Roles** viewer/operator/admin bound to users and groups, per cluster and per tool (globs); server-wide actions need an unlimited binding | Teams own clusters and tools; platform admins own the server |
| W6 | **The server's access is never the user's input.** Clusters and tools are picked by name and their credentials come from the configuration; a submitted resource can't name files, kubeconfigs or tokens. Optionally clusters are read **as the signed-in person** (impersonation, never `system:` identities) | Operators must not reach the server's files or other clusters through a YAML field |
| W7 | **Git is the dangerous edge.** Remotes are https/ssh only, validated before use, run with protocol allowlists, no hooks or symbolic links, stall timeouts and a context; `git.repositories` says which repositories each cluster's and tool's runs may use with the server's credentials | A remote URL can be a command (`--upload-pack=`); a checkout can redirect writes (symlinks); shared credentials can write another team's GitOps repository |
| W8 | **Secret values stay out of runs** unless an admin allows them (`allowSecretValues`): no strategies that store values, no transforms that can move or test data (jsonPatch copy/move/test, kind changes), the default SOPS regex, strict sealing with the controller the server configures per cluster, Helm values that look like credentials redacted; SOPS only to admin-approved recipients unless the cluster impersonates | Runs are downloadable by every viewer of the cluster; a migration is code the operator writes, so every field that decides where values go is the server's |
| W9 | **Storage behind an interface**: the data directory (file, one replica) or PostgreSQL (several replicas: logs followed from the database, cancel through a flag, outputs archived and versioned, unpacked again when a push changed them, pushes locked across replicas, heartbeats in database time, owner-fenced updates, atomic reaping) | The demo/PVC pattern and the production pattern, with the same code above them |
| W10 | **Operability**: JSON logs with request IDs, an audit log (JSON lines or a table) for SIEMs, Prometheus metrics, health probes, rate limits behind trusted proxies, webhooks (Slack, Teams, JSON), a hardened chart (non-root, read-only root, no capabilities, scoped RBAC, NetworkPolicy, ServiceMonitor, PDB) | What a platform team asks before running something in production |

## Tests

- `internal/server` tests: an in-process fake OIDC provider (sign-in,
  refusals, forged cookies), roles, tokens, CSRF, runs and live events,
  cancel and restart, files and archives without secrets, exports through
  a fake provider (served by the test binary), notifications, headers and
  CSP, rate limits, the store conformance suite on files and PostgreSQL, two
  replicas on one database, and the review's attack cases (remote URLs,
  symlinks, redirects, impersonation, unverified emails, proxies).
- `test/e2e/webui/run.sh`: kind seeded like the CLI's e2e suite, Keycloak,
  a Git server and Playwright signing in as an operator, a viewer and an
  admin; `chart.sh` repeats the browser tests against the image and the
  chart (two replicas on PostgreSQL, impersonation) in kind.

## Not yet

- Revoking single sessions (sessions are stateless until they expire;
  rotating the key signs everyone out).
- Redacting Helm values by more than key names, or writing them as
  encrypted values files referenced from the HelmRelease.
- Batching run log writes, and resuming runs a stopping replica had to
  interrupt.
- Editing the server configuration (clusters, tools, bindings) in the UI:
  it lives in Git, deployed with the chart.
- Bootstrap and status of the target (`gitopsify bootstrap`/`status`) from
  the UI: they change clusters, which runs deliberately never do yet.
