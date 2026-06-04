-- E90 P2b / NFR-10: durable record of a parked dangerous-write confirmation.
--
-- The in-memory confirm registry owns the live rendezvous channel; this table mirrors
-- the parked entry so a POST /confirm that misses the in-memory map can distinguish
-- "the pod restarted — re-issue the action" from "expired / unknown". makeConfirm
-- INSERTs the row on register and DELETEs it when the confirm resolves (defer), so a
-- row that outlives the confirm timeout is an orphan left by a process restart.
--
-- It does NOT resume the write after a restart (the turn runs in an in-memory goroutine
-- that is gone) — it drives the clearer re-issue error and cross-pod visibility only.
--
-- Strong per-user isolation (E90 invariant I3): the owning "userId" comes from the
-- authenticated request context, every runtime query filters by it, and the FK to
-- NexusUser ON DELETE CASCADE removes the rows when the user is deleted.
--
-- Idempotent (IF NOT EXISTS) so re-applying is a no-op.

CREATE TABLE IF NOT EXISTS public."AssistantPendingConfirm" (
  "key"            TEXT PRIMARY KEY,
  "userId"         TEXT NOT NULL REFERENCES public."NexusUser"("id") ON DELETE CASCADE,
  "sessionId"      TEXT NOT NULL,
  "callId"         TEXT NOT NULL,
  "tool"           TEXT NOT NULL,
  "input"          JSONB NOT NULL,
  "reason"         TEXT NOT NULL DEFAULT '',
  "requiresSecond" BOOLEAN NOT NULL DEFAULT false,
  "isProd"         BOOLEAN NOT NULL DEFAULT false,
  "createdAt"      TIMESTAMPTZ(3) NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS "AssistantPendingConfirm_userId_createdAt_idx"
  ON public."AssistantPendingConfirm" ("userId", "createdAt");
