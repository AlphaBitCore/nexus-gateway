-- Phase 6 cleanup: drop the now-redundant identity columns from
-- thing_agent. Phase 1 promoted hostname / os / os_version up to
-- first-class thing.* columns; the Hub heartbeat handler + enrollment
-- helper now write them there, and every CP reader has been migrated
-- to SELECT t.hostname / t.os / t.os_version.
--
-- DROP COLUMN is destructive. Any rolling restart that mixes old +
-- new binaries would have the old binary still reading from
-- thing_agent.hostname and panicking on missing column. Apply this
-- migration ONLY after the new binaries are fully rolled out (the
-- production deploy in Phase 8 takes care of this — all 4 services
-- restart together).

ALTER TABLE thing_agent DROP COLUMN IF EXISTS hostname;
ALTER TABLE thing_agent DROP COLUMN IF EXISTS os;
ALTER TABLE thing_agent DROP COLUMN IF EXISTS os_version;
