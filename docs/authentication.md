# Cluster access

gitopsify talks to two kinds of clusters: the **source** it reads from, and the
**environments** (targets) that `check`, `diff`, `adopt`, `status` and
`bootstrap` act on. Both connect the way kubectl does, so if this works:

```sh
kubectl --context test get namespaces
```

then `gitopsify --context test ...` works too: the same `KUBECONFIG` or
`~/.kube/config`, the same contexts, the same credential plugins. When it
doesn't, run `gitopsify doctor`: it shows which server it reached, how it
authenticated, who the server thinks you are, and what you're allowed to do.

## Choosing clusters

```sh
gitopsify scan --context test                       # the source
gitopsify check --env uat=uat-context               # an environment, by context
gitopsify check --env uat --target-kubeconfig ~/.kube/uat.yaml
```

Contexts from several kubeconfig files can be combined like in `KUBECONFIG`:
`--kubeconfig ~/.kube/test.yaml:~/.kube/uat.yaml`.

In `gitopsify.yaml`, `source` and each environment's `cluster` take the same
settings, so you don't repeat flags:

```yaml
source:
  context: test
environments:
  - name: uat
    cluster:
      kubeconfig: ~/.kube/uat.yaml
      context: uat-admin
```

## All the options

The source takes kubectl's flags. The environment a command acts on takes the
same flags with a `target-` prefix.

| Source flag | Target flag | `gitopsify.yaml` | |
|---|---|---|---|
| `--kubeconfig` | `--target-kubeconfig` | `kubeconfig` | One file, or several separated like in `KUBECONFIG` |
| `--context` | `--target-context`, `--env name=context` | `context` | |
| `--server` | `--target-server` | `apiServer` | API server URL. Without `--kubeconfig`/`--context`, the kubeconfig is ignored entirely |
| `--token` | `--target-token` | (never) | Also `$GITOPSIFY_TOKEN` / `$GITOPSIFY_TARGET_TOKEN` |
| `--token-file` | `--target-token-file` | `tokenFile` | Re-read when it changes, for projected tokens |
| `--client-certificate`, `--client-key` | `--target-client-certificate`, `--target-client-key` | `clientCertificate`, `clientKey` | |
| `--certificate-authority` | `--target-certificate-authority` | `certificateAuthority` | |
| `--insecure-skip-tls-verify` | `--target-insecure-skip-tls-verify` | `insecureSkipTLSVerify` | Throwaway clusters only |
| `--tls-server-name` | `--target-tls-server-name` | `tlsServerName` | When you reach the server by an address its certificate doesn't name |
| `--proxy-url` | `--target-proxy-url` | `proxyURL` | `http://`, `https://` or `socks5://` |
| `--as`, `--as-group`, `--as-uid` | `--target-as`, ... | `impersonate`, `impersonateGroups`, `impersonateUID` | |
| `--in-cluster` | `--target-in-cluster` | `inCluster` | The ServiceAccount of the pod gitopsify runs in |
| `--request-timeout` | `--target-request-timeout` | `timeout` | For example `30s` |

**Tokens never go into `gitopsify.yaml`.** `generate` copies that file into the
repository, so a `token:` field is rejected; use `tokenFile`, the flag or the
environment variable. Paths may start with `~/`.

## Your platform

| Platform | Get a kubeconfig | What runs behind it |
|---|---|---|
| Amazon EKS | `aws eks update-kubeconfig --name prod --region eu-west-1` | `aws eks get-token`; log in with `aws sso login` when it expires |
| Google GKE | `gcloud container clusters get-credentials prod --region europe-west1` | `gke-gcloud-auth-plugin` (`gcloud components install gke-gcloud-auth-plugin`) |
| Azure AKS | `az aks get-credentials -g rg -n prod`, then `kubelogin convert-kubeconfig -l azurecli` | `kubelogin`; `-l spn` or `-l workloadidentity` in CI |
| OpenShift | `oc login https://api.prod.example.com:6443` | a token that expires; log in again |
| Rancher | download the kubeconfig from the cluster page | a Rancher token |
| k3s, RKE2 | copy `/etc/rancher/k3s/k3s.yaml` and replace `127.0.0.1` with the server's address | client certificate |
| kind, k3d, minikube | created by the tool | client certificate |
| Talos | `talosctl kubeconfig --nodes 10.0.0.2` | client certificate |
| DigitalOcean | `doctl kubernetes cluster kubeconfig save prod` | `doctl` |
| Teleport | `tsh kube login prod` | `tsh` |
| OIDC (Dex, Keycloak, Okta, Entra ID) | `kubectl oidc-login setup` | `kubectl oidc-login get-token`; older kubeconfigs with `auth-provider: oidc` work too |

