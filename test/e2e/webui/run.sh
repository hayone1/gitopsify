#!/usr/bin/env bash
# End-to-end test of the web UI and server on a throwaway machine (see
# test/live/hetzner/devbox.sh): a kind cluster seeded like the CLI's e2e
# suite, Keycloak as the identity provider, a Git server, gitopsify server
# with the built UI, and a browser (Playwright) signing in as three people.
#
#   ./devbox.sh run test/e2e/webui/run.sh          (from /src on the machine)
set -euo pipefail
cd "$(dirname "$0")"
here=$PWD
root=$(cd ../../.. && pwd)
work=/tmp/webui-e2e
mkdir -p "$work"
log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

if ! kind get clusters | grep -qx gitopsify-ui; then
  log "kind cluster and its apps"
  kind create cluster --name gitopsify-ui --wait 120s >/dev/null
  ../setup-source.sh kind-gitopsify-ui >/dev/null
fi
# Kubernetes RBAC for the people gitopsify impersonates: shop and platform
# may read everything.
kubectl --context kind-gitopsify-ui apply -f - >/dev/null <<YAML
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: gitopsify-e2e-reader}
rules:
  - {apiGroups: ["*"], resources: ["*"], verbs: [get, list, watch]}
  - {nonResourceURLs: ["*"], verbs: [get]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: gitopsify-e2e-reader}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: gitopsify-e2e-reader}
subjects:
  - {apiGroup: rbac.authorization.k8s.io, kind: Group, name: shop}
  - {apiGroup: rbac.authorization.k8s.io, kind: Group, name: platform}
YAML

log "Keycloak"
if ! docker ps --format '{{.Names}}' | grep -qx keycloak; then
  docker rm -f keycloak >/dev/null 2>&1 || true
  docker run -d --name keycloak -p 8180:8080 -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e KC_BOOTSTRAP_ADMIN_PASSWORD=admin \
    -v "$here/realm.json:/opt/keycloak/data/import/realm.json:ro" quay.io/keycloak/keycloak:26.4 start-dev --import-realm >/dev/null
fi
until curl -fsS http://localhost:8180/realms/acme/.well-known/openid-configuration >/dev/null 2>&1; do sleep 3; done

log "Git server"
rm -rf "$work/git" && mkdir -p "$work/git" && git init -q --bare -b main "$work/git/gitops.git"
(cd "$root" && go build -o "$work/githttp" ./test/e2e/githttp)
pkill -x githttp 2>/dev/null || true
(setsid nohup "$work/githttp" -root "$work/git" -addr 127.0.0.1:8099 >"$work/githttp.log" 2>&1 &)

log "gitopsify with the UI"
(cd "$root" && make ui >/dev/null && make build >/dev/null)
cat >"$work/server.yaml" <<YAML
apiVersion: gitopsify.io/v1alpha1
kind: Server
metadata: {name: e2e}
spec:
  listen: 127.0.0.1:8080
  externalURL: http://localhost:8080
  dataDir: $work/data
  auth:
    oidc:
      issuer: http://localhost:8180/realms/acme
      clientID: gitopsify
      clientSecret: {value: gitopsify-secret}
    sessionKey: {value: e2e-session-key-0123456789abcdef0123}
    tokens:
      - {name: ci, role: operator, sha256: $(printf %s gtk_e2e_ci_token | sha256sum | cut -d' ' -f1), clusters: [kind]}
  rbac:
    defaultRole: viewer
    bindings:
      - {role: admin, groups: [platform]}
      - {role: operator, groups: [shop], clusters: [kind]}
  clusters:
    - name: kind
      description: The e2e source cluster
      impersonate: true
      access: {kubeconfig: $HOME/.kube/config, context: kind-gitopsify-ui}
  audit: {stdout: true}
YAML
rm -rf "$work/data"
pkill -f "gitopsify server" 2>/dev/null || true
(setsid nohup "$root/bin/gitopsify" server --config "$work/server.yaml" >"$work/server.log" 2>&1 &)
until curl -fsS http://localhost:8080/healthz >/dev/null 2>&1; do sleep 1; done

log "API with a token"
code=$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer gtk_e2e_ci_token' http://localhost:8080/api/v1/clusters)
[[ $code == 200 ]] || { echo "token: HTTP $code"; exit 1; }

log "browser"
cd "$here"
if [[ ! -d node_modules ]]; then
  npm install --silent --no-fund --no-audit @playwright/test@1 >/dev/null
  npx playwright install --with-deps chromium >/dev/null 2>&1
fi
rm -rf screenshots
GIT_DIR="$work/git/gitops.git" npx playwright test --reporter=list
log "pushed to the Git server"
git --git-dir="$work/git/gitops.git" log --oneline -3
git --git-dir="$work/git/gitops.git" ls-tree -r --name-only HEAD | head -20
log "audit log"
tail -5 "$work/data/audit/"*.jsonl
