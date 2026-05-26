# E50 — Story 5: Historical Backfill (Hub + Agent Local)

## Context

Pre-E50 `traffic_event` rows carry only `latency_ms`. To make the new
Dashboard / Analytics / Model Detail surfaces useful from day one, this story
reconstructs phase data on historical rows using the data we already have in
the row (per-hook JSONB, routing trace) and an aggressive residual estimation
for `upstream_total_ms`.

Per the product decision, the UI does NOT distinguish reconstructed-vs-measured
rows. Aggregates and P95s blend old and new data without a `latency_source`
flag.

## User Story

**As a** platform owner enabling E50 in production,
**I want** historical `traffic_event` rows to immediately participate in the new
Latency Health dashboards,
**so that** P95 trends and Provider Leaderboards are meaningful from the first
hour of the rollout rather than after a 30-day backfill of new traffic.

## Tasks

### 5.1 Hub-side SQL backfill — `tools/db-migrate/manual-scripts/`

New file `e50_backfill_latency_phases.sql` — batched, resumable, runs on prod.
Pattern (mirrors the e46s4 hook-config fix script's idiom):

```sql
-- Configurable batch size and cursor table.
CREATE TABLE IF NOT EXISTS _e50_backfill_cursor (
    last_id   TEXT PRIMARY KEY,
    batch_n   INTEGER NOT NULL,
    rows_done BIGINT  NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

DO $$
DECLARE
    v_batch_size   INT := 10000;
    v_inter_batch_sleep INTERVAL := '500 milliseconds';
    v_last_id      TEXT;
    v_batch_count  BIGINT;
BEGIN
    SELECT last_id INTO v_last_id FROM _e50_backfill_cursor
        ORDER BY updated_at DESC LIMIT 1;
    IF v_last_id IS NULL THEN v_last_id := ''; END IF;

    LOOP
        WITH batch AS (
            SELECT id
            FROM   traffic_event
            WHERE  id > v_last_id
              AND  (request_hooks_ms IS NULL OR upstream_total_ms IS NULL)
            ORDER BY id
            LIMIT  v_batch_size
            FOR UPDATE SKIP LOCKED
        )
        UPDATE traffic_event te
        SET
            request_hooks_ms = COALESCE(req_sum.s, request_hooks_ms),
            response_hooks_ms = COALESCE(resp_sum.s, response_hooks_ms),
            latency_breakdown = CASE
                WHEN te.source = 'ai-gateway' AND te.routing_trace IS NOT NULL
                THEN jsonb_build_object(
                    'routing_ms',
                    COALESCE(
                        (SELECT SUM((stage->>'durationMs')::int)
                         FROM jsonb_array_elements(te.routing_trace->'stages') AS stage),
                        0))
                ELSE COALESCE(te.latency_breakdown, '{}'::jsonb)
            END,
            upstream_total_ms = GREATEST(0,
                te.latency_ms
                - COALESCE(req_sum.s, 0)
                - COALESCE(resp_sum.s, 0)
                - COALESCE(
                    CASE WHEN te.source = 'ai-gateway' AND te.routing_trace IS NOT NULL
                         THEN (SELECT SUM((stage->>'durationMs')::int)
                               FROM jsonb_array_elements(te.routing_trace->'stages') AS stage)
                         ELSE 0
                    END, 0))
        FROM batch b
        LEFT JOIN LATERAL (
            SELECT SUM((elem->>'latencyMs')::int) AS s
            FROM jsonb_array_elements(te.request_hooks_pipeline) AS elem
        ) req_sum ON TRUE
        LEFT JOIN LATERAL (
            SELECT SUM((elem->>'latencyMs')::int) AS s
            FROM jsonb_array_elements(te.response_hooks_pipeline) AS elem
        ) resp_sum ON TRUE
        WHERE te.id = b.id
        RETURNING te.id INTO v_last_id;

        GET DIAGNOSTICS v_batch_count = ROW_COUNT;
        EXIT WHEN v_batch_count = 0;

        INSERT INTO _e50_backfill_cursor (last_id, batch_n, rows_done)
            VALUES (v_last_id, v_batch_count, v_batch_count)
            ON CONFLICT (last_id) DO UPDATE
            SET batch_n = EXCLUDED.batch_n,
                rows_done = _e50_backfill_cursor.rows_done + EXCLUDED.rows_done,
                updated_at = now();

        RAISE NOTICE 'e50 backfill: batch=%, last_id=%, total_done=%',
            v_batch_count, v_last_id,
            (SELECT SUM(rows_done) FROM _e50_backfill_cursor);

        PERFORM pg_sleep(EXTRACT(EPOCH FROM v_inter_batch_sleep));
    END LOOP;

    RAISE NOTICE 'e50 backfill complete.';
END $$;
```

Constraints:
- `FOR UPDATE SKIP LOCKED` lets the live writer continue inserting new rows
  during backfill without contention.
- 10k batch + 500ms sleep keeps backfill at ~20k rows/sec, which is fine for
  a 100M-row table (~80 min total).
- Cursor table is a persistent resumption point; rerunning the script picks up
  where it left off.
- `upstream_ttfb_ms` is NOT touched (stays NULL on all historical rows).
- `tls_handshake_ms` on compliance-proxy historical rows is NOT reconstructed
  (Prom histogram estimate ruled out per product decision; introduces too much
  uncertainty atop the residual estimate).

### 5.2 Agent-side run-once backfill — `audit/backfill.go` (new)

`packages/agent/core/observability/audit/backfill.go` runs once per agent on first boot
of an E50-enabled binary. Detection: a version row in a new
`migrations_local` table or a magic file under the platform data dir.

Logic (Go-side, executed inside the same SQLite transaction batch by batch):

```go
// e50BackfillLatencyPhases runs once per agent install per upgrade to E50.
// Reconstructs request_hooks_ms, response_hooks_ms from hooks_pipeline JSON
// and upstream_total_ms as the residual. Caps batches to keep the local DB
// responsive.
func (q *Queue) e50BackfillLatencyPhases(ctx context.Context) error { /* … */ }
```

Logged to lifecycle_event so the agent UI Activity page records the operation.

### 5.3 Hub stats rollup phase aggregation

`packages/nexus-hub/internal/jobs/scheduler/` — the per-Thing rollup job that
populates `thing_metric_rollup_*` already SUMs `latency_ms`. Extend the
SELECT/UPDATE to also accumulate:
- `latency_us_sum = SUM(GREATEST(0, latency_ms - COALESCE(upstream_total_ms, 0)))`
- `latency_upstream_ttfb_sum = SUM(COALESCE(upstream_ttfb_ms, 0))`
  / `latency_upstream_ttfb_count = COUNT(upstream_ttfb_ms)` (skip NULLs)
- `latency_upstream_total_sum` / `_count` similar
- `latency_hooks_sum = SUM(COALESCE(request_hooks_ms, 0) + COALESCE(response_hooks_ms, 0))`
  / `_count` based on non-NULL of either

Backfill of historical rollups: re-run the rollup job over the historical
window after 5.1 completes (existing `rollup-rebuild` admin job mechanism).

### 5.4 Backfill ops runbook

`docs/operators/ops/runbooks/e50-backfill-procedure.md` (new) — operator-facing checklist:

1. `pg_dump --table traffic_event` for rollback safety.
2. `psql -f tools/db-migrate/manual-scripts/e50_backfill_latency_phases.sql`
   from an admin host with `NOTICE` output piped to a file for progress.
3. Verify `SELECT COUNT(*) FROM traffic_event WHERE request_hooks_ms IS NULL
   AND timestamp < <cutoff>` returns 0 (or only rows with no hooks pipeline).
4. Trigger the rollup rebuild via admin API.
5. Verify Dashboard `Latency Health` row renders non-NULL P95s for the historical
   window in the UI smoke test.

## Acceptance Criteria

- On a dev DB with sample historical rows, running the SQL script populates
  `request_hooks_ms`, `response_hooks_ms`, `upstream_total_ms`, and
  `latency_breakdown.routing_ms` (for ai-gateway rows). `upstream_ttfb_ms`
  stays NULL.
- Backfill is resumable: interrupt the script mid-run, restart, and it picks
  up from the cursor row.
- A second run on already-backfilled rows is idempotent (the WHERE clause
  prevents unnecessary updates).
- Agent run-once backfill completes locally; the lifecycle_event log records
  start + end with row count.
- `thing_metric_rollup_*` queries against the historical window return
  non-zero P95s for `latency_us`, `latency_upstream_total`, `latency_hooks`.

## Non-Goals

- Backfilling `upstream_ttfb_ms` (not reconstructable).
- Reconstructing per-source long-tail JSONB keys other than
  `latency_breakdown.routing_ms` for ai-gateway.
- A live-traffic dual-write mode — backfill is a one-shot, not a permanent
  background job.
