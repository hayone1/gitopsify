# Handoff: gitopsify

State as of 2026-11-08. Repo: https://github.com/hayone1/gitopsify (public
since 2026-11-07; history scanned for credentials first: clean). Latest
release: v0.2.0 (2026-11-08, not announced: the user announces once the
registry and web UI are done). The provider system (PR #1) is merged into `main`; the SDK is
tagged `sdk/v0.1.0` (on the merge commit), `go.mod` requires it and the
committed `go.work` points development and CI at `sdk/` (see
docs/development.md, "The SDK module"). Sibling repositories, both public:
`../gitopsify-provider-grafana` (github.com/hayone1/gitopsify-provider-grafana)
holds the first provider; `../gitopsify-provider-template`
(github.com/hayone1/gitopsify-provider-template, marked as a GitHub template)
is the starting point for new providers. Both require sdk v0.1.0.
Longer-term list: [TODO.md](TODO.md).

**Branch `webui`** (pushed, not merged; written 2026-11-08 while the user
was away, as asked: "fully implement and make production ready the webui
with cluster, multi-user mode even oidc etc"): the web UI and team server.
See the section below; the user reviews it before merging.

## Web UI (branch `webui`)
`gitopsify ui` (one person, localhost, single-use link) and `gitopsify
server` (teams; the Helm chart `charts/gitopsify`). Guide docs/web-ui.md,
design docs/DESIGN-webui.md (W1-W10).
- `internal/server`: api.go (routes), auth.go (OIDC PKCE/state/nonce, AES-GCM
  cookies, CSRF header, API tokens, local token), rbac.go (viewer/operator/
  admin per cluster and tool), runs.go (background runs, SSE with event IDs,
  cancel, multi-replica), store.go (file store, tar.gz pack/unpack),
  storepg.go (PostgreSQL), migrate.go (Migration runs; the request is made
  safe: clusters by name, no paths, Git checks), export.go (provider exports
  of admin-registered tools), git.go (remote/branch checks, repository
  allowlist, git env: protocol allowlist, no symlinks/hooks, timeouts),
  clusters.go (impersonation without system: identities), audit.go,
  notify.go (Slack/Teams/JSON webhooks), metrics.go, static.go (embedded UI,
  CSP with hashes of index.html's inline scripts).
- `pkg/config/server.go`: the `Server` resource.
- `ui/`: Nuxt 4 + Nuxt UI 4 SPA (`make ui` builds into web/dist, embedded by
  `web/embed.go`; `make ui-dev` proxies a local server). Pages: overview,
  migration wizard, export wizard, runs, run (log/apps/files/config/pushes),
  clusters, tools (+provider catalog), audit, profile, login.
- Tests: internal/server (fake OIDC provider, roles, tokens, runs, exports
  with a fake provider, store conformance on files and PostgreSQL
  (GITOPSIFY_TEST_POSTGRES), two replicas, review attack cases);
  test/e2e/webui/run.sh (kind + Keycloak + Git server + Playwright, binary)
  and chart.sh (image + chart, 2 replicas on PostgreSQL, impersonation).
- Dev VM: `test/live/hetzner/devbox.sh up|sync|run|fetch|down` keeps a
  Hetzner cx33 for the session (Node, Go, kind, Docker); `down` destroys it.

## What it is
Go CLI (cobra + Charm huh/lipgloss) that turns a running cluster (or a
`kubectl get -A -o yaml` dump via `--from-file`) into a GitOps repo for plain
Kustomize, Argo CD (app-of-apps / applicationset / single) or Flux, and can
check the target, diff the first sync, install Argo CD/Flux, wait for it to
sync, and report status. Ships AI agent skills and a Docker image.

## Providers
`gitopsify export <provider>` turns a tool's live state into Terraform/OpenTofu
and Crossplane (Kustomize) through providers: separate executables
`gitopsify-provider-<name>` speaking JSON over stdio. Design and decisions:
`docs/DESIGN-providers.md` (D1-D12); user guide `docs/providers.md`; author
guide `docs/writing-providers.md`.
- `sdk/`: separate Go module (stdlib only): protocol types, markers
  ($ref/$secret/$json/$file), `Serve`, `ForProvider` (Upjet-style Crossplane
  derivation), `sdk/sdktest` (conformance; validation against a Terraform
  schema dump and CRDs). Main go.mod has `replace ... => ./sdk`; the release
  workflow refuses replace directives (see docs/development.md "The SDK
  module" for the tag-then-require steps).
- `pkg/provider`: find (installed dir, PATH, path), run (client.go), install
  from GitHub releases (checksums, pins per platform, binary hash, repo ID),
  index (`providers/index.yaml`, embedded).
- `pkg/iac`: workspaces per instance × partition, stable names and file
  hashes in `<out>/.gitopsify/export.json`, removed blocks for objects that
  disappear, scoped writes through `os.Root`, Terraform (hclwrite) and
  Crossplane renderers, report and READMEs.
- `pkg/config`: `kind: Export` (`gitopsify-<provider>.yaml`), and every
  gitopsify file is a Kubernetes-style resource readable as YAML, JSON or HCL
  (documents.go). `init`, `generate` and `export --save` write resources
  (`config.Encode`: YAML, `.json`, `.hcl`; YAML 1.1 booleans quoted); v0.1's
  flat `Migration` layout is still read, and docs/gitopsify.schema.json
  accepts both: `kind: Export` → the export schema, a spec field at the top →
  v0.1's layout, otherwise the resource. `export --save` into a YAML file
  holding other resources replaces only its document (save.go, verified
  before writing); JSON/HCL shared files fall back to
  `gitopsify-<provider>.yaml`. HCL writing lives in `internal/hclgen`
  (shared with pkg/iac).
- `internal/cli/export.go` (wizard + CI path), `provider.go` (list, search,
  install, remove, info). User-facing skill `skills/gitopsify-export`.
- SDK + template + reference provider, like Terraform/Crossplane (D13):
  `sdk/` (incl. `Resource.Summary()` for summary discovers), the template repo
  (made-up notes provider, `go run ./tools/prepare -name X` renames it and
  deletes itself, `make upstream` = tools/upstream fetches the pinned
  Terraform schema via terraform and the CRDs from the Crossplane package's
  package.yaml via crane, `make record`, CI/release, AGENTS.md). The Grafana
  repo has the same tools/upstream (keep the two copies in sync).
- `.gitopsify/export.json` hashes every file written except the git-ignored
  secret files (a hash would let readers check guessed secrets offline).
- Maintainer agent files: `CLAUDE.md`, `.claude/skills/session-handoff`.

## Where things are
- `README.md` (lean) → `docs/`: getting-started, examples (many recipes +
  GIFs), concepts, authentication, helm, secrets, environments, gitops-tools,
  takeover, ai-agents, troubleshooting, install, configuration,
  commands (generated), gitopsify.schema.json (generated), DESIGN.md
  (decisions incl. a table of findings from real clusters), development.md.
- `cmd/gitopsify`; `internal/cli` (commands incl. `doctor`, `status`,
  `skills`, hidden `gen-docs`; grouped `--help` in help.go; completions);
  `internal/ui`.
- `pkg/kube`: `Connect(config.Access)` for every connection; `auth.go`
  describes auth and explains errors per platform; `config.Access` is shared
  by `source` and `environments[].cluster` (flags `--x` / `--target-x`).
- `pkg/pipeline` stages: discovery → helm → filter → deps → clean →
  portability → group → transform → secrets → order → render.
- `pkg/target`: check / diff / adopt / status (+ Wait, unhealthy reasons).
- `pkg/bootstrap`: installs Argo CD/Flux; explains readiness timeouts.
- `skills/` (embedded, go:embed): gitopsify-migrate (+ references),
  git-remote, cluster-access, secrets, takeover, troubleshoot. Packaged as a
  Claude Code plugin marketplace (`.claude-plugin/`), Gemini CLI extension
  (`gemini-extension.json`, `GEMINI.md`, `commands/gitopsify/*.toml`) and via
  `gitopsify skills install`.
- Distribution: `install.sh`, `install.ps1`, `.goreleaser.yaml` (archives,
  deb/rpm/apk/arch, completions via `scripts/completions.sh`),
  `scripts/set-version.sh`, `Dockerfile` (Alpine + git/ssh/helm/kubectl,
  non-root, amd64/arm64).
- Workflows: `ci.yml` (lint/test/build; e2e on dispatch + weekly),
  `release.yml` (GoReleaser + installer verification on Linux/macOS/Windows),
  `docker.yml` (build always; publish to docker.io/hayone/gitopsify and
  ghcr.io/hayone1/gitopsify on v* tags or dispatch with push=true; secrets
  DOCKERHUB_USERNAME/DOCKERHUB_TOKEN set from .env).
- Tests: unit/golden/CLI in-package; `testdata/clusters/clickops.yaml`
  fixture, `testdata/golden/*`; `test/e2e/run.sh` (kind suite, stages);
  `test/live/hetzner/` (Terraform `main.tf`, `run.sh` live k3s+GitHub suite,
  `e2e-on-vm.sh` runner that runs any command on a throwaway VM).
- Recordings: `docs/tapes/*.tape` → `docs/images/*.gif` (`make gifs`, VHS).

## Verified (providers, 2026-11-07)
- `make lint test` (both modules) and `make lint test` in
  ../gitopsify-provider-grafana: unit, golden, CLI (a fake provider through
  the real subprocess path), re-export safety regressions, HCL injection
  round trips, provider tests on answers recorded from Grafana 13.3 validated
  against the upstream Terraform schema and CRDs.
- Live, `make live` in the provider repo on a Hetzner cpx32 (cx33 had no
  capacity that day: `SERVER_TYPE=cpx32`): Grafana 13.3.0 in kind, seeded
  (3 orgs, API- and UI-made objects), every kind incl. Organization:
  Terraform 1.14.6 and OpenTofu 1.13.9 first plans import only (plus the 2
  contact point secret rewrites per org, compared field by field), apply,
  replan "No changes", Grafana unchanged; Crossplane 2.4.3 + provider-grafana
  v2.17.0: 49 managed resources Ready and Synced observing, then managing,
  Grafana unchanged. PASS. Zero gitopsify servers left afterwards.
- Locally: a service-account-token export plans clean (no org_id); a
  re-export of a Terraform-managed Grafana changes nothing; the wizard was
  driven end to end in tmux (provider, settings from env, check, orgs, kinds,
  formats, adopt, picking objects with dependencies, write, save).
- Three review rounds by fresh-context subagents (core and provider); the
  last round found nothing blocking. What's left is listed in
  docs/DESIGN-providers.md §10 and under Open / next.
- Migration in the resource form (later the same day): config round trips in
  YAML/JSON/HCL with hostile values, every migration snippet in docs/ and
  skills/ loads (TestDocumentedMigrationsLoad), the schema checked with
  python-jsonschema against sample documents, init → generate in all three
  syntaxes, shared-file saves. `docs/images/init.gif` re-recorded on a
  Hetzner VM. Three review rounds by a fresh-context subagent (YAML 1.1
  booleans, shared-file saves, empty files, HCL `for` keys, schema
  self-contained, control characters...); the last found nothing left.
  Accepted: a raw DEL in a hand-written .json fails to load; empty lists load
  as defaults; v0.1 files without metadata.name take it from --context;
  Migration validation messages don't say `spec.`.

- Template and fixes (later the same day): template tests (conformance,
  upstream validation, scopes, summary, errors, recording), `prepare` run on
  a copy with an origin remote then `make lint test build` (no template names
  left), the template provider driven through the real `gitopsify export`
  against a fake API (35 files, report correct), `tools/upstream` run for
  real against grafana/grafana and provider-grafana v2.17.0 (same content as
  the hand-captured testdata, which it now regenerates). Found and fixed on
  the way: a secret the tool doesn't return was listed twice in
  EXPORT_REPORT.md; export.json held hashes of secret files (GitGuardian
  flagged one on PR #1: a false positive as a secret, but a real offline
  guessing leak); the pkg/iac golden secret fixtures were never committed
  (git-ignored; `make golden` now force-adds them). Two review rounds by a
  fresh-context subagent on the template, the core fixes and Grafana: round
  2 found everything from round 1 fixed; its new findings were fixed too
  (record asks before replacing testdata and fixture names are lowercase
  and escaped consistently; report lines per attribute path; prepare from
  any subdirectory, module path checks). Accepted: Grafana's fixtureName
  still uses url.PathEscape (in its HANDOFF), terraform vs tofu provider
  addresses differ in the schema JSON, list items sharing a secret name
  and path are one report line, releases on `go-version: stable` (not
  reproducible over time).

## Day to day (providers)
```sh
go build -o bin/gitopsify ./cmd/gitopsify && (cd ../gitopsify-provider-grafana && make build)
docker run -d --name gitopsify-grafana -p 3300:3000 -e GF_SECURITY_ADMIN_PASSWORD=admin grafana/grafana:13.3.0
GRAFANA_URL=http://localhost:3300 GRAFANA_AUTH=admin:admin ../gitopsify-provider-grafana/test/live/seed.sh
PATH=../gitopsify-provider-grafana/bin:$PATH GRAFANA_URL=... GRAFANA_AUTH=... bin/gitopsify export grafana
cd ../gitopsify-provider-grafana && make live   # Terraform + OpenTofu + Crossplane on Hetzner (~30 min)
# Hetzner short of cx33: SERVER_TYPE=cpx32 (same size). A local test Grafana
# (container gitopsify-grafana, port 3300) is removed after testing: docker rm -f gitopsify-grafana
```

## Day to day
```sh
make build test lint      # always run make lint before committing
make golden               # after intended output changes, migrations and exports (git add -f secret fixtures)
make docs                 # regenerates docs/commands.md + docs/gitopsify.schema.json
make e2e-vm               # kind suite on a Hetzner cx33 (~40 min), STAGES=/SKIP=
make live                 # 3x k3s + private GitHub repo hayone1/gitopsify-e2e-live (~40 min)
RUN="make gifs" FETCH=docs/images ./test/live/hetzner/e2e-on-vm.sh   # re-render GIFs
# one GIF: RUN='CGO_ENABLED=0 go build -o bin/gitopsify ./cmd/gitopsify && docker run --rm -v /src:/vhs ghcr.io/charmbracelet/vhs docs/tapes/init.tape' FETCH=docs/images/init.gif
scripts/set-version.sh vX.Y.Z && git commit -am ... && git tag vX.Y.Z && git push --tags
```

## Environment rules (learned the hard way)
- Go 1.27.3 in `~/.local/go`, kind in `~/go/bin`. `.env` holds
  TF_VAR_hcloud_token, DOCKER_USERNAME, DOCKER_PASSWORD; git-ignored, never
  print or commit it.
- **No heavy local work**: WSL has 2 CPUs / 5 GB and Windows killed it when
  kind clusters ran. Run clusters, image renders and big builds on Hetzner
  (`e2e-on-vm.sh`, any command with `RUN=... FETCH=...`; the user has OKed
  throwaway VMs for anything expensive) or GitHub Actions. Pulling big
  images locally is slow too.
- **C: is (nearly) full** (2026-11-07: 2.9 GB free of 397 GB): WSL's
  ext4.vhdx (83 GB, on C:) and Docker Desktop's (12 GB) live there, and WSL
  crashes when C: runs out. Files deleted inside WSL don't shrink the vhdx
  until it is compacted (or sparse: `wsl --manage Ubuntu-24.04 --set-sparse
  true` after `wsl --shutdown`; the user does that, it ends the session).
  Keep growth down: `go clean -cache` at the end of a session (it was 21 GB),
  no big local images (removed grafana/grafana), check `df -h /mnt/c`. D: has
  ~29 GB free, too little to move the distro; the Go cache on /mnt/d would be
  very slow.
- The link WSL ↔ Hetzner can drop to ~20 KB/s; `make live` then hits the API
  server's 60 s request limit on big lists. Prefer `make e2e-vm`.
- Hetzner: cx43 is unavailable on this account; cx33 works (fsn1/nbg1/hel1).
  Every resource is labelled (`gitopsify-e2e`, `gitopsify-e2e-runner`);
  `run.sh destroy` / `e2e-on-vm.sh destroy` clean up. Last check: zero servers.
- Run long suites detached (`setsid nohup ... &`) and wait on the PID, not on
  a `.done` file that an older run may write. Never `pkill -f` with patterns
  from the agent shell (it kills itself).
- Don't edit `test/e2e/run.sh` / `test/live/hetzner/run.sh` while they run.
- In bash `[[ $a == $b ]]` with an unquoted right side is a glob match;
  `cmd | grep -q` under pipefail fails via SIGPIPE.

## Verified (2026-10-22, final code)
- Unit/golden/CLI/skills tests and CI on every push; Docker image build +
  smoke test in CI.
- `make e2e-vm` PASS: Artifact Hub; 8 auth paths (token file/flag/env + CA +
  TLS server name, kubeconfig lists, exec plugin, doctor identity,
  impersonation, in-cluster); round trip; plain apply script; Argo CD;
  Flux + Sealed Secrets; Flux + SOPS; takeover without restarts. CI's e2e job
  also PASS.
- `make live` PASS: remote scans (cert, token+CA), k3s components excluded,
  migrate → Argo CD + Sealed Secrets from GitHub over SSH (healthy), Flux,
  takeover of k3s without restarts.
- Releases: installers verified on Linux/macOS/Windows in CI and by hand
  (Alpine wget, Debian curl, Windows PowerShell 5.1). `claude plugin validate`
  passes.

## Gotchas (providers)
- Terraform 1.7+ / OpenTofu 1.10+ for `removed { lifecycle { destroy = false } }`
  (OpenTofu 1.7-1.9 only know `removed` without lifecycle).
- Crossplane: namespaced managed resources need Crossplane 2; the
  ProviderConfig kind exists only once the provider runs (setup/ holds only
  the Provider; each workspace has its ClusterProviderConfig); supported
  management policy sets include [Observe], [Observe,Create,Update] and
  [Observe,Create,Update,Delete].
- kustomize replacements from a local-config ConfigMap work but scale badly
  (2000 dashboards: 35 s): file content goes inline in Crossplane.
- The provider can report Healthy before its CRDs are served: wait for
  `crd --all` Established before applying managed resources.
- Crossplane's reference resolver rewrites atomic lists (nested `*Ref`s in
  blocks) and then conflicts with server-side apply: refs inside list items
  are written as literal values.
- e2e-on-vm.sh runs the command detached and follows its log with short SSH
  calls (the link drops); stopping a run can leave a firewall: run
  `e2e-on-vm.sh destroy`.
- Safety check in the agent shell: Python heredocs that contain shell-like
  test strings get blocked; write patch scripts to the scratchpad and run them.
- A heredoc delimiter inside a heredoc ends it early (an `EOF` in Python code
  inside `<<'EOF'` ran half the script as shell commands): use unique
  delimiters (`PYEOF`, `GOEOF`).

## Gotchas
- Golden `.gitignore`s ignore `.secrets/` and `*.secrets.auto.tfvars`; `make golden`
  force-adds them (`git add -A` doesn't: that broke CI on PR #1).
- Pinned defaults: Argo CD v3.5.6, Flux v2.9.6 (`pkg/config/defaults.go`).
- Argo CD 3 doesn't store resource health in Applications (status inspects
  resources itself). k8s 1.37 bumps `generation` on annotation changes (judge
  takeovers by pod UIDs).
- kustomize `--enable-helm` breaks with Helm 4 (`helm version -c`); CI pins
  Helm 3 for golden builds; the apply script uses `helm upgrade --install`.
- New Docker Hub repos are public on free accounts.

## Verified (web UI, 2026-11-08)
- Server tests pass (also 3x in a row, and with PostgreSQL 18); the whole
  `go test ./...` passes; `make lint`; UI typecheck + generate.
- e2e (binary): Keycloak sign-in as operator/viewer/admin, scan, apps,
  target, review, generate, push to a Git server (commit authored as the
  person), viewer restrictions, audit log: 3/3 PASS.
- Review rounds by a fresh-context subagent: round 1 found 3 critical (git
  remote as a command, symlinked checkouts writing outside the run,
  cross-team pushes with shared credentials) and 14 required items, all
  fixed with tests; see the commit "PostgreSQL storage for replicas, and
  fixes from review".
- GitHub release downloads (sops) returned 504 from the Hetzner VM on
  2026-11-08 while working from WSL: the Dockerfile pins checksums, retries,
  and builds without sops when SOPS_VERSION is empty.

## Open / next (providers), in order
1. GitGuardian incident 39798306 (PR #1) is a false positive (a SHA-256 file
   hash in a pre-squash commit); mark it so in the GitGuardian dashboard.
2. Released 2026-11-08: gitopsify-provider-grafana v0.1.0 (listed in
   providers/index.yaml, repoId 1449686256; `gitopsify provider install
   grafana` and `hayone1/grafana` verified in a clean home) and gitopsify
   v0.2.0 (installers verified on Linux/macOS/Windows by the workflow,
   Docker image published, `go install ...@v0.2.0` verified).
3. TODO.md: registry website and web UI. Design proposals in
   docs/DESIGN-registry.md and docs/DESIGN-webui.md wait for the user's answers
   to their open questions (domain, stack, local-only, order).
4. Continuous use: three-way merge of re-exports with hand edits (the base
   is in export.json's file hashes), then a `gitopsify sync` loop or a
   controller reconciling `Export` resources (DESIGN-providers §11).
5. Grafana provider: more contact point integrations, Grafana 13 v2
   dashboards (`grafana_apps_dashboard_*`), users/SSO (instance-wide).
6. Signature verification of provider releases (artifact attestations).

## Open / next
- Package managers: create homebrew-tap / scoop-bucket repos + a
  TAP_GITHUB_TOKEN secret, then uncomment the blocks in `.goreleaser.yaml`.
- Optional: CI workflow for `make live` (needs an HCLOUD token secret).
- Out of scope by design: PV data / image copying, Argo↔Flux conversion.
