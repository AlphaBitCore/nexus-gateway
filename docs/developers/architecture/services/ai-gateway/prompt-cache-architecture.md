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

## 3. Two-tier configuration

Prompt-cache behavior is configured through a two-tier model in `packages/shared/storage/cacheconfig`, shared by the Control Plane (DB I/O and blob assembly), the AI Gateway (resolution and the manager set), and config reconciliation (drift detection):

- **Tier 2 — `cache_adapter_config`** (one row per adapter family): the Gemini knobs (`cache_enabled`, `min_system_chars`, `ttl_seconds`, circuit-breaker threshold and open seconds), the Anthropic marker toggles, and a per-rule override map.
- **Tier 3 — `cache_provider_config`** (one row per provider): a strict subset of the adapter fields, overriding the family default for a single provider (no rule overrides at this tier).

The tiers keep their historical numbering because a **Tier 1 used to exist**: a `cache_global_config` singleton carrying two pipeline-wide switches, `NormaliserEnabled` and `CacheMasterKillSwitch`. Both are retired. Emergency gateway-side cache-off is served by the two surfaces in [cache-multi-tier-architecture.md](../../cross-cutting/storage/cache-multi-tier-architecture.md) (the fleet disable-all on `/ai-gateway/cache`, and Emergency Passthrough's time-boxed `bypassCache`), and the upstream wire-rewrite pipeline is now demand-driven off the Tier-2/Tier-3 rule and marker-inject settings rather than a global gate (see [shared-wirerewrite-architecture.md](../../cross-cutting/shared/shared-wirerewrite-architecture.md)). The `cache_global_config` table is orphaned — nothing reads or writes it — and is left in place pending a later cleanup migration.

`Resolve(blob, providerID, adapterType)` composes the tiers into a flat `ProviderEffective`: pointer fields are nil when "not set at this tier", so a knob inherits Tier 3 → Tier 2 → code default, and each resolved value records which tier supplied it. The whole `CacheConfigBlob` (`{adapters, providers}`) reaches the gateway over the Hub shadow key `cache`; the dispatch handler feeds it to `ManagerSet.SetConfig` and reloads the wire-rewrite config from the same blob.

## 4. Anthropic prompt-cache markers

Anthropic prompt caching works by marking the request rather than by uploading a
separate cache object, so it has no manager and no lifecycle — the whole mechanism
is one field on the outbound body.

The gateway uses Anthropic's **automatic caching**: a single `cache_control` at the
ROOT of the request. Anthropic then places the cache breakpoint on the last
cacheable block itself and advances it as the conversation grows, so the cached
prefix covers the whole request instead of a position the gateway guessed.

This replaced an explicit two-breakpoint scheme that stamped the last system text
block and, optionally, the second-to-last user message. Measured against every
Anthropic model the gateway routes to, one arm per request so no arm could read
what another wrote, the root marker cached the system prompt **and** the message
turn while the system-block marker cached only the system prompt — 14597 vs 12489
tokens on Sonnet 4.6, 27529 vs 23573 on Opus 4.7, the same ratio on all ten. The
scheme's second breakpoint fared worse still: anchored one turn behind the request,
it wrote a new entry almost every turn and rarely read one back.

**Where it lives.** `cache_control` is a field of the Anthropic Messages wire, so
the marker is written by the codec that speaks that wire
(`providers/specs/anthropic/codec/prompt_cache.go`), on **both** of that codec's
doors — `EncodeRequest` for a cross-format caller (an OpenAI `/v1/chat/completions`
request routed to Claude) and `RewriteNative` for a same-spec one (`/v1/messages`).
Which door a request takes is decided by the caller's ingress, something the codec
cannot see, so a rule on one door only would turn caching on for part of production
and nothing would notice.

**Caller intent wins.** A body that already carries any `cache_control` is
forwarded untouched. This is not politeness: Anthropic answers 400 when the last
block's marker names a different TTL than the root one, and again when four
explicit breakpoints already occupy every slot.

**Bedrock is excluded.** Its codec clears the flag before delegating. AWS documents
its InvokeModel Claude integration answering 400 for a root `cache_control`, and
nothing here has been measured against a live Bedrock endpoint — an unverified
field on a wire documented to reject it turns every request into a 400.

**Configuration.** The operator toggle is `marker_inject_enabled` on the same
three-tier blob, resolved Tier 3 → Tier 2 → code default. The AI Gateway holds the
blob in `internal/cache/promptcache` and resolves per request, so no provider list
is precomputed and the order the config loader applies shadow keys in cannot
disable markers. The resolved answer reaches the codec on
`CallTarget.PromptCacheMarkers`; the cache stage and the executor's target resolver
read the same holder, because the marker is a body edit and a leg that disagreed
would build the cache key over bytes the other leg does not send.

**The second breakpoint (`marker_boundary3_enabled`, off by default).** The
automatic breakpoint writes one entry, at the last cacheable block, and finds the
previous turn's entry by walking backward — but only 20 blocks. A turn that
appends more than that, which an agent round with many tool_use / tool_result
blocks does routinely, pushes the previous write out of reach and the conversation
stops hitting with no error to notice. This knob adds a second, explicit
breakpoint at the end of the previous assistant turn: a position that is stable
(finished content the next turn will not edit) and sits behind whatever the
current turn appended, however much that was.

Measured on the live wire with a turn appending 26 blocks, two arms with
independent session identities:

| turn | root marker only | root + second breakpoint |
|---|---|---|
| 1 | creation 11765, read 0 | creation 11767, read 0 |
| 2 (+26 blocks) | creation 14242, **read 0** | creation 2477, **read 11767** |
| 3 | creation 14512, **read 0** | creation 2747, **read 11767** |

Root-only stops hitting entirely once the lookback is exceeded and re-creates the
whole prefix every turn; the second breakpoint reads it back and writes only the
delta — about 4x cheaper on that turn at 1.25x write against 0.1x read.

Its historical anchor was the second-to-last USER message, which staging traffic
measured writing 2.8 tokens of cache for every token it read: that position moved
every turn and cached a prefix one turn shorter than the automatic breakpoint
already covered. The anchor above replaced it. `thinking` blocks are skipped —
they cannot carry `cache_control` and marking one is a 400.

Provider-reported prompt-cache token and cost stamping (cache-read / cache-creation
tokens, provider cache status) is covered in
[cost-estimation-architecture.md](cost-estimation-architecture.md) and
[normalization-architecture.md](normalization-architecture.md). The count of markers
the codec actually wrote reaches the audit row as
`traffic_event.cache_marker_injected` — what was sent, not what was configured: a
caller that sent its own marker is forwarded untouched and reports 0.

## References

- `packages/ai-gateway/internal/cache/gemini/manager.go` — cachedContent lifecycle: inject, async create, circuit breaker, stale-ref invalidation
- `packages/ai-gateway/internal/cache/gemini/managerset.go` — per-provider manager pool and config-driven rebuild
- `packages/ai-gateway/internal/cache/gemini/client.go` — Gemini `cachedContents` REST client
- `packages/ai-gateway/internal/cache/gemini/key.go` — content-hash key derivation and JSON canonicalization
- `packages/ai-gateway/internal/cache/gemini/config.go` — manager config and defaults
- `packages/shared/storage/cacheconfig/` — three-tier config types, `Resolve`, and the allocation-free `MarkerInjectEnabledFor`
- `packages/ai-gateway/internal/cache/promptcache/` — the live cache-config holder the request path reads
- `packages/ai-gateway/internal/providers/specs/anthropic/codec/prompt_cache.go` — the root `cache_control` marker, on both codec doors
- `packages/ai-gateway/cmd/ai-gateway/configdispatch/configdispatch.go` — `cache` shadow-key dispatch into the manager set, the prompt-cache holder, and the normaliser
- `packages/ai-gateway/internal/ingress/proxy/proxy.go` — Gemini inject integration and stale-ref invalidation hook
- `packages/shared/schemas/configkey/configkey.go` — `cache` shadow key
