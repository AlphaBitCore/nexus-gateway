-- E52-S1 — Group-scoped config resolution.
--
-- Lifts the per-device config concept up one level: an admin can now
-- attach a (config_key, state) payload to any DeviceGroup, and Hub
-- resolves the effective state per device via the cascade
--
--   override > group > fleet
--
-- where the group winner is the highest-priority group the device
-- belongs to. Same cascade applies to every Cat A and Cat B key in
-- thing_config_template — agent_settings, hook_config, payload_capture,
-- interception_domains, kill_switch, prompt_cache, etc. — through one
-- new resolver, no per-key code changes.
--
-- Schema is additive only. Existing per-thing overrides
-- (`thing_config_override`) and fleet defaults (`thing_config_template`)
-- continue to behave identically; empty `device_group_config` collapses
-- the cascade back to today's two-tier behaviour.

-- 1. Priority column on DeviceGroup. Higher = higher rank. Ops re-ranks
--    by editing this single integer per group.
ALTER TABLE "DeviceGroup" ADD COLUMN IF NOT EXISTS priority INTEGER NOT NULL DEFAULT 100;

-- 2. Per-group config payloads. PK is (group, key) — one payload per
--    config key per group. `version` is the same global monotonic
--    revision counter `thing_config_template.version` uses, so the
--    Hub config-push loop's "highest version wins" rule works
--    unchanged.
CREATE TABLE IF NOT EXISTS device_group_config (
    group_id   TEXT NOT NULL REFERENCES "DeviceGroup"(id) ON DELETE CASCADE,
    config_key TEXT NOT NULL,
    state      JSONB NOT NULL,
    version    BIGINT NOT NULL DEFAULT 1,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by TEXT,
    PRIMARY KEY (group_id, config_key)
);

-- 3. Per-key lookup. The resolver fetches every row for `config_key=$1`
--    across all groups the device belongs to and picks the
--    highest-priority match, so this index covers the hot read path
--    even when a device is in many groups.
CREATE INDEX IF NOT EXISTS device_group_config_key_idx
    ON device_group_config(config_key);
