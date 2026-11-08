# Design: provider registry website (proposal)

Status: proposal, to agree on before building. See also
[DESIGN-providers.md](DESIGN-providers.md) (D7: the index).

## Goal

A public site where people find gitopsify providers and decide whether to use
one, like registry.terraform.io or Artifact Hub: what a provider reads, which
Terraform and Crossplane providers it writes for, its settings, versions and
how to install it. Provider authors get a "publish your provider" page that
leads to the template.

## Data

Nothing is entered by hand beyond what exists today:

| Source | Gives |
|---|---|
| `providers/index.yaml` (reviewed PRs) | which providers are listed, repo ID, maturity, maintainers |
| Each provider's GitHub releases | versions, dates, archives, checksums, attestations |
| `gitopsify-provider-<name> describe` of each release | kinds (Terraform types, Crossplane GVKs), settings and their environment variables, partition noun, upstream provider versions |
| The repository at the release tag | README |

`describe` is the same contract the CLI uses, so the registry can't drift from
what the provider really does.

## How it's built

- A generator in this repository (`cmd/registry` or `internal/registry`, Go,
  reusing `pkg/provider` for the index, downloads, checksum checks and
  `describe` parsing) writes a **static site**: HTML pages from Go templates,
  plus a JSON API (`/v1/providers.json`, `/v1/providers/<name>.json`) that
  the CLI's `provider search` can use later instead of the embedded index.
- Search and filters (output: Terraform/Crossplane, maturity) run in the
  browser over the JSON; no server.
- Hosting: GitHub Pages, rebuilt by a workflow on index changes, on a schedule
  (hourly) and when a provider release triggers it (`repository_dispatch`).
- **Running providers in CI is running third-party code**: the job that calls
  `describe` has no secrets and no write token, verifies the checksum (and
  attestation, when present) first, runs the binary without network
  (`unshare -n` or `docker run --network none`) and with a timeout.

## Pages

- Home: search, filters, providers as cards (title, description, outputs,
  maturity, latest version).
- Provider: install and export commands, kinds table (kind, Terraform type,
  Crossplane GVK), settings table (name, environment variable, secret or
  not), upstream versions, README, versions list, links (repo, issues).
- Publish a provider: the template, the index rules, conformance.

## Open questions

1. **Domain.** Needs a DNS zone (the existing one is taken). Until then
   `hayone1.github.io/gitopsify-registry` (or a project page).
2. **Where it lives:** the generator here (it shares `pkg/provider`), the
   published site in its own repository/Pages branch. Recommended.
3. **Also host gitopsify's docs** on the same site later? It would make the
   domain the project's home.
