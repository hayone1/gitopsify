---
name: gitopsify-migrate
description: Move what runs in a Kubernetes cluster into a GitOps repository for Argo CD, Flux or plain Kustomize with the gitopsify CLI, end to end. Use when someone wants to gitopsify a cluster, put ClickOps or helm-installed apps into Git, copy an environment (test to uat or prod), rebuild a cluster from Git, or have Argo CD or Flux take over a running cluster. Covers the work around the CLI too - choosing the setup, creating and pushing the Git repository, preparing the target cluster, Secrets, bootstrap and verification.
---

# Migrate a cluster to GitOps with gitopsify

gitopsify reads a running cluster and writes a GitOps repository: clean
manifests, Helm releases kept as charts, ordered apps, Secrets kept out of Git
in plain text, and Argo CD or Flux configuration. It can also check the target
cluster, preview the first sync and install Argo CD or Flux.

You drive the migration with the person. The CLI does the deterministic work;
you do what needs judgement, credentials or other tools: deciding the setup,
creating the remote repository, installing prerequisites, handling Secrets,
reviewing the report and verifying the result.

## Ground rules

- **Read before you write.** `scan`, `generate`, `check`, `diff`, `status` and
  `doctor` never change a cluster. `bootstrap`, `migrate`, `adopt`, `kubectl
  apply` and anything that creates repositories or keys do: say exactly what
  will change and get a clear yes first, every time.
- **Never touch the source cluster.** gitopsify only reads it. Don't "clean
  up" the source; it is the rollback.
- **Secrets stay secret.** Never print Secret values, never paste them into
  chat, never commit `.secrets/` or a plain Secret. Prefer sealing or a secret
  manager; delete local copies when done.
- **Least privilege.** Read-only access to the source; deploy keys or
  read-only tokens scoped to one repository for the GitOps tool.
- **One environment at a time**, starting with the least important one.
- When a step needs something only the person can provide (a login, an
  approval, a password manager), stop and ask; don't work around it.

## 1. Make sure the tools are there

```sh
gitopsify version || curl -fsSL https://raw.githubusercontent.com/hayone1/gitopsify/main/install.sh | sh
```

`git` is needed for pushing; `helm` makes `diff` exact for Helm apps;
`kubectl` runs the plain apply script; `sops`/`age` or `kubeseal` only for
those Secret strategies.

## 2. Agree on the setup

Ask only what you can't find out, in one short round, and offer the
recommendation. Look things up first: `kubectl config get-contexts`, `kubectl
get ns`, `helm list -A`, whether Argo CD (`kubectl get ns argocd`) or Flux
(`kubectl get ns flux-system`) already runs in the target, which Git host the
team uses (`git remote -v` in their other repos).

| Decision | Recommended | Alternatives and when |
|---|---|---|
| What to migrate | The namespaces of one team or product (`-n shop -n payments`) | The whole cluster (DR, rebuilds); one app and what it needs (`-r deployment/shop/web`); a label (`-l app.kubernetes.io/part-of=shop`) |
| New cluster or takeover | New environment (uat/prod built from Git) | In-place takeover of the same cluster: see the `gitopsify-takeover` skill |
| GitOps tool | The one the team already runs; otherwise Argo CD for teams who want a UI, Flux for platform teams who prefer Git and CLI | `--tool none`: clean Kustomize apps for any other tool (Rancher Fleet, Config Sync, their own pipelines) |
| Argo CD pattern | `app-of-apps` | `applicationset` (many similar apps, central control), `single` (tiny clusters) |
| Environments and layout | `folder-per-env`: one repository, `apps/<app>/base` + `envs/<env>` | `repo-per-env` when environments have different owners or access rules |
| Secrets | `sealed-secrets` (works everywhere, no external service) | `external-secrets` when a cloud secret manager or Vault exists; `sops` with Flux teams that already use age/KMS; `local` to create them by hand; `skip` to recreate them yourself. See the `gitopsify-secrets` skill |
| Git hosting | Wherever the team's code lives, private repository | Any host; the `gitopsify-git-remote` skill covers GitHub, GitLab, Azure DevOps, Bitbucket, Gitea/Forgejo and CodeCommit |

Also ask what differs per environment: hostnames, image registries, storage
and ingress classes, replica counts, cloud identities. Each becomes a
transform (see [references/config-recipes.md](references/config-recipes.md)).

## 3. Check access

```sh
gitopsify doctor --context <source> --env uat=<target-context> --tool argocd --repo-url <url>
```

Fix every ✗ before going on. Connection and permission problems are covered by
the `gitopsify-cluster-access` skill. A missing remote repository is expected
at this point; it is created in step 6.

## 4. Describe the migration

Write `gitopsify.yaml` in a working directory (not inside the output), either
with `gitopsify init --no-input <flags>` or by hand. It is the record of every
decision and makes the run repeatable; show it to the person.

```yaml
apiVersion: gitopsify.io/v1alpha1
kind: Migration
source: {context: test}
scope: {namespaces: [shop, payments]}
secrets: {strategy: sealed-secrets}
output:
  tool: argocd
  repo: {url: https://github.com/acme/platform-gitops.git}
environments:
  - name: uat
    cluster: {context: uat}
    transforms:
      - {type: replace, from: test.acme.io, to: uat.acme.io}
```

## 5. Scan, then refine

