// Package discovery lists the objects of a cluster through API discovery.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/hayone1/gitopsify/pkg/kube"
	"github.com/hayone1/gitopsify/pkg/match"
	"github.com/hayone1/gitopsify/pkg/model"
)

// Resource is an API resource gitopsify can list.
type Resource struct {
	GVR        schema.GroupVersionResource
	Kind       string
	Namespaced bool
	ShortNames []string
}

// KindInfo returns the matching information of the resource.
func (r Resource) KindInfo() match.KindInfo {
	return match.KindInfo{Group: r.GVR.Group, Kind: r.Kind, Resource: r.GVR.Resource, ShortNames: r.ShortNames}
}

// neverList are resources that are never deployable and can be very large,
// so they are not even listed.
var neverList = map[string]string{
	"events":                    "events are runtime records",
	"events.events.k8s.io":      "events are runtime records",
	"componentstatuses":         "deprecated, read-only status",
	"bindings":                  "write-only API",
	"podtemplates":              "rarely used, controller-owned in practice",
	"metrics.k8s.io":            "metrics are runtime data",
	"custom.metrics.k8s.io":     "metrics are runtime data",
	"external.metrics.k8s.io":   "metrics are runtime data",
	"packages.operators.coreos": "OLM package manifests are generated",
}

func skipReason(group, resource string) (string, bool) {
	if r, ok := neverList[group]; ok {
		return r, true
	}
	key := resource
	if group != "" {
		key += "." + group
	}
	r, ok := neverList[key]
	return r, ok
}

