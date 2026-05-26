# AI Gateway Prompt Cache

*Audience: operators tuning cost efficiency; contributors working on provider adapters.*

Prompt caching in Nexus Gateway reuses the upstream LLM provider's KV-cache across requests that share a common prefix. The gateway emits the cache directives each provider expects and parses back the reported cache statistics — the entire caching machinery lives on the provider's side. Configuring prompt cache correctly reduces input-token costs on repetitive workloads such as long system prompts, document-analysis pipelines, and multi-turn conversations.

---

## How provider-side prompt caching works

Prompt caching is entirely provider-side machinery. The provider's infrastructure stores the KV-state of tokenized prefix blocks; subsequent requests that share that prefix skip reprocessing them. The gateway's only involvement is emitting the right marker on the way in and reading the reported cache statistics on the way out.

Each supported provider has its own caching mechanism:

| Provider | Mechanism | Gateway action | Usage fields returned |
|---|---|---|---|
| **Anthropic** | Ephemeral KV cache; two TTL windows (~5-minute and 1-hour) with different prices | Pass `cache_control: { type: "ephemeral" }` markers on content blocks | `cache_read_input_tokens`, `cache_creation_input_tokens` |
| **OpenAI** | Implicit auto-caching on Service Tier for identical prefixes | Emit `service_tier: "auto"` when the adapter config enables it | `usage.prompt_tokens_details.cached_tokens` |
| **Google Gemini** | Explicit cached content object (two-step: create then reference) | Create or refresh a `cachedContents` resource; stamp `cachedContent: <name>` on each `generateContent` call | `usageMetadata.cachedContentTokenCount` |

The gateway's job is exactly three things: emit the directive in the correct wire shape, parse the reported cache numbers back into canonical `Usage.CachedTokens` / `Usage.CacheCreationTokens`, and apply the cached-input price from the `Model` row to compute the cost discount. All three live in the provider adapter under `packages/ai-gateway/internal/providers/specs/<adapter>/codec/`.

### Cross-format ingress and cache directives

When a request arrives over a non-native ingress (e.g., an OpenAI `/v1/chat/completions` call routed to Anthropic), the canonical bridge carries cache markers under `nexus.ext.<provider>.<key>` per adapter architecture Rule 4. The destination codec re-emits them in the destination provider's wire shape. This means Anthropic `cache_control` markers attached to a request arriving via the OpenAI ingress survive the cross-format canonicalization and reach the Anthropic upstream correctly.

## Where the code lives

**Per-adapter codecs** own all prompt-cache wire handling:

- [`packages/ai-gateway/internal/providers/specs/anthropic/codec/`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/packages/ai-gateway/internal/providers/specs/anthropic/codec/) — passes `cache_control` markers through canonical → wire and back; parses `cache_read_input_tokens` and `cache_creation_input_tokens`.
- [`packages/ai-gateway/internal/providers/specs/openai/codec/`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/packages/ai-gateway/internal/providers/specs/openai/codec/) — emits `service_tier: "auto"` when the route policy enables it; parses `prompt_tokens_details.cached_tokens`.
- [`packages/ai-gateway/internal/providers/specs/gemini/codec/`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/packages/ai-gateway/internal/providers/specs/gemini/codec/) — emits `cachedContent` references; parses `usageMetadata.cachedContentTokenCount`.

The **Gemini cache lifecycle** (create, refresh, TTL-manage) is the one operational concern beyond pure pass-through, because Gemini caches are explicit objects with their own API. That lifecycle lives under [`packages/ai-gateway/internal/cache/gemini/`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/packages/ai-gateway/internal/cache/gemini/):

- `manager.go` + `managerset.go` — per-config manager that calls `cachedContents.create` on first use, refreshes on near-expiry, and deletes on policy change.
- `client.go` + `config.go` — Gemini API client and per-provider config snapshot.
- `key.go` — cache-key derivation so two requests sharing a long system prompt reuse the same upstream `cachedContent`.
- `metrics.go` — Prometheus instrumentation (`nexus_aigateway_gemini_cache_*`).

There is no `internal/promptcache/` package. The other directories under `internal/cache/` (`core`, `layer`, `stream`, `semantic`, `freshness`, `budget`) all serve the gateway **response** cache — a separate concept documented in [AI Gateway Response Cache](AI-Gateway-Response-Cache).

## Configuration model

Prompt-cache settings share the three-tier cache config model with the response cache:

| Model | Scope | What it holds |
|---|---|---|
| `CacheGlobalConfig` (singleton) | Fleet-wide | `normaliser_enabled`, `cache_master_kill_switch` |
| `CacheAdapterConfig` (one row per adapter family) | Per adapter type | Anthropic marker knobs, Gemini `cachedContent` TTL, OpenAI `service_tier` toggle, plus per-rule override map |
| `CacheProviderConfig` | Per Provider row | Strict subset of `AdapterConfig`; absent row = fully inherits from adapter |

Admin CRUD for these three tables lives in the Control Plane Cache Settings page. The JSONB shape for each tier is governed by `packages/shared/cacheconfig/types.go` (`GlobalConfig`, `AdapterConfig`, `ProviderConfig`).

