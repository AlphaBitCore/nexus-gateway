-- Promote `metadata.staticInfo.deviceFingerprint` (jsonb-buried) to a
-- first-class `thing.physical_id` column with a partial
-- UNIQUE(type, physical_id) index for type='agent'. This stops the
-- platform from minting duplicate `thing` rows for the same physical
-- agent device on every re-enrollment — the fingerprint-based dedupe
-- in Hub's enrollment handler couldn't enforce uniqueness at write
-- time without a DB constraint, so a race between two concurrent
-- enrollments (or a daemon dying before its first static_info push)
-- could still create duplicates even after the dedupe lookup landed.
--
-- The column is named `physical_id` (not `device_fingerprint`) because
-- its content varies by Thing type — for agents it's the sha256
-- fingerprint hash, for server services it's the yaml-configured id
-- or hostname+type+port auto-derivation (which equals thing.id today).
-- The UNIQUE constraint is gated on type='agent' so server services
-- can leave it NULL without colliding with each other.
--
-- Sequence is critical:
--   1. ADD COLUMN (NULL allowed)
--   2. Backfill agent fingerprints from existing metadata
--   3. Consolidate any duplicate (type='agent', physical_id) groups so
--      the UNIQUE index can be added without collision
--   4. CREATE UNIQUE INDEX

-- 1. ADD COLUMN.
ALTER TABLE thing ADD COLUMN IF NOT EXISTS physical_id TEXT;

-- 2. Backfill from existing metadata for agent rows. Older rows may
--    not have this — they'll be NULL and skipped by the partial index.
UPDATE thing
SET physical_id = metadata #>> '{staticInfo,deviceFingerprint}'
WHERE type = 'agent'
  AND physical_id IS NULL
  AND metadata ? 'staticInfo'
  AND (metadata->'staticInfo') ? 'deviceFingerprint'
  AND (metadata #>> '{staticInfo,deviceFingerprint}') <> '';

-- 3. Consolidate duplicate (type='agent', physical_id) groups before the
--    UNIQUE constraint blocks them. Behaviour:
--       a. Keep the row with the latest last_seen_at per group.
--       b. Release the active DeviceAssignment rows attached to the
--          duplicates (`releasedAt = NOW()`) so the partial unique
--          index `DeviceAssignment_deviceId_active_uidx` (WHERE
--          releasedAt IS NULL) doesn't reject the reparent step that
--          follows.
--       c. Reparent DeviceAssignment rows to the surviving canonical
--          thing (FK is ON DELETE RESTRICT, so we MUST move them
--          before deleting the thing row).
--       d. DELETE the duplicate thing rows — CASCADE handles
--          thing_agent / thing_service / diag rows / etc.;
--          enrollment_token uses SET NULL.
WITH ranked AS (
    SELECT
        id,
        physical_id AS pid,
        ROW_NUMBER() OVER (
            PARTITION BY type, physical_id
            ORDER BY last_seen_at DESC NULLS LAST, enrolled_at DESC
        ) AS rn
    FROM thing
    WHERE type = 'agent'
      AND physical_id IS NOT NULL
      AND physical_id <> ''
),
duplicate_ids AS (
    SELECT r.id
    FROM ranked r
    WHERE r.rn > 1
      AND r.pid IN (
          SELECT pid FROM ranked GROUP BY pid HAVING COUNT(*) > 1
      )
)
-- Step (b): release active assignments on the duplicate rows. They
-- become historical instead of conflicting active.
UPDATE "DeviceAssignment" da
SET "releasedAt" = NOW()
WHERE da."deviceId" IN (SELECT id FROM duplicate_ids)
  AND da."releasedAt" IS NULL;

-- Step (c): reparent ALL (now-released) DeviceAssignment rows to the
-- canonical thing so the audit trail survives the DELETE in step (d).
WITH ranked AS (
    SELECT
        id,
        physical_id AS pid,
        ROW_NUMBER() OVER (
            PARTITION BY type, physical_id
            ORDER BY last_seen_at DESC NULLS LAST, enrolled_at DESC
        ) AS rn
    FROM thing
    WHERE type = 'agent'
      AND physical_id IS NOT NULL
      AND physical_id <> ''
),
groups AS (
    SELECT pid, MIN(CASE WHEN rn = 1 THEN id END) AS canonical_id
    FROM ranked
    GROUP BY pid
    HAVING COUNT(*) > 1
),
duplicates AS (
    SELECT r.id AS old_id, g.canonical_id AS new_id
    FROM ranked r
    JOIN groups g ON g.pid = r.pid
    WHERE r.rn > 1
)
UPDATE "DeviceAssignment" da
SET "deviceId" = d.new_id
FROM duplicates d
WHERE da."deviceId" = d.old_id;

WITH ranked AS (
    SELECT
        id,
        ROW_NUMBER() OVER (
            PARTITION BY type, physical_id
            ORDER BY last_seen_at DESC NULLS LAST, enrolled_at DESC
        ) AS rn
    FROM thing
    WHERE type = 'agent'
      AND physical_id IS NOT NULL
      AND physical_id <> ''
)
DELETE FROM thing
WHERE id IN (SELECT id FROM ranked WHERE rn > 1);

-- 4. Partial UNIQUE — agent fingerprints only. Server services keep
--    physical_id NULL today; should they ever populate it (e.g. a
--    future yaml id pipeline) we can broaden this index then.
CREATE UNIQUE INDEX IF NOT EXISTS thing_type_physical_id_uniq
    ON thing(type, physical_id)
    WHERE type = 'agent' AND physical_id IS NOT NULL;
