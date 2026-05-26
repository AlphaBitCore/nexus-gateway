-- 2026-05-21 — Migration B-1 of the "single source of truth = Model row" series.
--
-- Backfills "Model".cachedInputReadPricePerMillion + cachedInputWritePricePerMillion
-- for every existing row whose provider matches one of the 5 in-use adapter types.
-- Multipliers are the publicly-documented provider ratios as of 2026-Q1:
--
--   Anthropic claude-*:        cache_read = 0.10× input        cache_write = 1.25× input
--   OpenAI gpt-*/o-*:          cache_read = 0.50× input        cache_write = 0  (no surcharge)
--   Google Gemini:             cache_read = 0.25× input        cache_write = 0
--   DeepSeek:                  cache_read = 0.10× input        cache_write = 0
--   Moonshot:                  leave NULL (per-SKU rates are not uniformly published; admin sets manually)
--
-- Embedding / image / audio model types are skipped — they don't have prompt cache.
-- We only fill rows where the cache columns are currently NULL so an admin who has
-- manually configured prices via the CP UI keeps their value.
--
-- This migration is part of the Plan B refactor that removes the provider_pricing
-- table. Run order: B-1 (this) → B-2 (gateway code) → B-3 (drop provider_pricing).

-- Anthropic
UPDATE "Model" m
   SET "cachedInputReadPricePerMillion"  = COALESCE(m."cachedInputReadPricePerMillion",  m."inputPricePerMillion" * 0.10),
       "cachedInputWritePricePerMillion" = COALESCE(m."cachedInputWritePricePerMillion", m."inputPricePerMillion" * 1.25)
  FROM "Provider" p
 WHERE m."providerId" = p.id
   AND p.adapter_type = 'anthropic'
   AND m.type = 'chat'
   AND m."inputPricePerMillion" IS NOT NULL;

-- OpenAI (matches both gpt-* and o-* models routed through the openai adapter)
UPDATE "Model" m
   SET "cachedInputReadPricePerMillion"  = COALESCE(m."cachedInputReadPricePerMillion",  m."inputPricePerMillion" * 0.50),
       "cachedInputWritePricePerMillion" = COALESCE(m."cachedInputWritePricePerMillion", 0)
  FROM "Provider" p
 WHERE m."providerId" = p.id
   AND p.adapter_type = 'openai'
   AND m.type = 'chat'
   AND m."inputPricePerMillion" IS NOT NULL;

-- Google Gemini
UPDATE "Model" m
   SET "cachedInputReadPricePerMillion"  = COALESCE(m."cachedInputReadPricePerMillion",  m."inputPricePerMillion" * 0.25),
       "cachedInputWritePricePerMillion" = COALESCE(m."cachedInputWritePricePerMillion", 0)
  FROM "Provider" p
 WHERE m."providerId" = p.id
   AND p.adapter_type = 'gemini'
   AND m.type = 'chat'
   AND m."inputPricePerMillion" IS NOT NULL;

-- DeepSeek
UPDATE "Model" m
   SET "cachedInputReadPricePerMillion"  = COALESCE(m."cachedInputReadPricePerMillion",  m."inputPricePerMillion" * 0.10),
       "cachedInputWritePricePerMillion" = COALESCE(m."cachedInputWritePricePerMillion", 0)
  FROM "Provider" p
 WHERE m."providerId" = p.id
   AND p.adapter_type = 'deepseek'
   AND m.type = 'chat'
   AND m."inputPricePerMillion" IS NOT NULL;

-- Moonshot: intentionally not backfilled. The 8k/32k/128k SKUs use different cache
-- read rates and Moonshot doesn't publish a uniform discount ratio. Operators fill
-- via the CP Models page once they have current SKU pricing.
