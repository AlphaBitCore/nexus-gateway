-- E48 passthrough FK alignment with Prisma canonical naming.
--
-- The original e48 S1 migration (20260517000000_e48_gateway_passthrough_config_3tier)
-- created the FK on gateway_passthrough_config_provider.provider_id with:
--   * Non-canonical name "gateway_passthrough_provider_provider_fk"
--   * No ON UPDATE clause (defaults to NO ACTION)
--
-- Prisma's relation handling expects the canonical name
-- "gateway_passthrough_config_provider_provider_id_fkey" and
-- ON UPDATE CASCADE. Every `prisma migrate dev` since e48 landed has
-- generated a spurious DROP+ADD diff that operators kept ignoring.
--
-- Rename + add ON UPDATE CASCADE so the schema matches Prisma's
-- inference. Migration is a no-op for runtime behavior (DELETE
-- CASCADE preserved; UPDATE CASCADE is a non-event because
-- Provider.id is a UUID that never changes in practice).

ALTER TABLE "gateway_passthrough_config_provider"
  DROP CONSTRAINT "gateway_passthrough_provider_provider_fk";

ALTER TABLE "gateway_passthrough_config_provider"
  ADD CONSTRAINT "gateway_passthrough_config_provider_provider_id_fkey"
  FOREIGN KEY ("provider_id")
  REFERENCES "Provider"("id")
  ON DELETE CASCADE
  ON UPDATE CASCADE;
