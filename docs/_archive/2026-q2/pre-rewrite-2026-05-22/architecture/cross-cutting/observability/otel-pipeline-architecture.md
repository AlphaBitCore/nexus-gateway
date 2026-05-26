---
doc: otel-pipeline-architecture
area: cross-cutting
service: observability
tier: 1
updated: 2026-05-20
---

# OpenTelemetry Pipeline Architecture

> **Tier 2 architecture doc.** Read when touching `packages/shared/core/telemetry/` or any service's OTel setup. The trace_id propagation contract (request_id is Nexus-internal, trace_id is OTel-W3C) lives in `trace-id-propagation-architecture.md`.

Nexus exports distributed traces via OpenTelemetry (OTLP). Spans cover the request path across services; attributes carry the same canonical identifiers (org_id, project_id, virtual_key_id, error_class) that `traffic_event` uses, so dashboards can pivot freely between traces and DB queries.

---

## 1. The pipeline shape

```
service code  →  packages/shared/core/telemetry  →  OTel SDK  →  OTLP exporter  →  collector  →  back-end
```

`shared/core/telemetry` owns the **lifecycle** of the OTel TracerProvider — Init / hot-swap / Shutdown — but does NOT wrap the SDK's surface. Service code uses the OTel SDK directly for span creation, attributes, errors, and propagation:

```go
// init at boot
tp, err := telemetry.Init(ctx, telemetry.Config{
    Enabled:      cfg.OTelEnabled,
    Endpoint:     cfg.OTLPEndpoint,
    ServiceName:  "ai-gateway",
    SamplingRate: cfg.OTelSampleRatio,
}, logger)
// …
defer tp.Shutdown(ctx)

// service code (anywhere)
tracer := otel.GetTracerProvider().Tracer("ai-gateway/proxy")
ctx, span := tracer.Start(ctx, "dispatch")
defer span.End()
span.SetAttributes(attribute.String("provider", "openai"))
if err != nil {
    span.RecordError(err)
    span.SetStatus(codes.Error, err.Error())
}
```

`telemetry.SwappableTracerProvider` exposes one operational verb beyond the SDK: `Reconfigure(cfg)` for hot-swap (atomic pointer + 5-second async shutdown of the old provider). The W3C propagator is also installed by the telemetry package; `httptrace.go` in the same package provides the HTTP-client/server middleware that injects/extracts `traceparent`.

## 2. Service initialisation

At process start, each service calls `telemetry.Init(ctx, Config{Enabled, Endpoint, ServiceName, SamplingRate}, logger)`. This:

1. Builds an SDK TracerProvider (with `otlptracehttp` exporter, OTel resource attrs, ratio sampler).
2. Wraps it in `SwappableTracerProvider` and registers it via `otel.SetTracerProvider`.
3. Installs the W3C trace-context propagator.
4. Returns the swappable provider so the caller can call `Reconfigure` later (e.g., when admin flips the sampling ratio).

`Endpoint` comes from bootstrap config (`service-bootstrap-config-architecture.md`); `Enabled=false` builds a no-op provider so the in-process API still works (useful for tests).