```sh
gitopsify scan --show-excluded
gitopsify scan --format yaml > scan.yaml   # for a closer look
```

Go through the result with the person:

- **Apps and waves**: does the grouping match how they think of their apps?
  Adjust with `grouping` and `ordering` (`--wave`, `--depends-on`).
- **Helm releases**: each should show a repository. When it says `vendor` or
  `manifests`, find the chart's real repository (the chart's `home`/`sources`,
  Artifact Hub, the team's docs) and set `helm.repositories.<chart>`.
- **Exclusions**: every excluded object has a rule and a reason. Explain any
  that surprise them; switch a rule off only for a reason.
- **Warnings**: drifted Helm objects, values that look like credentials,
  references to missing objects, workloads pinned to nodes.

Repeat until it looks right. Changing `gitopsify.yaml` and re-running is cheap.

## 6. Generate and review

```sh
gitopsify generate
```

Read `<output>/MIGRATION_REPORT.md` completely and work through it with
[references/report-warnings.md](references/report-warnings.md). Fix what the
configuration can fix, and turn the rest into a short, explicit list of manual
tasks for the person. Skim a few generated apps to confirm they look like
something a person would have written.

Then create the remote repository and push (gitopsify only pushes to a remote
that exists, with the user's own Git credentials). Follow the
`gitopsify-git-remote` skill: create the repository on their host, push with
`gitopsify generate --push` or `git push`, protect the default branch, and
create a read-only credential for the GitOps tool.

## 7. Prepare the target

```sh
gitopsify check --env uat
gitopsify diff --env uat
```

`check` errors must be fixed first. Typical work for you here, each with the
person's go-ahead:

- install what the report says is missing: an operator whose CRDs the apps
  need, an ingress controller, a CSI driver, the sealed-secrets controller or
  External Secrets Operator;
- map StorageClasses and IngressClasses that don't exist there
  (`--storage-class standard=gp3`) or create them;
- create the Secrets the report lists (the `gitopsify-secrets` skill);
- for a takeover, make `diff` show only changes everyone expects.

## 8. Hand over to Argo CD or Flux

If the target has no GitOps tool yet:

```sh
gitopsify bootstrap --env uat --ssh-key deploy-key --known-hosts known_hosts --wait
# or: --git-token "$TOKEN" for HTTPS
```

If the organization already runs Argo CD or Flux, don't install a second one:
register the repository with the existing installation and apply only the
root object (`argocd/<env>/root.yaml`, or the `clusters/<env>/flux-system`
Kustomization) after the person agrees. For a hub Argo CD managing other
clusters, register the cluster (`argocd cluster add`) and set
`environments[].cluster.name` before generating.

## 9. Verify

```sh
gitopsify status --env uat --wait
kubectl --context uat get pods -A | grep -v Running
```

Then check what users would notice: ingress hostnames answer, jobs ran,
volumes bound. Compare replica counts and images with the source. When
something doesn't sync, use the `gitopsify-troubleshoot` skill.

## 10. Finish

- Commit `MIGRATION_REPORT.md` and the configuration with the repository, and
  open a pull request (or write a summary) that lists the manual tasks left.
- Delete `.secrets/` and any key files from disk once they are loaded;
  rotate credentials that passed through a laptop.
- Suggest a guardrail: CI that builds every Kustomization, and
  `gitopsify diff --env <env> --exit-code` on a schedule to catch drift.
- Next environment: add it to `environments`, regenerate, repeat 7-9.

## What gitopsify leaves to you on purpose

| Task | Why not the CLI | How |
|---|---|---|
| Create the remote repository, protect branches, add deploy keys | Every host differs, and it needs org permissions | `gitopsify-git-remote` skill |
| Install operators, CSI drivers, ingress controllers in the target | Platform choices, often owned by another team | Their Helm charts or the platform's add-on mechanism |
| Load Secret values into Vault or a cloud secret manager | Needs secret-manager credentials gitopsify never holds | `gitopsify-secrets` skill |
| Copy persistent volume data and images | Out of scope: data moves with Velero or `crane transfer-pvc`, images with registry replication | Plan it with the person; do it before cutover |
| DNS, certificates, load balancer IPs, firewall rules | Outside Kubernetes | Note them as cutover tasks |
| Enable pruning after a takeover | A judgement call once the first syncs are proven | `gitopsify-takeover` skill |

## Commands

| Command | Use |
|---|---|
| `gitopsify doctor` | Can everything be reached, with enough permissions? |
| `gitopsify init` | Write `gitopsify.yaml` |
| `gitopsify scan` | What would be migrated, and what is left out and why |
| `gitopsify rules` | The built-in exclusion rules |
| `gitopsify generate [--push]` | Write (and push) the repository |
| `gitopsify check --env E` | Preflight the target |
| `gitopsify diff --env E [--exit-code]` | What the first sync would change |
| `gitopsify adopt --env E` | Retire Helm release records before a takeover |
| `gitopsify bootstrap --env E [--wait]` | Install Argo CD or Flux and apply the root object |
| `gitopsify status --env E [--wait]` | Sync and health of every app |
| `gitopsify migrate` | generate, push, check and bootstrap in one go, for repeat runs |

Every command reads `./gitopsify.yaml`, accepts `--help`, and takes
`--no-input` for unattended use. The full documentation is at
https://github.com/hayone1/gitopsify/tree/main/docs.
