-- E90 #18(a): record the channel that initiated each admin mutation.
--
-- "via" = 'assistant' for an AI-initiated admin write performed by the web
-- assistant ("Chat with Nexus") on a user's behalf, NULL for a direct human/UI
-- action. The value is set server-side from the X-Nexus-Initiated-By header (stamped by
-- the assistant's self-call transport; never trusted from arbitrary clients for
-- any authorization decision) and is folded into the AdminAuditLog tamper-evident
-- hash chain (packages/nexus-hub/internal/traffic/chain/chain.go) so the
-- AI-attribution marker cannot be stripped without breaking integrityHash
-- (E90 invariant I5).
--
-- The hash recipe adds "via" with omitempty + sorted-key canonical encoding, so
-- every existing row and every future human/system row (via IS NULL) hashes
-- byte-identically to the pre-via recipe. No chain re-anchoring or backfill is
-- required; the column is plain nullable text.
--
-- Idempotent (IF NOT EXISTS) so re-applying is a no-op.

ALTER TABLE public."AdminAuditLog"
  ADD COLUMN IF NOT EXISTS "via" TEXT;

CREATE INDEX IF NOT EXISTS "AdminAuditLog_via_idx"
  ON public."AdminAuditLog" ("via");
