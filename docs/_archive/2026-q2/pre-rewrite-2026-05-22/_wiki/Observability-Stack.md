# Observability Stack

*Audience: operators configuring monitoring and contributors adding new instrumentation.*

Nexus Gateway's observability stack has three layers: Prometheus metrics for aggregated signals, OpenTelemetry distributed tracing for request lifecycle spans, and runtime introspection for querying the live in-memory config state of any running service. A fourth cross-cutting concern is trace-ID propagation — two identifiers (`request_id` for Nexus-internal correlation and `trace_id` for OTel-W3C tracing) flow end-to-end and land in every audit row, traffic event, and log line.

---

## Prometheus metrics

Every Nexus service emits Prometheus metrics using the `promauto` package via `packages/shared/core/metrics/`. The naming convention is:

```
nexus_<subsystem>_<measurement>[_<unit>]
```

| Part | Values | Notes |
|---|---|---|
| `nexus` | always `nexus` | Constant namespace |
| `<subsystem>` | `gateway`, `hub`, `compliance_proxy`, `agent`, `cp` | The emitting service |
| `<measurement>` | `requests_total`, `hook_latency_seconds`, `cache_hit_ratio` | What is measured |
| `<unit>` | `_seconds`, `_bytes`, `_total` (counters), `_ratio` | SI units; `_total` on counters only |

Examples from the actual metric surface:

```
nexus_gateway_requests_total
nexus_gateway_request_duration_seconds
nexus_hub_audit_queue_lag_seconds
nexus_compliance_proxy_cert_cache_hits_total
nexus_agent_audit_queue_depth
```

Rules:
- Counters end in `_total`. Histograms and gauges do not.
- Use `_seconds` for durations (never `_ms`), `_bytes` for sizes (never `_kb`).
- Labels use lowercase, underscore-separated names with bounded cardinality. PII in labels is forbidden — `org_id` (stable ID) is fine; `org_email` is not.
- Each metric carries a required `Help` string describing what it measures and the label cardinality.

Cache hit/miss counters use a `{cache="..."}` label: `nexus_cache_hits_total{cache="iam_policy"}`. An alert fires when any cache drops below 80% hit rate.

Org-level cardinality (`provider × model × error_class × org_id`) easily exceeds 10,000 series for a large tenant. The architectural pattern: emit without `org_id` to Prometheus (low cardinality) and push high-cardinality details to Postgres `traffic_event` (where SQL is the query tool).

## OpenTelemetry tracing

Nexus exports distributed traces via OTLP. The pipeline:

```
service code → packages/shared/core/telemetry → OTel SDK → OTLP exporter → collector → backend
```

`shared/core/telemetry` manages the TracerProvider lifecycle (Init / hot-swap / Shutdown) but does not wrap the SDK surface — service code uses the OTel SDK directly for span creation, attributes, and propagation. The `SwappableTracerProvider` exposes one extra verb: `Reconfigure(cfg)` for atomic hot-swap of the sampling ratio without a service restart.

Sampling hierarchy (evaluated top-down):
1. **Always-on**: every trust-boundary crossing (`admin API`, `/v1/*` ingress), every passthrough request, every failed request.
2. **High-rate (100%)**: alert-related traffic during incident windows.
3. **Default ratio**: 10% in production, 100% in dev.

The sampling decision is taken at the edge and propagated via `traceparent` — downstream services obey the parent's decision.

Spans use vanilla OTel HTTP semantic attributes (`http.method`, `http.url`, `http.status_code`) plus resource-level `service.name`, `service.version`, `host.name`. There is no `nexus.*` business-domain attribute namespace enforced in current code. The `x-nexus-request-id` header (returned on every response and stored in `traffic_event.nexus_request_id`) is the primary cross-service join key — not a span attribute lookup.

## Trace-ID and request-ID propagation

Two identifiers travel end-to-end:

| ID | Format | Header | Created by | Used for |
|---|---|---|---|---|
| `request_id` | UUID v4 | `x-nexus-request-id` | First Nexus service that handles the request | Analytics joins, `traffic_event` correlation, admin audit lookup |
| `trace_id` | W3C 16-byte | `traceparent` | Same edge; or extracted from inbound header | OTel tracing, span correlation, trace-backend queries |

The middleware reuses an inbound `x-nexus-request-id` header if present — this lets clients pre-correlate and lets internal service-to-service calls inherit the upstream ID without minting a new one per hop.

`request_id` is forwarded to other Nexus services but NOT to upstream providers (providers don't know what to do with it). `traceparent` IS forwarded to providers (harmless if they ignore it; some honour it).

Agent ↔ server stitching: the agent generates its own `request_id` and `trace_id` at the edge (the agent is the edge for endpoint-intercepted traffic). On audit upload, both IDs are emitted. The unified timeline in the CP UI joins on `nexus_request_id` / `nexusRequestId` to show agent and server-side rows for the same request.

MQ messages carry both IDs in the envelope: `traceId` and `nexusRequestId` on `TrafficEventMessage`. This allows Hub's audit consumer to reconstruct a child OTel span and write both IDs to `traffic_event`.

## Runtime introspection

The runtime introspection endpoint (`/runtime/config`) lets operators query the live in-memory config state of any running service without redeployment. This answers "what is the effective hook config right now?" without waiting for logs to surface a mis-apply.

Endpoints per service:

| Endpoint | Returns |
|---|---|
| `GET /runtime/config` | Full effective config snapshot (sensitive fields redacted) |
| `GET /runtime/config/{key}` | Single config key value (redacted if sensitive) |
| `GET /runtime/sync-status` | Per-key sync state: last-applied generation, drift, error |
| `GET /runtime/health` | Readiness: DB up, Hub WS connected, shadow reconciled |

Auth: bearer token, disabled by default (`HandlerOptions.Token` empty → `503`). An unset token means introspection is off in production until an operator explicitly provisions one. Sensitive fields (API keys, tokens, private keys) are always redacted — `[redacted]` in the response — using the same list as the audit pipeline redaction rules.

Two access patterns:
- **Operator on-host**: SSH, set `RUNTIME_INTROSPECT_TOKEN`, `curl http://localhost:3050/runtime/config`.
- **Admin via CP UI**: CP fetches via Hub mTLS → Thing runtime API → returns to admin browser. Visible at CP UI → Infrastructure → Nodes → select node → "Runtime Introspection" tab.

pprof is intentionally not wired to any production binary — profiling requires a dev-environment mirror with explicit per-PR approval.

---

## Canonical docs

- [`prometheus-naming-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/observability/prometheus-naming-architecture.md) — naming convention, label cardinality budget, registration pattern
- [`otel-pipeline-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/observability/otel-pipeline-architecture.md) — OTel pipeline, sampling, span lifecycle
- [`trace-id-propagation-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/observability/trace-id-propagation-architecture.md) — `request_id` vs `trace_id`, MQ envelope, audit storage
- [`runtime-introspection-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/observability/runtime-introspection-architecture.md) — introspection endpoints, auth, redaction

**Adjacent wiki pages**: [Hub Coordination](Hub-Coordination) · [Storage Cache MQ Stack](Storage-Cache-MQ-Stack) · [Operations Logs Metrics Traces](Operations-Logs-Metrics-Traces) · [Spillstore](Spillstore) · [Control Plane Audit Log](Control-Plane-Audit-Log)
