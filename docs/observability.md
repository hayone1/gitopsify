# Observability: OpenTelemetry

gitopsify sends OpenTelemetry traces, metrics and logs when the standard
`OTEL_*` environment variables ask for it. This covers `gitopsify server` (and
the Helm chart), `gitopsify ui` and every CLI command. With none of them set,
nothing is started and nothing is sent. The server also serves Prometheus
metrics on `/metrics` ([web UI guide](web-ui.md)).

## Turning it on

Point gitopsify at a collector's OTLP/HTTP endpoint:

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector.observability:4318
gitopsify scan
```

With the Helm chart:

```yaml
opentelemetry:
  endpoint: http://opentelemetry-collector.observability:4318
```

The chart also adds the pod's name and namespace and the Deployment as
resource attributes (`k8s.pod.name`, `k8s.namespace.name`,
`k8s.deployment.name`, `service.instance.id`). Other variables go under
`opentelemetry.env`. Credentials, such as an `Authorization` header in
`OTEL_EXPORTER_OTLP_HEADERS`, belong in `extraEnv`, taken from a Secret.

| Variable | What it does |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | The collector, for every signal (`http://host:4318`) |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, `..._METRICS_ENDPOINT`, `..._LOGS_ENDPOINT` | One signal's own endpoint (full URL, with `/v1/traces` and so on). Setting only one sends only that signal |
| `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER` | `otlp` or `none` |
| `OTEL_EXPORTER_OTLP_HEADERS` | Headers for the collector, such as `Authorization=Bearer ...` |
| `OTEL_EXPORTER_OTLP_CERTIFICATE`, `OTEL_EXPORTER_OTLP_COMPRESSION`, `OTEL_EXPORTER_OTLP_TIMEOUT` | TLS CA, `gzip`, timeout |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | `service.name` (default `gitopsify-server` for the server, `gitopsify` otherwise) and other attributes |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | Sampling, for example `parentbased_traceidratio` and `0.1` |
| `OTEL_METRIC_EXPORT_INTERVAL` | Milliseconds between metric exports (default 60000) |
| `OTEL_SDK_DISABLED` | `true` turns everything off |

gitopsify sends OTLP over HTTP (`http/protobuf`), the specification's default.
Collectors receive it on port 4318. If `OTEL_EXPORTER_OTLP_PROTOCOL` asks for
`grpc` or anything else, gitopsify says so at start and sends nothing.

## What's sent

**Traces**

- **API requests (server):** one span each, named after the route, such as
  `GET /api/v1/runs/{id}`. The span carries `gitopsify.request.id`, the same
  ID as the `X-Request-Id` header and the request's log line. The UI's files,
  the probes and `/metrics` aren't traced. A `traceparent` header continues
  its trace only when it comes from a trusted proxy
  (`spec.trustedProxies`). From anyone else the request starts a new trace,
  linked to theirs.
- **Runs:** a span named `run scan`, `run migrate` or `run export`, a child of
  the request that started the run.
  - Attributes: `gitopsify.run.id`, `gitopsify.run.type`, `gitopsify.cluster`
    or `gitopsify.tool`, and `gitopsify.run.status`.
  - A `started` event marks when the run left the queue.
  - Failed and canceled runs have an error status with the reason.
- **Kubernetes API calls:** a client span each, from both the CLI and the
  server.
- **CLI commands:** one span for the command (for example `gitopsify scan`).
  Its Kubernetes calls are its children.

**Metrics**

- `http.server.request.duration` and the other `http.server.*` metrics, by
  route.
- `gitopsify.runs` (finished, by `gitopsify.run.type` and
  `gitopsify.run.status`).
- `gitopsify.runs.active` (queued or working).
- `gitopsify.logins` (by `gitopsify.login.outcome`).
- Go runtime metrics (`go.memory.used`, `go.goroutine.count`, ...).

**Logs**

- The server's log lines (requests, warnings, errors) as OTLP log records.
  Request lines, and errors from a run, carry their trace and span.
- On stderr, the same lines carry `trace_id` and `span_id`, so you can go
  from a log line to its trace.
- A run's own log stays with the run (in the UI and API), and the audit log
  goes to its own sink ([web UI guide](web-ui.md)).

## Notes

- **What's personal:** request spans carry the client's address (the one
  the audit log records, taken through trusted proxies only) and its user
  agent. Request log records carry the user name and address, as the stderr
  log does. Treat the collector as you treat the logs.
- **What else spans contain:** routes, run IDs, and cluster and tool names.
  The URLs of Kubernetes API calls include namespaces and object names.
  Spans carry no query strings (sign-in codes), request bodies or Secret
  values.
- **Metrics a client can't inflate:** request metrics are kept by route,
  method and status. `server.address` and `server.port` are dropped, because
  they come from the client's `Host` header. A run's live log
  (`GET /api/v1/runs/{id}/events`) is one long request, which stretches the
  duration percentiles of that route.
- **At exit:** a CLI command waits up to five seconds to send what's left,
  less when the collector answers; a second Ctrl-C stops the wait. Shell
  completion and help never start telemetry.
- **Settings gitopsify can't follow** (another exporter or protocol for one
  signal) turn that signal off, with a warning. The other signals still
  start.
- **`gitopsify ui` logs:** its terminal shows warnings only, but its
  request lines still go to the collector.
- **Why it's built in:** the zero-code options for Go are an eBPF agent (OBI)
  and compile-time instrumentation (otelc). OBI needs root or extra Linux
  capabilities next to the pod, which the chart doesn't grant. otelc's spans
  carry no routes, and it doesn't send logs. Built in, gitopsify follows the
  same `OTEL_*` variables, needs no privileges and does nothing until asked.
