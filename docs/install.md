# Installing gitopsify

gitopsify is a single binary with no runtime dependencies. A few commands call
other tools when you use them; you can install those later.

## Linux and macOS

```sh
curl -fsSL https://raw.githubusercontent.com/hayone1/gitopsify/main/install.sh | sh
```

The script picks the right build for your system, checks it against the
release's `checksums.txt`, and puts `gitopsify` in `/usr/local/bin` if you can
write there, otherwise in `~/.local/bin` (it tells you if that isn't on your
`PATH`). It never asks for `sudo` unless you choose a directory that needs it.

Options, as environment variables or arguments:

```sh
curl -fsSL https://raw.githubusercontent.com/hayone1/gitopsify/main/install.sh | GITOPSIFY_VERSION=v0.2.0 sh
curl -fsSL https://raw.githubusercontent.com/hayone1/gitopsify/main/install.sh | sh -s -- --dir ~/bin
```

| Variable | Argument | |
|---|---|---|
| `GITOPSIFY_VERSION` | `--version` | A release tag; default the latest |
| `GITOPSIFY_INSTALL_DIR` | `--dir` | Where the binary goes |
| `GITHUB_TOKEN` | | Needed while the repository is private, and helps with GitHub's API rate limits |

Prefer to read scripts before running them? Download it first:
`curl -fsSLO https://raw.githubusercontent.com/hayone1/gitopsify/main/install.sh && less install.sh && sh install.sh`.

## Windows

In PowerShell:

```powershell
irm https://raw.githubusercontent.com/hayone1/gitopsify/main/install.ps1 | iex
```

It installs to `%LOCALAPPDATA%\gitopsify\bin` and adds that to your user
`PATH` (set `GITOPSIFY_NO_MODIFY_PATH=1` to skip that). The same
`GITOPSIFY_VERSION`, `GITOPSIFY_INSTALL_DIR` and `GITHUB_TOKEN` variables
apply.

## Linux packages

Every release has `.deb`, `.rpm`, `.apk` and Arch packages, with shell
completions included:

```sh
# Debian, Ubuntu
curl -fsSLO https://github.com/hayone1/gitopsify/releases/download/v0.1.0/gitopsify_0.1.0_linux_amd64.deb
sudo dpkg -i gitopsify_0.1.0_linux_amd64.deb
# Fedora, RHEL
sudo rpm -i https://github.com/hayone1/gitopsify/releases/download/v0.1.0/gitopsify_0.1.0_linux_amd64.rpm
```

Homebrew, Scoop and winget packages are planned.

## Other ways

- **Archives**: download from the [releases page](https://github.com/hayone1/gitopsify/releases),
  verify with `sha256sum -c checksums.txt --ignore-missing`, and put the binary
  on your `PATH`.
- **Go**: `go install github.com/hayone1/gitopsify/cmd/gitopsify@latest`.
- **From source**: `git clone https://github.com/hayone1/gitopsify && cd gitopsify && make build`
  (the binary is `bin/gitopsify`).

## Shell completion

Packages install completions for bash, zsh and fish. Otherwise:

```sh
gitopsify completion bash > ~/.local/share/bash-completion/completions/gitopsify
gitopsify completion zsh > "${fpath[1]}/_gitopsify"
gitopsify completion fish > ~/.config/fish/completions/gitopsify.fish
gitopsify completion powershell | Out-String | Invoke-Expression
```

Completion knows your kubeconfig contexts, the environments in
`gitopsify.yaml`, and the values of flags such as `--tool` and `--secrets`.

## Tools gitopsify calls

| Tool | Needed for |
|---|---|
| `git` | `--commit`, `--push`, `migrate` |
| `helm` | `diff` renders Helm apps exactly; the plain apply script installs Helm apps |
| `kubectl` (or `kustomize`) | the generated `apply-<env>.sh` of `--tool none` |
| `sops` | `--secrets sops` |

`gitopsify doctor` tells you which of them you have and which you need.

## Upgrading and uninstalling

Run the installer again to upgrade. To uninstall, delete the binary
(`rm "$(command -v gitopsify)"`), or remove the package. gitopsify keeps no
state outside the directories you generate.

## AI agent skills

`gitopsify skills install` copies the bundled agent skills to where Claude
Code, Codex, Gemini CLI, Cursor and GitHub Copilot find them. See
[AI agents](ai-agents.md).
