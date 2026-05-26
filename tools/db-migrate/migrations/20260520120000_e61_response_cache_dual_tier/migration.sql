-- E61 — Smart Response Cache: Dual-Tier Schema Migration
--
-- Scope: two-layer cache architecture split
--   L1 (infrastructure): new singleton table `semantic_cache_config` —
--       stores the fleet-wide embedding provider, model, dimension, and
--       fingerprint. Mirrors ai_guard_config exactly. One row per cluster.
--   L2 (policy): new column `response_cache_policy` on `routing_rule` —
--       per-route nested policy carrying separate Extract and Semantic
--       configurations plus a master time-sensitive-skip toggle.
--
-- Prior art: the flat `{enabled, ttl, vary_by_user, vary_by_vk}` shape
-- was NEVER written to the routing_rule table. A full audit of:
--   - seed-baseline.sql  (no response_cache_policy column in RoutingRule INSERT)
--   - schema.prisma      (no responseCachePolicy field on RoutingRule)
--   - migrations/*       (no prior ALTER TABLE routing_rule ADD COLUMN
--                          response_cache_policy)
-- confirms the column does not exist. This migration adds it fresh in the
-- new nested shape. No UPDATE rewrite required.
--
-- See docs/sdd/e61-s2-policy-schema-migration.md and
-- docs/dev/architecture/response-cache-architecture.md §5.
--
-- Pre-GA: no backward compatibility (CLAUDE.md development-phase policy).

-- ─── L1: semantic_cache_config singleton ─────────────────────────────────────

CREATE TABLE IF NOT EXISTS semantic_cache_config (
    -- Sentinel PK; exactly one row per cluster.
    -- Future SaaS multi-tenant: add org_id column and widen PK.
    id                      TEXT PRIMARY KEY DEFAULT 'singleton',

    -- Fleet-wide embedding provider + model (nullable until first admin config).
    -- ON DELETE SET NULL so model/provider removal does not orphan the row.
    embedding_provider_id   TEXT REFERENCES "Provider"(id) ON DELETE SET NULL,
    embedding_model_id      TEXT REFERENCES "Model"(id)    ON DELETE SET NULL,

    -- Cached dimension for fast index creation (avoids extra FK lookup at
    -- FT.CREATE time). Updated by the Control Plane SemanticCacheStore.Save
    -- alongside the FK fields.
    embedding_dimension     INTEGER,

    -- Fingerprint = sha256(provider_id || ':' || model_id || ':' || dimension).
    -- L1 ConfigCache compares on load; a non-empty mismatch triggers a
    -- semantic_cache.invalidate_all job → versioned index blue/green swap.
    -- Mirrors ai_guard_config.backend_fingerprint.
    embedding_fingerprint   TEXT NOT NULL DEFAULT '',

    -- Versioned Redis Vector index name. Default value; bumped to
    -- 'nexus:semantic-cache:v2', 'v3', … on (provider, model) changes.
    -- Admin can override on advanced migration scenarios, but this is rare.
    redis_index_name        TEXT NOT NULL DEFAULT 'nexus:semantic-cache:v1',

    -- Fleet-wide kill switch. Incident response: flip false to disable
    -- semantic cache everywhere instantly without touching any routing rule.
    enabled                 BOOLEAN NOT NULL DEFAULT false,

    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by              TEXT
);

-- Guarantee uniqueness on the singleton sentinel. Supports ON CONFLICT in
-- idempotent seed inserts.
CREATE UNIQUE INDEX IF NOT EXISTS semantic_cache_config_singleton_id
    ON semantic_cache_config(id);

-- Seed the singleton row so L2 validator + UI can reference L1 immediately
-- without an explicit admin save. enabled=false until admin configures a
-- provider+model on the Cache Embedding Settings page (E61-S6c).
INSERT INTO semantic_cache_config (id, enabled)
VALUES ('singleton', false)
ON CONFLICT (id) DO NOTHING;

-- ─── L2: routing_rule.response_cache_policy (new column) ─────────────────────
--
-- The response_cache_policy column is added as JSONB NULL (no policy = inherit
-- system default: extract disabled, semantic disabled). The nested shape:
--
-- {
--   "extract": {
--     "enabled": <bool>,       -- default true when admin first enables
--     "ttl": <seconds>,        -- default 300
--     "vary_by": <string>      -- "none" | "user" | "vk" | "org"; default "none"
--   },
--   "semantic": {
--     "enabled": false,        -- per-route opt-in; default OFF
--     "threshold": 0.96,
--     "embed_strategy": "system_plus_last_user",
--     "vary_by": "vk",         -- stricter than extract default
--     "allow_cross_model": false,
--     "max_entry_bytes": 262144,
--     "embedding_cost_ceiling_usd_per_day": null
--   },
--   "skip_time_sensitive": true
-- }
--
-- NULL policy rows continue to use the gateway's built-in defaults (extract
-- disabled per-rule, semantic disabled). The AI Gateway handler reads this
-- column on each request and falls back to defaults on NULL.

ALTER TABLE "RoutingRule"
    ADD COLUMN IF NOT EXISTS response_cache_policy JSONB;

-- ─── traffic_event: embedding cost + model attribution columns ────────────────
--
-- Stamped on every L1 miss that triggered an embedding call (whether the
-- embedding resulted in an L2 hit or miss). Supports:
--   - Cache ROI net-savings calc: net_savings = gateway_cache_savings_usd
--                                               - SUM(embedding_cost_usd)
--   - Per-model ROI breakdown after admin swaps embedding models in L1.
--
-- Both columns nullable: pre-E61 rows (and L1-hit rows) have no embedding.

ALTER TABLE traffic_event
    ADD COLUMN IF NOT EXISTS embedding_cost_usd   DECIMAL(20, 10),
    ADD COLUMN IF NOT EXISTS embedding_model_id   TEXT REFERENCES "Model"(id) ON DELETE SET NULL;

-- Partial index: only rows with a non-NULL embedding_model_id benefit from
-- this index. Optimises the "per-model embedding cost breakdown" analytic
-- query without a full-table overhead on the overwhelming majority of rows
-- that have no embedding.
CREATE INDEX IF NOT EXISTS traffic_event_embedding_model_id_idx
    ON traffic_event (embedding_model_id)
    WHERE embedding_model_id IS NOT NULL;
