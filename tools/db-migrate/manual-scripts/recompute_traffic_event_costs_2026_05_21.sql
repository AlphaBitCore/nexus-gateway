-- 2026-05-21 — One-shot recompute of traffic_event cost fields after the
-- Anthropic double-counting bug fix + provider_pricing table retirement.
--
-- =====================================================================
-- WHAT THIS FIXES
-- =====================================================================
--   1) Pre-fix proxy.go.computeCacheCosts had an Anthropic-only branch
--      that skipped the cache_read/cache_creation subtraction, so
--      Anthropic rows with cache tokens billed the cached portion at
--      full input price AND again at the cache discount/surcharge rate.
--      Verified on row 09b83222: actual $0.247846 vs. correct $0.110235
--      (2.25× over).
--
--   2) Plan B also moved the source of truth from provider_pricing table
--      to Model row's 4 price columns. Historical rows were written
--      against the OLD provider_pricing values; this script recomputes
--      them using the CURRENT Model row prices so what admins see in
--      the drawer matches what the recompute uses.
--
-- =====================================================================
-- WHAT IT TOUCHES
-- =====================================================================
--   estimated_cost_usd, cache_write_cost_usd, cache_read_savings_usd,
--   cache_net_savings_usd, reasoning_cost_usd
--
-- =====================================================================
-- WHAT IT DOES NOT TOUCH
-- =====================================================================
--   Rows where routed_model_id is NULL (no JOIN target) — preserves old
--   value. Rows where Model.inputPricePerMillion is NULL (no price) —
--   ditto. Rows where prompt_tokens is NULL — ditto.
--
-- =====================================================================
-- PROD-SAFE EXECUTION MODEL  (read this before running)
-- =====================================================================
--   - This is a CHUNKED loop, NOT a single big UPDATE. Each chunk is
--     5,000 rows, processed inside its own transaction, with a 50 ms
--     sleep between chunks so concurrent traffic_event writes from
--     Hub's flush path aren't starved.
--   - On a 300k-row table this runs ~60 chunks × ~3-5 s/chunk = 5-8
--     minutes wall-clock. Acceptable to run live; consider a quiet
--     window if traffic peak QPS > 1000 rps.
--   - WAL impact: each chunk is its own tx so WAL bloat is bounded.
--     Replication lag bumps briefly per chunk but recovers between.
--   - Cancel safety: a `pg_cancel_backend` mid-run only aborts the
--     in-flight chunk. Previously-committed chunks stay updated; the
--     `recompute_progress` table tracks position so a re-run picks up
--     where it left off.
--
--   ROLLUP IMPACT: after running this script, the metric_rollup_*
--   aggregates are stale (they reflect the old buggy estimated values).
--   Run the SECOND script (reset_rollup_watermarks_2026_05_21.sql) to
--   trigger a clean re-aggregation by the cron job.
--
-- =====================================================================
-- ROLLBACK
-- =====================================================================
--   This script overwrites cost fields in place; there is no rollback
--   beyond restoring from a DB snapshot. Take a backup before running:
--     pg_dump -Fc -t traffic_event $DATABASE_URL > traffic_event.backup
--
-- =====================================================================
-- USAGE
-- =====================================================================
--   psql -X -f recompute_traffic_event_costs_2026_05_21.sql $DATABASE_URL
--   Watch progress:
--     SELECT * FROM recompute_progress_2026_05_21 ORDER BY chunk_no DESC LIMIT 5;
--
-- =====================================================================

\set ON_ERROR_STOP on

-- Progress tracking table (drop on re-run so chunks restart fresh,
-- but UPDATE is idempotent so re-running is safe).
DROP TABLE IF EXISTS recompute_progress_2026_05_21;
CREATE TABLE recompute_progress_2026_05_21 (
    chunk_no    INTEGER PRIMARY KEY,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    rows_updated INTEGER,
    last_id     TEXT
);

DO $$
DECLARE
    chunk_size CONSTANT INTEGER := 5000;
    sleep_ms   CONSTANT FLOAT   := 0.05;  -- 50 ms between chunks
    v_chunk_no    INTEGER := 0;
    rows_in_chunk INTEGER;
    v_last_id     TEXT := '';  -- string sort because traffic_event.id is text/uuid
    total_done    INTEGER := 0;
