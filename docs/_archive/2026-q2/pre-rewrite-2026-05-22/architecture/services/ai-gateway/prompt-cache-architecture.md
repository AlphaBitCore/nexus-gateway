---
doc: prompt-cache-architecture
area: service
service: ai-gateway
tier: 1
updated: 2026-05-21
---

# Prompt Cache Architecture

> **Tier 1 architecture doc.** Read this before touching prompt-cache code, Gemini cachedContents integration, or the Cache Settings UI. The Nexus gateway **response cache** (extract / semantic) is a separate concept and lives in `response-cache-architecture.md`. The unified `cache_status` rollup column on `traffic_event` (HIT | MISS) is documented in `cost-estimation-architecture.md` §6.4.

Prompt caching reuses the upstream model's KV-cache (or equivalent) across requests that share a prefix. **It is entirely provider-side machinery** — the gateway emits the cache directives the provider expects (Anthropic `cache_control` markers, Gemini cached-content references, OpenAI Service Tier auto-caching) and observes the provider's reported cache statistics in the response usage block. The gateway does not maintain a per-request shadow of the provider's KV-cache.

The internal cache directories under `packages/ai-gateway/internal/cache/` (`core`, `layer`, `stream`, `semantic`, `freshness`, `budget`) serve the gateway **response** cache (covered in `response-cache-architecture.md`), not prompt caching. The one exception is `internal/cache/gemini/`, documented in §2 — that lifecycle management is specific to Gemini's explicit `cachedContents` API.

---

## 1. The single tier — provider-side cached content

There is one prompt-cache tier, and it lives in the upstream provider:

| Provider | Mechanism | Marker / API | Usage reporting |
|---|---|---|---|
| **Anthropic** | Ephemeral KV cache, ~5-minute TTL (5m) plus 1-hour SKU. Author marks content blocks with `cache_control: { type: "ephemeral" }`. | Inline marker on request content blocks. | `usage.cache_read_input_tokens`, `usage.cache_creation_input_tokens` |
| **Gemini** | Explicit cached content created via the `cachedContents.create` REST resource; subsequent `generateContent` calls reference it by name. | Two-step: separate cache-create call returns a `name`; later requests include `cachedContent: <name>`. | `usageMetadata.cachedContentTokenCount` |
| **OpenAI Service Tier** | Implicit — OpenAI auto-caches identical prefix prompts on the Service Tier (`service_tier: "auto"`). No explicit marker. | Per-request body field `service_tier: "auto"`. | `usage.prompt_tokens_details.cached_tokens` |

A request walks the path: gateway emits the appropriate directive → provider decides whether to use its own cache → provider returns usage with cache stats → gateway stamps those stats on `traffic_event` for billing + analytics.

The **gateway's only job** is (a) emit the directive in the right wire shape for each provider **when injection is enabled for that provider** (see §4 — injection is opt-in per provider and short-circuits when the client has already supplied cache markers), (b) parse the provider's reported cache numbers back into canonical `Usage.CachedTokens` / `Usage.CacheCreationTokens`, and (c) compute the cost discount on those tokens at the configured cache-price columns. All three live in the provider adapter (`packages/ai-gateway/internal/providers/specs/<adapter>/codec/`); none of them live in a separate "prompt cache" package.

> Cache directive injection is **opt-in** and **caller-respecting** — this doc is the canonical mechanism reference; the repo root `README.md` links here for the user-facing summary.

## 2. Where the code is

The **provider adapters** own all prompt-cache wire handling:

- `packages/ai-gateway/internal/providers/specs/anthropic/codec/` — passes `cache_control` markers through canonical → wire and back; parses `cache_read_input_tokens` + `cache_creation_input_tokens` from the response Usage.
- `packages/ai-gateway/internal/providers/specs/openai/codec/` — emits `service_tier: "auto"` when the route policy enables it; parses `prompt_tokens_details.cached_tokens` from the response.
- `packages/ai-gateway/internal/providers/specs/gemini/codec/` — emits `cachedContent: <name>` references on `generateContent` calls; parses `usageMetadata.cachedContentTokenCount` from the response.

The **Gemini cachedContents lifecycle** (creating, refreshing, and TTL-managing the upstream cache resource) is the one operational concern the gateway carries beyond pure pass-through, because Gemini caches are explicit objects with their own create/delete API. That lifecycle lives in `packages/ai-gateway/internal/cache/gemini/`:

- `manager.go` + `managerset.go` — per-config manager that calls `cachedContents.create` on first use, refreshes on near-expiry, and deletes on policy change.
- `client.go` + `config.go` — Gemini API client + per-provider config snapshot.
- `key.go` — cache-key derivation for the manager (so two requests sharing a long system prompt reuse the same upstream `cachedContent`).
- `metrics.go` — Prometheus instrumentation (`nexus_aigateway_gemini_cache_*`).

