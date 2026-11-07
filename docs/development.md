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
| `sdk/` | The provider protocol and Go SDK: its own module (`github.com/hayone1/gitopsify/sdk`), standard library only; `sdk/sdktest` tests providers |
| `pkg/provider` | Finds, installs (GitHub releases, checksums) and runs providers; the index |
| `pkg/iac` | Turns a provider's inventory into Terraform and Crossplane: workspaces, stable names, adoption, secrets, safe re-exports |
| `providers/index.yaml` | The provider index (embedded in the binary) |
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
make e2e       # kind clusters on this machine: auth, round trip, plain, Argo CD, Flux, SOPS, takeover
make e2e-vm    # the same suite on a throwaway Hetzner machine (needs TF_VAR_hcloud_token)
make live      # Hetzner k3s clusters and a private GitHub repository (~40 min)
```

The golden trees contain generated `.gitignore` files that ignore `.secrets/`;
`make golden` adds those fixtures with `git add -f`. Review golden diffs like
code: they are the product.

`TestGoldenKustomizationsBuild` builds every generated Kustomization.
kustomize's Helm support calls `helm version -c`, which Helm 4 removed, so with
Helm 4 those few builds are skipped; CI pins Helm 3 for them.

## End-to-end tests

`test/e2e/run.sh` needs docker, kind, kubectl, helm, jq, git, curl and go, and
more CPU and memory than a laptop or WSL comfortably gives it (two clusters,
Argo CD, Flux). Prefer `make e2e-vm`, which runs it on a Hetzner `cx33`
created for the run and destroyed afterwards (`LOCATIONS`, `SERVER_TYPE` to
choose; `test/live/hetzner/e2e-on-vm.sh destroy` cleans up after an
interruption). Select stages with `STAGES="auth argocd"` or `SKIP="sops
takeover"`; `KEEP=1` keeps the clusters. It downloads `sops` and `age` when
missing. The CI workflow runs it too, on demand and weekly.

`test/live/hetzner/run.sh` needs terraform, gh (with `repo` scope) and
`TF_VAR_hcloud_token` (it reads `.env` at the repository root, which is
git-ignored). It creates three `cx33` servers with k3s, reachable only from
your IP, and a private `<you>/gitopsify-e2e-live` repository with per-run
deploy keys and branches. Everything is destroyed at the end, also on failure;
`./run.sh destroy` cleans up after an interrupted run, including anything
labelled `gitopsify-e2e` that Terraform lost track of. `STAGES="argocd"` runs
some of argocd, flux and takeover on fewer servers; `KEEP=1` keeps them; on
failure, reports, scans and diffs are kept in `.state/last-run`. Its cluster
traffic goes through your connection, so on a slow link prefer `make e2e-vm`.
A run costs a few cents.

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

## The SDK module

`sdk/` is a separate Go module so providers don't inherit gitopsify's
dependencies. During development the main `go.mod` points at it with
`replace github.com/hayone1/gitopsify/sdk => ./sdk`, and `make test lint`
cover both modules. A release can't keep the replace directive (`go install
...@vX.Y.Z` refuses it; the release workflow checks), so before tagging the CLI:

```sh
git tag sdk/v0.1.0 && git push origin sdk/v0.1.0       # if sdk/ changed since its last tag
go mod edit -dropreplace github.com/hayone1/gitopsify/sdk
go get github.com/hayone1/gitopsify/sdk@v0.1.0 && go mod tidy
git commit -am "Require sdk v0.1.0"                     # then tag vX.Y.Z as usual
go mod edit -replace github.com/hayone1/gitopsify/sdk=./sdk   # back to development
```

Protocol changes are additive within `v1alpha1`; anything else needs a new
protocol version that gitopsify and providers both list.
