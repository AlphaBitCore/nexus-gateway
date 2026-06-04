-- E90 P6: web assistant ("Chat with Nexus") persistence.
--
-- Three tables index the assistant's per-user data: conversation sessions,
-- durable memory, and sandbox file metadata. The large CONTENT (session
-- transcript, file bytes) lives in object storage, keyed by userId; these tables
-- are the queryable DB index for listing / CRUD.
--
-- Strong per-user isolation (E90 invariant I3): every runtime query filters by
-- "userId", and the FK to NexusUser ON DELETE CASCADE removes a user's assistant
-- data when the user is deleted. The owning userId is always taken from the
-- authenticated request context, never from client input.
--
-- Idempotent (IF NOT EXISTS) so re-applying is a no-op.

CREATE TABLE IF NOT EXISTS public."AssistantSession" (
  "id"        TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
  "userId"    TEXT NOT NULL REFERENCES public."NexusUser"("id") ON DELETE CASCADE,
  "title"     TEXT NOT NULL DEFAULT '',
  "model"     TEXT NOT NULL DEFAULT '',
  "msgCount"  INTEGER NOT NULL DEFAULT 0,
  "lastSeq"   INTEGER NOT NULL DEFAULT 0,
  "spillRef"  JSONB,
  "createdAt" TIMESTAMPTZ(3) NOT NULL DEFAULT now(),
  "updatedAt" TIMESTAMPTZ(3) NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS "AssistantSession_userId_updatedAt_idx"
  ON public."AssistantSession" ("userId", "updatedAt");

CREATE TABLE IF NOT EXISTS public."AssistantMemory" (
  "userId"    TEXT NOT NULL REFERENCES public."NexusUser"("id") ON DELETE CASCADE,
  "name"      TEXT NOT NULL,
  "type"      TEXT NOT NULL,
  "body"      TEXT NOT NULL,
  "updatedAt" TIMESTAMPTZ(3) NOT NULL DEFAULT now(),
  PRIMARY KEY ("userId", "name")
);

CREATE TABLE IF NOT EXISTS public."AssistantFile" (
  "id"          TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
  "userId"      TEXT NOT NULL REFERENCES public."NexusUser"("id") ON DELETE CASCADE,
  "sessionId"   TEXT NOT NULL,
  "name"        TEXT NOT NULL,
  "size"        INTEGER NOT NULL,
  "contentType" TEXT NOT NULL DEFAULT 'application/octet-stream',
  "spillRef"    JSONB NOT NULL,
  "createdAt"   TIMESTAMPTZ(3) NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS "AssistantFile_userId_sessionId_idx"
  ON public."AssistantFile" ("userId", "sessionId");
