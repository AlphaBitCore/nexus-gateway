-- Multi-key tracking for AdminApiKey: supports rotation, expiry, and an
-- explicit "unavailable" status (operator disablement separate from the
-- existing boolean `enabled` flag).
--
-- Lifecycle states (single source of truth — every row carries one):
--   * active       — current, accepted by the auth middleware
--   * rotating     — superseded by a newer key but still accepted during the
--                    rotation window so callers can swap in the new value
--                    without service interruption
--   * expired      — past expiresAt OR explicitly retired by the operator;
--                    rejected by auth middleware
--   * unavailable  — operator-marked as compromised / withdrawn; rejected by
--                    auth middleware (distinct from expiry so audit can
--                    distinguish "natural sunset" from "active revocation")
--
-- The existing `enabled BOOLEAN` column is kept (operator quick-toggle); the
-- middleware now requires BOTH enabled=true AND status IN ('active','rotating').
ALTER TABLE "AdminApiKey"
  ADD COLUMN status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'rotating', 'expired', 'unavailable'));

-- Stamp of the moment this row was rotated out (status moved active→rotating).
-- NULL for rows that have never been rotated.
ALTER TABLE "AdminApiKey"
  ADD COLUMN "rotatedAt" TIMESTAMPTZ NULL;

-- Forward link: when a new row is minted as the rotation successor, this
-- column points back to the row it replaces. The predecessor's status
-- transitions to 'rotating' in the same transaction. ON DELETE SET NULL so
-- purging the old row does not cascade-delete the active successor.
ALTER TABLE "AdminApiKey"
  ADD COLUMN "rotatedFromId" TEXT NULL
    REFERENCES "AdminApiKey"(id) ON DELETE SET NULL;

-- Fast lookup: "all currently-active keys for owner X" — used by the rotate
-- handler to decide whether the requesting key is already part of an
-- in-flight rotation chain, and by future bulk-revocation tooling.
CREATE INDEX IF NOT EXISTS "AdminApiKey_owner_active_partial_idx"
  ON "AdminApiKey" ("ownerUserId")
  WHERE status = 'active';
