package clean

import (
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/model"
)

func obj(t *testing.T, y string) *model.Object {
	t.Helper()
	m := map[string]any{}
	if err := yaml.Unmarshal([]byte(y), &m); err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: m}
	gvk := u.GroupVersionKind()
	return &model.Object{
		Ref:        model.RefOf(u),
		GVR:        schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: "x"},
		Namespaced: u.GetNamespace() != "",
		Live:       u,
	}
}

func clean(t *testing.T, strategy string, o *model.Object, others ...*model.Object) map[string]any {
	t.Helper()
	cfg := config.New()
	cfg.Clean.Strategy = strategy
	inv := &model.Inventory{}
	inv.Add(append([]*model.Object{o}, others...)...)
	Run(inv, cfg)
	return o.Out.Object
}

func get(t *testing.T, m map[string]any, path ...string) (any, bool) {
	t.Helper()
	v, found, err := unstructured.NestedFieldNoCopy(m, path...)
	if err != nil {
		t.Fatal(err)
	}
	return v, found
}

const deployment = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
  uid: 123
  resourceVersion: "42"
  generation: 3
  creationTimestamp: "2024-01-01T00:00:00Z"
  labels: {app: web}
  annotations:
    deployment.kubernetes.io/revision: "3"
    kubectl.kubernetes.io/last-applied-configuration: "{}"
    team: shop
  managedFields:
    - manager: kubectl-create
      operation: Update
      fieldsV1: {"f:spec": {"f:replicas": {}}}
spec:
  replicas: 2
  progressDeadlineSeconds: 600
  revisionHistoryLimit: 10
  selector: {matchLabels: {app: web}}
  strategy:
    type: RollingUpdate
    rollingUpdate: {maxSurge: 25%, maxUnavailable: 25%}
  template:
    metadata:
      creationTimestamp: null
      labels: {app: web}
      annotations:
        kubectl.kubernetes.io/restartedAt: "2024-01-02T00:00:00Z"
    spec:
      dnsPolicy: ClusterFirst
      restartPolicy: Always
      schedulerName: default-scheduler
      securityContext: {}
      terminationGracePeriodSeconds: 30
      containers:
        - name: web
          image: nginx:1.27
          imagePullPolicy: IfNotPresent
          terminationMessagePath: /dev/termination-log
          terminationMessagePolicy: File
          resources: {}
          ports:
            - containerPort: 80
              protocol: TCP
          readinessProbe:
            httpGet: {path: /, port: 80, scheme: HTTP}
            timeoutSeconds: 1
            periodSeconds: 10
            successThreshold: 1
            failureThreshold: 5
      volumes:
        - name: config
          configMap: {name: web, defaultMode: 420}
status:
  replicas: 2
