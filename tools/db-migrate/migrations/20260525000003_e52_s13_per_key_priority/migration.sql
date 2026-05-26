-- E52-S13: per-key priority overrides on device_group_config rows.
--
-- Today `DeviceGroup.priority` ranks the group across ALL config keys.
-- Some operator workflows want different rankings per-key — e.g.
-- "hook_config from compliance-group should always win, but
--  agent_settings from regional-group should win for that region".
--
-- Adding `device_group_config.priority_override INT NULL` lets a row
-- override its parent group's priority just for that key. ResolveConfig
-- uses `COALESCE(dgc.priority_override, g.priority)` in the ORDER BY,
-- so the resolution model stays a single integer with deterministic
-- group_id tiebreak — just sourced from the row when present.
--
-- Schema additive only; existing rows have NULL and inherit group
-- priority (today's behaviour).

ALTER TABLE device_group_config ADD COLUMN IF NOT EXISTS priority_override INTEGER NULL;
