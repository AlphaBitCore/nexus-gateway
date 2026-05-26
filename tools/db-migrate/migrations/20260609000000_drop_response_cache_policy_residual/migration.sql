-- 2026-05-23 cleanup: drop the residual `RoutingRule.response_cache_policy`
-- column that ends up re-created on databases where the migrations
-- 20260520120000_e61_response_cache_dual_tier and
-- 20260605000000_drop_routing_rule_legacy_fields were applied out of
-- order. On the original timeline drop runs AFTER dual_tier so the
-- column ends up gone. On the 2026-05-23 prod deploy, dual_tier was
-- missing entirely and only landed AFTER the drop, so the drop's
-- `IF EXISTS` made it a no-op and dual_tier re-introduced the column.
--
-- The gateway no longer reads response_cache_policy (per
-- 20260605000000_drop_routing_rule_legacy_fields). This idempotent
-- DROP brings every database — including the one where the original
-- drop misfired — to the same clean end-state.

ALTER TABLE "RoutingRule" DROP COLUMN IF EXISTS response_cache_policy;
