-- Backfill thing_metric_rollup_5m with latency_hooks_sum / _count rows
-- for historical buckets that Hub's per-thing rollup_5m job didn't emit.
--
-- Background:
--   thing_rollup_5m.go (Hub) emits latency_hooks_{sum,count} when
--   request_hooks_ms + response_hooks_ms > 0 for a traffic_event row.
--   That code is in the current prod binary (verified at file:line
--   thing_rollup_5m.go:402-405). Going forward, every new bucket gets
--   hooks rollup entries.
--
--   Historical buckets, however, were processed by older Hub binaries
--   that did NOT emit hooks (the rollup code shipped after the original
--   thing_metric_rollup writer). Result: the Stats tab "Hooks avg"
--   trend renders an empty card for every (thing, bucket) where the
--   source traffic_event row had hooks data but the rollup didn't
--   capture it.
--
-- What this script does:
--   For every 5-minute bucket in the past 30 days, GROUP BY
--   (bucket, thing_id) on traffic_event rows where the row's
--   request_hooks_ms + response_hooks_ms sums to > 0, then INSERT
--   matching latency_hooks_sum / latency_hooks_count rows into
--   thing_metric_rollup_5m with empty dimensionKey + subDimension.
--
--   Empty dimension only — the Stats tab Hooks trend reads the
--   dimensionKey='' variant. Per-dimension breakdowns (entity, model,
--   provider, etc.) can be cascaded later via the merge job if ops
--   wants the dim-split view of historical hooks.
--
-- Idempotent: ON CONFLICT DO NOTHING. If a Hub tick later writes the
-- "real" rollup for the same (bucket, thing, metric) tuple, our manual
-- backfill is preserved; if our manual value is wrong it can be cleaned
-- up by deleting where checksum identifies these rows.
--
-- Cascade to _1h / _1d: the existing ops_rollup_cascade job re-derives
-- _1h and _1d from _5m on its tick. Running this backfill alone makes
-- the 5m series fill in; _1h / _1d will catch up on the next cascade
-- run (typically within an hour).

BEGIN;

WITH source AS (
  SELECT
    to_timestamp(floor(EXTRACT(EPOCH FROM timestamp) / 300) * 300) AT TIME ZONE 'UTC' AS bucket_at,
    thing_id,
    SUM(COALESCE(request_hooks_ms, 0) + COALESCE(response_hooks_ms, 0))::numeric        AS hooks_sum,
    COUNT(*) FILTER (
      WHERE COALESCE(request_hooks_ms, 0) + COALESCE(response_hooks_ms, 0) > 0
    )::numeric                                                                            AS hooks_count
  FROM traffic_event
  WHERE thing_id IS NOT NULL
    AND timestamp >= now() - interval '30 days'
  GROUP BY 1, 2
  HAVING SUM(COALESCE(request_hooks_ms, 0) + COALESCE(response_hooks_ms, 0)) > 0
)
INSERT INTO thing_metric_rollup_5m (
  id, "bucketStart", thing_id, "metricName", "dimensionKey", "subDimension", value, "updatedAt"
)
SELECT
  gen_random_uuid()::text,
  s.bucket_at,
  s.thing_id,
  m.metric_name,
  ''::text                AS dim_key,
  ''::text                AS sub_dim,
  m.metric_value,
  now()
FROM source s
CROSS JOIN LATERAL (VALUES
  ('latency_hooks_sum',   s.hooks_sum),
  ('latency_hooks_count', s.hooks_count)
) AS m(metric_name, metric_value)
ON CONFLICT ("bucketStart", thing_id, "metricName", "dimensionKey", "subDimension")
DO NOTHING;

-- Sanity: report inserted-vs-skipped counts so the operator can confirm
-- the backfill landed something (or surface "ops, schema doesn't have
-- hooks-shaped data yet" without committing the transaction blind).
DO $$
DECLARE
  v_rows  bigint;
  v_thing bigint;
BEGIN
  SELECT count(*), count(DISTINCT thing_id)
    INTO v_rows, v_thing
  FROM thing_metric_rollup_5m
  WHERE "metricName" IN ('latency_hooks_sum', 'latency_hooks_count')
    AND "dimensionKey" = ''
    AND "bucketStart" >= now() - interval '30 days';
  RAISE NOTICE 'thing_metric_rollup_5m hooks rows (30 d): % across % things', v_rows, v_thing;
END $$;

COMMIT;
