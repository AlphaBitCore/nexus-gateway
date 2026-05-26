# Observability architecture

Umbrella for everything under `docs/developers/architecture/cross-cutting/observability/`. The Nexus Gateway runs five distinct observability surfaces, each with its own producer, transport, persistence, retention, and admin surface. They are deliberately separate — they answer different operator questions, decay on different timescales, and are queried by different consumers (admin UI, alert engine, external SIEM, ad-hoc operator debugging) — but they share one correlation key (`trace_id`) and one set of source services.

This document is the entry point. Each surface gets its own detail doc; this page only states the shape of the system, the wire vocabulary, and the cross-surface invariants.

## 1. The five surfaces

| # | Surface | Answers "what happened…" | Producer | Transport | Persistence |
| - | --- | --- | --- | --- | --- |
| 1 | Audit | …for every request that crossed an enforcement surface (AI Gateway, Compliance Proxy, Agent) and every admin mutation | All three data-plane services + Control Plane | NATS JetStream | `traffic_event` (+ sidecars) + `AdminAuditLog` |
| 2 | Diag | …went wrong inside a service or agent (warn / error / fatal / crash / watchdog / lifecycle) | Any Thing via `slog` | WebSocket `diag_event` + HTTP drain | `thing_diag_event` |
| 3 | Metrics | …is happening right now, in aggregate, with low cardinality | Every Thing (services + agents) | Prometheus `/metrics` scrape **and** WebSocket `metrics_sample` push | `/metrics` (in-process) + `metric_ops_*` + `metric_rollup_*` + `thing_metric_rollup_*` |
| 4 | Traces | …happened to one specific request, span-by-span, across services | Any service via OTel SDK | OTLP HTTP → external collector | External (Tempo / Jaeger / vendor) — Nexus does not persist spans |
| 5 | SIEM | …needs to flow to a customer-owned external security system for retention and correlation | Hub-side polling forwarder reads every audit row regardless of source — server traffic, compliance-proxy traffic, and admin audit events all funnel through the same bridge | Pluggable Sink (HTTP webhook / Splunk HEC / Datadog / file / NDJSON spool) | External SIEM (no Nexus persistence) |

Detail docs:

- Audit pipeline — [audit-pipeline-architecture.md](audit-pipeline-architecture.md)
- Admin audit log coverage — [admin-audit-log-coverage.md](admin-audit-log-coverage.md)
- Diag event triage — [diag-event-triage-architecture.md](diag-event-triage-architecture.md)
- Metrics rollup — [metrics-rollup-architecture.md](metrics-rollup-architecture.md)
- Prometheus naming — [prometheus-naming-architecture.md](prometheus-naming-architecture.md)
- OTel pipeline — [otel-pipeline-architecture.md](otel-pipeline-architecture.md)
- OTel span attributes — [otel-span-attributes-architecture.md](otel-span-attributes-architecture.md)
- Trace-ID propagation — [trace-id-propagation-architecture.md](trace-id-propagation-architecture.md)
- SIEM bridge — [siem-bridge-architecture.md](siem-bridge-architecture.md)
- Alerting — [alerting-architecture.md](alerting-architecture.md)
- Runtime introspection — [runtime-introspection-architecture.md](runtime-introspection-architecture.md)
- Test harness — [test-harness-architecture.md](test-harness-architecture.md)

## 2. Anchor packages

Each surface lands in a small set of Go packages; all five surfaces share the same shared-libraries spine.

