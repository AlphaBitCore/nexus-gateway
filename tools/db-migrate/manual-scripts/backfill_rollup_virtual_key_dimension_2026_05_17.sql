-- backfill_rollup_virtual_key_dimension_2026_05_17.sql
--
-- Backfill metric_rollup_5m + thing_metric_rollup_5m with the
-- per-Virtual-Key (`virtual_key=<UUID>`) dimension buckets that have
-- been missing since the identity.credential → identity.vk rename in
-- the producers (several sprints ago). The Hub rollup jobs read
-- `identity->'credential'->>'id'` to source virtual_key_id, but
-- ai-gateway has ALWAYS written `identity.vk` and never had a
-- `credential` key in the identity JSON. The result: every per-VK
-- rollup bucket was created with virtualKeyID="" — and the rollup
-- code's `if virtualKeyID != "" { ... }` guard then dropped the
-- `virtual_key=<UUID>` dimension entirely. Net effect: zero
-- per-VK buckets exist in metric_rollup_5m for historical data.
--
-- Fix landed in commit 220319dda (2026-05-17). NEW data from this
-- point forward rolls up correctly. This script backfills the
-- historical gap for the 6 highest-impact metrics that dashboards
-- key by virtual_key:
--
--   - request_count
--   - prompt_tokens / completion_tokens / total_tokens
--   - estimated_cost_usd
--   - latency_sum + latency_count (mean latency, p50 visualisation)
--
-- Other metrics (cache_*, ttft_*, model_shift_count, …) will have
-- historical per-VK gaps for the retention window; new buckets are
-- correct. Adding them here is mechanical but unbounded — only
-- include them if a specific dashboard or alert needs them.
--
-- IDEMPOTENT: the UNIQUE index (bucketStart, metricName, dimensionKey,
-- subDimension) means re-running this script is a no-op for already-
-- present buckets. Safe to retry / run multiple times.
--
-- SCOPE: ai-gateway source only. compliance-proxy + agent never
-- carry VK identity in this codebase (compliance-proxy stamps
-- {status: "pending"}, agent forwards or stamps the same fallback);
-- there is no per-VK breakdown to backfill there.
--
-- WHERE clause time window: 90 days back, matching the data_retention
-- job's traffic_event purge horizon. Going further is wasted work
-- since the source rows have been purged.

BEGIN;

-- 1. metric_rollup_5m — per-bucket per-VK aggregates
-- ─────────────────────────────────────────────────
-- The bucket grain is 5 minutes (date_trunc('hour') + N×5min). The
-- subDimension column carries `source=ai-gateway`, matching what the
-- live rollup job emits (see shared/metrics/types.go BuildSubDimension).

WITH bucket_data AS (
    SELECT
        date_trunc('hour', timestamp)
            + (floor(extract(minute from timestamp)::int / 5) * INTERVAL '5 minutes')
            AS bucket_start,
        identity->'vk'->>'id' AS vk_id,
        COUNT(*)                                        AS request_count,
        SUM(COALESCE(prompt_tokens, 0))::numeric        AS prompt_tokens_sum,
        SUM(COALESCE(completion_tokens, 0))::numeric    AS completion_tokens_sum,
        SUM(COALESCE(total_tokens, 0))::numeric         AS total_tokens_sum,
        SUM(COALESCE(estimated_cost_usd, 0))::numeric   AS cost_sum,
        SUM(COALESCE(latency_ms, 0))::numeric           AS latency_sum,
        COUNT(*) FILTER (WHERE latency_ms IS NOT NULL)::numeric AS latency_count
    FROM traffic_event
    WHERE source = 'ai-gateway'
      AND identity->'vk'->>'id' IS NOT NULL
      AND timestamp >= NOW() - INTERVAL '90 days'
    GROUP BY 1, 2
)
INSERT INTO metric_rollup_5m
    (id, "bucketStart", "metricName", "dimensionKey", "subDimension", value, metadata, "updatedAt")
SELECT
    'backfill-vk-' || md5(bucket_start::text || metric_name || vk_id) AS id,
    bucket_start,
    metric_name,
    'virtual_key=' || vk_id AS dimension_key,
    'source=ai-gateway'     AS sub_dimension,
    metric_value,
    NULL,
    NOW()
