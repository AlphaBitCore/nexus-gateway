-- E52-S2 — Smart / dynamic groups.
--
-- Adds predicate-driven membership to DeviceGroup. A group with a
-- non-NULL `membership_query` is "smart": its members are computed
-- from the predicate evaluated against attributes on `thing.*`
-- (hostname / os / agentVersion / primary_ip / bound user / status /
-- enrolled_at / last_heartbeat / physical_id / metadata.<key>) by
-- the same matcher that routing rules use. Static groups (NULL
-- membership_query) continue to use DeviceGroupMembership rows
-- exactly as before — backwards compatible.
--
-- The cache table materializes the predicate result so that downstream
-- readers — config resolution (S1), IAM scope enforcement (S3) — don't
-- re-run the predicate engine. The cache is recomputed on every
-- relevant device heartbeat plus a 60s safety-net job in Hub.
--
-- Schema is additive only. Existing DeviceGroup / DeviceGroupMembership
-- rows are untouched.

-- 1. Predicate column on DeviceGroup.
ALTER TABLE "DeviceGroup" ADD COLUMN IF NOT EXISTS membership_query JSONB;

-- 2. Materialized cache of (group_id, device_id) for smart groups.
--    Composite PK enforces uniqueness; per-device index supports the
--    GroupsOfDevice() lookup the IAM middleware (S3) calls per
--    request.
CREATE TABLE IF NOT EXISTS device_group_membership_cache (
    group_id    TEXT NOT NULL REFERENCES "DeviceGroup"(id) ON DELETE CASCADE,
    device_id   TEXT NOT NULL REFERENCES thing(id) ON DELETE CASCADE,
    computed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (group_id, device_id)
);

CREATE INDEX IF NOT EXISTS device_group_membership_cache_device_idx
    ON device_group_membership_cache(device_id);
