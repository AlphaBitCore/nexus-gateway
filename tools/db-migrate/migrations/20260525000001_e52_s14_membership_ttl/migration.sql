-- E52-S14: time-bounded DeviceGroupMembership.
--
-- A NULL expires_at means "permanent" (today's behaviour). A
-- non-NULL value scopes the membership to a window: GroupsOfDevice +
-- MembersOfGroup filter `expires_at > NOW()` so the device leaves
-- the group automatically without a separate eviction workflow.
--
-- Use case: incident response — temporarily add a suspected
-- compromised device to `quarantine-group` with `expires_at =
-- NOW() + 4 hours` so the device falls out automatically when the
-- incident closes.
--
-- Schema is additive only; existing rows have NULL and behave
-- identically to today.

ALTER TABLE "DeviceGroupMembership" ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ NULL;

-- Indexed so the recompute job's "evict expired" sweep is fast.
-- Partial index — most rows are NULL (permanent) and don't need to
-- show up here.
CREATE INDEX IF NOT EXISTS device_group_membership_expires_idx
    ON "DeviceGroupMembership"(expires_at)
    WHERE expires_at IS NOT NULL;
