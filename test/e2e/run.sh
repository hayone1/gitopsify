#!/usr/bin/env bash
# End-to-end test of gitopsify against real kind clusters. Stages:
#   source    a "ClickOps" source cluster (setup-source.sh)
#   auth      every way of reaching a cluster: kubeconfig contexts and lists,
#             ServiceAccount tokens (flag, file, environment), CA and insecure
#             TLS, impersonation, an exec credential plugin, in-cluster
#   roundtrip the generated manifests, dry-run against the source itself, leave
#             every object unchanged except the deliberate Helm drift
#   artifacthub  chart repositories found on artifacthub.io (no cluster needed)
#   plain     --tool none on a fresh cluster with the generated apply script
#   argocd    generate --push to a local Git server, bootstrap --wait, status
#   flux      the same with Flux and Secrets sealed in-process
#   sops      Flux with Secrets encrypted by sops and age
#   takeover  Argo CD takes over the source cluster; no pod may restart except
#             the drifted Helm release
#
# Needs docker, kind, kubectl, helm, jq, git, curl and go. About 45 minutes.
#   STAGES="auth argocd"  run only these stages (source always runs)
#   SKIP="flux sops"      skip these stages
#   KEEP=1                keep the clusters and Git server afterwards
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
src=gitopsify-e2e-src
dst=gitopsify-e2e-dst
port=${GIT_PORT:-8099}
bin="$work/gitopsify"
export KUBECONFIG="$work/kubeconfig"
# Keep the Helm repository setup-source.sh adds out of the user's Helm config;
# gitopsify reads the same variables to resolve chart repositories.
export HELM_REPOSITORY_CONFIG="$work/helm/repositories.yaml" HELM_REPOSITORY_CACHE="$work/helm/cache"
export PATH="$work/bin:$PATH"
mkdir -p "$work/bin"

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$*"; exit 1; }
pass() { printf '\033[32mok\033[0m %s\n' "$*"; }

want() {
  local s=$1
  [[ " ${SKIP:-} " == *" $s "* ]] && return 1
  [[ -z "${STAGES:-}" || " $STAGES " == *" $s "* ]]
}

cleanup() {
  [[ -n "${server_pid:-}" ]] && kill "$server_pid" 2>/dev/null || true
  if [[ "${KEEP:-}" != 1 ]]; then
    kind delete cluster --name "$src" >/dev/null 2>&1 || true
    kind delete cluster --name "$dst" >/dev/null 2>&1 || true
    rm -rf "$work"
  else
    echo "kept: KUBECONFIG=$work/kubeconfig, work dir $work"
  fi
}
trap cleanup EXIT

# wait_for <description> <seconds> <command...> retries the command until it succeeds.
wait_for() {
  local what=$1 timeout=$2
  shift 2
  local deadline=$((SECONDS + timeout))
  until "$@" >/dev/null 2>&1; do
    ((SECONDS < deadline)) || fail "timed out waiting for $what"
    sleep 10
  done
}

g() { "$bin" --no-input "$@"; }
apps_of() { jq -r '[.apps[].name] | sort | join(",")' "$1"; }

new_dst() {
  kind delete cluster --name "$dst" >/dev/null 2>&1 || true
  kind create cluster --name "$dst" --wait 180s
}

log "building"
(cd "$root" && CGO_ENABLED=0 go build -o "$bin" ./cmd/gitopsify && go build -o "$work/githttp" ./test/e2e/githttp)