There is no `internal/promptcache/` package. The other directories under `internal/cache/` (`core`, `layer`, `stream`, `semantic`, `freshness`, `budget`) all serve the gateway **response** cache and are documented in `response-cache-architecture.md`.

## 3. Configuration

Prompt-cache configuration is part of the 3-tier cache config model in `tools/db-migrate/schema.prisma:2394-2434`:

| Model | Scope | What it holds |
|---|---|---|
| `CacheGlobalConfig` (singleton) | Fleet-wide | `normaliser_enabled`, `cache_master_kill_switch`. JSONB shape governed by `packages/shared/storage/cacheconfig/types.go::GlobalConfig`. |
| `CacheAdapterConfig` (one row per adapter family) | Per adapter type (`anthropic`, `openai`, `gemini`, …) | Adapter-specific cache knobs — `anthropic_marker_*`, `gemini_cached_content_ttl`, OpenAI `service_tier` toggle — plus the rule override map nested under `rules`. JSONB shape governed by `cacheconfig.AdapterConfig`. |
| `CacheProviderConfig` (per-provider override) | Per Provider row | Strict subset of `AdapterConfig` (no `rules` — those stay Tier 2). Empty/absent row = "fully inherits from adapter + global". Validated on PUT that every key is appropriate for the Provider's `adapter_type`. |

There is no `prompt_cache_policy` Prisma model — that name was an earlier design proposal that never landed. Admin CRUD for these three tables lives in the Control Plane "Cache Settings" page (the same page that controls extract-cache + semantic-cache on its sibling sections).

## 4. Cache directive emission (request side)

Each adapter codec is responsible for emitting the right wire-level directive **only when both gates pass**: (a) injection is enabled for the destination provider, and (b) the caller has not already set a `cache_control` field (explicit caller intent wins). The actual gates:

- **Per-provider injection toggle** — `packages/shared/transport/wirerewrite/engine.go:252` guards the Anthropic/Bedrock-Claude injection branch behind `resolved.providerInjectEnabled[providerID]`. If the provider's `CacheProviderConfig` does not enable injection (or the global / adapter-level config is off), the engine skips the rewrite and the upstream request goes out with whatever markers the caller supplied — none, if the caller supplied none.
- **Caller-set short-circuit** — `packages/shared/transport/wirerewrite/rule_cache_inject.go:32-44` checks `countExistingMarkers(body) > 0` at the top of `injectCacheMarkers`; if the client already set any `cache_control` (root or block level), the body is returned unchanged. Explicit caller intent is always respected.

Per-provider mechanism, gated on both checks above:

- **Anthropic / Bedrock-Claude** — the canonical request carries `cache_control` markers on content blocks. When the caller omits them and `providerInjectEnabled` is true, the wirerewrite engine auto-injects ephemeral markers on the system prompt and the last user message (and optionally the third-to-last assistant boundary, when `providerBoundary3` is enabled). The codec passes the (possibly injected) markers through to the upstream verbatim.
- **OpenAI** — when `service_tier_emit = true` in the adapter config, the codec adds `service_tier: "auto"` to the outgoing request body before calling `PrepareBody`. There is no caller-set short-circuit for this field today; if the caller already set `service_tier`, the codec overwrites it — track that gap if it matters for your use case.
- **Gemini** — when the route uses an explicit `cachedContent` reference, the Gemini cache manager (`internal/cache/gemini/manager.go`) resolves the name to use and the codec stamps it onto the request body's `cachedContent` field. The caller cannot meaningfully "already set" a `cachedContent` they don't own, so no short-circuit applies.


Cross-format ingress (e.g., a request arriving as OpenAI Chat but routed to Anthropic) goes through `internal/execution/canonicalbridge` first — the canonical body carries cache markers under `nexus.ext.<provider>.<key>` per provider-adapter Rule 4, and the destination codec re-emits them in the destination provider's wire shape.

## 5. Usage parsing (response side)

After the upstream responds, the codec parses cache usage back into canonical `providers.Usage`:

- **Anthropic** — `cache_read_input_tokens` → `Usage.CachedTokens`; `cache_creation_input_tokens` → `Usage.CacheCreationTokens`. The normalizer at `packages/shared/transport/normalize/codecs/anthropic_messages.go` sums all three (uncached + read + creation) into the canonical `Usage.PromptTokens` to match the OpenAI convention; the per-bucket split is preserved separately.
- **OpenAI** — `prompt_tokens_details.cached_tokens` → `Usage.CachedTokens`; OpenAI has no equivalent of cache creation (auto-cache is implicit), so `CacheCreationTokens = 0`.
- **Gemini** — `usageMetadata.cachedContentTokenCount` → `Usage.CachedTokens`; cache creation is a separate API call billed independently and is not stamped on the request's `traffic_event`.