| Surface | AI Gateway | Compliance Proxy | Agent | Nexus Hub | Shared libraries |
| --- | --- | --- | --- | --- | --- |
| Audit | `packages/ai-gateway/internal/platform/audit/` | `packages/compliance-proxy/internal/audit/` | (forwarder; emits `TrafficEventMessage`) | `packages/nexus-hub/internal/observability/consumer/` (`traffic.go`, `admin_audit.go`, DLQ in `traffic.go`) + `packages/nexus-hub/internal/traffic/chain/` | `packages/shared/transport/mq/` (`messages.go`, `streams.go`) |
| Diag | `slog` + `diag.SlogSink` | same | same | `packages/nexus-hub/internal/observability/opsmetrics/` + `packages/nexus-hub/internal/observability/handler/diag/` | `packages/shared/core/diag/` + `packages/shared/core/metrics/registry/` (DiagEvent type) |
| Metrics | `packages/ai-gateway/internal/platform/metrics/` | (in-service `Recorder`) | (in-service `Recorder`) | `packages/nexus-hub/internal/jobs/defs/rollup/` + `packages/nexus-hub/internal/jobs/defs/metrics/` | `packages/shared/core/metrics/registry/`, `instruments/`, `platform/` |
| Traces | OTel SDK | OTel SDK | OTel SDK | OTel SDK | `packages/shared/core/telemetry/` |
| SIEM | (no direct sink — traffic flows to Hub first) | (no direct sink — traffic flows to Hub via MQ) | n/a | `packages/nexus-hub/internal/observability/siem/` + admin handler at `packages/control-plane/internal/observability/siem/handler/` | n/a |

The MQ envelopes (`TrafficEventMessage`, `AdminAuditMessage`) live in `packages/shared/transport/mq/messages.go`; the WS envelopes (`metrics_sample`, `diag_event`) are dispatched through `packages/shared/transport/thingclient/`.

## 3. Audit pipeline (high level)

The audit pipeline is the system's record-of-truth for business traffic and admin mutations. Two wire envelopes, two persistence tables, one consumer.

**Traffic audit.** Every enforcement surface (`source = "ai-gateway" | "compliance-proxy" | "agent"`) builds a `TrafficEventMessage` (`packages/shared/transport/mq/messages.go`) and publishes it to one of three NATS subjects:

| Source | NATS subject |
| --- | --- |
| `ai-gateway` | `nexus.event.ai-traffic` |
| `compliance-proxy` | `nexus.event.compliance` |
| `agent` | `nexus.event.agent` |

All three subjects bind to the `NEXUS_EVENTS` JetStream stream (`packages/shared/transport/mq/streams.go` — subject pattern `nexus.event.>`). The Hub's `TrafficEventWriter` consumer (`packages/nexus-hub/internal/observability/consumer/traffic.go`) reads all three queues under the consumer group `hub-db-writer` and batch-inserts into:

- `traffic_event` — one row per request, ~90 columns covering identity, model, tokens, cost, cache, hooks, normalize, latency phases, attestation, passthrough flags, error classification.
- `traffic_event_payload` — 1:1 sidecar carrying captured raw request/response bytes (inline OR spillstore reference).
- `traffic_event_normalized` — 1:1 sidecar carrying the canonical normalized representation produced by `shared/transport/normalize`.

The AI Gateway producer batches in-memory (default `defaultBatchSize=100`, `defaultFlushInterval=5s`, `maxQueueSize=10000` — see `packages/ai-gateway/internal/platform/audit/audit.go`) and stamps `Source: "ai-gateway"` on every message; the compliance-proxy producer (`packages/compliance-proxy/internal/audit/event_message.go`) stamps `"compliance-proxy"`; the agent stamps `"agent"`.

**Admin audit.** The Control Plane publishes `AdminAuditMessage` (`packages/shared/transport/mq/messages.go`) on `nexus.event.admin-audit` (`packages/control-plane/cmd/control-plane/wiring/audit.go`). The Hub's admin-audit consumer (`packages/nexus-hub/internal/observability/consumer/admin_audit.go`) inserts into `AdminAuditLog` with a hash chain stamped Hub-side (`packages/nexus-hub/internal/traffic/chain/chain.go` — `pg_advisory_xact_lock` plus `previousHash` + `integrityHash` + `hashInput Bytes` columns; the canonical hash payload is stored verbatim so chain verification survives JSONB roundtrip normalization). Hashing is centralised in the Hub on purpose — letting any Control Plane replica compute its own hash would let concurrent admins fork the chain.

