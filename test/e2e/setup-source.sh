#!/usr/bin/env bash
# Builds a "ClickOps" source cluster for the end-to-end tests: objects created
# with kubectl create/expose/autoscale, client-side and server-side kubectl
# apply, Helm releases from a repository and from a local chart, a CRD with a
# custom resource, RBAC bound across scopes, and runtime leftovers (a finished
# Job, a bare Pod) that must not be migrated.
set -euo pipefail
CTX=${1:?usage: $0 <kube-context>}
cd "$(dirname "$0")"
k() { kubectl --context "$CTX" "$@"; }

k apply -f fixtures/widgets-crd.yaml
k wait --for=condition=Established crd/widgets.example.com --timeout=60s

# shop: built by hand
k create namespace shop
k label namespace shop team=shop pod-security.kubernetes.io/enforce=baseline
k apply -f fixtures/shop-apply.yaml
k -n shop create configmap web-config --from-literal=GREETING=hello --from-literal=BACKEND_URL=http://api.payments.svc.cluster.local
k -n shop create secret generic web-db --from-literal=password=s3cr3t --from-literal=username=web
k -n shop create deployment web --image=nginx:1.27 --replicas=2 --port=80
k -n shop set env deployment/web --from=configmap/web-config
k -n shop set env deployment/web --from=secret/web-db
k -n shop set serviceaccount deployment/web web
k -n shop set resources deployment/web --requests=cpu=50m,memory=32Mi
k -n shop expose deployment web --port=80 --target-port=80
k -n shop create ingress web --rule="web.test.example.com/*=web:80"
k -n shop autoscale deployment web --min=2 --max=5 --cpu-percent=80
k -n shop create role widget-editor --verb=get,list,update --resource=widgets.example.com
k -n shop create rolebinding web-widget-editor --role=widget-editor --serviceaccount=shop:web
k -n shop create cronjob cleanup --image=busybox:1.36 --schedule="0 * * * *" -- echo cleanup
k -n shop create job migrate-once --image=busybox:1.36 -- echo "done"
k -n shop run debug --image=busybox:1.36 --restart=Never -- sleep 3600

# payments: server-side apply
k apply --server-side --field-manager=platform-team -f fixtures/payments.yaml

# Helm: a chart from a repository, with a later manual edit (drift)
helm repo add podinfo https://stefanprodan.github.io/podinfo >/dev/null
helm repo update podinfo >/dev/null
helm --kube-context "$CTX" install podinfo podinfo/podinfo --version 6.9.2 -n podinfo --create-namespace \
  --set replicaCount=1 --set ui.message="hello from test" --set ingress.enabled=true \
  --set "ingress.hosts[0].host=podinfo.test.example.com" --set "ingress.hosts[0].paths[0].path=/" \
  --set "ingress.hosts[0].paths[0].pathType=Prefix" --wait
k -n podinfo set env deployment/podinfo PODINFO_UI_COLOR=#ff0000

# Helm: a local chart nobody published
tmp=$(mktemp -d)
helm create "$tmp/inhouse" >/dev/null
helm --kube-context "$CTX" install inhouse "$tmp/inhouse" -n tools --create-namespace --set replicaCount=1
rm -rf "$tmp"

k -n shop wait --for=condition=complete job/migrate-once --timeout=120s
k -n shop rollout status deployment/cache --timeout=180s
echo "source cluster ready"
