# Ops Metrics & Diagnostic Events Architecture — Design Spec

**Date:** 2026-04-27
**Status:** Approved (brainstorm complete; ready for writing-plans)
**Supersedes:** `2026-04-15-status-page-metrics-design.md` (backend data source for Status Overview metrics cards)
**Reverses:** Service-call-framework §17 decision (2026-04-21) that removed the metrics push pipeline. The previous push pipeline was deleted because no consumer was wired; this spec re-introduces it with a complete consumer + storage + UI stack.

---

## 1. Context & Motivation

The platform has two operational data flows today:

| Flow | Path | State |
|------|------|-------|
| AI traffic business rollup (`request_count`, tokens, cost) | `traffic_event` table → Hub rollup-5m/1h/1d/1mo jobs → `metric_rollup_*` tables | In production. Untouched by this spec. |
| Service operational metrics (cache hits, GC, dial counts, latency) | Each service exposes Prometheus `/metrics` → Control Plane `promparse.go` direct-scrapes on demand → Status page (15 s in-memory cache) | In production. **No history, no Agent coverage.** |

Two structural gaps in the operational metrics path:

1. **Agent has no scrape surface.** Tens of thousands of agents run on NAT'd end-user laptops with no inbound HTTP. The relay metrics shipped in `2026-04-26-agent-transport-rewrite-design.md` (`relay.dial_total`, `relay.handshake_total`) are emitted but unobservable in production. Only integration tests can read them today.
2. **No history for any service.** `promparse.go` is real-time only. Operators cannot answer "what changed in cache hit rate after the v1.4 release" or "is this AI-Gateway pod's heap trending up over the last week".

Beyond metrics, agents have a third gap: **no way to surface crashes or runtime ERROR-level events to operators**. Local file logs and OS-level crash reporters are not centrally accessible across the agent fleet.

This spec replaces the current promparse-based path with a **Hub-collected operational telemetry stack** covering metrics, diagnostic events, and the supporting UI for fleet visibility.

`traffic_event` and the four `metric_rollup_*` tables are explicitly out of scope and remain unchanged. AI traffic rollup is a separate concern with its own retention requirements (5-year compliance) and its own pipeline.

---

## 2. Scope

### 2.1 In Scope (v1, fully implemented)

- New PostgreSQL schema: 4 metrics tables, 1 diag event table, 1 diag-mode-window table, 1 retention-config table.
- `thingclient` WebSocket message types `metrics_sample` and `diag_event`.
- HTTP endpoint `POST /api/internal/things/diag-events:batch` (agent crash drain).
- Hub-side metrics writer, diag writer, three rollup jobs (1h, 1d, 1mo), one retention job.
- Agent-side metrics sampler + slog ERROR sink + panic recovery + SQLCipher local crash buffer.
- All four cluster services (Hub / Control Plane / AI Gateway / Compliance Proxy) emit `metrics_sample`.
- Agent diagnostic mode (Hub admin API → shadow → agent ups frequency and per-instance labeling for the configured window).
- Configurable retention (7 keys: runtime/business × raw/1h/1d/1mo + diag warn/error/fatal).
- Full Control Plane UI:
  - Status Overview metrics cards rewired to query `metric_ops_raw` (replaces promparse path).
  - Infrastructure → Recent Errors page.
  - Infrastructure → Agent Diag Mode page (single-toggle and bulk-by-filter).
  - Infrastructure → Crash Reports page (FATAL by `agent_version × os` cohort).
  - Settings → Observability → Retention page.
  - Nodes detail page: new Metrics tab + Logs tab with a shared time axis.
- Deletion of `packages/control-plane/internal/handler/promparse.go` and the `/api/admin/service-metrics` route.

### 2.2 In Scope (v1, design only — no service-side wiring)

The cluster services (Hub, CP, AI Gateway, Compliance Proxy) do **not** emit `diag_event` in v1. Operators rely on existing local file logs and `kubectl logs --previous` for cluster-service ERROR diagnosis.

The schema, Hub WebSocket handler, retention config, and CP UI all accept `thing_type='service'` rows and render them. Adding cluster-service emission later requires only a slog sink in each service binary; no Hub or CP change.

### 2.3 Out of Scope

- `traffic_event` table and the four `metric_rollup_*` tables.
- Agent local file logs (`~/Library/Logs/...`, `journalctl`, etc.) — those are platform-native, not collected here.
- Tracing / spans (OpenTelemetry traces). This spec covers metrics and discrete events only.
- External alerting integration on top of diag events. The existing Hub Unified Alerting (`/api/v1/alerts/raise`) can later consume diag events as a producer; that wiring is a follow-up.
- Loki / ELK / Grafana integration. Operators query everything through the Control Plane UI; raw Postgres is the only store.

---

## 3. Design Decisions Locked in Brainstorm

| # | Decision | Choice |
|---|----------|--------|
| 1 | Collection model | Hub-collected for all 5 thing types; Control Plane queries Postgres directly via pgx (no Hub HTTP intermediation). |
| 2 | Agent metrics cardinality | Default fleet-aggregate; per-thing rows preserved in raw (7-day retention) but rollups collapse non-diagnostic agents into a single `thing_id=NULL, thing_type='agent'` aggregate row. |
| 3 | Retention configurability | Per-tier × per-class. Eleven independent settings (runtime/business × raw/1h/1d/1mo + diag warn/error/fatal). |
| 4 | Sampling cadence | 30 s, piggybacked on the existing thingclient heartbeat. |
| 5 | Diag event scope (v1 implementation) | Agent only emits. Schema and consumer accept service-emitted events; service emission is design-only. |
| 6 | INFO-level lifecycle events | Default off. Operators can enable via diagnostic mode for individual agents. |
| 7 | Diag transport | WebSocket only for normal events; SQLCipher local crash buffer + HTTP drain for crash safety. No NATS / no JetStream. |
| 8 | L2 static info storage | All host/process/build identity goes into `thing.metadata.staticInfo` JSONB on the core `thing` table. No new columns on `thing_service` / `thing_agent`. Unified read path regardless of thing type. |
| 9 | UI scope | All v1, no v2 deferral. Five new pages + Status rewiring + Nodes detail tabs. |

---

