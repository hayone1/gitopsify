#!/usr/bin/env bash
# The web UI as teams deploy it: the image built from this tree, the Helm
# chart in the kind cluster with two replicas on PostgreSQL, the cluster read
# through the pod's ServiceAccount as the signed-in person (impersonation),
# Keycloak for sign-in, and the browser tests of run.sh against it. Run after
# run.sh (which creates the cluster, Keycloak and the Git server).
set -euo pipefail
cd "$(dirname "$0")"
here=$PWD
root=$(cd ../../.. && pwd)
work=/tmp/webui-e2e
ctx=kind-gitopsify-ui
k() { kubectl --context "$ctx" "$@"; }
log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

# The host as pods and the browser both reach it (the kind network's gateway).
host=$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Gateway}} {{end}}' | tr ' ' '\n' | grep -E '^[0-9]+\.' | head -1)
node=$(docker inspect gitopsify-ui-control-plane -f '{{.NetworkSettings.Networks.kind.IPAddress}}')
base="http://$node:30080"
echo "gitopsify at $base, Keycloak and Git at $host"

log "image"
(cd "$root" && DOCKER_BUILDKIT=1 docker build -q -t gitopsify:e2e --build-arg VERSION=e2e ${SOPS_VERSION+--build-arg SOPS_VERSION=$SOPS_VERSION} . >/dev/null)
kind load docker-image gitopsify:e2e --name gitopsify-ui >/dev/null

log "PostgreSQL"
k create namespace gitopsify --dry-run=client -o yaml | k apply -f - >/dev/null
k -n gitopsify apply -f - >/dev/null <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: postgres}
spec:
  selector: {matchLabels: {app: postgres}}
  template:
    metadata: {labels: {app: postgres}}
    spec:
      containers:
        - name: postgres
          image: postgres:18-alpine
          env:
            - {name: POSTGRES_PASSWORD, value: e2e}
            - {name: POSTGRES_DB, value: gitopsify}
          ports: [{containerPort: 5432}]
---
apiVersion: v1
kind: Service
metadata: {name: postgres}
spec: {selector: {app: postgres}, ports: [{port: 5432}]}
---
apiVersion: v1
kind: Secret
metadata: {name: gitopsify-db}
stringData: {url: "postgres://postgres:e2e@postgres.gitopsify:5432/gitopsify?sslmode=disable"}
YAML
k -n gitopsify rollout status deploy/postgres --timeout=180s >/dev/null

log "Git server reachable from pods"
pkill -x githttp 2>/dev/null || true
sleep 1
rm -rf "$work/git" && mkdir -p "$work/git" && git init -q --bare -b main "$work/git/gitops.git"
git -C "$work/git/gitops.git" config http.receivepack true
(setsid nohup "$work/githttp" -root "$work/git" -addr "0.0.0.0:8099" >"$work/githttp.log" 2>&1 &)

log "chart"
cat >"$work/values.yaml" <<YAML
image: {repository: gitopsify, tag: e2e, pullPolicy: Never}
replicaCount: 2
storage:
  type: postgres
  postgres: {existingSecret: gitopsify-db}
service: {type: NodePort, nodePort: 30080}
persistence: {enabled: false}
server:
  externalURL: $base
  auth:
    oidc:
      issuer: http://$host:8180/realms/acme
      clientID: gitopsify
  rbac:
    defaultRole: viewer
    bindings:
      - {role: admin, groups: [platform]}
      - {role: operator, groups: [shop], clusters: [in-cluster]}
  git:
    allowHTTP: true
    repositories:
      - {url: "http://$host:8099/*", clusters: [in-cluster]}
inCluster: {name: in-cluster, impersonate: true}
rbac: {impersonate: true}
secrets: {oidcClientSecret: gitopsify-secret, sessionKey: e2e-session-key-0123456789abcdef0123}
YAML
helm upgrade --install gitopsify "$root/charts/gitopsify" --kube-context "$ctx" -n gitopsify -f "$work/values.yaml" --wait --timeout 5m >/dev/null
k -n gitopsify get pods -o wide
until curl -fsS "$base/healthz" >/dev/null 2>&1; do sleep 2; done

log "browser against the chart"
cd "$here"
rm -rf screenshots results
BASE_URL=$base GIT_URL="http://$host:8099/gitops.git" GIT_DIR="$work/git/gitops.git" npx playwright test --reporter=list
log "both replicas served"
for p in $(k -n gitopsify get pods -l app.kubernetes.io/name=gitopsify -o name); do
  echo "$p: $(k -n gitopsify logs "$p" | grep -c '"msg":"request"') requests"
done
k -n gitopsify exec deploy/postgres -- psql -U postgres -d gitopsify -tc "SELECT type, status FROM (SELECT data->>'type' AS type, status FROM gitopsify_runs) r"
