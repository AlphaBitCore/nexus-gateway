-- Drop the dead IdentityProvider.roleMapping column. It was decoded at load
-- time but never read by any login/JIT path — group-to-role mapping is driven
-- entirely by the IdpGroupMapping table (externalGroupId -> iamGroupId), which
-- has its own admin API, UI, and JIT wiring. roleMapping was a redundant,
-- unused duplicate; dev-phase policy bans parallel/legacy paths, so it is
-- removed outright rather than kept as a vestige.
--
-- Idempotent (IF EXISTS) so re-applying is a no-op.

ALTER TABLE public."IdentityProvider"
  DROP COLUMN IF EXISTS "roleMapping";
