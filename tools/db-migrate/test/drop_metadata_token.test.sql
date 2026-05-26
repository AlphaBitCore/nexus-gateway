-- Assertion test for migration 20260418010000_drop_metadata_token.
--
-- Verifies that no thing row retains a plaintext metadata.token key after
-- the migration has been applied.

DO $$
DECLARE
    offenders int;
BEGIN
    SELECT COUNT(*) INTO offenders FROM thing WHERE metadata ? 'token';
    IF offenders > 0 THEN
        RAISE EXCEPTION 'drop_metadata_token failed: % rows still have plaintext token', offenders;
    END IF;
END $$;
