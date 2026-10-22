// Package kube builds Kubernetes clients from a kubeconfig context and applies
// objects with server-side apply.
package kube

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	// Auth providers referenced by kubeconfigs: oidc, and the removed gcp and
	// azure providers, which then fail with a pointer to their replacement
	// instead of "no Auth Provider found".
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/hayone1/gitopsify/pkg/config"
)

// FieldManager is the server-side apply field manager gitopsify uses.
const FieldManager = "gitopsify"

// Client bundles the clients gitopsify needs for one cluster.
type Client struct {
	// Context names the cluster for people: the kubeconfig context, the API
	// server URL or "in-cluster".
	Context    string
	Server     string
	Config     *rest.Config
	Discovery  discovery.DiscoveryInterface
	Dynamic    dynamic.Interface
	Kubernetes kubernetes.Interface
	Mapper     *restmapper.DeferredDiscoveryRESTMapper
	// Auth describes how the client authenticates.
	Auth string
}

// ConnectOptions tune Connect.
type ConnectOptions struct {
	// NoInput stops credential plugins from prompting on the terminal; they
	// fail instead of waiting for input that never comes.
	NoInput bool
}

// Connect builds the clients of a cluster from its access settings: the
// kubeconfig (with its credential plugins, as kubectl uses it), overridden or
// replaced by the explicit settings, or the pod's ServiceAccount.
func Connect(a config.Access, o ConnectOptions) (*Client, error) {
	cfg, name, err := RESTConfig(a, o)
	if err != nil {
		return nil, err
	}
	c, err := NewForConfig(cfg, name)
	if err != nil {
		return nil, err
	}
	c.Auth = DescribeAuth(cfg, a.InCluster)
	return c, nil
}

// New connects to the cluster of a kubeconfig context. Empty values use the
// standard kubeconfig loading rules and the current context.
func New(kubeconfig, kubeContext string) (*Client, error) {
	return Connect(config.Access{Kubeconfig: kubeconfig, Context: kubeContext}, ConnectOptions{})
}

// RESTConfig resolves access settings into a rest config and a display name.
func RESTConfig(a config.Access, o ConnectOptions) (*rest.Config, string, error) {
	if err := a.Validate(); err != nil {
		return nil, "", err
	}
	var cfg *rest.Config
	name := a.Context
	if a.InCluster {
		var err error
		if cfg, err = rest.InClusterConfig(); err != nil {
			return nil, "", fmt.Errorf("in-cluster access: %w (inCluster only works inside a pod)", err)
		}
		name = "in-cluster"
		cfg = overrideInCluster(cfg, a)
	} else {
		rules := LoadingRules(a.Kubeconfig)
		if a.APIServer != "" && a.Kubeconfig == "" && a.Context == "" {
			// An explicit server without a kubeconfig or context stands
			// alone: the current context's credentials and CA belong to
			// another server.
			rules = &clientcmd.ClientConfigLoadingRules{}
		}
		overrides := &clientcmd.ConfigOverrides{
			CurrentContext: a.Context,
			ClusterInfo: clientcmdapi.Cluster{
				Server:                a.APIServer,
				CertificateAuthority:  config.ExpandHome(a.CertificateAuthority),
				InsecureSkipTLSVerify: a.InsecureSkipTLSVerify,
				TLSServerName:         a.TLSServerName,
				ProxyURL:              a.ProxyURL,
			},
			AuthInfo: clientcmdapi.AuthInfo{
				Token:             a.Token,
				TokenFile:         config.ExpandHome(a.TokenFile),
				ClientCertificate: config.ExpandHome(a.ClientCertificate),
				ClientKey:         config.ExpandHome(a.ClientKey),
				Impersonate:       a.Impersonate,
				ImpersonateGroups: a.ImpersonateGroups,
				ImpersonateUID:    a.ImpersonateUID,
			},
		}
		if a.Timeout != "" {
			overrides.Timeout = a.Timeout
		}
		cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
		var err error
		if cfg, err = cc.ClientConfig(); err != nil {
			return nil, "", explainConfigError(err, rules, a)
		}
		if name == "" {
			if raw, err := cc.RawConfig(); err == nil && raw.CurrentContext != "" && a.APIServer == "" {
				name = raw.CurrentContext
			}
		}
		if name == "" {
			name = cfg.Host
		}
	}
	if cfg.ExecProvider != nil && o.NoInput {
		cfg.ExecProvider.StdinUnavailable = true
		cfg.ExecProvider.StdinUnavailableMessage = "gitopsify runs with --no-input; log in first (for example aws sso login, gcloud auth login, az login) or use a non-interactive credential"
	}
	return cfg, name, nil
}

