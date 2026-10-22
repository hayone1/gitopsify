package target

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/helm"
	"github.com/hayone1/gitopsify/pkg/kube"
)

var (
	gvrApplication   = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	gvrKustomization = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	gvrHelmRelease   = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
)

// AppStatus is the state of one GitOps object of an environment.
type AppStatus struct {
	// Kind is Application, Kustomization or HelmRelease.
	Kind string `json:"kind"`
	// Name is namespace/name of the object.
	Name string `json:"name"`
	// Sync is Argo CD's sync status, or the revision Flux applied.
	Sync string `json:"sync,omitempty"`
	// Health is Argo CD's health status, or Ready/NotReady/Suspended for Flux.
	Health  string `json:"health"`
	Ready   bool   `json:"ready"`
	Message string `json:"message,omitempty"`
}

// Status reads the state of the environment's GitOps objects. dir is the
// generated repository root of the environment; the objects it declares that
// don't exist yet are reported as Missing.
func Status(ctx context.Context, cfg *config.Config, env *config.Environment, c *kube.Client, dir string) ([]AppStatus, error) {
	var out []AppStatus
	var err error
	switch cfg.Output.Tool {
	case config.ToolArgoCD:
		out, err = argoStatus(ctx, cfg, env, c, dir)
	case config.ToolFlux:
		out, err = fluxStatus(ctx, cfg, env, c, dir)
	default:
		return nil, fmt.Errorf("--tool %s has no GitOps objects to report on", cfg.Output.Tool)
	}
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Ready != out[j].Ready {
			return !out[i].Ready
		}
		return out[i].Kind+out[i].Name < out[j].Kind+out[j].Name
	})
	return out, nil
}

// WaitOptions tune Wait.
type WaitOptions struct {
	Timeout time.Duration
	// SyncedOnly waits for Argo CD to sync (Flux: Ready) without requiring
	// health, for clusters where some resources can't become healthy (no
	// ingress controller or metrics server).
	SyncedOnly bool
	// DegradedGrace ends the wait early, with an error, once everything is
	// synced and the only unready apps have stayed Degraded this long.
	// Default 2 minutes.
	DegradedGrace time.Duration
	// Interval between polls (default 5 seconds).
	Interval time.Duration
	// Progress receives every poll's result.
	Progress func([]AppStatus)
}

// Wait polls Status until every object is ready, the timeout passes or the
// context ends.
func Wait(ctx context.Context, cfg *config.Config, env *config.Environment, c *kube.Client, dir string, o WaitOptions) ([]AppStatus, error) {
	if o.DegradedGrace == 0 {
		o.DegradedGrace = 2 * time.Minute
	}
	if o.Interval == 0 {
		o.Interval = 5 * time.Second
	}
	deadline := time.Now().Add(o.Timeout)
	var degradedSince time.Time
	for {
		st, err := Status(ctx, cfg, env, c, dir)
		if err != nil && !isTransient(err) {
			return nil, err
		}
		if err == nil {
			if o.SyncedOnly {
				for i := range st {
					st[i].Ready = st[i].Ready || st[i].Kind == "Application" && st[i].Sync == "Synced"
				}
			}
			if o.Progress != nil {
				o.Progress(st)
			}
			if len(st) > 0 && AllReady(st) {
				return st, nil
			}
			if stuckDegraded(st) {
				if degradedSince.IsZero() {
					degradedSince = time.Now()
				} else if time.Since(degradedSince) >= o.DegradedGrace {
					return st, fmt.Errorf("%d of %d synced but degraded for %s; see the messages (or wait for sync only with --wait-for synced)", len(st)-countReady(st), len(st), o.DegradedGrace)
				}
			} else {
				degradedSince = time.Time{}
			}
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("timed out after %s: %d of %d not ready", o.Timeout, len(st)-countReady(st), len(st))
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(o.Interval):
		}
	}
}

// stuckDegraded reports whether everything that isn't ready is synced and
// Degraded: Argo CD has done its part and something in the cluster is wrong.
// Applications that only wait for their children don't count.
func stuckDegraded(st []AppStatus) bool {
	found := false
	for _, s := range st {
		if s.Ready {
			continue
		}
		if s.Kind != "Application" || s.Sync != "Synced" || s.Health != "Degraded" {
			return false
		}
		found = true
	}
	return found
}

// AllReady reports whether every object is ready.
func AllReady(st []AppStatus) bool { return countReady(st) == len(st) }

func countReady(st []AppStatus) int {
	n := 0
	for _, s := range st {
		if s.Ready {
			n++
		}
	}
	return n
}

func isTransient(err error) bool {
	return meta.IsNoMatchError(err) || apierrors.IsServiceUnavailable(err) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err)
}

