# E38-S13 — Prompt Cache 3-Tier Config (JSONB-per-Scope)

**Epic:** E38 Prompt Cache Friendliness
**Story:** S13 — 3-tier cache configuration with per-provider override
**Status:** Draft
**Requirements:** `docs/developers/specs/e38/e38-prompt-cache-3tier-config.md`
**OpenAPI:** `docs/users/api/openapi/admin/e38-s13-prompt-cache-3tier-config.yaml`

---

## User Story

As a platform operator, I want to manage AI Gateway cache configuration in three concrete scopes — global, per adapter family, per individual Provider — with explicit "inherited vs overridden" visibility on every field, so that I can apply organization-wide defaults while still tuning specific providers (e.g. a canary Gemini account with a shorter TTL) without leaking those tweaks to all other providers of the same family.

---

## Acceptance Criteria

| ID | Criterion |
|----|-----------|
| AC1 | The DB layer carries exactly three cache config tables (`cache_global_config`, `cache_adapter_config`, `cache_provider_config`), each with a `config JSONB` column. Adding a new cache knob (Go struct field) requires zero DB migrations. |
| AC2 | The CP exposes 7 admin endpoints under `/api/admin/cache/*` (per FR-2.1–FR-2.9 in the requirements doc). All are IAM-gated by `admin:prompt-cache.{read,update}`. |
| AC3 | Every CP handler that calls `Hub.NotifyConfigChange` captures both return values and returns HTTP 502 to the caller on error, with a body describing that the row is committed but propagation is pending. No fire-and-forget calls remain in the codebase after this story. |
| AC4 | The AI Gateway subscribes to a single shadow key `cache_config`. The old `prompt_cache` and `gemini_cache` keys are not referenced anywhere in the runtime code after this story. |
| AC5 | `geminicache.ManagerSet` holds a `map[providerID]*Manager`. On shadow reload, the set is reconciled atomically: each Gemini/Vertex Provider gets a Manager with its effective config; Providers removed since last reload have their Manager torn down. The hot path is unaffected during reload. |
| AC6 | A Provider's effective config equals `MERGE(global, adapter[provider.adapter_type], provider_override[provider.id])` evaluated per knob. The `cache_provider_effective` view returns the merged JSON plus per-key source attribution. |
| AC7 | The CP rejects a PUT to `/api/admin/cache/provider/:id` with HTTP 400 if the body contains a knob that is not valid for the Provider's adapter_type (e.g. `gemini_ttl_seconds` on an Anthropic provider, or any cache knob on a Provider whose adapter has no admin-tunable cache config such as OpenAI). |
| AC8 | The reconcile job runs every 60s, compares CP DB to `thing.desired` for the watched config keys, logs drift, increments `cp_config_drift_total` counter, and re-emits `NotifyConfigChange` once per drift detection cycle. |
| AC9 | The redesigned `/ai-gateway/prompt-cache` page renders three panels (Global / Adapter tabs / Active Overrides) and the redesigned Provider detail Cache tab renders per-field "Inherited vs Overridden" badges. No "globally to all Gemini providers" UI copy remains. |
| AC10 | The 9 stale offline dev `thing` rows are deleted from prod in the same migration as the cache config schema rollout. |

---

## Background

See requirements doc § "Background" for the two operational issues this story fixes (silent UI ↔ runtime drift; absence of per-provider Gemini tuning).

The deeper structural cause is two-headed:
- Cache config is split across two `system_metadata` rows with different shapes, two Hub shadow keys, and two handler families.
- The "Gemini" knobs live in the global blob and the UI exposes them under a Provider detail page anyway, creating a placement-vs-scope mismatch.

The fix is to **unify storage in a single 3-table 3-tier model** and **unify shadow propagation under a single key**. JSONB-per-scope (rather than column-per-knob) is the chosen shape: it preserves table-level structure (one table per scope, with FK and CASCADE where applicable) while eliminating the "every new knob = schema migration" friction, which would otherwise accrue as E38/E39 add more cache features.

This story also bundles two adjacent fixes whose root cause is the same fragility (Tier 1 of which is the same fire-and-forget pattern, Tier 2 of which is the absence of any cross-check between source-of-truth and shadow state):
- All `Hub.NotifyConfigChange` callers stop discarding errors (4 sites: cache normaliser, gemini cache, virtual keys, aiguard).
- A new CP reconcile job watches drift between `system_metadata` and `thing.desired` for known Category-A keys.

