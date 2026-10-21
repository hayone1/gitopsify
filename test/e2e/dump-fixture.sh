#!/usr/bin/env bash
# Regenerates testdata/clusters/clickops.yaml from a cluster prepared with
# setup-source.sh: every listable object except Events, with managed fields,
# minus Secret data and Pods of system namespaces. Needs python3 with PyYAML.
set -euo pipefail
CTX=${1:?usage: $0 <kube-context>}
out="$(dirname "$0")/../../testdata/clusters/clickops.yaml"
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
resources=$(kubectl --context "$CTX" api-resources --verbs=list -o name | grep -v '^events' | paste -sd,)
kubectl --context "$CTX" get "$resources" -A -o yaml --show-managed-fields > "$tmp"
python3 - "$tmp" "$out" <<'PY'
import sys, yaml
src, dst = sys.argv[1], sys.argv[2]
d = yaml.safe_load(open(src))
system = {"kube-system", "kube-public", "kube-node-lease", "local-path-storage"}
items = []
for it in d["items"]:
    ns = it["metadata"].get("namespace")
    if it["kind"] == "Pod" and ns in system:
        continue
    if it["kind"] == "Secret" and ns in system:
        it.pop("data", None)
        it.pop("stringData", None)
    items.append(it)
d["items"] = items
with open(dst, "w") as f:
    f.write("# Objects of a kind cluster built by test/e2e/setup-source.sh (kubectl create/apply,\n# Helm releases, a CRD with a custom resource). Secret data of system\n# namespaces and system Pods were removed. Regenerate with test/e2e/dump-fixture.sh.\n")
    yaml.safe_dump(d, f, sort_keys=False)
PY
echo "wrote $out"
