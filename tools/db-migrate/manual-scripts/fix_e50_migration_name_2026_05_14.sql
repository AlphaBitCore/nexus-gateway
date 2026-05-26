-- Rename the prod-applied E50 phase-columns migration row so it matches the
-- (renamed) repo folder.
--
-- Background:
--   On 2026-05-14 production was found to be missing the 5 phase columns
--   (`upstream_ttfb_ms`, `upstream_total_ms`, `request_hooks_ms`,
--   `response_hooks_ms`, `latency_breakdown`) on `traffic_event`, which is what
--   Hub's TrafficEventWriter expects to insert into. Every batch flush failed
--   with `42703 column ... does not exist`, freezing the audit pipeline for
--   ~16 hours.
--
--   Root cause: two migrations in the repo shared the same timestamp prefix:
--     20260522000000_e50_traffic_event_latency_phases
--     20260522000000_thing_identity_columns
--   Prisma's `migrate deploy` treats migrations by their full folder name as
--   the unique key, but in practice the directory-walk + sort step collapsed
--   the two onto the same timestamp slot, so once `thing_identity_columns`
--   was applied, prisma considered the slot "done" and never touched
--   `_e50_traffic_event_latency_phases`. The E50 migration was silently
--   skipped on every subsequent `migrate deploy`.
--
-- Hotfix already applied to prod (2026-05-14 07:32 UTC):
--   Manual SQL: 5 ALTER TABLE / 5 CHECK constraints + INSERT into
--   `_prisma_migrations` with name = `20260522000000_e50_traffic_event_latency_phases`.
--
-- Permanent repo fix (this commit):
--   The folder is renamed to `20260522000001_e50_traffic_event_latency_phases`
--   so the timestamp prefix is unique. To keep `prisma migrate deploy`
--   idempotent against prod, this script renames the corresponding row in
--   `_prisma_migrations` so its `migration_name` matches the new folder name.
--
-- Apply on prod with:
--   PGPASSWORD=... psql -h localhost -U nexus -d nexus_gateway \
--     -v ON_ERROR_STOP=1 -f fix_e50_migration_name_2026_05_14.sql
--
-- Safe to run more than once: the UPDATE is a no-op if already applied
-- (WHERE clause becomes empty).

BEGIN;

UPDATE "_prisma_migrations"
SET migration_name = '20260522000001_e50_traffic_event_latency_phases'
WHERE migration_name = '20260522000000_e50_traffic_event_latency_phases';

-- Sanity check: there must be exactly one row matching the new name and zero
-- matching the old.
DO $$
DECLARE
  new_count INT;
  old_count INT;
BEGIN
  SELECT count(*) INTO new_count FROM "_prisma_migrations"
    WHERE migration_name = '20260522000001_e50_traffic_event_latency_phases';
  SELECT count(*) INTO old_count FROM "_prisma_migrations"
    WHERE migration_name = '20260522000000_e50_traffic_event_latency_phases';
  IF new_count <> 1 OR old_count <> 0 THEN
    RAISE EXCEPTION 'fix_e50_migration_name: unexpected counts new=% old=%', new_count, old_count;
  END IF;
END $$;

COMMIT;
