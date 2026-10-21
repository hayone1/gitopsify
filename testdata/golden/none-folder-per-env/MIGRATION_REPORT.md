# Migration report

Source: offline (clickops.yaml). Target environments: uat, prod.

## Summary

| | Count |
|---|---:|
| Objects discovered | 374 |
| Objects migrated | 29 |
| Objects excluded | 345 |
| Helm releases migrated | 2 |
| Helm releases skipped | 0 |
| Apps | 7 |
| Secrets that need action | 1 |
| Warnings | 2 |

## Helm releases

| Release | Chart | Version | Mode | Repository | Resolved by | Notes |
|---|---|---|---|---|---|---|
| podinfo/podinfo | podinfo | 6.9.2 | chart | `https://stefanprodan.github.io/podinfo` | config | Deployment.apps/podinfo/podinfo differs from the chart's manifest at spec.template.spec.containers[0].env[1].value; the GitOps tool will reset it to the chart's value |
| tools/inhouse | inhouse | 0.1.0 | vendor | — | — | chart repository unknown; the chart is vendored from the release. Set helm.repositories.inhouse to reference the upstream chart instead |

Modes: **chart** pulls the chart from its repository; **vendor** writes the chart, rebuilt from the release, into `apps/<app>/charts/`; **manifests** writes the release's objects as plain manifests (used when the chart can be neither found nor rebuilt).

## Apps

| Wave | App | Phase | Depends on | Objects |
|---:|---|---|---|---:|
| -30 | crds | crds |  | 1 |
| -20 | namespaces | namespaces |  | 4 |
| -10 | cluster | cluster |  | 3 |
| 0 | inhouse | apps |  | 3 |
| 0 | payments | apps |  | 3 |
| 0 | podinfo | apps |  | 3 |
| 0 | shop | apps | crds | 12 |

## Secrets that need action

- shop/web-db (Opaque, keys: password, username): not migrated; create it in the target cluster

## Warnings

- Helm release `podinfo/podinfo`: Deployment.apps/podinfo/podinfo differs from the chart's manifest at spec.template.spec.containers[0].env[1].value; the GitOps tool will reset it to the chart's value
- Helm release `tools/inhouse`: chart repository unknown; the chart is vendored from the release. Set helm.repositories.inhouse to reference the upstream chart instead

## Excluded objects

### bare-pods (1)

Pods without an owner, usually created ad hoc with kubectl run; wrap them in a Deployment or Job to migrate them. Disable with `scope.disableDefaultRules: [bare-pods]`.

- `Pod/shop/debug` — pod without a controller

### cluster-provided-config (5)

ConfigMaps every namespace receives automatically (kube-root-ca.crt, istio-ca-root-cert, openshift-service-ca.crt). Disable with `scope.disableDefaultRules: [cluster-provided-config]`.

- `ConfigMap/default/kube-root-ca.crt` — injected into every namespace
- `ConfigMap/payments/kube-root-ca.crt` — injected into every namespace
- `ConfigMap/podinfo/kube-root-ca.crt` — injected into every namespace
- `ConfigMap/shop/kube-root-ca.crt` — injected into every namespace
- `ConfigMap/tools/kube-root-ca.crt` — injected into every namespace

### completed-jobs (1)

Jobs without an owner that already finished; migrating them would run them again. Disable with `scope.disableDefaultRules: [completed-jobs]`.

- `Job.batch/shop/migrate-once` — completed at 2026-10-21T17:09:46Z

### controller-endpoints (5)

Endpoints and EndpointSlices maintained for Services with selectors; those of selector-less Services are kept. Disable with `scope.disableDefaultRules: [controller-endpoints]`.

- `EndpointSlice.discovery.k8s.io/default/kubernetes` — maintained by the API server
- `Endpoints/payments/api` — maintained for Service api
- `Endpoints/podinfo/podinfo` — maintained for Service podinfo
- `Endpoints/shop/web` — maintained for Service web
- `Endpoints/tools/inhouse` — maintained for Service inhouse

### default-namespace (1)

The default Namespace object, which exists in every cluster (its contents are still migrated). Disable with `scope.disableDefaultRules: [default-namespace]`.

