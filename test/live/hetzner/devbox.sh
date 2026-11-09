#!/usr/bin/env bash
# A throwaway Hetzner Cloud machine to develop on when this one is short of
# CPU, memory or disk: builds, the web UI's Node toolchain, kind clusters. It
# stays up until "down" (unlike e2e-on-vm.sh, which runs one command).
#
#   ./devbox.sh up                 create it and install the tools
#   ./devbox.sh sync               copy the working tree to /src (and siblings in UPLOAD)
#   ./devbox.sh run 'make test'    run a command in /src (after a sync)
#   ./devbox.sh fetch ui/pnpm-lock.yaml   copy paths from /src back here
#   ./devbox.sh ssh [-L 8080:localhost:8080]
#   ./devbox.sh down               destroy it (also removes leftovers by label)
#
# SERVER_TYPE (default cx33: 4 vCPU, 8 GB), LOCATIONS as in e2e-on-vm.sh.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
state="$here/.state-devbox"
label=gitopsify-devbox
mkdir -p "$state"
if [[ -f "$root/.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  . "$root/.env"
  set +a
fi
: "${TF_VAR_hcloud_token:?set TF_VAR_hcloud_token (or put it in .env)}"
tf() { terraform -chdir="$state" "$@"; }
log() { printf '\n\033[1m== %s\033[0m\n' "$*" >&2; }
ip() { tf output -json ips 2>/dev/null | jq -r '.devbox // empty'; }
ssh_() {
  ssh -i "$state/ssh" -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$state/known_hosts" \
    -o ConnectTimeout=30 -o ConnectionAttempts=3 -o BatchMode=yes -o ServerAliveInterval=30 "root@$(ip)" "$@"
}

up() {
  if [[ -f "$state/terraform.tfstate" ]] && [[ -n "$(ip)" ]] && ssh_ true 2>/dev/null; then
    echo "devbox $(ip) is up"
    return
  fi
  log "creating the devbox"
  rm -f "$state"/ssh "$state"/ssh.pub "$state"/known_hosts
  ssh-keygen -q -t ed25519 -N '' -f "$state/ssh"
  cp "$here/main.tf" "$state/"
  tf init -input=false >/dev/null
  myip=$(curl -fsS https://api.ipify.org)
  for location in ${LOCATIONS:-fsn1 nbg1 hel1}; do
    if tf apply -auto-approve -input=false -var run_id="$(date +%Y%m%d%H%M%S)" -var ssh_public_key="$(cat "$state/ssh.pub")" \
      -var allowed_cidr="$myip/32" -var label="$label" -var install_k3s=false -var location="$location" \
      -var 'clusters=["devbox"]' -var server_type="${SERVER_TYPE:-cx33}" >"$state/apply.log" 2>&1; then
      break
    fi
    grep -q resource_unavailable "$state/apply.log" || { cat "$state/apply.log"; exit 1; }
    echo "${SERVER_TYPE:-cx33} unavailable in $location" >&2
  done
  [[ -n "$(ip)" ]] || { echo "no location has ${SERVER_TYPE:-cx33}: try SERVER_TYPE=cpx32 or other LOCATIONS" >&2; exit 1; }
  until ssh_ true 2>/dev/null; do sleep 5; done
  log "installing the tools on $(ip)"
  go_version=$(sed -n 's/^go //p' "$root/go.mod")
  node_version=${NODE_VERSION:-v26.13.0}
  ssh_ "bash -s" <<EOF >/dev/null
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
cloud-init status --wait >/dev/null || true
apt-get update -qq && apt-get install -qq -y docker.io docker-buildx git jq curl make rsync unzip xz-utils libatomic1 >/dev/null
curl -fsSL https://go.dev/dl/go$go_version.linux-amd64.tar.gz | tar -C /usr/local -xz
ln -sf /usr/local/go/bin/go /usr/local/bin/go
curl -fsSL https://nodejs.org/dist/$node_version/node-$node_version-linux-x64.tar.xz | tar -C /usr/local -xJ --strip-components=1
npm install -g -s pnpm >/dev/null
curl -fsSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/\$(curl -fsSL https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
chmod +x /usr/local/bin/kubectl
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash >/dev/null
GOBIN=/usr/local/bin go install sigs.k8s.io/kind@v0.33.0
git config --global user.name gitopsify-dev && git config --global user.email dev@example.com
EOF
  echo "devbox $(ip) is up"
}

sync_() {
  log "syncing the working tree to /src"
  (cd "$root" && git ls-files --cached --others --exclude-standard) >"$state/files"
  rsync -az --delete-after --files-from="$state/files" -e "ssh -i $state/ssh -o UserKnownHostsFile=$state/known_hosts -o BatchMode=yes" \
    "$root/" "root@$(ip):/src/" --filter='P ui/node_modules/' --filter='P ui/.nuxt/' --filter='P ui/.output/' --filter='P web/dist/' --filter='P bin/'
  for dir in ${UPLOAD:-}; do
    abs=$(cd "$root" && cd "$dir" && pwd)
    name=$(basename "$abs")
    (cd "$abs" && git ls-files --cached --others --exclude-standard) >"$state/files-$name"
    rsync -az --files-from="$state/files-$name" -e "ssh -i $state/ssh -o UserKnownHostsFile=$state/known_hosts -o BatchMode=yes" \
      "$abs/" "root@$(ip):/$name/"
  done
}

case "${1:-}" in
up) up ;;
sync) sync_ ;;
run)
  shift
  ssh_ "cd /src && export PATH=/usr/local/go/bin:/usr/local/bin:\$PATH && $*"
  ;;
fetch)
  shift
  # shellcheck disable=SC2029
  ssh_ "cd /src && tar -cz $*" | tar -xz -C "$root"
  ;;
ssh)
  shift
  ssh -i "$state/ssh" -o UserKnownHostsFile="$state/known_hosts" "$@" "root@$(ip)"
  ;;
ip) ip ;;
down)
  log "destroying the devbox"
  if [[ -f "$state/terraform.tfstate" ]]; then
    tf destroy -auto-approve -input=false -var run_id=x -var ssh_public_key=x -var allowed_cidr=0.0.0.0/32 \
      -var label="$label" >/dev/null || true
  fi
  api=https://api.hetzner.cloud/v1 h="Authorization: Bearer $TF_VAR_hcloud_token"
  for kind in servers firewalls ssh_keys; do
    for id in $(curl -fsS -H "$h" "$api/$kind?label_selector=$label" | jq -r ".${kind}[].id"); do
      curl -fsS -X DELETE -H "$h" "$api/$kind/$id" >/dev/null || true
      echo "deleted leftover $kind $id"
    done
  done
  rm -f "$state/terraform.tfstate" "$state/terraform.tfstate.backup"
  ;;
*)
  sed -n '2,13p' "$0"
  exit 2
  ;;
esac
