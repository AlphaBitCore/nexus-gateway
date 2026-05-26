# E31 — Story 1: AI Gateway response cache — enable + 5-state status

## Context

The AI Gateway has a Redis-backed response cache (`packages/ai-gateway/internal/cache/`) that has been wired in `proxy.go` since the initial build but **never enabled** in any deployed config. Both `ai-gateway.dev.yaml` and `ai-gateway.config.yaml` ship `cache.enabled: false`.

A second issue compounds this: the audit record's `CacheHit bool` only encodes two outcomes (`true` / `false`), so the Live Traffic UI displays "MISS" for **four** materially different situations: cache disabled, streaming-bypass, client `X-Nexus-No-Cache`, and a real lookup miss. This makes the column actively misleading once cache is on.

A third issue is correctness: `cache.BuildKey` hashes only `provider || model || messages || temperature || max_tokens` extracted via gjson. That key set is OpenAI-shaped; for Anthropic / Gemini / Bedrock requests the relevant fields live under different paths, so two requests with **different system prompts, different `tools` arrays, or different `seed`** can hash to the same key — a quiet correctness bug that becomes a data leak the moment cache is turned on.

This story enables the cache, switches the audit/UI representation to a 5-state status, and replaces the selective hash with a full-body hash so cache key = (provider, model, post-hook body bytes). Pre-GA, no compat shims.

## User Story

**As a** Nexus Gateway operator,
**I want** the AI Gateway response cache to be on by default with an honest 5-state status visible in Live Traffic,
**so that** I can see whether requests are actually being deduplicated against Redis, distinguish "cache off" from "cache miss," and trust that two cached responses came from semantically identical upstream calls.

## Tasks

### 1. Schema — `tools/db-migrate/`

Replace the boolean `traffic_event.cache_hit` with a string `cache_status` column.

**Prisma migration** (new directory `tools/db-migrate/migrations/<timestamp>_traffic_event_cache_status/migration.sql`):

```sql
ALTER TABLE "traffic_event" DROP COLUMN "cache_hit";
ALTER TABLE "traffic_event" ADD COLUMN "cache_status" TEXT;
```

**`schema.prisma`** — replace `cache_hit Boolean? @map("cache_hit")` with `cache_status String? @map("cache_status")`.

**`seed/seed.ts`** — replace `cache_hit: i % 4 === 0` with `cache_status: ['HIT','MISS','DISABLED','SKIP_STREAM','SKIP_NO_CACHE'][i % 5]`. Existing seed-only rollup math (`metric_name='cache_hits'`) is unaffected.

### 2. Audit type + writer — `packages/ai-gateway/internal/observability/audit/`

`audit.go`:

```go
// CacheStatus is the per-request response-cache outcome. Values are
// uppercase snake to match other traffic_event enums (bump_status,
// hook_decision). Empty string = unknown / not yet evaluated.
type CacheStatus string

const (
    CacheStatusHit          CacheStatus = "HIT"
    CacheStatusMiss         CacheStatus = "MISS"
    CacheStatusDisabled     CacheStatus = "DISABLED"      // cache module nil
    CacheStatusSkipStream   CacheStatus = "SKIP_STREAM"   // SSE / streaming bypass
    CacheStatusSkipNoCache  CacheStatus = "SKIP_NO_CACHE" // client sent X-Nexus-No-Cache
)
```

Replace `Record.CacheHit bool` with `Record.CacheStatus CacheStatus`. Update writer INSERT column list and value binding.

### 3. Cache key correctness — `packages/ai-gateway/internal/cache/cache.go`

Replace `BuildKey` selective gjson hash with a full-body hash:

```go
func (c *Cache) BuildKey(provider, model string, body []byte) string {
    if c == nil {
        return ""
    }
    h := sha256.New()
    fmt.Fprintf(h, "v1\nprovider=%s\nmodel=%s\n", provider, model)
    h.Write(body)
    return c.prefix + ":" + hex.EncodeToString(h.Sum(nil))
}
```

**Why `v1` prefix in the hash input**: pins the key schema so any future change to the hash recipe (e.g. include hook-rewrite version) bumps to `v2` and invalidates without manual flush.

