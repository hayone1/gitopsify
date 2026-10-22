package kube

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/hayone1/gitopsify/pkg/config"
)

// credentialPlugins maps the commands kubeconfigs run to get credentials to
// the platform they belong to and what to do when the command is missing or
// its login expired.
var credentialPlugins = map[string]struct{ platform, install, login string }{
	"aws":                    {"Amazon EKS", "install the AWS CLI v2 (https://aws.amazon.com/cli/)", "refresh your AWS credentials (aws sso login, or check AWS_PROFILE)"},
	"aws-iam-authenticator":  {"Amazon EKS", "install aws-iam-authenticator, or regenerate the kubeconfig with `aws eks update-kubeconfig`", "refresh your AWS credentials (aws sso login, or check AWS_PROFILE)"},
	"gke-gcloud-auth-plugin": {"Google GKE", "install it with `gcloud components install gke-gcloud-auth-plugin`", "log in again with `gcloud auth login`"},
	"gcloud":                 {"Google GKE", "install the Google Cloud CLI and gke-gcloud-auth-plugin", "log in again with `gcloud auth login`"},
	"kubelogin":              {"Azure AKS (Entra ID) or an OIDC provider", "install kubelogin (https://azure.github.io/kubelogin/ for AKS, or `kubectl krew install oidc-login`)", "log in again (az login, or remove the cached token with `kubelogin remove-tokens`)"},
	"kubectl-oidc_login":     {"an OIDC provider", "install it with `kubectl krew install oidc-login`", "log in again; the cached OIDC token expired"},
	"tsh":                    {"Teleport", "install the Teleport client (tsh)", "log in again with `tsh login`"},
	"doctl":                  {"DigitalOcean Kubernetes", "install doctl (https://docs.digitalocean.com/reference/doctl/)", "log in again with `doctl auth init`"},
	"oci":                    {"Oracle OKE", "install the OCI CLI", "refresh your OCI session"},
	"ibmcloud":               {"IBM Cloud Kubernetes Service", "install the IBM Cloud CLI", "log in again with `ibmcloud login`"},
	"rancher":                {"Rancher", "install the Rancher CLI", "log in again with `rancher login`"},
	"openstack":              {"OpenStack Magnum", "install the OpenStack client", "refresh your OpenStack credentials"},
	"pinniped":               {"Pinniped", "install the pinniped CLI", "log in again with Pinniped"},
	"vcluster":               {"vcluster", "install the vcluster CLI", "reconnect with `vcluster connect`"},
	"civo":                   {"Civo", "install the Civo CLI", "log in again with the Civo CLI"},
	"linode-cli":             {"Linode LKE", "install linode-cli", "refresh your Linode token"},
	"scw":                    {"Scaleway Kapsule", "install the Scaleway CLI", "refresh your Scaleway credentials"},
}

func pluginCommand(cfg *rest.Config) string {
	if cfg == nil || cfg.ExecProvider == nil {
		return ""
	}
	cmd := filepath.Base(cfg.ExecProvider.Command)
	cmd = strings.TrimSuffix(cmd, ".exe")
	// `kubectl oidc-login get-token` runs the plugin through kubectl.
	if cmd == "kubectl" && len(cfg.ExecProvider.Args) > 0 && cfg.ExecProvider.Args[0] == "oidc-login" {
		return "kubectl-oidc_login"
	}
	return cmd
}

// DescribeAuth says how a rest config authenticates, for people.
func DescribeAuth(cfg *rest.Config, inCluster bool) string {
	var s string
	switch {
	case inCluster:
		s = "in-cluster ServiceAccount token"
	case cfg.ExecProvider != nil:
		cmd := pluginCommand(cfg)
		s = "credential plugin " + cmd
		if p, ok := credentialPlugins[cmd]; ok {
			s += " (" + p.platform + ")"
		}
	case cfg.AuthProvider != nil:
		s = "auth provider " + cfg.AuthProvider.Name
	case cfg.BearerTokenFile != "":
		s = "bearer token from " + cfg.BearerTokenFile
	case cfg.BearerToken != "":
		s = "bearer token"
	case cfg.CertFile != "" || len(cfg.CertData) > 0:
		s = "client certificate"
	case cfg.Username != "":
		s = "basic auth as " + cfg.Username
	default:
		s = "anonymous"
	}
	if cfg.Impersonate.UserName != "" {
		s += ", impersonating " + cfg.Impersonate.UserName
		if len(cfg.Impersonate.Groups) > 0 {
			s += " (groups " + strings.Join(cfg.Impersonate.Groups, ", ") + ")"
		}
	}
	if cfg.Insecure {
		s += ", TLS verification off"
	}
	return s
}