// declared returns the names of the objects of a kind declared in the YAML
// files of a directory.
func declared(dir, kind string) []string {
	var out []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, u := range helm.ManifestObjects(string(data)) {
			if u.GetKind() == kind {
				out = append(out, u.GetName())
			}
		}
	}
	return out
}

func argoStatus(ctx context.Context, cfg *config.Config, env *config.Environment, c *kube.Client, dir string) ([]AppStatus, error) {
	ns := cfg.Output.ArgoCD.Namespace
	envDir := filepath.Join(dir, "argocd", env.Name)
	want := map[string]bool{}
	for _, n := range declared(envDir, "Application") {
		want[n] = true
	}
	for _, n := range declared(filepath.Join(envDir, "apps"), "Application") {
		want[n] = true
	}
	files, _ := filepath.Glob(filepath.Join(envDir, "config", "*.json"))
	for _, f := range files {
		var p struct {
			AppName string `json:"appName"`
		}
		if b, err := os.ReadFile(f); err == nil && json.Unmarshal(b, &p) == nil && p.AppName != "" {
			want[p.AppName] = true
		}
	}
	list, err := c.Dynamic.Resource(gvrApplication).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return missing("Application", ns, want), nil
		}
		return nil, c.Explain(err)
	}
	var out []AppStatus
	for i := range list.Items {
		u := &list.Items[i]
		if !want[u.GetName()] && u.GetLabels()["gitopsify.io/env"] != env.Name {
			continue
		}
		delete(want, u.GetName())
		s := applicationStatus(u)
		if !s.Ready && s.Message == "" {
			s.Message = explainResources(ctx, c, u)
		}
		out = append(out, s)
	}
	return append(out, missing("Application", ns, want)...), nil
}

// explainResources says why an Application isn't healthy by looking at the
// resources that commonly keep apps from becoming healthy. Argo CD 3 no
// longer records resource health in the Application.
func explainResources(ctx context.Context, c *kube.Client, app *unstructured.Unstructured) string {
	if c.Mapper == nil {
		return ""
	}
	res, _, _ := unstructured.NestedSlice(app.Object, "status", "resources")
	var reasons []string
	for _, r := range res {
		m, _ := r.(map[string]any)
		group, _ := m["group"].(string)
		version, _ := m["version"].(string)
		kind, _ := m["kind"].(string)
		ns, _ := m["namespace"].(string)
		name, _ := m["name"].(string)
		switch kind {
		case "HorizontalPodAutoscaler", "PersistentVolumeClaim", "Ingress", "Service", "Deployment", "StatefulSet", "DaemonSet", "Application":
		default:
			continue
		}
		apiVersion := version
		if group != "" {
			apiVersion = group + "/" + version
		}
		stub := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": map[string]any{"name": name, "namespace": ns}}}
		live, err := c.Get(ctx, stub)
		if err != nil || live == nil {
			continue
		}
		if why := unhealthyReason(live); why != "" {
			reasons = append(reasons, kind+" "+ns+"/"+name+": "+why)
		}
		if len(reasons) == 3 {
			break
		}
	}
	return strings.Join(reasons, "; ")
}

// unhealthyReason explains why a resource isn't healthy, or returns "".
func unhealthyReason(u *unstructured.Unstructured) string {
	obj := u.Object
	desired := func(path ...string) int64 {
		v, found, _ := unstructured.NestedInt64(obj, path...)
		if !found {
			return 1
		}
		return v
	}
	count := func(path ...string) int64 {
		v, _, _ := unstructured.NestedInt64(obj, path...)
		return v
	}
	switch u.GetKind() {
	case "Application":
		// The root app of app-of-apps waits for its children.
		if h, _, _ := unstructured.NestedString(obj, "status", "health", "status"); h != "" && h != "Healthy" {
			return h
		}
	case "HorizontalPodAutoscaler":
		conds, _, _ := unstructured.NestedSlice(obj, "status", "conditions")
		for _, c := range conds {
			m, _ := c.(map[string]any)
			if (m["type"] == "ScalingActive" || m["type"] == "AbleToScale") && m["status"] == "False" {
				return fmt.Sprintf("%v (%v): %v", m["type"], m["reason"], m["message"])
			}
		}
	case "PersistentVolumeClaim":
		if phase, _, _ := unstructured.NestedString(obj, "status", "phase"); phase == "Pending" {
			sc, _, _ := unstructured.NestedString(obj, "spec", "storageClassName")
			return fmt.Sprintf("Pending with StorageClass %q; it binds once its StorageClass provisions a volume (for WaitForFirstConsumer classes, once a pod uses it)", sc)
		}
	case "Ingress":
		if lb, _, _ := unstructured.NestedSlice(obj, "status", "loadBalancer", "ingress"); len(lb) == 0 {
			return "no address yet; is an ingress controller running for its class?"
		}
	case "Service":
		t, _, _ := unstructured.NestedString(obj, "spec", "type")
		if lb, _, _ := unstructured.NestedSlice(obj, "status", "loadBalancer", "ingress"); t == "LoadBalancer" && len(lb) == 0 {
			return "no external address yet; does the cluster provision load balancers?"
		}
	case "Deployment", "StatefulSet":
		want, ready := desired("spec", "replicas"), count("status", "readyReplicas")
		if ready < want {
			return fmt.Sprintf("%d of %d replicas ready", ready, want)
		}
	case "DaemonSet":
		want, ready := count("status", "desiredNumberScheduled"), count("status", "numberReady")
		if ready < want {
			return fmt.Sprintf("%d of %d pods ready", ready, want)
		}
	}
	return ""
}