## 4. Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                  Things (4 cluster services + Agents)                │
│                                                                      │
│  Hub / CP / AG / Compliance Proxy   │            Agent                │
│  ───────────────────────────────     │   ───────────────────────────   │
│  L1 runtime sampler  ─┐              │   L1 runtime sampler  ─┐       │
│  L3 business gauges  ─┼─┐            │   L3 business gauges  ─┤       │
│                       │ │            │   slog ERROR sink     ─┤       │
│                       │ │            │   panic → SQLCipher buf┤       │
│                       │ │            │                        │       │
│  thingclient WS  ─────┘ │            │   thingclient WS ──────┤       │
│  (metrics_sample only)  │            │   (metrics_sample      │       │
│                         │            │    + diag_event)       │       │
└─────────────────────────┼────────────┴────────────────────────┼──────┘
                          │                                      │
                          ▼                                      ▼
                  ┌────────────────────────────────────────────────┐
                  │                  Nexus Hub                      │
                  │   ws.OnMessage("metrics_sample") → metricsWriter│
                  │   ws.OnMessage("diag_event")     → diagWriter   │
                  │   POST /api/internal/things/diag-events:batch   │
                  │                                                  │
                  │   metric_ops_raw   thing_diag_event             │
                  │        │                                         │
                  │   ops-rollup-1h  → metric_ops_rollup_1h         │
                  │   ops-rollup-1d  → metric_ops_rollup_1d         │
                  │   ops-rollup-1mo → metric_ops_rollup_1mo        │
                  │   ops-retention  (DELETE per retention-config)  │
                  └─────────────┬────────────────────────────────────┘
                                │
                                ▼  pgx direct read (no Hub HTTP hop)
                  ┌────────────────────────────────────────────────┐
                  │              Control Plane                      │
                  │   /api/admin/ops-metrics/{current,timeseries,fleet} │
                  │   /api/admin/diag-events/{list,groups,crash-cohorts}│
                  │   /api/admin/agents/:id/diagnostic-mode             │
                  │   /api/admin/observability/retention                │
                  └─────────────┬────────────────────────────────────┘
                                │
                                ▼
                  ┌────────────────────────────────────────────────┐
                  │             Control Plane UI                    │
                  │   Status Overview (rewired)                     │
                  │   Infrastructure → Errors / Crashes / Diag Mode │
                  │   Nodes detail → Metrics tab + Logs tab         │
                  │   Settings → Observability → Retention          │
                  └────────────────────────────────────────────────┘
```

### 4.1 Three Layers of Data (per Thing)

| Layer | Content | Storage | Write cadence |
|-------|---------|---------|---------------|
| **L1 — Runtime (Go-process universal)** | goroutines, heap_alloc, heap_sys, gc_pause_p50, gc_count, threads, open_fds, cpu_user, cpu_sys, rss, uptime | `metric_ops_raw` (time series) | Every 30 s |
| **L2 — Static identity** | hostname, primary IP, OS, OS version, kernel, CPU cores, total RAM, service version, build SHA, build time, start time, applied config version | `thing.metadata.staticInfo` JSONB (one row) | Register + restart + config change |
| **L3 — Business (per-service)** | Service-specific counters, gauges, histograms (catalog in §6.3) | `metric_ops_raw` (time series) | Every 30 s |

L2 is intentionally **not** time-series. Hostname does not change at 30 s granularity; storing it 2,880 times per thing per day is waste. L2 lives where it conceptually belongs: the Thing's identity record. The Control Plane Nodes detail page reads `thing.metadata.staticInfo` directly for the identity panel.

---

## 5. Storage Schema

All tables live in the existing Hub Postgres database. Migrations are added via Prisma (`tools/db-migrate/schema.prisma`); Go types are regenerated via `codegen-go.mjs`.

### 5.1 `metric_ops_raw` — 30-second sampling, short retention

```sql
CREATE TABLE metric_ops_raw (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sampled_at      TIMESTAMPTZ NOT NULL,
    thing_id        UUID NOT NULL REFERENCES thing(id) ON DELETE CASCADE,
    thing_type      TEXT NOT NULL,                 -- 'service' | 'agent' (denormalized for filter speed)
    metric_name     TEXT NOT NULL,                 -- 'runtime.heap_alloc_bytes' | 'cache.hits_total' | ...
    metric_kind     TEXT NOT NULL,                 -- 'gauge' | 'counter' | 'histogram'
    dimension_key   TEXT NOT NULL DEFAULT '',      -- low-cardinality label, e.g. 'cache=snapshot_providers'
    value           DOUBLE PRECISION,              -- gauge / counter cumulative; NULL for histogram
    metadata        JSONB,                         -- histogram buckets, etc.
    UNIQUE (sampled_at, thing_id, metric_name, dimension_key)
);

CREATE INDEX idx_ops_raw_thing_time   ON metric_ops_raw (thing_id, sampled_at DESC);
CREATE INDEX idx_ops_raw_metric_time  ON metric_ops_raw (metric_name, sampled_at DESC);
CREATE INDEX idx_ops_raw_type_time    ON metric_ops_raw (thing_type, sampled_at DESC);
```

Default retention: 7 days. The UNIQUE constraint deduplicates redundant samples if a Thing's reconnect causes a re-emit.

### 5.2 `metric_ops_rollup_1h`, `metric_ops_rollup_1d`, `metric_ops_rollup_1mo`

Identical DDL across the three rollup tiers; only the bucket granularity and retention differ.

```sql
CREATE TABLE metric_ops_rollup_1h (
    bucket_start    TIMESTAMPTZ NOT NULL,
    thing_id        UUID,                          -- NULL = aggregated across non-diagnostic agents
    thing_type      TEXT NOT NULL,
    metric_name     TEXT NOT NULL,
    metric_kind     TEXT NOT NULL,
    dimension_key   TEXT NOT NULL DEFAULT '',
    value_avg       DOUBLE PRECISION,              -- gauge average
    value_sum       DOUBLE PRECISION,              -- counter delta or histogram count
    value_min       DOUBLE PRECISION,
    value_max       DOUBLE PRECISION,
    sample_count    INTEGER NOT NULL,
    metadata        JSONB                          -- merged histogram buckets
);

-- A UNIQUE INDEX with COALESCE is used instead of PRIMARY KEY because
-- thing_id is nullable (NULL = fleet aggregate). Postgres PRIMARY KEY
-- requires NOT NULL on every constituent column.
CREATE UNIQUE INDEX uq_ops_rollup_1h
  ON metric_ops_rollup_1h
     (bucket_start,
      COALESCE(thing_id, '00000000-0000-0000-0000-000000000000'::uuid),
      metric_name, dimension_key);