**Dead-letter queue.** Persistent insert failures route to `traffic_event_dlq` instead of redelivering forever. The consumer's `nakOrDLQ` helper inspects each `mq.Message.NumDelivered` (populated from the NATS metadata via `packages/shared/transport/mq/consumer.go`); messages at or above `redeliveryThresholdAttempts` (5) are written to the DLQ table with `msg_id`, `subject`, raw `payload`, `delivery_count`, and `last_error`, then ACKed so the broker moves on. Below the threshold the message is Nak'd as before. Admin inspection + retry live at `GET /api/admin/observability/dlq` + `POST /api/admin/observability/dlq/:id/retry` (`packages/control-plane/internal/observability/dlq/handler/dlq.go`), which proxy to `GET /api/hub/dlq` + `POST /api/hub/dlq/:id/retry` (`packages/nexus-hub/internal/fleet/handler/hubapi/hub_api_dlq.go`). Retry republishes the raw payload to the original subject; on success the DLQ row is deleted and CP stamps an `AdminAuditLog` entry against `admin:observability-dlq.manage`. The CP-UI surface lives at `/infrastructure/dlq` (`packages/control-plane-ui/src/pages/infrastructure/dlq/InfraDlqPage.tsx`).

**Normalize backfill.** When `insertNormalizedPayloads` partially fails the parent transaction still commits — raw bytes survive on `traffic_event_payload` but the `traffic_event_normalized` sidecar is left missing or NULL. The `normalize-backfill` job (`packages/nexus-hub/internal/jobs/defs/audit/normalize_backfill.go`, default 5-minute interval) scans for sidecar gaps, re-runs `shared/transport/normalize.BuildRegistry()` against the inline bodies, and upserts the sidecar. Spill-ref-only payloads are skipped pending backend access. Counters: `nexus_normalize_backfill_{scanned,filled,skipped,errors}_total` (skipped carries a `reason` label; errors carries `phase`).

Detail in [audit-pipeline-architecture.md](audit-pipeline-architecture.md). Per-mutation coverage (which CP admin handlers emit which `action` / `entityType` strings, and how the UI renders them) is in [admin-audit-log-coverage.md](admin-audit-log-coverage.md).

## 4. Diag pipeline (high level)

Diag is the runtime-error feed: any `slog` record at `Level >= ERROR` (default; `LevelWarn` and `LevelInfo` are off unless an admin enables diag mode) flows to Hub as a `DiagEvent` envelope.

**Producer.** Every service / agent installs `diag.SlogSink` (`packages/shared/core/diag/slog_sink.go`) as a `slog.Handler`. On each record the sink:

1. Maps the slog level via `mapLevel` — `≥ LevelError+4 → fatal`, `≥ LevelError → error`, `≥ LevelWarn → warn`, else `info`.
2. Computes `messageHash = md5(level|source|message)` — the dedup key.
3. Walks both the `WithAttrs` chain and the record's attrs; lifts a `trace_id` attr (key `TraceIDAttrKey`) into the typed `DiagEvent.TraceID` field so downstream queries hit a real column rather than probing the JSONB `Attrs` map. Remaining attrs flow into `Attrs` unchanged.
4. Runs the event through `opsmetrics.Dedup` (`packages/shared/core/metrics/registry/dedup.go`). Dedup folds duplicates within a tick into a single emit with `repeatCount`. Every service (agent + all four server services) wires Dedup uniformly — agent constructs it manually so its `DiagBundle` can expose the handle for `Tick()` access; server services use the `SlogSinkConfig.OpsReg` auto-construct path which registers `nexus_diag_dedup_collapsed_total{thing_type, severity}` against the opsmetrics registry. The `thing_type` label is pinned to the sink's `Source` so per-service contribution stays separable in the Prometheus view.
5. Routes the result: if the WebSocket transport is up, push directly via `thingclient.PushDiagEvent`; if it's down, queue in the in-process `ReconnectBuffer` (`packages/shared/core/diag/reconnect_buffer.go`) for replay on reconnect.
6. For FATAL events, **also** persist to a `LocalBufferInserter` (when wired — the agent uses a SQLCipher-backed buffer) so a process crash before the next WS flush still recovers the event on next boot.

The crash recovery path is in `packages/shared/core/diag/recovery.go`: a recovered panic builds a FATAL `DiagEvent`, persists it locally (best-effort), and ships via the same pipeline.

**Wire envelopes.** Two delivery paths:

