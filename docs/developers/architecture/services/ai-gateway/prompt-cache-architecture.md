# Prompt cache architecture

Prompt caching is provider-side caching of a request's large, stable prefix so repeated calls pay for it once. This page covers the prompt-cache work the AI Gateway actively manages — the **Gemini `cachedContent`** lifecycle — and the **three-tier cache configuration** that drives it. It is distinct from the response cache (the L1/L2 reply cache in [response-cache-architecture.md](response-cache-architecture.md)): prompt caching keeps the upstream call but shrinks its billed input; the response cache skips the upstream call entirely.

## 1. Gemini cachedContent

When a Gemini request carries a large `systemInstruction`, the gateway uploads that instruction to Gemini's `cachedContents` API once and rewrites later requests to reference the returned cache object instead of resending the full text — cutting prompt-token cost on every reuse. When the same request also carries `tools` / `toolConfig` (function-calling — e.g. the operator agent), those blocks are folded into the same cache object: Gemini **forbids** a request that references a `cachedContent` from also setting `systemInstruction`, `tools`, or `toolConfig` (it returns `400 CachedContent can not be used with GenerateContent request setting system_instruction, tools or tool_config`), so anything cached must be removed from the wire on a hit. The lifecycle manager lives in `packages/ai-gateway/internal/cache/gemini`.

`Manager.Inject` is the entry point and is fully fail-open — the caller always uses the returned body, whatever happens:

1. If the manager is disabled, the body has no `systemInstruction`, or that instruction is shorter than `min_system_chars`, the request passes through unchanged.
2. Otherwise the manager computes a content hash over `(providerID, model, systemInstruction, tools, toolConfig)` — `tools`/`toolConfig` only contribute when present — and looks it up in Redis.
3. **Hit** — it rewrites the body (`rewriteBody` deletes `systemInstruction`, `tools`, and `toolConfig`, then sets `cachedContent` to the stored name) and returns an `InjectResult` carrying the cache name and an `Invalidate` closure.
4. **Miss** — it fires an asynchronous creation goroutine and passes the original body through this time; the next matching request gets the hit.

### Content hash

The Redis key is `gemini:cc:<sha256(providerID|model|canonical-systemInstruction[|tools|canonical-tools][|toolcfg|canonical-toolConfig])>`. The system instruction (and `tools`/`toolConfig` when present) is canonicalized through a JSON round-trip (sorted keys, whitespace stripped) before hashing, so the same logical instruction produces the same key whether it arrived through the canonical bridge (compact JSON) or through native `/v1beta` passthrough (pretty JSON, different key order). Without that normalization the two ingresses would hash differently and never reuse each other's cached content. The `tools`/`toolConfig` segments are appended only when those blocks are present, so a request with no tools keys identically to the historical system-only form — existing cache entries keep hitting — while two requests that share a system prompt but use different tool sets correctly map to different cache objects.

### Asynchronous creation and the TTL invariant

On a miss, `asyncCreate` resolves the provider's API key and base URL, calls `POST {baseURL}/v1beta/cachedContents` (adding the required `models/` prefix, the request's `tools`/`toolConfig` when present, and a `ttl` of `<n>s`), and stores the returned record in Redis. The Redis TTL is set **strictly shorter** than the Gemini object's TTL — `ttl_seconds - 300`, floored at 60 seconds. This is a load-bearing invariant: if Redis outlived the Gemini object it would keep vending a `cachedContent` name that Gemini had already evicted, and every request would fail with `403 CachedContent not found`. Keeping Redis shorter guarantees the reference is dropped before Gemini's own eviction.

### Stale-reference invalidation

Gemini's eviction is best-effort, so an object can still disappear while Redis points at it. When the upstream returns `403 "CachedContent not found (or permission denied)"`, the proxy calls the hit's `Invalidate` closure, which deletes the Redis entry so the next request regenerates instead of looping on the stale reference. The proxy stashes this closure on the request context so the streaming and non-streaming response paths can fire it.

### Circuit breaker

Consecutive creation failures trip a per-manager circuit breaker (default threshold five). While open (default 300 seconds) the manager skips creation attempts and passes requests through, so a failing Gemini cachedContents endpoint degrades to plain prompt forwarding rather than stalling every request. A successful creation resets the breaker.

