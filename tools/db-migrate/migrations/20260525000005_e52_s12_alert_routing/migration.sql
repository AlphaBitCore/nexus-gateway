-- E52-S12: per-group alert routing.
--
-- Adds an optional `group_id_filter` on AlertRule. When set, the
-- alert dispatcher only fires the rule for events whose target
-- (device) is a member of the filter's DeviceGroup. When NULL,
-- the rule fires fleet-wide (today's behaviour).
--
-- Use case: send the same `thing.offline` alert rule to different
-- destinations per region (finance → finance-secops Slack;
-- engineering → engineering-oncall). Each rule attaches to a
-- specific destination + group filter combo.
--
-- Schema additive only; existing rules have NULL filter and fire
-- fleet-wide unchanged.

ALTER TABLE "AlertRule" ADD COLUMN IF NOT EXISTS group_id_filter TEXT NULL;

-- FK to DeviceGroup with SET NULL on delete so deleting a group
-- gracefully drops the filter rather than orphaning the rule.
ALTER TABLE "AlertRule" ADD CONSTRAINT alert_rule_group_filter_fkey
    FOREIGN KEY (group_id_filter) REFERENCES "DeviceGroup"(id) ON DELETE SET NULL;

-- Index for the dispatcher's "rules for this device's groups" query.
CREATE INDEX IF NOT EXISTS alert_rule_group_filter_idx
    ON "AlertRule"(group_id_filter)
    WHERE group_id_filter IS NOT NULL;
