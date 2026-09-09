# Diagnostics & event triage architecture

The diagnostics subsystem is the operator's microscope. It does three things:

- **Capture** every `slog` record at `ERROR` and above across all five Thing types and ship it to Hub as a `DiagEvent`.
- **Raise verbosity on demand** — an admin opens a time-boxed diagnostic window on a specific agent so its local log file captures `debug`-level detail while a problem is reproduced.
- **Triage + silence** — surface the captured events grouped for a human to scan, and let operators collapse known-noise message groups so the signal stays visible.

It deliberately does **not** do anomaly detection (that is the alerting subsystem) and does **not** produce a downloadable support archive (no such feature exists). See [observability-architecture.md](observability-architecture.md) for how diag sits alongside the audit, metrics, and tracing pipelines.

## 1. Pieces and where they live

| Concern | Code |
|---|---|
| Capture (slog sink, dedup, buffers, recovery) | `packages/shared/core/diag/` |
| DiagEvent envelope + level/event-type vocabulary | `packages/shared/core/metrics/registry/types.go` |
| Process-wide dynamic log level | `packages/shared/core/logging/logging.go` |
| Hub-side persistence | `packages/nexus-hub/internal/observability/opsmetrics/diag_writer.go` |
| Agent diag wiring + diagnostic-mode level control | `packages/agent/cmd/agent/` |
| Agent crash buffer + startup drain | `packages/agent/internal/observability/diag/` |
| CP admin: diagnostic mode, event triage, silences | `packages/control-plane/internal/infrastructure/infra/` |
| Silence store | `packages/control-plane/internal/observability/diag/diagstore/` |
| Triage aggregation queries | `packages/control-plane/internal/observability/opsmetrics/opsstore/` |

## 2. Data model

Three tables. `ThingDiagEvent` and `ThingDiagModeWindow` are mirrored as Go structs under `packages/shared/schemas/configtypes/observability/`; `DiagSilence`'s struct lives in the Control Plane `diagstore` package.

- **`thing_diag_event`** (`ThingDiagEvent`) — one row per captured event: `thingId`, `thingType`, `occurredAt`, `receivedAt`, `level`, `eventType`, `source`, `message`, `messageHash`, `externalRequestId`, `attrs` (JSONB), `stackTrace`, `repeatCount`, `agentVersion`, `osInfo`. `externalRequestId` is a first-class column (the cross-service request id — `X-Nexus-Request-Id` or its `X-Request-Id` alias, minted when neither arrived) with a `(thingId, externalRequestId, occurredAt DESC)` btree index, so "every diag row for request X" hits a real index rather than probing the JSONB map. Indexes also cover `(thingId, occurredAt)`, `(level, occurredAt)`, `(eventType, occurredAt)`, and `(messageHash, occurredAt)`.
- **`thing_diag_mode_window`** (`ThingDiagModeWindow`) — per-Thing audit history of a diagnostic-mode window: `thingId`, `startedAt`, `endedAt`, `setBy`, `reason`. This is the record the admin list endpoint reads; it is not the delivery channel (see §5).
- **`diag_silence`** (`DiagSilence`) — `messageHash`, `level`, `silencedBy`, `silencedAt`, `expiresAt` (nullable), `reason`, with a `(messageHash, level)` lookup index.

## 3. Capture — the slog sink

Every service and agent installs `diag.SlogSink` (`packages/shared/core/diag/slog_sink.go`) as a `slog.Handler`, composed alongside the normal stdout/file handler by `diag.MultiHandler` (`multi_handler.go`). `MultiHandler.Enabled` is permissive — it returns true when *any* wrapped handler accepts the level — so each handler applies its own threshold: the file handler prints whatever the process log level allows, while the sink suppresses everything below its own threshold.

On each record the sink:

- Gates on `Level >= cfg.Level`, which defaults to `ERROR`. `IncludeInfo` defaults to false, so info-level records are dropped unless explicitly enabled.
- Maps the slog level to the `opsmetrics` vocabulary via `mapLevel`: `>= ERROR+4 → fatal`, `>= ERROR → error`, `>= WARN → warn`, else `info`.
- Lifts an `external_request_id` attribute (key `diag.ExternalRequestIDAttrKey`) — walking both the `WithAttrs` chain and the record's own attrs — into the typed `DiagEvent.ExternalRequestID` field. A non-string value falls through into the loose `Attrs` map so a malformed value is still visible. The key is consumed, not duplicated into `Attrs`.
- Computes `messageHash = md5(level | source | message)` — the dedup key.
- Runs the event through `opsmetrics.Dedup` when configured: a 60-second collapse window over 100 distinct active message hashes, folding duplicates into a single emit carrying `repeatCount`. The collapsed count is exported as `diag.dedup_collapsed_total{thing_type, severity}`, with `thing_type` pinned to the sink's `Source`.
- Routes the result (`routeLocked`): when the WebSocket transport is up, push directly via `thingclient.PushDiagEvent`; when it is down, queue in the bounded in-process `ReconnectBuffer` (`reconnect_buffer.go`) for replay on reconnect.
- For `FATAL` events, **also** persists to a `LocalBufferInserter` when one is wired, regardless of WS state — crash buffering is at-least-once.

