-- E48 S1 — Emergency Passthrough 3-Tier Config — schema foundation.
--
-- Three tables (global / adapter / provider) mirror the E38-S13 prompt cache
-- pattern, plus one VIEW computing the effective merged JSONB per Provider.
-- DB CHECK constraints enforce the bug-fix invariants from the
-- requirements doc (M3: expires_at non-null + max 8h when enabled; M5:
-- reason ≥ 20 chars when enabled). The constraints catch any operator
-- who SQL-bypasses the admin API to flip a passthrough on directly.
--
-- The cross-toggle invariant `bypassNormalize=true ⇒ bypassCache=true`
-- (requirements M2) is enforced at the admin API + UX layer, not as a
-- DB CHECK, because the JSONB shape requires query-time inspection that
-- doesn't generalise cleanly to a column-level constraint. The two
-- enforcement layers together (admin API in S6 + UX in S6) cover every
-- realistic write path.
--
-- This migration is forward-compatible with an OLD ai-gateway binary
-- that does not read the new tables. Applying ahead of the binary swap
-- is safe.

-- ── Tier 1: global singleton ──────────────────────────────────────────────
CREATE TABLE "gateway_passthrough_config_global" (
  "id"         TEXT PRIMARY KEY DEFAULT 'singleton',
  "enabled"    BOOLEAN NOT NULL DEFAULT FALSE,
  "config"     JSONB   NOT NULL DEFAULT '{"bypassHooks": false, "bypassCache": false, "bypassNormalize": false}'::jsonb,
  "expires_at" TIMESTAMPTZ(3),
  "enabled_by" TEXT,
  "reason"     TEXT,
  "updated_at" TIMESTAMPTZ(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT "gateway_passthrough_global_singleton_check" CHECK ("id" = 'singleton'),
  CONSTRAINT "gateway_passthrough_global_expires_required_when_enabled" CHECK (
    "enabled" = FALSE OR "expires_at" IS NOT NULL
  ),
  CONSTRAINT "gateway_passthrough_global_expires_max_8h" CHECK (
    "enabled" = FALSE OR "expires_at" <= NOW() + INTERVAL '8 hours'
  ),
  CONSTRAINT "gateway_passthrough_global_reason_min_20" CHECK (
    "enabled" = FALSE OR (CHAR_LENGTH(COALESCE("reason", '')) >= 20)
  )
);

INSERT INTO "gateway_passthrough_config_global" ("id", "enabled", "config")
VALUES ('singleton', FALSE, '{"bypassHooks": false, "bypassCache": false, "bypassNormalize": false}'::jsonb);

-- ── Tier 2: per adapter_type ──────────────────────────────────────────────
CREATE TABLE "gateway_passthrough_config_adapter" (
  "adapter_type" TEXT PRIMARY KEY,
  "enabled"      BOOLEAN NOT NULL DEFAULT FALSE,
  "config"       JSONB   NOT NULL DEFAULT '{}'::jsonb,
  "expires_at"   TIMESTAMPTZ(3),
  "enabled_by"   TEXT,
  "reason"       TEXT,
  "updated_at"   TIMESTAMPTZ(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT "gateway_passthrough_adapter_expires_required_when_enabled" CHECK (
    "enabled" = FALSE OR "expires_at" IS NOT NULL
  ),
  CONSTRAINT "gateway_passthrough_adapter_expires_max_8h" CHECK (
    "enabled" = FALSE OR "expires_at" <= NOW() + INTERVAL '8 hours'
  ),
  CONSTRAINT "gateway_passthrough_adapter_reason_min_20" CHECK (
    "enabled" = FALSE OR (CHAR_LENGTH(COALESCE("reason", '')) >= 20)
  )
);

-- ── Tier 3: per Provider (FK CASCADE) ─────────────────────────────────────
CREATE TABLE "gateway_passthrough_config_provider" (
  "provider_id" TEXT PRIMARY KEY,
  "enabled"     BOOLEAN NOT NULL DEFAULT FALSE,
  "config"      JSONB   NOT NULL DEFAULT '{}'::jsonb,
  "expires_at"  TIMESTAMPTZ(3),
  "enabled_by"  TEXT,
  "reason"      TEXT,
  "updated_at"  TIMESTAMPTZ(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
  CONSTRAINT "gateway_passthrough_provider_provider_fk" FOREIGN KEY ("provider_id")
    REFERENCES "Provider"("id") ON DELETE CASCADE,
  CONSTRAINT "gateway_passthrough_provider_expires_required_when_enabled" CHECK (
    "enabled" = FALSE OR "expires_at" IS NOT NULL
  ),
  CONSTRAINT "gateway_passthrough_provider_expires_max_8h" CHECK (
    "enabled" = FALSE OR "expires_at" <= NOW() + INTERVAL '8 hours'
  ),
  CONSTRAINT "gateway_passthrough_provider_reason_min_20" CHECK (
    "enabled" = FALSE OR (CHAR_LENGTH(COALESCE("reason", '')) >= 20)
  )
);

-- ── Effective view: 3-tier merge per Provider ─────────────────────────────
-- Returns one row per Provider. "enabled" is TRUE iff any tier is
-- enabled AND its expires_at is still in the future. "config" is the
-- JSONB merge of all tiers (provider > adapter > global). "expires_at"
-- is the soonest expiry among active tiers (tightest bound wins).
CREATE VIEW "gateway_passthrough_config_effective" AS
SELECT
  p."id" AS "provider_id",
  COALESCE(
    (g."enabled" AND g."expires_at" > NOW())
    OR (a."enabled" AND a."expires_at" > NOW())
    OR (pr."enabled" AND pr."expires_at" > NOW()),
    FALSE
  ) AS "enabled",
  COALESCE(g."config", '{}'::jsonb)
    || COALESCE(a."config", '{}'::jsonb)
    || COALESCE(pr."config", '{}'::jsonb)
    AS "config",
  LEAST(
    CASE WHEN g."enabled"  AND g."expires_at"  > NOW() THEN g."expires_at"  END,
    CASE WHEN a."enabled"  AND a."expires_at"  > NOW() THEN a."expires_at"  END,
    CASE WHEN pr."enabled" AND pr."expires_at" > NOW() THEN pr."expires_at" END
  ) AS "expires_at",
  COALESCE(
    CASE WHEN pr."enabled" AND pr."expires_at" > NOW() THEN pr."enabled_by" END,
    CASE WHEN a."enabled"  AND a."expires_at"  > NOW() THEN a."enabled_by"  END,
    CASE WHEN g."enabled"  AND g."expires_at"  > NOW() THEN g."enabled_by"  END
  ) AS "enabled_by",
  COALESCE(
    CASE WHEN pr."enabled" AND pr."expires_at" > NOW() THEN pr."reason" END,
    CASE WHEN a."enabled"  AND a."expires_at"  > NOW() THEN a."reason"  END,
    CASE WHEN g."enabled"  AND g."expires_at"  > NOW() THEN g."reason"  END
  ) AS "reason"
FROM "Provider" p
LEFT JOIN "gateway_passthrough_config_adapter"  a  ON a."adapter_type" = p."adapter_type"
LEFT JOIN "gateway_passthrough_config_provider" pr ON pr."provider_id" = p."id"
CROSS JOIN "gateway_passthrough_config_global"  g;

-- ── thing_config_template row for ai-gateway type ─────────────────────────
-- Cold-start state: fully disabled at every tier. Matches the runtime
-- fail-closed invariant (S3): even if a stale DB row says enabled=true,
-- the cold cache snapshot defaults to disabled until Hub pushes the
-- effective state.
INSERT INTO "thing_config_template" ("type", "config_key", "state", "version", "updated_at")
VALUES (
  'ai-gateway',
  'gateway_passthrough_config',
  '{
    "global": {
      "enabled": false,
      "bypassHooks": false,
      "bypassCache": false,
      "bypassNormalize": false,
      "expiresAt": null,
      "enabledBy": null,
      "reason": null
    },
    "adapters": {},
    "providers": {}
  }'::jsonb,
  1,
  NOW()
);