func applicationStatus(u *unstructured.Unstructured) AppStatus {
	s := AppStatus{Kind: "Application", Name: u.GetNamespace() + "/" + u.GetName()}
	s.Sync, _, _ = unstructured.NestedString(u.Object, "status", "sync", "status")
	s.Health, _, _ = unstructured.NestedString(u.Object, "status", "health", "status")
	if s.Sync == "" {
		s.Sync = "Unknown"
	}
	if s.Health == "" {
		s.Health = "Unknown"
	}
	s.Ready = s.Sync == "Synced" && s.Health == "Healthy"
	if s.Ready {
		return s
	}
	phase, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "phase")
	msg, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "message")
	if (phase == "Failed" || phase == "Error") && msg != "" {
		s.Message = msg
	}
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if t, _ := m["type"].(string); strings.HasSuffix(t, "Error") {
			if msg, _ := m["message"].(string); msg != "" {
				s.Message = strings.TrimSpace(s.Message + " " + msg)
			}
		}
	}
	if s.Message == "" {
		s.Message, _, _ = unstructured.NestedString(u.Object, "status", "health", "message")
	}
	return s
}

func fluxStatus(ctx context.Context, cfg *config.Config, env *config.Environment, c *kube.Client, dir string) ([]AppStatus, error) {
	ns := cfg.Output.Flux.Namespace
	want := map[string]bool{"flux-system": true}
	for _, n := range declared(filepath.Join(dir, "clusters", env.Name, "apps"), "Kustomization") {
		want[n] = true
	}
	list, err := c.Dynamic.Resource(gvrKustomization).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return missing("Kustomization", ns, want), nil
		}
		return nil, c.Explain(err)
	}
	var out []AppStatus
	apps := map[string]bool{}
	for i := range list.Items {
		u := &list.Items[i]
		if !want[u.GetName()] && u.GetLabels()["gitopsify.io/wave"] == "" {
			continue
		}
		delete(want, u.GetName())
		apps[u.GetName()] = true
		s := fluxReady(u, "Kustomization")
		s.Sync, _, _ = unstructured.NestedString(u.Object, "status", "lastAppliedRevision")
		out = append(out, s)
	}
	out = append(out, missing("Kustomization", ns, want)...)
	hrs, err := c.Dynamic.Resource(gvrHelmRelease).List(ctx, metav1.ListOptions{LabelSelector: "kustomize.toolkit.fluxcd.io/namespace=" + ns})
	if err == nil {
		for i := range hrs.Items {
			u := &hrs.Items[i]
			if !apps[u.GetLabels()["kustomize.toolkit.fluxcd.io/name"]] {
				continue
			}
			s := fluxReady(u, "HelmRelease")
			s.Sync, _, _ = unstructured.NestedString(u.Object, "status", "lastAttemptedRevision")
			out = append(out, s)
		}
	}
	return out, nil
}

func fluxReady(u *unstructured.Unstructured, kind string) AppStatus {
	s := AppStatus{Kind: kind, Name: u.GetNamespace() + "/" + u.GetName(), Health: "Unknown"}
	if suspended, _, _ := unstructured.NestedBool(u.Object, "spec", "suspend"); suspended {
		s.Health, s.Message = "Suspended", "reconciliation is suspended"
		return s
	}
	observed, _, _ := unstructured.NestedInt64(u.Object, "status", "observedGeneration")
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] != "Ready" {
			continue
		}
		s.Message, _ = m["message"].(string)
		switch m["status"] {
		case "True":
			s.Health = "Ready"
			s.Ready = observed >= u.GetGeneration()
			if !s.Ready {
				s.Health = "Progressing"
			}
		case "False":
			s.Health = "NotReady"
		default:
			s.Health = "Progressing"
		}
	}
	if s.Ready {
		s.Message = ""
	}
	return s
}

func missing(kind, ns string, want map[string]bool) []AppStatus {
	var out []AppStatus
	for n := range want {
		out = append(out, AppStatus{Kind: kind, Name: ns + "/" + n, Health: "Missing", Message: "not created yet"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
