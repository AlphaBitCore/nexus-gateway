-- OAuth client admin (issue #40) — schema-side prerequisites.
--
-- Three gaps surfaced while designing the CP-UI admin page; all three
-- are fixed in one migration because each blocks a specific UX feature
-- and they all touch the same two tables.
--
-- Gap 1: RefreshToken.client FK was created without ON DELETE behaviour,
-- which defaults to RESTRICT in Postgres. Today every attempt to delete
-- an OAuthClient that has even one row in RefreshToken fails with a FK
-- violation. The admin Delete flow needs this to cascade — orphan
-- refresh tokens cannot be redeemed anyway because the token-exchange
-- path re-checks client_id against the OAuthClient row.
--
-- Gap 2: OAuthClient had no record of "when was the secret last
-- rotated". The detail-page Authentication card needs to show this so
-- admins can judge whether a stale-secret incident is plausible. NULL
-- means "never rotated since creation".
--
-- Gap 3: RefreshToken had no index on clientId. The detail page's
-- Activity card runs CountActiveRefreshTokens(clientId) on every load;
-- without the index that is O(table size). Postgres does not auto-
-- index FK columns on the child side, so we add it explicitly. This
-- also speeds up the cascade walk for the new ON DELETE CASCADE rule.

ALTER TABLE "RefreshToken" DROP CONSTRAINT "RefreshToken_clientId_fkey";

-- ON UPDATE NO ACTION (default): OAuthClient.id is immutable by design — the
-- admin PATCH endpoint refuses to rename ids. Marking ON UPDATE CASCADE
-- would only describe a path the application layer guarantees never fires.
ALTER TABLE "RefreshToken"
    ADD CONSTRAINT "RefreshToken_clientId_fkey"
    FOREIGN KEY ("clientId") REFERENCES "OAuthClient"("id")
    ON DELETE CASCADE;

ALTER TABLE "OAuthClient"
    ADD COLUMN "lastSecretRotatedAt" TIMESTAMPTZ(3);

CREATE INDEX "RefreshToken_clientId_idx" ON "RefreshToken"("clientId");