- `Namespace/default` — exists in every cluster

### default-serviceaccount (5)

The default ServiceAccount of each namespace, unless it was customized (image pull secrets, annotations such as workload identity). Disable with `scope.disableDefaultRules: [default-serviceaccount]`.

- `ServiceAccount/default/default` — not customized
- `ServiceAccount/payments/default` — not customized
- `ServiceAccount/podinfo/default` — not customized
- `ServiceAccount/shop/default` — not customized
- `ServiceAccount/tools/default` — not customized

### default-storageclass (1)

The cluster's default StorageClass, which the target platform provides. Disable with `scope.disableDefaultRules: [default-storageclass]`.

- `StorageClass.storage.k8s.io/standard` — default StorageClass

### generated-kinds (14)

Kinds that controllers create and maintain (ReplicaSets, Leases, Nodes, cert-manager CertificateRequests, Cilium endpoints, policy reports, ...). Disable with `scope.disableDefaultRules: [generated-kinds]`.

- `ComponentStatus/controller-manager` — deprecated, read-only status
- `ComponentStatus/etcd-0` — deprecated, read-only status
- `ComponentStatus/scheduler` — deprecated, read-only status
- `Node/gitopsify-src-control-plane` — cluster infrastructure
- `CertificateSigningRequest.certificates.k8s.io/csr-dwj9v` — one-off certificate requests
- `ClusterTrustBundle.certificates.k8s.io/kubernetes.io:kube-apiserver-serving:8ba4f1e2def31afb182c03c8` — published by Kubernetes for signer kubernetes.io/kube-apiserver-serving
- `IPAddress.networking.k8s.io/10.96.0.1` — allocated Service IPs
- `IPAddress.networking.k8s.io/10.96.0.10` — allocated Service IPs
- `IPAddress.networking.k8s.io/10.96.173.216` — allocated Service IPs
- `IPAddress.networking.k8s.io/10.96.22.202` — allocated Service IPs
- `IPAddress.networking.k8s.io/10.96.236.113` — allocated Service IPs
- `IPAddress.networking.k8s.io/10.96.70.139` — allocated Service IPs
- `ServiceCIDR.networking.k8s.io/kubernetes` — cluster network configuration
- `CSINode.storage.k8s.io/gitopsify-src-control-plane` — maintained by the kubelet

### helm-release-storage (2)

Helm's release records (sh.helm.release.v1.* Secrets/ConfigMaps); releases are migrated as charts instead. Disable with `scope.disableDefaultRules: [helm-release-storage]`.

- `Secret/podinfo/sh.helm.release.v1.podinfo.v1` — Helm release record
- `Secret/tools/sh.helm.release.v1.inhouse.v1` — Helm release record

### kubernetes-service (2)

The default/kubernetes Service and its Endpoints. Disable with `scope.disableDefaultRules: [kubernetes-service]`.

- `Endpoints/default/kubernetes` — API server Service
- `Service/default/kubernetes` — API server Service

### owned-by-controller (19)

Objects with a controller ownerReference (Pods of ReplicaSets, Jobs of CronJobs, objects created by operators); their owner recreates them. Disable with `scope.disableDefaultRules: [owned-by-controller]`.

<details><summary>Objects</summary>

