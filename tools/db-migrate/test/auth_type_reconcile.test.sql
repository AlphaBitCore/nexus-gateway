-- Requires a test DB where both migrations have been applied.
-- Run: psql $TEST_DB -f tools/db-migrate/test/auth_type_reconcile.test.sql
-- Exits non-zero if any mtls agent still shows auth_type=bearer.

DO $$
DECLARE
    bad_count int;
BEGIN
    SELECT COUNT(*) INTO bad_count
    FROM thing t
    JOIN thing_agent ta ON ta.thing_id = t.id
    WHERE t.type = 'agent'
      AND t.auth_type = 'bearer'
      AND ta.cert_serial IS NOT NULL
      AND ta.cert_serial <> '';

    IF bad_count > 0 THEN
        RAISE EXCEPTION 'auth_type reconcile failed: % mtls agents still marked bearer', bad_count;
    END IF;
END $$;
