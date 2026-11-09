// Package otlptest is an OTLP/HTTP receiver for tests: it records the spans,
// metric names and log records exporters send it.
package otlptest

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Receiver records what it receives; use its URL as OTEL_EXPORTER_OTLP_ENDPOINT.
type Receiver struct {
	URL string

	mu      sync.Mutex
	spans   []*tracepb.Span
	metrics map[string]map[string]bool // name: attribute keys of its points
	logs    []string
}

// ClearEnv empties the OTEL_* variables of the machine running the tests.
func ClearEnv(t testing.TB) {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "OTEL_") {
			t.Setenv(k, "")
		}
	}
}

// New starts a receiver, stopped at the end of the test.
func New(t testing.TB) *Receiver {
	r := &Receiver{metrics: map[string]map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(srv.Close)
	r.URL = srv.URL
	return r
}

func (r *Receiver) serve(w http.ResponseWriter, req *http.Request) {
	var body io.Reader = req.Body
	if req.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body = zr
	}
	data, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch req.URL.Path {
	case "/v1/traces":
		var m coltrace.ExportTraceServiceRequest
		if err = proto.Unmarshal(data, &m); err == nil {
			for _, rs := range m.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					r.spans = append(r.spans, ss.Spans...)
				}
			}
		}
	case "/v1/metrics":
		var m colmetrics.ExportMetricsServiceRequest
		if err = proto.Unmarshal(data, &m); err == nil {
			for _, rm := range m.ResourceMetrics {
				for _, sm := range rm.ScopeMetrics {
					for _, x := range sm.Metrics {
						keys := r.metrics[x.Name]
						if keys == nil {
							keys = map[string]bool{}
							r.metrics[x.Name] = keys
						}
						for _, p := range x.GetHistogram().GetDataPoints() {
							for _, a := range p.Attributes {
								keys[a.Key] = true
							}
						}
						for _, p := range x.GetSum().GetDataPoints() {
							for _, a := range p.Attributes {
								keys[a.Key] = true
							}
						}
					}
				}
			}
		}
	case "/v1/logs":
		var m collogs.ExportLogsServiceRequest
		if err = proto.Unmarshal(data, &m); err == nil {
			for _, rl := range m.ResourceLogs {
				for _, sl := range rl.ScopeLogs {
					for _, l := range sl.LogRecords {
						r.logs = append(r.logs, l.GetBody().GetStringValue())
					}
				}
			}
		}
	default:
		http.NotFound(w, req)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
}

// Spans returns the spans received so far.
func (r *Receiver) Spans() []*tracepb.Span {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*tracepb.Span(nil), r.spans...)
}

// Span returns the first span with this name, or nil.
func (r *Receiver) Span(name string) *tracepb.Span {
	for _, s := range r.Spans() {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// Metric reports whether a metric with this name was received.
func (r *Receiver) Metric(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.metrics[name] != nil
}

// MetricKeys returns the attribute keys of a metric's points (histograms
// and sums).
func (r *Receiver) MetricKeys(name string) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]bool{}
	for k := range r.metrics[name] {
		out[k] = true
	}
	return out
}

// Logs returns the bodies of the log records received so far.
func (r *Receiver) Logs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.logs...)
}

// Attr returns a span's string attribute.
func Attr(s *tracepb.Span, key string) string {
	for _, a := range s.Attributes {
		if a.Key == key {
			return a.Value.GetStringValue()
		}
	}
	return ""
}
