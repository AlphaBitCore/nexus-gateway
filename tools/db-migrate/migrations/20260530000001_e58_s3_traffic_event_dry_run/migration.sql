-- E58-S3 T8.2: traffic_event dry-run discriminator.
--
-- is_dry_run flags rows produced by the nexus.dry_run pipeline branch
-- (cost estimate, no upstream call). dry_run_assumptions carries the
-- estimator's assumption list as JSONB so the Traffic Drawer can
-- surface them. Default false on is_dry_run leaves every historical
-- and future real-request row unchanged.

ALTER TABLE traffic_event
    ADD COLUMN IF NOT EXISTS is_dry_run BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS dry_run_assumptions JSONB;

-- Partial index on the rare-true side keeps the default Traffic-list
-- query (WHERE is_dry_run = false) fast while still letting the
-- "show dry-runs" toggle query the small slice efficiently.
CREATE INDEX IF NOT EXISTS idx_traffic_event_dry_run
    ON traffic_event (created_at DESC)
    WHERE is_dry_run = true;
