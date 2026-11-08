package server

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/hayone1/gitopsify/pkg/config"
	"github.com/hayone1/gitopsify/pkg/kube"
	"github.com/hayone1/gitopsify/pkg/match"
)

// clusterInfo is a cluster as the API lists it.
type clusterInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Impersonate bool   `json:"impersonate,omitempty"`
	// Role is the caller's role on it.
	Role string `json:"role"`
}

// clusters returns the configured clusters; in local mode, the contexts of
// the kubeconfig gitopsify ui runs with.
func (s *Server) clusterList() ([]config.ServerCluster, error) {
	if !s.local {
		return s.cfg.Clusters, nil
	}
	if len(s.cfg.Clusters) > 0 {
		return s.cfg.Clusters, nil
	}
	rules := kube.LoadingRules(s.kubeconfig)
	raw, err := rules.Load()
	if err != nil {
		return nil, fmt.Errorf("reading the kubeconfig: %w", err)
	}
	var out []config.ServerCluster
	for name := range raw.Contexts {
		desc := ""
		if name == raw.CurrentContext {
			desc = "current context"
		}
		out = append(out, config.ServerCluster{Name: name, Description: desc, Access: config.Access{Kubeconfig: s.kubeconfig, Context: name}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Server) cluster(name string) (*config.ServerCluster, error) {
	list, err := s.clusterList()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == name {
			return &list[i], nil
		}
	}
	return nil, fmt.Errorf("no cluster named %q", name)
}

// access returns how to reach a cluster on behalf of u: with the cluster's
// token, and as u when the cluster impersonates.
func (s *Server) access(c *config.ServerCluster, u *User) (config.Access, error) {
	a := c.Access
	if c.Token != (config.SettingValue{}) {
		t, err := c.Token.Resolve()
		if err != nil {
			return a, fmt.Errorf("cluster %s token: %w", c.Name, err)
		}
		a.Token = t
	}
	if c.Impersonate && u != nil && u.Via != "local" {
		// Never a Kubernetes-reserved identity: an identity provider group
		// called system:masters must not become cluster-admin.
		if strings.HasPrefix(u.Username, "system:") {
			return a, fmt.Errorf("cluster %s: %q can't be impersonated", c.Name, u.Username)
		}
		a.Impersonate = c.ImpersonateUserPrefix + u.Username
		a.ImpersonateGroups = []string{"system:authenticated"}
		for _, g := range u.Groups {
			if impersonable(c, g) {
				a.ImpersonateGroups = append(a.ImpersonateGroups, c.ImpersonateGroupPrefix+g)
			}
		}
	}
	return a, nil
}

// impersonable reports whether a group of the signed-in person is passed
// to the cluster. Groups Kubernetes and kubeadm bind to administrators
// (system:masters, kubeadm:cluster-admins) never are, unprefixed.
func impersonable(c *config.ServerCluster, g string) bool {
	if strings.HasPrefix(g, "system:") {
		return false
	}
	if len(c.ImpersonateGroups) > 0 && !match.Glob(c.ImpersonateGroups, g) {
		return false
	}
	return c.ImpersonateGroupPrefix != "" || !strings.HasPrefix(g, "kubeadm:")
}

func (s *Server) connect(name string, u *User) (*kube.Client, error) {
	c, err := s.cluster(name)
	if err != nil {
		return nil, err
	}
	a, err := s.access(c, u)
	if err != nil {
		return nil, err
	}
	return kube.Connect(a, kube.ConnectOptions{NoInput: true})
}

// clusterCheck is the result of checking a cluster connection.
type clusterCheck struct {
	OK       bool   `json:"ok"`
	Version  string `json:"version,omitempty"`
	Identity string `json:"identity,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (s *Server) checkCluster(ctx context.Context, name string, u *User) clusterCheck {
	cl, err := s.connect(name, u)
	if err != nil {
		return clusterCheck{Error: err.Error()}
	}
	v, err := cl.Ping()
	if err != nil {
		return clusterCheck{Error: cl.Explain(err).Error()}
	}
	res := clusterCheck{OK: true, Version: v}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if id, err := cl.WhoAmI(ctx); err == nil {
		res.Identity = id.Username
	}
	return res
}

func (s *Server) namespaces(ctx context.Context, name string, u *User) ([]string, error) {
	cl, err := s.connect(name, u)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	list, err := cl.Kubernetes.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, cl.Explain(err)
	}
	out := make([]string, 0, len(list.Items))
	for _, n := range list.Items {
		out = append(out, n.Name)
	}
	sort.Strings(out)
	return out, nil
}
