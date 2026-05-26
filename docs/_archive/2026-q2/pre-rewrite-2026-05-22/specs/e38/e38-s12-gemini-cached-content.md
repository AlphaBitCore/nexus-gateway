# E38-S12 — Gemini cachedContent Auto-Managed Lifecycle

**Epic:** E38 Prompt Cache Friendliness  
**Story:** S12 — Gemini cachedContent auto-managed lifecycle  
**Status:** Implemented

---

## User Story

As a platform operator, when traffic is routed to a Gemini provider, I want the AI Gateway to automatically create, reuse, and expire Gemini `cachedContent` objects for large system prompts, so that cache-read tokens reduce per-request token costs without any application-level SDK changes.

---

## Acceptance Criteria

| ID | Criterion |
|----|-----------|
| AC1 | For Gemini-format requests, the gateway extracts the `systemInstruction` block, computes a stable SHA-256 content hash, and checks whether a live `cachedContent` object exists for that hash in Redis. |
| AC2 | On a Redis miss, the gateway fires an async goroutine to create a new `cachedContent` via the Gemini v1beta REST API. The current request proceeds with the original (unmodified) body while the cache is being created. |
| AC3 | On a Redis hit, the gateway rewrites the outbound Gemini body: removes `systemInstruction` and injects `"cachedContent": "<name>"`. The upstream call uses fewer tokens on the prompt side. |
| AC4 | The gateway records `cache_read_tokens` via `usageMetadata.cachedContentTokenCount` in the traffic event (handled by `spec_gemini/codec.go` `DecodeResponse`, which already extracts this field into `providers.Usage.CachedTokens`). |
| AC5 | A circuit breaker tracks consecutive cache-creation failures. After 5 failures it opens for 5 minutes; during the open window creation goroutines are skipped so a broken Gemini API does not pile up goroutines. |
| AC6 | The feature is gated by `Config.Enabled` (false by default) and can be hot-reloaded via the `"gemini_cache"` Hub shadow key without a gateway restart. |
| AC7 | When `cachedContent` creation fails (quota, model not supported, etc.), the gateway falls back to the original request body transparently; the error is logged and metered but not returned to the caller. |
| AC8 | Content hash algorithm: `sha256(providerID + "|" + model + "|" + canonicalJSON(systemInstruction))`. Redis key: `"gemini:cc:" + hex(hash)`. Redis TTL: `TTLSeconds + 300`. Hash algorithm is documented in this SDD. |

---

## Background

Gemini's native caching mechanism works differently from Anthropic's:

| Aspect | Anthropic | Gemini |
|--------|-----------|--------|
| Cache signal | `cache_control` marker in request body | Separate `cachedContent` resource pre-created via REST API |
| Cache scope | Per-request prefix match | Pre-stored object looked up by `name` |
| Management | Implicit (provider manages) | Explicit: caller creates, uses, and the provider expires |
| TTL | Provider-managed (ephemeral or persistent) | Caller-set (default 60 min, max 1–7 days) |

Because Gemini requires out-of-band creation, the gateway acts as a `cachedContent` lifecycle manager backed by Redis.

---

## Architecture

### Package Layout

```
packages/ai-gateway/internal/cache/gemini/
├── config.go       — Config struct (Enabled, MinSystemChars, TTLSeconds)
├── key.go          — content hash computation (sha256 → Redis key)
├── client.go       — HTTP client for cachedContents REST API (create)
├── manager.go      — Manager: Inject(), circuit breaker, async create goroutine
├── metrics.go      — Prometheus counters (hit, miss, create_ok, create_err, skipped)
└── manager_test.go — unit tests
```

### Integration Point in proxy.go

The injection step is inserted **after L3+L4 normalisation** and **before `runViaBroker`** — that is, after `PrepareBody` has translated the OpenAI-format body to Gemini format and after the normaliser has stripped volatile fields and injected Anthropic-style cache markers (which are no-ops for Gemini). At this point `cachePreparedBody` holds the final Gemini-wire body ready for the injection step.

```
PrepareBody (OpenAI → Gemini translate)
  → L0 key normalisation (cache key computation)
  → L1 cache lookup
  → L3 body strip
  → L4 cache_control inject (no-op for Gemini)
  → [NEW] Gemini cachedContent inject         ← E38-S12 step
  → runViaBroker (upstream call)
```

### Request Flow

```
Inject(ctx, providerID, baseURL, model, body)
  │
  ├─ Enabled? No → return body unchanged
  ├─ Extract systemInstruction from Gemini body
  ├─ len(systemJSON) < MinSystemChars? → return body unchanged
  ├─ Compute redisKey = "gemini:cc:" + sha256hex(providerID|model|systemJSON)
  ├─ Redis GET redisKey
  │    MISS → circuit breaker open? No → async create goroutine
  │           return (originalBody, injected=false, nil)
  │    HIT  → parse cachedRecord.Name
  │           rewrite: delete "systemInstruction", set "cachedContent" = Name
  │           return (modifiedBody, injected=true, nil)
  └─ Error → fail-open (return originalBody, false, nil) + log
```

### Async Cache Creation

