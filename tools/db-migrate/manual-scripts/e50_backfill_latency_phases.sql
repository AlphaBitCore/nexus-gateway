-- E50 — Historical traffic_event latency-phase backfill.
--
-- Reconstructs phase fields on pre-E50 traffic_event rows using data
-- already present on the row:
--   request_hooks_ms   = SUM(latencyMs in request_hooks_pipeline)
--   response_hooks_ms  = SUM(latencyMs in response_hooks_pipeline)
--   latency_breakdown.routing_ms = SUM(routing_trace.stages[*].DurationMs)
--                                  (ai-gateway rows only)
--   upstream_total_ms  = GREATEST(0, latency_ms
--                        - request_hooks_ms
--                        - response_hooks_ms
--                        - routing_ms)   ← aggressive residual estimate
--
-- upstream_ttfb_ms is NOT reconstructable from existing data; left NULL.
-- tls_handshake_ms / intercept_ms / auth_ms / quota_ms / cache_lookup_ms /
-- req_adapter_ms / resp_adapter_ms also left absent — Prometheus-derived
-- estimates were ruled out as too lossy per the E50 product decision.
--
-- Operational notes (see docs/operators/ops/runbooks/e50-backfill-procedure.md):
--   - Runs in 10k-row batches with a 500ms inter-batch sleep.
--   - Cursor in _e50_backfill_cursor lets a rerun resume after the
--     interrupted batch.
--   - `FOR UPDATE SKIP LOCKED` keeps live INSERTs unblocked during run.
--   - Re-running on already-backfilled rows is a no-op (the WHERE clause
--     filters NULL request_hooks_ms / NULL upstream_total_ms).
--
-- Pre-flight: `pg_dump --table traffic_event > traffic_event.predump.sql`.
-- Run: `psql -f e50_backfill_latency_phases.sql 2>&1 | tee backfill.log`.

BEGIN;

CREATE TABLE IF NOT EXISTS _e50_backfill_cursor (
    id          SERIAL PRIMARY KEY,
    last_id     TEXT NOT NULL,
    batch_n     INTEGER NOT NULL,
    rows_done   BIGINT NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;

DO $$
DECLARE
    v_batch_size  INT := 10000;
    v_sleep_secs  REAL := 0.5;
    v_last_id     TEXT;
    v_max_id      TEXT;
    v_batch_count BIGINT;
    v_total_done  BIGINT := 0;
BEGIN
    SELECT last_id INTO v_last_id FROM _e50_backfill_cursor
        ORDER BY updated_at DESC LIMIT 1;
    IF v_last_id IS NULL THEN v_last_id := ''; END IF;

    RAISE NOTICE 'e50 backfill: resuming from last_id = %', v_last_id;

    LOOP
        WITH batch AS (
            SELECT id
            FROM   traffic_event
            WHERE  id > v_last_id
              AND  (request_hooks_ms IS NULL OR upstream_total_ms IS NULL)
            ORDER BY id
            LIMIT  v_batch_size
            FOR UPDATE SKIP LOCKED
        ),
        agg AS (
            SELECT
                te.id,
                COALESCE((
                    SELECT SUM(NULLIF((elem->>'latencyMs'), '')::int)
                    FROM jsonb_array_elements(
                        CASE jsonb_typeof(te.request_hooks_pipeline)
                            WHEN 'array' THEN te.request_hooks_pipeline
                            ELSE '[]'::jsonb
                        END) AS elem
                ), 0) AS req_hooks,
                COALESCE((
                    SELECT SUM(NULLIF((elem->>'latencyMs'), '')::int)
                    FROM jsonb_array_elements(
                        CASE jsonb_typeof(te.response_hooks_pipeline)
                            WHEN 'array' THEN te.response_hooks_pipeline
                            ELSE '[]'::jsonb
                        END) AS elem
                ), 0) AS resp_hooks,
                CASE WHEN te.source = 'ai-gateway' AND te.routing_trace IS NOT NULL
                     THEN COALESCE((
                         SELECT SUM(NULLIF((stage->>'durationMs'), '')::int)
                         FROM jsonb_array_elements(
                             CASE jsonb_typeof(te.routing_trace->'stages')
                                 WHEN 'array' THEN te.routing_trace->'stages'
                                 ELSE '[]'::jsonb
                             END) AS stage
                     ), 0)
                     ELSE 0
                END AS routing_ms,
                te.source,
                te.latency_ms
            FROM   traffic_event te
            WHERE  te.id IN (SELECT id FROM batch)
        )
        UPDATE traffic_event t
        SET    request_hooks_ms  = NULLIF(agg.req_hooks, 0),
               response_hooks_ms = NULLIF(agg.resp_hooks, 0),
               upstream_total_ms = GREATEST(0,
                                       COALESCE(t.latency_ms, 0)
                                     - agg.req_hooks
                                     - agg.resp_hooks
                                     - agg.routing_ms),
               latency_breakdown = CASE
                   WHEN agg.source = 'ai-gateway' AND agg.routing_ms > 0
                   THEN COALESCE(t.latency_breakdown, '{}'::jsonb)
                       || jsonb_build_object('routing_ms', agg.routing_ms)
                   ELSE t.latency_breakdown
               END
        FROM   agg
        WHERE  t.id = agg.id;

        GET DIAGNOSTICS v_batch_count = ROW_COUNT;
        EXIT WHEN v_batch_count = 0;

        -- Advance the cursor: pick the largest id we just touched. The
        -- prior aggregate-with-ORDER-BY/LIMIT form was invalid PL/pgSQL
        -- (aggregates yield 1 row; ORDER/LIMIT is meaningless and PG
        -- rejects the column reference). The right shape is a subquery
        -- that selects the next page of ids and takes their max.
        SELECT MAX(id) INTO v_max_id
        FROM (
            SELECT id
            FROM   traffic_event
            WHERE  id > v_last_id
              AND  (request_hooks_ms IS NOT NULL OR upstream_total_ms IS NOT NULL)
            ORDER BY id
            LIMIT  v_batch_size
        ) sub;
        IF v_max_id IS NULL THEN
            -- No more rows match — exit the loop on the next iteration's
            -- empty UPDATE (v_batch_count will be 0).
            v_max_id := v_last_id;
        END IF;
        v_last_id := v_max_id;
        v_total_done := v_total_done + v_batch_count;

        INSERT INTO _e50_backfill_cursor (last_id, batch_n, rows_done)
            VALUES (v_last_id, v_batch_count, v_batch_count);

        RAISE NOTICE 'e50 backfill: batch=%, last_id=%, total_done=%',
            v_batch_count, v_last_id, v_total_done;

        PERFORM pg_sleep(v_sleep_secs);
    END LOOP;

    RAISE NOTICE 'e50 backfill complete. Total rows processed: %', v_total_done;
END $$;
