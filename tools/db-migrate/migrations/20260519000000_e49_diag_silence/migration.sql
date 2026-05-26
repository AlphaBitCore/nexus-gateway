-- E49 — Diag-event silence registry.
--
-- The /infrastructure/errors page surfaces the diag stream as an issue
-- list grouped by (level, message_hash). Some of those issues are known
-- noise (e.g. the agent's "auto-updater disabled: no Ed25519 key" warning
-- repeats 30+ times a day on dev). Operators need an ack mechanism so
-- triage isn't dominated by repeats.
--
-- A row in `diag_silence` matches a (message_hash, level) pair. Hub's
-- groups endpoint joins it in to mark each row `silenced=true` so the UI
-- can collapse / de-emphasize them. `expires_at` NULL means a permanent
-- silence (use sparingly); a TTL silence auto-clears via index-only scan.
-- `silenced_by` is the admin actorId — never trust the client to populate.

CREATE TABLE "diag_silence" (
  "id"           UUID PRIMARY KEY,
  "message_hash" TEXT NOT NULL,
  "level"        TEXT NOT NULL,
  "silenced_by"  TEXT NOT NULL,
  "silenced_at"  TIMESTAMPTZ(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
  "expires_at"   TIMESTAMPTZ(3),
  "reason"       TEXT
);

-- Lookup index used by the groups JOIN: hash + level + active check.
-- (expires_at IS NULL OR expires_at > now()) selectivity is handled by
-- the partial index below.
CREATE INDEX "diag_silence_lookup_idx"
  ON "diag_silence" ("message_hash", "level");

-- Partial index for the TTL sweeper / "active silences" filter. Excludes
-- expired rows so an "active list" scan is proportional to live silences,
-- not the historical pile.
CREATE INDEX "diag_silence_active_idx"
  ON "diag_silence" ("expires_at")
  WHERE "expires_at" IS NULL OR "expires_at" > '2000-01-01'::timestamptz;
