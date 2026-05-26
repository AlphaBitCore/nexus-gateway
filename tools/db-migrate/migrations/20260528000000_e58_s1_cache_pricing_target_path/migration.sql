-- E58-S1: cache pricing + reasoning cost + target_path/method columns.
--
-- Adds:
--   Model.cachedInputReadPricePerMillion  Decimal? — read-side cache discount (Anthropic 0.10x, OpenAI 0.50x, Gemini 0.25x; NULL = no discount)
--   Model.cachedInputWritePricePerMillion Decimal? — write-side cache surcharge (Anthropic 1.25x; NULL = no surcharge)
--   traffic_event.reasoning_cost_usd      Decimal(12,8)? — cost attributable to reasoning_tokens (already counted inside cost_usd; surfaced separately for analytics)
--   traffic_event.target_method           Text? — HTTP method actually sent to upstream (may differ from `method` for AI gateway cross-format routing)
--   traffic_event.target_path             Text? — HTTP path actually sent to upstream (e.g. Anthropic /v1/messages when client called /v1/chat/completions)
--
-- No data is dropped. ModelPricing + provider_pricing tables remain (unused
-- by runtime since pre-E58; cleanup deferred to a follow-up migration).

ALTER TABLE "Model"
    ADD COLUMN "cachedInputReadPricePerMillion"  DECIMAL,
    ADD COLUMN "cachedInputWritePricePerMillion" DECIMAL;

ALTER TABLE "traffic_event"
    ADD COLUMN "reasoning_cost_usd" DECIMAL(12, 8),
    ADD COLUMN "target_method"      TEXT,
    ADD COLUMN "target_path"        TEXT;

-- Backfill cache prices from existing provider_pricing rows where matches exist.
-- The match key is (Provider.adapterType, regex against Model.providerModelId).
-- Highest priority row wins; ordered by provider_pricing.priority DESC then created_at DESC.
UPDATE "Model" m
SET
    "cachedInputReadPricePerMillion"  = pp.cache_read_usd_per_m,
    "cachedInputWritePricePerMillion" = pp.cache_write_usd_per_m
FROM (
    SELECT DISTINCT ON (m_inner.id) m_inner.id AS model_id,
           pp_inner.cache_read_usd_per_m,
           pp_inner.cache_write_usd_per_m
    FROM "Model"            AS m_inner
    JOIN "Provider"         AS p_inner ON p_inner.id = m_inner."providerId"
    JOIN "provider_pricing" AS pp_inner
         ON pp_inner.adapter_type = p_inner.adapter_type
        AND m_inner."providerModelId" ~ pp_inner.model_pattern
    ORDER BY m_inner.id, pp_inner.priority DESC, pp_inner.created_at DESC
) pp
WHERE m.id = pp.model_id;

-- Apply per-adapter defaults where step above left columns NULL.
-- Anthropic: read 0.10×, write 1.25× (cache_control ephemeral cache).
UPDATE "Model" m
SET
    "cachedInputReadPricePerMillion"  = COALESCE(m."cachedInputReadPricePerMillion",  m."inputPricePerMillion" * 0.10),
    "cachedInputWritePricePerMillion" = COALESCE(m."cachedInputWritePricePerMillion", m."inputPricePerMillion" * 1.25)
FROM "Provider" p
WHERE m."providerId" = p.id
  AND p.adapter_type = 'anthropic'
  AND m."inputPricePerMillion" IS NOT NULL;

-- OpenAI / Azure: read 0.50× (auto prompt cache discount). No write surcharge.
UPDATE "Model" m
SET "cachedInputReadPricePerMillion" = COALESCE(m."cachedInputReadPricePerMillion", m."inputPricePerMillion" * 0.50)
FROM "Provider" p
WHERE m."providerId" = p.id
  AND p.adapter_type IN ('openai', 'azure-openai')
  AND m."inputPricePerMillion" IS NOT NULL;

-- Gemini / Vertex: read 0.25× (implicit cache discount).
UPDATE "Model" m
SET "cachedInputReadPricePerMillion" = COALESCE(m."cachedInputReadPricePerMillion", m."inputPricePerMillion" * 0.25)
FROM "Provider" p
WHERE m."providerId" = p.id
  AND p.adapter_type IN ('gemini', 'vertex')
  AND m."inputPricePerMillion" IS NOT NULL;

-- Bedrock-Anthropic: same ratios as Anthropic-direct.
UPDATE "Model" m
SET
    "cachedInputReadPricePerMillion"  = COALESCE(m."cachedInputReadPricePerMillion",  m."inputPricePerMillion" * 0.10),
    "cachedInputWritePricePerMillion" = COALESCE(m."cachedInputWritePricePerMillion", m."inputPricePerMillion" * 1.25)
FROM "Provider" p
WHERE m."providerId" = p.id
  AND p.adapter_type = 'bedrock'
  AND m."inputPricePerMillion" IS NOT NULL
  AND m."providerModelId" ~ '^anthropic\.';

-- Backfill target_method + target_path on historical rows.
-- compliance-proxy + agent traffic: target = request (transparent forwarding).
UPDATE "traffic_event"
SET
    target_method = method,
    target_path   = path
WHERE source IN ('compliance-proxy', 'agent')
  AND target_method IS NULL;

-- AI gateway traffic: target_path defaults to request path on historical rows
-- (best-effort — cross-format routing is rare on existing data; precise
-- backfill via model_id → provider.adapterType → upstream-path mapping
-- would require a function and isn't worth the complexity for pre-E58
-- historical analytics).
UPDATE "traffic_event"
SET
    target_method = method,
    target_path   = path
WHERE source NOT IN ('compliance-proxy', 'agent')
  AND target_method IS NULL;
