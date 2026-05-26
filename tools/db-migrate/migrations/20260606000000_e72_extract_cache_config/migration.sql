-- E72: Extract (L1 exact-match) response-cache fleet-wide config.
--
-- Singleton row promoted from yaml-only to admin-managed 2026-05-20:
--   * Hot-toggle without service restart.
--   * Carry the freshness-rules-skip gate that lost its per-route home when
--     RoutingRule.response_cache_policy.skip_time_sensitive was retired.
--
-- One row only, id = 'singleton'. The CP handler enforces single-row
-- semantics; the table itself trusts that contract (no CHECK because the
-- same shape exists on semantic_cache_config).

CREATE TABLE "extract_cache_config" (
    "id"                      TEXT         NOT NULL DEFAULT 'singleton',
    "enabled"                 BOOLEAN      NOT NULL DEFAULT true,
    "ttl_seconds"             INTEGER      NOT NULL DEFAULT 3600,
    "apply_freshness_rules"   BOOLEAN      NOT NULL DEFAULT true,
    "updated_at"              TIMESTAMPTZ(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updated_by"              TEXT,
    CONSTRAINT "extract_cache_config_pkey" PRIMARY KEY ("id")
);

-- Seed the singleton row so the AI Gateway's first config-cache read finds a
-- value and the admin UI doesn't need to handle a "no row yet" edge.
INSERT INTO "extract_cache_config" ("id", "enabled", "ttl_seconds", "apply_freshness_rules")
VALUES ('singleton', true, 3600, true);