- `Pod/payments/api-8644ff5878-lfvnm` — owned by ReplicaSet/api-8644ff5878
- `ReplicaSet.apps/payments/api-8644ff5878` — owned by Deployment/api
- `EndpointSlice.discovery.k8s.io/payments/api-cdwwz` — owned by Service/api
- `Pod/podinfo/podinfo-854c8966f7-c5jbl` — owned by ReplicaSet/podinfo-854c8966f7
- `Pod/podinfo/podinfo-8585b9d99b-2svkx` — owned by ReplicaSet/podinfo-8585b9d99b
- `ReplicaSet.apps/podinfo/podinfo-854c8966f7` — owned by Deployment/podinfo
- `ReplicaSet.apps/podinfo/podinfo-8585b9d99b` — owned by Deployment/podinfo
- `EndpointSlice.discovery.k8s.io/podinfo/podinfo-pwkn6` — owned by Service/podinfo
- `Pod/shop/migrate-once-7pq86` — owned by Job/migrate-once
- `Pod/shop/web-775db97fdb-99rkz` — owned by ReplicaSet/web-775db97fdb
- `Pod/shop/web-775db97fdb-zcnpg` — owned by ReplicaSet/web-775db97fdb
- `ReplicaSet.apps/shop/web-557577df75` — owned by Deployment/web
- `ReplicaSet.apps/shop/web-659c7985c6` — owned by Deployment/web
- `ReplicaSet.apps/shop/web-775db97fdb` — owned by Deployment/web
- `ReplicaSet.apps/shop/web-7c66bb45cb` — owned by Deployment/web
- `EndpointSlice.discovery.k8s.io/shop/web-pp8bb` — owned by Service/web
- `Pod/tools/inhouse-78b6c57c65-65jhz` — owned by ReplicaSet/inhouse-78b6c57c65
- `ReplicaSet.apps/tools/inhouse-78b6c57c65` — owned by Deployment/inhouse
- `EndpointSlice.discovery.k8s.io/tools/inhouse-7bcnt` — owned by Service/inhouse

</details>

### system-apiservices (23)

APIServices registered automatically for built-in API groups. Disable with `scope.disableDefaultRules: [system-apiservices]`.

<details><summary>Objects</summary>

- `APIService.apiregistration.k8s.io/v1.` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.admissionregistration.k8s.io` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.apiextensions.k8s.io` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.apps` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.authentication.k8s.io` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.authorization.k8s.io` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.autoscaling` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.batch` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.certificates.k8s.io` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.coordination.k8s.io` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.discovery.k8s.io` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.events.k8s.io` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.example.com` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.flowcontrol.apiserver.k8s.io` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.networking.k8s.io` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.node.k8s.io` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.policy` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.rbac.authorization.k8s.io` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.resource.k8s.io` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.scheduling.k8s.io` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.storage.k8s.io` — registered by the API server
- `APIService.apiregistration.k8s.io/v1.storagemigration.k8s.io` — registered by the API server
- `APIService.apiregistration.k8s.io/v2.autoscaling` — registered by the API server

</details>

### system-component-rbac (4)

ClusterRoleBindings that only grant system-namespace service accounts or system: users, and ClusterRoles used only by them (CNI, CSI and cloud components). Disable with `scope.disableDefaultRules: [system-component-rbac]`.

- `ClusterRole.rbac.authorization.k8s.io/kindnet` — used only by system components
- `ClusterRole.rbac.authorization.k8s.io/local-path-provisioner-role` — used only by system components
- `ClusterRoleBinding.rbac.authorization.k8s.io/kindnet` — binds only system components
- `ClusterRoleBinding.rbac.authorization.k8s.io/local-path-provisioner-bind` — binds only system components

### system-flowcontrol (19)

API Priority and Fairness defaults kept up to date by the API server. Disable with `scope.disableDefaultRules: [system-flowcontrol]`.

<details><summary>Objects</summary>

- `FlowSchema.flowcontrol.apiserver.k8s.io/catch-all` — maintained by the API server
- `FlowSchema.flowcontrol.apiserver.k8s.io/exempt` — maintained by the API server
- `FlowSchema.flowcontrol.apiserver.k8s.io/global-default` — maintained by the API server
- `FlowSchema.flowcontrol.apiserver.k8s.io/kube-controller-manager` — maintained by the API server
- `FlowSchema.flowcontrol.apiserver.k8s.io/kube-scheduler` — maintained by the API server
- `FlowSchema.flowcontrol.apiserver.k8s.io/kube-system-service-accounts` — maintained by the API server
- `FlowSchema.flowcontrol.apiserver.k8s.io/probes` — maintained by the API server
- `FlowSchema.flowcontrol.apiserver.k8s.io/service-accounts` — maintained by the API server
- `FlowSchema.flowcontrol.apiserver.k8s.io/system-leader-election` — maintained by the API server
- `FlowSchema.flowcontrol.apiserver.k8s.io/system-node-high` — maintained by the API server
- `FlowSchema.flowcontrol.apiserver.k8s.io/system-nodes` — maintained by the API server
- `PriorityLevelConfiguration.flowcontrol.apiserver.k8s.io/catch-all` — maintained by the API server
- `PriorityLevelConfiguration.flowcontrol.apiserver.k8s.io/exempt` — maintained by the API server
- `PriorityLevelConfiguration.flowcontrol.apiserver.k8s.io/global-default` — maintained by the API server
- `PriorityLevelConfiguration.flowcontrol.apiserver.k8s.io/leader-election` — maintained by the API server
- `PriorityLevelConfiguration.flowcontrol.apiserver.k8s.io/node-high` — maintained by the API server
- `PriorityLevelConfiguration.flowcontrol.apiserver.k8s.io/system` — maintained by the API server
- `PriorityLevelConfiguration.flowcontrol.apiserver.k8s.io/workload-high` — maintained by the API server
- `PriorityLevelConfiguration.flowcontrol.apiserver.k8s.io/workload-low` — maintained by the API server