**Body is what we hash**: this is the post-hook, post-quota body that proxy sends upstream — i.e. exactly what determines the upstream response. Hooks rewriting body before BuildKey is the existing flow; this story preserves it.

### 4. Proxy 5-state wiring — `packages/ai-gateway/internal/handler/proxy.go`

Restructure the cache block (`Phase 5.5: Cache lookup`) so every request takes exactly one of five paths and `rec.CacheStatus` is set on every path:

| Condition | `rec.CacheStatus` | `X-Nexus-Cache-Status` header | `X-Cache` header |
|-----------|-------------------|------------------------------|------------------|
| `Cache == nil` | `DISABLED` | `DISABLED` | (not set) |
| `isStream == true` | `SKIP_STREAM` | `SKIP_STREAM` | (not set) |
| `X-Nexus-No-Cache` header present | `SKIP_NO_CACHE` | `SKIP_NO_CACHE` | (not set) |
| `Lookup` returns entry | `HIT` | `HIT` | `HIT` |
| `Lookup` returns nil | `MISS` | `MISS` | `MISS` |

Order of evaluation: `DISABLED` → `SKIP_STREAM` → `SKIP_NO_CACHE` → `Lookup` → `HIT` / `MISS`. Stream and no-cache paths short-circuit before any Redis call. `X-Cache` is kept (CDN convention) only on `HIT`/`MISS` so it remains semantically a Varnish-style header.

### 5. Control Plane — `packages/control-plane/internal/store/`, `internal/handler/admin_traffic.go`

- `TrafficEventListParams.CacheHit *bool` → `CacheStatus *string` (nil = no filter; non-nil = exact match).
- SQL: `SELECT … a.cache_status … FROM traffic_event a` and `WHERE a.cache_status = $N` when filtered.
- Query parameter rename: `cacheHit=true|false` → `cacheStatus=HIT|MISS|DISABLED|SKIP_STREAM|SKIP_NO_CACHE`.
- `TrafficEvent.CacheHit *bool json:"cacheHit"` → `CacheStatus *string json:"cacheStatus"` in the response shape.
- Update `docs/users/api/openapi/e??-s??-admin-traffic.yaml` if such a spec exists; otherwise add the `cacheStatus` query parameter and response field to whichever admin-traffic spec is canonical.

### 6. Rollup compat — `packages/nexus-hub/internal/jobs/rollup_5m.go`

`MetricCacheHitCount` aggregation must stay identical. Change the SELECT projection only:

```sql
SELECT
  …
  (a.cache_status = 'HIT') AS cache_hit,
  …
FROM traffic_event a
```

The Go-side `cacheHit *bool` variable name is left as-is (this is internal to the rollup). Result: `1` is added to `cache_hit_count` when status is `HIT`; all other statuses count as 0, which matches the previous boolean's true/false semantics.

### 7. UI — `packages/control-plane-ui/src/pages/traffic/`

- `liveTrafficFilters.ts`: `LiveTrafficCacheHit` type → `LiveTrafficCacheStatus = '' | 'HIT' | 'MISS' | 'DISABLED' | 'SKIP_STREAM' | 'SKIP_NO_CACHE'`. Rename field `cacheHit` → `cacheStatus`. Query param emit + chip-line accordingly.
- `LiveTrafficAdvancedFilters.tsx`: replace boolean select with 6-option select (empty + 5 statuses).
- `TrafficTab.tsx`: column key `cacheStatus`, badge with 5 colors:
  - `HIT` → green
  - `MISS` → grey
  - `DISABLED` → muted/dim
  - `SKIP_STREAM` → blue
  - `SKIP_NO_CACHE` → blue
- `trafficAuditDrawer.tsx`: detail row shows raw status string (or `-` when null).
- `TrafficAnalyticsPage.module.css`: extend `.cacheBadge` modifiers.
- i18n: add keys `pages:traffic.cacheStatus.{HIT,MISS,DISABLED,SKIP_STREAM,SKIP_NO_CACHE}` to all 3 locales (`en/zh/es`) and copy to `public/locales/`.
- Extend the existing `liveTrafficFilters.test.ts` with cases asserting query-param emission + chip-line text for each status.

