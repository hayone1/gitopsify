// Package bootstrap installs Argo CD or Flux in an environment's cluster and
// points it at the generated repository.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/helm"
	"github.com/hayone1/gitopsify/pkg/kube"
	"github.com/hayone1/gitopsify/pkg/render"
)

// GitCredentials give the GitOps tool access to a private repository.
type GitCredentials struct {
	Username string
	// Password is a password or access token for HTTPS repositories.
	Password string
	// SSHKeyFile is a private key for SSH repositories.
	SSHKeyFile string
	// KnownHostsFile holds the SSH host keys of the Git server (Flux).
	KnownHostsFile string
}

// Options configure Run.
type Options struct {
	Config *config.Config
	Env    *config.Environment
	Client *kube.Client
	// Dir is the generated repository's root directory for the environment.
	Dir string
	Git GitCredentials
	// SOPSAgeKeyFile is loaded as Flux's decryption key.
	SOPSAgeKeyFile string
	Timeout        time.Duration
	Log            func(format string, args ...any)
	HTTPClient     *http.Client
}

func (o *Options) log(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// Run bootstraps the configured GitOps tool.
func Run(ctx context.Context, o Options) error {
	if o.Timeout == 0 {
		o.Timeout = 15 * time.Minute
	}
	switch o.Config.Output.Tool {
	case config.ToolArgoCD:
		return argoCD(ctx, o)
	case config.ToolFlux:
		return flux(ctx, o)
	}
	return fmt.Errorf("--tool %s has nothing to bootstrap; apply the manifests with apply-%s.sh", o.Config.Output.Tool, o.Env.Name)
}

func argoCD(ctx context.Context, o Options) error {
	a := o.Config.Output.ArgoCD
	c := o.Client
	o.log("installing Argo CD %s in namespace %s", a.Version, a.Namespace)
	objs, err := fetch(ctx, o.HTTPClient, render.ArgoCDInstallURL(a.Version))
	if err != nil {
		return err
	}
	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": a.Namespace}}}
	if err := applyAll(ctx, c, append([]*unstructured.Unstructured{ns}, objs...), a.Namespace, "argocd"); err != nil {
		return err
	}
	restartAppSet := false
	for _, f := range []string{"argocd-cm.yaml", "argocd-cmd-params-cm.yaml"} {
		p := filepath.Join(o.Dir, "argocd", "bootstrap", f)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		// A separate field manager: applying only these keys as the manager
		// that applied the install would remove the install's labels, and
		// Argo CD ignores ConfigMaps without them.
		if err := applyFile(ctx, c, p, settingsFieldManager); err != nil {
			return err
		}
		restartAppSet = restartAppSet || f == "argocd-cmd-params-cm.yaml"
	}
	if restartAppSet {
		if err := restart(ctx, c, a.Namespace, "argocd-applicationset-controller"); err != nil {
			return err
		}
	}
	if err := argoRepoSecret(ctx, o); err != nil {
		return err
	}
	o.log("waiting for Argo CD to become ready")
	if err := waitReady(ctx, c, a.Namespace, []string{"argocd-server", "argocd-repo-server", "argocd-applicationset-controller"}, []string{"argocd-application-controller"}, o.Timeout); err != nil {
		return err
	}
	root := filepath.Join(o.Dir, "argocd", o.Env.Name, "root.yaml")
	o.log("applying %s", root)
	if err := applyFile(ctx, c, root, kube.FieldManager); err != nil {
		return err
	}
	o.log("done. Initial admin password: kubectl -n %s get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d", a.Namespace)
	return nil
}

func argoRepoSecret(ctx context.Context, o Options) error {
	g := o.Git
	if g.Password == "" && g.SSHKeyFile == "" {
		return nil
	}
	data := map[string]any{"type": "git", "url": o.Config.Output.Repo.URL}
	if url := o.Env.RepoURL; url != "" && o.Config.Output.Layout == config.LayoutRepoPerEnv {
		data["url"] = url
	}
	if g.SSHKeyFile != "" {
		key, err := os.ReadFile(g.SSHKeyFile)
		if err != nil {
			return err
		}
		data["sshPrivateKey"] = string(key)
	} else {
		user := g.Username
		if user == "" {
			user = "git"
		}
		data["username"], data["password"] = user, g.Password
	}
	s := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": "gitopsify-repo", "namespace": o.Config.Output.ArgoCD.Namespace,
			"labels": map[string]any{"argocd.argoproj.io/secret-type": "repository"},
		},
		"stringData": data,
	}}
	o.log("registering repository %s", data["url"])
	if _, err := o.Client.Apply(ctx, s, kube.ApplyOptions{Force: true}); err != nil {
		return err
	}
	if g.SSHKeyFile != "" && g.KnownHostsFile != "" {
		return argoKnownHosts(ctx, o)
	}
	return nil
}