- WS push: `msgTypeDiagEvent = "diag_event"` (`packages/shared/transport/thingclient/opsmetrics.go`). Outbox drops surface as `outbox_dropped_total{type="diag_event"}`.
- HTTP drain: `POST /api/internal/things/diag-events:batch` (`packages/nexus-hub/internal/observability/handler/diag/opsmetrics_diag.go`, route registered in `packages/nexus-hub/internal/handler/routes.go`) — the agent posts pending rows from its local crash buffer here at startup.

**Consumer.** `DiagWriter` (`packages/nexus-hub/internal/observability/opsmetrics/diag_writer.go`) is a bounded-channel batch writer; on overflow events are dropped. It issues `pgx.CopyFrom` into `thing_diag_event`.

**Persistence.** `tools/db-migrate/schema.prisma`:

- `thing_diag_event` (model `ThingDiagEvent`) — id, thingId, thingType, occurredAt, receivedAt, level, eventType (`error | crash | watchdog | lifecycle`), source, message, messageHash, traceId (typed cross-service correlation column with `(thing_id, trace_id, occurred_at DESC)` btree index), attrs, stackTrace, repeatCount, agentVersion, osInfo.
- `thing_diag_mode_window` (`ThingDiagModeWindow`) — per-Thing time window during which the Thing emits at full diag verbosity.
- `diag_silence` (`DiagSilence`) — `(messageHash, level)` silence registry so the admin Recent Errors page can collapse known-noise issues.

Detail in [diag-event-triage-architecture.md](diag-event-triage-architecture.md).

## 5. Metrics — Prometheus + rollup tables

Metrics live in two universes simultaneously: a real-time Prometheus surface (low-cardinality, scraped) and a Hub-bound rollup pipeline (per-Thing samples, aggregated for the admin UI and the alert engine).

**One registration site, two surfaces.** Every metric is created via the shared `opsmetrics.Registry` (`packages/shared/core/metrics/registry/registry.go`):

- The same `NewCounter` / `NewGauge` / `NewHistogram` call registers a real Prometheus `CounterVec` / `GaugeVec` / `HistogramVec` (scraped by `/metrics`) **and** binds the instrument so the per-tick `Sampler.Collect()` can include it in `metrics_sample` messages pushed to Hub.
- Dotted opsmetrics names (`cache.hits_total`) convert to Prometheus snake_case (`cache_hits_total`) via `promName`.
- Histograms use the canonical six-bucket layout `HistogramBucketsMs = {50, 100, 200, 500, 1000}` (the five explicit bounds plus the implicit +Inf bucket).

**Prometheus scrape.** All four server-side services expose `/metrics`:

- Nexus Hub — `packages/nexus-hub/cmd/nexus-hub/wiring/routes.go` (`promhttp.Handler()`).
- AI Gateway — `packages/ai-gateway/cmd/ai-gateway/wiring/routes.go` (`promhttp.Handler()`).
- Control Plane — `packages/control-plane/cmd/control-plane/wiring/echosetup.go` (`promhttp.Handler()`).
- Compliance Proxy — `packages/compliance-proxy/internal/health/handler.go` (`promhttp.Handler()`); the internal admin endpoint at `packages/compliance-proxy/internal/runtime/server/server.go` requires the internal token via `tokenAuth.Require(promhttp.Handler())`.

The Hub registers its own scrape URL on the Self Thing (`packages/nexus-hub/cmd/nexus-hub/wiring/self.go` — `MetricsURL: http://<advertise-addr>/metrics`) so the admin UI's Nodes page can deep-link.

**AI Gateway business metrics.** `packages/ai-gateway/internal/platform/metrics/metrics.go` registers the AI-Gateway-specific counters and histograms: `requests_total{provider,model,endpoint,status}`, `request_duration_ms{provider,model,endpoint}`, `tokens_total{provider,model,direction}`, `errors_total{provider,error_type}`, `schema_mismatch_total`, `hook.pipeline_total`, `traffic.extract_total`, `router.retry_total`, `forward_header_dropped_total`, `reasoning_passthrough_total`, the `estimate_*` family.