The cost-stamp sites in `proxy.go` / `proxy_cache.go` (`cost-estimation-architecture.md` §3.3) stamp these onto `traffic_event.cache_read_tokens` and `traffic_event.cache_creation_tokens`; the cost function (`metrics.CalculateCost`) then computes the discount using the `cachedInputRead/WritePricePerMillion` columns on the `Model` row.

## 6. `provider_cache_status` — the rollup field for UI

`provider_cache_status` (one column on `traffic_event`, added in E59) is the **single field** the audit drawer and analytics queries use to express what the provider prompt cache did on this request:

| Value | Meaning |
|---|---|
| `hit` | Provider's response reported cached input tokens (Anthropic `cache_read_input_tokens > 0`, OpenAI `prompt_tokens_details.cached_tokens > 0`, Gemini `cachedContentTokenCount > 0`). At least some input tokens were billed at the cache-read rate. |
| `miss` | Provider was called, the model supports prompt caching, and `cache_read_tokens = 0`. |
| `na` | The provider was **not** called this request (gateway cache hit / singleflight coalesce) OR the provider was called but the model doesn't support prompt caching (e.g., a self-hosted vLLM endpoint). The drawer disambiguates these two sub-cases by checking whether `cache_read_tokens` is NULL (no provider call) vs zero (called, no cache support). |

`provider_cache_status` rolls up into the unified `traffic_event.cache_status` (`HIT | MISS`) per `cost-estimation-architecture.md` §6.4. UI filters bind to the unified field; this column is detail-only.

## 7. Failure modes

| Failure | Behaviour |
|---|---|
| Gemini cache-create call fails | Request falls back to the no-cache path (full prompt forwarded). The Gemini manager logs + increments `nexus_aigateway_gemini_cache_create_errors_total`; no client-visible failure. |
| Anthropic `cache_control` marker rejected (bad shape) | Request fails at upstream with 4xx; the gateway returns the upstream error envelope to the caller. The error normalizer in `internal/providers/specs/anthropic/codec/` maps the upstream 4xx into the ingress-format error shape per `provider-adapter-architecture.md` §3a. |
| OpenAI Service Tier unavailable | OpenAI silently downgrades — the response simply doesn't carry `cached_tokens`. `provider_cache_status` lands on `miss`; no error. |
| Adapter config snapshot empty (cold start before first Hub pull) | The codec emits no cache directive; the request behaves as if the operator hadn't configured prompt cache yet. No error. |

## 8. Smoke test virtual key

A standing local test VK exists (memory `project_local_test_vk`). The smoke flow for prompt cache:

1. Send a request with a long prefix + short tail (with the adapter's prompt-cache directive enabled).
2. Verify `traffic_event.cache_creation_tokens > 0` on the first response (Anthropic) or that the upstream auto-cached it (OpenAI / Gemini).
3. Repeat the same prefix + a different tail.
4. Verify `traffic_event.cache_read_tokens > 0` and `provider_cache_status = 'hit'` on the second response.

This smoke is integrated into the `/smoke-gateway` skill.

## 9. Sources

- `packages/ai-gateway/internal/providers/specs/anthropic/codec/` — Anthropic `cache_control` passthrough + usage parsing.
- `packages/ai-gateway/internal/providers/specs/openai/codec/` — OpenAI Service Tier emission + `cached_tokens` parsing.
- `packages/ai-gateway/internal/providers/specs/gemini/codec/` — Gemini `cachedContent` reference emission + `cachedContentTokenCount` parsing.
- `packages/ai-gateway/internal/cache/gemini/` — Gemini cachedContents lifecycle (create / refresh / delete).
- `packages/shared/transport/normalize/codecs/anthropic_messages.go` — canonical-Usage sum-up convention.
- `packages/shared/storage/cacheconfig/types.go` — `GlobalConfig`, `AdapterConfig`, `ProviderConfig` Go shapes.
- `tools/db-migrate/schema.prisma` — `CacheGlobalConfig`, `CacheAdapterConfig`, `CacheProviderConfig` models (lines 2386-2426); `traffic_event.{cache_status, gateway_cache_status, gateway_cache_skip_reason, gateway_cache_kind, provider_cache_status, cache_read_tokens, cache_creation_tokens}` columns.

## 10. Cross-references

- `response-cache-architecture.md` — the Nexus gateway response cache (extract + semantic). The Cache Settings UI in the Control Plane hosts both this and the Provider Prompt Cache section.
- `provider-adapter-architecture.md` — token-field stamping rule + canonical ↔ wire translation rules (§3a) that constrain how cache directives flow through cross-format ingress.
- `cost-estimation-architecture.md` §6.4 + §6.5 — unified `cache_status` rollup and the three HIT × MISS worked examples (Case B is the provider-prompt-cache discount case).
- `routing-architecture.md` — route policy resolution that determines which adapter config applies.
- `quota-architecture.md` — cached tokens still count toward usage (but at the cached-input price).