---

## Architecture

### Package Layout

```
packages/control-plane/
├── internal/
│   ├── db/
│   │   └── cacheconfig.go               ← NEW: pgx repo for 3 tables + view
│   ├── handler/
│   │   ├── admin_cache.go               ← NEW: 7 endpoints under /api/admin/cache/*
│   │   └── admin_extras.go              ← MODIFY: delete cache normaliser + gemini cache handlers (lines 90–93, 1626–1763); delete struct types
│   ├── handler/
│   │   ├── admin_virtual_keys.go        ← MODIFY: capture NotifyConfigChange err (line 37)
│   │   └── admin_aiguard.go             ← MODIFY: capture NotifyConfigChange err (line 201)
│   └── configreconcile/                 ← NEW: drift watchdog goroutine
│       ├── reconcile.go
│       ├── reconcile_test.go
│       └── metrics.go

packages/ai-gateway/
├── internal/
│   ├── geminicache/
│   │   ├── config.go                    ← MODIFY: Config struct unchanged; add NewConfigFromJSON
│   │   ├── manager.go                   ← MODIFY: Manager unchanged; the ManagerSet owns lifecycle
│   │   ├── managerset.go                ← NEW: map[providerID]*Manager, atomic Reload
│   │   └── managerset_test.go
│   ├── handler/
│   │   └── proxy.go                     ← MODIFY: Inject call site reads ManagerSet.Get(providerID)
│   └── cmd/ai-gateway/main.go           ← MODIFY: subscribe to "cache_config" shadow key only; build ManagerSet

packages/control-plane-ui/
├── src/
│   ├── pages/ai-gateway/
│   │   ├── prompt-cache/
│   │   │   └── PromptCachePage.tsx       ← REWRITE: 3-panel layout
│   │   └── providers/detail/
│   │       └── ProviderCacheTab.tsx      ← REWRITE: badge + reset-to-default per field
│   ├── api/services/
│   │   └── cache.ts                      ← NEW: 7 endpoint client functions
│   └── i18n/locales/{en,zh,es}/
│       └── pages.json                    ← ADD: cache config keys

tools/db-migrate/
├── prisma/migrations/
│   └── <timestamp>_e38_s13_prompt_cache_3tier/
│       └── migration.sql                 ← NEW: 3 tables + view + seed + delete old rows + drop stale things

docs/
├── dev/architecture.md                   ← MODIFY: append "Prompt Cache 3-Tier Config (E38-S13)" section
├── requirements/e38-prompt-cache-3tier-config.md   ← NEW (companion to this SDD)
└── openapi/e38-s13-prompt-cache-3tier-config.yaml  ← NEW
```

### Database Schema