`

func TestHeuristicDeployment(t *testing.T) {
	out := clean(t, config.CleanHeuristic, obj(t, deployment))

	for _, p := range [][]string{
		{"status"},
		{"metadata", "uid"}, {"metadata", "resourceVersion"}, {"metadata", "generation"},
		{"metadata", "creationTimestamp"}, {"metadata", "managedFields"},
		{"spec", "progressDeadlineSeconds"}, {"spec", "revisionHistoryLimit"}, {"spec", "strategy"},
		{"spec", "template", "metadata", "creationTimestamp"},
		{"spec", "template", "metadata", "annotations"},
		{"spec", "template", "spec", "dnsPolicy"}, {"spec", "template", "spec", "restartPolicy"},
		{"spec", "template", "spec", "schedulerName"}, {"spec", "template", "spec", "securityContext"},
		{"spec", "template", "spec", "terminationGracePeriodSeconds"},
	} {
		if _, found := get(t, out, p...); found {
			t.Errorf("%v should have been removed", p)
		}
	}
	ann, _ := get(t, out, "metadata", "annotations")
	if want := map[string]any{"team": "shop"}; !Equal(ann, want) {
		t.Errorf("annotations = %v, want %v", ann, want)
	}
	if r, _ := get(t, out, "spec", "replicas"); !Equal(r, 2) {
		t.Errorf("replicas = %v, want 2", r)
	}
	c := out["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	for _, k := range []string{"imagePullPolicy", "terminationMessagePath", "terminationMessagePolicy", "resources"} {
		if _, ok := c[k]; ok {
			t.Errorf("container %s should have been removed", k)
		}
	}
	probe := c["readinessProbe"].(map[string]any)
	if want := map[string]any{"httpGet": map[string]any{"path": "/", "port": 80}, "failureThreshold": 5}; !Equal(probe, want) {
		t.Errorf("probe = %v, want %v (non-default failureThreshold kept)", probe, want)
	}
	if p := c["ports"].([]any)[0].(map[string]any); !Equal(p, map[string]any{"containerPort": 80}) {
		t.Errorf("port = %v", p)
	}
	vol := out["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["volumes"].([]any)[0]
	if !Equal(vol, map[string]any{"name": "config", "configMap": map[string]any{"name": "web"}}) {
		t.Errorf("volume = %v", vol)
	}
}

func TestMinimalKeepsDefaults(t *testing.T) {
	out := clean(t, config.CleanMinimal, obj(t, deployment))
	if v, _ := get(t, out, "spec", "template", "spec", "dnsPolicy"); v != "ClusterFirst" {
		t.Errorf("minimal must keep defaults, dnsPolicy = %v", v)
	}
	if _, found := get(t, out, "metadata", "managedFields"); found {
		t.Error("minimal must still strip runtime metadata")
	}
}

func TestHPATargetLosesReplicas(t *testing.T) {
	hpa := obj(t, `
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: web, namespace: shop}
spec:
  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: web}
  minReplicas: 2
  maxReplicas: 5
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 0
      selectPolicy: Max
      policies:
        - {type: Pods, value: 4, periodSeconds: 15}
        - {type: Percent, value: 100, periodSeconds: 15}
    scaleDown:
      selectPolicy: Max
      policies:
        - {type: Percent, value: 100, periodSeconds: 15}
`)
	d := obj(t, deployment)
	out := clean(t, config.CleanHeuristic, d, hpa)
	if _, found := get(t, out, "spec", "replicas"); found {
		t.Error("replicas of an HPA target must be removed")
	}
	if _, found := get(t, hpa.Out.Object, "spec", "behavior"); found {
		t.Error("default HPA behavior must be removed")
	}
}

func TestServiceAllocatedFields(t *testing.T) {
	svc := `
apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: shop
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: '{"spec":{"type":"NodePort","ports":[{"name":"https","port":443,"nodePort":30443}]}}'
spec:
  type: NodePort
  clusterIP: 10.96.0.12
  clusterIPs: [10.96.0.12]
  ipFamilies: [IPv4]
  ipFamilyPolicy: SingleStack
  sessionAffinity: None
  externalTrafficPolicy: Cluster
  internalTrafficPolicy: Cluster
  selector: {app: web}
  ports:
    - {name: http, port: 80, targetPort: 80, protocol: TCP, nodePort: 31234}
    - {name: https, port: 443, targetPort: 8443, protocol: TCP, nodePort: 30443}
`
	out := clean(t, config.CleanHeuristic, obj(t, svc))
	spec := out["spec"].(map[string]any)
	for _, k := range []string{"clusterIP", "clusterIPs", "ipFamilies", "ipFamilyPolicy", "sessionAffinity", "externalTrafficPolicy", "internalTrafficPolicy"} {
		if _, ok := spec[k]; ok {
			t.Errorf("spec.%s should have been removed", k)
		}
	}
	ports := spec["ports"].([]any)
	if want := map[string]any{"name": "http", "port": 80}; !Equal(ports[0], want) {
		t.Errorf("http port = %v, want %v (allocated nodePort and defaults dropped)", ports[0], want)
	}
	if want := map[string]any{"name": "https", "port": 443, "targetPort": 8443, "nodePort": 30443}; !Equal(ports[1], want) {
		t.Errorf("https port = %v, want %v (nodePort chosen in kubectl apply kept)", ports[1], want)
	}

	headless := clean(t, config.CleanHeuristic, obj(t, `
