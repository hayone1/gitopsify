# Handoff: gitopsify

State as of 2026-10-21. One local commit, no remote yet.

## What it is
Go CLI (cobra + Charm huh/lipgloss) that turns a running cluster (or a
`kubectl get -A -o yaml` dump via `--from-file`) into a GitOps repo for plain
Kustomize, Argo CD (app-of-apps / applicationset / single) or Flux. Design and
rationale: `docs/DESIGN.md`. Config reference: `docs/configuration.md`.

## Layout
- `cmd/gitopsify` entrypoint; `internal/cli` commands; `internal/ui` output/prompts
- `pkg/pipeline` runs the stages: discovery → helm → filter → deps → clean →
  group → transform → secrets → order → render
- `pkg/target` check / diff / adopt; `pkg/bootstrap` installs Argo CD/Flux
- `testdata/clusters/clickops.yaml` fixture; `testdata/golden/*` snapshots
- `test/e2e/run.sh` real-cluster test; `test/e2e/githttp` local Git server

## Day to day
```sh
make build    # bin/gitopsify
make test     # unit + golden (needs kubectl; helm for vendored charts)
make golden   # after an intended output change; also git add -f's .secrets fixtures
make e2e      # kind + Argo CD + Flux + sealed-secrets, ~15 min, needs docker/kind/helm/jq
```
Toolchain here: Go 1.27.3 in `~/.local/go` (PATH in `~/.bashrc`), kind in `~/go/bin`.

## Verified
All unit/golden tests pass from a fresh clone. `test/e2e/run.sh` passed:
round trip on the source is a no-op except deliberate Helm drift; Argo CD and
Flux migrations sync fully; sealed Secret unseals. Argo CD takeover of the
source cluster was checked by hand (no pod restarts/deletions).

## Gotchas
- Golden trees contain generated `.gitignore`s ignoring `.secrets/`; commit
  those fixtures with `git add -f` (the `golden` target does it).
- Pinned defaults: Argo CD v3.5.6, Flux v2.9.6 (`pkg/config/defaults.go`).
- k8s 1.37 bumps Deployment `generation` on annotation-only changes; judge
  takeovers by pod UIDs, not generations.
- Argo CD only refreshes app health every ~3 min, so app-of-apps waves look
  stalled briefly.

## Open / next
- Create `github.com/hayone1/gitopsify` and push; CI (`.github/workflows`)
  and GoReleaser (`.goreleaser.yaml`, tag `v*`) are ready.
- Not done: PV data / image copying (out of scope), Argo↔Flux conversion,
  SOPS path not exercised end to end (needs `sops` binary), Artifact Hub
  resolver only unit-tested against a fake server.