### 8. Config — `packages/ai-gateway/`

Both YAMLs:

```yaml
cache:
  enabled: true
  ttl: 5m
  prefix: "ai-gw:"
```

`CACHE_TTL` env override path in `internal/config/config.go` is already wired (`time.ParseDuration`). No code change for env override; just confirm under unit test.

**TTL configurability — explicit scope (per user decision)**:
- **In scope**: file-level (yaml `cache.ttl`) and process-env (`CACHE_TTL`) overrides.
- **Out of scope**: per-route TTL, per-request TTL header, admin-UI runtime override (would require system_metadata pattern + new admin endpoint + UI page — track as separate story if/when ops asks).

### 9. Tests

- `cache/cache_test.go` — assert `BuildKey` is deterministic over identical bytes, differs across (provider, model, body) variations, and is **stable** when irrelevant whitespace is canonical (i.e. body is treated as bytes — no JSON normalization is performed; we treat hooks as the canonicalizer).
- `audit/audit_test.go` — `CacheStatus` round-trip through writer SQL.
- `handler/proxy_*_test.go` — table-driven test covering all 5 paths; assert `rec.CacheStatus` and response headers.
- `control-plane/internal/store/store_test.go` — `cacheStatus` filter SQL.
- `control-plane/internal/handler/admin_traffic_test.go` — query-param parsing for each enum value.
- `control-plane-ui/.../liveTrafficFilters.test.ts` — extend with `cacheStatus` cases.

## Acceptance Criteria

1. After running migrations, `traffic_event` has column `cache_status TEXT` and no column `cache_hit`.
2. Sending the same non-stream `POST /v1/chat/completions` (or `/v1/embeddings`) request body twice, with cache enabled, the second response carries `X-Cache: HIT` and `X-Nexus-Cache-Status: HIT`, and the corresponding traffic_event row has `cache_status = 'HIT'`.
3. A streaming request (e.g. `"stream": true`) records `cache_status = 'SKIP_STREAM'` and emits `X-Nexus-Cache-Status: SKIP_STREAM`. No Redis lookup is attempted.
4. A request with header `X-Nexus-No-Cache: 1` records `cache_status = 'SKIP_NO_CACHE'`.
5. With `cache.enabled: false` (regression check), every request records `cache_status = 'DISABLED'`.
6. Two requests with the same `messages`, same `temperature`, same `max_tokens`, but **different `tools` arrays** (or different system prompt, or different `seed`) hash to **different cache keys** and do not share cached responses.
7. Live Traffic UI:
   - The Cache column shows one of five badges, not just HIT/MISS.
   - The Cache filter dropdown offers 5 enum values + clear.
   - Filter emits `cacheStatus=<enum>` query parameter; control-plane returns only matching rows.
8. `nexus_ai_gateway_*` rollup metric `cache_hit_count` equals the count of rows with `cache_status = 'HIT'` over the rollup window — i.e. analytics dashboards see no regression.
9. `CACHE_TTL=10m` env at process start overrides the yaml `5m` default and is reflected in the next Redis SET TTL.
10. `go test ./... -race -count=1` passes for `ai-gateway`, `control-plane`, `nexus-hub`, `shared`. `npm test` passes for `control-plane-ui`.
11. No new `TODO` / `FIXME` / stub in production code paths touched by this story.

## Out of Scope

- Per-request `X-Nexus-Cache-TTL` header, per-route TTL, admin-UI runtime TTL editor.
- Streaming-response caching (would require SSE accumulation + buffered replay; promote `compliance-proxy`'s streaming primitive into `shared/` first — separate epic).
- Cached entry compression (Redis values are JSON+bytes; if we hit memory pressure later, gzip-on-store is one knob — out of scope).
- Per-VK / per-org / per-project cache scoping. Current design is global by `(provider, model, body)`. This is correct under the assumption that the post-hook body fully determines the upstream response, which it does today.
- Cache invalidation on model/provider config change. TTL is the only invalidation. If ops needs a manual flush, `cache.Flush()` already exists; an admin endpoint is a separate story.
