-- Convert metric_ops_raw into a daily RANGE-partitioned table (partition key
-- sampled_at). Dropping a whole-day partition is an O(1) metadata operation,
-- replacing the chunked row-by-row DELETE that made ops-retention run for
-- minutes against the 13M-row / 8GB table.
--
-- PostgreSQL native partitioning cannot be expressed through Prisma, so this is
-- raw SQL. Development-phase policy (no backward compatibility; telemetry is
-- disposable) lets us DROP and recreate the table partitioned rather than carry
-- a non-partitioned legacy path or run a slow in-place backfill. A partitioned
-- table's PRIMARY KEY and every UNIQUE index must include the partition key, so
-- the surrogate id becomes part of the composite primary key (sampled_at, id).

DROP TABLE IF EXISTS metric_ops_raw CASCADE;

CREATE TABLE metric_ops_raw (
    id            uuid                        NOT NULL,
    sampled_at    timestamp(6) with time zone NOT NULL,
    thing_id      text                        NOT NULL,
    thing_type    text                        NOT NULL,
    metric_name   text                        NOT NULL,
    metric_kind   text                        NOT NULL,
    dimension_key text DEFAULT ''::text       NOT NULL,
    value         double precision,
    metadata      jsonb,
    PRIMARY KEY (sampled_at, id)
) PARTITION BY RANGE (sampled_at);

-- Indexes + FK declared on the partitioned parent propagate to every current
-- and future partition. The unique key already leads with the partition key.
CREATE UNIQUE INDEX metric_ops_raw_sampled_at_thing_id_metric_name_dimension_ke_key
    ON metric_ops_raw (sampled_at, thing_id, metric_name, dimension_key);
CREATE INDEX metric_ops_raw_metric_name_sampled_at_idx
    ON metric_ops_raw (metric_name, sampled_at DESC);
CREATE INDEX metric_ops_raw_thing_id_sampled_at_idx
    ON metric_ops_raw (thing_id, sampled_at DESC);
CREATE INDEX metric_ops_raw_thing_type_sampled_at_idx
    ON metric_ops_raw (thing_type, sampled_at DESC);
ALTER TABLE metric_ops_raw
    ADD CONSTRAINT metric_ops_raw_thing_id_fkey FOREIGN KEY (thing_id)
    REFERENCES thing(id) ON UPDATE CASCADE ON DELETE CASCADE;

-- Seed the initial partition window [today-30d, today+2d] (UTC). The
-- ops-raw-partition Hub job maintains the set thereafter. Naming
-- (metric_ops_raw_pYYYYMMDD) and the bound convention ('YYYY-MM-DD 00:00:00+00')
-- match internal/jobs/defs/retention/ops_raw_partition.go so the job and this
-- migration produce identical, non-overlapping partitions.
DO $$
DECLARE
    d0 date := (now() AT TIME ZONE 'UTC')::date;
    d  date;
BEGIN
    FOR d IN SELECT generate_series(d0 - 30, d0 + 2, interval '1 day')::date LOOP
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I PARTITION OF metric_ops_raw FOR VALUES FROM (%L) TO (%L)',
            'metric_ops_raw_p' || to_char(d, 'YYYYMMDD'),
            to_char(d, 'YYYY-MM-DD') || ' 00:00:00+00',
            to_char(d + 1, 'YYYY-MM-DD') || ' 00:00:00+00'
        );
    END LOOP;
END $$;
