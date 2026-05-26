# Feature Prompt Cache

Prompt caching reuses the upstream model's KV-cache (or equivalent) across requests that share a prompt prefix, reducing both input-token cost and time-to-first-token. Nexus manages the provider-specific wire details so applications do not need to change their API calls — the gateway emits the right directives and parses the resulting cache statistics back into the traffic event for billing and analytics.

---

## What Nexus does

Prompt caching is entirely provider-side machinery. The gateway's job is to emit the correct cache directive for each provider and record the outcome:

| Provider | Mechanism | What Nexus emits | What Nexus records |
|---|---|---|---|
| Anthropic | Ephemeral KV cache, ~5-minute TTL. Content blocks marked with `cache_control: { type: "ephemeral" }`. | Passes `cache_control` markers through canonical → wire. Auto-injects markers on system prompt and last user message when the config flag is on. | `cache_read_input_tokens`, `cache_creation_input_tokens` from usage block. |
| OpenAI | Implicit Service Tier auto-caching for identical prefix prompts on `service_tier: "auto"`. | Adds `service_tier: "auto"` to the request body when enabled. | `prompt_tokens_details.cached_tokens` from usage block. |
| Gemini | Explicit cached content created via `cachedContents.create` REST resource; referenced by name on subsequent calls. | Manages the `cachedContent` resource lifecycle (create, refresh on near-expiry, delete on policy change). Stamps the `cachedContent` field on `generateContent` calls. | `usageMetadata.cachedContentTokenCount` from usage metadata. |

For each provider, the gateway maps the provider's cache statistics into the canonical `Usage.CachedTokens` and `Usage.CacheCreationTokens` fields. These stamp `traffic_event.cache_read_tokens` and `traffic_event.cache_creation_tokens`. The cost function then computes the discount using the `cachedInputReadPricePerMillion` and `cachedInputWritePricePerMillion` columns on the `Model` row.

## Cache status visibility

The `provider_cache_status` column on `traffic_event` summarises the prompt-cache outcome per request:

| Value | Meaning |
|---|---|
| `hit` | Provider reported cached input tokens — at least some input was billed at the discounted cache-read rate. |
| `miss` | Provider was called, the model supports prompt caching, but no cached tokens were reported. |
| `na` | Provider was not called (gateway response-cache hit), or the model does not support prompt caching. |

This field rolls up into the unified `traffic_event.cache_status` (`HIT | MISS`) that UI filters bind to. The Traffic Event drawer in the Control Plane shows both the unified field and the per-token split (input uncached / cache-read / cache-creation / output) so operators can see exactly what was discounted.

## Cross-format requests

When a request arrives via one ingress and routes to a different provider's wire format (e.g., OpenAI-ingress request routed to Anthropic), cache markers travel through the canonical bridge. Extension fields ride inside `nexus.ext.<provider>.<key>` on the canonical form, and the destination adapter re-emits them in the correct wire shape. This means an application sending standard OpenAI Chat requests can still benefit from Anthropic's prompt caching without any change to the calling code.

## Where it sits

Prompt-cache wire handling lives inside each **provider adapter**:

- `packages/ai-gateway/internal/providers/specs/anthropic/codec/` — Anthropic `cache_control` passthrough and usage parsing.
- `packages/ai-gateway/internal/providers/specs/openai/codec/` — OpenAI Service Tier emission and `cached_tokens` parsing.
- `packages/ai-gateway/internal/providers/specs/gemini/codec/` — Gemini `cachedContent` reference emission and `cachedContentTokenCount` parsing.

The Gemini `cachedContents` lifecycle (create, refresh, delete) lives in `packages/ai-gateway/internal/cache/gemini/`. There is no `internal/promptcache/` package — the other directories under `internal/cache/` (`core`, `layer`, `stream`, `semantic`, `freshness`) all serve the gateway **response** cache, documented in [Feature Response Cache](Feature-Response-Cache).

## How to enable and configure

Prompt-cache configuration follows a 3-tier model in the Control Plane **AI Gateway → Cache** page:

- **Global config** (fleet-wide) — `normaliser_enabled` and a cache master kill switch. Disabling the kill switch turns off all cache tiers globally for incident response.
- **Adapter config** (per adapter family) — adapter-specific knobs: `anthropic_marker_*`, `gemini_cached_content_ttl`, OpenAI `service_tier` toggle. This is where prompt caching is enabled or disabled per provider type.
- **Provider config** (per-provider override) — strict subset of adapter config for cases where one specific provider instance needs different settings. An absent row inherits from the adapter tier.

To enable Anthropic prompt caching, set the `anthropic_marker_auto_inject = true` flag on the Anthropic adapter config. No application code change is needed.

For Gemini, set a `gemini_cached_content_ttl` on the adapter config. The gateway manages the two-step create/reference flow automatically. Cache creation failures fall back to the no-cache path with no client-visible error — the Gemini manager logs the failure and increments `nexus_aigateway_gemini_cache_create_errors_total`.

For OpenAI, enable `service_tier_emit = true` on the adapter config. OpenAI silently downgrades if the Service Tier is unavailable; the request proceeds at full token rates with `provider_cache_status = miss`.

## Failure modes

| Failure | Behaviour |
|---|---|
| Gemini `cachedContents.create` fails | Falls back to full prompt, no cache. No client error. |
| Anthropic `cache_control` marker rejected | Request fails with the upstream 4xx mapped to the ingress-format error shape. |
| OpenAI Service Tier unavailable | OpenAI silently downgrades. `provider_cache_status = miss`. No error. |
| Adapter config not yet synced (cold start) | No cache directive emitted; request behaves as if prompt cache is disabled. |

---

## Canonical docs

- [`prompt-cache-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/prompt-cache-architecture.md) — single-tier model, per-provider wire details, configuration 3-tier model, usage parsing, cache status field, failure modes, smoke test flow
- [`cost-estimation-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md) — how cached-token costs are computed and stamped on `traffic_event`

**Adjacent wiki pages**: [Feature Response Cache](Feature-Response-Cache) · [Feature Cost Tracking](Feature-Cost-Tracking) · [AI Gateway Prompt Cache](AI-Gateway-Prompt-Cache) · [AI Gateway Cost Estimation](AI-Gateway-Cost-Estimation) · [Features Index](Features-Index)
