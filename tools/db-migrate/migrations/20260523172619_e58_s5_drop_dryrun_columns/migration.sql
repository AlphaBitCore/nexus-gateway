-- E58-S5: drop dry-run-specific columns after Go code stopped referencing them.
-- KEEP estimated_cost_usd — that's the canonical total-cost field for all
-- traffic_event rows (real-call + previously-dry-run), populated on every
-- real dispatch from cost.Total. The column name is historical.

ALTER TABLE "traffic_event"
  DROP COLUMN IF EXISTS "is_dry_run",
  DROP COLUMN IF EXISTS "dry_run_assumptions";

ALTER TABLE "VirtualKey"
  DROP COLUMN IF EXISTS "dryRunRateLimitRpm";