CREATE INDEX idx_ops_1h_metric_time   ON metric_ops_rollup_1h (metric_name, bucket_start DESC);
CREATE INDEX idx_ops_1h_thing_time    ON metric_ops_rollup_1h (thing_id, bucket_start DESC) WHERE thing_id IS NOT NULL;
CREATE INDEX idx_ops_1h_fleet_time    ON metric_ops_rollup_1h (thing_type, bucket_start DESC) WHERE thing_id IS NULL;
```

`metric_ops_rollup_1d` and `metric_ops_rollup_1mo` have the same DDL with different default retention.

`thing_id IS NULL` means "aggregated across all non-diagnostic agents of this thing_type for this bucket". Cluster services (`thing_type='service'`) always have a real `thing_id`; cardinality is bounded (4 services × ≤3 instances).

### 5.3 `thing_diag_event`

```sql
CREATE TABLE thing_diag_event (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    thing_id        UUID NOT NULL REFERENCES thing(id) ON DELETE CASCADE,
    thing_type      TEXT NOT NULL,                 -- denormalized, accepts 'agent' v1, 'service' future
    occurred_at     TIMESTAMPTZ NOT NULL,          -- client clock
    received_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    level           TEXT NOT NULL,                 -- 'fatal' | 'error' | 'warn' | 'info'
    event_type      TEXT NOT NULL,                 -- 'crash' | 'error' | 'watchdog' | 'lifecycle'
    source          TEXT NOT NULL,                 -- slog source, e.g. 'relay' | 'hook' | 'audit'
    message         TEXT NOT NULL,
    message_hash    TEXT NOT NULL,                 -- md5(level + source + first_stack_frame_or_message); for grouping
    attrs           JSONB,
    stack_trace     TEXT,
    repeat_count    INTEGER NOT NULL DEFAULT 1,    -- client-side dedup count
    agent_version   TEXT,
    os_info         JSONB
);