The crash-recovery helper (`recovery.go`) builds a `FATAL` `DiagEvent` from a recovered panic, persists it to the local crash buffer best-effort, and re-raises; the buffered crash event reaches Hub on the next startup drain (§4).

The `DiagEvent` envelope (`packages/shared/core/metrics/registry/types.go`) carries `Level` ∈ `{info, warn, error, fatal}` and `EventType` ∈ `{error, crash, watchdog, lifecycle}`. The slog sink stamps `EventType = error`; other producers (crash recovery, lifecycle emitters) set the other event types.

## 4. Transport and persistence

**Wire paths.** WS push uses message type `diag_event`; outbox drops surface as `outbox_dropped_total{msg_type="diag_event"}`. The agent additionally drains its crash buffer over HTTP at startup: `DrainPending` (`packages/agent/internal/observability/diag/drain.go`) batches rows (100 per POST, under Hub's defensive 500 cap) to `POST /api/internal/things/diag-events:batch`, sharing the agent's existing H2 pool and mTLS identity.

**Agent crash buffer.** `LocalBuffer` (`packages/agent/internal/observability/diag/local_buffer.go`) is a SQLCipher-backed `pending_diag_event` table. The slog sink inserts `FATAL` events into it (the panic-recovery path does so before re-panicking); a duplicate primary key is a no-op so a redelivered crash event stays idempotent. Drain reads oldest-first and deletes on successful upload.

**Hub writer.** `DiagWriterImpl` (`packages/nexus-hub/internal/observability/opsmetrics/diag_writer.go`) is a bounded-channel batch writer (defaults: 100 events / 100 ms) that issues `pgx.CopyFrom` into `thing_diag_event`. On queue overflow the event is dropped and `diag.dropped_total{reason="queue_overflow"}` increments. The writer backfills `messageHash` server-side via `ComputeMessageHash` when a client omits it, and maps empty `externalRequestId` / `stackTrace` / `agentVersion` to SQL NULL so `WHERE external_request_id IS NULL` filters cleanly.

## 5. Log-level control: services vs agent

Verbosity is controlled by two parallel, intentionally-separate mechanisms. Both drive the same process-wide `slog.LevelVar` in `packages/shared/core/logging/logging.go` (`SetLevel` / `CurrentLevel`), so the live level swaps without rebuilding the handler chain.

- **Server Things** (Hub, Control Plane, AI Gateway, Compliance Proxy) carry a **`log_level`** shadow key. Each service's config dispatch registers it and calls `logging.SetLevel` on change. This is a durable level change — it stays until an admin changes it back.
- **The agent** has **no `log_level` key**. Its equivalent is the **`diag_mode`** key: a per-Thing, time-boxed, audited, auto-expiring window. This split is deliberate — the agent runs on an end-user device, where a verbose level needs a time box (so it cannot be left on by accident, draining battery or capturing more than intended) and an audit trail of who opened it.

The agent's `diagModeLevelController` (`packages/agent/cmd/agent/diagmodelevel.go`) implements the window. When the `diag_mode` override carries a future `until`, it calls `logging.SetLevel("debug")` and arms a local timer that restores the startup baseline (`cfg.Log.Level`, which defaults to `info`) when the window ends — so the agent self-recovers to quiet logging even if it never receives the expiry signal (offline device). A cleared or past window restores the baseline immediately.

Crucially, raising the level affects only the **local log file**: the slog sink's DiagEvent threshold stays fixed at `ERROR+`. Diagnostic mode does not flood Hub with debug events; it makes the on-device log verbose for the operator who later collects it.

## 6. Diagnostic mode management

The Control Plane exposes four endpoints (`packages/control-plane/internal/infrastructure/infra/diagmode.go`), all gated on the carved-out `diagnostic-mode` IAM resource so a compliance/security team can be granted toggle access without holding write on every observability surface:

- `GET /agents/diagnostic-mode` — active windows (read).
- `POST /agents/:nodeId/diagnostic-mode` — enable on one agent (update).
- `POST /agents/diagnostic-mode/bulk` — enable across a filter, capped at 500 resolved agents (update).
- `DELETE /agents/:nodeId/diagnostic-mode` — disable (update).

A window's `until` is validated to fall within `[5m, 24h]` of now (`parseUntil`). The lower bound matches the override TTL floor; the upper bound caps how long a window can stay open.

**Delivery is a per-Thing override.** Enabling writes a `diag_mode` `thing_config_override` (state `{until}`, `expires_at = until`) through the Hub override API (`PUT /api/hub/things/:id/overrides/diag_mode`). Hub's `SetOverride` (`packages/nexus-hub/internal/fleet/manager/override.go`) recomputes `thing.desired`, bumps `desired_ver`, writes the audit row in-transaction, and pushes the key — so the agent receives the window through the standard config-pull path and the controller in §5 applies it. The handler records the `thing_diag_mode_window` audit-history row for the list endpoint but does **not** write a second `admin_audit_log` row (Hub's override write already did). Disabling clears the override and closes the window row.