```
goroutine:
  1. Resolve (apiKey, baseURL) via KeyResolver.Resolve(ctx, providerID, modelID)
  2. Create: POST {baseURL}/v1beta/cachedContents
     Body: { "model": "models/{model}", "systemInstruction": {...}, "ttl": "{TTLSeconds}s" }
  3. On 200: store cachedRecord{Name, ExpireTime, TokenCount} in Redis
     Redis TTL = TTLSeconds + 300 (5 min grace)
     Reset circuit breaker consecutive failures
  4. On error: increment circuit breaker failures
     Log at WARN; do NOT propagate to caller
```

### Circuit Breaker

Simple atomic counter with a timestamp:
- `consecutiveFailures int64` — incremented on each create error
- `openUntil int64` — Unix nanoseconds; zero means closed
- Threshold: 5 failures → set openUntil = now + 5 min; reset consecutiveFailures
- On successful create: reset consecutiveFailures = 0, openUntil = 0

### Redis Key Schema

| Key | Value | TTL |
|-----|-------|-----|
| `gemini:cc:{sha256hex(providerID\|model\|systemJSON)}` | JSON `{"name":"cachedContents/abc","expire_time":"...","token_count":N}` | `TTLSeconds + 300` seconds |

### Content Hash Algorithm

```
hash_input = providerID + "|" + providerModelID + "|" + json.Marshal(systemInstruction)
redis_key  = "gemini:cc:" + hex(sha256(hash_input))
```

`systemInstruction` is the raw `gjson.Result.Raw` value extracted from the Gemini body — this is the canonical JSON string as emitted by `json.Marshal` in `spec_gemini/codec.go`.

### Body Rewrite on Cache Hit

Input:
```json
{
  "generationConfig": {...},
  "systemInstruction": {"parts": [{"text": "...long system prompt..."}]},
  "contents": [...]
}
```

Output:
```json
{
  "generationConfig": {...},
  "cachedContent": "cachedContents/abc123xyz",
  "contents": [...]
}
```

Implemented using `sjson.DeleteBytes` + `sjson.SetBytes`.

### Config Hot-Reload

`geminicache.Config` is loaded from the Hub shadow key `"gemini_cache"`. The `main.go` switch block handles:

```go
case "gemini_cache":
    var gcfg geminicache.Config
    json.Unmarshal(cs.State, &gcfg)
    geminiCacheMgr.Reload(gcfg)
```

### KeyResolver Interface

The `geminicache` package defines a minimal interface to avoid a direct `provtarget` dependency:

```go
type KeyResolver interface {
    Resolve(ctx context.Context, providerID, modelID string) (apiKey, baseURL string, err error)
}
```

Production wiring in `main.go` wraps `*provtarget.PgResolver`:

```go
type geminiKeyResolver struct{ r *provtarget.PgResolver }
func (g geminiKeyResolver) Resolve(ctx context.Context, providerID, modelID string) (string, string, error) {
    t, err := g.r.Resolve(ctx, providerID, modelID, provtarget.ResolveHints{})
    return t.APIKey, t.BaseURL, err
}
```

### Gemini cachedContents API

- **Create**: `POST https://generativelanguage.googleapis.com/v1beta/cachedContents`
  - Auth: `x-goog-api-key: {API_KEY}` header
  - Body: `{"model": "models/gemini-2.0-flash", "systemInstruction": {...}, "ttl": "3600s"}`
  - Response: `{"name": "cachedContents/abc123", "expireTime": "...", "usageMetadata": {"totalTokenCount": N}}`
- **Use in generateContent**: add `"cachedContent": "cachedContents/abc123"` to the request body; omit `systemInstruction`
- **Token minimum**: 4,096 tokens for Pro models (~16KB chars); 1,024 for Flash (~4KB chars). Configurable via `MinSystemChars`.

### Metrics

| Metric | Type | Labels |
|--------|------|--------|
| `nexus_aigw_gemini_cache_hit_total` | Counter | `model` |
| `nexus_aigw_gemini_cache_miss_total` | Counter | `model` |
| `nexus_aigw_gemini_cache_create_ok_total` | Counter | `model` |
| `nexus_aigw_gemini_cache_create_err_total` | Counter | `model` |
| `nexus_aigw_gemini_cache_skipped_total` | Counter | `reason` |

---

## Tasks

| # | Task | Status |
|---|------|--------|
| T1 | Write `geminicache/config.go` | Done |
| T2 | Write `geminicache/key.go` | Done |
| T3 | Write `geminicache/client.go` | Done |
| T4 | Write `geminicache/manager.go` | Done |
| T5 | Write `geminicache/metrics.go` | Done |
| T6 | Write `geminicache/manager_test.go` | Done |
| T7 | Wire `GeminiCacheMgr` into `handler/proxy.go` | Done |
| T8 | Wire `geminicache.Manager` in `cmd/ai-gateway/main.go` | Done |
| T9 | Build + unit test + VK smoke test | Done |

---

## Out of Scope (This Story)

- Vertex AI (`vertex` adapter type) — different auth model (OAuth/service account); separate story.
- Proactive TTL refresh (PATCH cachedContent before expiry) — Redis TTL grace period is sufficient for V1.
- Multi-turn conversation caching (caching `contents[]` beyond system instruction) — Gemini currently only supports system prefix.
- UI for listing/deleting cached content objects.
