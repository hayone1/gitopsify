# Secrets

Secret values never go into Git in plain text unless you explicitly ask for it.
Every Secret that isn't migrated as-is shows up under **Secrets that need
action** in `MIGRATION_REPORT.md`, with what to do about it.

## Pick a strategy

| `--secrets` | What you get | What the target needs |
|---|---|---|
| `skip` (default) | Nothing written; each Secret is listed in the report | You create them |
| `local` | `.secrets/<namespace>/<name>.yaml`, git-ignored | `kubectl apply -f .secrets/...` once |
| `sealed-secrets` | A `SealedSecret` per Secret, sealed for each environment with that cluster's key | The sealed-secrets controller |
| `sops` | The Secret encrypted with `sops`, plus `.sops.yaml` | Flux: the decryption key. Argo CD: KSOPS or helm-secrets |
| `external-secrets` | An `ExternalSecret` per Secret; the values in `.secrets/external-secrets/` for you to load into your secret manager | External Secrets Operator and a store |
| `plain` | The Secret as it is (needs `--allow-plain-secrets`) | Nothing. Only for values that aren't secret |

Which one?

- **No secret manager, any GitOps tool**: `sealed-secrets`. Nothing external to
  run, and each environment can only decrypt its own Secrets.
- **AWS Secrets Manager, Google Secret Manager, Azure Key Vault or Vault in
  place**: `external-secrets`. Git holds references only.
- **Flux, and you already use age or a KMS**: `sops`.
- **A handful of Secrets, managed elsewhere**: `local` or `skip`.

Mix them with overrides, first match wins:

```yaml
spec:
  secrets:
    strategy: sealed-secrets
    overrides:
      - {namespaces: [monitoring], strategy: local}
      - {names: ["*-tls"], strategy: skip}
```

Some Secrets are never migrated, whatever the strategy: TLS Secrets issued by
cert-manager (it issues new ones from the migrated Certificates), ServiceAccount
token Secrets, Helm release records, and OpenShift's generated pull secrets.

## Sealed Secrets

Install the controller in each target first, because sealing uses that
cluster's public key:

```sh
helm repo add sealed-secrets https://bitnami-labs.github.io/sealed-secrets
helm install sealed-secrets sealed-secrets/sealed-secrets -n kube-system \
  --set fullnameOverride=sealed-secrets-controller --kube-context uat
gitopsify generate --secrets sealed-secrets --env uat=uat --env prod=prod
```

gitopsify seals in-process (the same encryption `kubeseal` uses, in strict
scope by default) with the certificate it fetches through each environment's
API server. With several environments, each gets its own SealedSecret in its
overlay. Where the API server can't reach the controller (some private
clusters), fetch the certificate yourself and pass it:

```sh
kubeseal --fetch-cert --controller-namespace kube-system --context uat > uat.pem
gitopsify generate --secrets sealed-secrets --sealed-secrets-cert uat.pem
```

Back up the controller's private key somewhere safe: without it, nobody can
decrypt the SealedSecrets after the cluster is rebuilt.

## SOPS

```sh
age-keygen -o age.key
gitopsify generate --tool flux --secrets sops --sops-age "$(age-keygen -y age.key)"
gitopsify bootstrap --env uat --sops-age-key age.key
```

Only `data` and `stringData` are encrypted, so the rest of the Secret stays
readable in reviews. PGP, AWS KMS, GCP KMS and Azure Key Vault recipients go in
`secrets.sops` in `gitopsify.yaml`. The Flux Kustomizations of apps with
encrypted Secrets get a `decryption` section; `--sops-age-key` loads the key
into the cluster. Edit a Secret later with `sops <file>`.

## External Secrets

1. Install the External Secrets Operator in the target and create a
   `ClusterSecretStore` (or `SecretStore`) for your secret manager.
2. Generate, naming the store:

   ```sh
   gitopsify generate --secrets external-secrets --eso-store aws-sm --eso-store-kind ClusterSecretStore
   ```

3. Load the values: they are in `.secrets/external-secrets/<namespace>/<name>.json`
   (git-ignored), under the key the report shows:

   ```sh
   aws secretsmanager create-secret --name shop/web-db --secret-string file://.secrets/external-secrets/shop/web-db.json
   vault kv put secret/shop/web-db @.secrets/external-secrets/shop/web-db.json
   ```

Remote key names follow `secrets.externalSecrets.keyTemplate` (default
`{{ .Namespace }}/{{ .Name }}`); change it for managers that don't allow `/`.
With `propertyPerKey: false`, each ExternalSecret extracts the whole JSON
document instead of one property per key.

## Local files

```sh
gitopsify generate --secrets local
kubectl --context uat apply --server-side -f gitops/.secrets/shop/
```

Apply them once their namespaces exist; pods that need them wait and start
when they appear. Pull secrets (`kubernetes.io/dockerconfigjson`) first,
otherwise nothing can pull its image.

## Hygiene

- Delete `.secrets/` once the values are where they belong.
- Values copied to a laptop count as exposed under strict policies; rotate them
  after the migration.
- Never `git add -f` anything in `.secrets/`; the generated `.gitignore` keeps
  it out for a reason.