**WS metrics_sample push.** Every Thing (services + agents) periodically calls `thingclient.PushMetricsSample` with a `SampleBatch{ThingID, SampledAt, Samples[]}` (`packages/shared/core/metrics/registry/types.go`). The Hub `MetricsWriter` (`packages/nexus-hub/internal/observability/opsmetrics/writer.go`) ingests these into `metric_ops_raw`. Static-identity (`StaticInfo` — hostname, OS, CPU, machine ID, device fingerprint — `packages/shared/core/metrics/registry/types.go` and `packages/shared/core/metrics/platform/staticinfo.go`) lands on `thing.metadata.staticInfo` via `thingclient.UpdateStaticInfo`.

**Rollup tables.** Two families:

| Family | Source | 5m | 1h | 1d | 1mo |
| --- | --- | --- | --- | --- | --- |
| Fleet-wide traffic | `traffic_event` | `metric_rollup_5m` | `MetricRollup1h` | `MetricRollup1d` | `MetricRollup1mo` |
| Per-Thing traffic — server sources | `traffic_event` rows where `source IN ('ai-gateway', 'compliance-proxy')` | `ThingMetricRollup5m` | `ThingMetricRollup1h` | `ThingMetricRollup1d` | `ThingMetricRollup1mo` |
| Per-Thing traffic — agent sources | `traffic_event` rows where `source = 'agent'`, gated by `scheduler.enableAgentRollup` (default `false`) — agents compute their own rollups locally at fleet scale | (gated) | (gated) | (gated) | (gated) |
| Per-Thing ops samples | `metric_ops_raw` (from `metrics_sample`) | (raw) | `metric_ops_rollup_1h` | `metric_ops_rollup_1d` | `metric_ops_rollup_1mo` |

`Rollup5mJob` (`packages/nexus-hub/internal/jobs/defs/rollup/rollup_5m.go`) is the canonical pattern: each run catches up from a persisted watermark to the most recent sealed 5-minute bucket; per-bucket writes are idempotent via DELETE+INSERT in a single transaction that also advances the watermark, so a replica restarting mid-bucket produces the same output. Merge jobs (`rollup_merge.go`, `ops_rollup_cascade.go`) cascade 5m → 1h → 1d → 1mo. Derived health rollups (`credential_health_rollup.go`, `provider_health_rollup.go`) sit on top. Retention is handled by `rollup_retention.go`.

Detail in [metrics-rollup-architecture.md](metrics-rollup-architecture.md); naming conventions in [prometheus-naming-architecture.md](prometheus-naming-architecture.md).

## 6. Traces — OTel pipeline

Traces are a thin OTel layer that exports spans to an external collector; Nexus stores no spans itself.

**Provider.** `packages/shared/core/telemetry/provider.go` defines `SwappableTracerProvider` — an `embedded.TracerProvider`-conformant wrapper that uses `atomic.Pointer` for lock-free reads on every span creation, and `sync.Mutex` only for `Reconfigure` (rare, serialized). Old providers are shut down in a background goroutine with a 5-second timeout. `Init` registers itself as the global TracerProvider via `otel.SetTracerProvider`.

`Config{Enabled, Endpoint, ServiceName, SamplingRate}` controls behaviour:

- `Enabled=false` or empty `Endpoint` → no-op provider, no exporter, no goroutines.
- Otherwise → OTLP HTTP exporter (`otlptracehttp.New(WithEndpoint, WithInsecure)`) + a `Resource` carrying `service.name = ServiceName` + a `ParentBased(TraceIDRatioBased(SamplingRate))` sampler. The provider uses `WithBatcher(exporter)` so spans are batched, not synchronous.

**HTTP middleware.** `packages/shared/core/telemetry/httptrace.go` exposes `HTTPTrace(serviceName)`:

- Extracts incoming trace context via `otel.GetTextMapPropagator().Extract` over the request header carrier.
- Opens a `SpanKindServer` span named `"<METHOD> <PATH>"` with `HTTPRequestMethodKey`, `URLPath`, `ServerAddress` semconv attributes.
- On response, stamps `HTTPResponseStatusCode` and sets `error=true` when status ≥ 400.
- The internal `statusWriter` declares `Flush()` and `Unwrap()` so handlers performing `w.(http.Flusher)` or going through `http.ResponseController` see the underlying capabilities through the middleware chain — without `Flush()`, every SSE handler sitting behind `HTTPTrace` would silently degrade to a Content-Length-buffered response.

