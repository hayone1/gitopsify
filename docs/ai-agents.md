# Migrating with an AI coding agent

gitopsify does the deterministic part of a migration. The rest needs judgement,
credentials and other tools: choosing a setup, creating the repository on your
Git host, installing controllers in the target, loading Secrets into a secret
manager, reading the report and checking the result. gitopsify ships **agent
skills** so an AI coding agent can do that part with you, the same way every
time.

## The skills

| Skill | The agent helps you... |
|---|---|
| `gitopsify-migrate` | run a whole migration: agree on a setup (recommended defaults, and when to choose otherwise), check access, scan, generate, work through the report, push, prepare the target, bootstrap, verify |
| `gitopsify-git-remote` | create the repository on GitHub, GitLab, Azure DevOps, Bitbucket, Gitea/Forgejo, CodeCommit or a self-hosted server, push it, protect `main`, and set up a read-only deploy key or token |
| `gitopsify-cluster-access` | connect to EKS, GKE, AKS, OpenShift, Rancher, k3s and others, set up least-privilege ServiceAccounts, and fix connection errors |
| `gitopsify-secrets` | choose a Secret strategy and carry it out: install Sealed Secrets, set up SOPS keys, load values into AWS, Google, Azure or Vault |
| `gitopsify-takeover` | let Argo CD or Flux take over a running cluster with a clean diff and no restarts |
| `gitopsify-troubleshoot` | fix apps that don't sync after bootstrap |

The skills are opinionated, recommending a default for each decision, while
still covering the alternatives. They tell the agent never to change a cluster,
create repositories or keys without your explicit yes, never to print Secret
values, and never to touch the source cluster.

## Installing

**Any agent**, from the gitopsify binary (the skills match its version):

```sh
gitopsify skills install                          # ~/.agents/skills and ~/.claude/skills
gitopsify skills install --agent copilot --project   # .github/skills in this repository
gitopsify skills install --dir ~/my-agent/skills
gitopsify skills list
```

| `--agent` | Directory (user / `--project`) | Read by |
|---|---|---|
| `agents` (also `codex`, `opencode`, `antigravity`) | `~/.agents/skills` / `.agents/skills` | Codex, Gemini CLI, Cursor, GitHub Copilot and others |
| `claude` | `~/.claude/skills` / `.claude/skills` | Claude Code (and Cursor, Copilot) |
| `copilot` | `~/.copilot/skills` / `.github/skills` | GitHub Copilot |
| `cursor` | `~/.cursor/skills` / `.cursor/skills` | Cursor |
| `gemini` | `~/.gemini/skills` / `.gemini/skills` | Gemini CLI |

Installing again replaces the gitopsify skills and leaves your other skills
alone.

**Claude Code**, as a plugin:

```text
/plugin marketplace add hayone1/gitopsify
/plugin install gitopsify@gitopsify
```

**Gemini CLI**, as an extension, which adds `/gitopsify:migrate`,
`/gitopsify:review`, `/gitopsify:remote` and `/gitopsify:doctor` commands:

```sh
gemini extensions install https://github.com/hayone1/gitopsify
```

## Using them

Start a new session in an empty working directory and say what you want:

> Move the shop and payments namespaces from the test cluster to uat with Argo CD.
> Our repositories are on Azure DevOps in the Platform project.

The agent asks what it can't find out, proposes a setup, runs `gitopsify
doctor` and `scan`, and walks you through the rest. Skills are also available
by name, for example `/gitopsify:gitopsify-git-remote` in Claude Code.

## Writing your own

The skills are plain Markdown in [skills/](../skills), in the open Agent Skills
format. Copy one to your agent's skills directory and adapt it to your
organization: your Git host, your secret manager, your naming rules.