// explainConfigError adds what to do to kubeconfig loading errors.
func explainConfigError(err error, rules *clientcmd.ClientConfigLoadingRules, a config.Access) error {
	msg := err.Error()
	switch {
	case clientcmd.IsEmptyConfig(err):
		files := rules.Precedence
		if rules.ExplicitPath != "" {
			files = []string{rules.ExplicitPath}
		}
		return fmt.Errorf("no cluster to connect to: no kubeconfig found (looked in %s). Point KUBECONFIG or --kubeconfig at one, pass --server with --token or --client-certificate, or --in-cluster inside a pod", strings.Join(files, ", "))
	case strings.Contains(msg, "context was not found") || strings.Contains(msg, "does not exist") && a.Context != "":
		if raw, lerr := rules.Load(); lerr == nil {
			names := make([]string, 0, len(raw.Contexts))
			for n := range raw.Contexts {
				names = append(names, n)
			}
			sort.Strings(names)
			if len(names) > 0 {
				return fmt.Errorf("%w (contexts in the kubeconfig: %s)", err, strings.Join(names, ", "))
			}
		}
	}
	return fmt.Errorf("loading kubeconfig: %w", err)
}

// Explain adds what to do to an error from talking to the API server:
// unreachable servers, untrusted certificates, missing credential plugins and
// rejected credentials.
func (c *Client) Explain(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	cmd := pluginCommand(c.Config)
	plugin, known := credentialPlugins[cmd]
	var dnsErr *net.DNSError
	var opErr *net.OpError
	switch {
	case strings.Contains(msg, "executable file not found") || strings.Contains(msg, "exec: executable") && strings.Contains(msg, "not found"):
		hint := "install it or fix the command in the kubeconfig"
		if known {
			hint = plugin.install
		}
		return fmt.Errorf("%w\nThe kubeconfig gets credentials by running %q (%s): %s", err, cmd, orUnknown(plugin.platform), hint)
	case strings.Contains(msg, "exec plugin") || strings.Contains(msg, "getting credentials"):
		hint := "run the plugin by hand to see what it needs"
		if known {
			hint = plugin.login
		}
		return fmt.Errorf("%w\nThe credential plugin %q failed: %s", err, cmd, hint)
	case strings.Contains(msg, "x509: certificate is valid for") || strings.Contains(msg, "certificate is not valid for"):
		return fmt.Errorf("%w\nThe API server's certificate doesn't name the address used to reach it: pass --tls-server-name with a name it is valid for", err)
	case strings.Contains(msg, "x509:") || strings.Contains(msg, "tls: failed to verify certificate"):
		return fmt.Errorf("%w\nThe API server's certificate isn't trusted: pass --certificate-authority with its CA (or set certificate-authority-data in the kubeconfig); --insecure-skip-tls-verify only for throwaway clusters", err)
	case apierrors.IsUnauthorized(err) || strings.Contains(msg, "Unauthorized"):
		hint := "the token or client certificate was rejected or has expired"
		switch {
		case known:
			hint = plugin.login
		case c.Config.AuthProvider != nil && c.Config.AuthProvider.Name == "oidc":
			hint = "the OIDC id-token expired and couldn't be refreshed; log in again"
		case c.Config.BearerToken != "" || c.Config.BearerTokenFile != "":
			hint = "the bearer token is wrong or has expired"
		}
		return fmt.Errorf("%w\nThe API server rejected the credentials (%s): %s", err, c.Auth, hint)
	case errors.As(err, &dnsErr):
		return fmt.Errorf("%w\nThe API server's name doesn't resolve: check the server URL in the kubeconfig, your VPN or DNS", err)
	case errors.As(err, &opErr), strings.Contains(msg, "connection refused"), strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "context deadline exceeded"), strings.Contains(msg, "no route to host"):
		return fmt.Errorf("%w\nCan't reach %s: is the cluster running, and reachable from here (VPN, firewall, proxy, private endpoint)?", err, c.Server)
	}
	return err
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown platform"
	}
	return s
}

// Ping returns the API server's version, or an explained connection error.
func (c *Client) Ping() (string, error) {
	v, err := c.ServerVersion()
	if err != nil {
		return "", c.Explain(err)
	}
	return v, nil
}

// Identity is who the API server thinks the client is.
type Identity struct {
	Username string   `json:"username"`
	Groups   []string `json:"groups,omitempty"`
}

// WhoAmI asks the API server who the client is (SelfSubjectReview, Kubernetes
// 1.28+).
func (c *Client) WhoAmI(ctx context.Context) (*Identity, error) {
	r, err := c.Kubernetes.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return nil, c.Explain(err)
	}
	u := r.Status.UserInfo
	return &Identity{Username: u.Username, Groups: u.Groups}, nil
}

// Permission is an API access to check.
type Permission struct {
	Verb      string
	Group     string
	Resource  string
	Namespace string
}

func (p Permission) String() string {
	r := p.Resource
	if p.Group != "" && p.Group != "*" {
		r += "." + p.Group
	}
	if p.Namespace != "" {
		return p.Verb + " " + r + " in " + p.Namespace
	}
	return p.Verb + " " + r
}

// CanI reports whether the client may perform an access
// (SelfSubjectAccessReview, like kubectl auth can-i).
func (c *Client) CanI(ctx context.Context, p Permission) (bool, error) {
	r, err := c.Kubernetes.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
			Verb: p.Verb, Group: p.Group, Resource: p.Resource, Namespace: p.Namespace,
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, c.Explain(err)
	}
	return r.Status.Allowed, nil
}