if want artifacthub; then
  log "Artifact Hub"
  # With no local Helm repositories, podinfo can only be found on Artifact Hub;
  # the unpublished in-house chart stays vendored.
  mkdir -p "$work/nohelm"
  HELM_REPOSITORY_CONFIG="$work/nohelm/repositories.yaml" HELM_REPOSITORY_CACHE="$work/nohelm/cache" \
    g scan --from-file "$root/testdata/clusters/clickops.yaml" --artifacthub --format json > "$work/ah.json"
  [[ $(jq -r '.helmReleases[] | select(.release == "podinfo/podinfo") | .repository' "$work/ah.json") == https://stefanprodan.github.io/podinfo ]] || fail "podinfo not resolved on Artifact Hub: $(jq -c .helmReleases "$work/ah.json")"
  [[ $(jq -r '.helmReleases[] | select(.release == "tools/inhouse") | .mode' "$work/ah.json") == vendor ]] || fail "inhouse should stay vendored"
  pass "Artifact Hub resolves published charts, vendoring keeps the rest"
fi

log "source cluster"
kind create cluster --name "$src" --wait 180s
"$here/setup-source.sh" "kind-$src"
g scan --context "kind-$src" --format json > "$work/scan.json"
baseline=$(apps_of "$work/scan.json")
echo "apps: $baseline"

if want auth; then
  log "auth"
  g doctor --context "kind-$src" > "$work/doctor.txt" || { cat "$work/doctor.txt"; fail "doctor"; }
  grep -q "client certificate" "$work/doctor.txt" || fail "doctor doesn't describe the client certificate"
  pass "doctor with a kubeconfig context"

  server=$(kubectl config view --raw -o jsonpath="{.clusters[?(@.name=='kind-$src')].cluster.server}")
  kubectl config view --raw -o jsonpath="{.clusters[?(@.name=='kind-$src')].cluster.certificate-authority-data}" | base64 -d > "$work/ca.pem"
  # The least-privilege reader from the gitopsify-cluster-access skill.
  k() { kubectl --context "kind-$src" "$@"; }
  k create serviceaccount gitopsify-reader -n kube-system
  k create clusterrole gitopsify-reader --verb=get,list --resource='*.*'
  k create clusterrolebinding gitopsify-reader --clusterrole=gitopsify-reader --serviceaccount=kube-system:gitopsify-reader
  k -n kube-system create token gitopsify-reader --duration=2h > "$work/reader.token"

  scan_as() { # <name> <args...>: the same apps as with the kubeconfig
    local name=$1
    shift
    g scan --format json "$@" > "$work/scan-$name.json" || fail "scan with $name"
    [[ $(apps_of "$work/scan-$name.json") == "$baseline" ]] || fail "$name: apps $(apps_of "$work/scan-$name.json"), want $baseline"
    pass "scan with $name"
  }
  scan_as token-file --server "$server" --token-file "$work/reader.token" --certificate-authority "$work/ca.pem"
  scan_as token-flag --server "$server" --token "$(cat "$work/reader.token")" --insecure-skip-tls-verify
  GITOPSIFY_TOKEN=$(cat "$work/reader.token") scan_as token-env --server "$server" --certificate-authority "$work/ca.pem" --tls-server-name kubernetes
  printf 'apiVersion: v1\nkind: Config\nclusters: [{name: other, cluster: {server: "https://127.0.0.1:1"}}]\ncontexts: [{name: other, context: {cluster: other, user: other}}]\nusers: [{name: other, user: {token: x}}]\ncurrent-context: other\n' > "$work/other.yaml"
  scan_as kubeconfig-list --kubeconfig "$work/other.yaml:$KUBECONFIG" --context "kind-$src"

  # An exec credential plugin, as EKS, GKE and AKS kubeconfigs use.
  cat > "$work/bin/fake-cred-plugin" <<EOF
#!/bin/sh
printf '{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"%s"}}' "\$(cat $work/reader.token)"
EOF
  chmod +x "$work/bin/fake-cred-plugin"
  cat > "$work/exec.yaml" <<EOF
apiVersion: v1
kind: Config
current-context: exec
clusters: [{name: exec, cluster: {server: "$server", certificate-authority: "$work/ca.pem"}}]
contexts: [{name: exec, context: {cluster: exec, user: exec}}]
users:
- name: exec
  user:
    exec: {apiVersion: client.authentication.k8s.io/v1, command: fake-cred-plugin, interactiveMode: Never}
EOF
  scan_as exec-plugin --kubeconfig "$work/exec.yaml"
  g doctor --kubeconfig "$work/exec.yaml" | grep -q "credential plugin fake-cred-plugin" || fail "doctor doesn't name the credential plugin"
  g doctor --kubeconfig "$work/exec.yaml" | grep -q "system:serviceaccount:kube-system:gitopsify-reader" || fail "doctor doesn't show the identity"
  pass "doctor with an exec plugin shows the plugin and the identity"

  # Impersonating a ServiceAccount that may read nothing.
  k create serviceaccount nobody -n kube-system
  g scan --context "kind-$src" --as system:serviceaccount:kube-system:nobody --format json > "$work/scan-nobody.json" || fail "scan as nobody"
  jq -e '[.warnings[] | select(contains("not allowed to list"))] | length > 10' "$work/scan-nobody.json" >/dev/null || fail "impersonation had no effect"
  pass "impersonation"

  # In-cluster: gitopsify inside a pod, with the pod's ServiceAccount.
  k -n kube-system run gitopsify --image=busybox:1.36 --overrides='{"spec":{"serviceAccountName":"gitopsify-reader"}}' --command -- sleep 3600
  k -n kube-system wait --for=condition=Ready pod/gitopsify --timeout=120s
  k -n kube-system cp "$bin" gitopsify:/tmp/gitopsify
  k -n kube-system exec gitopsify -- /tmp/gitopsify scan --no-input --in-cluster --format json > "$work/scan-incluster.json"
  [[ $(apps_of "$work/scan-incluster.json") == "$baseline" ]] || fail "in-cluster: $(apps_of "$work/scan-incluster.json")"
  k -n kube-system exec gitopsify -- /tmp/gitopsify scan --no-input --format json > "$work/scan-default.json"
  [[ $(apps_of "$work/scan-default.json") == "$baseline" ]] || fail "in-cluster without flags"
  k -n kube-system delete pod gitopsify --wait=false
  pass "in-cluster, explicit and automatic"
fi

if want roundtrip; then
  log "round trip against the source"
  g diff --context "kind-$src" --env same="kind-$src" --format json > "$work/roundtrip.json"
  changed=$(jq -r '[.[] | select(.action == "update")] | map(.object) | join(",")' "$work/roundtrip.json")
  errors=$(jq '[.[] | select(.action == "error")] | length' "$work/roundtrip.json")
  unchanged=$(jq '[.[] | select(.action == "unchanged")] | length' "$work/roundtrip.json")
  [[ "$errors" == 0 ]] || fail "round trip errors: $(jq -c '[.[] | select(.action == "error")]' "$work/roundtrip.json")"
  [[ "$changed" == "Deployment.apps/podinfo/podinfo" ]] || fail "round trip changed: $changed (only the Helm drift on podinfo was expected)"
  echo "round trip: $unchanged unchanged, only $changed changes (manual drift on a Helm release)"
fi

log "git server"
gw=$(docker network inspect kind -f '{{range .IPAM.Config}}{{if .Gateway}}{{.Gateway}} {{end}}{{end}}' | tr ' ' '\n' | grep -E '^[0-9.]+$' | head -1)
mkdir -p "$work/git"
"$work/githttp" -root "$work/git" -addr "0.0.0.0:$port" > "$work/githttp.log" 2>&1 &
server_pid=$!
new_repo() {
  git init -q --bare -b main "$work/git/$1.git"
  git -C "$work/git/$1.git" config http.receivepack true
  echo "http://$gw:$port/$1.git"
}

if want plain; then
  log "plain Kustomize with the apply script"
  new_dst
  (cd "$work" && g generate --context "kind-$src" --env uat="kind-$dst" --secrets local -o plain)
  kubectl --context "kind-$dst" create namespace shop
  kubectl --context "kind-$dst" apply -f "$work/plain/.secrets/shop/web-db.yaml"
  "$work/plain/apply-uat.sh" --context "kind-$dst" --dry-run > "$work/plain-dry.log" 2>&1 || { tail -20 "$work/plain-dry.log"; fail "apply script --dry-run"; }
  "$work/plain/apply-uat.sh" --context "kind-$dst" > "$work/plain.log" 2>&1 || { tail -30 "$work/plain.log"; fail "apply script"; }
  helm --kube-context "kind-$dst" status podinfo -n podinfo >/dev/null || fail "podinfo release missing"
  helm --kube-context "kind-$dst" status inhouse -n tools >/dev/null || fail "vendored inhouse release missing"
  kubectl --context "kind-$dst" -n shop get widget blue >/dev/null || fail "custom resource missing"
  kubectl --context "kind-$dst" -n shop rollout status deploy/web --timeout=300s
  kubectl --context "kind-$dst" -n payments rollout status deploy/api --timeout=300s
  pass "apply-uat.sh applied every app, Helm apps with helm"
fi

if want argocd; then
  log "Argo CD"
  new_dst
  url=$(new_repo argocd)
  (cd "$work" && g generate --context "kind-$src" --tool argocd --env uat="kind-$dst" \
    --repo-url "$url" --secrets local -o argocd --push)
  (cd "$work" && g check -c argocd/gitopsify.yaml)
  kubectl --context "kind-$dst" create namespace shop
  kubectl --context "kind-$dst" apply -f "$work/argocd/.secrets/shop/web-db.yaml"
  # kind has no ingress controller or metrics server: Ingresses and the HPA
  # can't become healthy, so wait for the sync and check the explanations.
  (cd "$work" && g bootstrap -o argocd --yes --timeout 25m --wait --wait-for synced --wait-timeout 20m) || fail "bootstrap --wait"
  (cd "$work" && g status -o argocd --format json) > "$work/argocd-status.json" || true
  [[ $(jq 'length' "$work/argocd-status.json") -ge 8 ]] || fail "too few applications"
  jq -e 'all(.[]; .sync == "Synced")' "$work/argocd-status.json" >/dev/null || fail "not every application synced"
  unexplained=$(jq -r '.[] | select(.ready | not) | select((.message // "") == "") | .name' "$work/argocd-status.json")
  [[ -z "$unexplained" ]] || fail "unhealthy applications without an explanation: $unexplained"
  jq -r '.[] | select(.ready | not) | "  \(.name): \(.message)"' "$work/argocd-status.json"
  kubectl --context "kind-$dst" -n shop get widget blue >/dev/null || fail "custom resource missing"
  kubectl --context "kind-$dst" -n shop rollout status deploy/web --timeout=300s
  pass "Argo CD: every application synced and healthy"
fi

if want flux; then
  log "Flux with sealed-secrets"
  new_dst
  kubectl --context "kind-$dst" apply -f https://github.com/bitnami-labs/sealed-secrets/releases/download/v0.40.0/controller.yaml
  kubectl --context "kind-$dst" -n kube-system rollout status deploy/sealed-secrets-controller --timeout=300s
  url=$(new_repo flux)
  (cd "$work" && g generate --context "kind-$src" --tool flux --env uat="kind-$dst" \
    --repo-url "$url" --secrets sealed-secrets -o flux --push)
  (cd "$work" && g bootstrap -o flux --yes --timeout 25m --wait --wait-timeout 20m) || fail "bootstrap --wait"
  [[ $(kubectl --context "kind-$dst" get helmreleases.helm.toolkit.fluxcd.io -A -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' | grep -c '^True$') == 2 ]] || fail "HelmReleases not ready"
  password=$(kubectl --context "kind-$dst" -n shop get secret web-db -o jsonpath='{.data.password}' | base64 -d)
  [[ "$password" == "s3cr3t" ]] || fail "unsealed password is '$password'"
  pass "Flux: every Kustomization and HelmRelease ready; SealedSecret unsealed"
fi

if want sops; then
  log "Flux with sops and age"
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
  if ! command -v sops >/dev/null; then
    v=$(curl -fsSL https://api.github.com/repos/getsops/sops/releases/latest | jq -r .tag_name)
    curl -fsSL -o "$work/bin/sops" "https://github.com/getsops/sops/releases/download/$v/sops-$v.$os.$arch" && chmod +x "$work/bin/sops"
  fi
  if ! command -v age-keygen >/dev/null; then
    v=$(curl -fsSL https://api.github.com/repos/FiloSottile/age/releases/latest | jq -r .tag_name)
    curl -fsSL "https://github.com/FiloSottile/age/releases/download/$v/age-$v-$os-$arch.tar.gz" | tar -xz -C "$work" && cp "$work/age/age" "$work/age/age-keygen" "$work/bin/"
  fi
  age-keygen -o "$work/age.key" 2>/dev/null
  new_dst
  url=$(new_repo sops)
  (cd "$work" && g generate --context "kind-$src" --tool flux --env uat="kind-$dst" \
    --repo-url "$url" --secrets sops --sops-age "$(age-keygen -y "$work/age.key")" -o sops --push)
  ! grep -rq "s3cr3t" "$work/sops/apps" || fail "plain-text secret in the repository"
  grep -rq "ENC\[AES256_GCM" "$work/sops/apps/shop" || fail "no encrypted Secret"
  (cd "$work" && g bootstrap -o sops --yes --timeout 25m --sops-age-key "$work/age.key" --wait --wait-timeout 20m) || fail "bootstrap --wait"
  password=$(kubectl --context "kind-$dst" -n shop get secret web-db -o jsonpath='{.data.password}' | base64 -d)
  [[ "$password" == "s3cr3t" ]] || fail "decrypted password is '$password'"
  pass "Flux decrypted the sops Secret"
fi
kind delete cluster --name "$dst" >/dev/null 2>&1 || true

if want takeover; then
  log "Argo CD takes over the source cluster"
  url=$(new_repo takeover)
  (cd "$work" && g generate --context "kind-$src" --tool argocd --adopt --env prod="kind-$src" \
    --repo-url "$url" -o takeover --push)
  (cd "$work" && g diff -c takeover/gitopsify.yaml --format json) > "$work/takeover-diff.json"
  changed=$(jq -r '[.[] | select(.action == "update")] | map(.object) | join(",")' "$work/takeover-diff.json")
  [[ "$changed" == "Deployment.apps/podinfo/podinfo" ]] || fail "takeover diff changed: $changed"
  pods() { kubectl --context "kind-$src" get pods -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}={.metadata.uid}{"\n"}{end}' | grep -Ev '^(kube-system|local-path-storage|argocd)/|^shop/cleanup-' | sort; }
  pods > "$work/pods-before.txt"
  (cd "$work" && g adopt -c takeover/gitopsify.yaml --helm-records backup-delete --yes --backup-dir helm-backup)
  ls "$work/helm-backup"/*/sh.helm.release.v1.* >/dev/null || fail "no Helm record backups"
  (cd "$work" && g bootstrap -o takeover --yes --timeout 25m --wait --wait-for synced --wait-timeout 20m) || fail "bootstrap --wait"
  pods > "$work/pods-after.txt"
  restarted=$(comm -23 "$work/pods-before.txt" "$work/pods-after.txt" | grep -v '^podinfo/' | grep -v '^shop/debug=' || true)
  [[ -z "$restarted" ]] || fail "pods restarted or deleted during the takeover: $restarted"
  pass "takeover: no pod restarted except the drifted podinfo release"
fi

log "PASS"