```sql
-- ── Tier 1: global singleton ──────────────────────────────────────────────
CREATE TABLE cache_global_config (
  id           TEXT PRIMARY KEY DEFAULT 'singleton' CHECK (id = 'singleton'),
  config       JSONB NOT NULL DEFAULT '{}'::jsonb,
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_by   TEXT
);

-- Seed singleton row
INSERT INTO cache_global_config (id, config) VALUES (
  'singleton',
  '{"normaliser_enabled": true, "cache_master_kill_switch": false}'::jsonb
);

-- ── Tier 2: per adapter_type ──────────────────────────────────────────────
CREATE TABLE cache_adapter_config (
  adapter_type TEXT PRIMARY KEY,
  config       JSONB NOT NULL DEFAULT '{}'::jsonb,
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_by   TEXT
);

-- Seed Tier-2 rows for adapters with cache knobs
INSERT INTO cache_adapter_config (adapter_type, config) VALUES
  ('anthropic', '{
    "marker_inject_enabled": true,
    "marker_boundary3_enabled": false,
    "rules": {
      "claude-code-cch-strip": {"enabled": true}
    }
  }'::jsonb),
  ('bedrock', '{
    "marker_inject_enabled": true,
    "marker_boundary3_enabled": false,
    "rules": {
      "bedrock-claude-cch-strip": {"enabled": true}
    }
  }'::jsonb),
  ('gemini', '{
    "cache_enabled": true,
    "min_system_chars": 4096,
    "ttl_seconds": 3600,
    "circuit_breaker_threshold": 5,
    "circuit_breaker_open_secs": 300
  }'::jsonb),
  ('vertex', '{
    "cache_enabled": false,
    "min_system_chars": 4096,
    "ttl_seconds": 3600,
    "circuit_breaker_threshold": 5,
    "circuit_breaker_open_secs": 300
  }'::jsonb);

-- Adapters with no admin-tunable cache config still get a row so PUT for
-- their rule overrides has a home; they just don't get cache_* keys.
INSERT INTO cache_adapter_config (adapter_type, config) VALUES
  ('openai',       '{"rules":{"openai-field-order-normalize":{"enabled": true}}}'::jsonb),
  ('azure-openai', '{"rules":{"azure-openai-field-order-normalize":{"enabled": true}}}'::jsonb),
  ('deepseek',     '{"rules":{"deepseek-field-order-normalize":{"enabled": true}}}'::jsonb),
  ('glm',          '{"rules":{"glm-field-order-normalize":{"enabled": true}}}'::jsonb),
  ('moonshot',     '{"rules":{"moonshot-field-order-normalize":{"enabled": true}}}'::jsonb);

-- ── Tier 3: per-provider override ─────────────────────────────────────────
CREATE TABLE cache_provider_config (
  provider_id  TEXT PRIMARY KEY REFERENCES "Provider"(id) ON DELETE CASCADE,
  config       JSONB NOT NULL DEFAULT '{}'::jsonb,
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_by   TEXT
);

-- ── View: effective config per provider, with per-key source attribution ─
-- jsonb merge operator `||` is right-biased: later operands override earlier ones,
-- which gives us global → adapter → override resolution naturally.
CREATE VIEW cache_provider_effective AS
SELECT
  p.id                    AS provider_id,
  p.name                  AS provider_name,
  p.adapter_type          AS adapter_type,
  COALESCE(g.config, '{}'::jsonb)
    || COALESCE(a.config, '{}'::jsonb)
    || COALESCE(o.config, '{}'::jsonb)             AS effective_config,
  COALESCE(g.config, '{}'::jsonb)                  AS global_config,
  COALESCE(a.config, '{}'::jsonb)                  AS adapter_config,
  COALESCE(o.config, '{}'::jsonb)                  AS override_config,
  -- Pre-computed source tags for the keys most read by UI
  CASE WHEN o.config ? 'cache_enabled' THEN 'provider-override'
       WHEN a.config ? 'cache_enabled' THEN 'adapter-default'
       WHEN g.config ? 'cache_enabled' THEN 'global-default'
       ELSE 'code-default' END AS cache_enabled_source,
  -- (further per-key sources computed at view query time as needed; the UI
  --  may also call a helper PG function `cache_key_source(provider_id, key)` —
  --  declared in the migration but read-only.)
  o.updated_at            AS override_updated_at,
  o.updated_by            AS override_updated_by
FROM "Provider" p
LEFT JOIN cache_global_config   g ON g.id = 'singleton'
LEFT JOIN cache_adapter_config  a ON a.adapter_type = p.adapter_type
LEFT JOIN cache_provider_config o ON o.provider_id = p.id;
```

### Go Type Definitions (Shape Authority)

`packages/shared/storage/cacheconfig/types.go` (NEW shared package — consumed by CP repo, gateway runtime, and reconcile job).

