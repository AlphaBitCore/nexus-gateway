# E50 — Story 4: Agent Backend Phase Instrumentation

## Context

Brings the agent's `traffic_event` rows to parity with compliance-proxy
(structurally the same forwarding proxy, just running on the laptop). Populates
the 5 phase columns, the `agent` slice of `latency_breakdown`
(`intercept_ms`), and extends the local SQLite `audit_events` table + the
rollup tables so the agent desktop UI Stats page (S7) can render phase trends
without re-aggregating from raw rows.

## User Story

**As an** agent end user looking at the desktop UI Stats page,
**I want** the per-destination latency breakdown to show how much was the
agent's own overhead versus the network round-trip,
**so that** when my coding assistant feels slow I can tell instantly whether
to file a bug against Nexus or curse my Wi-Fi.

## Tasks

### 4.1 Wire `PhaseTimer` through agent intercept handler

`packages/agent/core/network/intercept/handler.go` — at flow start, construct
`timer := traffic.NewPhaseTimer()` and thread it to `OnFlowComplete`. Phase
boundaries:

| Checkpoint | Phase |
|---|---|
| Kernel/NE/pf → Go handler entry | `PhaseIntercept` |
| After request hooks pipeline | (aggregated to `request_hooks_ms`) |
| After response inspector / response hooks | (aggregated to `response_hooks_ms`) |

For macOS: the existing `neFlowMsg.DurationMs` reports total flow duration. Split:
the time between `neFlowMsg` delivery and `OnFlowComplete` callback entry is the
intercept overhead. For Linux/Windows: time between `accept()` and handler entry.

### 4.2 Upstream TTFB + total via httptrace on `relayClient.Do`

`packages/agent/core/network/proxy/proxy.go` — wrap `relayClient.Do` (`proxy.go:435`)
with `httptrace.ClientTrace.GotFirstResponseByte`. Same pattern as
ai-gateway S2.2 and compliance-proxy S3.3.

For SSE: the existing `PassthroughWithAccumulator` (`proxy.go:707`) already
inspects `byteCounter`. Add a first-non-zero-bytes hook on the underlying
reader to record `UpstreamTtfbMs`. Record `UpstreamTotalMs` at relay completion.

### 4.3 Response hooks timing (new — was uninstrumented)

The agent today runs `respInspector` (`proxy.go:752-755`) without timing it.
Add `time.Now()` before / `time.Since()` after, fed into `timer.Mark` for the
aggregate `response_hooks_ms` column. Per-hook latency is captured by the
existing `HookResult.LatencyMs` machinery from `shared/hooks`.

### 4.4 Per-hook latency surface to audit row (bonus)

The existing `OnFlowComplete` (`cmd/agent/main.go:2318-2346`) builds the
`audit.Event` but does not serialize `HooksPipeline` into the row that goes
into local SQLite — the data exists in memory but is dropped before persistence.
Wire `HooksPipeline` into the `audit.Event` so backfill (S5) and the UI
(S7) can both compute aggregates.

### 4.5 Local SQLite schema migration — `audit/queue.go`

`packages/agent/core/observability/audit/queue.go` `CREATE TABLE audit_events` schema
gains 5 columns + JSONB:

```sql
ALTER TABLE audit_events ADD COLUMN upstream_ttfb_ms     INTEGER;
ALTER TABLE audit_events ADD COLUMN upstream_total_ms    INTEGER;
ALTER TABLE audit_events ADD COLUMN request_hooks_ms     INTEGER;
ALTER TABLE audit_events ADD COLUMN response_hooks_ms    INTEGER;
ALTER TABLE audit_events ADD COLUMN latency_breakdown    TEXT;  -- JSON (SQLite has no JSONB)
ALTER TABLE audit_events ADD COLUMN hooks_pipeline       TEXT;  -- JSON (was dropped before)
```

Run as a SQLite migration on first boot of the upgraded agent (idempotent ALTER
with `IF NOT EXISTS` guards or version-checked migration table).

### 4.6 Rollup table phase columns — `rollup_local_*`

The agent's local stats rollup tables (`thing_metric_rollup_local_*`) feed the
Stats page in S7. Today they carry `request_count`, `bytes_in/out`,
`latency_sum`, `latency_count`, etc. Add matching sum/count pairs per phase so
P50/P95 and means can render fast for 7d/30d ranges:

```sql
ALTER TABLE thing_metric_rollup_local_* ADD COLUMN latency_us_sum               INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thing_metric_rollup_local_* ADD COLUMN latency_us_count             INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thing_metric_rollup_local_* ADD COLUMN latency_upstream_ttfb_sum    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thing_metric_rollup_local_* ADD COLUMN latency_upstream_ttfb_count  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thing_metric_rollup_local_* ADD COLUMN latency_upstream_total_sum   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thing_metric_rollup_local_* ADD COLUMN latency_upstream_total_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thing_metric_rollup_local_* ADD COLUMN latency_hooks_sum            INTEGER NOT NULL DEFAULT 0;
ALTER TABLE thing_metric_rollup_local_* ADD COLUMN latency_hooks_count          INTEGER NOT NULL DEFAULT 0;
```

`latency_us` is the "our overhead" derived metric (the agent's intercept +
hooks part). The rollup writer computes it as
`latency_ms - upstream_total_ms` at insert time and adds to the sum.

### 4.7 statusapi extension — `bridge.go` + `server.go`

`packages/agent/core/sync/statusapi/server.go` `handleQueryEvents` SELECT (line
~567 in queue.go's `QueryEvents`) currently projects 23 columns. Extend the
projection to include the 5 new latency phase columns + `hooks_pipeline` +
`latency_breakdown`. The Wails bridge `AgentEvent` type
(`packages/agent/ui/frontend/src/types/agent.ts`) gains matching fields.

`handleQueryStats` SELECT extends to include the 8 new rollup columns; the
`StatsResponse.rows` shape gains the new metric names.

### 4.8 Unit tests

- `intercept_test.go`: fake flow with controlled timing — intercept_ms within
  a few ms of expected, hooks aggregates non-zero, upstream_total propagated.
- `proxy_test.go`: fake upstream with controlled TTFB — agent records it.
- `queue_test.go`: SQLite ALTER migration runs idempotently; QueryEvents
  returns the new columns.

## Acceptance Criteria

- A live flow through dev agent produces a row in local `audit_events` with
  all 5 new columns + `hooks_pipeline` JSON + `latency_breakdown` JSON
  (containing `intercept_ms`).
- The same row, once uploaded to Hub via `/agent-audit`, lands in
  `traffic_event` with identical values (no Hub-side reshape).
- Agent statusapi `QueryEvents` returns the new fields; Wails UI receives
  them in the next refresh.
- `thing_metric_rollup_local_*` insert path correctly accumulates the 8 new
  metrics; running the Stats query yields non-zero values after a few minutes
  of traffic.
- `go test -race -count=1 ./packages/agent/...` passes.

## Non-Goals

- ai-gateway / compliance-proxy (S2, S3).
- Hub-side rollup table changes — the Hub-side `thing_metric_rollup_*` is
  populated from the agent's uploaded `traffic_event` rows; that path will
  follow the same `latency_ms - upstream_total_ms` recipe and is wired in S5
  alongside the backfill.
- Agent UI rendering — that's S7.
