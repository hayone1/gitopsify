# Keeping the repository up to date on a schedule

A migration is rarely a one-off. The `test` cluster keeps changing by hand
after the first export, or someone runs `kubectl edit` in a cluster Argo CD
manages. Run gitopsify on a schedule and let it open a pull request when the
cluster and Git differ. Reviewers merge what should be kept, and the GitOps
tool rolls it out.

gitopsify has no controller for this. A CronJob or CI job runs the same
commands you would, and is built from these pieces:

| | |
|---|---|
| `generate --push --push-branch gitopsify/sync` | regenerates on top of the branch Argo CD or Flux follows and pushes to a branch of its own, only when something changed |
| `generate --exit-code` | exits 2 when files changed (0 when not), so the job knows whether to open a pull request |
| `generate --dry-run --exit-code` | compares with the output directory as it is (a checkout you keep), without writing: `+` new, `~` changed, `-` removed, `=` kept. In an empty directory everything is new |
| `GITOPSIFY_PUSH_TOKEN` | pushes over HTTPS with a token, without a credential helper (`GITOPSIFY_PUSH_USERNAME`, default `x-access-token`). It goes only to the host of the configured repository, so keep the configuration where only trusted people change it |
| `run start --publish --branch ...` | the same on a [gitopsify server](web-ui.md), which holds the cluster and Git credentials |

Re-running gives the same files when nothing changed: objects keep their file
names, apps their order, and sealed or SOPS-encrypted Secrets keep their
ciphertext while the Secret doesn't change. The `gitopsify.yaml` the
repository keeps leaves out what belongs to one machine (kubeconfig paths,
`--in-cluster`, the output directory). An unchanged cluster therefore pushes
nothing and opens no pull request. Two things do change files once: a new
gitopsify version (it is named in `gitopsify.yaml` and the README; pin the
version the job installs), and a new sealing key (the sealed-secrets
controller makes one every 30 days, and gitopsify seals with the newest).

## How the branch behaves

`--push-branch` (and `--branch` on the server) pushes the regenerated output,
committed on top of `--revision`, to its own branch:

- **Nothing differs from `--revision`:** nothing is pushed and the exit code
  is 0. An earlier pull request whose changes were merged, or undone in the
  cluster, stays open until you close it or delete the branch.
- **Something differs:** the branch is created, or replaced when every commit
  on it was made by gitopsify and not changed since (each gitopsify commit
  ends with `Generated-by: gitopsify`; an amended one has another
  committer). One pull request then follows the cluster, as Renovate's do.
  Only branches under `gitopsify/` are ever replaced: any other branch is
  only moved forward, so a typo can't overwrite a branch Argo CD follows.
- **Someone pushed or amended a commit on the branch:** gitopsify stops
  rather than overwrite it. Merge the pull request, or delete the branch.
- **The branch already holds the same files:** nothing is pushed, so the pull
  request isn't touched.

## A Kubernetes CronJob

Running in the source cluster, with a ServiceAccount that may read everything
(see [authentication](authentication.md)). The migration comes from a
ConfigMap: start from the `gitopsify.yaml` in your repository. Each run starts
from an empty directory, so the branch is checked out fresh.

```yaml
apiVersion: v1
kind: ServiceAccount
metadata: {name: gitopsify-reader, namespace: gitopsify}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: gitopsify-reader}
rules:
  - {apiGroups: ["*"], resources: ["*"], verbs: [get, list]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: gitopsify-reader}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: gitopsify-reader}
subjects: [{kind: ServiceAccount, name: gitopsify-reader, namespace: gitopsify}]
---
apiVersion: batch/v1
kind: CronJob
metadata: {name: gitopsify-sync, namespace: gitopsify}
spec:
  schedule: "0 6 * * 1-5"
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      backoffLimit: 0
      template:
        spec:
          serviceAccountName: gitopsify-reader
          restartPolicy: Never
          volumes:
            - {name: work, emptyDir: {}}
            - {name: migration, configMap: {name: gitopsify-migration}}
          initContainers:
            - name: generate
              image: docker.io/hayone/gitopsify:latest   # pin a release in production
              command: [sh, -c]
              args:
                - |
                  gitopsify generate -c /migration/gitopsify.yaml -o /work/gitops --in-cluster --no-input \
                    --push --push-branch gitopsify/sync --message "gitopsify: changes in the test cluster" --exit-code
                  code=$?; echo "$code" > /work/code; [ "$code" = 0 ] || [ "$code" = 2 ]
              env:
                - name: GITOPSIFY_PUSH_TOKEN
                  valueFrom: {secretKeyRef: {name: gitopsify-git, key: token}}
              volumeMounts: [{name: work, mountPath: /work}, {name: migration, mountPath: /migration}]
          containers:
            - name: pull-request
              image: curlimages/curl:8.23.0
              command: [sh, -c]
              args:
                - |
                  [ "$(cat /work/code)" = 2 ] || { echo "no changes"; exit 0; }
                  status=$(curl -sS -o /tmp/out -w '%{http_code}' -X POST \
                    -H "Authorization: Bearer $GITHUB_TOKEN" -H "Accept: application/vnd.github+json" \
                    https://api.github.com/repos/acme/gitops/pulls \
                    -d '{"title":"gitopsify: changes in the test cluster","head":"gitopsify/sync","base":"main"}')
                  # 422: the pull request is already open, and now has the new commit.
                  [ "$status" = 201 ] || [ "$status" = 422 ] || { cat /tmp/out; exit 1; }
              env:
                - name: GITHUB_TOKEN
                  valueFrom: {secretKeyRef: {name: gitopsify-git, key: token}}
              volumeMounts: [{name: work, mountPath: /work}]
```