Expiry needs no dedicated job: the generic `override-expiry` job (`packages/nexus-hub/internal/jobs/defs/expiry/override_expiry.go`) clears the `diag_mode` override once its `expires_at` passes, and the agent's local timer is the offline-safe backstop. See [jobs-architecture.md](../foundation/jobs-architecture.md).

## 7. Event triage surface

Three read-only endpoints (`packages/control-plane/internal/infrastructure/infra/diagevents.go`), gated on `observability:read`:

- **`GET /diag-events`** — newest-first paginated list from `thing_diag_event`, with `nodeId` / `level` / `eventType` / `source` / free-text / time-range filters and an `(occurred_at, id)` cursor.
- **`GET /diag-events/groups`** — the top-100 `message_hash` buckets in a time window (`ListDiagGroups` in `opsmetrics_store.go`): each bucket carries a sample message, source, distinct-affected-Thing count, total occurrences, first/last seen, max level, a per-5-minute sparkline, and a `silenced` flag derived via `EXISTS` against `diag_silence`. This is the "bucket view" an operator scans — buckets are computed by grouping on `message_hash`, not a fixed classifier.
- **`GET /diag-events/crash-cohorts`** — `FATAL`/crash events grouped by `(agent_version, os, os_version)` for the Crash Reports page.

## 8. Silences

Operators silence a `(messageHash, level)` pair (`packages/control-plane/internal/infrastructure/infra/diag_silences.go`, store in `diagstore/diag_silence_store.go`), gated on `observability:read` for list and `observability:write` for create/delete. A TTL of 0 means a permanent silence; any positive TTL is capped at 30 days to discourage "silence and forget". Duplicate active `(messageHash, level)` rows are allowed — the `EXISTS` join treats them as equivalent.

A silence's only effect is the `silenced` flag on `ListDiagGroups`: it marks the matching group so the Recent Errors page can collapse it. It does **not** filter the raw `/diag-events` stream, and it does **not** suppress anything in the alerting pipeline — silences and alerts are independent.

## 9. Boundaries

- **Anomaly / trend detection is not here.** Provider error-rate spikes, hook-latency regressions, and traffic drops are the alerting subsystem's job — see [alerting-architecture.md](alerting-architecture.md). Diag stays narrow: capture slog records and surface them for human triage.
- **Diag and audit are sibling pipelines.** Operational events flow through diag; governance/admin events flow through the audit pipeline — see [audit-pipeline-architecture.md](audit-pipeline-architecture.md).
- **Correlation is by the request id** at query time (`thing_diag_event.external_request_id` against `traffic_event.external_request_id`), joining diag rows to the traffic and audit rows for the same request. Note that `traffic_event.trace_id` is a different column holding the caller's W3C trace id, and is not what these joins use.

## References

- `packages/shared/core/diag/` — slog sink, multi-handler, reconnect buffer, crash recovery
- `packages/shared/core/metrics/registry/types.go` — `DiagEvent` envelope, level + event-type vocabulary
- `packages/shared/core/logging/logging.go` — process-wide `LevelVar`, `SetLevel`
- `packages/shared/schemas/configtypes/observability/thing_diag_event.go` — `ThingDiagEvent` mirror
- `packages/shared/schemas/configtypes/observability/thing_diag_mode_window.go` — `ThingDiagModeWindow` mirror
- `packages/agent/cmd/agent/diagmodelevel.go` — diagnostic-mode log-level controller
- `packages/agent/cmd/agent/configappliers.go` — agent `diag_mode` + `agent_settings` appliers
- `packages/agent/cmd/agent/wiring/observability.go` — agent diag subsystem wiring
- `packages/agent/internal/observability/diag/` — agent crash buffer + startup drain
- `packages/nexus-hub/internal/observability/opsmetrics/diag_writer.go` — Hub batch writer
- `packages/nexus-hub/internal/fleet/manager/override.go` — override write (recompute desired + audit + push)
- `packages/nexus-hub/internal/jobs/defs/expiry/override_expiry.go` — override (incl. diag_mode) expiry job
- `packages/control-plane/internal/infrastructure/infra/diagmode.go` — diagnostic-mode endpoints
- `packages/control-plane/internal/infrastructure/infra/diagevents.go` — triage endpoints
- `packages/control-plane/internal/infrastructure/infra/diag_silences.go` — silence endpoints
- `packages/control-plane/internal/observability/diag/diagstore/diag_silence_store.go` — silence store
- `packages/control-plane/internal/observability/opsmetrics/opsstore/opsmetrics_store.go` — diag group aggregation, diag-mode window store
- `tools/db-migrate/schema/nodes.prisma` — `ThingDiagEvent`, `ThingDiagModeWindow`, `DiagSilence` models
