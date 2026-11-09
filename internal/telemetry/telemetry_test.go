package telemetry_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/hayone1/gitopsify/internal/telemetry"
	"github.com/hayone1/gitopsify/internal/telemetry/otlptest"
)

// clearEnv empties the machine's OTEL_* variables and stops telemetry at
// the end of the test.
func clearEnv(t *testing.T) {
	otlptest.ClearEnv(t)
	t.Cleanup(func() { _ = telemetry.Shutdown(context.Background()) })
}

func TestOffByDefault(t *testing.T) {
	clearEnv(t)
	if err := telemetry.Setup(context.Background(), "gitopsify", "dev"); err != nil || telemetry.Enabled() {
		t.Fatal(err, telemetry.Enabled())
	}
	// Disabled wins over an endpoint.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	t.Setenv("OTEL_SDK_DISABLED", "true")
	if err := telemetry.Setup(context.Background(), "gitopsify", "dev"); err != nil || telemetry.Enabled() {
		t.Fatal(err, telemetry.Enabled())
	}
	// Every exporter none: nothing to start.
	t.Setenv("OTEL_SDK_DISABLED", "")
	for _, s := range []string{"TRACES", "METRICS", "LOGS"} {
		t.Setenv("OTEL_"+s+"_EXPORTER", "none")
	}
	if err := telemetry.Setup(context.Background(), "gitopsify", "dev"); err != nil || telemetry.Enabled() {
		t.Fatal(err, telemetry.Enabled())
	}
	mux := http.NewServeMux()
	if telemetry.Handler(mux, "/", nil) != http.Handler(mux) || telemetry.Transport(http.DefaultTransport) != http.DefaultTransport {
		t.Error("off, the handler and the transport should be left alone")
	}
}

// Settings gitopsify can't follow turn their signal off and say why; the
// other signals start.
func TestConfigErrors(t *testing.T) {
	for _, c := range []struct {
		env, want string
		enabled   bool
	}{
		{"OTEL_EXPORTER_OTLP_PROTOCOL=grpc", "http/protobuf", false},
		{"OTEL_TRACES_EXPORTER=zipkin", "traces off: OTEL_TRACES_EXPORTER=zipkin", true},
		{"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL=http/json", "metrics off", true},
	} {
		t.Run(c.env, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
			k, v, _ := strings.Cut(c.env, "=")
			t.Setenv(k, v)
			err := telemetry.Setup(context.Background(), "gitopsify", "dev")
			if err == nil || !strings.Contains(err.Error(), c.want) || telemetry.Enabled() != c.enabled {
				t.Errorf("%v (enabled %v)", err, telemetry.Enabled())
			}
		})
	}
}

func TestExport(t *testing.T) {
	clearEnv(t)
	rcv := otlptest.New(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", rcv.URL)
	ctx := context.Background()
	if err := telemetry.Setup(ctx, "gitopsify", "dev"); err != nil || !telemetry.Enabled() {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = telemetry.Shutdown(context.Background()) })

	// A routed API request and an outgoing call.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer backend.Close()
	var buf bytes.Buffer
	log := telemetry.Logger(slog.NewJSONHandler(&buf, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		telemetry.Route(r.Context(), r.Method, "/api/v1/runs/{id}")
		log.InfoContext(r.Context(), "looking up a run")
		out, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, backend.URL, nil)
		resp, err := (&http.Client{Transport: telemetry.Transport(http.DefaultTransport)}).Do(out)
		if err == nil {
			_ = resp.Body.Close()
		}
	})
	srv := httptest.NewServer(telemetry.Handler(mux, "/api/", nil))
	defer srv.Close()
	// The client picks the Host header, the query and the trace header.
	const remoteTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	for i, p := range []string{"/api/v1/runs/20261109-x?token=hunter2", "/api/v1/runs/20261109-y", "/assets/app.js"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+p, nil)
		req.Host = fmt.Sprintf("client-picked-%d:%d", i, 1000+i)
		req.Header.Set("traceparent", "00-"+remoteTrace+"-00f067aa0ba902b7-00")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if err := telemetry.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	server := rcv.Span("GET /api/v1/runs/{id}")
	if server == nil {
		var names []string
		for _, s := range rcv.Spans() {
			names = append(names, s.Name+" "+s.Kind.String()+" "+otlptest.Attr(s, "url.path")+" "+otlptest.Attr(s, "http.route"))
		}
		t.Fatalf("no span for the route: %q", names)
	}
	if otlptest.Attr(server, "http.route") != "/api/v1/runs/{id}" {
		t.Errorf("http.route: %v", server.Attributes)
	}
	var client *tracepb.Span
	for _, s := range rcv.Spans() {
		if s.Kind == tracepb.Span_SPAN_KIND_CLIENT && bytes.Equal(s.ParentSpanId, server.SpanId) {
			client = s
		}
	}
	if client == nil {
		t.Error("the outgoing call isn't a child of the request")
	}
	for _, s := range rcv.Spans() {
		if strings.Contains(s.Name, "assets") || otlptest.Attr(s, "url.path") == "/assets/app.js" {
			t.Errorf("a request outside the prefix was traced: %s", s.Name)
		}
		if strings.Contains(s.String(), "hunter2") {
			t.Errorf("a span has the query string: %v", s)
		}
	}
	// A client's trace header doesn't make the span part of its trace (it
	// asked not to sample): the span starts a trace, linked to it.
	if hex.EncodeToString(server.TraceId) == remoteTrace || len(server.Links) != 1 ||
		hex.EncodeToString(server.Links[0].TraceId) != remoteTrace {
		t.Errorf("an untrusted traceparent was followed: trace %x, links %v", server.TraceId, server.Links)
	}
	if keys := rcv.MetricKeys("http.server.request.duration"); keys["server.address"] || keys["server.port"] || !keys["http.route"] {
		t.Errorf("request metrics by %v: series the client picks, or no route", keys)
	}
	if !rcv.Metric("http.server.request.duration") || !rcv.Metric("go.goroutine.count") {
		t.Error("missing HTTP or runtime metrics")
	}
	if logs := rcv.Logs(); len(logs) != 2 || logs[0] != "looking up a run" {
		t.Errorf("logs: %v", logs)
	}
	// The local log line carries the trace, for correlation.
	if !strings.Contains(buf.String(), `"trace_id":"`) {
		t.Errorf("local log: %s", buf.String())
	}
	if telemetry.Enabled() {
		t.Error("still enabled after Shutdown")
	}
}
