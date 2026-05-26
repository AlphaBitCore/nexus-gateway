# E48 S1 — Schema foundation: 3-tier passthrough config tables + CHECK constraints

**Epic:** E48
**Requirements:** [e48-emergency-passthrough.md](../../../../docs/developers/specs/e48/e48-emergency-passthrough.md) — Must M1, M3, M5
**OpenAPI:** none (this story is DDL only)
**Status:** Approved (user-approved plan 2026-05-13)
**Date:** 2026-05-13

---

## Architecture summary

S1 lands the storage substrate that the rest of E48 builds on. Three tables (global, adapter, provider) mirror the E38-S13 prompt-cache pattern exactly; one VIEW computes the effective merged JSONB per provider; CHECK constraints enforce the M3 (max 8h expiry) and M5 (reason ≥ 20 chars) invariants at the database level so a SQL-bypass operator cannot create a non-conforming row.

S1 ships **no runtime behaviour**. The new tables sit empty (deny-all default), the ai-gateway binary that reads them does not exist yet (lands in S3), and no admin API endpoints accept writes (S6). This is intentional: pre-prod the migration is forward-compatible with a running OLD ai-gateway (which simply doesn't know the tables exist).

### Schema

```sql
-- Tier 1: global singleton
CREATE TABLE "gateway_passthrough_config_global" (
  "id"         TEXT PRIMARY KEY DEFAULT 'singleton',
  "enabled"    BOOLEAN NOT NULL DEFAULT FALSE,
  "config"     JSONB   NOT NULL DEFAULT '{}'::jsonb,   -- bypassHooks/Cache/Normalize
  "expires_at" TIMESTAMPTZ(3),
  "enabled_by" TEXT,                                    -- NexusUser.id who flipped enabled=true
  "reason"     TEXT,                                    -- free-form, ≥ 20 chars when enabled
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

-- Tier 2: per adapter_type
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

-- Tier 3: per provider (FK CASCADE so deleting a Provider also drops its row)
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

-- Effective: 3-tier merge with provider > adapter > global precedence.
-- Returns one row per Provider; "enabled" is OR of any tier active and unexpired;
-- "config" is the JSONB merge of all three tiers' bypass flags.
CREATE VIEW "gateway_passthrough_config_effective" AS
SELECT
  p."id" AS "provider_id",
  -- effective enabled = any tier enabled + unexpired
  COALESCE(
    (g."enabled" AND g."expires_at" > NOW())
    OR (a."enabled" AND a."expires_at" > NOW())
    OR (pr."enabled" AND pr."expires_at" > NOW()),
    FALSE
  ) AS "enabled",
  -- effective config: global || adapter || provider (last write wins per key)
  COALESCE(g."config", '{}'::jsonb)
    || COALESCE(a."config", '{}'::jsonb)
    || COALESCE(pr."config", '{}'::jsonb)
    AS "config",
  -- effective expiry: earliest of the active tiers (tightest wins)
  LEAST(
    CASE WHEN g."enabled"  AND g."expires_at"  > NOW() THEN g."expires_at"  END,
    CASE WHEN a."enabled"  AND a."expires_at"  > NOW() THEN a."expires_at"  END,
    CASE WHEN pr."enabled" AND pr."expires_at" > NOW() THEN pr."expires_at" END
  ) AS "expires_at",
  -- attribution: prefer the most specific tier that contributed enabled
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

-- Seed: empty singleton + thing_config_template row for ai-gateway.
INSERT INTO "gateway_passthrough_config_global" ("id", "enabled", "config")
VALUES ('singleton', FALSE, '{"bypassHooks": false, "bypassCache": false, "bypassNormalize": false}'::jsonb);

-- thing_config_template uses (type, config_key, state JSONB, version, updated_at)
-- The state is the cold-start template the Hub uses to compute desired state
-- for newly-spun-up ai-gateway things. Mirror cache_config shape (3-tier).
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
```

### CHECK constraint rationale

The three CHECK guards on each tier (expires-required-when-enabled, expires-max-8h, reason-min-20) are designed to be evaluated together. A row in any of three states is legal:

1. `enabled = FALSE` (everything else irrelevant; default state)
2. `enabled = TRUE AND expires_at = X AND reason = Y` where `X <= now() + 8h` and `LENGTH(Y) >= 20`
3. (no other states)

A row that says `enabled = TRUE AND expires_at IS NULL` is rejected at INSERT/UPDATE time by `*_expires_required_when_enabled`. A row that says `enabled = TRUE AND expires_at = now() + 24h` is rejected by `*_expires_max_8h`. A row that says `enabled = TRUE AND reason = NULL` (or short) is rejected by `*_reason_min_20`.

**SQL-bypass safety**: an operator who skips the admin API to INSERT directly hits the same constraints. The runbook for E48-emergency-bypass-via-SQL (future, if needed) will document the legitimate path.

### Effective view semantics

The view materialises the in-DB equivalent of what `passthrough.Cache.Effective(providerID)` will compute at runtime. Useful for:

- Admin API debug endpoint `GET /api/admin/passthrough/effective/:providerId` (S6) — direct passthrough query
- Hub reconcile job (S7) — `SELECT * FROM gateway_passthrough_config_effective WHERE expires_at < NOW()` finds expired rows
- Audit / operator SQL — "show me every provider currently under passthrough"

The view's expiry filter (`expires_at > NOW()`) means an enabled row past its expiry naturally drops out — even before the Hub reconcile job flips `enabled = false`. This is defence-in-depth: if the reconcile job is delayed for any reason, the effective config still reflects "expired".

---

## Story

### S1 — Schema foundation

**User story:** As a Nexus platform engineer, I want the three passthrough tables + the effective view + DB-level CHECK constraints in place so that subsequent stories (S2-S7) can build runtime behaviour against a stable, safety-constrained substrate.

**Tasks:**

- **T1.1** — Create migration directory `tools/db-migrate/migrations/20260517000000_e48_gateway_passthrough_config_3tier/` with `migration.sql` matching the schema above. All three tables + the view + the seed inserts + the thing_config_template row.

- **T1.2** — Update `tools/db-migrate/schema.prisma`:
  - Add three Prisma models: `gateway_passthrough_config_global`, `gateway_passthrough_config_adapter`, `gateway_passthrough_config_provider`. Mirror the SQL column types; reference Provider via `provider Provider @relation(...)` on the third.
  - Update Provider model to add the back-reference `passthroughConfig gateway_passthrough_config_provider?`.

- **T1.3** — Run `cd tools/db-migrate && npx prisma migrate dev --create-only` against an empty local DB to verify the SQL applies cleanly. Then apply: `npx prisma migrate dev`. Verify Prisma's generated client reflects the new models.

- **T1.4** — SQL-level constraint validation (deferred to S3 for Go-level integration tests, since this story produces no consumer package). Verify all four CHECK constraints reject the bad inputs and accept the good inputs via direct `psql` matrix below. Go-level integration tests under `packages/ai-gateway/internal/execution/passthrough/` land in S3 alongside the cache loader implementation.

- **T1.5** — Build verification:
  - `go build ./...` clean
  - `npx prisma generate` clean
  - `go test -race -count=1 ./packages/control-plane/internal/store/... -run TestPassthrough` green

**Acceptance:**

- Migration applies clean on a fresh DB and on a copy of current prod schema.
- Effective view returns the correct merge for a sample provider with rows in all 3 tiers, in 2 tiers, in 1 tier, and in 0 tiers.
- All 4 CHECK constraints reject the bad inputs and accept the good inputs per the test matrix.
- No DB writes from any binary outside this migration. The new tables sit empty (deny-all default) until S3 wires the runtime cache.
- `thing_config_template` carries the new `gateway_passthrough_config` row for `ai-gateway` type.

**Validation script:**

```bash
cd tools/db-migrate && npx prisma migrate dev --name e48_gateway_passthrough_config_3tier
go build ./...
go test -race -count=1 ./packages/control-plane/internal/store/... -run TestPassthrough

# Sanity: tables empty, view returns rows for every Provider with default config
psql -c "SELECT count(*) FROM gateway_passthrough_config_global;"   # 1 (singleton)
psql -c "SELECT count(*) FROM gateway_passthrough_config_adapter;"  # 0
psql -c "SELECT count(*) FROM gateway_passthrough_config_provider;" # 0
psql -c "SELECT provider_id, enabled FROM gateway_passthrough_config_effective LIMIT 5;"  # all enabled=false
```
