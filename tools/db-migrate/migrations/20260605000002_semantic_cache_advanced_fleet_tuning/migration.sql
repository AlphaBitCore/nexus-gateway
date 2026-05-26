-- Promote the remaining two L2 knobs (embed_strategy + allow_cross_model) to
-- the fleet singleton. Justified after self-review caught inconsistent
-- gate-keeping: exposing vary_by="none" (cross-tenant cache) while hiding
-- allow_cross_model and embed_strategy was indefensible. Either both are
-- footgun-exposed via fleet config, or neither is — picked "both exposed,
-- defaults sane, admins tune when they need".
ALTER TABLE semantic_cache_config
  ADD COLUMN IF NOT EXISTS embed_strategy TEXT NOT NULL DEFAULT 'system_plus_last_user',
  ADD COLUMN IF NOT EXISTS allow_cross_model BOOLEAN NOT NULL DEFAULT false;
