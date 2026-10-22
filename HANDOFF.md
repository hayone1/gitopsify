# Handoff: gitopsify

State as of 2026-10-22. Pushed to https://github.com/hayone1/gitopsify
(private for now; everything is written to go public as is).

## What it is
Go CLI (cobra + Charm huh/lipgloss) that turns a running cluster (or a
`kubectl get -A -o yaml` dump via `--from-file`) into a GitOps repo for plain
Kustomize, Argo CD (app-of-apps / applicationset / single) or Flux, and can
check the target, preview the first sync, install Argo CD/Flux and watch it
sync. User docs: `README.md` → `docs/`. Design and rationale:
`docs/DESIGN.md`. Contributor guide: `docs/development.md`.

## Layout
- `cmd/gitopsify` entrypoint; `internal/cli` commands (incl. `doctor`,
  `status`, `skills`, hidden `gen-docs`); `internal/ui` output/prompts
- `pkg/kube`: every connection goes through `kube.Connect(config.Access)`;
  `auth.go` describes the auth method and explains errors per platform
- `pkg/pipeline` stages: discovery → helm → filter → deps → clean →
  portability → group → transform → secrets → order → render
- `pkg/target` check / diff / adopt / status; `pkg/bootstrap` installs Argo CD/Flux
- `skills/` agent skills (embedded via go:embed; also the Claude Code plugin
  and Gemini CLI extension content). `.claude-plugin/`, `gemini-extension.json`,
  `GEMINI.md`, `commands/gitopsify/*.toml` package them
- `install.sh`, `install.ps1`, `.goreleaser.yaml` (archives, deb/rpm/apk/arch,
  completions via `scripts/completions.sh`), `scripts/set-version.sh`
- `testdata/clusters/clickops.yaml` fixture; `testdata/golden/*` snapshots
- `test/e2e/run.sh` kind suite; `test/live/hetzner/` Terraform + live suite

## Day to day
```sh
make build    # bin/gitopsify
make test     # unit + golden + CLI tests (needs kubectl; helm for vendored charts)
make golden   # after an intended output change; also git add -f's .secrets fixtures
make docs     # regenerates docs/commands.md and docs/gitopsify.schema.json (tests enforce)
make e2e      # kind; stages via STAGES=/SKIP=, KEEP=1 keeps clusters (~45 min)
make live     # Hetzner k3s x3 + private GitHub repo; needs TF_VAR_hcloud_token in .env
```
Toolchain: Go 1.27.3 in `~/.local/go`, kind in `~/go/bin`. `.env` (Hetzner
token) is git-ignored; never commit it. Run long suites detached
(`setsid nohup ... &`): an editor crash killed in-process runs once.

## Done in this round
- Cluster access: kubeconfig lists, context, in-cluster, `--server` +
  token/token-file/`$GITOPSIFY_TOKEN`, client certs, CA, insecure, TLS server
  name, proxy, impersonation, timeouts; `--target-*` and
  `environments[].cluster` for targets. oidc/gcp/azure auth providers
  registered; `--no-input` stops exec plugins prompting; actionable errors.
  Tokens can't be written to `gitopsify.yaml` (schema and loader reject them).
- New commands: `doctor`, `status [--wait]`, `bootstrap --wait`, `skills`.
- Edge cases: list retries / expired continue tokens / broken custom APIs
  become warnings; rules `terminating`, `platform-crds`,
  `platform-serviceaccounts`; more distro namespaces and runtime annotations
  (OpenShift, Rancher, GKE, AKS, k0s, Cilium); portability warnings; reserved
  and long app names; long file names; manifest path traversal; failed newer
  Helm revisions; `generate --push` onto existing remote history.
- `--tool none` apply script: Helm apps via `helm upgrade --install` (Helm 3/4),
  `--context/--kubeconfig/--dry-run`, waits for platform apps.
- Grouped `--help`, shell completion (contexts, envs, enums), JSON Schema for
  `gitopsify.yaml`, parallel diff/check.
- Docs rewritten as guides; README lean.
- Installers + GoReleaser packages; release workflow verifies installers on
  Linux/macOS/Windows.
- AI: six skills (migrate, git-remote, cluster-access, secrets, takeover,
  troubleshoot), validated with `claude plugin validate`.

## Verified
RESULTS_PLACEHOLDER

## Gotchas
- Golden trees contain generated `.gitignore`s ignoring `.secrets/`; commit
  those fixtures with `git add -f` (the `golden` target does it).
- Pinned defaults: Argo CD v3.5.6, Flux v2.9.6 (`pkg/config/defaults.go`).
- k8s 1.37 bumps Deployment `generation` on annotation-only changes; judge
  takeovers by pod UIDs, not generations.
- kustomize's `--enable-helm` runs `helm version -c`, which Helm 4 removed: CI
  pins Helm 3 for the golden Kustomize builds; the build test skips those
  under Helm 4.
- Don't edit `test/e2e/run.sh` or `test/live/hetzner/run.sh` while they run
  (bash reads scripts incrementally).
- In bash, `[[ $a == $b ]]` with an unquoted right side is a glob match.
- The plugin/extension version lives in `.claude-plugin/plugin.json` and
  `gemini-extension.json`; bump both with `scripts/set-version.sh` before a tag.

## Open / next
- Make the repository public (`gh repo edit hayone1/gitopsify --visibility public`);
  until then installers need `GITHUB_TOKEN`.
- Package managers: create `homebrew-tap`/`scoop-bucket` repos and a
  `TAP_GITHUB_TOKEN` secret, then uncomment the blocks in `.goreleaser.yaml`.
- Optional: a CI workflow for `make live` (needs an `HCLOUD_TOKEN` secret).
- Out of scope by design: PV data / image copying, Argo↔Flux conversion
  (already-GitOps-managed objects are skipped; `--include-managed` exports them
  as plain objects).
