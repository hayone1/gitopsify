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
- [ ] **Web UI** ([design proposal](docs/DESIGN-webui.md), waiting for the user's answers). A browser front end for what the CLI does: connect to a
  cluster or a tool, pick what to migrate or export, review the generated
  repository and the first-sync diff, push. Likely `gitopsify ui` serving a
  local UI on localhost (credentials stay on the machine), reusing the
  wizard's steps; decide the stack and whether it can run in-cluster later.

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