// argoKnownHosts adds the Git server's host keys to the ones Argo CD ships
// with (GitHub, GitLab, Bitbucket, Azure DevOps).
func argoKnownHosts(ctx context.Context, o Options) error {
	extra, err := os.ReadFile(o.Git.KnownHostsFile)
	if err != nil {
		return err
	}
	ns := o.Config.Output.ArgoCD.Namespace
	cm, err := o.Client.Kubernetes.CoreV1().ConfigMaps(ns).Get(ctx, "argocd-ssh-known-hosts-cm", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading Argo CD's known hosts: %w", err)
	}
	merged := MergeKnownHosts(cm.Data["ssh_known_hosts"], string(extra))
	if merged == cm.Data["ssh_known_hosts"] {
		return nil
	}
	o.log("adding %s to Argo CD's SSH known hosts", o.Git.KnownHostsFile)
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "argocd-ssh-known-hosts-cm", "namespace": ns},
		"data":     map[string]any{"ssh_known_hosts": merged},
	}}
	_, err = o.Client.Apply(ctx, u, kube.ApplyOptions{Force: true, FieldManager: settingsFieldManager})
	return err
}

// MergeKnownHosts appends the entries of extra that existing lacks, ignoring
// comments and blank lines.
func MergeKnownHosts(existing, extra string) string {
	have := map[string]bool{}
	for _, l := range strings.Split(existing, "\n") {
		have[strings.TrimSpace(l)] = true
	}
	out := strings.TrimRight(existing, "\n")
	for _, l := range strings.Split(extra, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") || have[l] {
			continue
		}
		have[l] = true
		if out != "" {
			out += "\n"
		}
		out += l
	}
	if out != "" {
		out += "\n"
	}
	return out
}

func flux(ctx context.Context, o Options) error {
	f := o.Config.Output.Flux
	c := o.Client
	o.log("installing Flux %s in namespace %s", f.Version, f.Namespace)
	objs, err := fetch(ctx, o.HTTPClient, render.FluxInstallURL(f.Version))
	if err != nil {
		return err
	}
	if err := applyAll(ctx, c, objs, f.Namespace, "flux-system"); err != nil {
		return err
	}
	o.log("waiting for Flux to become ready")
	if err := waitReady(ctx, c, f.Namespace, []string{"source-controller", "kustomize-controller", "helm-controller", "notification-controller"}, nil, o.Timeout); err != nil {
		return err
	}
	data := map[string]any{}
	g := o.Git
	switch {
	case g.SSHKeyFile != "":
		key, err := os.ReadFile(g.SSHKeyFile)
		if err != nil {
			return err
		}
		data["identity"] = string(key)
		if g.KnownHostsFile == "" {
			//lint:ignore ST1005 a product name
			return fmt.Errorf("Flux needs the Git server's host keys for SSH: pass --known-hosts (for example from ssh-keyscan)")
		}
		kh, err := os.ReadFile(g.KnownHostsFile)
		if err != nil {
			return err
		}
		data["known_hosts"] = string(kh)
	case g.Password != "":
		user := g.Username
		if user == "" {
			user = "git"
		}
		data["username"], data["password"] = user, g.Password
	}
	secret := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata":   map[string]any{"name": f.SecretName, "namespace": f.Namespace},
		"stringData": data,
	}}
	if _, err := c.Apply(ctx, secret, kube.ApplyOptions{Force: true}); err != nil {
		return err
	}
	if o.SOPSAgeKeyFile != "" {
		key, err := os.ReadFile(o.SOPSAgeKeyFile)
		if err != nil {
			return err
		}
		s := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Secret",
			"metadata":   map[string]any{"name": o.Config.Secrets.SOPS.DecryptionSecret, "namespace": f.Namespace},
			"stringData": map[string]any{"age.agekey": string(key)},
		}}
		if _, err := c.Apply(ctx, s, kube.ApplyOptions{Force: true}); err != nil {
			return err
		}
	}
	sync := filepath.Join(o.Dir, "clusters", o.Env.Name, "flux-system", "gotk-sync.yaml")
	o.log("applying %s", sync)
	return applyFile(ctx, c, sync, kube.FieldManager)
}

// fetch downloads a multi-document manifest.
func fetch(ctx context.Context, hc *http.Client, url string) ([]*unstructured.Unstructured, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return helm.ManifestObjects(string(b)), nil
}

