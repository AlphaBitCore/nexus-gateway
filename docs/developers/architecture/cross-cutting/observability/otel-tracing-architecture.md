# OTel tracing architecture

Nexus exposes OpenTelemetry tracing as a thin, optional layer: every service can create spans and export them to an external OTLP collector (Tempo, Jaeger, …), but Nexus stores no spans itself. Tracing is off until an operator configures an endpoint, and it can be turned on, retargeted, or resampled at runtime without a restart.

This document covers the tracer provider, its lifecycle and hot-reconfigure path, where spans are actually created, and how trace context propagates. The high-level placement of tracing among the observability surfaces is [observability-architecture.md](observability-architecture.md) §6; the cross-surface correlation strategy that complements tracing — which keys on the request id, not on `trace_id` — is §8.1 of the same doc.

## 1. The hot-swappable provider

`SwappableTracerProvider` (`packages/shared/core/telemetry/provider.go`) wraps an OTel `TracerProvider` so the underlying provider can be replaced at runtime without coordinating with the request path. It satisfies `trace.TracerProvider` (via the embedded interface constraint) and holds the live provider in an `atomic.Pointer`:

- `Tracer(...)` — the hot path, called on every span creation — does a lock-free atomic load and delegates. No mutex.
- `Reconfigure(cfg)` — rare, serialized by a `sync.Mutex` — builds a fresh provider, atomically swaps it in, and hands the displaced provider to a background goroutine that shuts it down with a five-second timeout. In-flight spans on the old provider drain while new spans go to the new one.
- `Shutdown(ctx)` — flushes and stops the current provider on process exit; a no-op when the current provider is the no-op provider.

`Config` carries four fields: `Enabled`, `Endpoint` (OTLP HTTP), `ServiceName`, and `SamplingRate` (0.0–1.0).

`newProvider` decides between two builds:

- **Disabled / no endpoint** — when `Enabled` is false or `Endpoint` is empty, it returns a `noop.NewTracerProvider()`. No exporter, no batch processor, no goroutines. This is the default state.
- **Enabled** — it builds an OTLP HTTP exporter (`otlptracehttp.New` with `WithEndpoint` and `WithInsecure`), a resource carrying `service.name`, and a `ParentBased(TraceIDRatioBased(SamplingRate))` sampler, then assembles an SDK `TracerProvider` with a batching span processor (`WithBatcher`). Batching means span export never blocks the request that created the span.

`Init` builds the first provider, stores it, registers it as the global provider via `otel.SetTracerProvider`, and registers the global propagator (§5).

## 2. Lifecycle across services

Every service initializes the provider at boot through `telemetry.Init`:

- Nexus Hub — `packages/nexus-hub/cmd/nexus-hub/wiring/observability.go`
- AI Gateway — `packages/ai-gateway/cmd/ai-gateway/wiring/boot.go`
- Compliance Proxy — `packages/compliance-proxy/cmd/compliance-proxy/main.go`
- Control Plane — `packages/control-plane/cmd/control-plane/wiring/observability.go`
- Agent — `packages/agent/cmd/agent/wiring/telemetry.go`, through a thin wrapper package (`packages/agent/internal/observability/telemetry/`) that aliases the shared `Config` / `Provider` types and delegates `Init` to the shared package.

**Config source.** Each service composes its `Config` from file configuration (the `Otel.Endpoint` / `Otel.ServiceName` fields) plus the `observability.config` row in `system_metadata`, which supplies `otelEnabled` and `samplingRate`. The AI Gateway's `InitOtelConfig` and the Compliance Proxy's `LoadOtelConfig` are the per-service composition helpers; each defaults `ServiceName` to its own identity (`nexus-ai-gateway`, `nexus-compliance-proxy`, …).

**Hot reconfigure.** Four services re-derive the config and call `Reconfigure` when observability settings change, so toggling tracing, changing the endpoint, or adjusting the sampling rate takes effect without a restart:

- AI Gateway, Compliance Proxy, and Control Plane register an `observability` config handler in their `configdispatch` package that rebuilds the `Config` and calls `TelemetryProvider.Reconfigure`.
- The Hub applies the same change through its self-shadow handler (`packages/nexus-hub/cmd/nexus-hub/wiring/self.go`), merging the pushed `Enabled` / `Endpoint` / `ServiceName` / `SamplingRate` fields before reconfiguring.

