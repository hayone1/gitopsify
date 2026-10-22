# Development

## Layout

| Path | |
|---|---|
| `cmd/gitopsify` | Entry point |
| `internal/cli` | Commands, flags, `--help` sections, doctor, status, skills, docs generator |
| `internal/ui` | Terminal output and prompts |
| `pkg/pipeline` | Runs the stages: discovery → Helm → filter → dependencies → clean → portability → group → transform → Secrets → order → render |
| `pkg/kube` | Cluster connections (every authentication method), error explanations, server-side apply |
| `pkg/target` | `check`, `diff`, `adopt`, `status` |
| `pkg/bootstrap` | Installs Argo CD or Flux |
| `pkg/render` | Writes the repository from the inventory |
| `skills/` | Agent skills, embedded in the binary; also the Claude Code plugin and Gemini CLI extension content |
| `testdata/clusters/clickops.yaml` | A real cluster dump the golden tests run on |
| `testdata/golden/*` | Expected output of every mode |
| `test/e2e` | kind end-to-end test |
| `test/live/hetzner` | Live test on remote k3s clusters and GitHub |

[DESIGN.md](DESIGN.md) explains why things are the way they are.

## Everyday commands

```sh
make build     # bin/gitopsify
make test      # unit, golden and CLI tests; needs kubectl and helm for the Kustomize build checks
make lint      # go vet and gofmt
make golden    # regenerate testdata/golden after an intended output change
make docs      # regenerate docs/commands.md after changing a command or flag
make e2e       # kind clusters: auth, round trip, plain, Argo CD, Flux, SOPS, takeover (~45 min)
make live      # Hetzner k3s clusters and a private GitHub repository (~30 min)
```

The golden trees contain generated `.gitignore` files that ignore `.secrets/`;
`make golden` adds those fixtures with `git add -f`. Review golden diffs like
code: they are the product.

`TestGoldenKustomizationsBuild` builds every generated Kustomization.
kustomize's Helm support calls `helm version -c`, which Helm 4 removed, so with
Helm 4 those few builds are skipped; CI pins Helm 3 for them.

## End-to-end tests

`test/e2e/run.sh` needs docker, kind, kubectl, helm, jq, git, curl and go.
Select stages with `STAGES="auth argocd"` or `SKIP="sops takeover"`; `KEEP=1`
keeps the clusters. It downloads `sops` and `age` when missing.

`test/live/hetzner/run.sh` needs terraform, gh (with `repo` scope) and
`TF_VAR_hcloud_token` (it reads `.env` at the repository root, which is
git-ignored). It creates three `cx33` servers with k3s, reachable only from
your IP, and a private `<you>/gitopsify-e2e-live` repository with per-run
deploy keys and branches. Everything is destroyed at the end, also on failure;
`./run.sh destroy` cleans up after an interrupted run, including anything
labelled `gitopsify-e2e` that Terraform lost track of. A run costs a few cents.

## Releasing

1. `scripts/set-version.sh v0.2.0` (the Claude Code plugin and Gemini CLI
   extension carry the version; a test keeps them equal), commit.
2. `git tag v0.2.0 && git push --tags`.

The release workflow runs GoReleaser: archives for Linux, macOS and Windows on
amd64 and arm64, `.deb`/`.rpm`/`.apk`/Arch packages, shell completions and
`checksums.txt`, which `install.sh` and `install.ps1` verify. Homebrew, Scoop
and winget need repositories of their own; the configuration for them is
commented in `.goreleaser.yaml`. Try a release locally with
`goreleaser release --snapshot --clean`.

## Pinned versions

Argo CD v3.5.6 and Flux v2.9.6 are the defaults `bootstrap` installs
(`pkg/config/defaults.go`). After bumping them, run the e2e test.