Logins that need you at the keyboard (SSO, device codes) can prompt in the
terminal. With `--no-input` they fail instead of hanging, so log in first.

## Least privilege

**Source**: reading is enough. A ServiceAccount that may read everything:

```sh
kubectl --context test create serviceaccount gitopsify-reader -n kube-system
kubectl --context test create clusterrole gitopsify-reader --verb=get,list --resource='*.*'
kubectl --context test create clusterrolebinding gitopsify-reader --clusterrole=gitopsify-reader \
  --serviceaccount=kube-system:gitopsify-reader
kubectl --context test -n kube-system create token gitopsify-reader --duration=8h > reader.token

gitopsify scan --server https://test.example.com:6443 --token-file reader.token --certificate-authority ca.pem
```

With less (Kubernetes' `view` role, for example, can't read Secrets or RBAC),
gitopsify migrates what it may read and lists the kinds it couldn't as
warnings, so nothing is left out silently. Delete the binding afterwards.

**Targets**: `check` and `diff` need read access and server-side dry-run.
`bootstrap` needs cluster-admin once, to install Argo CD or Flux. Afterwards
the GitOps tool works with its own ServiceAccount.

## In CI

- Prefer short-lived credentials: federate the CI's OIDC identity to your cloud
  and use the platform's kubeconfig command.
- Otherwise, keep a ServiceAccount token in a CI secret and pass it through
  `GITOPSIFY_TOKEN` / `GITOPSIFY_TARGET_TOKEN`, not on the command line where
  process listings show it.
- In a Kubernetes Job, use `--in-cluster` (and `--target-in-cluster` when the
  target is the same cluster). Inside a pod without a kubeconfig, gitopsify
  uses the pod's ServiceAccount automatically.
- Always pass `--no-input`.

```yaml
# GitHub Actions: report drift every night
- run: gitopsify diff --no-input --env prod --exit-code
  env:
    GITOPSIFY_TOKEN: ${{ secrets.TEST_CLUSTER_TOKEN }}
    GITOPSIFY_TARGET_TOKEN: ${{ secrets.PROD_CLUSTER_TOKEN }}
```

## When it doesn't connect

gitopsify adds what to do to the error. The common ones:

| Message | What to do |
|---|---|
| `no kubeconfig found` | Set `KUBECONFIG` or `--kubeconfig`, or use `--server` with `--token`/`--client-certificate`, or `--in-cluster` |
| `context "x" does not exist (contexts in the kubeconfig: ...)` | Use one of the listed contexts |
| `gets credentials by running "gke-gcloud-auth-plugin" ...` | Install the plugin named in the message |
| `The credential plugin "aws" failed: ...` | Log in again as the message says |
| `The API server rejected the credentials` | The token or certificate expired or is wrong |
| `certificate isn't trusted` | `--certificate-authority`; `--insecure-skip-tls-verify` only for test clusters |
| `doesn't name the address used to reach it` | `--tls-server-name` with a name in the certificate |
| `Can't reach https://...` | VPN, firewall, private endpoint, or `--proxy-url` |
| `The gcp auth plugin has been removed` | Regenerate the kubeconfig (the old built-in GKE and AKS logins are gone from Kubernetes) |
| scan warns `not allowed to list secrets` | Missing permissions; those objects weren't migrated |