For GitLab, `POST /api/v4/projects/<id>/merge_requests` with
`source_branch`/`target_branch` does the same (409 when one is open). Azure
DevOps, Bitbucket and Gitea have equivalent endpoints.

## A scheduled CI job

In the repository itself, with `gh` (GitHub Actions; other CI systems look
the same). Allow GitHub Actions to create pull requests (Settings, Actions,
General), and know that pull requests opened with `GITHUB_TOKEN` don't start
other workflows: use a GitHub App or a token of your own if checks must run on
them.

```yaml
on:
  schedule: [{cron: "0 6 * * 1-5"}]
  workflow_dispatch:
permissions: {contents: write, pull-requests: write}
jobs:
  sync:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: curl -fsSL https://raw.githubusercontent.com/hayone1/gitopsify/main/install.sh | GITOPSIFY_VERSION=v0.3.0 sh
      - name: Regenerate
        # actions/checkout left its credentials in the checkout: git pushes with them.
        env:
          KUBECONFIG_DATA: ${{ secrets.TEST_KUBECONFIG }}
          GH_TOKEN: ${{ github.token }}
        run: |
          echo "$KUBECONFIG_DATA" > "$RUNNER_TEMP/kubeconfig"
          set +e
          gitopsify generate -c gitopsify.yaml -o . --kubeconfig "$RUNNER_TEMP/kubeconfig" --no-input \
            --push --push-branch gitopsify/sync --exit-code
          code=$?
          set -e
          [ "$code" = 0 ] && exit 0
          [ "$code" = 2 ] || exit "$code"
          # An open pull request already has the new commit.
          gh pr view gitopsify/sync --json state -q .state 2>/dev/null | grep -qx OPEN && exit 0
          gh pr create --head gitopsify/sync --base main --title "gitopsify: changes in the test cluster" \
            --body "Regenerated from the test cluster by the scheduled job."
        shell: bash
```

`-c gitopsify.yaml -o .` regenerates from the configuration the repository
already holds, into the checkout. Files the job doesn't produce (the workflow
itself) are left alone.

## Through a gitopsify server

The server reaches the cluster and the repository with its own credentials.
The job only needs an API token (`gitopsify server token --role operator
--cluster test`):

```sh
set -euo pipefail
export GITOPSIFY_REMOTE=https://gitopsify.acme.io GITOPSIFY_REMOTE_TOKEN="$TOKEN"
# Exits 1 when the run or a push failed.
run=$(gitopsify run start -c gitopsify.yaml --cluster test --publish --branch gitopsify/sync --wait --format json)
if jq -e '.publishes | any(.pushed or .upToDate)' <<<"$run" >/dev/null; then
  gh pr view gitopsify/sync --json state -q .state 2>/dev/null | grep -qx OPEN ||
    gh pr create --head gitopsify/sync --base main --title "gitopsify: changes in test" --body "Run $(jq -r .id <<<"$run")"
fi
```

With `--wait`, `-q` prints only the run's ID, and only when it succeeded. The
run's `summary.changed` counts the files that differ from the repository, and
the run stays in the server's history, with its log, for the audit trail.

## Things to know

- **Deletions are real.** An object deleted from the cluster disappears from
  the pull request too. So does an object the job can't read: give its
  ServiceAccount read access to everything you migrate, and watch the job's
  warnings.
- **Clusters Argo CD or Flux already manage.** Their objects are left out by
  default (`already-gitops-managed`). After a takeover that would empty the
  repository. To capture hand edits made in such a cluster, generate with
  `--include-managed`. The tracking labels Argo CD and Flux add are removed,
  so the pull request shows only the edits.
- **Hand edits in the repository.** By default the job overwrites them, with a
  warning naming the files. Set `spec.output.merge.edited: keep` (or `fail`)
  and `keep` paths so the job leaves them alone (see
  [regenerating](concepts.md#regenerating-into-an-existing-repository)).
- **Several environments, several repositories.** With `--layout
  repo-per-env` each environment's repository gets the same branch, and only
  their files count for `--exit-code`.
- **A directory of a bigger repository** (`--repo-path`): gitopsify doesn't
  commit or push those (`--push` only warns). Commit and push from the job
  instead.
- **A checkout you keep between runs**: fetch and reset it to the remote
  branch first. `--push-branch` commits on the checked-out branch, and an
  out-of-date checkout gives an out-of-date pull request.
- **One job at a time.** Use `concurrencyPolicy: Forbid`, or a CI concurrency
  group. Two jobs pushing the same branch stop each other rather than lose
  commits.
- **Secrets read from files** (`--from-file`) are encrypted afresh on every
  run. Read from the cluster, they aren't.