</details>

### system-namespaces (104)

Namespaces created by Kubernetes and its distributions (kube-system, kube-public, openshift-*, gke-managed-*, ...) and everything in them. Disable with `scope.disableDefaultRules: [system-namespaces]`.

<details><summary>Objects</summary>

- `Namespace/kube-node-lease` — system namespace kube-node-lease
- `Namespace/kube-public` — system namespace kube-public
- `Namespace/kube-system` — system namespace kube-system
- `Namespace/local-path-storage` — system namespace local-path-storage
- `ConfigMap/kube-node-lease/kube-root-ca.crt` — system namespace kube-node-lease
- `ServiceAccount/kube-node-lease/default` — system namespace kube-node-lease
- `Lease.coordination.k8s.io/kube-node-lease/gitopsify-src-control-plane` — system namespace kube-node-lease
- `ConfigMap/kube-public/cluster-info` — system namespace kube-public
- `ConfigMap/kube-public/kube-root-ca.crt` — system namespace kube-public
- `ServiceAccount/kube-public/default` — system namespace kube-public
- `Role.rbac.authorization.k8s.io/kube-public/kubeadm:bootstrap-signer-clusterinfo` — system namespace kube-public
- `Role.rbac.authorization.k8s.io/kube-public/system:controller:bootstrap-signer` — system namespace kube-public
- `RoleBinding.rbac.authorization.k8s.io/kube-public/kubeadm:bootstrap-signer-clusterinfo` — system namespace kube-public
- `RoleBinding.rbac.authorization.k8s.io/kube-public/system:controller:bootstrap-signer` — system namespace kube-public
- `ConfigMap/kube-system/coredns` — system namespace kube-system
- `ConfigMap/kube-system/extension-apiserver-authentication` — system namespace kube-system
- `ConfigMap/kube-system/kube-apiserver-legacy-service-account-token-tracking` — system namespace kube-system
- `ConfigMap/kube-system/kube-proxy` — system namespace kube-system
- `ConfigMap/kube-system/kube-root-ca.crt` — system namespace kube-system
- `ConfigMap/kube-system/kubeadm-config` — system namespace kube-system
- `ConfigMap/kube-system/kubelet-config` — system namespace kube-system
- `Endpoints/kube-system/kube-dns` — system namespace kube-system
- `Secret/kube-system/bootstrap-token-abcdef` — system namespace kube-system
- `Service/kube-system/kube-dns` — system namespace kube-system
- `ServiceAccount/kube-system/attachdetach-controller` — system namespace kube-system
- `ServiceAccount/kube-system/bootstrap-signer` — system namespace kube-system
- `ServiceAccount/kube-system/certificate-controller` — system namespace kube-system
- `ServiceAccount/kube-system/clusterrole-aggregation-controller` — system namespace kube-system
- `ServiceAccount/kube-system/coredns` — system namespace kube-system
- `ServiceAccount/kube-system/cronjob-controller` — system namespace kube-system
- `ServiceAccount/kube-system/daemon-set-controller` — system namespace kube-system
- `ServiceAccount/kube-system/default` — system namespace kube-system
- `ServiceAccount/kube-system/deployment-controller` — system namespace kube-system
- `ServiceAccount/kube-system/device-taint-eviction-controller` — system namespace kube-system
- `ServiceAccount/kube-system/disruption-controller` — system namespace kube-system
- `ServiceAccount/kube-system/endpoint-controller` — system namespace kube-system
- `ServiceAccount/kube-system/endpointslice-controller` — system namespace kube-system
- `ServiceAccount/kube-system/endpointslicemirroring-controller` — system namespace kube-system
- `ServiceAccount/kube-system/ephemeral-volume-controller` — system namespace kube-system
- `ServiceAccount/kube-system/expand-controller` — system namespace kube-system
- `ServiceAccount/kube-system/generic-garbage-collector` — system namespace kube-system
- `ServiceAccount/kube-system/horizontal-pod-autoscaler` — system namespace kube-system
- `ServiceAccount/kube-system/job-controller` — system namespace kube-system
- `ServiceAccount/kube-system/kindnet` — system namespace kube-system
- `ServiceAccount/kube-system/kube-apiserver-serving-clustertrustbundle-publisher` — system namespace kube-system
- `ServiceAccount/kube-system/kube-proxy` — system namespace kube-system
- `ServiceAccount/kube-system/legacy-service-account-token-cleaner` — system namespace kube-system
- `ServiceAccount/kube-system/namespace-controller` — system namespace kube-system
- `ServiceAccount/kube-system/node-controller` — system namespace kube-system
- `ServiceAccount/kube-system/persistent-volume-binder` — system namespace kube-system
- `ServiceAccount/kube-system/pod-garbage-collector` — system namespace kube-system
- `ServiceAccount/kube-system/podcertificaterequestcleaner` — system namespace kube-system
- `ServiceAccount/kube-system/pv-protection-controller` — system namespace kube-system
- `ServiceAccount/kube-system/pvc-protection-controller` — system namespace kube-system
- `ServiceAccount/kube-system/replicaset-controller` — system namespace kube-system
- `ServiceAccount/kube-system/replication-controller` — system namespace kube-system
- `ServiceAccount/kube-system/resource-claim-controller` — system namespace kube-system
- `ServiceAccount/kube-system/resourcequota-controller` — system namespace kube-system
- `ServiceAccount/kube-system/root-ca-cert-publisher` — system namespace kube-system
- `ServiceAccount/kube-system/service-account-controller` — system namespace kube-system
- `ServiceAccount/kube-system/service-cidrs-controller` — system namespace kube-system
- `ServiceAccount/kube-system/statefulset-controller` — system namespace kube-system
- `ServiceAccount/kube-system/storage-version-migrator-controller` — system namespace kube-system
- `ServiceAccount/kube-system/token-cleaner` — system namespace kube-system
- `ServiceAccount/kube-system/ttl-after-finished-controller` — system namespace kube-system
- `ServiceAccount/kube-system/ttl-controller` — system namespace kube-system
- `ServiceAccount/kube-system/validatingadmissionpolicy-status-controller` — system namespace kube-system
- `ServiceAccount/kube-system/volumeattributesclass-protection-controller` — system namespace kube-system
- `ControllerRevision.apps/kube-system/kindnet-6c45c8b764` — system namespace kube-system
- `ControllerRevision.apps/kube-system/kube-proxy-6f6bf99bf5` — system namespace kube-system
- `DaemonSet.apps/kube-system/kindnet` — system namespace kube-system
- `DaemonSet.apps/kube-system/kube-proxy` — system namespace kube-system
- `Deployment.apps/kube-system/coredns` — system namespace kube-system
- `ReplicaSet.apps/kube-system/coredns-559f6c778d` — system namespace kube-system
- `Lease.coordination.k8s.io/kube-system/apiserver-qfglsynwtsv3hnev5o6s7nn4tq` — system namespace kube-system
- `Lease.coordination.k8s.io/kube-system/kube-controller-manager` — system namespace kube-system
- `Lease.coordination.k8s.io/kube-system/kube-scheduler` — system namespace kube-system
- `EndpointSlice.discovery.k8s.io/kube-system/kube-dns-dkn8g` — system namespace kube-system
- `Role.rbac.authorization.k8s.io/kube-system/extension-apiserver-authentication-reader` — system namespace kube-system
- `Role.rbac.authorization.k8s.io/kube-system/kube-proxy` — system namespace kube-system
- `Role.rbac.authorization.k8s.io/kube-system/kubeadm:kubelet-config` — system namespace kube-system
- `Role.rbac.authorization.k8s.io/kube-system/kubeadm:nodes-kubeadm-config` — system namespace kube-system
- `Role.rbac.authorization.k8s.io/kube-system/system::leader-locking-kube-controller-manager` — system namespace kube-system
- `Role.rbac.authorization.k8s.io/kube-system/system::leader-locking-kube-scheduler` — system namespace kube-system
- `Role.rbac.authorization.k8s.io/kube-system/system:controller:bootstrap-signer` — system namespace kube-system
- `Role.rbac.authorization.k8s.io/kube-system/system:controller:cloud-provider` — system namespace kube-system
- `Role.rbac.authorization.k8s.io/kube-system/system:controller:token-cleaner` — system namespace kube-system
- `RoleBinding.rbac.authorization.k8s.io/kube-system/kube-proxy` — system namespace kube-system
- `RoleBinding.rbac.authorization.k8s.io/kube-system/kubeadm:kubelet-config` — system namespace kube-system
- `RoleBinding.rbac.authorization.k8s.io/kube-system/kubeadm:nodes-kubeadm-config` — system namespace kube-system
- `RoleBinding.rbac.authorization.k8s.io/kube-system/system::extension-apiserver-authentication-reader` — system namespace kube-system
- `RoleBinding.rbac.authorization.k8s.io/kube-system/system::leader-locking-kube-controller-manager` — system namespace kube-system
- `RoleBinding.rbac.authorization.k8s.io/kube-system/system::leader-locking-kube-scheduler` — system namespace kube-system
- `RoleBinding.rbac.authorization.k8s.io/kube-system/system:controller:bootstrap-signer` — system namespace kube-system
- `RoleBinding.rbac.authorization.k8s.io/kube-system/system:controller:cloud-provider` — system namespace kube-system
- `RoleBinding.rbac.authorization.k8s.io/kube-system/system:controller:token-cleaner` — system namespace kube-system
- `ConfigMap/local-path-storage/kube-root-ca.crt` — system namespace local-path-storage
- `ConfigMap/local-path-storage/local-path-config` — system namespace local-path-storage
- `ServiceAccount/local-path-storage/default` — system namespace local-path-storage
- `ServiceAccount/local-path-storage/local-path-provisioner-service-account` — system namespace local-path-storage
- `Deployment.apps/local-path-storage/local-path-provisioner` — system namespace local-path-storage
- `ReplicaSet.apps/local-path-storage/local-path-provisioner-75f7fc7dc5` — system namespace local-path-storage
- `Role.rbac.authorization.k8s.io/local-path-storage/local-path-provisioner-role` — system namespace local-path-storage
- `RoleBinding.rbac.authorization.k8s.io/local-path-storage/local-path-provisioner-bind` — system namespace local-path-storage