// overrideInCluster applies the explicit settings that make sense for the
// pod's own cluster.
func overrideInCluster(cfg *rest.Config, a config.Access) *rest.Config {
	if a.Impersonate != "" {
		cfg.Impersonate = rest.ImpersonationConfig{UserName: a.Impersonate, Groups: a.ImpersonateGroups, UID: a.ImpersonateUID}
	}
	if a.Timeout != "" {
		cfg.Timeout, _ = time.ParseDuration(a.Timeout)
	}
	if a.Token != "" || a.TokenFile != "" {
		cfg.BearerToken, cfg.BearerTokenFile = a.Token, config.ExpandHome(a.TokenFile)
	}
	return cfg
}

// LoadingRules returns kubectl's kubeconfig loading rules for an optional
// kubeconfig setting, which may list several files.
func LoadingRules(kubeconfig string) *clientcmd.ClientConfigLoadingRules {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	kubeconfig = config.ExpandHome(kubeconfig)
	switch files := filepath.SplitList(kubeconfig); {
	case len(files) > 1:
		rules.Precedence = files
	case kubeconfig != "":
		rules.ExplicitPath = kubeconfig
	}
	return rules
}

// NewForConfig builds the clients from a rest config.
func NewForConfig(cfg *rest.Config, contextName string) (*Client, error) {
	cfg = rest.CopyConfig(cfg)
	// Listing a whole cluster issues many requests; the client-go defaults
	// (5 QPS) make that crawl.
	cfg.QPS = 50
	cfg.Burst = 100
	cfg.UserAgent = "gitopsify"
	// Deprecation warnings (v1 Endpoints, ...) are noise for an exporter.
	cfg.WarningHandler = rest.NoWarnings{}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	cached := memory.NewMemCacheClient(dc)
	return &Client{
		Context:    contextName,
		Server:     cfg.Host,
		Config:     cfg,
		Discovery:  cached,
		Dynamic:    dyn,
		Kubernetes: kc,
		Mapper:     restmapper.NewDeferredDiscoveryRESTMapper(cached),
	}, nil
}

// ServerVersion returns the API server's git version.
func (c *Client) ServerVersion() (string, error) {
	v, err := c.Discovery.ServerVersion()
	if err != nil {
		return "", err
	}
	return v.GitVersion, nil
}

// Mapping resolves the resource of a kind, refreshing discovery once when
// the kind is unknown (for example right after its CRD was created).
func (c *Client) Mapping(gvk schema.GroupVersionKind) (*meta.RESTMapping, error) {
	m, err := c.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil && meta.IsNoMatchError(err) {
		c.Mapper.Reset()
		m, err = c.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	}
	return m, err
}

// ResourceFor returns the dynamic resource interface of an object.
func (c *Client) ResourceFor(u *unstructured.Unstructured) (dynamic.ResourceInterface, *meta.RESTMapping, error) {
	m, err := c.Mapping(u.GroupVersionKind())
	if err != nil {
		return nil, nil, err
	}
	if m.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := u.GetNamespace()
		if ns == "" {
			ns = metav1.NamespaceDefault
		}
		return c.Dynamic.Resource(m.Resource).Namespace(ns), m, nil
	}
	return c.Dynamic.Resource(m.Resource), m, nil
}

// ApplyOptions tune Apply.
type ApplyOptions struct {
	DryRun       bool
	FieldManager string
	Force        bool
}

// Apply server-side applies an object and returns the result.
func (c *Client) Apply(ctx context.Context, u *unstructured.Unstructured, o ApplyOptions) (*unstructured.Unstructured, error) {
	ri, m, err := c.ResourceFor(u)
	if err != nil {
		return nil, err
	}
	obj := u.DeepCopy()
	if m.Scope.Name() == meta.RESTScopeNameNamespace && obj.GetNamespace() == "" {
		obj.SetNamespace(metav1.NamespaceDefault)
	}
	data, err := obj.MarshalJSON()
	if err != nil {
		return nil, err
	}
	fm := o.FieldManager
	if fm == "" {
		fm = FieldManager
	}
	po := metav1.PatchOptions{FieldManager: fm, Force: &o.Force}
	if o.DryRun {
		po.DryRun = []string{metav1.DryRunAll}
	}
	return ri.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, po)
}

// Get returns the live object, or nil when it does not exist.
func (c *Client) Get(ctx context.Context, u *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	ri, _, err := c.ResourceFor(u)
	if err != nil {
		return nil, err
	}
	live, err := ri.Get(ctx, u.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return live, err
}

// WaitForCRDs waits until the given CRDs are established.
func (c *Client) WaitForCRDs(ctx context.Context, names []string, timeout time.Duration) error {
	gvr := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	deadline := time.Now().Add(timeout)
	for _, n := range names {
		for {
			crd, err := c.Dynamic.Resource(gvr).Get(ctx, n, metav1.GetOptions{})
			if err == nil && crdEstablished(crd) {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("timed out waiting for CRD %s to be established", n)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	c.Mapper.Reset()
	return nil
}

func crdEstablished(crd *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if ok && m["type"] == "Established" && m["status"] == "True" {
			return true
		}
	}
	return false
}
