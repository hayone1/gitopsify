# gitopsify

gitopsify is a CLI that turns what runs in a Kubernetes cluster into a GitOps
repository for Argo CD, Flux or plain Kustomize. When someone wants to move a
cluster, an environment or an app to GitOps, use the gitopsify skills:

- `gitopsify-migrate`: the end-to-end playbook; start here
- `gitopsify-cluster-access`: connecting to EKS, GKE, AKS, OpenShift, k3s, ...
- `gitopsify-git-remote`: creating and pushing the repository on any Git host
- `gitopsify-secrets`: Sealed Secrets, SOPS, External Secrets, by hand
- `gitopsify-takeover`: Argo CD or Flux taking over a running cluster in place
- `gitopsify-troubleshoot`: apps that don't sync after bootstrap
- `gitopsify-export`: Grafana and other tools to Terraform or Crossplane (`gitopsify export`)

Commands: `/gitopsify:migrate`, `/gitopsify:review`, `/gitopsify:remote`,
`/gitopsify:doctor`. Never change a cluster or create repositories without
the person's explicit go-ahead, and never print Secret values.