`SamplingRate` is supplied by the caller (each service's bootstrap config — see `service-bootstrap-config-architecture.md`). The current code does NOT auto-bump sampling when admin enables diag mode — diag mode flips slog verbosity + body-capture, but the OTel ratio change is admin-initiated via `Reconfigure`.

## 3. Trace context propagation

Inbound:

- HTTP middleware extracts `traceparent` + `tracestate` from headers; creates a child span under the inbound context.
- MQ consumers extract trace context from envelope `trace_id` field; create a child span.

Outbound:

- The OTel HTTP middleware (`packages/shared/core/telemetry/httptrace.go`) injects `traceparent` on every outbound HTTP call (Nexus → Nexus AND Nexus → provider). Outbound clients are constructed via `packages/shared/transport/http/`.
- MQ producer envelope carries `traceId` on `TrafficEventMessage`.

The result: a single trace spans `client → AI Gateway → upstream provider`, and the same trace also covers `AI Gateway → MQ → Hub audit-sink → Postgres`. Pivoting from trace UI to `traffic_event` row uses `trace_id` as the join key.

## 4. Span attributes today

Spans use **vanilla OTel HTTP semantic attributes only** — `http.method`, `http.url`, `http.status_code`, plus the resource-level `service.name` / `service.version` / `host.name`. No `nexus.*` business-domain attribute namespace is enforced or helper-supported in code. See `otel-span-attributes-architecture.md` for the current state and the deferred work.

Operators who need to pivot from a trace to a `traffic_event` row use the `x-nexus-request-id` header (returned on every response, persisted on `traffic_event.nexus_request_id`) rather than a span attribute lookup.

## 5. Sampling strategy

The current sampler is `sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SamplingRate))` — `packages/shared/core/telemetry/provider.go:159-160`. The edge service samples by ratio; downstream services obey the parent's decision via `traceparent`.

There is no per-request "always-on for errors / always-on for emergency passthrough / always-on across the trust boundary" override implemented today. Ratio-only sampling is intentional in dev where the ratio is set to 1.0; production deployments tune the ratio in their bootstrap config.

## 6. Span lifecycle conventions

- Span name: `<service>.<verb>.<resource>` for top-level (`ai-gateway.dispatch.openai`); `<package>.<function>` for internal helpers.
- One span per logical operation. Don't span-spam every method.
- Use `RecordError` for errors that affect span outcome; use `AddEvent` for points-of-interest within a span.
- Set `Status(codes.Error, msg)` on failure so the trace UI highlights the failed span.

## 7. Cost & cardinality

Span attribute values must be bounded-cardinality:

- ✓ `nexus.error_class = "Rate429"` (enum).
- ✓ `nexus.provider = "openai"` (enum).
- ✗ `nexus.request_payload = "..."` (raw body). Use spillstore for that.
- ✗ `nexus.user_email = "..."`. Use stable IDs.

Cardinality explosion is the #1 way traces become expensive. Stick to enums + IDs.

## 8. Exporter resilience

The OTLP exporter has a bounded in-memory queue:

- Queue full → drop oldest spans (no dedicated drop counter today; observed via the exporter's own logs).
- Collector unreachable → SDK-default exponential backoff.
- Exporter shutdown takes up to 5s to flush (the async-shutdown timeout in `provider.go:125`); service shutdown waits.

Failure to export does NOT block service code. Traces are observability, not correctness.

<!-- 💡 harvest: the canonical `nexus.*` attribute set (§4) is binding for consistency. Could become a shared helper `telemetry.AttachCanonical(span, ctx)` that injects the standard set from RequestContext. Worth surfacing in the shared/telemetry README; not a Cursor rule. -->

## 9. Sources

- `packages/shared/core/telemetry/` — `Init`, `SwappableTracerProvider`, `Reconfigure`, `Shutdown`, plus the HTTP-trace middleware (`httptrace.go`).
- `packages/shared/transport/http/` — outbound HTTP client; participates in OTel propagation via the middleware.
- `packages/shared/transport/mq/` — MQ envelope (`mq.TrafficEventMessage` carries `traceId` so the consumer can re-create a child span).
- `packages/shared/audit/` — `AuditEvent.TraceID` is stamped from the inbound TLS-bumped header for cross-service stitching.

## 10. Cross-references

- `trace-id-propagation-architecture.md` — `request_id` vs `trace_id` semantics.
- `service-bootstrap-config-architecture.md` — `otel_endpoint`, `otel_sample_ratio` bootstrap config.
- `metrics-rollup-architecture.md` — sibling metrics pipeline.
- `audit-pipeline-architecture.md` — `trace_id` cross-reference into audit rows.
- `diag-event-triage-architecture.md` — diag mode is independent of the OTel ratio in current code.
- `otel-span-attributes-architecture.md` — current attribute surface + deferred `nexus.*` namespace work.