// applyAll applies CRDs first and waits for them, then everything else.
// Namespaced objects without a namespace go to namespace; references to the
// upstream default namespace are rewritten when they differ.
func applyAll(ctx context.Context, c *kube.Client, objs []*unstructured.Unstructured, namespace, upstreamNS string) error {
	var crds, rest []*unstructured.Unstructured
	for _, u := range objs {
		if u.GetKind() == "CustomResourceDefinition" {
			crds = append(crds, u)
		} else {
			rest = append(rest, u)
		}
	}
	var names []string
	for _, u := range crds {
		if _, err := c.Apply(ctx, u, kube.ApplyOptions{Force: true}); err != nil {
			return fmt.Errorf("applying CRD %s: %w", u.GetName(), err)
		}
		names = append(names, u.GetName())
	}
	if err := c.WaitForCRDs(ctx, names, 2*time.Minute); err != nil {
		return err
	}
	for _, u := range rest {
		m, err := c.Mapping(u.GroupVersionKind())
		if err != nil {
			return fmt.Errorf("%s %s: %w", u.GetKind(), u.GetName(), err)
		}
		if m.Scope.Name() == meta.RESTScopeNameNamespace && u.GetNamespace() == "" {
			u.SetNamespace(namespace)
		}
		if namespace != upstreamNS {
			subjects, _, _ := unstructured.NestedSlice(u.Object, "subjects")
			for _, s := range subjects {
				if sm, ok := s.(map[string]any); ok && sm["namespace"] == upstreamNS {
					sm["namespace"] = namespace
				}
			}
			if subjects != nil {
				_ = unstructured.SetNestedSlice(u.Object, subjects, "subjects")
			}
		}
		if _, err := c.Apply(ctx, u, kube.ApplyOptions{Force: true}); err != nil {
			return fmt.Errorf("applying %s %s: %w", u.GetKind(), u.GetName(), err)
		}
	}
	return nil
}

// settingsFieldManager owns the keys gitopsify adds to upstream objects.
const settingsFieldManager = "gitopsify-settings"

func applyFile(ctx context.Context, c *kube.Client, path, fieldManager string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, u := range helm.ManifestObjects(string(data)) {
		if _, err := c.Apply(ctx, u, kube.ApplyOptions{Force: true, FieldManager: fieldManager}); err != nil {
			return fmt.Errorf("applying %s %s from %s: %w", u.GetKind(), u.GetName(), path, err)
		}
	}
	return nil
}

func restart(ctx context.Context, c *kube.Client, ns, name string) error {
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`, time.Now().Format(time.RFC3339))
	_, err := c.Kubernetes.AppsV1().Deployments(ns).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: kube.FieldManager})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func waitReady(ctx context.Context, c *kube.Client, ns string, deployments, statefulSets []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var pending []string
		for _, d := range deployments {
			dep, err := c.Kubernetes.AppsV1().Deployments(ns).Get(ctx, d, metav1.GetOptions{})
			if err != nil || !deploymentReady(dep) {
				pending = append(pending, "deployment/"+d)
			}
		}
		for _, s := range statefulSets {
			sts, err := c.Kubernetes.AppsV1().StatefulSets(ns).Get(ctx, s, metav1.GetOptions{})
			if err != nil || sts.Status.ReadyReplicas < replicas(sts.Spec.Replicas) {
				pending = append(pending, "statefulset/"+s)
			}
		}
		if len(pending) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			msg := fmt.Sprintf("timed out after %s waiting for %s in namespace %s", timeout, strings.Join(pending, ", "), ns)
			if why := podProblems(ctx, c, ns); why != "" {
				msg += ": " + why
			}
			return errors.New(msg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// podProblems describes the pods of a namespace that aren't running and
// ready: image pulls that fail, crash loops, pods that can't be scheduled.
func podProblems(ctx context.Context, c *kube.Client, ns string) string {
	pods, err := c.Kubernetes.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return ""
	}
	var out []string
	for _, p := range pods.Items {
		var why []string
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status != corev1.ConditionTrue && cond.Message != "" {
				why = append(why, "not scheduled: "+cond.Message)
			}
		}
		for _, cs := range append(p.Status.InitContainerStatuses, p.Status.ContainerStatuses...) {
			switch {
			case cs.State.Waiting != nil && cs.State.Waiting.Reason == "ContainerCreating":
				why = append(why, cs.Name+" still being created (pulling its image? kubelet pulls one image at a time, so slow registries add up)")
			case cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && cs.State.Waiting.Reason != "PodInitializing":
				why = append(why, fmt.Sprintf("%s %s: %s", cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message))
			case cs.LastTerminationState.Terminated != nil && cs.RestartCount > 0:
				t := cs.LastTerminationState.Terminated
				why = append(why, fmt.Sprintf("%s restarted %d times, last %s (exit %d)", cs.Name, cs.RestartCount, t.Reason, t.ExitCode))
			case !cs.Ready && cs.State.Running != nil:
				why = append(why, cs.Name+" running but not ready")
			}
		}
		if len(why) > 0 {
			out = append(out, p.Name+": "+strings.Join(why, "; "))
		}
	}
	sort.Strings(out)
	if len(out) > 5 {
		out = append(out[:5], fmt.Sprintf("and %d more", len(out)-5))
	}
	return strings.Join(out, " | ")
}

func deploymentReady(d *appsv1.Deployment) bool {
	return d.Status.ObservedGeneration >= d.Generation && d.Status.UpdatedReplicas >= replicas(d.Spec.Replicas) && d.Status.AvailableReplicas >= replicas(d.Spec.Replicas)
}

func replicas(r *int32) int32 {
	if r == nil {
		return 1
	}
	return *r
}