apiVersion: v1
kind: Service
metadata: {name: db, namespace: shop}
spec: {clusterIP: None, clusterIPs: [None], selector: {app: db}, ports: [{port: 5432}]}
`))
	if v, _ := get(t, headless, "spec", "clusterIP"); v != "None" {
		t.Errorf("headless clusterIP = %v, want None", v)
	}
}

func TestJobGeneratedSelector(t *testing.T) {
	out := clean(t, config.CleanHeuristic, obj(t, `
apiVersion: batch/v1
kind: Job
metadata:
  name: once
  namespace: shop
  labels: {controller-uid: abc, job-name: once, team: x}
spec:
  backoffLimit: 6
  completions: 1
  parallelism: 1
  selector: {matchLabels: {batch.kubernetes.io/controller-uid: abc}}
  template:
    metadata:
      labels: {batch.kubernetes.io/controller-uid: abc, batch.kubernetes.io/job-name: once, controller-uid: abc, job-name: once}
    spec:
      restartPolicy: Never
      containers: [{name: c, image: busybox}]
`))
	for _, p := range [][]string{{"spec", "selector"}, {"spec", "template", "metadata"}, {"spec", "backoffLimit"}, {"spec", "completions"}, {"spec", "parallelism"}} {
		if _, found := get(t, out, p...); found {
			t.Errorf("%v should have been removed", p)
		}
	}
	if v, _ := get(t, out, "spec", "template", "spec", "restartPolicy"); v != "Never" {
		t.Errorf("Job restartPolicy must be kept, got %v", v)
	}
	if l, _ := get(t, out, "metadata", "labels"); !Equal(l, map[string]any{"team": "x"}) {
		t.Errorf("labels = %v", l)
	}
}

func TestPVCAndNamespace(t *testing.T) {
	pvc := clean(t, config.CleanHeuristic, obj(t, `
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
  namespace: shop
  annotations:
    pv.kubernetes.io/bind-completed: "yes"
    volume.kubernetes.io/storage-provisioner: rancher.io/local-path
  finalizers: [kubernetes.io/pvc-protection]
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 1Gi}}
  storageClassName: fast
  volumeMode: Filesystem
  volumeName: pvc-123
`))
	if want := map[string]any{"accessModes": []any{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "1Gi"}}, "storageClassName": "fast"}; !Equal(pvc["spec"], want) {
		t.Errorf("pvc spec = %v", pvc["spec"])
	}
	if want := map[string]any{"name": "data", "namespace": "shop"}; !Equal(pvc["metadata"], want) {
		t.Errorf("pvc metadata = %v", pvc["metadata"])
	}

	ns := clean(t, config.CleanHeuristic, obj(t, `
apiVersion: v1
kind: Namespace
metadata: {name: shop, labels: {kubernetes.io/metadata.name: shop, team: shop}}
spec: {finalizers: [kubernetes]}
`))
	if want := map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "shop", "labels": map[string]any{"team": "shop"}}}; !Equal(ns, want) {
		t.Errorf("namespace = %v", ns)
	}
}

func TestCAInjection(t *testing.T) {
	out := clean(t, config.CleanHeuristic, obj(t, `
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: hook
  annotations: {cert-manager.io/inject-ca-from: x/y}
webhooks:
  - name: a.example.com
    clientConfig: {caBundle: Zm9v, service: {name: s, namespace: x, port: 443}}
    matchPolicy: Equivalent
    timeoutSeconds: 10
    failurePolicy: Fail
    sideEffects: None
    admissionReviewVersions: [v1]
    namespaceSelector: {}
    objectSelector: {}
    rules: [{apiGroups: [""], apiVersions: [v1], operations: [CREATE], resources: [pods], scope: "*"}]
