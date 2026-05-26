-- RevokedToken: store JTI / Session ID as text, not UUID.
--
-- The OAuth access-token JTI is a base64url-encoded random string
-- (token.NewJTI() → 16-byte crypto/rand, encoded ~22 chars). It is
-- NOT formatted as a hyphenated UUID and the pre-existing UUID column
-- type rejected the natural value with `invalid input syntax for type
-- uuid (SQLSTATE 22P02)` — every /oauth/revoke call silently failed
-- to insert a row, leaving RFC 7009 revocation unenforced.
--
-- Session ID can be either form depending on issuer; text accepts both.
--
-- Idempotent: the cast text -> text on a column that has already been
-- migrated is a no-op for production environments that already ran a
-- prior fix.
ALTER TABLE "RevokedToken" ALTER COLUMN "targetJti" TYPE text;
ALTER TABLE "RevokedToken" ALTER COLUMN "targetSessionId" TYPE text;
