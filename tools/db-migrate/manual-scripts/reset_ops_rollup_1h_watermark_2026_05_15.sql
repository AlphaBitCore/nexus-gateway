-- Reset the ops_rollup_1h job watermark so the next Hub tick backfills all
-- historical 1h buckets that were lost to the broken thing_type filter.
--
-- Background:
--   Until the e3273a94 fix, ops_rollup_1h's INSERT filtered on
--   `thing_type = 'service'`. metric_ops_raw.thing_type carries the
--   canonical Thing.type (ai-gateway / control-plane / compliance-proxy
--   / nexus-hub / agent), so the literal 'service' bucket matched
--   nothing. metric_ops_rollup_1h has been empty for every server Thing
--   since the rollup writer shipped.
--
--   The watermark advanced one bucket per tick anyway (the empty INSERT
--   was still considered a successful run), so the cursor is now at the
--   most-recent sealed hour. Without intervention the fix only fills
--   forward — every Stats / Metrics card prior to the deploy stays blank.
--
-- What this script does:
--   Deletes the rollup_watermark row for jobName='ops_1h'. The next
--   ops_rollup_1h tick will call GetWatermark, get the zero-time
--   sentinel, fall into the bootstrap branch (cursor = MIN(sampled_at)
--   truncated to the hour), and iterate every bucket in order. Each
--   bucket is its own transaction (DELETE + 3 INSERTs + advance
--   watermark) so the catch-up is safe to interrupt — the next tick
--   resumes from where it left off.
--
-- Cost / runtime:
--   metric_ops_raw retains ~7 days of samples by default → ~168 buckets
--   to process. Each bucket aggregates ~10k-100k rows depending on
--   sample cadence. On the prod box (t3.medium) we expect this to
--   complete in 5-15 minutes; the scheduler reschedules the job every
--   5 min so even a slow run finishes before the next tick stomps it.
--
-- Cascade to _1d / _1mo:
--   ops_rollup_1d (interval 1h) reads from metric_ops_rollup_1h via the
--   ops_rollup_cascade pattern. Once the 1h table is back-filled, the
--   next 1d / 1mo ticks pick up the new rows naturally — no separate
--   reset needed.
--
-- Run after deploying the Hub binary that contains the SQL fix
-- (commit e3273a94). Running this against the OLD binary just makes
-- the empty rollup churn through 168 no-op buckets and resets the
-- watermark back to "now" within ~5 minutes — wasted CPU but no
-- corruption.

BEGIN;

-- Sanity: confirm the job exists in the watermark table before deleting.
-- If the row is missing (fresh DB), the next tick will start from MIN
-- naturally anyway — no harm done.
SELECT "jobName", "watermark", "updatedAt"
  FROM "rollup_watermark"
 WHERE "jobName" = 'ops_1h';

DELETE FROM "rollup_watermark" WHERE "jobName" = 'ops_1h';

COMMIT;

-- Verify the next ops_rollup_1h tick (within 5 min) actually fills the
-- table by running, after waiting one job interval:
--   SELECT COUNT(*) FROM metric_ops_rollup_1h
--    WHERE bucket_start >= NOW() - INTERVAL '24 hours';
-- The count should grow steadily as buckets are processed. Expect ~24
-- rows per metric per Thing for a fully back-filled 24h window.