`))
	w := out["webhooks"].([]any)[0].(map[string]any)
	want := map[string]any{
		"name":                    "a.example.com",
		"clientConfig":            map[string]any{"service": map[string]any{"name": "s", "namespace": "x"}},
		"sideEffects":             "None",
		"admissionReviewVersions": []any{"v1"},
		"rules":                   []any{map[string]any{"apiGroups": []any{""}, "apiVersions": []any{"v1"}, "operations": []any{"CREATE"}, "resources": []any{"pods"}}},
	}
	if !Equal(w, want) {
		b, _ := json.Marshal(w)
		t.Errorf("webhook = %s", b)
	}
}

func TestManagedFieldsPrune(t *testing.T) {
	o := obj(t, `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: payments
  labels: {app: api}
  managedFields:
    - manager: platform-team
      operation: Apply
      fieldsV1:
        f:metadata: {f:labels: {f:app: {}}}
        f:spec:
          f:replicas: {}
          f:selector: {}
          f:template:
            f:metadata: {f:labels: {f:app: {}}}
            f:spec:
              f:containers:
                k:{"name":"api"}:
                  .: {}
                  f:image: {}
                  f:name: {}
                  f:ports:
                    k:{"containerPort":8080,"protocol":"TCP"}:
                      .: {}
                      f:containerPort: {}
    - manager: kube-controller-manager
      operation: Update
      subresource: status
      fieldsV1: {f:status: {f:replicas: {}}}
spec:
  replicas: 1
  progressDeadlineSeconds: 600
  selector: {matchLabels: {app: api}}
  template:
    metadata: {labels: {app: api}}
    spec:
      dnsPolicy: ClusterFirst
      containers:
        - name: api
          image: pause:3.10
          imagePullPolicy: IfNotPresent
          ports: [{containerPort: 8080, protocol: TCP}]
status: {replicas: 1}
`)
	cfg := config.New()
	c := &Cleaner{Config: cfg, hpaTargets: map[string]bool{}}
	if s := c.Strategy(o); s != config.CleanManagedFields {
		t.Fatalf("strategy = %s, want managed-fields for an object applied server-side", s)
	}
	out := c.Clean(o).Object
	want := map[string]any{
		"replicas": 1,
		"selector": map[string]any{"matchLabels": map[string]any{"app": "api"}},
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{"app": "api"}},
			"spec": map[string]any{"containers": []any{map[string]any{
				"name": "api", "image": "pause:3.10",
				"ports": []any{map[string]any{"containerPort": 8080}},
			}}},
		},
	}
	if !Equal(out["spec"], want) {
		b, _ := json.Marshal(out["spec"])
		t.Errorf("spec = %s", b)
	}
}

func TestIsController(t *testing.T) {
	for m, want := range map[string]bool{
		"kube-controller-manager": true, "cert-manager-cainjector": true, "my-operator": true,
		"kubectl-client-side-apply": false, "helm": false, "argocd-controller": false,
		"kustomize-controller": false, "platform-team": false, "Mozilla": false,
	} {
		if got := IsController(m, nil); got != want {
			t.Errorf("IsController(%q) = %v, want %v", m, got, want)
		}
	}
}

func TestImageDefaultPullPolicy(t *testing.T) {
	for img, want := range map[string]string{
		"nginx": "Always", "nginx:latest": "Always", "nginx:1.27": "IfNotPresent",
		"localhost:5000/app": "Always", "localhost:5000/app:v1": "IfNotPresent",
		"nginx@sha256:abc": "IfNotPresent",
	} {
		if got := ImageDefaultPullPolicy(img); got != want {
			t.Errorf("ImageDefaultPullPolicy(%q) = %s, want %s", img, got, want)
		}
	}
}
