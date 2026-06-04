-- 5-minute ops-metric rollup tier. metric_ops_raw is aggregated into this table
-- by the ops-rollup-5m Hub job; the 1h/1d/1mo tiers cascade from here. Columns,
-- index shape, and the partial-COALESCE uniqueness guard mirror
-- metric_ops_rollup_1h exactly so the cascade code path is uniform across tiers.

CREATE TABLE metric_ops_rollup_5m (
    id            uuid                        NOT NULL,
    bucket_start  timestamp(6) with time zone NOT NULL,
    thing_id      text,
    thing_type    text                        NOT NULL,
    metric_name   text                        NOT NULL,
    metric_kind   text                        NOT NULL,
    dimension_key text DEFAULT ''::text       NOT NULL,
    value_avg     double precision,
    value_sum     double precision,
    value_min     double precision,
    value_max     double precision,
    sample_count  integer                     NOT NULL,
    metadata      jsonb,
    CONSTRAINT metric_ops_rollup_5m_pkey PRIMARY KEY (id)
);

-- Natural-identity uniqueness guard, identical column set to uq_ops_rollup_1h.
-- thing_id is nullable (NULL = fleet aggregate), so COALESCE collapses NULL to
-- '' to keep one row per (bucket, thing-or-fleet, metric, dimension).
CREATE UNIQUE INDEX uq_ops_rollup_5m
    ON metric_ops_rollup_5m (bucket_start, COALESCE(thing_id, ''::text), metric_name, dimension_key);

-- Fleet-aggregate read path (thing_id IS NULL) and per-thing read path.
CREATE INDEX idx_ops_5m_fleet_time
    ON metric_ops_rollup_5m (thing_type, bucket_start DESC) WHERE thing_id IS NULL;
CREATE INDEX idx_ops_5m_thing_time
    ON metric_ops_rollup_5m (thing_id, bucket_start DESC) WHERE thing_id IS NOT NULL;

-- Per-metric time scan (matches the Prisma @@index).
CREATE INDEX metric_ops_rollup_5m_metric_name_bucket_start_idx
    ON metric_ops_rollup_5m (metric_name, bucket_start DESC);
