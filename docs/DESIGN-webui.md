# Design: web UI (proposal)

Status: proposal, to agree on before building.

## Goal

A browser front end for what the CLI does, for people who'd rather see and
click than read flags: migrate a cluster to a GitOps repository, and export a
tool through a provider. It must not become a second product: every run
produces the same configuration file the CLI uses (a `Migration` or `Export`
resource), with the command to replay it in CI.

## Shape

- **Local first:** `gitopsify ui` starts a server on `127.0.0.1` (random
  port), opens the browser and uses the same kubeconfig, environment and
  provider directory as the CLI. Credentials never leave the machine.
- **Same engine:** an HTTP API in the binary (`internal/web`) on top of the
  packages the CLI uses (`pkg/pipeline`, `pkg/target`, `pkg/bootstrap`,
  `pkg/provider`, `pkg/iac`); progress streams over server-sent events.
- **Frontend** embedded in the binary with `go:embed`, built in CI.
- Later, not v1: an in-cluster, multi-user mode (OIDC, RBAC).

## Flows

1. **Migrate:** pick a kube context (with `doctor`'s checks), scan, select
   namespaces and apps in a tree (dependencies shown), choose the target
   (Kustomize / Argo CD / Flux, layout, secrets), preview the repository
   (file tree, contents, `kustomize build`), diff the first sync against the
   target cluster, write or push, bootstrap, watch status.
2. **Export:** pick a provider (installed or from the index), fill settings
   (secrets masked), check, pick partitions, kinds and objects (references
   pulled in), choose Terraform and/or Crossplane, preview, write, save the
   `Export` resource.
3. **Configuration:** open an existing `gitopsify.yaml` /
   `gitopsify-<provider>.yaml`, edit with schema validation, run it.

## Security (local server)

Bound to localhost only; a random token in the opening URL, then a cookie;
`Host` header checked (DNS rebinding); no CORS; state-changing requests need
the token; secret values are never sent back to the browser; `--listen` for
other addresses prints a warning.

## Stack (open question)

| Option | For | Against |
|---|---|---|
| **React + TypeScript (Vite)** (recommended) | Biggest ecosystem for what this needs: tree pickers, a YAML/HCL editor with schema validation (Monaco), diff viewers; easy to find contributors | Node toolchain for frontend work; larger bundle |
| Svelte + TypeScript | Smaller and simpler | Fewer ready-made components |
| Go templates + htmx | No JS toolchain at all | Hard for the editor, diff and tree views |

## Testing

API tests in Go; browser tests with Playwright in CI (not on the WSL machine).

## Open questions

1. Stack (above).
2. Local-only for v1 (recommended), or in-cluster from the start?
3. Order: the registry first (smaller, and it needs the releases that now
   exist), then the UI (recommended).
