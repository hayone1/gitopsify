package server

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// metrics is a small Prometheus text-format registry: request counts and
// durations, runs started and finished. Enough for dashboards and alerts
// without the client library's dependencies.
type metrics struct {
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
	return &metrics{requests: map[string]float64{}, duration: map[string]*histogram{}, runs: map[string]float64{},
		active: map[string]float64{}, logins: map[string]float64{}, started: time.Now()}
}

func (m *metrics) request(method, route string, code int, d time.Duration) {
	if m == nil {
		return
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
}

func (m *metrics) runFinished(typ, status string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.active[typ]--
	m.runs[typ+","+status]++
	m.mu.Unlock()
}

func (m *metrics) login(outcome string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.logins[outcome]++
	m.mu.Unlock()
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
