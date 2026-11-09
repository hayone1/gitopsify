package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/hayone1/gitopsify/internal/telemetry"
)

// metrics is a small Prometheus text-format registry: request counts and
// durations, runs started and finished. Enough for dashboards and alerts
// without the client library's dependencies. The same counts go to
// OpenTelemetry when its metrics are on (requests through otelhttp).
type metrics struct {
	otelRuns   metric.Int64Counter
	otelActive metric.Int64UpDownCounter
	otelLogins metric.Int64Counter

	mu       sync.Mutex
	requests map[string]float64 // method,route,code
	duration map[string]*histogram
	runs     map[string]float64 // type,status
	active   map[string]float64 // type
	logins   map[string]float64 // outcome
	started  time.Time
}

var buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type histogram struct {
	counts []float64
	sum    float64
	count  float64
}

func newMetrics() *metrics {
	m := &metrics{requests: map[string]float64{}, duration: map[string]*histogram{}, runs: map[string]float64{},
		active: map[string]float64{}, logins: map[string]float64{}, started: time.Now()}
	meter := telemetry.Meter()
	// Errors only come from invalid names; the instruments are then no-ops.
	m.otelRuns, _ = meter.Int64Counter("gitopsify.runs", metric.WithUnit("{run}"), metric.WithDescription("Finished runs by type and status."))
	m.otelActive, _ = meter.Int64UpDownCounter("gitopsify.runs.active", metric.WithUnit("{run}"), metric.WithDescription("Runs queued or working, by type."))
	m.otelLogins, _ = meter.Int64Counter("gitopsify.logins", metric.WithUnit("{login}"), metric.WithDescription("Sign-ins by outcome."))
	return m
}

func (m *metrics) request(method, route string, code int, d time.Duration) {
	if m == nil {
		return
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
	default:
		method = "other" // clients pick methods: keep the series bounded
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[method+","+route+","+strconv.Itoa(code)]++
	h := m.duration[route]
	if h == nil {
		h = &histogram{counts: make([]float64, len(buckets))}
		m.duration[route] = h
	}
	s := d.Seconds()
	for i, b := range buckets {
		if s <= b {
			h.counts[i]++
		}
	}
	h.sum += s
	h.count++
}

func (m *metrics) runStarted(typ string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.active[typ]++
	m.mu.Unlock()
	m.otelActive.Add(context.Background(), 1, metric.WithAttributes(attribute.String("gitopsify.run.type", typ)))
}

func (m *metrics) runFinished(typ, status string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.active[typ]--
	m.runs[typ+","+status]++
	m.mu.Unlock()
	t := attribute.String("gitopsify.run.type", typ)
	m.otelActive.Add(context.Background(), -1, metric.WithAttributes(t))
	m.otelRuns.Add(context.Background(), 1, metric.WithAttributes(t, attribute.String("gitopsify.run.status", status)))
}

func (m *metrics) login(outcome string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.logins[outcome]++
	m.mu.Unlock()
	m.otelLogins.Add(context.Background(), 1, metric.WithAttributes(attribute.String("gitopsify.login.outcome", outcome)))
}

func (m *metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	write := func(name, help, typ string, vals map[string]float64, labels ...string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		keys := make([]string, 0, len(vals))
		for k := range vals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			parts := strings.Split(k, ",")
			var ls []string
			for i, l := range labels {
				if i < len(parts) {
					ls = append(ls, fmt.Sprintf("%s=%q", l, parts[i]))
				}
			}
			fmt.Fprintf(&b, "%s{%s} %g\n", name, strings.Join(ls, ","), vals[k])
		}
	}
	write("gitopsify_http_requests_total", "HTTP requests by method, route and status code.", "counter", m.requests, "method", "route", "code")
	fmt.Fprintf(&b, "# HELP gitopsify_http_request_duration_seconds HTTP request durations by route.\n# TYPE gitopsify_http_request_duration_seconds histogram\n")
	routes := make([]string, 0, len(m.duration))
	for r := range m.duration {
		routes = append(routes, r)
	}
	sort.Strings(routes)
	for _, r := range routes {
		h := m.duration[r]
		for i, bk := range buckets {
			fmt.Fprintf(&b, "gitopsify_http_request_duration_seconds_bucket{route=%q,le=\"%g\"} %g\n", r, bk, h.counts[i])
		}
		fmt.Fprintf(&b, "gitopsify_http_request_duration_seconds_bucket{route=%q,le=\"+Inf\"} %g\n", r, h.count)
		fmt.Fprintf(&b, "gitopsify_http_request_duration_seconds_sum{route=%q} %g\n", r, h.sum)
		fmt.Fprintf(&b, "gitopsify_http_request_duration_seconds_count{route=%q} %g\n", r, h.count)
	}
	write("gitopsify_runs_total", "Finished runs by type and status.", "counter", m.runs, "type", "status")
	write("gitopsify_runs_active", "Runs queued or working, by type.", "gauge", m.active, "type")
	write("gitopsify_logins_total", "Sign-ins by outcome.", "counter", m.logins, "outcome")
	fmt.Fprintf(&b, "# HELP gitopsify_start_time_seconds When the server started.\n# TYPE gitopsify_start_time_seconds gauge\ngitopsify_start_time_seconds %d\n", m.started.Unix())
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
