// Package telemetry exports OpenTelemetry traces, metrics and logs when the
// standard OTEL_* environment variables ask for it, for the CLI and the
// server alike. Nothing is started otherwise.
//
// A signal is on when OTEL_<SIGNAL>_EXPORTER is otlp, or when it's unset and
// an OTLP endpoint is (OTEL_EXPORTER_OTLP_ENDPOINT or the signal's own
// OTEL_EXPORTER_OTLP_<SIGNAL>_ENDPOINT); none turns it off, and
// OTEL_SDK_DISABLED=true turns everything off. OTLP goes over HTTP
// (http/protobuf, the specification's default; collectors listen on 4318).
// The exporters, the sampler (OTEL_TRACES_SAMPLER), the batching and the
// resource (OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES) follow the other
// standard variables.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// scope names gitopsify's tracer, meter and logger.
const scope = "github.com/hayone1/gitopsify"

var (
	mu        sync.Mutex
	enabled   atomic.Bool
	shutdowns []func(context.Context) error
	logs      *sdklog.LoggerProvider
	// errLog reports export errors: to stderr, never to the logs signal
	// (a failing log export would feed itself).
	errLog atomic.Pointer[slog.Logger]
)

// Enabled reports whether a signal is exported.
func Enabled() bool { return enabled.Load() }

// Tracer is gitopsify's tracer (a no-op one when traces are off).
func Tracer() trace.Tracer { return otel.Tracer(scope) }

// Meter is gitopsify's meter (a no-op one when metrics are off).
func Meter() metric.Meter { return otel.Meter(scope) }

// signalOn reads whether a signal (traces, metrics or logs) is asked for.
func signalOn(getenv func(string) string, signal string) (bool, error) {
	upper := strings.ToUpper(signal)
	switch e := strings.TrimSpace(getenv("OTEL_" + upper + "_EXPORTER")); e {
	case "none":
		return false, nil
	case "otlp":
	case "":
		if getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && getenv("OTEL_EXPORTER_OTLP_"+upper+"_ENDPOINT") == "" {
			return false, nil
		}
	default:
		return false, fmt.Errorf("OTEL_%s_EXPORTER=%s: gitopsify exports otlp or none", upper, e)
	}
	p := strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_" + upper + "_PROTOCOL"))
	if p == "" {
		p = strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_PROTOCOL"))
	}
	if p != "" && p != "http/protobuf" {
		return false, fmt.Errorf("OTEL_EXPORTER_OTLP_PROTOCOL=%s: gitopsify sends OTLP over HTTP (http/protobuf, usually port 4318)", p)
	}
	return true, nil
}

// Setup starts the signals the environment asks for. service is the
// service.name unless OTEL_SERVICE_NAME or OTEL_RESOURCE_ATTRIBUTES set one.
// A signal whose settings gitopsify can't follow stays off (the error says
// why); the others start.
func Setup(ctx context.Context, service, version string) error {
	mu.Lock()
	defer mu.Unlock()
	if enabled.Load() || strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")), "true") {
		return nil
	}
	var on [3]bool
	var skipped []error
	for i, s := range []string{"traces", "metrics", "logs"} {
		var err error
		if on[i], err = signalOn(os.Getenv, s); err != nil {
			skipped = append(skipped, fmt.Errorf("%s off: %w", s, err))
		}
	}
	if !on[0] && !on[1] && !on[2] {
		return errors.Join(skipped...)
	}
	env, err := resource.New(ctx, resource.WithFromEnv(), resource.WithTelemetrySDK())
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return fmt.Errorf("OTEL_RESOURCE_ATTRIBUTES: %w", err)
	}
	res, err := resource.Merge(resource.NewSchemaless(semconv.ServiceName(service), semconv.ServiceVersion(version)), env)
	if err != nil {
		return err
	}
	var stop []func(context.Context) error
	fail := func(err error) error {
		for _, f := range stop {
			_ = f(ctx)
		}
		return err
	}
	var tp *sdktrace.TracerProvider
	var mp *sdkmetric.MeterProvider
	var lp *sdklog.LoggerProvider
	if on[0] {
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return fail(fmt.Errorf("traces: %w", err))
		}
		tp = sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
		stop = append(stop, tp.Shutdown)
	}
	if on[1] {
		exp, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return fail(fmt.Errorf("metrics: %w", err))
		}
		mp = sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)), sdkmetric.WithResource(res),
			// otelhttp takes server.address and server.port from the
			// request's Host header: clients would pick the series.
			sdkmetric.WithView(sdkmetric.NewView(sdkmetric.Instrument{Name: "http.server.*"},
				sdkmetric.Stream{AttributeFilter: attribute.NewDenyKeysFilter(semconv.ServerAddressKey, semconv.ServerPortKey)})))
		stop = append(stop, mp.Shutdown)
		if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
			return fail(fmt.Errorf("runtime metrics: %w", err))
		}
	}
	if on[2] {
		exp, err := otlploghttp.New(ctx)
		if err != nil {
			return fail(fmt.Errorf("logs: %w", err))
		}
		lp = sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)), sdklog.WithResource(res))
		stop = append(stop, lp.Shutdown)
	}
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { errorLogger().Warn("opentelemetry", "error", err) }))
	// W3C trace context only: baggage from clients would travel on to the
	// Kubernetes API.
	otel.SetTextMapPropagator(propagation.TraceContext{})
	if tp != nil {
		otel.SetTracerProvider(tp)
	}
	if mp != nil {
		otel.SetMeterProvider(mp)
	}
	if lp != nil {
		global.SetLoggerProvider(lp)
	}
	logs, shutdowns = lp, stop
	enabled.Store(true)
	return errors.Join(skipped...)
}

