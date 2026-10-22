#!/usr/bin/env bash
# Live test of gitopsify against real, remote k3s clusters on Hetzner Cloud and
# a private GitHub repository:
#   - a ClickOps source cluster (k3s ships Traefik, ServiceLB, local-path and
#     its own CRDs, which must all stay out of the export)
#   - cluster access with client certificates, ServiceAccount tokens and CAs
#   - migrate to a new cluster with Argo CD and Sealed Secrets, pulling from
#     GitHub over SSH with a read-only deploy key
#   - generate + bootstrap to another cluster with Flux
#   - Argo CD taking over the source cluster without restarting pods
#
# Needs terraform, gh (logged in, repo scope), kubectl, helm, jq, git, ssh,
# curl and go, and TF_VAR_hcloud_token (read from the repository's .env).
#
#   ./run.sh          create the clusters, test, destroy them
#   ./run.sh destroy  destroy what a failed or kept run left behind
#   KEEP=1            keep the clusters (destroy later with ./run.sh destroy)
#   STAGES="argocd"   run only these of argocd, flux and takeover (fewer servers)
#   REPO=owner/name   GitHub repository to use (default <you>/gitopsify-e2e-live)
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
state="$here/.state"
mkdir -p "$state"
if [[ -f "$root/.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  . "$root/.env"
  set +a
fi
: "${TF_VAR_hcloud_token:?set TF_VAR_hcloud_token (or put it in .env)}"
tf() { terraform -chdir="$state" "$@"; }

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$*"; exit 1; }
pass() { printf '\033[32mok\033[0m %s\n' "$*"; }

destroy() {
  log "destroying the Hetzner resources"
  if [[ -f "$state/terraform.tfstate" ]]; then
    tf destroy -auto-approve -input=false -var run_id="$(cat "$state/run_id" 2>/dev/null || echo x)" \
      -var ssh_public_key=x -var allowed_cidr=0.0.0.0/32 >/dev/null || true
  fi
  # Belt and braces: anything labelled gitopsify-e2e that terraform lost track of.
  local api=https://api.hetzner.cloud/v1 h="Authorization: Bearer $TF_VAR_hcloud_token"
  for kind in servers firewalls ssh_keys; do
    for id in $(curl -fsS -H "$h" "$api/$kind?label_selector=gitopsify-e2e" | jq -r ".${kind}[].id"); do
      curl -fsS -X DELETE -H "$h" "$api/$kind/$id" >/dev/null || true
      echo "deleted leftover $kind $id"
    done
  done
  if [[ -f "$state/deploy-keys" ]]; then
    while read -r repo id; do gh repo deploy-key delete "$id" -R "$repo" >/dev/null 2>&1 || true; done < "$state/deploy-keys"
    rm -f "$state/deploy-keys"
  fi
  if [[ -f "$state/branches" && "${KEEP:-}" != 1 ]]; then
    while read -r repo branch; do gh api -X DELETE "repos/$repo/git/refs/heads/$branch" >/dev/null 2>&1 || true; done < "$state/branches"
    rm -f "$state/branches"
  fi
  rm -f "$state/terraform.tfstate" "$state/terraform.tfstate.backup"
}

if [[ "${1:-}" == destroy ]]; then
  destroy
  exit 0
fi

run_id=$(date +%Y%m%d%H%M%S)
rm -f "$state"/ssh "$state"/ssh.pub "$state"/known_hosts "$state"/*.yaml "$state"/ips.json
echo "$run_id" > "$state/run_id"
work=$(mktemp -d)
export KUBECONFIG="$state/kubeconfig"
rm -f "$KUBECONFIG"
want() { [[ -z "${STAGES:-}" || " $STAGES " == *" $1 "* ]]; }
clusters='"src"'
want argocd && clusters+=',"argocd"'
want flux && clusters+=',"flux"'
cleanup() {
  local rc=$?
  if ((rc != 0)) && [[ -d "${work:-}" ]]; then
    # Keep what explains the failure: reports, scans, diffs, statuses.
    rm -rf "$state/last-run" && mkdir -p "$state/last-run"
    find "$work" -maxdepth 3 \( -name '*.json' -o -name '*.txt' -o -name 'MIGRATION_REPORT.md' \) -exec cp --parents {} "$state/last-run" \; 2>/dev/null || true
    echo "diagnostics kept in $state/last-run"
  fi
  if [[ "${KEEP:-}" == 1 ]]; then
    echo "kept: KUBECONFIG=$KUBECONFIG, work dir $work; destroy with $0 destroy"
    return
  fi
  destroy
  rm -rf "$work"
}
trap cleanup EXIT

log "building gitopsify"
(cd "$root" && CGO_ENABLED=0 go build -o "$work/gitopsify" ./cmd/gitopsify)
g() { "$work/gitopsify" --no-input "$@"; }

log "creating the clusters"
ssh-keygen -q -t ed25519 -N '' -C "gitopsify-e2e-$run_id" -f "$state/ssh"
myip=$(curl -fsS https://api.ipify.org)
cp "$here/main.tf" "$state/"
tf init -input=false >/dev/null
tf apply -auto-approve -input=false -var run_id="$run_id" -var ssh_public_key="$(cat "$state/ssh.pub")" -var allowed_cidr="$myip/32" -var "clusters=[$clusters]" >/dev/null
tf output -json ips > "$state/ips.json"
cat "$state/ips.json"
ssh_() { ssh -i "$state/ssh" -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$state/known_hosts" -o ConnectTimeout=10 -o BatchMode=yes "root@$1" "${@:2}"; }

for name in $(jq -r 'keys[]' "$state/ips.json"); do
  ip=$(jq -r --arg n "$name" '.[$n]' "$state/ips.json")
  deadline=$((SECONDS + 600))
  until ssh_ "$ip" 'k3s kubectl get nodes 2>/dev/null | grep -q " Ready"' 2>/dev/null; do
    ((SECONDS < deadline)) || fail "k3s on $name ($ip) didn't become ready"
    sleep 10
  done
  # k3s names everything "default"; give each cluster its own names.
  ssh_ "$ip" cat /etc/rancher/k3s/k3s.yaml |
    sed -e "s/127.0.0.1/$ip/" -e "s/: default$/: hz-$name/" -e "s/name: default$/name: hz-$name/" > "$state/$name.yaml"
  echo "k3s ready on $name ($ip)"
done
KUBECONFIG=$(jq -r 'keys | map("'"$state"'/" + . + ".yaml") | join(":")' "$state/ips.json")
export KUBECONFIG
kubectl config view --flatten > "$state/kubeconfig"
export KUBECONFIG="$state/kubeconfig"
kubectl --context hz-src get nodes -o wide

log "source cluster"
export HELM_REPOSITORY_CONFIG="$work/helm/repositories.yaml" HELM_REPOSITORY_CACHE="$work/helm/cache"
"$root/test/e2e/setup-source.sh" hz-src

log "cluster access"
g doctor --context hz-src | tee "$work/doctor.txt" | grep -q "client certificate" || fail "doctor"
src_ip=$(jq -r .src "$state/ips.json")
kubectl config view --raw -o jsonpath='{.clusters[?(@.name=="hz-src")].cluster.certificate-authority-data}' | base64 -d > "$work/ca.pem"
kubectl --context hz-src create serviceaccount gitopsify-reader -n kube-system
kubectl --context hz-src create clusterrole gitopsify-reader --verb=get,list --resource='*.*'
kubectl --context hz-src create clusterrolebinding gitopsify-reader --clusterrole=gitopsify-reader --serviceaccount=kube-system:gitopsify-reader
kubectl --context hz-src -n kube-system create token gitopsify-reader --duration=2h > "$work/token"
g scan --context hz-src --format json > "$work/scan.json"
g scan --server "https://$src_ip:6443" --token-file "$work/token" --certificate-authority "$work/ca.pem" --format json > "$work/scan-token.json"
a=$(jq -c '[.apps[].name]' "$work/scan.json")
b=$(jq -c '[.apps[].name]' "$work/scan-token.json")
[[ "$a" == "$b" ]] || fail "token scan differs: $b, want $a"
pass "remote scan with a client certificate and with a ServiceAccount token + CA"

# k3s's own pieces stay out: Traefik and its CRDs, k3s CRDs, ServiceLB, local-path.
crds=$(jq -r '.apps[] | select(.name == "crds") | .objects | join(",")' "$work/scan.json")
[[ "$crds" == "CustomResourceDefinition.apiextensions.k8s.io/widgets.example.com" ]] || fail "crds app: $crds"
jq -e '.excludedByRule["platform-crds"] > 0' "$work/scan.json" >/dev/null || fail "k3s CRDs not recognized"
if jq -r '.apps[].objects[]' "$work/scan.json" | grep -Eqi 'traefik|svclb|local-path'; then fail "k3s components exported"; fi
pass "k3s components and CRDs left out"

log "GitHub repository"
repo=${REPO:-$(gh api user -q .login)/gitopsify-e2e-live}
gh repo view "$repo" >/dev/null 2>&1 || gh repo create "$repo" --private --add-readme --description "gitopsify live test output" >/dev/null
ssh-keygen -q -t ed25519 -N '' -C "gitopsify-e2e-writer-$run_id" -f "$work/writer"
ssh-keygen -q -t ed25519 -N '' -C "gitopsify-e2e-reader-$run_id" -f "$work/reader"
add_key() { # <file> <title> [--allow-write]
  gh repo deploy-key add "$1.pub" -R "$repo" --title "$2" "${@:3}" >/dev/null
  gh repo deploy-key list -R "$repo" --json id,title -q ".[] | select(.title == \"$2\") | \"$repo \(.id)\"" >> "$state/deploy-keys"
}
add_key "$work/writer" "gitopsify-e2e-writer-$run_id" --allow-write
add_key "$work/reader" "gitopsify-e2e-reader-$run_id"
ssh-keyscan -t ed25519,rsa,ecdsa github.com 2>/dev/null > "$work/known_hosts"
export GIT_SSH_COMMAND="ssh -i $work/writer -o IdentitiesOnly=yes -o UserKnownHostsFile=$work/known_hosts"
git config --global user.email >/dev/null || export GIT_AUTHOR_NAME=gitopsify-e2e GIT_AUTHOR_EMAIL=e2e@example.com GIT_COMMITTER_NAME=gitopsify-e2e GIT_COMMITTER_EMAIL=e2e@example.com
url="git@github.com:$repo.git"
branch_for() { echo "$repo run-$run_id-$1" >> "$state/branches"; echo "run-$run_id-$1"; }

if want argocd; then
log "Argo CD with Sealed Secrets, via migrate"
kubectl --context hz-argocd apply -f https://github.com/bitnami-labs/sealed-secrets/releases/download/v0.40.0/controller.yaml >/dev/null
kubectl --context hz-argocd -n kube-system rollout status deploy/sealed-secrets-controller --timeout=300s
branch=$(branch_for argocd)
(cd "$work" && g migrate --context hz-src --env uat=hz-argocd --tool argocd --secrets sealed-secrets \
  --repo-url "$url" --revision "$branch" -o argocd --push --yes --timeout 25m --ssh-key "$work/reader" --known-hosts "$work/known_hosts" \
  --wait --wait-timeout 25m) || fail "migrate to Argo CD"
password=$(kubectl --context hz-argocd -n shop get secret web-db -o jsonpath='{.data.password}' | base64 -d)
[[ "$password" == s3cr3t ]] || fail "unsealed password is '$password'"
kubectl --context hz-argocd -n shop get widget blue >/dev/null || fail "custom resource missing"
g status -o "$work/argocd" --env uat >/dev/null || fail "status"
pass "Argo CD synced everything from GitHub; SealedSecret unsealed"
fi

if want flux; then
log "Flux, via generate + bootstrap"
branch=$(branch_for flux)
(cd "$work" && g generate --context hz-src --env uat=hz-flux --tool flux --secrets local \
  --repo-url "$url" --revision "$branch" -o flux --push)
kubectl --context hz-flux create namespace shop
kubectl --context hz-flux apply -f "$work/flux/.secrets/shop/web-db.yaml"
(cd "$work" && g check -c flux/gitopsify.yaml) || fail "check"
(cd "$work" && g bootstrap -o flux --yes --timeout 25m --ssh-key "$work/reader" --known-hosts "$work/known_hosts" --wait --wait-timeout 25m) || fail "bootstrap Flux"
kubectl --context hz-flux -n shop rollout status deploy/web --timeout=300s
pass "Flux synced everything from GitHub"
fi

if want takeover; then
log "Argo CD takes over the source cluster"
branch=$(branch_for takeover)
(cd "$work" && g generate --context hz-src --env prod=hz-src --tool argocd --adopt \
  --repo-url "$url" --revision "$branch" -o takeover --push)
(cd "$work" && g diff -c takeover/gitopsify.yaml --format json) > "$work/takeover-diff.json"
changed=$(jq -r '[.[] | select(.action == "update")] | map(.object) | join(",")' "$work/takeover-diff.json")
if [[ "$changed" != "Deployment.apps/podinfo/podinfo" ]]; then
  jq -r '.[] | select(.action == "update") | .diff' "$work/takeover-diff.json"
  fail "takeover diff changed: $changed"
fi
pods() { kubectl --context hz-src get pods -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}={.metadata.uid}{"\n"}{end}' | grep -Ev '^(kube-system|argocd)/|^shop/cleanup-' | sort; }
pods > "$work/pods-before.txt"
(cd "$work" && g adopt -c takeover/gitopsify.yaml --helm-records backup-delete --yes --backup-dir helm-backup)
(cd "$work" && g bootstrap -o takeover --yes --timeout 25m --ssh-key "$work/reader" --known-hosts "$work/known_hosts" --wait --wait-timeout 25m) || fail "bootstrap takeover"
pods > "$work/pods-after.txt"
restarted=$(comm -23 "$work/pods-before.txt" "$work/pods-after.txt" | grep -v '^podinfo/' | grep -v '^shop/debug=' || true)
[[ -z "$restarted" ]] || fail "pods restarted during the takeover: $restarted"
pass "takeover without restarts"
fi

log "PASS"
