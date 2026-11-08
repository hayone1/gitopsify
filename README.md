# gitopsify

**Turn what runs in a Kubernetes cluster into a GitOps repository.**

Your `test` cluster was built by hand with `kubectl`, Helm and a dashboard or
two. Now you want `uat` and `prod` built from Git by Argo CD or Flux, or you
want Argo CD or Flux to take over the cluster that already exists. gitopsify
reads the live cluster and writes that repository for you:

- **Clean manifests**, without status, runtime metadata, server-side defaults
  or values the cluster allocated.
- **Only what belongs in Git.** System objects, controller-generated objects
  and platform add-ons are left out, and every exclusion comes with a reason.
- **Helm stays Helm.** Releases become charts with their versions and values,
  even though Helm never recorded where the chart came from.
- **The right order.** CRDs before custom resources, operators before what they
  serve: Argo CD sync waves or Flux `dependsOn`.
- **No plain-text Secrets in Git.** Sealed Secrets, SOPS, External Secrets, or
  local files you apply yourself.
- **Every environment.** Overlays per environment or a repository per
  environment, with transforms for hostnames, registries, storage classes and
  more.
- **All the way, if you want.** Check the target, preview the first sync,
  install Argo CD or Flux and watch it sync.

```text
 live cluster ──► gitopsify ──► Git repository ──► Argo CD / Flux ──► uat, prod, ...
```

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/hayone1/gitopsify/main/install.sh | sh
```

Windows: `irm https://raw.githubusercontent.com/hayone1/gitopsify/main/install.ps1 | iex`.
Docker: `docker run --rm -v ~/.kube:/home/gitopsify/.kube:ro hayone/gitopsify scan`.
Debian, RPM, Alpine and Arch packages, archives and `go install` are in
[docs/install.md](docs/install.md).

## Try it

![gitopsify scan](docs/images/scan.gif)

```sh
gitopsify doctor                         # can gitopsify reach your cluster?
gitopsify scan                           # what would be migrated, and what is left out
gitopsify generate --tool argocd --env uat=uat-cluster \
  --repo-url https://github.com/acme/platform-gitops.git
```

Then read `gitops/MIGRATION_REPORT.md`, push the repository, and:

```sh
gitopsify check --env uat                # is the target ready?
gitopsify diff --env uat                 # what will the first sync change?
gitopsify bootstrap --env uat --wait     # install Argo CD and let it sync
```

[Getting started](docs/getting-started.md) walks through a whole migration, and
[examples](docs/examples.md) has recipes for most situations.

## Beyond Kubernetes: Grafana and other tools

Providers read other tools and `gitopsify export` writes Terraform or Crossplane
that manages what they hold: a Grafana with three organizations becomes three
Terraform workspaces (or Kustomizations) whose first plan imports everything
and changes nothing.

```sh
gitopsify provider install grafana
gitopsify export                         # asks; or: gitopsify export grafana --no-input
```

More in [docs/providers.md](docs/providers.md); to support another tool, see
[docs/writing-providers.md](docs/writing-providers.md).

## In the browser, for you or your team

```sh
gitopsify ui                             # on your machine: your kubeconfig, signed in
helm install gitopsify oci://ghcr.io/hayone1/charts/gitopsify -f values.yaml   # for a team
```

The web UI scans, picks apps, configures and pushes migrations, and exports
tools, with single sign-on (Keycloak, Okta, Entra ID, Dex...), roles per
cluster and tool, an audit log, and PostgreSQL for several replicas. More in
[docs/web-ui.md](docs/web-ui.md).

## With an AI coding agent

gitopsify ships skills that guide Claude Code, Codex, Gemini CLI, Cursor or
GitHub Copilot through a migration with you, including the parts gitopsify
leaves to people: creating the repository on GitHub, GitLab, Azure DevOps or
Bitbucket, deploy keys, installing controllers and loading Secrets.

```sh
gitopsify skills install                 # Codex, Gemini CLI, Cursor, Copilot, Claude Code
```

In Claude Code: `/plugin marketplace add hayone1/gitopsify`, then
`/plugin install gitopsify@gitopsify`. In Gemini CLI:
`gemini extensions install https://github.com/hayone1/gitopsify`.
More in [docs/ai-agents.md](docs/ai-agents.md).

## Documentation

| | |
|---|---|
| [Getting started](docs/getting-started.md) | A first migration, step by step |
| [Examples](docs/examples.md) | Recipes: scopes, tools, environments, Secrets, access, CI, Docker |
| [How it works](docs/concepts.md) | Cleaning, exclusions, apps, ordering, output |
| [Cluster access](docs/authentication.md) | EKS, GKE, AKS, OpenShift, k3s, tokens, CI |
| [Helm releases](docs/helm.md) | Charts, repositories, drift |
| [Secrets](docs/secrets.md) | Keeping credentials out of Git |
| [Environments and transforms](docs/environments.md) | uat, prod and what differs between them |
| [Argo CD, Flux or plain Kustomize](docs/gitops-tools.md) | What gets generated and how to bootstrap |
| [Taking over a running cluster](docs/takeover.md) | GitOps for the cluster you already have |
| [Grafana and other tools](docs/providers.md) | Terraform and Crossplane from a tool's live state |
| [Writing a provider](docs/writing-providers.md) | Support another tool |
| [Web UI](docs/web-ui.md) | `gitopsify ui`, and the team server: sign-in, roles, audit, Helm, high availability |
| [AI agents](docs/ai-agents.md) | Skills and plugins |
| [Troubleshooting](docs/troubleshooting.md) | Common problems and answers |
| [Configuration reference](docs/configuration.md) | Every field of `gitopsify.yaml` |
| [Command reference](docs/commands.md) | Every command and flag |
| [Design](docs/DESIGN.md) · [Providers design](docs/DESIGN-providers.md) · [Web UI design](docs/DESIGN-webui.md) · [Development](docs/development.md) | For contributors |

Example output for every mode, generated from [a real cluster](testdata/clusters/clickops.yaml),
is in [testdata/golden](testdata/golden).

## What it doesn't do

gitopsify moves configuration, not data: persistent volume contents and
container images stay where they are (use Velero or `crane transfer-pvc`, and
registry replication). Objects already managed by Argo CD, Flux or Config Sync
are left alone, since their source is already Git.

## License

[Apache 2.0](LICENSE)
