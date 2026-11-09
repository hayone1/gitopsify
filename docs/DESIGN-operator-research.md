# Research: how Argo CD and Argo Workflows are built

Notes for a possible gitopsify operator (CRDs, a controller, a CLI that talks to the server). Gathered from the projects' documentation and source on 2026-11-09; see HANDOFF.md for the decision still open.

Argo CD (v3.6.1)
- Components: argocd-server (gRPC + grpc-gateway REST + grpc-web on one port, Swagger /swagger-ui), repo-server, application-controller (StatefulSet, shards clusters; no leader election), applicationset-controller (leader election), redis (disposable cache), dex, notifications. One multicall binary/image (argv[0] dispatch, ARGOCD_BINARY_NAME); CLI is the default personality.
- CRDs: Application, ApplicationSet, AppProject (namespaced; by default only in argocd ns; apps-in-any-namespace opt-in via --application-namespaces + AppProject.sourceNamespaces). Application: spec, status{sync,health,resources,history(revisionHistoryLimit default 10),operationState,conditions}, top-level `operation` (imperative sync = write .operation). No status subresource.
- No DB: everything is K8s objects; server writes CRs with its own SA after its Casbin RBAC check.
- RBAC: argocd-rbac-cm policy.csv (p/g lines, deny wins), role:readonly/admin; AppProject restricts sourceRepos, destinations, resource allow/deny lists, roles with JWTs (proj role create-token, revocable), syncWindows, destinationServiceAccounts (sync impersonation, beta 3.5).
- "Anyone who can create Applications in argocd ns is an admin."
- CLI: argocd login [--sso localhost oauth :8085] [--core: talk to K8s directly, spawns local API server], contexts in ~/.config/argocd/config, argocd account generate-token (apiKey capability).
- Declarative setup: argocd-cm, argocd-rbac-cm..., Secrets labelled argocd.argoproj.io/secret-type: repository|repo-creds|cluster; repository-write / repo-write-creds for write creds (source hydrator). Self-manageable.
- Source Hydrator = closest analogue (writes rendered manifests to git; hydrateTo branch; write creds separate).
- Logs: not stored, streamed from pods.
- Packaging: install.yaml, namespace-install.yaml, core-install.yaml (no server/UI), HA; Helm charts community-maintained in argo-helm (argo-cd, argo-workflows), OCI ghcr.io/argoproj/argo-helm; Artifact Hub official+verified. Operator is a separate (now dormant) project argocd-operator (ArgoCD CR) on OperatorHub.

Argo Workflows (v4.1.6)
- workflow-controller (leader election, recommend 1 replica Recreate), argo-server (CLI image with `server` arg, scalable), argoexec executor per pod.
- CRDs: Workflow (live object: spec + status), WorkflowTemplate, ClusterWorkflowTemplate (cluster), CronWorkflow (successful/failedJobsHistoryLimit), WorkflowEventBinding, WorkflowTaskResult (internal), WorkflowTaskSet, WorkflowArtifactGCTask. Minimal CRDs / SSA for size.
- etcd 1MB: compressedNodes, offload to Postgres/MySQL; workflow archive (PG/MySQL) for completed; logs not archived (archiveLogs to artifact repo optional; recommend log stack + UI links).
- Server per-request K8s client from caller identity; DB only for archive/offload.
- Auth modes: client (forward caller's K8s bearer token, default), server (own SA), sso (OIDC groups -> ServiceAccount via annotation workflows.argoproj.io/rbac-rule; K8s RBAC enforces; SSO_DELEGATE_RBAC_TO_NAMESPACE).
- Confinement: pods run as workflow SA; managed namespace mode; workflowRestrictions templateReferencing Strict/Secure.
- CLI: argo talks to K8s API by default; ARGO_SERVER/--argo-server for server mode (gRPC/HTTP2; ARGO_HTTP1); ARGO_TOKEN "Bearer ..."; argo auth token. Same binary has `argo server`.
- Config: workflow-controller-configmap; per-namespace defaults ConfigMap (v4.2+); artifact-repositories ConfigMap.
- No operator.

Suggestions from research: Export-style CR (spec: scope, target repo, schedule; status: last run + capped history) + per-run object or .operation; treat create-CR as high privilege (Project CR allow-listing source namespaces/kinds, target repos; impersonation SA); server auth start with client mode (K8s token) or OIDC->SA mapping; labelled Secrets for creds (repository-write; interop with Argo CD's); CLI default to K8s API with --server mode, contexts ~/.config/gitopsify/config, --sso localhost callback; REST+OpenAPI fine; packaging install.yaml/namespace-install/core + Helm; OLM low priority.