// Shutdown sends what's left and stops the signals; ctx bounds the wait
// (an unreachable collector).
func Shutdown(ctx context.Context) error {
	mu.Lock()
	defer mu.Unlock()
	var errs []error
	for i := len(shutdowns) - 1; i >= 0; i-- {
		errs = append(errs, shutdowns[i](ctx))
	}
	shutdowns, logs = nil, nil
	if enabled.Swap(false) {
		otel.SetTracerProvider(tracenoop.NewTracerProvider())
		otel.SetMeterProvider(metricnoop.NewMeterProvider())
		global.SetLoggerProvider(lognoop.NewLoggerProvider())
	}
	return errors.Join(errs...)
}

func errorLogger() *slog.Logger {
	if l := errLog.Load(); l != nil {
		return l
	}
	return stderrLog
}

var stderrLog = slog.New(slog.NewTextHandler(os.Stderr, nil))

// Logger logs to base, with the trace and span IDs of the context's span,
// and to the logs signal when it's on.
func Logger(base slog.Handler) *slog.Logger {
	errLog.Store(slog.New(base))
	h := slog.Handler(traceIDs{base})
	mu.Lock()
	lp := logs
	mu.Unlock()
	if lp != nil {
		h = slog.NewMultiHandler(h, otelslog.NewHandler(scope, otelslog.WithLoggerProvider(lp)))
	}
	return slog.New(h)
}

// traceIDs adds trace_id and span_id to records logged with a span.
type traceIDs struct{ slog.Handler }

func (h traceIDs) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r = r.Clone()
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceIDs) WithAttrs(as []slog.Attr) slog.Handler { return traceIDs{h.Handler.WithAttrs(as)} }
func (h traceIDs) WithGroup(name string) slog.Handler    { return traceIDs{h.Handler.WithGroup(name)} }

// Transport traces and measures outgoing requests (client spans,
// http.client.* metrics) when telemetry is on.
func Transport(rt http.RoundTripper) http.RoundTripper {
	if !Enabled() {
		return rt
	}
	return otelhttp.NewTransport(rt)
}

// Handler traces and measures the requests whose path starts with prefix
// (server spans, http.server.* metrics), when telemetry is on. Name them
// with Route once the request is routed. A traceparent header continues its
// trace only from a trusted peer (a proxy, trusted may be nil: none);
// others start a trace of their own, linked to it: a client could otherwise
// turn tracing off for its requests, or on for all of them.
func Handler(h http.Handler, prefix string, trusted func(*http.Request) bool) http.Handler {
	if !Enabled() {
		return h
	}
	return otelhttp.NewHandler(h, "http.server",
		otelhttp.WithFilter(func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, prefix) }),
		otelhttp.WithSpanNameFormatter(spanName),
		otelhttp.WithPublicEndpointFn(func(r *http.Request) bool { return trusted == nil || !trusted(r) }))
}

// spanName is "GET /api/v1/runs/{id}" once a ServeMux matched the request
// (otelhttp names the span again then), the method before.
func spanName(_ string, r *http.Request) string {
	if r.Pattern == "" {
		return method(r.Method)
	}
	if _, path, ok := strings.Cut(r.Pattern, " "); ok {
		return method(r.Method) + " " + path
	}
	return method(r.Method) + " " + r.Pattern
}

// method keeps span names bounded: clients pick methods.
func method(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return m
	}
	return "HTTP"
}

// Route names a request's span and metrics after its route
// (GET /api/v1/runs/{id}), never its path, so they stay bounded, and adds
// attributes to the span. An empty route (no API route matched) only adds
// the attributes.
func Route(ctx context.Context, m, route string, attrs ...attribute.KeyValue) {
	span := trace.SpanFromContext(ctx)
	if route == "" {
		span.SetAttributes(attrs...)
		return
	}
	span.SetName(method(m) + " " + route)
	span.SetAttributes(append(attrs, semconv.HTTPRoute(route))...)
	// Metrics may be on with traces off: the labeler doesn't need a span.
	if l, ok := otelhttp.LabelerFromContext(ctx); ok {
		l.Add(semconv.HTTPRoute(route))
	}
}