CREATE INDEX idx_diag_thing_time     ON thing_diag_event (thing_id, occurred_at DESC);
CREATE INDEX idx_diag_level_time     ON thing_diag_event (level, occurred_at DESC) WHERE level IN ('error','fatal');
CREATE INDEX idx_diag_type_time      ON thing_diag_event (event_type, occurred_at DESC);
CREATE INDEX idx_diag_msg_hash_time  ON thing_diag_event (message_hash, occurred_at DESC);
CREATE INDEX idx_diag_crash_cohort   ON thing_diag_event (agent_version, (os_info->>'os'), occurred_at DESC) WHERE event_type = 'crash';
```

### 5.4 `thing_diag_mode_window`

Tracks diagnostic mode windows so the rollup job can decide which agents get per-instance retention vs. fleet aggregation.

```sql
CREATE TABLE thing_diag_mode_window (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    thing_id    UUID NOT NULL REFERENCES thing(id) ON DELETE CASCADE,
    started_at  TIMESTAMPTZ NOT NULL,
    ended_at    TIMESTAMPTZ NOT NULL,              -- bounded; max 24 h from started_at
    set_by      UUID,                              -- admin user id
    reason      TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_diag_window_thing   ON thing_diag_mode_window (thing_id, started_at DESC);
CREATE INDEX idx_diag_window_active  ON thing_diag_mode_window (ended_at) WHERE ended_at > NOW();
```

A new admin POST request closes any prior overlapping window for that thing and inserts a new row. `thing.metadata.diagModeUntil` is set to the new `ended_at` for shadow propagation (see §8).

### 5.5 `metric_ops_retention_config`

```sql
CREATE TABLE metric_ops_retention_config (
    layer            TEXT PRIMARY KEY,
    retention_days   INTEGER NOT NULL CHECK (retention_days >= 1),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by       UUID
);
```

Seeded rows:

| `layer` | Default | Allowed range | Used by |
|---------|---------|---------------|---------|
| `runtime_raw` | 7 | 1 – 30 | `metric_ops_raw` rows where `metric_name LIKE 'runtime.%'` |
| `business_raw` | 7 | 1 – 30 | `metric_ops_raw` rows where `metric_name NOT LIKE 'runtime.%'` |
| `runtime_1h` | 90 | 30 – 365 | `metric_ops_rollup_1h` rows where `metric_name LIKE 'runtime.%'` |
| `business_1h` | 90 | 30 – 365 | `metric_ops_rollup_1h` rows where `metric_name NOT LIKE 'runtime.%'` |
| `runtime_1d` | 365 | 90 – 1095 | `metric_ops_rollup_1d` |
| `runtime_1mo` | 1825 | 365 – 3650 | `metric_ops_rollup_1mo` |
| `business_1d` | 365 | 90 – 1095 | `metric_ops_rollup_1d` |
| `business_1mo` | 1825 | 365 – 3650 | `metric_ops_rollup_1mo` |
| `diag_warn` | 30 | 7 – 90 | `thing_diag_event` rows where `level='warn'` |
| `diag_error` | 180 | 30 – 730 | `thing_diag_event` rows where `level='error'` |
| `diag_fatal` | 365 | 90 – 1825 | `thing_diag_event` rows where `level='fatal'` |

Eleven keys total. `ops-retention` job re-reads this table every run; no Hub restart required after a change.

### 5.6 `thing.metadata.staticInfo` JSONB layout

Not a new table — a documented sub-object on the existing `thing.metadata` field. All Things populate this on register and on restart.

```json
{
  "deviceTokenHash": "...",
  "enrollmentTokenId": "...",
  "diagModeUntil": "2026-04-27T18:00:00Z",
  "staticInfo": {
    "hostname": "ag-pod-1.cluster.local",
    "primaryIp": "10.0.0.5",
    "os": "linux",
    "osVersion": "Ubuntu 22.04",
    "kernelVersion": "5.15.0-100-generic",
    "cpuCores": 8,
    "totalRamBytes": 16777216000,
    "serviceVersion": "v1.4.2",
    "buildSha": "73cd87b3...",
    "buildTime": "2026-04-25T10:00:00Z",
    "startTime": "2026-04-26T08:00:00Z",
    "configVersionApplied": 42
  }
}
```

The `staticInfo` sub-object is namespaced to avoid colliding with existing well-known keys (`deviceTokenHash`, `enrollmentTokenId`, `diagModeUntil`).

---

## 6. Metrics Catalog

### 6.1 L1 — Runtime (universal across all 5 Thing types)

| `metric_name` | `metric_kind` | Source |
|---------------|---------------|--------|
| `runtime.goroutines` | gauge | `runtime.NumGoroutine()` |
| `runtime.heap_alloc_bytes` | gauge | `runtime.MemStats.HeapAlloc` |
| `runtime.heap_sys_bytes` | gauge | `runtime.MemStats.HeapSys` |
| `runtime.gc_pause_p50_ms` | gauge | `runtime.MemStats.PauseNs` p50 over the last 256 samples |
| `runtime.gc_count_total` | counter | `runtime.MemStats.NumGC` |
| `runtime.threads` | gauge | `pprof.Lookup("threadcreate").Count()` |
| `runtime.open_fds` | gauge | `/proc/self/fd` count (Linux); equivalent on macOS / Windows |
| `runtime.cpu_user_seconds_total` | counter | `getrusage(RUSAGE_SELF).ru_utime` |
| `runtime.cpu_system_seconds_total` | counter | `getrusage(RUSAGE_SELF).ru_stime` |
| `runtime.rss_bytes` | gauge | `process_resident_memory_bytes` (from `prometheus/client_golang`'s process collector) |
| `runtime.uptime_seconds` | gauge | `now - process_start_time` |

A single sampler in `packages/shared/runtime/opsmetrics/runtime.go` captures all eleven metrics in one pass.

### 6.2 L2 — Static identity (in `thing.metadata.staticInfo`)

See §5.6 for the JSON shape. Captured by `packages/shared/runtime/opsmetrics/staticinfo.go` at startup; pushed via `thingclient.UpdateStaticInfo()` (which writes to the Hub via the existing register / re-register paths).

### 6.3 L3 — Business metrics (per Thing type)

Each service registers its L3 metrics with `packages/shared/runtime/opsmetrics/registry.go` at startup. The sampler iterates the registry every 30 s.

#### Hub
```
things.connected{type=service|agent}                gauge
things.total{status=online|offline|degraded}        gauge
ws.messages_total{direction=in|out, type}           counter
ws.reconnects_total                                 counter
jobs.runs_total{name, status=success|failed}        counter
jobs.duration_ms{name}                              histogram
jobs.last_run_seconds{name}                         gauge
mq.lag_messages{stream}                             gauge
mq.processed_total{stream, status}                  counter
db.pool{state=in_use|idle|waiting}                  gauge
db.query_ms                                         histogram
shadow.drift_things                                 gauge
enrollment.total{result}                            counter
ca.certs_issued_total                               counter
alerts.dispatched_total{channel, severity}          counter
metrics.dropped_total{reason}                       counter (self-emitted; covers backpressure drops)
```

#### Control Plane
```
http.requests_total{method, route_class, status_class}  counter
http.duration_ms{route_class}                            histogram
auth.attempts_total{result, method=password|sso|apikey}  counter
iam.eval_total{decision=allow|deny, cache=hit|miss}      counter
sessions.active                                          gauge
hub_api.calls_total{op, status}                          counter
hub_api.duration_ms                                      histogram
db.pool{state}                                           gauge
db.query_ms                                              histogram
audit.published_total{result}                            counter
```

`route_class` is the route template stripped of path parameters (e.g. `admin.users.detail`, not `/admin/users/u_abc123`) to keep label cardinality bounded.

#### AI Gateway
```
cache.hits_total{cache}                            counter
cache.misses_total{cache}                          counter
cache.size{cache}                                  gauge
cache.invalidations_total{cache, source}           counter
streams.active{format}                             gauge
provider.call_ms{provider, endpoint, result_class} histogram
quota.checks_total{result=allow|deny|error}        counter
routing.decisions_total{result=primary|fallback|rule_hit}  counter
hook.pipeline_ms{stage=request|response}           histogram
vk.lookups_total{result=hit|miss|expired|invalid}  counter
codec.calls_total{from_format, to_format, surface=S1|S2|S3|S4|S5}  counter
db.pool{state}                                     gauge
db.query_ms                                        histogram
redis.ops_total{op, result}                        counter
```

Per-request counts and tokens stay in `traffic_event` and the existing `metric_rollup_*` pipeline; this layer covers infrastructure-level signals only.

#### Compliance Proxy
```
tunnels.active                                     gauge
tunnels.total{result=BUMP_SUCCESS|BUMP_FAILED_PASSTHROUGH|...}  counter
tls.handshake_ms                                   histogram
cert_cache.hits_total                              counter
cert_cache.misses_total                            counter
cert_cache.size                                    gauge
hook.pipeline_ms                                   histogram
buffer.bytes_active                                gauge
buffer.bytes_peak                                  gauge
streaming.sessions{mode=passthrough|live|buffer}   gauge
killswitch.active                                  gauge   -- 0 | 1
bytes_proxied_total{direction=req|resp}            counter
hook.decisions_total{decision}                     counter
```

#### Agent
```
interception.state                                 gauge   -- 0 disabled | 1 partial | 2 enabled
connections.active                                 gauge
requests.total{action=intercept|relay|reject}      counter
relay.dial_total{mode=new|reused}                  counter
relay.handshake_total                              counter
hook.decisions_total{decision}                     counter
audit.queue_depth                                  gauge
audit.uploads_total{result=ok|fail}                counter
audit.last_upload_seconds                          gauge
ws.connected                                       gauge
ws.reconnects_total                                counter
config.apply_total{key, result}                    counter
cert_pin.events_total{event=detected|exempted}     counter
local_db.bytes                                     gauge
diag.dropped_total                                 counter   -- in-process buffer overflows
```

### 6.4 Histogram bucket layout

Same buckets as the existing AI traffic histograms (consistent UI percentile interpolation across the platform):

```
[0,50ms) [50,100ms) [100,200ms) [200,500ms) [500,1000ms) [1000ms,+∞)
```

`metadata`: `{"buckets": [c0, c1, c2, c3, c4, c5]}`. Bucket counts add element-wise during rollup merges.

---

## 7. Transport

### 7.1 `metrics_sample` WebSocket message

Emitted every 30 s on the existing thingclient heartbeat tick. One message per Thing per tick contains all L1 and L3 samples for that interval.

```json
{
  "type": "metrics_sample",
  "thingId": "uuid",
  "sampledAt": "2026-04-27T10:00:00Z",
  "samples": [
    { "name": "runtime.heap_alloc_bytes", "kind": "gauge", "dim": "", "value": 12345678 },
    { "name": "relay.dial_total",         "kind": "counter", "dim": "mode=new", "value": 42 },
    { "name": "hook.pipeline_ms",         "kind": "histogram", "dim": "stage=request",
      "metadata": { "buckets": [10, 5, 2, 1, 0, 0] } }
  ]
}
```

`dim` is the `dimension_key` column. When the agent is **not** in diagnostic mode, no per-instance label is added; the row's `thing_id` already disambiguates within `metric_ops_raw`. The fleet aggregation happens during rollup, not at write.

### 7.2 `diag_event` WebSocket message

Emitted in real time as a slog ERROR fires on the agent. Each event carries client-side dedup state so a single repeated error does not flood the WebSocket.

```json
{
  "type": "diag_event",
  "thingId": "uuid",
  "occurredAt": "2026-04-27T10:00:00Z",
  "level": "error",
  "eventType": "error",
  "source": "relay",
  "message": "dial to upstream failed",
  "messageHash": "9a8f...",
  "attrs": { "upstream": "api.openai.com:443", "requestId": "abc" },
  "stackTrace": null,
  "repeatCount": 1,
  "agentVersion": "v1.4.2",
  "osInfo": { "os": "darwin", "version": "14.4" }
}
```

Client-side dedup: the agent maintains an in-process LRU keyed by `messageHash` for a 60 s window. The first occurrence is sent immediately with `repeatCount=1`; subsequent occurrences within the window increment a counter; when the window expires, a "summary" event is sent with the final `repeatCount`. This caps the message rate at roughly one per unique error per minute.

### 7.3 Crash safety path (Agent only)

```go
// packages/agent/cmd/agent/main.go — outermost defer
defer func() {
    if r := recover(); r != nil {
        evt := DiagEvent{
            Level:        "fatal",
            EventType:    "crash",
            Source:       "main",
            Message:      fmt.Sprintf("%v", r),
            StackTrace:   string(debug.Stack()),
            OccurredAt:   time.Now().UTC(),
            AgentVersion: buildinfo.Version,
            OSInfo:       sysinfo.OSInfo(),
        }
        _ = localBuffer.Insert(evt) // SQLCipher synchronous write; best-effort
        panic(r)                    // re-panic so the OS crash reporter still fires
    }
}()
```

Local buffer table (in the agent's existing SQLCipher local DB):

```sql
CREATE TABLE pending_diag_event (
    id            BLOB PRIMARY KEY,    -- UUID
    occurred_at   TEXT NOT NULL,
    payload       BLOB NOT NULL,       -- the full JSON envelope
    attempts      INTEGER NOT NULL DEFAULT 0
);
```

On next agent startup, before the WebSocket connects:

1. Query `pending_diag_event` for any rows.
2. If non-empty, batch-POST them to `https://hub/api/internal/things/diag-events:batch` over mTLS with the device token.
3. Hub returns `{"acceptedIds": [...]}`. Delete those rows from local.
4. Rows with `attempts >= 5` are still retained (operator can pull from the local DB if Hub is permanently unreachable) but are rate-limited.

The HTTP path is used (not WebSocket) because:
- WebSocket handshake takes longer than HTTP and may fail on cold network.
- The HTTP path is already battle-tested for agent audit upload (`/api/internal/things/agent-audit`); the same partial-ack pattern applies.

### 7.4 WebSocket disconnect handling

| Stream | Behavior on WS disconnect |
|--------|---------------------------|
| `metrics_sample` | Skip the lost interval. Counters keep accumulating in-process; the next successful sample carries the latest cumulative value. Gauges resample fresh. **At-most-once.** |
| `diag_event` (non-fatal) | In-process ring buffer (100 events / 5 min cap). Flush on reconnect. Overflow drops increment `diag.dropped_total`. **At-most-once with bounded buffer.** |
| `diag_event` (fatal/crash) | Local SQLCipher buffer (unbounded by design — process is dying). Drained over HTTP on next startup. **At-least-once.** |

No NATS / no JetStream / no offset replay. The three streams have three different durability guarantees, intentionally.

### 7.5 Hub-side write path and backpressure

```
ws.OnMessage("metrics_sample") → metricsWriter chan(10000 batches) → COPY metric_ops_raw
ws.OnMessage("diag_event")     → diagWriter    chan(10000 events)  → INSERT thing_diag_event
http POST /diag-events:batch   → diagWriter.InsertBatch + ack acceptedIds
```

`metricsWriter` batches up to 1,000 samples or 200 ms, whichever first, and uses `pgx.CopyFrom` for the bulk insert. `diagWriter` batches up to 100 events or 100 ms.

Channel overflow drops the new payload and increments `nexus_hub_metrics_dropped_total{reason=overflow}` (which itself is reported via the same `metrics_sample` pipeline — Hub samples its own counters and writes them to `metric_ops_raw`, exactly like every other Thing).

---

## 8. Diagnostic Mode

### 8.1 Admin operation

```
POST   /api/admin/agents/:thing_id/diagnostic-mode    { "until": "2026-04-27T18:00:00Z" }
DELETE /api/admin/agents/:thing_id/diagnostic-mode
GET    /api/admin/agents/diagnostic-mode               (list active windows)
POST   /api/admin/agents/diagnostic-mode/bulk          { "filter": {...}, "until": "..." }
```

`until` is bounded at 24 h from `now`. The Recent Errors page also exposes a one-click "enable diagnostic mode for the affected agents" action.

### 8.2 Server-side handling

On enable:

1. Insert a row into `thing_diag_mode_window` with `started_at = NOW()`, `ended_at = until`.
2. Set `thing.metadata.diagModeUntil = until` for shadow propagation.
3. Push the shadow update via the existing Category A pipeline. The agent's `OnConfigChanged` callback for `staticInfo`-adjacent metadata reads the new `diagModeUntil` and adjusts behavior immediately.

On disable:

1. Set `ended_at = NOW()` for the active window row.
2. Clear `thing.metadata.diagModeUntil`.
3. Push shadow update.

Auto-expiry: the existing Hub scheduler runs a `diag-mode-expiry` job every minute that closes any `thing_diag_mode_window` rows whose `ended_at <= NOW()` and clears the corresponding `thing.metadata.diagModeUntil` value.

### 8.3 Agent-side behavior in diagnostic mode

When `now < diagModeUntil`:

- Sampling cadence is unchanged (still 30 s — the rollup, not the agent, controls cardinality).
- INFO-level lifecycle events (startup / shutdown / version upgrade / config apply) are emitted as `diag_event`. Outside diagnostic mode they are suppressed.

The agent does **not** label its own samples as "in diagnostic mode". The diagnostic determination is server-side: `metric_ops_raw` always carries the real `thing_id`; the rollup job is responsible for collapsing or preserving identity.

### 8.4 Rollup-time aggregation for agents

The `ops-rollup-1h` job's algorithm for `thing_type='agent'` rows:

```
For each sealed hour H:
  diagWindows := SELECT thing_id FROM thing_diag_mode_window
                 WHERE ended_at > H AND started_at < H + interval '1 hour'

  -- Per-thing rollup for agents in diag mode during this hour
  INSERT INTO metric_ops_rollup_1h
    SELECT date_trunc('hour', sampled_at), thing_id, 'agent', metric_name, metric_kind, dimension_key,
           AVG(value), SUM(value), MIN(value), MAX(value), COUNT(*),
           merge_metadata(metadata)
      FROM metric_ops_raw
     WHERE sampled_at >= H AND sampled_at < H + interval '1 hour'
       AND thing_type = 'agent'
       AND thing_id IN (SELECT thing_id FROM diagWindows)
     GROUP BY 1, 2, 4, 5, 6;

  -- Fleet aggregate for agents NOT in diag mode during this hour
  INSERT INTO metric_ops_rollup_1h
    SELECT date_trunc('hour', sampled_at), NULL, 'agent', metric_name, metric_kind, dimension_key,
           AVG(value), SUM(value), MIN(value), MAX(value), COUNT(*),
           merge_fleet_metadata(metadata)
      FROM metric_ops_raw
     WHERE sampled_at >= H AND sampled_at < H + interval '1 hour'
       AND thing_type = 'agent'
       AND thing_id NOT IN (SELECT thing_id FROM diagWindows)
     GROUP BY 1, 4, 5, 6;
```

For `thing_type='service'`:

```
INSERT INTO metric_ops_rollup_1h
  SELECT date_trunc('hour', sampled_at), thing_id, 'service', metric_name, metric_kind, dimension_key,
         AVG(value), SUM(value), MIN(value), MAX(value), COUNT(*),
         merge_metadata(metadata)
    FROM metric_ops_raw
   WHERE sampled_at >= H AND sampled_at < H + interval '1 hour'
     AND thing_type = 'service'
   GROUP BY 1, 2, 4, 5, 6;
```

The two operations (DELETE existing rows for bucket H, then INSERT) run in a single transaction for idempotency, identical to the AI traffic rollup pattern.

### 8.5 Operator UX implications

- **Real-time / last 7 days**: any agent's data is queryable from `metric_ops_raw`.
- **Beyond 7 days**: only fleet trend is queryable for agents that were **not** in diagnostic mode at the time. To preserve a specific agent's history past the raw retention, operators enable diagnostic mode before the time window of interest.
- Cluster services: always per-instance, all retention tiers.

---

## 9. Rollup Pipeline

| Job | Cadence | Input | Output |
|-----|---------|-------|--------|
| `ops-rollup-1h` | 5 min | sealed hours from `metric_ops_raw` | `metric_ops_rollup_1h` |
| `ops-rollup-1d` | 1 h | sealed days from `metric_ops_rollup_1h` | `metric_ops_rollup_1d` |
| `ops-rollup-1mo` | 24 h | sealed months from `metric_ops_rollup_1d` | `metric_ops_rollup_1mo` |
| `ops-retention` | 24 h | — | DELETE expired rows in `metric_ops_raw`, all three rollup tables, and `thing_diag_event`, per `metric_ops_retention_config` |
| `diag-mode-expiry` | 1 min | active windows past `ended_at` | UPDATE `thing.metadata.diagModeUntil` to NULL; close window |

Watermark table (existing pattern from AI traffic rollup):

```sql
INSERT INTO rollup_watermark (name, watermark) VALUES
  ('ops_1h', '1970-01-01'::timestamptz),
  ('ops_1d', '1970-01-01'::timestamptz),
  ('ops_1mo', '1970-01-01'::timestamptz);
```

Each job reads its watermark, finds sealed buckets `(watermark, NOW() - 1 bucket]`, processes them in transactions, advances the watermark.

Failure modes:

| Failure | Recovery |
|---------|----------|
| Job crash mid-transaction | Tx rolls back, watermark unchanged, next tick retries. |
| Postgres unavailable | Job skips this tick, alerts via existing scheduler health metric. |
| Late-arriving data | The 1h job processes only `bucket_start + 1h <= NOW()`, so late samples within the same hour are absorbed at the next tick. Beyond 1 h late, the operator can manually trigger a rebuild; no automated correction job is included in v1 (unlike traffic rollup, where compliance-grade exactness justifies the daily T-1 recompute). |

The lack of an `ops-rollup-correction` job is a deliberate scope decision: operational metrics tolerate small inaccuracies; the cost of a daily full recompute is not justified by the ops use case.

---

## 10. Control Plane API

All endpoints require the `admin:ReadObservability` permission for reads, `admin:WriteObservability` for mutations. Read endpoints use pgx directly against the Hub Postgres instance (the same DB Control Plane already reads for traffic_event analytics; no new connection).

### 10.1 Metrics

```
GET /api/admin/ops-metrics/current
    ?thingType=service|agent
    &thingId={uuid}                       (optional filter)
    → latest sample per (thing_id, metric_name, dimension_key) within last 90 s

GET /api/admin/ops-metrics/timeseries
    ?thingId={uuid}
    &metric={metric_name}
    &dim={dimension_key}                  (optional)
    &from={iso}&to={iso}
    &granularity=auto|raw|1h|1d|1mo
    → time-bucketed rows; granularity=auto picks per existing rule
      (≤6h→raw, 6h–7d→1h, 7d–90d→1d, >90d→1mo)

GET /api/admin/ops-metrics/fleet
    ?thingType=agent
    &metric={metric_name}
    &from={iso}&to={iso}
    → fleet-aggregate row per bucket (thing_id IS NULL slice)
```

### 10.2 Diagnostic events

```
GET /api/admin/diag-events
    ?thingId=&level=&source=&from=&to=&q=&limit=&cursor=
    → paginated list, newest first

GET /api/admin/diag-events/groups
    ?from=&to=&thingType=
    → grouped by message_hash, with affectedThings count and totalOccurrences

GET /api/admin/diag-events/crash-cohorts
    ?from=&to=
    → FATAL events grouped by (agentVersion, os, osVersion);
      one row per cohort with count and affectedThings
```

### 10.3 Diagnostic mode

```
POST   /api/admin/agents/:thingId/diagnostic-mode      { "until": "...", "reason": "..." }
DELETE /api/admin/agents/:thingId/diagnostic-mode
GET    /api/admin/agents/diagnostic-mode               → active windows list
POST   /api/admin/agents/diagnostic-mode/bulk
       { "filter": { "agentVersion": "...", "os": "...", "thingIds": [...] },
         "until": "...", "reason": "..." }
```

Bulk `filter` accepts any combination of attribute filters or a literal `thingIds` array. Max 500 things per bulk call.

### 10.4 Retention configuration

```
GET /api/admin/observability/retention
    → all 11 layers with current values and allowed ranges

PUT /api/admin/observability/retention
    { "runtime_raw": 7, "business_raw": 30, ... }
    → atomic update of one or more layers; validates against allowed ranges
```

### 10.5 Removed endpoints

```
GET    /api/admin/service-metrics       — DELETED in v1
```

`promparse.go` is removed entirely. The Status Overview tab calls `/api/admin/ops-metrics/current` instead. Other endpoints unrelated to metrics aggregation (e.g. node lifecycle management) are out of scope and unchanged.

---

## 11. Control Plane UI

All five new pages and the Nodes detail tabs ship in v1. No deferral.

### 11.1 Status Overview tab — rewired (existing page)

Visual layout unchanged. Backend data source for service metrics cards switches from `/api/admin/service-metrics` to `/api/admin/ops-metrics/current`. The cards iterate over Things with `thing_type='service'` and aggregate by `thing.metadata.staticInfo.role` (or service identity).

A new "Recent Errors" mini-widget is added below the service metrics cards: shows up to 5 latest ERROR/FATAL events across the fleet with a "View all" link to the Recent Errors page.

### 11.2 Infrastructure → Recent Errors (new page)

Layout:

```
[ filter bar: time range | level | source | thing type | search ]

┌────── Top groups (24h) ──────────────────────────────────┐
│ msg_hash (truncated)              affected   total count │
│ ---                               ---        ---         │
│ "dial to upstream failed"         12 things  1,420       │
│ "hook eval timeout"               5 things   78          │
│ ...                                                       │
└──────────────────────────────────────────────────────────┘

┌────── Recent stream ─────────────────────────────────────┐
│ time | level | thing | source | message    | actions     │
│ ...                                                        │
└──────────────────────────────────────────────────────────┘
```

Click a group → expand to affected Things list. Click a row → modal with full event detail (attrs, stack trace).

A row's "actions" column includes "Enable diag mode for this agent" (single-click, default 1 h window) when the affected thing is an agent.

### 11.3 Infrastructure → Crash Reports (new page)

Filtered subset of Recent Errors: `event_type = 'crash'`. Default view groups by `(agentVersion, os, osVersion)`:

```
agentVersion   os         osVersion   crashes   affectedThings
v1.4.2         darwin     14.4        12        8
v1.4.1         linux      Ubuntu 22.04 3        3
...
```

Click a cohort → list of crash events; click an event → modal with full stack trace and attrs.

### 11.4 Infrastructure → Agent Diag Mode (new page)

```
┌── Active windows ──────────────────────────────────────┐
│ thing | started | ends in | set by | reason | actions  │
└────────────────────────────────────────────────────────┘

┌── Enable on agents ────────────────────────────────────┐
│ Filter: [ version | os | thingIds | tag ]              │
│ Window: [ 1h | 4h | 12h | 24h ]                        │
│ Reason: [ ____________________________ ]               │
│                                            [ Enable ]   │
└────────────────────────────────────────────────────────┘
```

Bulk enable button is disabled until the filter resolves to ≤ 500 agents. The page polls `/api/admin/agents/diagnostic-mode` every 10 s for the active windows list.

### 11.5 Settings → Observability → Retention (new page)

Eleven controls in three groups:

- **Operational metrics — Runtime** (4 sliders: raw / 1h / 1d / 1mo days)
- **Operational metrics — Business** (4 sliders)
- **Diagnostic events** (3 sliders: warn / error / fatal days)

Each control shows the current value, default value, and allowed range. Save button applies all changed controls atomically. The next `ops-retention` run picks up the new values.

### 11.6 Nodes detail page — Metrics tab (new tab)

Available on `Infrastructure → Nodes → [thing_id]` for any thing type.

```
[ time range: 1h | 6h | 1d | 7d | 30d | 1y ]   (auto-disable per retention)

┌── Runtime ──────────────────────────────────────────────┐
│ heap_alloc · goroutines · gc_pause_p50 · cpu · uptime   │
│ (small-multiples line charts)                            │
└─────────────────────────────────────────────────────────┘

┌── Business ─────────────────────────────────────────────┐
│ (per-service set of charts; the catalog from §6.3)      │
└─────────────────────────────────────────────────────────┘
```

For agents not in diagnostic mode, ranges beyond 7 days display only fleet-aggregate data (not the specific agent). The UI shows an info banner: "Beyond 7 days, only fleet trend is preserved. Enable diagnostic mode to retain per-agent history."

### 11.7 Nodes detail page — Logs tab (new tab)

```
[ filter: level | source | search ]
[ time range — synced with Metrics tab via shared time-state in URL query ]

time     level   event_type   source   message               attrs/stack
---      ---     ---          ---      ---                   ---
14:32:01 error   error        relay    dial to upstream...   [view]
14:31:50 fatal   crash        main     runtime error...      [view]
```

Click a row → side panel with full attrs JSON and stack trace.

### 11.8 Cross-tab time axis sync

The Metrics tab and Logs tab both read `?from=` and `?to=` from the URL query string. Zooming or panning on a chart in the Metrics tab updates these query params; switching to the Logs tab inherits them. The same applies in reverse.

This is the explicit "easy correlation" UX requirement from the brainstorm: an operator who spots a heap spike at 14:32 immediately sees the relevant ERROR events at the same time.

---

## 12. Migration

Per CLAUDE.md "no backward compatibility" rule (pre-GA), this is a hard cutover within a single change set:

1. Add the new Postgres tables, retention seed, and watermark seed (Prisma migration).
2. Implement `packages/shared/runtime/opsmetrics/` (sampler, registry, runtime collector, static info reporter).
3. Wire each cluster service binary (Hub, CP, AI Gateway, Compliance Proxy) to register its L3 metrics with the shared registry and to call `thingclient.PushMetricsSample()` on the heartbeat tick.
4. Wire the agent: same registration path; additionally implement panic recovery, slog ERROR sink, SQLCipher local crash buffer, and HTTP drain on startup.
5. Implement Hub's `metricsWriter`, `diagWriter`, four scheduler jobs, and the ws message handlers.
6. Implement Control Plane's pgx-backed read endpoints.
7. Implement the Control Plane UI changes.
8. **Delete** `packages/control-plane/internal/handler/promparse.go`, the `/api/admin/service-metrics` route, and the `promparse_test.go` file.
9. **Delete** any dead UI code that called the removed endpoints.

There is no flag, no parallel pipeline, no deprecation period.

---

## 13. Testing

### 13.1 Unit

- `opsmetrics/runtime.go`: capture tests against a fixture process (mock `runtime.MemStats`).
- `opsmetrics/registry.go`: registration / iteration / histogram bucket math.
- Hub `metricsWriter` / `diagWriter`: backpressure under channel saturation (drop counter increments, no panic).
- Rollup job algorithm: agent diagnostic-mode partitioning (per-thing vs. fleet aggregation) under varying `thing_diag_mode_window` rows.
- Retention job: respect each layer's configured `retention_days`, do not delete rows in active retention.
- Client-side dedup: 100 rapid identical events produce 1 send + 1 summary.

### 13.2 Integration

- End-to-end agent → Hub → Postgres → Control Plane API for both `metrics_sample` and `diag_event`.
- Crash path: kill the agent process during operation, restart, assert pending events drain via HTTP and reach Postgres.
- Diagnostic mode: enable for a thing, confirm subsequent rollup buckets contain a per-thing row; disable, confirm subsequent buckets fold into fleet aggregate.
- Retention: configure a layer to 1 day, run retention job, assert older rows deleted while in-window rows survive.
- Status Overview rewired path: assert the page renders correct metrics from `metric_ops_raw` after promparse removal.

### 13.3 Load

- 10,000 agents × 30 s sampler × 30 metrics each → 10,000 samples / s on Hub. Hub `pgx.CopyFrom` with batch size 1,000 should sustain this on a single Postgres instance with ample headroom.
- Storm test: 1,000 concurrent diag_event emissions over WebSocket; Hub channel overflow path should drop gracefully and increment the dropped counter without disconnecting Things.

---

## 14. Operational Considerations

### 14.1 Dimension cardinality

`dimension_key` cardinality is bounded at design time. The largest contributors:

- AI Gateway `provider.call_ms{provider, endpoint, result_class}`: ~9 providers × 4 endpoints × 4 result classes = 144 combinations.
- Control Plane `http.requests_total{method, route_class, status_class}`: ~7 methods × ~50 route classes × 3 status classes = ~1,000 combinations.
- Agent: ~30 effective `(metric_name × dimension_key)` combinations per thing (low — agents have fewer label dimensions than service infrastructure).

In `metric_ops_rollup_*` the agent contribution collapses to one fleet row per `(metric_name, dimension_key, bucket)` whenever the agent is not in diagnostic mode — bounded at ~30 rows per bucket regardless of fleet size. This is the cardinality reason for the diagnostic-mode design.

### 14.2 Rollup lag SLO

- `metric_ops_rollup_1h` should always be within 10 minutes of wall clock. `ops-rollup-1h` runs every 5 min.
- Hub exposes `nexus_hub_rollup_lag_seconds{layer="ops_1h"}` so the operator can alert on lag.

### 14.3 Storage estimates

`metric_ops_raw` is per-thing for **all** Things including agents (the diagnostic-mode collapse happens at rollup time, not at write time — see §3 Decision #2 and §8.4). Volume is therefore dominated by agent count.

Two reference deployment sizes:

**Small/medium fleet (~100 agents):**

| Table | Rows / day | Default retention | Disk (rough) |
|-------|------------|-------------------|--------------|
| `metric_ops_raw` | ~10 M (12 services × 1,150 dim × 2880 buckets + 100 agents × 30 dim × 2880 buckets) | 7 d | ~6 GB |
| `metric_ops_rollup_1h` | ~50 K | 90 d | ~500 MB |
| `metric_ops_rollup_1d` | ~2 K | 365 d | ~80 MB |
| `metric_ops_rollup_1mo` | ~70 | 1825 d | <10 MB |
| `thing_diag_event` | ~10 K (healthy operation) | mixed (180–365 d) | ~5 GB |

**Large fleet (~10,000 agents):**

| Table | Rows / day | Default retention | Disk (rough) |
|-------|------------|-------------------|--------------|
| `metric_ops_raw` | ~900 M (12 services × 1,150 dim × 2880 buckets + 10,000 agents × 30 dim × 2880 buckets) | **lower to 1 d** | ~70 GB |
| `metric_ops_rollup_1h` | ~50 K (agent collapses to fleet) | 90 d | ~500 MB |
| `metric_ops_rollup_1d` | ~2 K | 365 d | ~80 MB |
| `metric_ops_rollup_1mo` | ~70 | 1825 d | <10 MB |
| `thing_diag_event` | ~100 K (healthy operation) | mixed (180–365 d) | ~50 GB |

**Operator guidance for >1,000 agents:**

- Lower `runtime_raw` and `business_raw` from the 7-day default to 1–2 days via the Settings → Observability → Retention page. Per-agent history beyond that window is preserved by enabling diagnostic mode for the specific agent before the time window of interest.
- `metric_ops_raw` should be **range-partitioned by `sampled_at`** (daily partitions). The retention job becomes `DROP PARTITION` instead of `DELETE` — instantaneous, no bloat. Partitioning is **not** included in v1 schema migrations but is the recommended operational mitigation; `tools/db-migrate/` can ship a partitioning migration as a follow-up when a customer crosses the threshold.
- The rollup tables are tiny in absolute size and never need partitioning.

`metric_ops_raw` is the dominant storage cost. Operators may lower `runtime_raw` and `business_raw` toward 3 days to halve storage if needed.

### 14.4 Self-monitoring

Hub samples its own opsmetrics (including `mq.lag_messages`, `metrics.dropped_total`, rollup lag) the same way every other Thing does. There is no separate "Hub metrics" pipeline; the loop is closed.

---

## 15. Out of Scope (explicit)

- **Cluster service `diag_event` emission.** Schema, Hub handler, and CP UI accept `thing_type='service'` events; no service binary emits in v1. Adding emission later is a slog handler in each service `cmd/<service>/main.go`.
- **`traffic_event` and `metric_rollup_*`.** Untouched.
- **Tracing.** No OpenTelemetry traces. Operators correlate via the metrics-tab / logs-tab time axis, not span IDs.
- **External alerting integration.** A diag event with `level='fatal'` could fire an alert via the existing `/api/v1/alerts/raise` raiser, but that wiring is a follow-up.
- **Agent local file logs.** Agents may continue to write platform-native logs locally; this spec does not collect them.
- **Loki / ELK / Grafana.** All observability in v1 is rendered by the Control Plane UI from Postgres.

---

## 16. Follow-Ups

1. Wire each cluster service's slog ERROR / FATAL handler to emit `diag_event` so the existing UI surfaces also cover service errors.
2. Connect `thing_diag_event` (level='fatal') to the Hub Unified Alerting raiser as an automatic alert producer with a configurable per-cohort threshold.
3. Add per-Thing `notes` field on the Logs tab so operators can pin observations during an incident.
4. Add CSV export on Recent Errors and Crash Reports for postmortem evidence.

---

## 17. References

- `docs/dev/architecture.md` — system overview, Thing model context.
- `docs/dev/service-call-framework.md` — Thing Registry, WebSocket protocol, MQ boundaries (specifically §17 which this spec reverses).
- `docs/dev/thing-model.md` — Thing data model (`thing` core table, `thing_service`, `thing_agent`, `metadata` JSONB conventions).
- `docs/dev/metrics-rollup-architecture.md` — AI traffic rollup pipeline (the watermark + sealed-bucket pattern reused here).
- `docs/superpowers/specs/2026-04-15-status-page-metrics-design.md` — superseded for the backend; UI layout principles carry over.
- `docs/superpowers/specs/2026-04-26-agent-transport-rewrite-design.md` — origin of the unobservable `relay.*` agent metrics motivating this work.
- `docs/superpowers/specs/2026-04-21-unified-alerting-design.md` — alerting raiser path for the Follow-Up #2 integration.
