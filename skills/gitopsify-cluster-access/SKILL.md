---
name: gitopsify-cluster-access
description: Get gitopsify connected to the source and target Kubernetes clusters - EKS, GKE, AKS, OpenShift, Rancher, k3s, kind, Talos, DigitalOcean, Teleport, OIDC logins, in-cluster ServiceAccounts and plain tokens - with least privilege. Use when gitopsify doctor, scan or bootstrap can't connect, is unauthorized or forbidden, or when setting up access for CI.
---

# Cluster access for gitopsify

gitopsify connects like kubectl: `KUBECONFIG` or `~/.kube/config`, the current
context, and whatever credential plugin the kubeconfig runs. If `kubectl
--context X get ns` works, `gitopsify --context X` works. Start with:

```sh
gitopsify doctor --context <source> --env uat=<target>
```

It shows the server, how gitopsify authenticates, who the API server thinks
you are, and whether you may read everything (source) or install Argo CD or
Flux (target).

## Flags

The source cluster takes kubectl's flags; the environment a command acts on
takes the same flags with a `target-` prefix.

| Source | Target | |
|---|---|---|
| `--kubeconfig` (several files like KUBECONFIG) | `--target-kubeconfig` | |
| `--context` | `--target-context` or `--env uat=<context>` | |
| `--server` | `--target-server` | API server URL; on its own, the kubeconfig is ignored |
| `--token`, `$GITOPSIFY_TOKEN` | `--target-token`, `$GITOPSIFY_TARGET_TOKEN` | never written to files |
| `--token-file` | `--target-token-file` | re-read when it changes |
| `--client-certificate`, `--client-key` | `--target-client-*` | |
| `--certificate-authority`, `--insecure-skip-tls-verify`, `--tls-server-name` | `--target-*` | |
| `--as`, `--as-group`, `--as-uid` | `--target-as`, ... | impersonation |
| `--proxy-url`, `--request-timeout` | `--target-*` | |
| `--in-cluster` | `--target-in-cluster` | the pod's ServiceAccount |

In `gitopsify.yaml`, `source:` and `environments[].cluster:` take the same
settings (`kubeconfig`, `context`, `apiServer`, `tokenFile`,
`certificateAuthority`, `impersonate`, ...). Tokens can't go in the file: it
is copied into the repository.

## Getting a kubeconfig, per platform

| Platform | Command | Credential plugin |
|---|---|---|
| Amazon EKS | `aws eks update-kubeconfig --name <cluster> --region <r> [--alias src]` | `aws eks get-token` (AWS CLI v2); refresh with `aws sso login` |
| Google GKE | `gcloud container clusters get-credentials <cluster> --region <r>` | `gke-gcloud-auth-plugin` (`gcloud components install gke-gcloud-auth-plugin`) |
| Azure AKS | `az aks get-credentials -g <rg> -n <cluster>` then `kubelogin convert-kubeconfig -l azurecli` | `kubelogin`; `-l spn`/`-l workloadidentity` for CI |
| OpenShift | `oc login --server=https://api.<cluster>:6443` | token in the kubeconfig; expires, log in again |
| Rancher | Download the kubeconfig from the cluster page, or `rancher clusters kf <cluster>` | Rancher token |
| k3s / RKE2 | copy `/etc/rancher/k3s/k3s.yaml` (`/etc/rancher/rke2/rke2.yaml`) and replace `127.0.0.1` with the server address | client certificate |
| kind / k3d / minikube | created automatically (`kind-<name>`, `k3d-<name>`, `minikube`) | client certificate |
| Talos | `talosctl kubeconfig --nodes <ip>` | client certificate |
| DigitalOcean | `doctl kubernetes cluster kubeconfig save <cluster>` | `doctl` |
| Teleport | `tsh kube login <cluster>` | `tsh` |
| OIDC (Dex, Keycloak, Okta, ...) | `kubectl oidc-login setup` (int128/kubelogin) | `kubectl oidc-login get-token`; older `auth-provider: oidc` kubeconfigs work too |

Merge kubeconfigs instead of juggling files: `--kubeconfig a.yaml:b.yaml`, or
`KUBECONFIG=a.yaml:b.yaml`. Interactive logins (SSO, device codes) can prompt
on the terminal; with `--no-input` they fail instead, so log in first in CI.

## Least privilege

**Source: read-only.** Exporting needs `get`/`list` on what is migrated,
Secrets included unless `--secrets skip`. A dedicated ServiceAccount:

```sh
kubectl --context src create serviceaccount gitopsify-reader -n kube-system
kubectl --context src create clusterrolebinding gitopsify-reader --clusterrole=view --serviceaccount=kube-system:gitopsify-reader
# view excludes Secrets; for Secret strategies other than skip add a read rule:
kubectl --context src create clusterrole gitopsify-secrets-reader --verb=get,list --resource=secrets
kubectl --context src create clusterrolebinding gitopsify-secrets-reader --clusterrole=gitopsify-secrets-reader --serviceaccount=kube-system:gitopsify-reader
kubectl --context src -n kube-system create token gitopsify-reader --duration=8h > source.token
gitopsify scan --server https://<api>:6443 --token-file source.token --certificate-authority ca.pem
```

`view` also skips some kinds (RBAC objects, CRD-based resources not aggregated
to view). For a complete export use a ClusterRole with `get`/`list` on `*`
(`kubectl create clusterrole gitopsify-reader --verb=get,list --resource='*.*'`),
and remove the binding afterwards.

**Target:** `check` and `diff` need read access and server-side dry-run
(`patch`); `bootstrap` needs cluster-admin once, to install CRDs and
ClusterRoles. After bootstrap the GitOps tool works with its own
ServiceAccount; nobody needs to keep cluster-admin credentials around.

## CI

- Prefer short-lived credentials: OIDC federation to the cloud (GitHub
  Actions to AWS/GCP/Azure), then the platform's kubeconfig command.
- Otherwise a ServiceAccount token from `kubectl create token` in a CI secret,
  passed through `GITOPSIFY_TOKEN` / `GITOPSIFY_TARGET_TOKEN`, never on the
  command line.
- Running inside the cluster (a Job): `--in-cluster` for the source,
  `--target-in-cluster` for the target.
- Always pass `--no-input`.

## Errors and what they mean

| Error | Fix |
|---|---|
| `no kubeconfig found` | Set `KUBECONFIG`/`--kubeconfig`, or `--server` with `--token`/`--client-certificate`, or `--in-cluster` |
| `context "x" does not exist (contexts in the kubeconfig: ...)` | Pick one of the listed contexts |
| `gets credentials by running "aws" ... install` | Install the plugin the message names |
| `The credential plugin "..." failed` | Log in again with the command the message names |
| `rejected the credentials` (401) | Expired token or certificate; log in again or issue a new token |
| `certificate isn't trusted` | `--certificate-authority`, or fix `certificate-authority-data`; `--insecure-skip-tls-verify` only for throwaway clusters |
| `doesn't name the address used to reach it` | `--tls-server-name <name in the certificate>` |
| `Can't reach ...` | VPN, firewall, private endpoint, or `--proxy-url` |
| `not allowed to list <kind>` (scan warning) | Missing RBAC; see Least privilege |
| `The gcp/azure auth plugin has been removed` | Old kubeconfig; regenerate it with gcloud or `kubelogin convert-kubeconfig` |