Detail in [otel-pipeline-architecture.md](otel-pipeline-architecture.md); semantic-convention attribute set in [otel-span-attributes-architecture.md](otel-span-attributes-architecture.md).

## 7. SIEM bridge

Customers running their own SIEM (Splunk / Datadog / Elastic / generic webhook / NDJSON file) receive a copy of every audit-relevant row. The Hub is the single canonical forwarder — compliance-proxy and the other emitters publish their audit events to MQ, and the Hub bridge picks them up off the `traffic_event` and `AdminAuditLog` tables.

**Hub bridge — `packages/nexus-hub/internal/observability/siem/bridge.go`.** A polling forwarder that reads new `traffic_event` and `AdminAuditLog` rows since independent checkpoints (persisted in `system_metadata` so a restart does not re-send) and pushes them through a pluggable `Sink` (`packages/nexus-hub/internal/observability/siem/sink.go`). Configuration is sourced from the `siem.config` row in `system_metadata`, which the Control Plane admin handler (`packages/control-plane/internal/observability/siem/handler/handler.go`) manages. The bridge re-reads `siem.config` at the head of every `Poll()` cycle (default 30s); admin-saved changes take effect within one poll interval without restart. Defaults: `PollInterval=30s`, `BatchSize=200`. `BridgeConfig` carries `EventTypes` (whitelist) and `TrafficMode` (selects which traffic rows qualify). Compliance-proxy traffic appears as `traffic_event` rows with `source='compliance-proxy'` and is forwarded by the same bridge — no per-service split.

Detail in [siem-bridge-architecture.md](siem-bridge-architecture.md).

## 8. Cross-surface concerns

### 8.1 Correlation via `trace_id`

Every audit row (`traffic_event.trace_id`), every diag event (`thing_diag_event.trace_id` typed column populated via the SlogSink auto-extract path), every span (OTel native), and every SIEM payload carries the same `trace_id`. The OTel propagator extracts incoming trace context at the HTTP edge (`HTTPTrace` middleware), the audit emitter snapshots it onto `TrafficEventMessage.TraceID`, request-scoped loggers stamp it via `logger.With(diag.TraceIDAttrKey, traceID)` so every diag emit through that scope picks it up, and downstream consumers index on the typed column. A `trace_id` lookup from a Tempo/Jaeger span resolves to one or more `traffic_event` rows (one per service the request crossed) and any `thing_diag_event` rows whose `trace_id` column matches. This is the cross-surface stitching strategy.

Detail in [trace-id-propagation-architecture.md](trace-id-propagation-architecture.md).

### 8.2 Endpoint-type vocabulary

`traffic_event.endpoint_type` is the canonical `typology.EndpointKind` string (`chat`, `embeddings`, `stt`, `tts`, `image_generation`, `batch`). The value is derived once per request at AI Gateway handler-dispatch time via `string(typology.KindFromWireShape(resolved.WireShape))`, stamped onto `audit.Record.EndpointType`, and travels on the MQ wire as `TrafficEventMessage.EndpointType` (`packages/shared/transport/mq/messages.go`) into the Hub db-writer.

Downstream consumers (Hub db-writer, SIEM forwarder, alert rules, analytics queries, AI Gateway Prometheus `endpoint` label, cost-formula registry) read this column verbatim — they do NOT translate. The canonical typology kind is the single vocabulary across in-process code and persistence; there is no second per-component dialect.

See [endpoint-typology-architecture.md §7 and §10](../foundation/endpoint-typology-architecture.md) for the path-segment helper and per-service surface map.

**Adding a new endpoint kind that needs traffic-event analytics.** Add the `EndpointKind*` constant in `packages/shared/transport/typology/endpointkind.go`, register the corresponding `ClassifyPath` rule in `defaults.go`, and append the matching path-segment case in `KindFromPathSegment` (`path_segment.go`) so audit's `EndpointTypeFromPath` (`packages/ai-gateway/internal/platform/audit/audit.go`) resolves the new request shape. Cost / Prometheus / SIEM consumers inherit the new kind automatically since they read the canonical string.

### 8.3 Alerting