## 2. Per-provider manager set

`ManagerSet` holds one `Manager` per Gemini/Vertex provider. The hot-path lookup is `Get(providerID)` — a single `sync.Map` load that returns nil for any non-Gemini/Vertex provider, short-circuiting the inject block in the proxy. The set is reconciled by `SetConfig` (a new cache config blob) and `ReloadProviders` (a changed provider list); both converge on a rebuild that resolves each provider's effective config, reuses existing managers via `Reload`, creates managers for new providers, and tears down managers for providers absent from the current list.

The proxy wires this in before the broker: for a Gemini-format primary target it calls `Get(providerID)` and, if a manager exists, `Inject`. On success it swaps in the rewritten body and captures the `Invalidate` hook.

## 3. Three-tier configuration

Prompt-cache behavior is configured through a three-tier model in `packages/shared/storage/cacheconfig`, shared by the Control Plane (DB I/O and blob assembly), the AI Gateway (resolution and the manager set), and config reconciliation (drift detection):

- **Tier 1 — `cache_global_config`** (singleton): pipeline-wide switches — `NormaliserEnabled` (gates the upstream wire-rewrite pipeline; see [shared-wirerewrite-architecture.md](../../cross-cutting/shared/shared-wirerewrite-architecture.md)) and `CacheMasterKillSwitch`, an emergency switch that disables all gateway-side caching for every provider.
- **Tier 2 — `cache_adapter_config`** (one row per adapter family): the Gemini knobs (`cache_enabled`, `min_system_chars`, `ttl_seconds`, circuit-breaker threshold and open seconds), the Anthropic marker toggles, and a per-rule override map.
- **Tier 3 — `cache_provider_config`** (one row per provider): a strict subset of the adapter fields, overriding the family default for a single provider (no rule overrides at this tier).

`Resolve(blob, providerID, adapterType)` composes the three tiers into a flat `ProviderEffective`: pointer fields are nil when "not set at this tier", so a knob inherits Tier 3 → Tier 2 → Tier 1 → code default, and each resolved value records which tier supplied it. The whole `CacheConfigBlob` (`{global, adapters, providers}`) reaches the gateway over the Hub shadow key `cache`; the dispatch handler feeds it to `ManagerSet.SetConfig` and reloads the normaliser config from the same blob.

## 4. Relationship to Anthropic prompt caching

The same config blob's Tier-2/Tier-3 rows also carry the Anthropic prompt-cache marker toggles (`marker_inject_enabled`, `marker_boundary3_enabled`). Anthropic prompt caching works by injecting `cache_control` breakpoints into the request rather than by uploading a separate cache object, so its mechanism lives in the wire-rewrite layer rather than in this manager. See [shared-wirerewrite-architecture.md](../../cross-cutting/shared/shared-wirerewrite-architecture.md). The provider-reported prompt-cache token and cost stamping (cache-read / cache-creation tokens, provider cache status) is covered in [cost-estimation-architecture.md](cost-estimation-architecture.md) and [normalization-architecture.md](normalization-architecture.md).

## References

- `packages/ai-gateway/internal/cache/gemini/manager.go` — cachedContent lifecycle: inject, async create, circuit breaker, stale-ref invalidation
- `packages/ai-gateway/internal/cache/gemini/managerset.go` — per-provider manager pool and config-driven rebuild
- `packages/ai-gateway/internal/cache/gemini/client.go` — Gemini `cachedContents` REST client
- `packages/ai-gateway/internal/cache/gemini/key.go` — content-hash key derivation and JSON canonicalization
- `packages/ai-gateway/internal/cache/gemini/config.go` — manager config and defaults
- `packages/shared/storage/cacheconfig/` — three-tier config types and `Resolve`
- `packages/ai-gateway/cmd/ai-gateway/configdispatch/configdispatch.go` — `cache` shadow-key dispatch into the manager set and normaliser
- `packages/ai-gateway/internal/ingress/proxy/proxy.go` — Gemini inject integration and stale-ref invalidation hook
- `packages/shared/schemas/configkey/configkey.go` — `cache` shadow key
