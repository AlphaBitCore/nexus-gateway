-- Drop the legacy DeviceGroupPolicyRule table.
--
-- Pre-GA cleanup: this table backed the E19 per-group domain
-- allow/inspect/deny mechanism. The Hub Cat B loader that fed
-- it into `policy_rules` shadow keys is being removed in the
-- same change, along with all CP CRUD endpoints and UI. Admins
-- that need per-group domain policy in the future should use
-- the E52-S1 device_group_config row keyed `policy_rules`.

DROP TABLE IF EXISTS "DeviceGroupPolicyRule";