```go
package cacheconfig

// GlobalConfig is the Tier-1 (singleton) blob.
type GlobalConfig struct {
  NormaliserEnabled      bool `json:"normaliser_enabled"`
  CacheMasterKillSwitch  bool `json:"cache_master_kill_switch"`
}

// AdapterConfig is the Tier-2 (per adapter_type) blob.
// All knob fields are pointer types so JSON "key absent" semantics survive
// round-trip — the resolution algorithm distinguishes "not set at this tier"
// from "set to zero value".
type AdapterConfig struct {
  // Anthropic family (anthropic, bedrock)
  MarkerInjectEnabled     *bool `json:"marker_inject_enabled,omitempty"`
  MarkerBoundary3Enabled  *bool `json:"marker_boundary3_enabled,omitempty"`

  // Gemini family (gemini, vertex)
  CacheEnabled              *bool  `json:"cache_enabled,omitempty"`
  MinSystemChars            *int   `json:"min_system_chars,omitempty"`
  TTLSeconds                *int   `json:"ttl_seconds,omitempty"`
  CircuitBreakerThreshold   *int   `json:"circuit_breaker_threshold,omitempty"`
  CircuitBreakerOpenSecs    *int   `json:"circuit_breaker_open_secs,omitempty"`

  // Rules (per-rule_id override; rule_id is code-baked, only enabled / dry_run can be admin-set)
  Rules map[string]RuleOverride `json:"rules,omitempty"`
}

type RuleOverride struct {
  Enabled      *bool `json:"enabled,omitempty"`
  DryRunAlways *bool `json:"dry_run_always,omitempty"`
}

// ProviderConfig is the Tier-3 (per-provider) override blob.
// Field set is a subset of AdapterConfig: only the family-relevant subset is valid
// for any given provider; CP handler enforces this on PUT.
type ProviderConfig struct {
  MarkerInjectEnabled     *bool `json:"marker_inject_enabled,omitempty"`
  MarkerBoundary3Enabled  *bool `json:"marker_boundary3_enabled,omitempty"`
  CacheEnabled            *bool `json:"cache_enabled,omitempty"`
  MinSystemChars          *int  `json:"min_system_chars,omitempty"`
  TTLSeconds              *int  `json:"ttl_seconds,omitempty"`
  CircuitBreakerThreshold *int  `json:"circuit_breaker_threshold,omitempty"`
  CircuitBreakerOpenSecs  *int  `json:"circuit_breaker_open_secs,omitempty"`
  // Note: rules are NOT in provider override (per FR-1.6 — rules stay Tier 2 only).
}

// CacheConfigBlob is what flows over the Hub shadow `cache_config` key.
type CacheConfigBlob struct {
  Global    GlobalConfig                 `json:"global"`
  Adapters  map[string]AdapterConfig     `json:"adapters"`  // keyed by adapter_type
  Providers map[string]ProviderConfig    `json:"providers"` // keyed by provider_id
}

// ProviderEffective is what the gateway hot path resolves against per request.
// All fields are concrete (non-pointer); each field's source tag indicates which
// tier supplied its value.
type ProviderEffective struct {
  ProviderID                string
  AdapterType               string
  // Resolved values
  NormaliserEnabled         bool
  CacheMasterKillSwitch     bool
  MarkerInjectEnabled       bool
  MarkerBoundary3Enabled    bool
  CacheEnabled              bool
  MinSystemChars            int
  TTLSeconds                int
  CircuitBreakerThreshold   int
  CircuitBreakerOpenSecs    int
  RuleOverrides             map[string]RuleOverride // adapter-level rules for this provider's adapter
}

// Resolve computes a ProviderEffective for one provider by merging Tier 1 → 2 → 3.
// Called by ManagerSet on shadow reload, not on every request.
func Resolve(blob CacheConfigBlob, providerID, adapterType string) ProviderEffective { /* ... */ }
```

### Shadow Payload Shape (example)

```jsonc
// Hub WebSocket frame, ConfigKey="cache_config", State=
{
  "global": {
    "normaliser_enabled": true,
    "cache_master_kill_switch": false
  },
  "adapters": {
    "anthropic": {
      "marker_inject_enabled": true,
      "marker_boundary3_enabled": false,
      "rules": {
        "claude-code-cch-strip": {"enabled": true}
      }
    },
    "gemini": {
      "cache_enabled": true,
      "min_system_chars": 4096,
      "ttl_seconds": 3600,
      "circuit_breaker_threshold": 5,
      "circuit_breaker_open_secs": 300
    }
  },
  "providers": {
    "c6e3b252-303a-45fd-b5dd-f9467eeba669": {
      "ttl_seconds": 7200
    }
  }
}
```

### Resolution Algorithm (per-knob)

```
EffectiveValue(providerID, adapterType, knob_name):
  if providers[providerID].knob_name is set: return (providers[providerID].knob_name, "provider-override")
  if adapters[adapterType].knob_name is set:  return (adapters[adapterType].knob_name, "adapter-default")
  if global.knob_name is set:                 return (global.knob_name, "global-default")
  return (codeDefault[knob_name], "code-default")
```

Implementation: pointer presence indicates "key set at this tier"; nil = "inherit". Code default lives in a hardcoded struct in `packages/shared/storage/cacheconfig/defaults.go` so adding a new knob always has a sensible zero baseline regardless of DB state.

### Hot Path (per request)

```go
// inside packages/ai-gateway/internal/handler/proxy.go
// (existing code, modified only at the geminicache.Manager lookup step)

mgr := h.GeminiCacheMgrSet.Get(req.ProviderID)
if mgr != nil {
  body, injected, err = mgr.Inject(ctx, req.ProviderID, req.BaseURL, req.ModelID, body)
  // ... existing handling unchanged
}
```

