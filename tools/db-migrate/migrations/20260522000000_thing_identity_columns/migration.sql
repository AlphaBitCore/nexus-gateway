-- Promote identity attributes from `metadata.staticInfo` (jsonb-buried)
-- and `thing_agent` (agent-only extension table) to first-class columns
-- on `thing`. Goal: make hostname / primary_ip / os / os_version
-- queryable + indexable + uniformly populated for every Thing type
-- (agent + services), so list / detail UIs don't have to dig into
-- jsonb to render identity, and operators can filter by IP / OS
-- without metadata extraction in SQL.
--
-- Schema additions (all nullable; legacy / sandboxed Things that can't
-- supply a value coexist):
--   thing.hostname           TEXT — OS hostname for agents,
--                                   container/EC2 hostname for services
--   thing.primary_ip         TEXT — last reported primary IP (agents:
--                                   local NIC; services: listen address)
--   thing.os                 TEXT — 'darwin' | 'linux' | 'windows'
--   thing.os_version         TEXT — semver/build identifier
--
-- After Phase 1, `thing_agent.hostname / os / os_version` become
-- redundant; Phase 6 (separate migration) drops them once all readers
-- are switched to `thing.*`.

-- 1. ADD COLUMNs.
ALTER TABLE thing ADD COLUMN IF NOT EXISTS hostname    TEXT;
ALTER TABLE thing ADD COLUMN IF NOT EXISTS primary_ip  TEXT;
ALTER TABLE thing ADD COLUMN IF NOT EXISTS os          TEXT;
ALTER TABLE thing ADD COLUMN IF NOT EXISTS os_version  TEXT;

-- 2. Backfill from thing_agent (authoritative for agents that have
--    already heartbeated; agent_device.go SELECTs hostname/os/os_version
--    from there today). Doesn't touch service Things.
UPDATE thing t
SET hostname   = COALESCE(t.hostname,   ta.hostname),
    os         = COALESCE(t.os,         ta.os),
    os_version = COALESCE(t.os_version, ta.os_version)
FROM thing_agent ta
WHERE ta.thing_id = t.id
  AND (t.hostname IS NULL OR t.os IS NULL OR t.os_version IS NULL);

-- 3. Backfill primary_ip from metadata.staticInfo for any thing that
--    has it (agents + services that emit primaryIp in staticInfo).
UPDATE thing
SET primary_ip = metadata #>> '{staticInfo,primaryIp}'
WHERE primary_ip IS NULL
  AND metadata ? 'staticInfo'
  AND (metadata->'staticInfo') ? 'primaryIp'
  AND (metadata #>> '{staticInfo,primaryIp}') <> '';

-- 4. Backfill hostname from metadata.staticInfo as a secondary source
--    (catches services where thing_agent has no row).
UPDATE thing
SET hostname = metadata #>> '{staticInfo,hostname}'
WHERE hostname IS NULL
  AND metadata ? 'staticInfo'
  AND (metadata->'staticInfo') ? 'hostname'
  AND (metadata #>> '{staticInfo,hostname}') <> '';

-- 5. Backfill os / os_version from metadata.staticInfo (same rationale).
UPDATE thing
SET os = metadata #>> '{staticInfo,os}'
WHERE os IS NULL
  AND metadata ? 'staticInfo'
  AND (metadata->'staticInfo') ? 'os'
  AND (metadata #>> '{staticInfo,os}') <> '';

UPDATE thing
SET os_version = metadata #>> '{staticInfo,osVersion}'
WHERE os_version IS NULL
  AND metadata ? 'staticInfo'
  AND (metadata->'staticInfo') ? 'osVersion'
  AND (metadata #>> '{staticInfo,osVersion}') <> '';

-- 6. Index on primary_ip for the planned "filter agents by subnet" UI
--    use case. Hostname / os are typically filtered via Q-search
--    (LIKE %hostname%) which doesn't benefit from a btree.
CREATE INDEX IF NOT EXISTS thing_primary_ip_idx ON thing(primary_ip)
    WHERE primary_ip IS NOT NULL;
