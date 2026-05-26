-- E58-S5: clean up dry-run traffic_event rows before the column drop migration.
--
-- Why: nexus.dry_run flag is deleted (Task 1 commit 14d625205). All existing
-- traffic_event rows with is_dry_run = true carried an estimated_cost_usd
-- forecast value, never a real upstream call. Keeping them mixed with real
-- rows after the column drop in Task 5 would mean post-migration rows have
-- no way to be distinguished from pre-migration estimate forecasts.
--
-- Safe to run any time AFTER Task 1 lands AND BEFORE Task 5 migration runs.
-- Idempotent — re-running on an already-clean table is a no-op.
--
-- Local: docker exec -i nexus-postgres psql -U postgres -d nexus_gateway \
--          < tools/db-migrate/manual-scripts/e58_s5_cleanup_dryrun_traffic_events.sql
-- Prod:  same connection method via the ec2 jump-box per the standard
--        manual-script runbook (prod-deploy skill).

BEGIN;

-- 1. Show what's about to be deleted (for audit log).
--    VK identity on traffic_event lives on entity_id (when entity_type='virtual_key'),
--    not a dedicated virtual_key_id column.
SELECT
  COUNT(*)                                                              AS dryrun_row_count,
  MIN(timestamp)                                                        AS earliest_dryrun,
  MAX(timestamp)                                                        AS latest_dryrun,
  COUNT(DISTINCT entity_id) FILTER (WHERE entity_type = 'virtual_key')  AS distinct_vks_affected
FROM traffic_event
WHERE is_dry_run = true;

-- 2. Delete the rows.
DELETE FROM traffic_event
WHERE is_dry_run = true;

-- 3. Reset analytics rollup watermarks so any aggregates that included these
--    rows get recomputed from scratch. Cost / latency rollups filter
--    is_dry_run = false but the safer move is to invalidate watermarks
--    in case a stray rollup ever loaded them.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_name = 'rollup_watermark') THEN
    UPDATE rollup_watermark
       SET last_processed_at = NULL
     WHERE rollup_kind IN ('cost_summary', 'analytics_latency',
                           'analytics_rollup', 'cache_roi');
  END IF;
END $$;

COMMIT;

-- 4. Verify post-cleanup state.
SELECT
  COUNT(*)                              AS remaining_dryrun_rows,
  (SELECT COUNT(*) FROM traffic_event)  AS total_traffic_events_after
FROM traffic_event
WHERE is_dry_run = true;
-- Expected: remaining_dryrun_rows = 0.