Lookup cost: one `sync.Map.Load` + one struct access. Sub-microsecond.

### ManagerSet Reload Semantics

```go
package geminicache

type ManagerSet struct {
  // map[providerID]*Manager
  managers sync.Map
  // shared resources
  rdb     *redis.Client
  res     KeyResolver
  metrics *Metrics
  logger  *slog.Logger
}

func (s *ManagerSet) Reload(blob cacheconfig.CacheConfigBlob, providers []ProviderInfo) {
  // Build the new set: for each Gemini/Vertex provider, resolve effective config
  // and create-or-reuse a Manager.
  seen := make(map[string]struct{})
  for _, p := range providers {
    if p.AdapterType != "gemini" && p.AdapterType != "vertex" { continue }
    eff := cacheconfig.Resolve(blob, p.ID, p.AdapterType)
    cfg := Config{
      Enabled:                 eff.CacheEnabled,
      MinSystemChars:          eff.MinSystemChars,
      TTLSeconds:              eff.TTLSeconds,
      CircuitBreakerThreshold: eff.CircuitBreakerThreshold,
      CircuitBreakerOpenSecs:  eff.CircuitBreakerOpenSecs,
    }
    if existing, ok := s.managers.Load(p.ID); ok {
      existing.(*Manager).Reload(cfg)
    } else {
      mgr := New(s.rdb, s.res, s.metrics, cfg, s.logger)
      s.managers.Store(p.ID, mgr)
    }
    seen[p.ID] = struct{}{}
  }
  // Tear down managers for Providers that no longer exist
  s.managers.Range(func(k, v any) bool {
    id := k.(string)
    if _, kept := seen[id]; !kept {
      s.managers.Delete(id)
    }
    return true
  })
}

func (s *ManagerSet) Get(providerID string) *Manager {
  v, ok := s.managers.Load(providerID)
  if !ok { return nil }
  return v.(*Manager)
}
```

Reload is invoked from `thingclient.OnConfigChanged` callback when shadow key `cache_config` arrives, and also from `OnProviderListChanged` (existing callback when Provider table syncs via Hub) so a new Provider with adapter_type=gemini gets a Manager immediately.

### NotifyConfigChange Error-Capture Pattern (the fix)

Every PUT/DELETE handler that writes a config row now uses this pattern (illustrated for the gemini-cache case):

```go
// inside admin_cache.go UpdateAdapterConfig
if h.Hub != nil {
  blob, err := h.assembleCacheConfigBlob(ctx)  // re-read all 3 tiers + serialize
  if err != nil {
    h.Logger.Error("assemble cache_config blob", "error", err)
    return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
  }
  resp, err := h.Hub.NotifyConfigChange(ctx, hubclient.ConfigChangeRequest{
    ThingType: "ai-gateway",
    ConfigKey: "cache_config",
    State:     blob,
  })
  if err != nil {
    h.Logger.Error("notify hub of cache config change",
      "config_key", "cache_config",
      "adapter_type", adapterType,
      "error", err)
    return c.JSON(http.StatusBadGateway, errJSON(
      "Config saved locally but propagation to gateway failed; "+
      "verify Hub health and retry, or wait for the reconcile job to recover.",
      "propagation_error", ""))
  }
  _ = resp
}
```

The 4 fire-and-forget sites identified during the 2026-05-13 prod debug session are fixed in this story:
- `admin_extras.go:1690` — cache normaliser — deleted entirely (replaced by `admin_cache.go`).
- `admin_extras.go:1751` — gemini cache — deleted entirely (replaced by `admin_cache.go`).
- `admin_virtual_keys.go:37` — VK invalidate — captured + 502 on err.
- `admin_aiguard.go:201` — aiguard — captured + 502 on err.

### Reconcile Job

`packages/control-plane/internal/configreconcile/reconcile.go`:

