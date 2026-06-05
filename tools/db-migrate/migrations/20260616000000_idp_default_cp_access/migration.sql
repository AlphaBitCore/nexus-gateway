-- Per-IdP default for whether JIT-provisioned (OIDC/SAML) users may access the
-- Control Plane. One IdP can federate both CP admins and agent end-users, so the
-- grant is admin-controlled per IdP rather than a global JIT default. Pairs with
-- the existing "defaultRole" column, which the JIT path now applies as a baseline
-- IamGroup membership.
--
-- Defaults false: a federated user is not granted CP access unless the admin opts
-- the IdP in. Existing rows therefore keep today's behaviour (JIT users had
-- canAccessControlPlane hardcoded false).
--
-- Idempotent (IF NOT EXISTS) so re-applying is a no-op.

ALTER TABLE public."IdentityProvider"
  ADD COLUMN IF NOT EXISTS "defaultControlPlaneAccess" BOOLEAN NOT NULL DEFAULT false;

-- The JIT path resolves "defaultRole" against IamGroup.name to grant a baseline
-- group. The historical default 'developer' (singular) matches no seeded group
-- ('developers' is the real one), so it silently granted nothing. Fix the column
-- default and re-point existing rows still carrying the stale singular value.
ALTER TABLE public."IdentityProvider"
  ALTER COLUMN "defaultRole" SET DEFAULT 'developers';

UPDATE public."IdentityProvider"
   SET "defaultRole" = 'developers'
 WHERE "defaultRole" = 'developer';
