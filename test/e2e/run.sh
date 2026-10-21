#!/usr/bin/env bash
# End-to-end test of gitopsify against real clusters:
#   1. a "ClickOps" source cluster (setup-source.sh)
#   2. round trip: the generated manifests, dry-run against the source itself,
#      must leave every object unchanged except the deliberate Helm drift
#   3. Argo CD: generate --push to a local Git server, bootstrap a target
#      cluster and wait for every Application to sync
#   4. Flux with sealed-secrets: same, on a fresh target, and check the
#      sealed Secret decrypts to the original value
#
# Needs docker, kind, kubectl, helm, jq, git and go. Takes about 15 minutes.
#   KEEP=1           keep the clusters and Git server afterwards
#   SKIP=argocd|flux skip one of the GitOps flows
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

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$*"; exit 1; }

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

log "building"
(cd "$root" && go build -o "$bin" ./cmd/gitopsify && go build -o "$work/githttp" ./test/e2e/githttp)

log "source cluster"
kind create cluster --name "$src" --wait 180s
"$here/setup-source.sh" "kind-$src"

log "round trip against the source"
"$bin" diff --no-input --context "kind-$src" --env same="kind-$src" --format json > "$work/roundtrip.json"
changed=$(jq -r '[.[] | select(.action == "update")] | map(.object) | join(",")' "$work/roundtrip.json")
errors=$(jq '[.[] | select(.action == "error")] | length' "$work/roundtrip.json")
unchanged=$(jq '[.[] | select(.action == "unchanged")] | length' "$work/roundtrip.json")
[[ "$errors" == 0 ]] || fail "round trip errors: $(jq -c '[.[] | select(.action == "error")]' "$work/roundtrip.json")"
[[ "$changed" == "Deployment.apps/podinfo/podinfo" ]] || fail "round trip changed: $changed (only the Helm drift on podinfo was expected)"
echo "round trip: $unchanged unchanged, only $changed changes (manual drift on a Helm release)"

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

apps_synced() {
  local out
  out=$(kubectl --context "kind-$dst" -n argocd get applications -o jsonpath='{range .items[*]}{.status.sync.status}{"\n"}{end}')
  [[ $(grep -c . <<<"$out") -ge 8 ]] && ! grep -qv '^Synced$' <<<"$out"
}

kustomizations_ready() {
  local out
  out=$(kubectl --context "kind-$dst" -n flux-system get kustomizations.kustomize.toolkit.fluxcd.io -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}')
  [[ $(grep -c . <<<"$out") -ge 8 ]] && ! grep -qv '^True$' <<<"$out"
}

helmreleases_ready() {
  [[ $(kubectl --context "kind-$dst" get helmreleases.helm.toolkit.fluxcd.io -A -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' | grep -c '^True$') == 2 ]]
}

if [[ "${SKIP:-}" != argocd ]]; then
  log "Argo CD"
  kind create cluster --name "$dst" --wait 180s
  url=$(new_repo argocd)
  (cd "$work" && "$bin" generate --no-input --context "kind-$src" --tool argocd --env uat="kind-$dst" \
    --repo-url "$url" --secrets local -o argocd --push)
  (cd "$work" && "$bin" check --no-input -c argocd/gitopsify.yaml)
  kubectl --context "kind-$dst" create namespace shop
  kubectl --context "kind-$dst" apply -f "$work/argocd/.secrets/shop/web-db.yaml"
  (cd "$work" && "$bin" bootstrap --no-input -o argocd --yes)
  wait_for "Argo CD applications to sync" 900 apps_synced
  kubectl --context "kind-$dst" -n shop get widget blue >/dev/null || fail "custom resource missing"
  kubectl --context "kind-$dst" -n shop rollout status deploy/web --timeout=300s
  echo "Argo CD: every application synced"
  kind delete cluster --name "$dst"
fi

if [[ "${SKIP:-}" != flux ]]; then
  log "Flux with sealed-secrets"
  kind create cluster --name "$dst" --wait 180s
  kubectl --context "kind-$dst" apply -f https://github.com/bitnami-labs/sealed-secrets/releases/download/v0.40.0/controller.yaml
  kubectl --context "kind-$dst" -n kube-system rollout status deploy/sealed-secrets-controller --timeout=300s
  url=$(new_repo flux)
  (cd "$work" && "$bin" generate --no-input --context "kind-$src" --tool flux --env uat="kind-$dst" \
    --repo-url "$url" --secrets sealed-secrets -o flux --push)
  (cd "$work" && "$bin" bootstrap --no-input -o flux --yes)
  wait_for "Flux Kustomizations to become ready" 900 kustomizations_ready
  wait_for "HelmReleases to become ready" 600 helmreleases_ready
  password=$(kubectl --context "kind-$dst" -n shop get secret web-db -o jsonpath='{.data.password}' | base64 -d)
  [[ "$password" == "s3cr3t" ]] || fail "unsealed password is '$password'"
  echo "Flux: every Kustomization and HelmRelease ready; SealedSecret unsealed"
fi

log "PASS"