```go
type Reconciler struct {
  DB     *sql.DB
  Hub    *hubclient.Client
  Logger *slog.Logger
  Metric *prometheus.CounterVec   // cp_config_drift_total{config_key,thing_type,thing_id}
  // Watched keys: maps config_key → (sourceFn, thingType)
  Watches []Watch
}

type Watch struct {
  ConfigKey      string
  ThingType      string
  SourceLoader   func(ctx context.Context, db *sql.DB) (any, error) // load source-of-truth from CP DB
  ShadowPath     string  // jsonb path inside thing.desired (e.g. just config key)
}

// At startup (main.go):
recon := &configreconcile.Reconciler{
  DB: db, Hub: hub, Logger: logger, Metric: cpConfigDriftTotal,
  Watches: []configreconcile.Watch{
    {ConfigKey: "cache_config",      ThingType: "ai-gateway",        SourceLoader: loadCacheConfigBlob},
    {ConfigKey: "kill_switch",       ThingType: "compliance-proxy",  SourceLoader: loadKillSwitchState},
    {ConfigKey: "agent_settings",    ThingType: "agent",             SourceLoader: loadAgentSettings},
    {ConfigKey: "aiguard",           ThingType: "ai-gateway",        SourceLoader: loadAIGuardConfig},
    {ConfigKey: "virtual_keys",      ThingType: "ai-gateway",        SourceLoader: loadVirtualKeysSnapshot},
  },
}
go recon.Run(ctx)  // every 60s, ranges Watches, queries thing.desired, diffs, logs, re-emits
```

The reconcile job is **read-mostly**: it queries `system_metadata` / source tables (via SourceLoader) and `thing` rows, computes a structural diff on the JSON, and on drift re-invokes `Hub.NotifyConfigChange` once per watch per cycle. If the re-emit also fails the metric counter just keeps incrementing — operator-facing alarm threshold can be wired into prometheus separately.

### Migration Plan

```sql
-- File: tools/db-migrate/migrations/<timestamp>_e38_s13_prompt_cache_3tier/migration.sql

BEGIN;

-- 1. Create the 3 new tables + view
CREATE TABLE cache_global_config (...);
CREATE TABLE cache_adapter_config (...);
CREATE TABLE cache_provider_config (...);
CREATE VIEW cache_provider_effective AS ...;

-- 2. Seed sensible defaults (per the example above)
INSERT INTO cache_global_config ...
INSERT INTO cache_adapter_config ...

-- 3. Delete the obsolete system_metadata rows (CLAUDE.md: no compat shim)
DELETE FROM system_metadata WHERE key IN ('prompt_cache', 'gemini_cache');

-- 4. Wipe the corresponding shadow desired-state keys on the ai-gateway thing
--    (the new gateway code subscribes to 'cache_config' only and ignores the old keys)
UPDATE thing
SET desired = (desired - 'prompt_cache') - 'gemini_cache'
WHERE type = 'ai-gateway';

-- 5. Clean up stale offline dev things (FR-7.3)
DELETE FROM thing
WHERE status = 'offline'
  AND last_seen_at < (now() - interval '30 days')
  AND id IN (
    'thing-gw-01','thing-cp-01','thing-proxy-01',
    'agent-dev-enrolled-01','agent-dev-linux-01','agent-dev-mac-01',
    'agent-dev-revoked-01','agent-dev-win-01',
    'agent-1e5377e923c56679','agent-ae7ad926d0181931',
    'agent-b953d52f900d5a98','agent-cc009d193f03a9de',
    'agent-d797b87fa56c3053'
  );

-- 6. Record migration in Prisma's _prisma_migrations table (handled by Prisma itself in dev;
--    in single-migration prod deploy, the prod-deploy skill applies this manually)

COMMIT;
```

Per CLAUDE.md prod-deploy mandate: a `pg_dump` backup is taken before this migration is applied to prod (`prod-deploy` skill Step 0a, no waiver).

---

## API Surface (Summary)

