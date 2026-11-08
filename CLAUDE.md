# gitopsify

Go CLI that turns a running Kubernetes cluster into a GitOps repository (Argo
CD, Flux, Kustomize), and, through providers, other tools' state into Terraform
or Crossplane. `docs/DESIGN.md` and `docs/development.md` have the details; this
file is the short version for agents working on the code.

## Every session

1. **Start:** read [HANDOFF.md](HANDOFF.md) with the `session-handoff` skill
   (`.claude/skills/session-handoff`). It says what the last session did, what is
   open and how the test environment is set up.
2. **End:** update HANDOFF.md before you stop, even when the work is unfinished.
3. Before calling a change done, have a fresh-context reviewer subagent review
   it (for example `agent-skills:code-reviewer`), fix what it finds, and record
   what can't be fixed.

## Commands

- `make build test lint` (always `make lint` before committing)
- `make golden` after intended output changes; `make docs` after changing a
  command, a flag or the configuration
- `make e2e-vm`, `make live`: real clusters on Hetzner (see HANDOFF.md first)

## Rules

- No heavy local work (WSL: 2 CPUs, 5 GB): clusters, recordings and big
  builds run on a throwaway Hetzner VM (`test/live/hetzner/e2e-on-vm.sh`,
  `RUN=... FETCH=...`) or in GitHub Actions.
- WSL's disk lives on a nearly full C: and WSL crashes when it fills: no big
  local images, check `df -h /mnt/c`, and run `go clean -cache` before you
  stop.
- `.env` holds credentials (git-ignored): never print or commit it.
- The `skills/` directory is user-facing (shipped in the binary and the
  plugin); agent instructions for maintainers live here and in `.claude/`.
