# Handoff: gitopsify

State as of 2026-10-22, end of day. Repo: https://github.com/hayone1/gitopsify
(private; written to go public as is). Latest release: v0.1.1 (v0.1.0 also
final; v0.1.0-rc.1 prerelease). Everything committed and pushed; CI green.

## What it is
Go CLI (cobra + Charm huh/lipgloss) that turns a running cluster (or a
`kubectl get -A -o yaml` dump via `--from-file`) into a GitOps repo for plain
Kustomize, Argo CD (app-of-apps / applicationset / single) or Flux, and can
check the target, diff the first sync, install Argo CD/Flux, wait for it to
sync, and report status. Ships AI agent skills and a Docker image.

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

## Day to day
```sh
make build test lint      # always run make lint before committing
make golden               # after intended output changes (git add -f .secrets fixtures)
make docs                 # regenerates docs/commands.md + docs/gitopsify.schema.json
make e2e-vm               # kind suite on a Hetzner cx33 (~40 min), STAGES=/SKIP=
make live                 # 3x k3s + private GitHub repo hayone1/gitopsify-e2e-live (~40 min)
RUN="make gifs" FETCH=docs/images ./test/live/hetzner/e2e-on-vm.sh   # re-render GIFs
scripts/set-version.sh vX.Y.Z && git commit -am ... && git tag vX.Y.Z && git push --tags
```

## Environment rules (learned the hard way)
- Go 1.27.3 in `~/.local/go`, kind in `~/go/bin`. `.env` holds
  TF_VAR_hcloud_token, DOCKER_USERNAME, DOCKER_PASSWORD; git-ignored, never
  print or commit it.
- **No heavy local work**: WSL has 2 CPUs / 5 GB and Windows killed it when
  kind clusters ran. Run clusters, image renders and big builds on Hetzner
  (`e2e-on-vm.sh`) or GitHub Actions. Pulling big images locally is slow too.
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

## Gotchas
- Golden `.gitignore`s ignore `.secrets/`; `make golden` force-adds fixtures.
- Pinned defaults: Argo CD v3.5.6, Flux v2.9.6 (`pkg/config/defaults.go`).
- Argo CD 3 doesn't store resource health in Applications (status inspects
  resources itself). k8s 1.37 bumps `generation` on annotation changes (judge
  takeovers by pod UIDs).
- kustomize `--enable-helm` breaks with Helm 4 (`helm version -c`); CI pins
  Helm 3 for golden builds; the apply script uses `helm upgrade --install`.
- New Docker Hub repos are public on free accounts.

## Open / next
- Make the repo public (`gh repo edit hayone1/gitopsify --visibility public`);
  until then installers need `GITHUB_TOKEN`.
- Package managers: create homebrew-tap / scoop-bucket repos + a
  TAP_GITHUB_TOKEN secret, then uncomment the blocks in `.goreleaser.yaml`.
- Optional: CI workflow for `make live` (needs an HCLOUD token secret).
- Out of scope by design: PV data / image copying, Argo↔Flux conversion.