Full schema in `docs/users/api/openapi/admin/e38-s13-prompt-cache-3tier-config.yaml`. Quick reference:

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/api/admin/cache/global` | Read Tier-1 row |
| PUT | `/api/admin/cache/global` | Replace Tier-1 row + propagate |
| GET | `/api/admin/cache/adapters` | List every Tier-2 row in one call (UI batch fetch) |
| GET | `/api/admin/cache/adapter/:adapter_type` | Read Tier-2 row |
| PUT | `/api/admin/cache/adapter/:adapter_type` | Upsert Tier-2 row + propagate |
| GET | `/api/admin/cache/provider/:provider_id` | Read Tier-3 row (empty `{}` if none) |
| PUT | `/api/admin/cache/provider/:provider_id` | Upsert Tier-3 row + propagate (validates fields vs adapter_type) |
| DELETE | `/api/admin/cache/provider/:provider_id` | Drop Tier-3 row + propagate |
| GET | `/api/admin/cache/effective?provider_id=<id>` | Resolved config + per-field source tags |
| GET | `/api/admin/cache/overrides` | List all Providers with non-empty Tier-3 |

All endpoints IAM-gated by `admin:prompt-cache.{read,update}`.

---

## Test Plan

### Unit Tests (Go)

- `packages/shared/storage/cacheconfig/types_test.go` — Resolve() correctness across 3 tiers, missing-key fallback, pointer-presence semantics.
- `packages/control-plane/internal/db/cacheconfig_test.go` — repo CRUD against a real Postgres test DB.
- `packages/control-plane/internal/handler/admin_cache_test.go` — handler tests, including:
  - PUT with field-vs-adapter-type mismatch returns 400 with structured error.
  - PUT with valid body persists and emits NotifyConfigChange.
  - PUT with Hub down returns 502 (test using a mock Hub client that returns error).
  - DELETE removes row and propagates.
- `packages/ai-gateway/internal/cache/gemini/managerset_test.go` — Reload reconciles managers correctly, Manager torn down on Provider removal, atomic during concurrent Get calls.
- `packages/control-plane/internal/configreconcile/reconcile_test.go` — drift detection logic; metric increments; re-emit on drift.

### Integration Tests (Go)

- New: `tests/integration/e38_s13_cache_3tier_test.go` — end-to-end: PUT a tier-3 override, observe shadow push, verify gateway runtime reflects new effective config, verify metric counters update accordingly.

### UI Tests (Vitest)

- `ProviderCacheTab.test.tsx` — given mocked effective config response, renders correct badges; reset-to-default button removes field from PUT body.
- `PromptCachePage.test.tsx` — three panels render; Active Overrides table cross-links to provider detail.

### Manual Verification (covered in Phase 8)

- Toggle `cache_master_kill_switch` to true, confirm all cache-related metrics stop incrementing across all adapters.
- Set per-provider TTL override on one Gemini Provider, run /smoke-gateway against that Provider only, verify the override takes effect (cache TTL observed via Gemini API).
- Stop Hub temporarily, attempt a UI save, verify 502 returned with structured error.
- Start reconcile job, manually inject drift via direct DB write to `thing.desired`, observe drift detection log + metric increment + automatic recovery.

---

## ADRs (Architecture Decision Records)

### ADR-1: Tier-3 storage as JSONB rather than column-per-knob

**Decision:** Use JSONB column for `config` in all 3 tier tables.

**Status:** Accepted.

**Context:** Two alternatives considered: (a) typed nullable columns per knob across tier tables; (b) JSONB per tier row. (a) gives DB-level type safety but requires a migration for every new knob — projected to be every 1–3 months as E38/E39 continues. (b) trades DB-level field-shape enforcement for Go-struct-layer enforcement.

**Consequences:**
- Adding a new knob requires no DB migration, only Go struct + UI form + i18n keys.
- Go marshal/unmarshal serves as the de-facto schema enforcement; CP handler is the only writer.
- Pure SQL queries cannot filter by individual knob value as easily, but no current or planned query has this requirement.

### ADR-2: Rules stay Tier-2-only (not exposed at Tier 3)

**Decision:** Normalisation Rules are configurable at adapter family level only; per-provider rule override is rejected for this story.

**Status:** Accepted.

**Context:** Rules currently affect both NormalizeKey (cache key computation, which runs before provider routing) and NormalizeUpstream (post-routing rewrite). Permitting per-provider override would create cache-key inconsistency: the same content sent to the same VK could land on different cache keys depending on which provider was routed to. All current rules are `KeyNormalizeSafe = true`, so the inconsistency would be observable in practice.

**Consequences:**
- Rules live in `cache_adapter_config.config.rules.<rule_id>` only.
- Future per-provider differential behavior (e.g. "one Anthropic provider doesn't get cache_control injection") is handled by separate per-provider knobs (`marker_inject_enabled`), not via rule override.
- If a future rule arrives that is `KeyNormalizeSafe = false` AND has legitimate per-provider differentiation need, a follow-up story can add Tier-3 rule override; the JSONB shape leaves room for it (just add `rules` to ProviderConfig).

### ADR-3: Single `cache_config` shadow key (delete old keys)

**Decision:** Replace the two existing shadow keys (`prompt_cache`, `gemini_cache`) with a single `cache_config` key carrying the full 3-tier blob.

**Status:** Accepted.

**Context:** Per CLAUDE.md dev-phase rule (no compat shims). Two shadow keys means two reload paths in the gateway, two reconcile entries, two error surfaces. Collapsing them simplifies the runtime considerably; cost is a one-time migration to drop the old keys from shadow desired state.

**Consequences:**
- The migration includes a `UPDATE thing SET desired = (desired - 'prompt_cache') - 'gemini_cache'` step.
- The gateway's thingclient subscription is registered for `cache_config` only.
- Reconcile job watches `cache_config` (cleaner than watching both old keys).

### ADR-4: CP returns 502 on Hub NotifyConfigChange failure

**Decision:** Every config-mutating handler returns HTTP 502 if Hub call fails, with a structured "propagation_error" body explaining that the row is committed to DB but the gateway has not yet seen it.

**Status:** Accepted.

**Context:** The historical fire-and-forget pattern caused the 2026-05-13 prod drift. Two recovery paths exist: synchronous error surfaced to UI, or async eventual consistency via reconcile job. We do both — 502 immediately so the operator sees the problem; reconcile job behind the scenes for any other source of drift.

**Consequences:**
- UI must render a meaningful error message on 502 (the structured body's `message` field).
- DB state and shadow state are momentarily divergent; the reconcile job will heal within 60s in the worst case.
- "Save then come back later to confirm gateway has applied" is no longer required — the 502 IS the signal that further action is needed.

### ADR-5: Per-provider field validation done in CP handler (not DB trigger)

**Decision:** CP handler validates that a Tier-3 PUT body contains only knob keys appropriate to the Provider's adapter_type.

**Status:** Accepted.

**Context:** DB-level CHECK constraints cannot reference another table (PostgreSQL standard). Options: (a) DB trigger that JOINs Provider to validate; (b) handler-layer validation. We chose (b) because: the only write path is the handler; trigger adds a stored function with cross-table coupling that is harder to evolve; handler-layer validation gives better error messages.

**Consequences:**
- Bypassing the handler (e.g. raw SQL by an admin) can technically write an invalid row. This is acceptable — direct DB writes are operator-discretionary and outside the threat model.
- The validation logic lives in one place (`admin_cache.go` `validateProviderConfigForAdapter`).

### ADR-6: New shared package `packages/shared/storage/cacheconfig/`

**Decision:** Type definitions and the Resolve function live in a new shared package consumed by CP, gateway, and reconcile job.

**Status:** Accepted.

**Context:** Three independent Go modules need to deserialize the same JSON shapes (CP for DB I/O; gateway for runtime; reconcile for diff). Defining the structs in any single module would force the other two to depend on a non-shared package.

**Consequences:**
- `packages/shared/storage/cacheconfig/` is added to the vetted core dependency set (it depends only on stdlib).
- Schema evolution requires touching one file rather than three.

---

## Rollout Plan

Per CLAUDE.md prod-deploy skill:

1. Phase 1 (this SDD + Requirements + OpenAPI + arch doc append) — user reviews before code begins.
2. Phases 2–7 (DB / CP / Gateway / UI / tests / adjacent fixes) — work locally; build + go test + npm test pass.
3. Phase 8 (local verify) — full smoke run against local stack; UI manual scenario tests.
4. Phase 9 (prod deploy) — `pg_dump` backup → apply migration via single-migration path → upload binaries → restart in canonical order → verify nodes online + in-sync.
5. Phase 10 (post-deploy verify) — toggle scenarios verified via prod CP API; reconcile job verified via injected drift; final commit; memory `project_e38_prompt_cache.md` updated to record story complete.

Tag: `prod-20260513b@<sha>` (assuming same-day deploy as the IAM hotfix already shipped under `prod-20260513`).

## Rollback

If post-deploy verification fails:

1. Restart all 4 services on the previous binary version (`git checkout <previous prod tag>` on the EC2 deploy script).
2. The migration is one-way (it deletes old `system_metadata` rows). To roll back DB state, restore from the pre-deploy `pg_dump` taken in Step 0a. The cache config returns to the pre-refactor state, accepting that any UI saves made between deploy and rollback are lost.
3. The shadow desired state is similarly rolled back via the dump restore.

Per CLAUDE.md "rollback is `git revert`" — destructive changes (the DELETE of old rows) are guarded by mandatory `pg_dump` before deploy.

---

## Out of Scope

- Per-provider rule overrides (rejected per ADR-2).
- A "cache config history" audit table beyond the existing `audit_log` table.
- Programmatic exposure of `cache_master_kill_switch` to any caller other than the admin UI (it remains a manually-operated switch).
- E38-S14 work (TBD).
