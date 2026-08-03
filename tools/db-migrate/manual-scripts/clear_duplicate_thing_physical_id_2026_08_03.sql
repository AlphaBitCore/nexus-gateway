-- clear_duplicate_thing_physical_id_2026_08_03.sql
--
-- WHY: schema-extras.sql creates
--     thing_type_physical_id_uniq
--       UNIQUE (type, physical_id) WHERE type='agent' AND physical_id IS NOT NULL
--   which is what keeps an agent's thing.id stable across reinstalls (the Hub
--   matches the hardware fingerprint on re-enroll instead of issuing a new id).
--   A database that already holds two 'agent' rows sharing one physical_id
--   cannot create that index, so `CREATE UNIQUE INDEX` fails with
--     could not create unique index "thing_type_physical_id_uniq"
--     DETAIL: Key (type, physical_id)=(agent, <fingerprint>) is duplicated.
--   Because the extras file is applied as one script and stops at its first
--   failing statement, that single conflict silently skips EVERY object declared
--   after it. Observed on a developer database: 29 of the 41 extras indexes were
--   missing, including exemption_request_pending_dedup_uniq (agent exemption
--   upserts then fail with "no unique or exclusion constraint matching the ON
--   CONFLICT specification"), DeviceAssignment_deviceId_active_uidx, both
--   VirtualKey name-uniqueness indexes and all four uq_ops_rollup_* keys.
--
--   Duplicates originate from re-enrollments that predate the index: the same
--   machine enrolled twice and produced two thing rows, and nothing rejected the
--   second one because the index that would have rejected it does not exist yet.
--
-- TARGET: any environment whose `thing` table predates
--   thing_type_physical_id_uniq (dev / staging / prod). Run it BEFORE applying
--   schema-extras.sql. A database that already carries the index cannot hold a
--   duplicate, so the script is a no-op there.
--
-- IDEMPOTENT: yes. It only touches rows that are still part of a duplicate
--   group, and re-running finds none.
--
-- NOT DESTRUCTIVE, AND REVERSIBLE: no row is deleted and no value is lost. The
--   index predicate excludes `physical_id IS NULL`, so clearing physical_id on
--   the stale duplicates is enough to satisfy uniqueness. Deleting the row
--   instead would cascade (thing_service, thing_config_override, metric_ops_raw
--   and the other thing-referencing tables are ON DELETE CASCADE) and would take
--   real history with it. The retained row is the one with the newest updated_at
--   — the live registration; the older rows are the superseded ghosts.
--
--   Every cleared value is first copied into `thing_physical_id_dedup_backup`
--   (created by this script), so the edit can be undone exactly:
--     UPDATE thing t SET physical_id = b.physical_id
--     FROM thing_physical_id_dedup_backup b WHERE t.id = b.thing_id;
--   This matters on environments that hold real fleet history: a hardware
--   fingerprint is not regenerable from anything else in the row, so it is
--   preserved rather than overwritten. Drop the backup table once the index is
--   in place and the fleet list has been eyeballed.
--
--   `updated_at` is deliberately NOT touched. It is Prisma's @updatedAt column,
--   i.e. the record of when the row last really changed; an administrative
--   uniqueness fix is not a fleet event, and bumping it would misdate the row in
--   every list ordered by it. Liveness is decided by last_seen_at
--   (fleet/store/thing_liveness.go), which this script also leaves alone, so no
--   node's online/offline state moves.
--
-- AFTER RUNNING: apply schema-extras.sql (or re-run scripts/dev-start.sh), then
--   confirm the index exists:
--     SELECT indexname FROM pg_indexes WHERE indexname = 'thing_type_physical_id_uniq';

BEGIN;

-- Report what is about to change, so the operator sees the affected nodes in the
-- transaction log before it commits.
DO $$
DECLARE
    r record;
    n int := 0;
BEGIN
    FOR r IN
        SELECT physical_id, count(*) AS dup_rows
        FROM thing
        WHERE type = 'agent' AND physical_id IS NOT NULL
        GROUP BY physical_id
        HAVING count(*) > 1
    LOOP
        RAISE NOTICE 'duplicate physical_id % held by % agent rows', r.physical_id, r.dup_rows;
        n := n + 1;
    END LOOP;

    IF n = 0 THEN
        RAISE NOTICE 'no duplicate (type=agent, physical_id) groups — nothing to clear';
    ELSE
        RAISE NOTICE '% duplicate group(s); keeping the newest updated_at row in each and clearing physical_id on the rest', n;
    END IF;
END $$;

-- The losing rows of each duplicate group: everything except the newest. ctid is
-- the final tiebreaker so the ranking is total even when two rows in a group
-- share updated_at — without it the window could keep two rows and leave the
-- conflict in place.
CREATE TEMP TABLE dedup_losers AS
SELECT id, physical_id
FROM (
    SELECT id,
           physical_id,
           row_number() OVER (
               PARTITION BY physical_id
               ORDER BY updated_at DESC, ctid DESC
           ) AS rn
    FROM thing
    WHERE type = 'agent' AND physical_id IS NOT NULL
      AND physical_id IN (
          SELECT physical_id
          FROM thing
          WHERE type = 'agent' AND physical_id IS NOT NULL
          GROUP BY physical_id
          HAVING count(*) > 1
      )
) ranked
WHERE rn > 1;

-- Preserve every value before clearing it, so the edit is reversible. Keyed on
-- thing_id so a re-run cannot double-insert, and never overwritten: the first
-- backup of a row is the one that holds its original fingerprint.
CREATE TABLE IF NOT EXISTS thing_physical_id_dedup_backup (
    thing_id    text        PRIMARY KEY,
    physical_id text        NOT NULL,
    cleared_at  timestamptz NOT NULL DEFAULT now()
);

INSERT INTO thing_physical_id_dedup_backup (thing_id, physical_id)
SELECT id, physical_id FROM dedup_losers
ON CONFLICT (thing_id) DO NOTHING;

-- Clear physical_id on the losing rows only. updated_at is left as-is on purpose
-- (see the header): this is an administrative uniqueness fix, not a fleet event.
UPDATE thing
SET physical_id = NULL
WHERE id IN (SELECT id FROM dedup_losers);

-- Refuse to commit unless every cleared value is recoverable from the backup.
DO $$
DECLARE
    unbacked int;
BEGIN
    SELECT count(*) INTO unbacked
    FROM dedup_losers l
    LEFT JOIN thing_physical_id_dedup_backup b ON b.thing_id = l.id
    WHERE b.thing_id IS NULL;

    IF unbacked > 0 THEN
        RAISE EXCEPTION 'refusing to commit: % cleared row(s) have no backup entry', unbacked;
    END IF;
END $$;

DROP TABLE dedup_losers;

-- Prove the blocker is gone before committing: this is the exact predicate the
-- index enforces, so an empty result means CREATE UNIQUE INDEX will now succeed.
DO $$
DECLARE
    remaining int;
BEGIN
    SELECT count(*) INTO remaining
    FROM (
        SELECT 1
        FROM thing
        WHERE type = 'agent' AND physical_id IS NOT NULL
        GROUP BY type, physical_id
        HAVING count(*) > 1
    ) dups;

    IF remaining > 0 THEN
        RAISE EXCEPTION 'still % duplicate (type=agent, physical_id) group(s) after the update — thing_type_physical_id_uniq would still fail', remaining;
    END IF;

    RAISE NOTICE 'no duplicate (type=agent, physical_id) groups remain — thing_type_physical_id_uniq can be created';
END $$;

COMMIT;
