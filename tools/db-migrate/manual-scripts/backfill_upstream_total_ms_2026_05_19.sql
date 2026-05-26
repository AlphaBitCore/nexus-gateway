-- Backfill `traffic_event.upstream_total_ms` for the streaming-MISS rows
-- whose audit row was emitted before the broker pump goroutine closed
-- the upstream body. Root cause + fix in shared/traffic/tracing.go
-- (phaseTrackedBody — stamps totalMs on every successful Read instead
-- of only on Close, so the goroutine race is impossible going forward).
--
-- Prod evidence (2026-05-19, before the fix):
--   - Of 2634 streaming-MISS rows in the last 24h, 2634 had
--     upstream_ttfb_ms populated but only 15 had upstream_total_ms.
--   - Non-streaming MISS rows were unaffected (181/181 had both).
-- See traffic_event id=57fd72e4-e385-49d7-9a40-3347bbbb4d5c for the
-- canonical mis-attributed row that motivated the investigation.
--
-- Strategy:
--   - For streaming MISS rows whose upstream_total_ms is NULL but
--     upstream_ttfb_ms IS present, set total = ttfb. This is an
--     under-estimate: the true upstream wall time for a streaming
--     response is ttfb + (time spent streaming the body), which we
--     cannot reconstruct. But ttfb is the closest lower bound and is
--     strictly better than NULL — UI fallbacks (LatencyMini /
--     LatencyWaterfall / agent TrafficEventDetail) now also fall back
--     to ttfb when total is null, so updating the column lines the DB
--     up with what the UI was already inferring.
--   - Skip rows where upstream_ttfb_ms IS NULL — without TTFB there is
--     no usable signal at all (e.g. cache HIT, joiner, error before
--     send).
--   - Skip non-streaming rows entirely — they all had upstream_total_ms
--     populated by the synchronous body-close path, so a NULL there
--     is intentional (e.g. cache HIT) and must not be overwritten.
--
-- Safety:
--   - Idempotent: re-running on already-backfilled rows is a no-op
--     (WHERE clause filters upstream_total_ms IS NULL).
--   - No schema change — the column already exists.
--   - Reversible: `UPDATE traffic_event SET upstream_total_ms = NULL
--     WHERE upstream_total_ms = upstream_ttfb_ms AND
--     usage_extraction_status = 'streaming_reported'` rolls the
--     backfill back if a future investigation needs the pre-backfill
--     NULL signal.
--
-- Run: `psql -h <host> -U nexus -d nexus_gateway -f \
--      tools/db-migrate/manual-scripts/backfill_upstream_total_ms_2026_05_19.sql \
--      2>&1 | tee backfill.log`
-- Pre-flight: `pg_dump --table traffic_event -F c -f predump.dump`.

BEGIN;

-- Lock budget guard so a wedged backfill cannot hold a long
-- transaction on the live traffic_event table.
SET LOCAL lock_timeout = '30s';
SET LOCAL statement_timeout = '15min';

WITH candidate AS (
    SELECT id
    FROM traffic_event
    WHERE upstream_total_ms IS NULL
      AND upstream_ttfb_ms IS NOT NULL
      AND usage_extraction_status = 'streaming_reported'
)
UPDATE traffic_event AS te
SET upstream_total_ms = te.upstream_ttfb_ms
FROM candidate AS c
WHERE te.id = c.id;

-- Post-condition: every streaming-MISS row in the candidate set now
-- has a non-NULL upstream_total_ms equal to its TTFB.
SELECT
    count(*)                                       AS rows_updated,
    count(*) FILTER (WHERE upstream_total_ms IS NULL) AS still_null_streaming_miss
FROM traffic_event
WHERE upstream_ttfb_ms IS NOT NULL
  AND usage_extraction_status = 'streaming_reported';

COMMIT;