The Agent initializes tracing at boot only; it has no `Reconfigure` path.

## 3. Where spans are created

Provider initialization is fleet-wide, but actual span emission is deliberately sparse — Nexus leans on the request id (§5) for most cross-surface stitching rather than dense manual instrumentation.

**HTTP server spans — AI Gateway.** `HTTPTrace(serviceName)` (`packages/shared/core/telemetry/httptrace.go`) is the only server-span middleware, and it is mounted only on the AI Gateway request handler (`packages/ai-gateway/cmd/ai-gateway/wiring/routes.go`). For each request it opens a `SpanKindServer` span named `"<METHOD> <PATH>"`, stamps `http.request.method`, `url.path`, and `server.address` semantic-convention attributes, and on completion records `http.response.status_code` plus an `error=true` attribute when the status is ≥ 400.

The middleware wraps the `ResponseWriter` in a `statusWriter` that captures the status code and — critically — forwards `Flush()` and exposes `Unwrap()`. Go does not promote methods that an embedded interface only satisfies dynamically, so without an explicit `Flush()` on the wrapper, a streaming handler doing `w.(http.Flusher)` behind this middleware would see no flusher and silently degrade to a buffered, `Content-Length` response — breaking Server-Sent Events for every streaming consumer. `Unwrap()` keeps `http.ResponseController` capabilities (write deadlines, hijack) reachable through the chain. Compile-time assertions hold the wrapper to the `http.Flusher` and `http.ResponseWriter` contracts.

**A manual span — Agent.** The Agent opens one explicit span, `audit.upload_batch` (`otel.Tracer("nexus-agent")` in `packages/agent/cmd/agent/cmd_run.go`), around its batched audit upload.

The Hub, Control Plane, and Compliance Proxy initialize the provider (so `otel.Tracer` and propagation work) but mount no server-span middleware of their own.

## 4. Client-side context injection

The Agent wraps its outbound Hub HTTP transport with `otelhttp.NewTransport` (`packages/agent/internal/sync/hub/client.go`), so calls to the Hub carry W3C `traceparent` headers injected through the global propagator. The inner transport continues to handle per-host pooling and HTTP/2 multiplexing.

## 5. Propagation and correlation

Two identifiers tie exported spans to the DB-side records, and which one applies depends on whether the caller runs tracing of its own.

**When the caller sends no `traceparent`**, the request id does the work: it is a UUID, and a UUID is exactly the sixteen bytes of an OTel trace ID, so the `IDGenerator` derives the root span's trace ID from it and the same value serves both the collector and the `traffic_event.external_request_id` / `thing_diag_event` / SIEM rows — the only difference is the rendered form (a hyphenated UUID versus thirty-two hex characters).

**When the caller does send `traceparent`**, the propagator continues the caller's trace instead, so the span's trace ID is the caller's, not one derived from the request id. That value is also persisted, to `traffic_event.trace_id`, which is what lets a span in the caller's own APM lead to the gateway's row for the same call. The column is written **only** on this path: with no inbound `traceparent` it stays NULL rather than receiving the derived id, because a column meaning "the caller's trace" must be empty when the caller has no trace. The request id continues to correlate the DB-side surfaces either way.

**One canonical request id, honored or minted at each surface.** Every entry point either honors an inbound `X-Nexus-Request-Id` or mints a fresh UUID:

- Bumped agent and Compliance Proxy traffic shares one path — the per-request forward handler in `packages/shared/transport/tlsbump/forward_handler.go` honors the inbound header, mints a UUID when absent, re-stamps it on the upstream request, and uses it as the audit trace id. Only intercepted (bumped) flows reach this handler.
- The AI Gateway's `RequestID` middleware (`packages/ai-gateway/internal/platform/middleware/middleware.go`) honors the inbound header, falls back to its `X-Request-Id` compatibility alias, and mints a UUID when neither arrived.
- The Hub and Control Plane Echo middlewares (for example `packages/control-plane/internal/platform/middleware/requestid.go`) honor the inbound header or mint a UUID.
- The Compliance Proxy also mints a UUID at the CONNECT edge (`packages/compliance-proxy/internal/proxy/server/server.go`) when no upstream id is present, so the connection-stage pipeline carries a correlation id.