// APIResources returns the preferred version of every listable resource.
// Groups whose discovery failed (typically an unavailable aggregated API) are
// reported as warnings rather than errors.
func APIResources(c *kube.Client) ([]Resource, []string, error) {
	lists, err := c.Discovery.ServerPreferredResources()
	var warnings []string
	if err != nil {
		var gdf *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &gdf) {
			return nil, nil, fmt.Errorf("API discovery: %w", err)
		}
		for gv, e := range gdf.Groups {
			warnings = append(warnings, fmt.Sprintf("API group %s is unavailable and was skipped: %v", gv, e))
		}
	}
	var out []Resource
	for _, l := range lists {
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range l.APIResources {
			if strings.Contains(r.Name, "/") || !hasVerbs(r.Verbs, "list", "get") {
				continue
			}
			out = append(out, Resource{
				GVR:        gv.WithResource(r.Name),
				Kind:       r.Kind,
				Namespaced: r.Namespaced,
				ShortNames: r.ShortNames,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GVR.Group != out[j].GVR.Group {
			return out[i].GVR.Group < out[j].GVR.Group
		}
		return out[i].GVR.Resource < out[j].GVR.Resource
	})
	sort.Strings(warnings)
	return out, warnings, nil
}

func hasVerbs(verbs metav1.Verbs, want ...string) bool {
	for _, w := range want {
		found := false
		for _, v := range verbs {
			if v == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Options select what to list.
type Options struct {
	// Namespaces to list namespaced resources in; empty lists all of them.
	Namespaces []string
	// ClusterScoped also lists cluster-scoped resources.
	ClusterScoped bool
	// Concurrency is the number of parallel list calls (default 8).
	Concurrency int
	// Progress is called after every resource is listed.
	Progress func(done, total int)
}

// Result is the outcome of Discover.
type Result struct {
	Resources []Resource
	Objects   []*model.Object
	Warnings  []string
	// Skipped lists resources that were not listed, with the reason.
	Skipped []string
}

// Discover lists every object of every listable resource in scope.
func Discover(ctx context.Context, c *kube.Client, opts Options) (*Result, error) {
	resources, warnings, err := APIResources(c)
	if err != nil {
		return nil, err
	}
	res := &Result{Resources: resources, Warnings: warnings}

	type job struct {
		r  Resource
		ns string
	}
	var jobs []job
	for _, r := range resources {
		if reason, skip := skipReason(r.GVR.Group, r.GVR.Resource); skip {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s: %s", groupResource(r.GVR), reason))
			continue
		}
		switch {
		case !r.Namespaced && !opts.ClusterScoped:
			continue
		case !r.Namespaced || len(opts.Namespaces) == 0:
			jobs = append(jobs, job{r: r})
		default:
			for _, ns := range opts.Namespaces {
				jobs = append(jobs, job{r: r, ns: ns})
			}
		}
	}

	conc := opts.Concurrency
	if conc <= 0 {
		conc = 8
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(conc)
	var mu sync.Mutex
	done := 0
	for _, j := range jobs {
		g.Go(func() error {
			objs, err := list(gctx, c, j.r, j.ns)
			mu.Lock()
			defer mu.Unlock()
			done++
			if opts.Progress != nil {
				opts.Progress(done, len(jobs))
			}
			if err != nil {
				switch {
				case apierrors.IsForbidden(err):
					res.Warnings = append(res.Warnings, fmt.Sprintf("not allowed to list %s%s, so none are migrated: %v", groupResource(j.r.GVR), inNS(j.ns), err))
					return nil
				case apierrors.IsNotFound(err) || apierrors.IsMethodNotSupported(err):
					res.Warnings = append(res.Warnings, fmt.Sprintf("cannot list %s%s: %v", groupResource(j.r.GVR), inNS(j.ns), err))
					return nil
				case gctx.Err() == nil && !builtinGroup(j.r.GVR.Group):
					// A custom or aggregated API that keeps failing (a
					// conversion webhook or an API server that is down)
					// shouldn't stop the whole export.
					res.Warnings = append(res.Warnings, fmt.Sprintf("listing %s%s failed, so none are migrated: %v", groupResource(j.r.GVR), inNS(j.ns), err))
					return nil
				}
				return fmt.Errorf("listing %s%s: %w", groupResource(j.r.GVR), inNS(j.ns), c.Explain(err))
			}
			res.Objects = append(res.Objects, objs...)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	model.SortObjects(res.Objects)
	sort.Strings(res.Warnings)
	return res, nil
}

// builtinGroup reports whether an API group is served by Kubernetes itself.
func builtinGroup(g string) bool {
	return g == "" || !strings.Contains(g, ".") || strings.HasSuffix(g, ".k8s.io") && !strings.HasSuffix(g, ".x-k8s.io")
}

// retryable reports whether a list error is worth retrying: throttling,
// overloaded or restarting API servers, dropped connections.
func retryable(err error) bool {
	if apierrors.IsTooManyRequests(err) || apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err) || apierrors.IsUnexpectedServerError(err) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection reset") || strings.Contains(msg, "http2: client connection lost") || strings.Contains(msg, "TLS handshake timeout")
}

// Backoff is the wait between attempts of a failing list call; tests shorten it.
var Backoff = func(attempt int) time.Duration { return time.Duration(attempt*attempt) * time.Second }

func listPage(ctx context.Context, ri dynamic.ResourceInterface, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	const attempts = 4
	for i := 1; ; i++ {
		ul, err := ri.List(ctx, opts)
		if err == nil || i == attempts || !retryable(err) {
			return ul, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(Backoff(i)):
		}
	}
}

func list(ctx context.Context, c *kube.Client, r Resource, ns string) ([]*model.Object, error) {
	var ri dynamic.ResourceInterface = c.Dynamic.Resource(r.GVR)
	if r.Namespaced && ns != "" {
		ri = c.Dynamic.Resource(r.GVR).Namespace(ns)
	}
	var out []*model.Object
	opts := metav1.ListOptions{Limit: 500}
	restarted := false
	for {
		ul, err := listPage(ctx, ri, opts)
		if apierrors.IsResourceExpired(err) && opts.Continue != "" && !restarted {
			// The continue token expired (a slow, very large list): start
			// over once, from a fresh snapshot.
			restarted, out, opts.Continue = true, nil, ""
			continue
		}
		if err != nil {
			return nil, err
		}
		for i := range ul.Items {
			u := &ul.Items[i]
			if u.GetKind() == "" {
				u.SetGroupVersionKind(r.GVR.GroupVersion().WithKind(r.Kind))
			}
			out = append(out, &model.Object{
				Ref:        model.RefOf(u),
				GVR:        r.GVR,
				Namespaced: r.Namespaced,
				Live:       u,
			})
		}
		if ul.GetContinue() == "" {
			return out, nil
		}
		opts.Continue = ul.GetContinue()
	}
}

func groupResource(gvr schema.GroupVersionResource) string {
	return gvr.GroupResource().String()
}

func inNS(ns string) string {
	if ns == "" {
		return ""
	}
	return " in namespace " + ns
}
