# TODO

The longer-term list. Released and not announced yet: the user announces after
extensive work on the registry and the web UI. [HANDOFF.md](HANDOFF.md) has the state of the current
work and its next steps; move items there when work on them starts.

## New

- [ ] **Registry website** ([design proposal](docs/DESIGN-registry.md), waiting for the user's answers). A public site listing providers, like the
  Terraform Registry or Artifact Hub: search, a page per provider (README,
  kinds with their Terraform and Crossplane types, settings, versions from its
  GitHub releases, install command, maturity), built from
  `providers/index.yaml` and the providers' releases. Static (generated in CI,
  hosted on GitHub Pages) unless something needs a server. Needs a domain
  (ask the user for a DNS zone: the existing one is taken).
- [x] **Web UI** (branch `webui`, PR to review): [docs/web-ui.md](docs/web-ui.md),
  [design](docs/DESIGN-webui.md). Next for it:
  - [ ] Diff against the target and bootstrap/status from the UI (they
    change clusters; runs never do yet).
  - [ ] Revocable sessions (server-side, in PostgreSQL).
  - [ ] Editing clusters, tools and bindings in the UI (today: the Server
    resource, from Git).
  - [x] A JSON Schema for the Server resource (docs/gitopsify-server.schema.json).
  - [ ] Scheduled scans (drift reports).
  - [ ] Helm values with secrets: write them as encrypted values files (or
    a Secret for Flux valuesFrom) instead of redacting by key name.
  - [ ] Batched run log writes; runs that survive a rolling update.

## Providers

- [ ] Continuous use: three-way merge of re-exports with hand edits, then a
  `gitopsify sync` loop or a controller reconciling `Export` resources
  (docs/DESIGN-providers.md §11).
- [ ] Grafana provider: more contact point integrations, Grafana 13 v2
  dashboards, users and SSO.
- [ ] More providers from the template (Keycloak, Vault, ...).
- [ ] Verify provider release signatures (artifact attestations).

## Distribution

- [ ] Homebrew tap and Scoop bucket (repositories, `TAP_GITHUB_TOKEN`, the
  blocks in `.goreleaser.yaml`).
- [ ] CI workflow for `make live` (needs an HCLOUD token secret).
