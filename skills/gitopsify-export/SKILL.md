---
name: gitopsify-export
description: Turn a tool configured by hand (Grafana and other tools with a gitopsify provider) into Terraform/OpenTofu or Crossplane configuration with gitopsify export - installing the provider, credentials, choosing organizations and objects, adopting versus replicating, secrets, the first plan, CI with the export file, and exporting again safely. Use when someone wants Grafana dashboards, folders, data sources or alerting (or another tool's objects) managed as code, mentions gitopsify export or gitopsify provider, or asks to bring a tool under Terraform or Crossplane.
---

# Exporting a tool to Terraform or Crossplane

`gitopsify export` reads a tool through a provider (`gitopsify-provider-<name>`)
and writes configuration that manages the same objects. Reference:
<https://github.com/hayone1/gitopsify/blob/main/docs/providers.md>.

Rules for you: never print credentials or secret values, never paste them into
the conversation, and never put them in `gitopsify-<provider>.yaml` (use
`{env: VAR}` or `{file: path}`). Don't run `terraform apply`, `kubectl apply`
or anything that changes the tool without the person's explicit yes. Reading is
safe; applying is theirs to decide.

## 1. The provider

```sh
gitopsify provider search            # is the tool supported?
gitopsify provider install grafana   # or owner/name for unlisted ones
gitopsify provider info grafana      # settings (and their env variables), kinds, outputs
```

No provider for the tool: say so, and point to
<https://github.com/hayone1/gitopsify/blob/main/docs/writing-providers.md>.

## 2. Credentials

Settings come from flags (`--set`), the export file, then the environment
(`gitopsify provider info` lists the variables; for Grafana `GRAFANA_URL` and
`GRAFANA_AUTH`). Ask the person to set secrets in their shell; don't ask them
to paste secrets to you.

For Grafana: a server admin's `user:password` exports every organization; a
service account token sees only its own organization (one instance per
organization, each with its own token, works too).

## 3. Decide, with the person

| Decision | Recommend | Alternatives |
|---|---|---|
| Adopt or replicate | Adopt (`import` blocks; the first plan changes nothing) | `--no-adopt` to build the same objects in another instance |
| Formats | What they already run: Terraform/OpenTofu, or Crossplane 2 if they have a cluster with GitOps | Both |
| Scope | Everything (`--dry-run` first to see it) | `--partition`, `--kind`, `--include 'Kind/partition/name'` |
| Secret values the tool returns | `skip` (variables / Secrets they fill in) | `--secret-values local` (git-ignored files; makes the first plan clean) |

Then:

```sh
gitopsify export grafana --dry-run                # what would be written
gitopsify export grafana -o grafana --save        # writes, and saves the choices to gitopsify-grafana.yaml
```

On a terminal, plain `gitopsify export` asks all of this itself; suggest it to
people who prefer that.

## 4. Read the report together

`<out>/EXPORT_REPORT.md` lists what was exported per organization, what was
left out and why (provisioned objects, unmapped integrations), secrets that
couldn't be exported (add them by hand later), Terraform variables and
Kubernetes Secrets to provide, and notes. Walk the person through anything in
those sections.

## 5. First plan (Terraform)

```sh
cd <out>/terraform/<instance>/<organization>
terraform init && terraform plan
```

Expect only imports, and in-place updates of objects whose secrets the tool
doesn't return to Terraform (Grafana contact points). Anything to add, replace
or destroy is a mapping problem: stop, show it, and suggest reporting it to the
provider's repository with the plan output (secrets redacted). Apply only when
the person says so; a second plan should show no changes.

## 6. Crossplane

Follow `<out>/crossplane/README.md`: apply `setup/`, create the credentials
Secret (the command is there), apply with `--server-side`. While adopting,
everything only observes; when `kubectl get managed -A` shows all Ready and
Synced, the person removes `components/observe-only` from the kustomization.

## 7. CI and exporting again

Commit the output (the `.gitignore` keeps secrets and state out) and
`gitopsify-<provider>.yaml`. In CI: `gitopsify export --no-input --install`
with the credentials as secrets, then open a PR with the changes. Re-exports
keep names stable, never make Terraform destroy (objects no longer exported get
`removed` blocks), and fail with exit code 3 if the provider couldn't read
everything; `--allow-partial` exports the rest and keeps the previous files of
what couldn't be read. Hand edits to generated files are overwritten: keep
custom configuration in separate files.