## Traffic event fields and cost impact

After the upstream responds, the provider codec parses cache usage into canonical `providers.Usage`:

- `CachedTokens` — input tokens served from the provider's cache (read path).
- `CacheCreationTokens` — input tokens written into a new provider cache entry (write path; Anthropic only, at a ~1.25× surcharge over the standard input price).

These are stamped onto `traffic_event.cache_read_tokens` and `traffic_event.cache_creation_tokens` at the five cost-stamping sites in `proxy.go` / `proxy_cache.go`. The cost function (`metrics.CalculateCost`) then applies the `cachedInputRead/WritePricePerMillion` columns from the `Model` row:

```
cost = uncachedInput     × inputPricePerMillion
     + cacheRead         × cachedInputReadPricePerMillion      (discount)
     + cacheCreation     × cachedInputWritePricePerMillion     (surcharge, Anthropic)
     + output            × outputPricePerMillion
```

Representative price ratios (from the `Model` rows seeded in `tools/db-migrate/`):

| Provider | Cache read / sticker | Cache write / sticker |
|---|---|---|
| Anthropic | 0.10× | 1.25× |
| OpenAI | 0.50× | 0× (no write surcharge — auto-cache is implicit) |
| Gemini | 0.25× | 0× (cache creation billed as a separate API call) |

The `provider_cache_status` column on `traffic_event` rolls up the provider prompt-cache outcome for the analytics UI:

| Value | Meaning |
|---|---|
| `hit` | Provider returned `cache_read_tokens > 0` — some input tokens were billed at the cache-read rate |
| `miss` | Provider was called, model supports caching, but `cache_read_tokens = 0` |
| `na` | No upstream call (gateway cache served it), or the model does not support prompt caching |

`provider_cache_status` feeds into the unified `traffic_event.cache_status` (`HIT` / `MISS`) that UI filter dropdowns bind to. See [AI Gateway Cost Estimation](AI-Gateway-Cost-Estimation) for the full formula and worked examples.

## Enabling and tuning prompt cache

### Anthropic

Enable `cache_control` pass-through in the Anthropic `CacheAdapterConfig` row (adapter family `anthropic`). The client request must include `cache_control: { type: "ephemeral" }` markers on the content blocks it wants cached (system prompt, long document prefixes). The gateway passes them through verbatim; the provider decides which blocks to cache based on prefix length and TTL.

For the 1-hour TTL SKU, Anthropic requires the content block to exceed a minimum token threshold (documented by Anthropic per model). The gateway does not enforce or auto-inject TTL choice — the caller's markers are used exactly as provided.

### OpenAI auto-caching

Set `service_tier_emit: true` in the OpenAI `CacheAdapterConfig` row. The gateway adds `"service_tier": "auto"` to every outbound request body. OpenAI applies the cache automatically for identical prefix prompts; no caller-side marker is needed. Cost analytics then shows `provider_cache_status=hit` on requests where OpenAI returned `prompt_tokens_details.cached_tokens > 0`.

### Gemini cached content

The Gemini cache manager (`packages/ai-gateway/internal/cache/gemini/manager.go`) handles the lifecycle automatically after the `CacheAdapterConfig` for Gemini enables it. The manager creates a `cachedContents` resource on first use, refreshes it before expiry, and deletes it when the config changes. The operator controls the TTL via `gemini_cached_content_ttl` in the adapter config (minimum 1 minute; Gemini-enforced minimum applies).

## Failure modes

| Failure | Behaviour |
|---|---|
| Gemini `cachedContents.create` fails | Request falls back to the no-cache path (full prompt forwarded). The manager logs the error and increments `nexus_aigateway_gemini_cache_create_errors_total`; no client-visible failure. |
| Anthropic `cache_control` marker rejected (bad shape) | Request fails at upstream with 4xx; the error normalizer in the Anthropic adapter maps the upstream error into the ingress-format error shape and returns it to the caller. |
| OpenAI Service Tier unavailable | OpenAI silently downgrades — the response simply lacks `cached_tokens`. `provider_cache_status` lands on `miss`; no error. |
| Adapter config snapshot empty on cold start | The codec emits no cache directive; the request behaves as if prompt cache is not yet configured. No error is surfaced; the next config-sync pull (after Hub signals) activates the config. |

---

## Canonical docs

- [`prompt-cache-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/prompt-cache-architecture.md) — full provider-by-provider mechanics and config model
- [`cost-estimation-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md) — cost stamping, `cache_status` derivation, and the three HIT × MISS worked examples
- [`response-cache-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/response-cache-architecture.md) — the gateway response cache (distinct from provider prompt cache)

**Adjacent wiki pages**: [AI Gateway Response Cache](AI-Gateway-Response-Cache) · [AI Gateway Cost Estimation](AI-Gateway-Cost-Estimation) · [AI Gateway Provider Adapters](AI-Gateway-Provider-Adapters) · [AI Gateway Overview](AI-Gateway-Overview)
