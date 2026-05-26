---
doc: agent-backpressure-rollup-architecture
area: service
service: agent
tier: 1
updated: 2026-05-21
---

# Agent Backpressure & Local Rollup Architecture

> **Tier 2 architecture doc.** Read when touching `packages/agent/internal/observability/backpressure/`, `localrollup/`, `audit/queue/`, `audit/hub/`, or `spilluploader/`. Sister doc: `audit-pipeline-architecture.md` (the server-side ingestion side).

The agent runs on flaky networks (laptops, coffee shops, conference Wi-Fi). The local pipeline must absorb network outages, throttle the hot-path forwarder when the local queue gets behind, and drain to Hub when connectivity returns — without unbounded memory growth.

Two independent concerns live in this doc: **backpressure** (a single atomic boolean the NE bridge polls on every flow to decide whether to passthrough) and **local rollup** (the agent-UI stats panel aggregator that produces 5m/1h/1d/1mo bins in local SQLite). They are not stages in a single pipeline.

---

## 1. The actual data flow

```
intercepted flow
   │
   ▼
Hook pipeline + audit emit
   │
   ▼
SQLCipher persistent queue (audit_events table, packages/agent/internal/observability/audit/queue/queue.go)
   │
   ├──▶ Hub upload         (mTLS WS primary, HTTP POST /api/internal/things/agent-audit fallback)
   │
   ├──▶ localrollup        (in-place aggregation into thing_metric_rollup_local_{5m,1h,1d,1mo} for UI)
   │
   └──▶ backpressure poll  (UnsyncedCount() → high/low-watermark hysteresis → atomic.Bool)
                                                                                    │
                                                                                    ▼
                                                                       NE bridge reads on hot path
                                                                       throttled → passthrough flow
```

There is **no** in-memory ringbuffer, no MPSC stage, no 500-event / 50K-event / 200MB capacity cap. SQLCipher is the only buffer between the hooks and Hub; backpressure is a single flag that controls whether the NE bridge keeps producing events.

## 2. The backpressure flag

Package `packages/agent/internal/observability/backpressure` exposes a `Store` that wraps a single `atomic.Bool`:

```go
type Config struct {
    HighWatermark int           // enter throttle when current >= HighWatermark (default 500)
    LowWatermark  int           // exit throttle when current <= LowWatermark  (default 200)
    PollInterval  time.Duration // queue depth poll cadence            (default 2s)
    Logger        *slog.Logger  // emits INFO on every state transition
}
```

- `IsThrottled()` is the hot-path call from the NE bridge. Lock-free atomic load; sub-microsecond.
- `Update(currentDepth int)` is called by the background poller (`Store.Poll(ctx, source)`). Hysteresis: enter throttle only when crossing `HighWatermark` from below, exit only when crossing `LowWatermark` from above. The hysteresis gap (300 events by default ≈ a few seconds of typical intercept traffic) prevents the flag from flapping on every Hub upload batch.
- `Poll(ctx, source)` ticks at `PollInterval`, calls `source()` (typically `func() int { return queue.UnsyncedCount() }`) and feeds the result to `Update`. Started once at agent boot.
- `NewStore` rejects `LowWatermark >= HighWatermark` by falling back to defaults — a missing-hysteresis store would flap on every call.

The poller's measurement (one `sqlite COUNT(*)` per poll) is borne off the hot path; the bridge reads the atomic only. When throttled, `handleNewFlow` returns the OS-passthrough decision for incoming flows, shedding the cost of running the hook pipeline + emitting an audit event. `deny` / `block` / `error` outcomes are unaffected — they were never produced from the cold path being throttled.

## 3. The SQLCipher persistent queue

`packages/agent/internal/observability/audit/queue/queue.go` — single SQLite file, SQLCipher-encrypted at rest with the key from the platform keystore (see `agent-keystore-architecture.md` §4). Tables include the `audit_events` queue plus the localrollup bin tables.

Properties:

- WAL mode for crash safety.
- The queue carries the columns the audit row needs (request/response phase durations, hook decisions, action, bytes/tokens, attestation fields, etc.). Field shape lives in `packages/agent/internal/observability/audit/event/event.go`.
- No documented hard cap on row count or on-disk size in the package today — backpressure is the throttle mechanism, not a queue-size limit. (If you want a hard cap, add it explicitly with an SDD.)

## 4. Local rollup is for the agent UI, not for upload

`packages/agent/internal/observability/localrollup/localrollup.go` is the agent-side mirror of Hub's per-Thing rollup pipeline. It scans the SQLCipher `audit_events` table on a 1-minute ticker, aggregates into 5m buckets in `thing_metric_rollup_local_5m`, then cascades into 1h / 1d / 1mo bins. The output is consumed by the agent UI's stats panel via `QueryRollup`; it is **not** a pre-upload network-compression step.

