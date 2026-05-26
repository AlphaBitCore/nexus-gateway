-- Prod sync after migrations dir reset (baseline + 4 in-flight).
--
-- ┌─────────────────────────────────────────────────────────────────────────┐
-- │ METADATA-ONLY SCRIPT — DOES NOT TOUCH SCHEMA OR APPLICATION DATA.       │
-- │                                                                         │
-- │ This file rewrites rows in `_prisma_migrations` so Prisma's bookkeeping │
-- │ matches the new migrations dir layout. It deliberately does NOT replay  │
-- │ the baseline migration SQL — prod already has those tables from the 60 │
-- │ originals it collapses. Running the baseline.sql against prod would     │
-- │ raise "relation X already exists" on every CREATE TABLE.                │
-- │                                                                         │
-- │ DO NOT pass this script through `prisma migrate`. Use plain psql.       │
-- └─────────────────────────────────────────────────────────────────────────┘
--
-- Context: tools/db-migrate/migrations/ was rebuilt to a single baseline
-- (00000000000000_baseline_2026_05_13) plus the e51/e48/e27 in-flight
-- migrations on top. Prod's _prisma_migrations table still records the 60
-- original migrations that have been collapsed into the baseline, so
-- `prisma migrate deploy` would refuse to run.
--
-- This script rebuilds _prisma_migrations to match the new layout: it deletes
-- the 60 collapsed rows and inserts a single baseline row marked applied. The
-- 3 in-flight migrations (e51, e48 S4+S5, e27) are NOT pre-inserted — they
-- remain "pending" so the next `prisma migrate deploy` applies them
-- (baseline is now marked applied, so deploy skips it).
--
-- We INSERT directly instead of `prisma migrate resolve --applied` because
-- resolve has been observed to have side effects beyond a simple row insert.
-- Direct SQL is reversible and auditable.
--
-- USAGE
--   1. Back up _prisma_migrations on prod:
--        pg_dump … --table=_prisma_migrations > _prisma_migrations.backup.sql
--   2. Run this script:
--        psql … -v ON_ERROR_STOP=1 -f sync_baseline_2026_05_13.sql
--   3. Verify exactly one row exists:
--        SELECT migration_name, applied_steps_count, finished_at IS NOT NULL
--        FROM _prisma_migrations;
--   4. Deploy:
--        cd tools/db-migrate && npx prisma migrate deploy
--      This applies the 4 in-flight migrations and leaves baseline untouched.
--
-- The checksum below is SHA256 of
-- migrations/00000000000000_baseline_2026_05_13/migration.sql at the
-- time this script was written. If the baseline file is regenerated, recompute
-- via `sha256sum migration.sql` and update both the file and this script.

BEGIN;

-- Snapshot count for the human running this; ROLLBACK if it doesn't look right.
\echo 'BEFORE — rows in _prisma_migrations:'
SELECT COUNT(*) FROM _prisma_migrations;

-- Drop the 60 rows that have been collapsed into the baseline. Keep every
-- migration that the new migrations/ directory still ships so `prisma
-- migrate deploy` doesn't try to re-apply them. The list MUST stay in sync
-- with `tools/db-migrate/migrations/` going forward — when a new migration
-- has already been applied to the source DB manually (e.g. an E49-style
-- hotfix), add its name here before re-running the sync.
DELETE FROM _prisma_migrations
WHERE migration_name NOT IN (
  '00000000000000_baseline_2026_05_13',
  '20260513103821_e51_per_thing_traffic_rollup',
  '20260517000000_e48_gateway_passthrough_config_3tier',
  '20260517000010_e48_traffic_event_passthrough_columns',
  '20260518000000_e27_reported_outcomes_and_process_started_at',
  '20260519000000_e49_diag_silence',
  '20260520000000_fix_e48_passthrough_fk'
);

-- Insert the baseline as applied (only if the idempotent UPDATE above did
-- not already rename an existing row). The 4 in-flight migrations are
-- intentionally omitted so `prisma migrate deploy` picks them up.
INSERT INTO _prisma_migrations (
  id,
  checksum,
  finished_at,
  migration_name,
  logs,
  rolled_back_at,
  started_at,
  applied_steps_count
)
SELECT
  gen_random_uuid()::text,
  '6d6eee82c318f0bcf35c38f7de0828e07766846cc6bb6bdce38bcec7df2e1acb',
  NOW(),
  '00000000000000_baseline_2026_05_13',
  NULL,
  NULL,
  NOW(),
  1
WHERE NOT EXISTS (
  SELECT 1 FROM _prisma_migrations
   WHERE migration_name = '00000000000000_baseline_2026_05_13'
);

\echo 'AFTER — rows in _prisma_migrations:'
SELECT migration_name, applied_steps_count, finished_at IS NOT NULL AS finished
  FROM _prisma_migrations
  ORDER BY started_at;

COMMIT;