The audit emitter carries this id to `traffic_event.external_request_id`, and request-scoped loggers stamp it so diag events pick it up into `thing_diag_event.external_request_id`; both columns are indexed on it, and it is one of the ids carried on SIEM payloads (see [observability-architecture.md](observability-architecture.md) §8.1).

**W3C propagation + trace-id derivation.** `Init` registers a global composite propagator of `propagation.TraceContext{}` and `propagation.Baggage{}` via `otel.SetTextMapPropagator`, so the `HTTPTrace` middleware's `Extract` continues an incoming `traceparent` rather than starting a fresh root, and the Agent's `otelhttp` transport injects `traceparent` on outbound calls. For root spans, the provider's custom `IDGenerator` (`packages/shared/core/telemetry/idgen.go`) derives the trace ID directly from the context's `X-Nexus-Request-Id` when it is a UUID — a direct sixteen-byte copy — so the exported span and the DB rows carry the same value. A request id that is not a UUID (the agent's intercepted-flow ids, for instance) falls back to a random trace ID. For the derivation to apply, the request-ID middleware must populate the context before the span is created: the AI Gateway wires `RequestID` to wrap outside `HTTPTrace` for exactly this reason.

**Where the caller's trace id is captured.** `HTTPTrace` records it between `Extract` and `tracer.Start`, and that placement is load-bearing in both directions. Before `Extract` there is nothing to read. After `Start` the current span context is the server's own span, whose `IsRemote` is always false and whose trace ID — absent an inbound `traceparent` — is the one the `IDGenerator` derived from the request id; a capture there would persist a Nexus-minted value into the column that means "the caller's trace". Between the two, the span context is remote and valid exactly when the inbound header parsed, so a malformed `traceparent` drops out without a special case. The captured value rides the request context and the audit path reads it; nothing else may write that column.

## 6. Operational invariants

- Tracing is off by default; a service with no endpoint configured runs the no-op provider — zero exporter, zero background goroutines.
- Enabling, retargeting, or resampling tracing is a runtime config change on the Hub, AI Gateway, Compliance Proxy, and Control Plane; the Agent picks up tracing config only at boot.
- Span export is always batched, so the exporter never blocks a request, and provider swaps drain the old provider in the background under a bounded timeout.
- The OTLP exporter connects with `WithInsecure`; the collector endpoint is expected to be reachable on a trusted network path.
- The `statusWriter` wrapper must keep forwarding `Flush()` / `Unwrap()` — dropping either silently breaks SSE streaming through the traced handler.

## References

- `packages/shared/core/telemetry/provider.go` — `SwappableTracerProvider`, `Config`, `Init` / `Reconfigure` / `Shutdown`, propagator registration
- `packages/shared/core/telemetry/httptrace.go` — `HTTPTrace` server-span middleware + `statusWriter`
- `packages/shared/core/telemetry/idgen.go` — request-id → trace-id `IDGenerator`
- `packages/shared/transport/tlsbump/forward_handler.go` — per-request `X-Nexus-Request-Id` honor/mint for bumped agent + Compliance Proxy traffic
- `packages/agent/internal/observability/telemetry/` — Agent wrapper that delegates to the shared provider
- `packages/agent/internal/sync/hub/client.go` — outbound `otelhttp` transport
- `packages/agent/cmd/agent/cmd_run.go` — `audit.upload_batch` manual span
- `packages/ai-gateway/cmd/ai-gateway/wiring/routes.go` — `HTTPTrace` mount
- `packages/ai-gateway/cmd/ai-gateway/wiring/observability.go` — `InitOtelConfig` config composition
- `packages/compliance-proxy/cmd/compliance-proxy/wiring/otel.go` — `LoadOtelConfig`
- `packages/nexus-hub/cmd/nexus-hub/wiring/self.go` — Hub self-shadow reconfigure
- `packages/control-plane/internal/platform/middleware/requestid.go` — Control Plane `X-Nexus-Request-Id` middleware (honors inbound, else mints)
- `packages/ai-gateway/internal/platform/middleware/middleware.go` — AI Gateway `RequestID` middleware (honors inbound, else mints)
- `packages/compliance-proxy/internal/proxy/server/server.go` — Compliance Proxy CONNECT-edge request-id honor/mint