The alert engine (`packages/nexus-hub/internal/alerts/eval/engine.go` + `packages/nexus-hub/internal/alerts/engine/dispatcher.go`) reads from the metrics surface — `metric_rollup_*` for time-windowed counts and the `Sampler` surface for sliding-window evaluation (`packages/nexus-hub/internal/alerts/eval/sample_window.go`). Rules live in the `AlertRule` model (`tools/db-migrate/schema.prisma`): each rule has a `sourceType` from `{quota, proxy, thing, provider, audit, system}`, a `defaultSeverity`, a JSON `params` blob validated against `paramsSchema`, a `cooldownSec`, and an optional `group_id_filter` that scopes firing to one `DeviceGroup`. Firing alerts land in `Alert` (`state` defaults to `FIRING`); dispatches go to `AlertChannel` rows. Detail in [alerting-architecture.md](alerting-architecture.md).

### 8.4 Runtime introspection

A safety valve for operators who need the in-memory state of a running Thing without writing a new endpoint per question. `packages/shared/core/diag/runtimeintrospect/` defines a `Source` interface (`Name() + Snapshot()`) that subsystems implement. Name convention: `config.<key>` for thingclient config_keys, `cache.<category>` for configcache categories, `runtime.<area>` for ad-hoc state. `Snapshot()` must be in-memory only (no DB or network) and must redact secrets. The handler (`packages/shared/core/diag/runtimeintrospect/handler.go`) requires a bearer token; an empty token returns HTTP 503 ("administratively disabled") rather than serving — refuse over expose. Default timeout 5 seconds; method GET only. Detail in [runtime-introspection-architecture.md](runtime-introspection-architecture.md).

### 8.5 Test harness

Unit tests for every surface (audit consumer, diag writer, SIEM bridge, slog sink, rollup jobs) use `pgxmock` for the DB side and in-memory channels for the MQ side; the AI Gateway audit tests carry the most coverage (`packages/ai-gateway/internal/platform/audit/audit_test.go`, `endpoint_type_test.go`, `payload_capture_test.go`, `derive_cache_status_test.go`). End-to-end smoke runs via `tests/scripts/smoke-gateway.py` exercise the full audit → MQ → consumer → traffic_event chain. Detail in [test-harness-architecture.md](test-harness-architecture.md).

## 9. Operational invariants

- **One vocabulary per concept.** `endpoint_type`, `cache_status`, `error_code`, `level` are canonical strings; consumers never translate.
- **Producers fail open.** A diag emit that cannot reach Hub queues into the reconnect ring (and the local crash buffer for FATAL); audit emits batch in-memory up to `maxQueueSize=10000` before back-pressure; metric sampler drops a tick rather than block the data plane. None of these surfaces can block a request.
- **Consumers are idempotent.** Rollup jobs use watermark + DELETE+INSERT-in-tx; the audit consumer treats duplicate IDs as ack-and-discard; the SIEM bridge persists its checkpoint after a successful flush.
- **One Hub-side sequencer for chained data.** `AdminAuditLog`'s `previousHash`/`integrityHash` chain is computed Hub-side under `pg_advisory_xact_lock` because letting any Control Plane replica fork the chain would be silent corruption. The same pattern would apply to any future tamper-evident log.
- **Prometheus instruments are registered once.** The shared `Registry.NewCounter` is idempotent on name collisions and returns the cached instance; the supplied labels are ignored on a cache hit (the original labels win). This is what lets `/metrics` and `metrics_sample` share the same instrument set without double-registration panics during hot-reload.

## References

