---
doc: otel-span-attributes-architecture
area: cross-cutting
service: observability
tier: 1
updated: 2026-05-21
---

# OpenTelemetry Span Attribute Conventions

> **Tier 2 architecture doc.** Read when adding new span attributes or considering whether a piece of context belongs in trace, metric, or audit. Parent: `otel-pipeline-architecture.md` (the OTel pipeline itself).

---

## 1. Current state

Today's spans use **vanilla OTel HTTP semantic attributes only**, via the `packages/shared/core/telemetry/httptrace.go` middleware:

- Resource attrs (set once at provider creation): `service.name`, `service.version`, `host.name`.
- Per-span attrs from the HTTP middleware: `http.method`, `http.url`, `http.status_code`, `http.user_agent` (inbound), `net.peer.name` (outbound).
- Anything else service code attaches via the OTel SDK directly (`span.SetAttributes(...)`) — no centralised helper, no cross-service consistency contract.

A code-wide search finds **zero** `SetAttributes` calls under any `nexus.*` key. Matches for `nexus_*` are slog field keys (a different surface — `slog.String("nexus_request_id", id)`), not span attributes.

## 2. Cross-trace ↔ traffic_event correlation today

Operators pivot from a trace to a `traffic_event` row by the **`x-nexus-request-id` header**, which is:

- Stamped at the Hub edge by `packages/nexus-hub/internal/handler/middleware.go::NexusRequestID` (UUID v4, or reused from the inbound header).
- Returned to the caller on every response.
- Persisted as `traffic_event.nexus_request_id` and `AdminAuditLog.nexusRequestId`.
- Threaded onto downstream outbound calls via `nexushttp.WithRequestID(ctx, id)` (`packages/shared/transport/http/`).

No span attribute is required for this flow — the back-end trace UI filter on the request id (visible in the response header or in the access log) is the entry point.

## 3. Deferred work — a `nexus.*` span attribute namespace

A canonical `nexus.*` namespace (covering org/project/virtual-key/provider/model/routing-rule/hook-outcome/cache-hit/etc.) has been discussed as future work. The intent: make it possible to query the trace back-end directly for "all spans where `nexus.org_id = X AND nexus.error_class = Rate429`" without first going through the DB.

What it would require:

- A shared helper (e.g., `telemetry.AttachCanonical(span, requestcontext.From(ctx))`) so attribute keys stay spelt the same across services.
- A required-per-span-type matrix (top-level `/v1/*` span vs admin API vs MQ consume vs DB query).
- A lint or unit-test gate that fails when a new top-level span omits the required attrs.
- Cardinality discipline (enums + stable IDs only; no PII; no raw bodies).

**None of this is enforced today.** Treat the namespace as roadmap-only. Add tag-namespaced helpers only when (a) a real investigation flow requires direct trace filtering on a business attribute and (b) the engineering plan opens it as a tracked workstream — not opportunistically inside an unrelated PR.

## 4. What stays standard

For HTTP / DB / messaging concerns the rule does not change — use OTel semantic conventions (`http.*`, `db.*`, `messaging.*`, `service.*`). Do not reinvent.

## 5. Cardinality discipline (binding even today)

Anything the codebase does attach to a span via the OTel SDK must respect:

- **Enums** for state (`Rate429`, `LoadBalance`, …).
- **Stable IDs** for entities (`org_id`, `user_id`, request id — billions of unique values are OK; arbitrary strings are not).
- **Bounded text** for protocol fields (HTTP method, status code).

Forbidden as span attributes (regardless of namespace):

- Request bodies / response bodies (use spillstore).
- Email addresses or any PII.
- Free-form user content (use stable IDs).
- Timestamps (time is the trace's X axis).

## 6. Sources

- `packages/shared/core/telemetry/provider.go` — `Init`, `SwappableTracerProvider`, propagator registration.
- `packages/shared/core/telemetry/httptrace.go` — HTTP server/client middleware that contributes the only span attributes auto-attached today.
- `otel-pipeline-architecture.md` — pipeline setup.
- `trace-id-propagation-architecture.md` — `x-nexus-request-id` / OTel `traceparent` propagation.

## 7. Cross-references

- `otel-pipeline-architecture.md` — pipeline + sampling.
- `prometheus-naming-architecture.md` — metric label conventions (cardinality principles apply identically).
- `audit-pipeline-architecture.md` — the DB side of request-id correlation.
- `tenancy-architecture.md` — `org_ancestor_path` materialisation (would be a candidate canonical attribute if/when §3 lands).