</details>

### system-priorityclasses (2)

Built-in system-* PriorityClasses. Disable with `scope.disableDefaultRules: [system-priorityclasses]`.

- `PriorityClass.scheduling.k8s.io/system-cluster-critical` — built-in
- `PriorityClass.scheduling.k8s.io/system-node-critical` — built-in

### system-rbac (137)

RBAC objects created by Kubernetes and cloud providers (system:*, eks:*, gce:*, kubeadm:*, bootstrapping defaults). Disable with `scope.disableDefaultRules: [system-rbac]`.

<details><summary>Objects</summary>

- `ClusterRole.rbac.authorization.k8s.io/admin` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/cluster-admin` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/edit` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/kubeadm:get-nodes` — system RBAC (kubeadm:*)
- `ClusterRole.rbac.authorization.k8s.io/system:aggregate-to-admin` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:aggregate-to-edit` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:aggregate-to-view` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:auth-delegator` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:basic-user` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:certificates.k8s.io:certificatesigningrequests:nodeclient` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:certificates.k8s.io:certificatesigningrequests:selfnodeclient` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:certificates.k8s.io:kube-apiserver-client-approver` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:certificates.k8s.io:kube-apiserver-client-kubelet-approver` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:certificates.k8s.io:kubelet-serving-approver` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:certificates.k8s.io:legacy-unknown-approver` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:cluster-trust-bundle-discovery` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:attachdetach-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:certificate-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:clusterrole-aggregation-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:cronjob-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:daemon-set-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:deployment-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:device-taint-eviction-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:disruption-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:endpoint-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:endpointslice-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:endpointslicemirroring-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:ephemeral-volume-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:expand-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:generic-garbage-collector` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:horizontal-pod-autoscaler` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:job-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:kube-apiserver-serving-clustertrustbundle-publisher` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:legacy-service-account-token-cleaner` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:namespace-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:node-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:persistent-volume-binder` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:pod-garbage-collector` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:podcertificaterequestcleaner` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:pv-protection-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:pvc-protection-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:replicaset-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:replication-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:resource-claim-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:resourcequota-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:root-ca-cert-publisher` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:route-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:selinux-warning-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:service-account-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:service-cidrs-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:service-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:statefulset-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:storage-version-migrator-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:ttl-after-finished-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:ttl-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:validatingadmissionpolicy-status-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:controller:volumeattributesclass-protection-controller` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:coredns` — system RBAC (system:*)
- `ClusterRole.rbac.authorization.k8s.io/system:discovery` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:heapster` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:kube-aggregator` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:kube-controller-manager` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:kube-dns` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:kube-scheduler` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:kubelet-api-admin` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:monitoring` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:node` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:node-bootstrapper` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:node-problem-detector` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:node-proxier` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:persistent-volume-provisioner` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:public-info-viewer` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:service-account-issuer-discovery` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/system:volume-scheduler` — Kubernetes default RBAC
- `ClusterRole.rbac.authorization.k8s.io/view` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/cluster-admin` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/kubeadm:apiserver-kubelet-client` — system RBAC (kubeadm:*)
- `ClusterRoleBinding.rbac.authorization.k8s.io/kubeadm:cluster-admins` — system RBAC (kubeadm:*)
- `ClusterRoleBinding.rbac.authorization.k8s.io/kubeadm:get-nodes` — system RBAC (kubeadm:*)
- `ClusterRoleBinding.rbac.authorization.k8s.io/kubeadm:kubelet-bootstrap` — system RBAC (kubeadm:*)
- `ClusterRoleBinding.rbac.authorization.k8s.io/kubeadm:node-autoapprove-bootstrap` — system RBAC (kubeadm:*)
- `ClusterRoleBinding.rbac.authorization.k8s.io/kubeadm:node-autoapprove-certificate-rotation` — system RBAC (kubeadm:*)
- `ClusterRoleBinding.rbac.authorization.k8s.io/kubeadm:node-proxier` — system RBAC (kubeadm:*)
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:basic-user` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:cluster-trust-bundle-discovery` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:attachdetach-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:certificate-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:clusterrole-aggregation-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:cronjob-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:daemon-set-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:deployment-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:device-taint-eviction-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:disruption-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:endpoint-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:endpointslice-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:endpointslicemirroring-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:ephemeral-volume-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:expand-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:generic-garbage-collector` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:horizontal-pod-autoscaler` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:job-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:kube-apiserver-serving-clustertrustbundle-publisher` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:legacy-service-account-token-cleaner` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:namespace-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:node-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:persistent-volume-binder` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:pod-garbage-collector` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:podcertificaterequestcleaner` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:pv-protection-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:pvc-protection-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:replicaset-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:replication-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:resource-claim-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:resourcequota-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:root-ca-cert-publisher` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:route-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:selinux-warning-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:service-account-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:service-cidrs-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:service-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:statefulset-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:storage-version-migrator-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:ttl-after-finished-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:ttl-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:validatingadmissionpolicy-status-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:controller:volumeattributesclass-protection-controller` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:coredns` — system RBAC (system:*)
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:discovery` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:kube-controller-manager` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:kube-dns` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:kube-scheduler` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:monitoring` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:node` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:node-proxier` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:public-info-viewer` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:service-account-issuer-discovery` — Kubernetes default RBAC
- `ClusterRoleBinding.rbac.authorization.k8s.io/system:volume-scheduler` — Kubernetes default RBAC

</details>