Retention defaults (user-decided): `5m=24h`, `1h=30d`, `1d=365d`, `1mo=5y`. Overridable per `Aggregator` instance for ops control.

Upload to Hub happens independently. The rollup keeps the agent native UI's stats panel responsive offline and makes the 10K-agent fleet scenario (per Hub config `enableAgentRollup=false`) safe without sacrificing detail at the device.

Metric names mirror the Hub-side constants (`request_count`, `status_2xx_count`, `latency_us_sum`, `latency_upstream_ttfb_sum`, `hook_allow_count`, `bump_success_count`, `action_passthrough_count`, etc.). The full list is in `localrollup.go`.

## 5. Drain-to-Hub upload

Primary: WebSocket to Hub (`packages/shared/transport/thingclient`). Sustained, low-latency.

Fallback: HTTPS `POST /api/internal/things/agent-audit` (mTLS + bearer device-token). Used when WS is unavailable. Server-side handler registered at `packages/nexus-hub/internal/handler/routes.go` → `agentAuditAPI.UploadAgentAudit`; client at `packages/agent/internal/observability/audit/hub/hub_client.go`.

Drain logic reads events from SQLCipher, posts to Hub, and marks rows uploaded on 2xx. On 5xx / network error the row stays queued; on persistent failure the agent stays throttled (because `UnsyncedCount` keeps rising).

## 6. Body-spill interaction

Audit events with large bodies reference the body in spillstore — but the agent never holds a long-lived presigned URL. Per request, the agent calls Hub `POST /api/internal/things/spill-uploads` (handled in `packages/agent/internal/observability/spilluploader/uploader.go::mint`), receives a short-lived PUT URL + storage key, uploads the body, and stamps an `audit.SpillRef` (backend, key, size, sha256, content-type) onto the audit envelope. Bodies above the mint cap fall back to inline; below the cap fall back to inline on Hub error. The `ErrFallbackInline` sentinel signals the audit emitter to keep the body inline.

## 7. Connectivity scenarios

| Scenario | Behaviour |
|---|---|
| Fully online | Drain in real time; queue stays near-empty; backpressure flag never sets. |
| Brief offline (1-30min) | Queue fills slowly; backpressure flag flips on once depth ≥ HighWatermark; new NE flows passthrough; drain resumes on reconnect; flag clears once depth ≤ LowWatermark. |
| Extended offline (hours) | Queue keeps growing; backpressure stays on for the duration; the UI's stats panel still works locally via localrollup. |
| Reconnect | Drain catches up; queue depth falls; backpressure clears. |

## 8. What's intentionally not in this design

- **No metric names like `agent_audit_queue_depth` / `agent_audit_queue_pressure_high` / `agent_audit_drops_total` / `agent_audit_drain_failures_total`.** None of these strings appear anywhere in `packages/`. Observability for queue depth + throttle transitions today is via slog log lines (`backpressure: entering throttle`, `backpressure: exiting throttle`) carrying the current depth + thresholds, not via Prometheus counters. If a counter is needed, file an SDD before adding it.
- **No DLQ table, no `status=dlq` rows.** A persistent-failure row simply stays in the queue.
- **No rate-limited burst-drain on reconnect.** The drain reads up to its batch size and posts; Hub's own ingress rate-limiting bounds the inflow.
- **No "rollup before upload" network-compression step.** Rollup serves the agent UI; events upload row-by-row from the queue.

## 9. Sources

- `packages/agent/internal/observability/backpressure/store.go` — `Store`, `Config`, `Poll`, `Update`, `IsThrottled`.
- `packages/agent/internal/observability/audit/queue/queue.go` — SQLCipher queue, `UnsyncedCount`, queue schema (including rollup bin tables).
- `packages/agent/internal/observability/audit/event/event.go` — `Event` shape (phase durations, hook outcomes, attestation fields).
- `packages/agent/internal/observability/audit/hub/hub_client.go` — POST to `/api/internal/things/agent-audit`.
- `packages/agent/internal/observability/localrollup/localrollup.go` — `Aggregator`, retention defaults, metric-name constants.
- `packages/agent/internal/observability/spilluploader/uploader.go` — body-spill mint via `POST /api/internal/things/spill-uploads`.

## 10. Cross-references

- `agent-keystore-architecture.md` — SQLCipher DB key.
- `audit-pipeline-architecture.md` — server-side ingestion.
- `agent-forwarder-architecture.md` — what produces the events.
- `spillstore-architecture.md` — body upload path.
- `agent-runtime-invariants.mdc` Rule 2 — `trafficUploadLevel` filter (governs which events are emitted in the first place).
