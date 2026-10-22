---
name: gitopsify-takeover
description: Have Argo CD or Flux take over a running Kubernetes cluster in place with gitopsify, without restarting or deleting anything - generate --adopt, a clean diff, retiring Helm release records, bootstrap, verifying that no pod restarted, and turning pruning on later. Use when the target of the migration is the cluster the objects already run in.
---

# In-place takeover

The goal: the first sync changes nothing that matters. Nothing is deleted,
pods keep running, and only deliberate differences (for example drift on a
Helm release) are corrected, with the person's agreement.

## 1. Generate with takeover settings

```sh
gitopsify generate --context prod --env prod=prod --tool argocd --adopt \
  --repo-url <url> --secrets sealed-secrets
```

`--adopt` turns automated pruning off (Argo CD), applies with server-side
apply without migrating client-side fields
(`ClientSideApplyMigration=false`), never prunes or deletes PVCs, PVs,
Namespaces and CRDs (`adopt.protectKinds`), and puts Flux Helm drift detection
in `warn` mode. Flux keeps `prune: true`: it only prunes what it applied
itself. Portability warnings are skipped, since the environment stays.

## 2. Prove the first sync is a no-op

```sh
gitopsify diff --env prod
```

Every `~` (change) needs an explanation:

- Helm drift (someone ran `kubectl edit` on a chart object): keep the live
  value by putting it into the values, or accept that Git wins.
- Fields owned by a controller the cleaner removed: add the field manager to
  `clean.controllerManagers` or the field to a transform, then regenerate.
- Objects only in Git (`+`): expected for generated SealedSecrets; anything
  else means the export picked up something new.

Repeat until the diff shows only what the person agreed to. In CI:
`gitopsify diff --env prod --exit-code` fails when anything would change.

## 3. Record the baseline

Pod UIDs prove afterwards that nothing restarted:

```sh
kubectl --context prod get pods -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,UID:.metadata.uid --no-headers | sort > pods-before.txt
```

Don't compare Deployment `generation`: Kubernetes 1.37 bumps it when the
tracking annotation is added, without a rollout.

## 4. Retire stale Helm release records (Argo CD and plain manifests)

Argo CD renders charts itself, so the cluster's Helm records go stale and a
later `helm upgrade` would fight it. With Flux, HelmReleases reuse the records;
keep them.

```sh
gitopsify adopt --env prod --dry-run
gitopsify adopt --env prod --helm-records backup-delete    # asks first; backups in .gitopsify-backup/
```

## 5. Push, bootstrap, watch

Push (the `gitopsify-git-remote` skill), then with the person's go-ahead:

```sh
gitopsify bootstrap --env prod --wait
gitopsify status --env prod
```

If Argo CD or Flux already runs there, apply only the root object instead of
installing a second instance.

## 6. Verify nothing restarted

```sh
kubectl --context prod get pods -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,UID:.metadata.uid --no-headers | sort > pods-after.txt
diff pods-before.txt pods-after.txt   # only expected changes, such as a drifted Helm Deployment rolling back
```

## 7. Later: turn pruning on

Once the apps have synced cleanly for a while and the person agrees, set
`output.argocd.prune: true` (or remove `--adopt` and regenerate), review the
`diff`, push. Pruning deletes objects that are in the cluster but not in Git,
so first check with Argo CD's "orphaned resources" view or `gitopsify scan`
that nothing important is missing from Git.

## Rollback

Takeover is reversible because nothing was deleted: suspend the GitOps tool
(`argocd app set <app> --sync-policy none`, `flux suspend kustomization --all`),
and restore Helm records from `.gitopsify-backup/helm-records` with
`kubectl apply -f` if Helm should manage a release again.
