-- E61 follow-up: surface the 2 fleet tuning knobs that L2 semantic cache
-- actually needs admins to be able to adjust. Per-route policy was retired in
-- the 2026-05-20 cleanup; without per-route, threshold + vary_by ended up
-- hardcoded in proxy_l2.go. Promoting them to the fleet singleton lets a
-- multi-tenant deployment flip vary_by to "org"/"user" and a precision-
-- sensitive workload bump threshold above 0.96 without a code change.
--
-- embed_strategy + allow_cross_model stay hardcoded — their defaults
-- (system_plus_last_user / false) cover 95%+ of workloads; promoting them
-- would be feature stacking with no observed customer demand.
ALTER TABLE semantic_cache_config
  ADD COLUMN IF NOT EXISTS threshold DOUBLE PRECISION NOT NULL DEFAULT 0.96,
  ADD COLUMN IF NOT EXISTS vary_by TEXT NOT NULL DEFAULT 'vk';
