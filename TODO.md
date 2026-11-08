# TODO

The longer-term list. [HANDOFF.md](HANDOFF.md) has the state of the current
work and its next steps; move items there when work on them starts.

## New

- [ ] **Registry website.** A public site listing providers, like the
  Terraform Registry or Artifact Hub: search, a page per provider (README,
  kinds with their Terraform and Crossplane types, settings, versions from its
  GitHub releases, install command, maturity), built from
  `providers/index.yaml` and the providers' releases. Static (generated in CI,
  hosted on GitHub Pages) unless something needs a server. Needs a domain
  (ask the user for a DNS zone: the existing one is taken).
- [ ] **Web UI.** A browser front end for what the CLI does: connect to a
  cluster or a tool, pick what to migrate or export, review the generated
  repository and the first-sync diff, push. Likely `gitopsify ui` serving a
  local UI on localhost (credentials stay on the machine), reusing the
  wizard's steps; decide the stack and whether it can run in-cluster later.

## Providers

- [ ] Release gitopsify-provider-grafana v0.1.0 and add its `repoId` to
  `providers/index.yaml`.
- [ ] Release gitopsify v0.2.0 (providers, the resource config form).
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