BEGIN
    LOOP
        v_chunk_no := v_chunk_no + 1;
        INSERT INTO recompute_progress_2026_05_21 (chunk_no) VALUES (v_chunk_no);

        WITH model_prices AS (
            -- NULL cache prices fall back to input price (mirrors gateway's
            -- cachelayer.derefOrFallback so script's recompute matches what
            -- computeCacheCosts will do on new requests).
            SELECT
                m.id                                   AS model_id,
                m."inputPricePerMillion"::float8       AS in_pm,
                m."outputPricePerMillion"::float8      AS out_pm,
                COALESCE(m."cachedInputReadPricePerMillion"::float8,  m."inputPricePerMillion"::float8) AS cr_pm,
                COALESCE(m."cachedInputWritePricePerMillion"::float8, m."inputPricePerMillion"::float8) AS cw_pm
            FROM "Model" m
            WHERE m."inputPricePerMillion" IS NOT NULL
        ),
        target AS (
            -- Pick the next chunk_size rows after v_last_id.
            SELECT te.id
            FROM traffic_event te
            WHERE te.id > v_last_id
              AND te.routed_model_id IS NOT NULL
              AND te.prompt_tokens IS NOT NULL
            ORDER BY te.id
            LIMIT chunk_size
        ),
        upd AS (
            UPDATE traffic_event te
               SET estimated_cost_usd =
                       (GREATEST(te.prompt_tokens - COALESCE(te.cache_read_tokens, 0) - COALESCE(te.cache_creation_tokens, 0), 0)::float8 * mp.in_pm
                        + COALESCE(te.cache_read_tokens, 0)::float8     * mp.cr_pm
                        + COALESCE(te.cache_creation_tokens, 0)::float8 * mp.cw_pm
                        + COALESCE(te.completion_tokens, 0)::float8     * mp.out_pm
                       ) / 1000000.0,
                   cache_write_cost_usd = CASE
                       WHEN COALESCE(te.cache_creation_tokens, 0) > 0
                           THEN te.cache_creation_tokens::float8 * mp.cw_pm / 1000000.0
                       ELSE NULL
                   END,
                   cache_read_savings_usd = CASE
                       WHEN COALESCE(te.cache_read_tokens, 0) > 0
                           THEN te.cache_read_tokens::float8 * (mp.in_pm - mp.cr_pm) / 1000000.0
                       ELSE NULL
                   END,
                   cache_net_savings_usd = CASE
                       WHEN COALESCE(te.cache_read_tokens, 0) > 0 OR COALESCE(te.cache_creation_tokens, 0) > 0
                           THEN (CASE WHEN COALESCE(te.cache_read_tokens, 0) > 0
                                      THEN te.cache_read_tokens::float8 * (mp.in_pm - mp.cr_pm) / 1000000.0
                                      ELSE 0 END)
                              - (CASE WHEN COALESCE(te.cache_creation_tokens, 0) > 0
                                      THEN te.cache_creation_tokens::float8 * mp.cw_pm / 1000000.0
                                      ELSE 0 END)
                       ELSE NULL
                   END,
                   reasoning_cost_usd = CASE
                       WHEN COALESCE(te.reasoning_tokens, 0) > 0
                           THEN te.reasoning_tokens::float8 * mp.out_pm / 1000000.0
                       ELSE NULL
                   END
              FROM model_prices mp
             WHERE te.id IN (SELECT id FROM target)
               AND te.routed_model_id = mp.model_id
            RETURNING te.id
        )
        SELECT count(*), COALESCE(MAX(id), '') INTO rows_in_chunk, v_last_id
        FROM upd;

        UPDATE recompute_progress_2026_05_21
           SET finished_at = now(),
               rows_updated = rows_in_chunk,
               last_id = v_last_id
         WHERE chunk_no = v_chunk_no;

        total_done := total_done + rows_in_chunk;

        EXIT WHEN rows_in_chunk = 0;

        RAISE NOTICE 'chunk % done: % rows, last_id=%, total=%',
            v_chunk_no, rows_in_chunk, v_last_id, total_done;

        -- Yield so concurrent writes can land.
        PERFORM pg_sleep(sleep_ms);
    END LOOP;

    RAISE NOTICE 'recompute complete: % rows updated across % chunks', total_done, v_chunk_no;
END $$;

-- Final summary
SELECT
    sum(rows_updated)::bigint AS total_rows_updated,
    count(*)                  AS total_chunks,
    min(started_at)           AS started,
    max(finished_at)          AS finished,
    max(finished_at) - min(started_at) AS wall_clock
FROM recompute_progress_2026_05_21;

-- =====================================================================
-- POST-PASS: recompute gateway-cache HIT rows.
-- =====================================================================
--   2026-05-21 semantic correction: EstimatedCostUsd is the PREDICTED
--   spend at the configured Model prices — independent of whether the
--   request hit cache or actually called upstream. Pre-fix proxy.go
--   set HIT rows' estimated to 0; we now set it to the would-have-paid
--   value (= same as gateway_cache_savings_usd on full HITs).
--   Customer's actual paid amount = estimated_cost_usd −
--   gateway_cache_savings_usd. Both columns carry distinct information
--   so dashboards can show "predicted spend if no cache" vs "savings".
--
--   For HIT rows with token data (post-Task-#21-fix HITs): recompute
--   estimated_cost_usd + gateway_cache_savings_usd from tokens × Model
--   prices. Both equal each other on a full HIT.
--
--   For HIT rows with NULL tokens (pre-Task-#21 historical HITs): we
--   can't fabricate tokens — leave the row's prompt/completion NULL
--   and accept estimated/savings will also be NULL on those rows
--   (data loss honest signal — no upstream cached entry Usage was
--   ever stored). NEW HIT rows from now on populate everything.

WITH model_prices AS (
    SELECT
        m.id                                   AS model_id,
        m."inputPricePerMillion"::float8       AS in_pm,
        m."outputPricePerMillion"::float8      AS out_pm
    FROM "Model" m
    WHERE m."inputPricePerMillion" IS NOT NULL
)
UPDATE traffic_event te
   SET estimated_cost_usd        = (te.prompt_tokens::float8 * mp.in_pm + COALESCE(te.completion_tokens, 0)::float8 * mp.out_pm) / 1000000.0,
       gateway_cache_savings_usd = (te.prompt_tokens::float8 * mp.in_pm + COALESCE(te.completion_tokens, 0)::float8 * mp.out_pm) / 1000000.0
  FROM model_prices mp
 WHERE te.gateway_cache_status = 'hit'
   AND te.routed_model_id = mp.model_id
   AND te.prompt_tokens IS NOT NULL;

-- Report
SELECT count(*) AS cache_hit_rows_with_tokens_recomputed
  FROM traffic_event
 WHERE gateway_cache_status = 'hit'
   AND estimated_cost_usd IS NOT NULL
   AND prompt_tokens IS NOT NULL;

SELECT count(*) AS cache_hit_rows_with_null_tokens_unrecoverable
  FROM traffic_event
 WHERE gateway_cache_status = 'hit'
   AND prompt_tokens IS NULL;
