#!/usr/bin/env bash
# Runs the kind end-to-end suite (test/e2e/run.sh) on a throwaway Hetzner Cloud
# machine instead of this one: clusters, Argo CD and Flux need more CPU, memory
# and bandwidth than a laptop or WSL has to spare. Only the source code goes
# up and the log comes back. The machine is destroyed afterwards, also on
# failure or Ctrl-C.
#
#   ./e2e-on-vm.sh                 the whole suite
#   STAGES="auth argocd" ./e2e-on-vm.sh
#   SERVER_TYPE=cx53 ./e2e-on-vm.sh  a bigger machine (default cx33: 4 vCPU, 8 GB)
#   LOCATIONS="nbg1 hel1"           where to try, in order (default fsn1 nbg1 hel1)
#   ./e2e-on-vm.sh destroy         remove what an interrupted run left behind
#   RUN="make gifs" FETCH=docs/images ./e2e-on-vm.sh
#                                  run another command there and copy files back
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
state="$here/.state-runner"
label=gitopsify-e2e-runner
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

destroy() {
  log "destroying the runner"
  if [[ -f "$state/terraform.tfstate" ]]; then
    tf destroy -auto-approve -input=false -var run_id=x -var ssh_public_key=x -var allowed_cidr=0.0.0.0/32 \
      -var label="$label" >/dev/null || true
  fi
  local api=https://api.hetzner.cloud/v1 h="Authorization: Bearer $TF_VAR_hcloud_token"
  for kind in servers firewalls ssh_keys; do
    for id in $(curl -fsS -H "$h" "$api/$kind?label_selector=$label" | jq -r ".${kind}[].id"); do
      curl -fsS -X DELETE -H "$h" "$api/$kind/$id" >/dev/null || true
      echo "deleted leftover $kind $id"
    done
  done
  rm -f "$state/terraform.tfstate" "$state/terraform.tfstate.backup"
}
if [[ "${1:-}" == destroy ]]; then
  destroy
  exit 0
fi

exec 9>"$state/lock"
flock -n 9 || { echo "another run is using $state" >&2; exit 1; }
trap destroy EXIT

log "creating the runner"
run_id=$(date +%Y%m%d%H%M%S)
rm -f "$state"/ssh "$state"/ssh.pub "$state"/known_hosts
ssh-keygen -q -t ed25519 -N '' -f "$state/ssh"
cp "$here/main.tf" "$state/"
tf init -input=false >/dev/null
myip=$(curl -fsS https://api.ipify.org)
# Cloud locations run out of a server type now and then: try the next one.
for location in ${LOCATIONS:-fsn1 nbg1 hel1}; do
  if tf apply -auto-approve -input=false -var run_id="$run_id" -var ssh_public_key="$(cat "$state/ssh.pub")" \
    -var allowed_cidr="$myip/32" -var label="$label" -var install_k3s=false -var location="$location" \
    -var 'clusters=["runner"]' -var server_type="${SERVER_TYPE:-cx33}" > "$state/apply.log" 2>&1; then
    break
  fi
  grep -q resource_unavailable "$state/apply.log" || { cat "$state/apply.log"; exit 1; }
  echo "${SERVER_TYPE:-cx33} unavailable in $location"
done
ip=$(tf output -json ips | jq -r .runner)
ssh_() { ssh -i "$state/ssh" -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$state/known_hosts" -o ConnectTimeout=10 -o BatchMode=yes -o ServerAliveInterval=30 "root@$ip" "$@"; }
until ssh_ true 2>/dev/null; do sleep 5; done
echo "runner $ip"

log "installing the tools"
go_version=$(sed -n 's/^go //p' "$root/go.mod")
ssh_ "bash -s" <<EOF >/dev/null
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
cloud-init status --wait >/dev/null || true
apt-get update -qq && apt-get install -qq -y docker.io git jq curl make >/dev/null
curl -fsSL https://go.dev/dl/go$go_version.linux-amd64.tar.gz | tar -C /usr/local -xz
ln -sf /usr/local/go/bin/go /usr/local/bin/go
curl -fsSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/\$(curl -fsSL https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
chmod +x /usr/local/bin/kubectl
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash >/dev/null
GOBIN=/usr/local/bin go install sigs.k8s.io/kind@v0.33.0
git config --global user.name gitopsify-e2e && git config --global user.email e2e@example.com
EOF

log "uploading the source"
(cd "$root" && git ls-files --cached --others --exclude-standard | grep -v '^testdata/golden/' | tar -cz -T -) |
  ssh_ "rm -rf /src && mkdir /src && tar -xz -C /src"

run=${RUN:-./test/e2e/run.sh}
log "running $run on the runner"
set -o pipefail
ssh_ "cd /src && PATH=/usr/local/go/bin:\$PATH STAGES='${STAGES:-}' SKIP='${SKIP:-}' $run" 2>&1 | tee "$state/e2e.log"
if [[ -n "${FETCH:-}" ]]; then
  log "fetching $FETCH"
  # shellcheck disable=SC2086
  ssh_ "cd /src && tar -cz $FETCH" | tar -xz -C "$root"
fi
