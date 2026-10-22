# Taking over a running cluster

Sometimes there is no new cluster: you want Argo CD or Flux to manage the
cluster you already have, without an outage. gitopsify's takeover mode makes
the first sync a no-op, apart from changes you agreed to.

## 1. Generate with `--adopt`

```sh
gitopsify generate --context prod --env prod=prod --tool argocd --adopt --repo-url ...
```

`--adopt`:

- turns Argo CD's automated **pruning off**, so nothing the export missed gets
  deleted (Flux keeps pruning on: it only prunes what it applied itself);
- applies with server-side apply and `ClientSideApplyMigration=false`, so
  fields that `kubectl apply` and other tools own aren't removed on the first
  sync;
- never prunes or deletes PVCs, PVs, Namespaces and CRDs
  (`adopt.protectKinds`);
- puts Flux's Helm drift detection in `warn` mode;
- skips portability warnings, since the environment doesn't change.

## 2. Make the diff clean

```sh
gitopsify diff --env prod
```

`diff` server-side dry-runs every generated object against the cluster, as a
new field manager, exactly like the first sync will. Every `~` line needs an
explanation. The usual one is Helm drift: someone changed a chart's object by
hand, and Git would reset it. Keep the change by moving it into the values, or
accept the reset. Iterate until the diff shows only what you expect. In CI,
`--exit-code` fails when anything would change.

## 3. Retire stale Helm records

Argo CD renders charts itself, so Helm's own release records go stale, and a
later `helm upgrade` from someone's laptop would fight Argo CD:

```sh
gitopsify adopt --env prod --dry-run
gitopsify adopt --env prod --helm-records backup-delete   # backs up to .gitopsify-backup/ first
```

With Flux, HelmReleases keep using the records; `adopt` leaves them alone.

## 4. Bootstrap and verify

Note the pod UIDs, bootstrap, and compare:

```sh
kubectl get pods -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,UID:.metadata.uid --no-headers | sort > before.txt
gitopsify bootstrap --env prod --wait
kubectl get pods -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,UID:.metadata.uid --no-headers | sort > after.txt
diff before.txt after.txt
```

Only the workloads you agreed to change (such as a drifted Helm release rolling
back) should differ. gitopsify's own tests check exactly this, on kind and on
k3s. Don't judge by Deployment `generation`: Kubernetes 1.37 bumps it when Argo
CD adds its tracking annotation, without rolling anything out.

## 5. Later: turn pruning on

After the apps have synced cleanly for a while, remove `--adopt` (or set
`output.argocd.prune: true`), regenerate, review the diff and push. From then
on, deleting a file from Git deletes the object.

## Rolling back

Nothing was deleted, so rolling back is cheap: stop the GitOps tool
(`argocd app set <app> --sync-policy none`, or `flux suspend kustomization
--all`) and, if Helm should manage a release again, restore its records from
`.gitopsify-backup/helm-records` with `kubectl apply -f`.
