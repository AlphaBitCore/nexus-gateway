-- Drop device_group_config table. The per-group config payload cascade
-- (device_group_config -> group resolution short-circuit -> Cat B loader
-- fallback) was built but never advertised in product; admin UI sections
-- existed but were unused. Product decision 2026-05-24: device groups
-- exist purely for targeting (bulk force-refresh / rotate-cert, alert
-- scoping, exemption attachment) — config flows go through
-- thing_config_template (fleet default) + thing_config_override
-- (per-Thing) only. Removing the table simplifies the resolution path
-- and matches the documented architecture.
--
-- Verified safe: prod has zero rows in this table (seed-baseline.sql
-- writes none; no admin UI surface ever populated it in production).
-- The hub_signal MQ message + WebSocket config_changed broadcast are
-- unchanged — only the table + the (now-removed) ResolveGroupConfig /
-- ResolveConfig group-tier resolution paths are gone.

DROP TABLE IF EXISTS public.device_group_config CASCADE;