- `packages/ai-gateway/internal/platform/audit/audit.go`
- `packages/ai-gateway/internal/platform/audit/audit_test.go`
- `packages/ai-gateway/internal/platform/audit/endpoint_type_test.go`
- `packages/ai-gateway/internal/platform/audit/payload_capture_test.go`
- `packages/ai-gateway/internal/platform/audit/derive_cache_status_test.go`
- `packages/ai-gateway/internal/platform/metrics/metrics.go`
- `packages/ai-gateway/cmd/ai-gateway/wiring/routes.go`
- `packages/compliance-proxy/internal/audit/event_message.go`
- `packages/compliance-proxy/internal/health/handler.go`
- `packages/compliance-proxy/internal/runtime/server/server.go`
- `packages/control-plane/cmd/control-plane/wiring/audit.go`
- `packages/control-plane/cmd/control-plane/wiring/echosetup.go`
- `packages/control-plane/internal/observability/siem/handler/handler.go`
- `packages/nexus-hub/cmd/nexus-hub/main.go`
- `packages/nexus-hub/cmd/nexus-hub/wiring/routes.go`
- `packages/nexus-hub/cmd/nexus-hub/wiring/self.go`
- `packages/nexus-hub/internal/handler/routes.go`
- `packages/nexus-hub/internal/observability/consumer/traffic.go`
- `packages/nexus-hub/internal/observability/consumer/admin_audit.go`
- `packages/nexus-hub/internal/observability/consumer/message.go`
- `packages/nexus-hub/internal/jobs/defs/audit/normalize_backfill.go`
- `packages/nexus-hub/internal/fleet/handler/hubapi/hub_api_dlq.go`
- `packages/control-plane/internal/observability/dlq/handler/dlq.go`
- `packages/control-plane-ui/src/pages/infrastructure/dlq/InfraDlqPage.tsx`
- `packages/nexus-hub/internal/jobs/defs/rollup/rollup_5m.go`
- `packages/nexus-hub/internal/jobs/defs/rollup/rollup_merge.go`
- `packages/nexus-hub/internal/jobs/defs/rollup/thing_rollup_5m.go`
- `packages/nexus-hub/internal/jobs/defs/rollup/thing_rollup_merge.go`
- `packages/nexus-hub/internal/jobs/defs/rollup/credential_health_rollup.go`
- `packages/nexus-hub/internal/jobs/defs/rollup/provider_health_rollup.go`
- `packages/nexus-hub/internal/jobs/defs/rollup/rollup_retention.go`
- `packages/nexus-hub/internal/jobs/defs/metrics/ops_rollup_1h.go`
- `packages/nexus-hub/internal/jobs/defs/metrics/ops_rollup_cascade.go`
- `packages/nexus-hub/internal/observability/opsmetrics/diag_writer.go`
- `packages/nexus-hub/internal/observability/opsmetrics/handlers.go`
- `packages/nexus-hub/internal/observability/opsmetrics/writer.go`
- `packages/nexus-hub/internal/observability/handler/diag/opsmetrics_diag.go`
- `packages/nexus-hub/internal/traffic/chain/chain.go`
- `packages/nexus-hub/internal/observability/siem/bridge.go`
- `packages/nexus-hub/internal/observability/siem/sink.go`
- `packages/nexus-hub/internal/observability/siem/classify.go`
- `packages/nexus-hub/internal/observability/siem/formatter.go`
- `packages/nexus-hub/internal/alerts/eval/engine.go`
- `packages/nexus-hub/internal/alerts/eval/sample_window.go`
- `packages/nexus-hub/internal/alerts/engine/dispatcher.go`
- `packages/shared/core/diag/slog_sink.go`
- `packages/shared/core/diag/reconnect_buffer.go`
- `packages/shared/core/diag/recovery.go`
- `packages/shared/core/diag/multi_handler.go`
- `packages/shared/core/diag/runtimeintrospect/runtimeintrospect.go`
- `packages/shared/core/diag/runtimeintrospect/handler.go`
- `packages/shared/core/metrics/registry/registry.go`
- `packages/shared/core/metrics/registry/types.go`
- `packages/shared/core/metrics/registry/dedup.go`
- `packages/shared/core/metrics/instruments/aggregator.go`
- `packages/shared/core/metrics/platform/sampler.go`
- `packages/shared/core/metrics/platform/runtime.go`
- `packages/shared/core/metrics/platform/staticinfo.go`
- `packages/shared/core/telemetry/provider.go`
- `packages/shared/core/telemetry/httptrace.go`
- `packages/shared/transport/mq/messages.go`
- `packages/shared/transport/mq/streams.go`
- `packages/shared/transport/thingclient/opsmetrics.go`
- `packages/shared/transport/typology/endpointkind.go`
- `packages/shared/transport/typology/defaults.go`
- `packages/shared/transport/typology/path_segment.go`
- `tools/db-migrate/schema.prisma`