FROM bucket_data
CROSS JOIN LATERAL (
    VALUES
        ('request_count',       request_count),
        ('prompt_tokens',       prompt_tokens_sum),
        ('completion_tokens',   completion_tokens_sum),
        ('total_tokens',        total_tokens_sum),
        ('estimated_cost_usd',  cost_sum),
        ('latency_sum',         latency_sum),
        ('latency_count',       latency_count)
) AS m(metric_name, metric_value)
WHERE metric_value IS NOT NULL AND metric_value > 0
ON CONFLICT ("bucketStart", "metricName", "dimensionKey", "subDimension")
DO NOTHING;

-- 2. thing_metric_rollup_5m — per-Thing per-VK aggregates
-- ────────────────────────────────────────────────────────
-- Same idea but the primary key includes thing_id. Only rows with
-- a thing_id (= attributed to a specific node) are eligible.

WITH bucket_data AS (
    SELECT
        date_trunc('hour', timestamp)
            + (floor(extract(minute from timestamp)::int / 5) * INTERVAL '5 minutes')
            AS bucket_start,
        thing_id,
        identity->'vk'->>'id' AS vk_id,
        COUNT(*)                                        AS request_count,
        SUM(COALESCE(prompt_tokens, 0))::numeric        AS prompt_tokens_sum,
        SUM(COALESCE(completion_tokens, 0))::numeric    AS completion_tokens_sum,
        SUM(COALESCE(total_tokens, 0))::numeric         AS total_tokens_sum,
        SUM(COALESCE(estimated_cost_usd, 0))::numeric   AS cost_sum,
        SUM(COALESCE(latency_ms, 0))::numeric           AS latency_sum,
        COUNT(*) FILTER (WHERE latency_ms IS NOT NULL)::numeric AS latency_count
    FROM traffic_event
    WHERE source = 'ai-gateway'
      AND identity->'vk'->>'id' IS NOT NULL
      AND thing_id IS NOT NULL
      AND timestamp >= NOW() - INTERVAL '90 days'
    GROUP BY 1, 2, 3
)
INSERT INTO thing_metric_rollup_5m
    (id, thing_id, "bucketStart", "metricName", "dimensionKey", "subDimension", value, metadata, "updatedAt")
SELECT
    'backfill-vk-' || md5(thing_id || bucket_start::text || metric_name || vk_id) AS id,
    thing_id,
    bucket_start,
    metric_name,
    'virtual_key=' || vk_id AS dimension_key,
    'source=ai-gateway'     AS sub_dimension,
    metric_value,
    NULL,
    NOW()
FROM bucket_data
CROSS JOIN LATERAL (
    VALUES
        ('request_count',       request_count),
        ('prompt_tokens',       prompt_tokens_sum),
        ('completion_tokens',   completion_tokens_sum),
        ('total_tokens',        total_tokens_sum),
        ('estimated_cost_usd',  cost_sum),
        ('latency_sum',         latency_sum),
        ('latency_count',       latency_count)
) AS m(metric_name, metric_value)
WHERE metric_value IS NOT NULL AND metric_value > 0
ON CONFLICT (thing_id, "bucketStart", "metricName", "dimensionKey", "subDimension")
DO NOTHING;

-- Quick sanity check: how many backfill buckets did we write?
DO $$
DECLARE
    n5m  INT;
    nt5m INT;
BEGIN
    SELECT COUNT(*) INTO n5m
        FROM metric_rollup_5m
        WHERE "dimensionKey" LIKE 'virtual_key=%' AND id LIKE 'backfill-vk-%';
    SELECT COUNT(*) INTO nt5m
        FROM thing_metric_rollup_5m
        WHERE "dimensionKey" LIKE 'virtual_key=%' AND id LIKE 'backfill-vk-%';
    RAISE NOTICE 'metric_rollup_5m backfill buckets: %', n5m;
    RAISE NOTICE 'thing_metric_rollup_5m backfill buckets: %', nt5m;
END $$;

COMMIT;
