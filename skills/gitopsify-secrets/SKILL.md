---
name: gitopsify-secrets
description: Choose and carry out how Kubernetes Secrets move in a gitopsify migration without plain-text credentials in Git - Sealed Secrets, SOPS with age or KMS, External Secrets Operator with AWS Secrets Manager, Google Secret Manager, Azure Key Vault or Vault, or creating them by hand. Use when picking --secrets, when MIGRATION_REPORT.md lists Secrets that need action, or when Secrets are missing after a sync.
---

# Secrets in a gitopsify migration

gitopsify never writes Secret data to Git in plain text unless told to
(`--secrets plain --allow-plain-secrets`). Every Secret it doesn't migrate is
listed under "Secrets that need action" in `MIGRATION_REPORT.md`.

Rules for you: never print Secret values or paste them into the conversation;
work with files and pipes; delete `.secrets/` and key files when done; never
`git add -f` anything under `.secrets/`.

## Choose a strategy

| The team has... | Use | Why |
|---|---|---|
| Nothing in particular | `sealed-secrets` | No external service; sealed per environment with that cluster's key; works with Argo CD and Flux |
| A cloud secret manager or Vault | `external-secrets` | Secrets stay in the manager; Git holds references only |
| Flux and age/KMS keys already | `sops` | Flux decrypts natively. With Argo CD it needs KSOPS or helm-secrets, so prefer the others there |
| Few Secrets, strict separation | `local`, then create them by hand | gitopsify writes them to the git-ignored `.secrets/` |
| Secrets someone else provisions | `skip` | Listed in the report, nothing written |

`secrets.overrides` mixes strategies, for example `local` for one namespace.
Helm values that look like credentials are reported separately: those are not
Secrets objects, so move them into Secrets referenced by the chart.

## Sealed Secrets

The controller must run in each target before `generate`, because sealing
uses that cluster's public key:

```sh
helm repo add sealed-secrets https://bitnami-labs.github.io/sealed-secrets
helm install sealed-secrets sealed-secrets/sealed-secrets -n kube-system \
  --set fullnameOverride=sealed-secrets-controller --kube-context uat
gitopsify generate --secrets sealed-secrets --env uat=uat
```

gitopsify fetches the certificate through the API server. Where that is
blocked (private GKE clusters, strict network policies), fetch it yourself and
pass it: `kubeseal --controller-namespace kube-system --fetch-cert --context uat > uat-cert.pem`
and `--sealed-secrets-cert uat-cert.pem`. Back up the controller's private
key (`kubectl -n kube-system get secret -l sealedsecrets.bitnami.com/sealed-secrets-key -o yaml`)
into the team's vault: without it, the sealed Secrets can't be decrypted
after a cluster rebuild. Ask the person where it should go; don't store it
yourself.

## SOPS with age

```sh
age-keygen -o age.key                       # keep age.key out of Git; put it in the team vault
gitopsify generate --tool flux --secrets sops --sops-age "$(age-keygen -y age.key)"
gitopsify bootstrap --env uat --sops-age-key age.key
```

gitopsify writes `.sops.yaml` and encrypts only `data`/`stringData`. KMS, GCP
KMS, Azure Key Vault and PGP recipients go in `secrets.sops` instead of `age`.
The person can edit encrypted Secrets later with `sops apps/<app>/.../secret-x.yaml`.

## External Secrets Operator

1. Install ESO in the target (`helm install external-secrets
   external-secrets/external-secrets -n external-secrets --create-namespace`).
2. Create the store the ExternalSecrets point at (`--eso-store`,
   `--eso-store-kind`, default `ClusterSecretStore/default`), with the cloud's
   workload identity rather than static keys where possible.
3. `gitopsify generate --secrets external-secrets --eso-store <name>`. Remote
   keys follow `secrets.externalSecrets.keyTemplate`
   (`{{ .Namespace }}/{{ .Name }}` by default).
4. Load the values: gitopsify writes them to
   `.secrets/external-secrets/<namespace>/<name>.json`. With the person's
   go-ahead, push each into the manager without echoing it:

```sh
f=.secrets/external-secrets/shop/web-db.json
aws secretsmanager create-secret --name shop/web-db --secret-string "file://$f"
gcloud secrets create shop-web-db --data-file="$f"            # GCP names can't contain "/": set keyTemplate
az keyvault secret set --vault-name acme-kv --name shop-web-db --file "$f"
vault kv put secret/shop/web-db @"$f"
```

Check the key names the store expects (some managers forbid `/`) and set
`keyTemplate` before generating.

## local and skip

`local` writes `.secrets/<namespace>/<name>.yaml`. After the person confirms
the target context:

```sh
kubectl --context uat apply --server-side -f .secrets/shop/   # namespaces must exist; apply after the namespaces app synced
```

Do it before or right after bootstrap; pods wait for missing Secrets and
start once they exist. Image pull secrets
(`kubernetes.io/dockerconfigjson`) first: without them nothing pulls.

## Afterwards

- `rm -rf .secrets` once everything is in place; confirm with the person.
- Credentials that were copied to a laptop count as exposed for strict
  policies: suggest rotating them.
- TLS Secrets issued by cert-manager are never migrated; cert-manager issues
  new ones in the target from the migrated Certificates.
- ServiceAccount tokens and Helm release records are never migrated either;
  the target creates its own.
