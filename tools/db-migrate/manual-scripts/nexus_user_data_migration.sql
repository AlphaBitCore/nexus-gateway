-- Data migration: AdminUser → NexusUser
-- Run AFTER the Prisma structural migration that creates the NexusUser and
-- DeviceAssignment tables (i.e. after `npx prisma migrate deploy`).
--
-- Purpose:
--   Copy all existing AdminUser rows into NexusUser so that:
--   1. Existing admin dashboard sessions and API keys continue to resolve.
--   2. Each AdminUser becomes a NexusUser with canAccessControlPlane=true.
--   3. The ownerUserId FK on AdminApiKey is left unchanged because the column
--      value (UUID) is shared between AdminUser.id and the new NexusUser.id.
--
-- Safety:
--   Uses ON CONFLICT (id) DO NOTHING so the script is idempotent and safe to
--   re-run if it is interrupted or replayed in a retry scenario.
--
-- AdminUser is retained unchanged for 30 days post-migration to allow
-- any legacy code paths to degrade gracefully.  It will be dropped in a
-- follow-up migration once all references have been confirmed absent.

INSERT INTO "NexusUser" (
  id,
  "organizationId",
  "displayName",
  email,
  status,
  "canAccessControlPlane",
  "passwordHash",
  "lastLoginAt",
  "ssoSubject",
  "createdBy",
  "createdAt",
  "updatedAt"
)
SELECT
  id,
  'default'                                                      AS "organizationId",
  username                                                       AS "displayName",
  email,
  CASE WHEN enabled THEN 'active' ELSE 'suspended' END           AS status,
  true                                                           AS "canAccessControlPlane",
  "hashedPassword"                                               AS "passwordHash",
  "lastLoginAt",
  "oidcSubject"                                                  AS "ssoSubject",
  "createdBy",
  "createdAt",
  "updatedAt"
FROM "AdminUser"
ON CONFLICT (id) DO NOTHING;
