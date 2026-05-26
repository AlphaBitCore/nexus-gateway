-- 2026-05-21 — Plan B step 3: drop provider_pricing.
--
-- The gateway no longer reads this table. All four prices (input, output,
-- cached_input_read, cached_input_write) now live on the Model row and are
-- the single source of truth. The previous migration
-- (20260608000000_model_cache_pricing_backfill) copied the cache ratios into
-- every Model row that needed them, so dropping this table is a no-op from
-- the cost-compute side.
--
-- Sequencing note: this migration MUST run AFTER the cachelayer.Layer
-- refactor that retired loadProviderPricing has shipped to all gateway
-- replicas, otherwise a still-running old gateway would error on its
-- next snapshot reload. The Hub WS push reconnect + the Layer.Start
-- aggregator mean a stale process exits cleanly without taking traffic
-- down, but it'll emit one round of error logs.

DROP TABLE IF EXISTS "provider_pricing";
