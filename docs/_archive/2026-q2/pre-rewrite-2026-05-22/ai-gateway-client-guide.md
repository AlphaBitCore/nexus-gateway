# Nexus AI Gateway — Client API Guide

> **Audience**: SDK developers and application engineers integrating with
> Nexus AI Gateway. Operators / admins: see `docs/developers/architecture/` + `docs/operators/ops/`.
>
> **Base URL**: configured per-deployment (e.g. `https://api.example.com`
> in prod, `http://localhost:3050` in local dev).
>
> **Default content-type**: `application/json` (multipart only for the
> embeddings-with-binary path).

---

## Table of contents

1. [What makes Nexus different](#1-what-makes-nexus-different)
2. [Authentication — Virtual Keys](#2-authentication--virtual-keys)
3. [Endpoint catalog](#3-endpoint-catalog)
4. [Calling the same model with different SDK shapes](#4-calling-the-same-model-with-different-sdk-shapes)
5. [The `nexus.*` request-body extension namespace](#5-the-nexus-request-body-extension-namespace)
6. [`nexus.dry_run` — cost estimate without an upstream call](#6-nexusdry_run--cost-estimate-without-an-upstream-call)
7. [`POST /v1/estimate` — multi-target compare](#7-post-v1estimate--multi-target-compare)
8. [Streaming](#8-streaming)
9. [Caching](#9-caching)
10. [Reasoning / extended-thinking](#10-reasoning--extended-thinking)
11. [Response headers](#11-response-headers)
12. [Request headers Nexus reads](#12-request-headers-nexus-reads)
13. [Rate limits + quota](#13-rate-limits--quota)
14. [Errors — envelopes by ingress](#14-errors--envelopes-by-ingress)
15. [Read endpoints: `/v1/models`, `/v1/usage`](#15-read-endpoints-v1models-v1usage)
16. [Migration cheatsheet: pointing your OpenAI / Anthropic / Gemini SDK at Nexus](#16-migration-cheatsheet)
17. [Embeddings — cross-format routing + capability mismatch (E62)](#17-embeddings--cross-format-routing--capability-mismatch-e62)

---

## 1. What makes Nexus different

Nexus is **not** a wrapper that adds latency for one provider. It is a
gateway that lets a single application:

- **Speak any of 4 popular SDK wire formats** (OpenAI Chat Completions,
  OpenAI Responses API, Anthropic Messages, Gemini generateContent) and
  reach **any** model the admin has wired up — including models the
  caller's SDK doesn't natively support. Your OpenAI-SDK code can call
  Claude, your Anthropic-SDK code can call Gemini, no shim required.
- **Estimate cost before sending** via either an in-band
  `nexus.dry_run: true` flag (works with your existing SDK code) or the
  dedicated `POST /v1/estimate` endpoint (multi-target compare).
- **Carry provider-specific extensions losslessly** through the
  `nexus.ext.<provider>.<key>` namespace, even when you're calling via
  a different ingress (e.g. Anthropic's `thinking` config survives a
  cross-format call from your OpenAI SDK).
- **Get tier-1 caching transparently** — both Nexus's response cache
  (exact-match dedup across requests) and the provider's own prompt
  cache (Anthropic prompt cache, Gemini cachedContent). Status is
  reported via the `x-nexus-cache` response header.
- **Get usage + cost stamping on every traffic_event row** — `/v1/usage`
  reports daily/monthly token + USD totals scoped by Virtual Key.

The product surface is intentionally close to OpenAI's so an existing
OpenAI SDK can be re-pointed at Nexus with one base-URL swap.

---

## 2. Authentication — Virtual Keys

Every request requires a **Virtual Key** (VK) — an `nvk_…` token issued
by an admin via the Nexus Control Plane. The VK is the unit of:

- cost budget (per VK in USD)
- model access (per VK allowedModels list, optional)
- rate limiting (real-call RPM, dry-run RPM, `/v1/estimate` RPM)
- analytics scoping (every traffic_event row carries the VK fingerprint)

### Header carriers — pick whichever your SDK already sends

Nexus accepts the VK on **any** of the following, scanned in this order
per request:

| Header / param | Always honoured? | Notes |
|---|---|---|
| `x-nexus-virtual-key: nvk_…` | yes, all routes | Nexus-native, easiest in custom clients |
| `Authorization: Bearer nvk_…` | yes, all routes | The default OpenAI SDK pattern |
| `x-api-key: nvk_…` | only on `/v1/messages` | Anthropic SDK convention |
| `x-goog-api-key: nvk_…` OR `?key=nvk_…` | only on `/v1beta/…` (Gemini) | Gemini SDK convention |
| `api-key: nvk_…` | only on `/openai/deployments/…` | Azure OpenAI SDK convention |

The header that matches the caller's SDK is always recognised on the
matching native route — you do not need to rewrite anything when
pointing an Anthropic / Gemini / Azure-OpenAI SDK at Nexus, only the
base URL.

### Auth errors

| HTTP | `error.code` | Trigger |
|---|---|---|
| 401 | `AUTH_KEY_MISSING` | No VK in any carrier |
| 401 | `AUTH_INVALID_KEY` | VK not recognised |
| 401 | `AUTH_KEY_DISABLED` | Admin disabled the VK |
| 401 | `AUTH_KEY_EXPIRED` | `expiresAt` passed |

---

## 3. Endpoint catalog

### Inference (proxied to upstream LLM)

| Method | Path | Ingress family | Body shape |
|---|---|---|---|
| POST | `/v1/chat/completions` | OpenAI Chat Completions | `{model, messages, …}` |
| POST | `/v1/responses` | OpenAI Responses API | `{model, input, …}` |
| POST | `/v1/messages` | Anthropic Messages | `{model, messages, max_tokens, …}` |
| POST | `/v1beta/models/{model}:generateContent` | Gemini generateContent | `{contents, generationConfig, …}` |
| POST | `/v1beta/models/{model}:streamGenerateContent` | Gemini generateContent (stream) | same as above |
| POST | `/v1/embeddings` | OpenAI embeddings | `{model, input, dimensions?, encoding_format?, …}` |
| POST | `/openai/deployments/{deployment}/chat/completions` | Azure OpenAI Chat | OpenAI shape |
| POST | `/openai/deployments/{deployment}/embeddings` | Azure OpenAI Embeddings | OpenAI shape |
| POST | `/api/paas/v4/chat/completions` | ZhipuAI GLM | OpenAI-compat shape |
| POST | `/api/paas/v4/embeddings` | ZhipuAI GLM embeddings | OpenAI-compat shape |

> **Embeddings — one ingress, many upstream shapes (E62).** `POST /v1/embeddings`
> is the **only** client-facing embedding endpoint. Cohere's `/v1/embed`,
> Gemini's `:embedContent`, and Gemini's `:batchEmbedContents` are **upstream**
> wire formats the gateway translates *to* on the way out — they are not
> registered as ingress routes. Send all embedding requests in OpenAI canonical
> shape (`{model, input, dimensions?, encoding_format?, …}`); cross-format
> routing to a Cohere or Gemini upstream is handled by the codec layer. Pass
> provider-specific knobs via `nexus.ext.<provider>.<key>` (see §17).

### Cost estimation (no upstream call)

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/estimate` | Multi-target cost compare (`compareTargets[]`). See [§7](#7-post-v1estimate--multi-target-compare). |
| POST | `/v1/*` with `nexus.dry_run: true` | In-band cost estimate via any inference endpoint. See [§6](#6-nexusdry_run--cost-estimate-without-an-upstream-call). |

### Read-only

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/models` | List models this VK can reach |
| GET | `/v1/models/{model}` | Detail for one model |
| GET | `/v1/usage` | Aggregate VK usage (tokens + USD) for the current period |
| GET | `/v1/usage/daily` | Daily VK usage time series |

---

## 4. Calling the same model with different SDK shapes

The headline feature: any (provider, model) the admin registered can be
reached from **any** of the 4 chat ingresses. The gateway canonicalizes
your request into the OpenAI Chat Completions shape, routes it to the
admin-configured target, and reshapes the upstream response back into
your ingress's native success shape — so your SDK never sees a wire
format it didn't expect.

### Example — call Claude via the OpenAI SDK

```python
from openai import OpenAI

# Point your OpenAI SDK at Nexus
client = OpenAI(
    base_url="https://api.example.com/v1",
    api_key="nvk_…",
)

resp = client.chat.completions.create(
    model="claude-sonnet-4-6",   # an Anthropic model
    messages=[{"role": "user", "content": "Hello"}],
)
# resp is a normal OpenAI ChatCompletion object — Nexus reshaped the
# Anthropic response on the way out. The actual upstream provider is
# in the x-nexus-aigw-model response header.
```

### Example — call GPT-5 via the Anthropic SDK

```python
from anthropic import Anthropic

client = Anthropic(
    base_url="https://api.example.com",
    api_key="nvk_…",   # carried via x-api-key by the SDK
)

resp = client.messages.create(
    model="gpt-5.5",   # an OpenAI model
    max_tokens=500,
    messages=[{"role": "user", "content": "Hello"}],
)
# resp is a normal Anthropic Message object — Nexus reshaped the
# OpenAI response back into Anthropic shape.
```

### How shape translation works (3 layers)

```
L1: Upstream native body          ← whatever the provider returned
    ↓ spec_*.DecodeResponse  +  ProjectToOpenAIChatCompletion
L2: Internal canonical pivot      ← always OpenAI chat-completion shape
                                    (used for cache key, hooks, audit, analytics)
    ↓ canonicalbridge.ResponseCanonicalToIngress
L3: Client response               ← always the client's INGRESS shape
                                    (your SDK never sees a wire format it didn't expect)
```

So **L3 differs by ingress** — `/v1/chat/completions` returns OpenAI shape,
`/v1/messages` returns Anthropic shape, `/v1beta/…` returns Gemini shape,
`/v1/responses` returns OpenAI Responses shape — regardless of which
upstream actually served the request.

### What survives end-to-end (L1 → L3)

| Surface | OpenAI chat ingress | OpenAI Responses ingress | Anthropic messages ingress | Gemini generateContent ingress |
|---|---|---|---|---|
| **Text content** | `choices[0].message.content` | `output[type=message].content[type=output_text].text` | `content[type=text].text` | `candidates[0].content.parts[].text` |
| **Tool calls** | `choices[0].message.tool_calls[]` | `output[type=function_call]` | `content[type=tool_use]` | `candidates[0].content.parts[].functionCall` |
| **Reasoning text** | `choices[0].message.reasoning_content` | `output[type=reasoning].summary[].text` | `content[type=thinking].thinking` | `candidates[0].content.parts[].{text, thought:true}` |
| **Reasoning token count** | `usage.completion_tokens_details.reasoning_tokens` | `usage.output_tokens_details.reasoning_tokens` | (Anthropic has no native count field) | `usageMetadata.thoughtsTokenCount` |
| **Cache-hit input tokens** | `usage.prompt_tokens_details.cached_tokens` | `usage.input_tokens_details.cached_tokens` | `usage.cache_read_input_tokens` | `usageMetadata.cachedContentTokenCount` |
| **finish_reason** | `choices[0].finish_reason` | `output[type=message].status` | `stop_reason` | `candidates[0].finishReason` |

Token counts are always normalised at L2 to the OpenAI canonical
convention (Anthropic's `input_tokens` is the **uncached** count on the
wire; Nexus's canonical PromptTokens is the **total** = uncached +
cache_read + cache_creation, then the L3 back-projection emits each
ingress's native field). So `usage.total_tokens` adds up the same way
regardless of which ingress you called.

### Cross-format reasoning preservation

When you call a non-OpenAI ingress and Nexus cross-routes to a model
that returns reasoning text (OpenAI o-series / gpt-5 / DeepSeek /
Moonshot / Kimi), the canonical `reasoning_content` is back-projected
into the ingress's native shape:

- `/v1/messages` receives `content[type=thinking].thinking` blocks
  prepended before text blocks (same ordering Anthropic uses when
  extended-thinking is enabled).
- `/v1beta/…:generateContent` receives `candidates[0].content.parts[]`
  with `{text, thought:true}` parts prepended before visible-text parts
  (same ordering Gemini 2.5+ uses when
  `thinkingConfig.includeThoughts` is set).

Same-format passthrough (Anthropic ingress → Anthropic upstream,
Gemini ingress → Gemini upstream) is also unaffected — upstream
thinking blocks survive verbatim through L1 forward + L3 back projection.

### What's NOT preserved (by design)

- **Anthropic citations** (cross-format only — preserved on native
  `/v1/messages`)
- **Gemini grounding metadata** (cross-format only — preserved on
  native `/v1beta/…`)
- **OpenAI Responses-API stateful fields** (`previous_response_id`,
  `store`) are preserved only when the routed target is also OpenAI
  Responses-API capable

For provider-specific knobs that don't have a clean OpenAI mapping,
use the `nexus.ext.<provider>.<key>` extension namespace (see [§5](#5-the-nexus-request-body-extension-namespace)).

### Forcing the body-format detection (rare)

By default Nexus detects the body format from the URL path. To override
the detection (e.g. you're hitting `/v1/chat/completions` with an
Anthropic-shape body in a test harness), set:

```
x-nexus-aigw-body-format: openai | anthropic | gemini | azure-openai | minimax | glm | deepseek | openai-responses | cohere | moonshot | bedrock | vertex
```

Accepted values are the `Format` constants in
`packages/ai-gateway/internal/providers/core/types.go` (full list there).
Unsupported values return 400.

---

## 5. The `nexus.*` request-body extension namespace

Nexus reserves two top-level JSON namespaces on every request body that
SDK clients can set:

### 5.1 `nexus.<flag>` — cross-provider behaviour controls

Reserved flags that apply regardless of which target wins routing. Today's
members:

| Field | Type | Default | Meaning |
|---|---|---|---|
| `nexus.dry_run` | `bool` | `false` | Run the request through routing + hooks + cache lookup but skip the upstream call; return an estimate. See [§6](#6-nexusdry_run--cost-estimate-without-an-upstream-call). |

The `nexus.*` namespace is preserved verbatim by every ingress's
canonicalisation step, so the same `nexus.dry_run: true` flag works in
an OpenAI body, an Anthropic body, or a Gemini body — your SDK doesn't
need to know which canonical form Nexus uses internally.

### 5.2 `nexus.ext.<provider>.<key>` — provider-scoped extensions

Carries fields that have no clean OpenAI mapping but are meaningful to a
specific provider. Nexus preserves these losslessly through cross-format
routing so a request authored in OpenAI shape can still convey
provider-specific knobs to the upstream target.

| Field | Provider | Purpose |
|---|---|---|
| `nexus.ext.anthropic.thinking` | Anthropic | Extended-thinking config. Object shape: `{type: "enabled", budget_tokens: 8000}`. |
| `nexus.ext.anthropic.cache_creation_input_tokens` | Anthropic | Round-trip preservation of the write-side cache counter from a previous response. |
| `nexus.ext.gemini.thinkingConfig` | Gemini | Gemini 2.5+ thinking config: `{includeThoughts: true, thinkingBudget: 8000}`. |
| `nexus.ext.openai.responses.previous_response_id` | OpenAI Responses | Statefulness anchor. |
| `nexus.ext.openai.responses.store` | OpenAI Responses | Per-request store opt-out. |
| `nexus.ext.cohere.input_type` | Cohere Embed | One of `search_document` / `search_query` / `classification` / `clustering`. **Required** for Cohere v3 embedding models — set via Cohere SDK natively, or via this extension when routing from a non-Cohere ingress (E62). |
| `nexus.ext.cohere.embedding_types` | Cohere Embed | Override the OpenAI canonical `encoding_format` with one or more Cohere-specific types (`["float","int8","uint8","binary","ubinary"]`). |
| `nexus.ext.cohere.truncate` | Cohere Embed | Truncation policy: `NONE` / `START` / `END` (default `END`). |
| `nexus.ext.gemini.taskType` | Gemini Embed | One of `RETRIEVAL_QUERY` / `RETRIEVAL_DOCUMENT` / `SEMANTIC_SIMILARITY` / `CLASSIFICATION` / `CLUSTERING` / `QUESTION_ANSWERING` / `FACT_VERIFICATION`. Defaults to `RETRIEVAL_QUERY` for the gateway-canonical embedding hub (E62). |
| `nexus.ext.gemini.title` | Gemini Embed | Used when `taskType=RETRIEVAL_DOCUMENT` to associate a title with the document being embedded. |

### Example — request Anthropic extended-thinking via the OpenAI SDK

```python
client.chat.completions.create(
    model="claude-sonnet-4-6",
    messages=[{"role": "user", "content": "Plan a 3-step research project."}],
    extra_body={
        "nexus": {
            "ext": {
                "anthropic": {
                    "thinking": {"type": "enabled", "budget_tokens": 8000}
                }
            }
        }
    },
)
```

If an adapter sees an `nexus.ext.<provider>.<key>` field for which it
has no mapping, it emits a one-shot WARN to operator logs and drops the
field (it does not reject the request). Use the OpenAPI spec for each
ingress (`docs/users/api/openapi/`) to see what's currently supported.

---

## 6. `nexus.dry_run` — cost estimate without an upstream call

Set `nexus.dry_run: true` on any inference request and Nexus will:

1. Run VK auth + per-VK dry-run rate-limit (separate bucket from real
   calls).
2. Run **request-stage classification hooks** (PII scan, toxicity) but
   skip modification hooks.
3. Route + canonicalize as if real (so the resolved model + pricing is
   loaded).
4. **Skip the upstream call.**
5. Return a normal-shape response with empty content + populated
   `usage`, plus an `x-nexus-estimate` header carrying the full
   low/expected/high cost breakdown.

The dry-run **does NOT** consume your cost budget. It is observable in
the Traffic dashboard with an `is_dry_run = true` discriminator, hidden
from the default list view (operator opts in via "Show dry-runs"
toggle).

### Example — OpenAI chat completions

```bash
curl -X POST https://api.example.com/v1/chat/completions \
  -H "Authorization: Bearer nvk_…" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5.5",
    "messages": [{"role":"user","content":"Explain quantum entanglement in two paragraphs."}],
    "reasoning_effort": "high",
    "nexus": {"dry_run": true}
  }'
```

### Response (OpenAI shape, content empty, usage populated)

```json
{
  "id": "dryrun-1747408532000000000",
  "object": "chat.completion",
  "created": 1747408532,
  "model": "gpt-5.5",
  "choices": [{
    "index": 0,
    "message": {"role": "assistant", "content": ""},
    "finish_reason": "dry_run"
  }],
  "usage": {
    "prompt_tokens": 12,
    "completion_tokens": 800,
    "total_tokens": 812,
    "prompt_tokens_details": {"cached_tokens": 0},
    "completion_tokens_details": {"reasoning_tokens": 600}
  }
}
```

### Response header (compact JSON, full breakdown)

```
x-nexus-dry-run: true
x-nexus-aigw-model: gpt-5.5
x-nexus-estimate: {"resolved":{"provider":"openai","model":"gpt-5.5"},"tokens":{...},"cost":{"currency":"USD","low":{...},"expected":{"total":0.018,...},"high":{...}},"cacheBenefit":{...},"assumptions":[...]}
```

> **Where the cost breakdown lives.** The dry-run response **body** uses
> each ingress's NATIVE success shape (empty `choices` for OpenAI Chat,
> empty `content` for Anthropic, empty `candidates` for Gemini, empty
> `output` for OpenAI Responses) — there is no consistent place across
> all four shapes to embed Nexus-specific `cost.*` / `dry_run` fields,
> so they ALL live in the `x-nexus-estimate` response header instead.
> SDK consumers expecting `nexus.dry_run` / `nexus.cost.*` JSON keys
> in the response body will see `null` and should switch to parsing
> the header.
>
> **`assumptions` is truncated to the first 5 entries** (plus a
> `"+N more (see request logs)"` marker) so the serialized header
> stays under nginx's default 8 KB header limit. Cap enforced in
> `packages/ai-gateway/internal/ingress/proxy/dry_run.go:462-469`.
> Full unbounded list is written to the request log instead.

### Streaming dry-run

`stream: true` + `nexus.dry_run: true` is supported. Each ingress emits
its native terminal frame:

- **OpenAI**: single `chat.completion.chunk` with `choices:[]` + usage + `[DONE]`
- **Anthropic**: `message_start` + `message_delta` + `message_stop`
- **Gemini**: single `{candidates:[], usageMetadata:{…}}` line

### Cache HIT case

If a real response for this exact request body is in Nexus's response
cache, the dry-run returns `cacheHit: true` in the `x-nexus-estimate`
header with cost = 0. This is the architectural invariant: a cache HIT
is reported as such, regardless of dry-run mode.

Full reference: [`docs/users/features/flows/dry-run.md`](../features/flows/dry-run.md).

---

## 7. `POST /v1/estimate` — multi-target compare

Send one request body + a list of up to 10 candidate `(provider, model)`
targets; receive per-target estimates and a summary in one round-trip.
Useful for "pick the cheapest model that meets my quality bar" UX.

### Request

```json
POST /v1/estimate
Authorization: Bearer nvk_…
Content-Type: application/json

{
  "request": {
    "model": "gpt-4o",
    "messages": [{"role":"user","content":"Summarise the attached PDF in 3 bullets."}],
    "max_tokens": 500,
    "reasoning_effort": "medium"
  },
  "compareTargets": [
    {"providerId": "openai-direct",    "modelId": "gpt-4o"},
    {"providerId": "openai-direct",    "modelId": "gpt-5.5",             "reasoningEffort": "high"},
    {"providerId": "anthropic-direct", "modelId": "claude-sonnet-4-6"}
  ]
}
```

### Response (abridged)

```json
{
  "targets": [
    {
      "providerId": "...", "providerName": "openai", "modelCode": "gpt-4o",
      "tokens":  {"uncachedInput": 312, "output": {"low": 100, "expected": 350, "high": 1500}},
      "cost":    {"currency": "USD", "expected": {"total": 0.00428, "...": "..."}},
      "assumptions": ["openai token count is a character-ratio heuristic..."]
    },
    {
      "providerId": "...", "providerName": "openai", "modelCode": "gpt-5.5",
      "error": {"code": "vk_model_not_allowed", "message": "VK 'vk-prod-abc' allowedModels does not include 'gpt-5.5'"}
    },
    { "providerId": "...", "providerName": "anthropic", "modelCode": "claude-sonnet-4-6", "tokens": {...}, "cost": {...} }
  ],
  "summary": {
    "cheapestExpectedTarget": "gpt-4o",
    "cheapestExpectedTotalUsd": 0.00428,
    "mostExpensiveExpectedTotalUsd": 0.006636,
    "errorsCount": 1,
    "successCount": 2
  }
}
```

### Notable contract

- **Per-target VK allowlist enforcement**: targets the VK can't access
  return a per-target `vk_model_not_allowed` error rather than failing
  the whole request with 403.
- **No traffic_event row** is written for the compare endpoint's inner
  estimates (it's meta-analysis, not real or simulated traffic).
- **No quota deduction**.
- **Parallel dispatch** — up to 8 targets dispatch concurrently.
- **Dedicated rate limit** — default 30 RPM/VK, configurable per VK as
  `compareEndpointRateLimitRpm`.

Full reference: [`docs/users/features/flows/v1-estimate-compare.md`](../features/flows/v1-estimate-compare.md).

---

## 8. Streaming

`stream: true` works on every chat ingress. Cross-format streaming is
supported — the gateway transcodes upstream frames into the client's
native SSE shape as they arrive.

| Ingress | Frame format | Terminator |
|---|---|---|
| `/v1/chat/completions` | OpenAI `data: {…}` JSON per chunk | `data: [DONE]` |
| `/v1/responses` | OpenAI Responses event stream | `event: response.completed` |
| `/v1/messages` | Anthropic event stream (`event: message_start`, `content_block_delta`, `message_stop`) | `event: message_stop` |
| `/v1beta/models/{model}:streamGenerateContent` | Gemini SSE | last chunk has `finishReason` |

The stream may carry per-chunk `usage` totals (OpenAI / Gemini) or only
a final `message_delta` usage frame (Anthropic). Nexus stamps
`x-nexus-cache: HIT/MISS/SKIP_STREAM` as a response header before
the first frame; cached stream replays are byte-equivalent to the
original MISS-time recording.

---

## 9. Caching

Nexus operates **two** independent cache layers, both transparent to
clients:

### 9.1 Gateway Cache (response-level, exact-match dedup)

When two requests with identical canonical bodies arrive within the TTL
window, Nexus serves the second from cache. Cache key includes:
provider, model, normalised body (volatile fields like the billing
nonce are stripped). Strict 1-1 match — no semantic similarity.

| Header value | Meaning |
|---|---|
| `x-nexus-cache: MISS` | Cache miss; upstream was called |
| `x-nexus-cache: HIT` | Served from gateway cache (cost = 0) |
| `x-nexus-cache: HIT_LIVE` | Joined an in-flight upstream call via the request-coalescing broker |
| `x-nexus-cache: DISABLED` | Cache is disabled for this provider |
| `x-nexus-cache: SKIP_NO_CACHE` | Caller sent `x-nexus-aigw-no-cache: 1` |
| `x-nexus-cache: SKIP_STREAM` | Streaming request with no cached stream available |

To opt out of cache for a single request:

```
x-nexus-aigw-no-cache: 1
```

### 9.2 Provider Prompt Cache (Anthropic prompt cache + Gemini cachedContent)

When a provider supports a long-prefix cache (Anthropic prompt cache,
Bedrock-Claude, Gemini cachedContent), Nexus can inject the required
cache markers on outbound bodies — `cache_control: {type: "ephemeral"}`
on Anthropic/Bedrock content blocks, `cachedContent` references on
Gemini. The cache discount is then applied by the provider (reflected
in `usage.prompt_tokens_details.cached_tokens` for OpenAI-shape
responses, `usage.cache_read_input_tokens` for native Anthropic,
`usageMetadata.cachedContentTokenCount` for native Gemini).

Two important caveats — injection is **not** unconditional:

- **Opt-in per provider.** The gateway only injects when the
  per-provider toggle is enabled via admin config
  (`providerInjectEnabled` in the cache settings;
  `packages/shared/transport/wirerewrite/engine.go:252`). Out of the
  box no provider is forced into injection.
- **Caller intent wins.** If the request body already carries any
  `cache_control` field (root or block level), the injection rule
  short-circuits and forwards the body unchanged
  (`packages/shared/transport/wirerewrite/rule_cache_inject.go:32-44`).
  This lets advanced callers pin their own cache boundaries.

The two layers compose: a cache MISS on the Gateway Cache can still
hit the Provider Prompt Cache on the upstream call (when injection is
enabled, or when the caller stamped its own markers).

---

## 10. Reasoning / extended-thinking

Nexus normalises reasoning/thinking surfaces across providers.

### Request-side knobs (use whatever your model supports)

| Provider | Native field | Cross-format via Nexus |
|---|---|---|
| OpenAI o-series + gpt-5 | `reasoning_effort: "low" | "medium" | "high"` | as-is on `/v1/chat/completions` or `/v1/responses` |
| Anthropic extended-thinking | `thinking: {type:"enabled", budget_tokens: N}` | `nexus.ext.anthropic.thinking` from any ingress |
| Gemini 2.5+ | `generationConfig.thinkingConfig: {includeThoughts: true, thinkingBudget: N}` | `nexus.ext.gemini.thinkingConfig` from any ingress |

### Response-side surface (per ingress, NOT a single canonical field)

Each ingress preserves its SDK's native contract — the field carrying
the reasoning text differs by which `/v1/*` you called, not by which
upstream actually served the request:

| Client called | Reasoning text field | Reasoning token count |
|---|---|---|
| `/v1/chat/completions` | `choices[0].message.reasoning_content` (string) | `usage.completion_tokens_details.reasoning_tokens` |
| `/v1/responses` | `output[type=reasoning].summary[type=summary_text].text` | `usage.output_tokens_details.reasoning_tokens` |
| `/v1/messages` | `content[type=thinking].thinking` (string) — prepended before text blocks | (Anthropic has no native count field) |
| `/v1beta/…:generateContent` | `candidates[0].content.parts[].{text, thought:true}` — prepended before visible-text parts | `usageMetadata.thoughtsTokenCount` |

Behind the scenes Nexus does carry a single canonical pivot
(`choices[0].message.reasoning_content` at the L2 internal layer — see
[§4](#4-calling-the-same-model-with-different-sdk-shapes) "3 layers"),
but client-facing responses are reshape to the ingress's native
contract before they leave the gateway. Your Anthropic SDK never sees
an OpenAI-shape body, your OpenAI SDK never sees an Anthropic-shape
body, etc.

### How upstream providers surface reasoning

Sources Nexus can extract reasoning from (token-count aliases live in
`packages/ai-gateway/internal/providers/specutil/usage.go:57-63`):

- OpenAI o-series + gpt-5 — `message.reasoning_content` on chat
  (`packages/ai-gateway/internal/providers/specs/openai/stream/stream.go:99-103`),
  or `output[type=reasoning]` on Responses
  (`packages/ai-gateway/internal/providers/specs/openai/responses/codec_responses_response.go:49,147`).
- Anthropic — `thinking` content blocks
  (`packages/ai-gateway/internal/providers/specs/anthropic/codec/codec.go:258`
  and `…/anthropic/ingress/hub_ingress.go:154-163,346-358`).
- Gemini — `thought=true` parts + `thoughtsTokenCount`
  (`packages/ai-gateway/internal/providers/specs/gemini/ingress/hub_ingress.go:355-358`).
- DeepSeek-reasoner + Moonshot kimi-k2-thinking — share OpenAI's
  `completion_tokens_details.reasoning_tokens` alias path
  (`…/specutil/usage.go:61`); text rides as `reasoning_content`.
- Cohere — `delta.reasoning_content` / `message.reasoning_content`
  (`packages/ai-gateway/internal/providers/specs/cohere/codec.go:101-103`).

---

## 11. Response headers

| Header | When | Meaning |
|---|---|---|
| `x-nexus-cache` | always | Cache status — see [§9.1](#91-gateway-cache-response-level-exact-match-dedup) |
| `x-nexus-aigw-model` | always | The model code Nexus actually routed to (may differ from the requested `model` if routing rules rewrote it) |
| `x-nexus-coerced` | when adapter renamed fields | `<from>→<to>` list, e.g. `max_tokens→max_completion_tokens` for o-series |
| `x-nexus-aigw-hook` | when a compliance hook ran | `ALLOW`, `MODIFY`, `BLOCK_SOFT`, `REJECT_HARD` |
| `x-nexus-dry-run` | only on dry-run responses | `true` |
| `x-nexus-estimate` | only on dry-run / estimate responses | Compact JSON with `resolved.{provider,model}`, `tokens.{input,output,reasoning}`, `cost.{low,expected,high}.total` (+ uncachedInput/cacheRead/cacheWrite/output components), `cacheBenefit`, `assumptions[]`. **This is the only place cost/breakdown lives on a dry-run response** — the response body uses the ingress's native success shape and does NOT carry `nexus.cost.*` fields. Cap on size: assumptions truncated to 5 entries to stay under nginx 8 KB header limit. |
| `x-nexus-upgraded-to` | when E57 auto-upgrade fired | `responses-api` (the original Chat Completions request was auto-routed to Responses API for state continuity) |
| `X-RateLimit-Limit` | when VK has rate limit | Numeric RPM cap |
| `Retry-After` | only on 429 | Seconds to wait before retry |
| `x-nexus-request-id` | always | Server-assigned request UUID (echoes `x-nexus-request-id` request header when supplied) |

Plus an allowlisted passthrough of upstream provider headers:
`anthropic-ratelimit-*`, `openai-organization`, `openai-version`,
`retry-after`, `x-request-id`, `via`, etc. Sensitive upstream headers
(auth tokens, internal trace IDs) are stripped.

---

## 12. Request headers Nexus reads

In addition to the auth carriers in [§2](#2-authentication--virtual-keys):

| Header | Purpose |
|---|---|
| `x-nexus-aigw-no-cache: 1` | Bypass the Gateway Cache for this request only |
| `x-nexus-aigw-body-format: <fmt>` | Override body-format auto-detection. Accepts any `Format` const from `packages/ai-gateway/internal/providers/core/types.go`: `openai`, `openai-responses`, `anthropic`, `gemini`, `azure-openai`, `minimax`, `glm`, `deepseek`, `cohere`, `moonshot`, `bedrock`, `vertex`, etc. |
| `x-nexus-request-id: <uuid>` | Client-supplied request ID (echoed back on the response). Generated server-side if absent. |
| `x-nexus-trace-id: <uuid>` | Cross-service trace ID for correlating Nexus internal spans. Falls back to `x-nexus-request-id` if absent. |
| `x-request-id: <id>` | External request ID (e.g. from your application's middleware). Forwarded to upstream and stored on the traffic_event row. |

Per-format provider beta headers (`anthropic-beta`,
`openai-beta`, `x-goog-user-project`, `anthropic-version`) are
forwarded to upstream verbatim — the spec adapter's allowlist
controls which actually reach the wire.

---

## 13. Rate limits + quota

Three independent buckets, all per-VK, all configurable by an admin:

| Bucket | Default | Configured by |
|---|---|---|
| Real-call RPM | unlimited (NULL on the VK row → no limit applied) | `rateLimitRpm` |
| Dry-run RPM | 60/min | `dryRunRateLimitRpm` |
| `/v1/estimate` RPM | 30/min | `compareEndpointRateLimitRpm` |

`VirtualKey.rateLimitRpm` is a nullable `Int?` on the schema
(`tools/db-migrate/schema.prisma:356`); when NULL the AI Gateway
short-circuits before invoking the limiter
(`packages/ai-gateway/internal/ingress/proxy/proxy.go:1401`), so an
unset value means "no real-call cap". Set a non-NULL positive integer
to enforce a per-VK RPM ceiling.

The buckets are independent so estimate flood cannot exhaust the
real-call rate, and vice versa.

**Cost budget** (USD-denominated) is checked on every real call. Dry-run
+ estimate-compare requests do NOT consume the cost budget.

429 response with `Retry-After` header on exhaustion. The `error.code`
distinguishes the bucket:
- `RATE_LIMITED` — real-call bucket
- `DRY_RUN_RATE_LIMITED` — dry-run bucket
- `estimate_compare_rate_limited` — `/v1/estimate` bucket
- `QUOTA_EXCEEDED` — cost budget

---

## 14. Errors — envelopes by ingress

Nexus reshapes errors into the client's native error envelope so your
SDK's error parser works without changes.

### OpenAI Chat Completions / Responses / Azure / GLM / DeepSeek / Moonshot

```json
{
  "error": {
    "message": "...",
    "type": "invalid_request_error | authentication_error | rate_limit_error | api_error",
    "code": "..."
  }
}
```

### Anthropic Messages

```json
{
  "type": "error",
  "error": {
    "type": "invalid_request_error | authentication_error | overloaded_error | api_error",
    "message": "..."
  }
}
```

### Gemini generateContent

```json
{
  "error": {
    "code": 400,
    "message": "...",
    "status": "INVALID_ARGUMENT | UNAUTHENTICATED | RESOURCE_EXHAUSTED | INTERNAL"
  }
}
```

Common Nexus-specific error codes (read from `error.code`):

| Code | Meaning |
|---|---|
| `AUTH_KEY_MISSING` / `AUTH_INVALID_KEY` / `AUTH_KEY_DISABLED` / `AUTH_KEY_EXPIRED` | VK auth failure |
| `RATE_LIMITED` / `DRY_RUN_RATE_LIMITED` | Per-VK RPM cap |
| `QUOTA_EXCEEDED` | Cost-budget cap |
| `ROUTING_NO_MATCH` | No routing rule matched (no admin-wired target) |
| `PROVIDER_RATE_LIMITED` | Upstream 429 |
| `PROVIDER_UNAVAILABLE` | All upstream targets failed |
| `PROVIDER_ERROR` | Generic upstream 4xx/5xx (the upstream's body is included in the response payload) |
| `PAYLOAD_TOO_LARGE` | Request body exceeds gateway max |
| `no_compatible_provider` | **E62** — embedding request asks for a capability (dimensions / batch size / encoding) that no routable target supports. The error envelope carries an `available_capabilities` array enumerating the considered targets and what each would have accepted, so admins can self-debug capability mismatches. See [§17 Embeddings — capability mismatch](#17-embeddings--cross-format-routing--capability-mismatch). |

### E62 — `no_compatible_provider` envelope (OpenAI ingress example)

When the gateway rejects an embedding request at the capability pre-filter, the error includes `available_capabilities`:

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "no_compatible_provider",
    "message": "No routing target supports dimensions=2048 for embedding requests",
    "param": "dimensions",
    "available_capabilities": [
      { "provider": "openai", "model": "text-embedding-3-small", "supported_dimensions": [512, 1024, 1536] },
      { "provider": "openai", "model": "text-embedding-3-large", "supported_dimensions": [256, 1024, 3072] },
      { "provider": "cohere", "model": "embed-english-v3",       "supported_dimensions": [1024], "required_extensions": ["nexus.ext.cohere.input_type"] }
    ]
  }
}
```

`available_capabilities` is empty / absent for non-capability-mismatch errors. The same envelope shape applies on Azure ingress; Cohere ingress wraps in `{message, code, available_capabilities}`; Gemini ingress nests `available_capabilities` under `error.details[0]`.

---

## 15. Read endpoints: `/v1/models`, `/v1/usage`

### `GET /v1/models`

Returns the list of model codes the presented VK can reach. Mirrors the
OpenAI `/v1/models` response shape so OpenAI SDK `client.models.list()`
works unchanged.

```json
{
  "object": "list",
  "data": [
    {"id": "gpt-5.5", "object": "model", "owned_by": "openai"},
    {"id": "claude-sonnet-4-6", "object": "model", "owned_by": "anthropic"},
    {"id": "gemini-2.5-pro", "object": "model", "owned_by": "google"}
  ]
}
```

### `GET /v1/models/{model}`

Detail for one model. Includes the 4 pricing fields
(`inputPricePerMillion`, `outputPricePerMillion`,
`cachedInputReadPricePerMillion`, `cachedInputWritePricePerMillion`)
and max-token limits — useful for client-side cost estimation when you
don't want to round-trip through `/v1/estimate`.

### `GET /v1/usage`

Aggregated current-period usage for the presented VK:

```json
{
  "periodStart": "2026-05-01T00:00:00Z",
  "periodEnd":   "2026-06-01T00:00:00Z",
  "totalRequests": 12345,
  "totalPromptTokens": 1234567,
  "totalCompletionTokens": 234567,
  "totalReasoningTokens": 12000,
  "totalCostUsd": 123.45,
  "budgetLimitUsd": 500.00,
  "budgetRemainingUsd": 376.55
}
```

### `GET /v1/usage/daily`

Per-day breakdown of the above for the trailing 30 days.

---

## 17. Embeddings — cross-format routing + capability mismatch (E62)

E62 makes embeddings fully cross-format: a request sent to the single
OpenAI-canonical ingress can route to any upstream the admin wired up
(OpenAI, Azure OpenAI, Cohere, Gemini, GLM) and the gateway translates
on the wire — exactly the way E28 enabled cross-format chat.

### Client-facing embedding ingresses

There is exactly **one** OpenAI-canonical client surface plus three
provider-native paths that share the same canonical pivot under the
hood:

| Ingress URL | Body shape | Notes |
|---|---|---|
| `POST /v1/embeddings` | `{model, input, dimensions?, encoding_format?, user?}` (OpenAI canonical) | The cross-format hub — use for OpenAI / Azure / Cohere / Gemini / GLM upstreams alike, with `nexus.ext.<provider>.<key>` for provider-specific knobs. |
| `POST /openai/deployments/{deployment}/embeddings?api-version=…` | OpenAI canonical | Azure SDK path style; same canonical body. |
| `POST /api/paas/v4/embeddings` | OpenAI canonical | ZhipuAI GLM SDK path style; same canonical body. |

Cohere's `/v1/embed` and Gemini's `:embedContent` / `:batchEmbedContents`
are **not** registered as ingress routes — they are the **upstream**
wire formats the gateway translates *to* on the way out. Send all
embedding requests in OpenAI canonical shape; carry Cohere or Gemini
specific knobs via `nexus.ext.<provider>.<key>` (table in §5.2).

### Cross-format routing — example (OpenAI SDK → Cohere upstream)

The admin wires a routing rule that pins `text-embedding-3-small` → Cohere's `embed-english-v3`. Your SDK code stays OpenAI-native; the gateway translates on the wire.

```python
from openai import OpenAI
client = OpenAI(base_url="https://api.example.com/v1", api_key="nvk_…")

# Cohere v3 models require an input_type; supply it via nexus.ext.cohere.*
resp = client.embeddings.create(
    model="text-embedding-3-small",
    input="What is Nexus?",
    extra_body={
        "nexus": {"ext": {"cohere": {"input_type": "search_query"}}}
    },
)
print(resp.data[0].embedding[:5])      # → OpenAI shape, but vectors are Cohere's
print(resp.usage.prompt_tokens)
```

When routing resolves to a Cohere v3 target (which requires `input_type`
on the upstream wire) and the request omits `nexus.ext.cohere.input_type`,
the capability pre-filter drops that candidate. If no other candidate
satisfies the capability set, the gateway returns `400 no_compatible_provider`
with the missing extension surfaced under `available_capabilities[].required_extensions`.

### Cross-format routing — example (Gemini upstream via the canonical ingress)

The Gemini-native `:embedContent` / `:batchEmbedContents` paths are
upstream wire formats only; clients hit the canonical `/v1/embeddings`
ingress and the codec translates to Gemini on the upstream side. Carry
Gemini-specific knobs via `nexus.ext.gemini.<key>`.

```python
from openai import OpenAI
client = OpenAI(base_url="https://api.example.com/v1", api_key="nvk_…")

# When the admin routes `text-embedding-004` to a Gemini upstream, supply
# Gemini-specific knobs through nexus.ext.gemini.*.
resp = client.embeddings.create(
    model="text-embedding-004",
    input="What is Nexus?",
    extra_body={
        "nexus": {"ext": {"gemini": {"taskType": "RETRIEVAL_DOCUMENT", "title": "Nexus FAQ"}}}
    },
)
print(resp.data[0].embedding[:5])      # → OpenAI shape, vectors are Gemini's
```

The codec maps canonical `input` (single string → Gemini `:embedContent`;
array → `:batchEmbedContents`) and re-shapes the upstream
`{embedding:{values:[…]}}` reply back into the OpenAI `{data:[{embedding:[…]}]}`
envelope the SDK expects.

### Capability fields gated by the pre-filter

The routing pre-filter rejects a candidate target when ANY of the following mismatch (per the Model's seeded `capabilityJson.embeddings` block):

| Canonical field | Failure mode |
|---|---|
| `dimensions` | Not present in `supported_dimensions` (e.g. `dimensions=2048` against Cohere fixed-1024 models) |
| input array length | Exceeds `max_batch_size` (e.g. 200-input batch against Cohere v3 with `max_batch_size=96`) |
| `encoding_format` | Not present in `supported_encoding_formats` (e.g. `"base64"` against a model that only does `"float"`) |
| `nexus.ext.<provider>.input_type` / `taskType` | Required by the provider model (Cohere v3) but absent |

If **every** candidate fails, the gateway emits `400 no_compatible_provider`. If at least one candidate passes, routing picks among the survivors per the strategy (priority / weighted / smart / etc.).

### Per-provider per-model rules (auto-applied)

The codec safety-net applies provider-specific wire rules transparently — these are stamped into the response header `x-nexus-coerced` so you can audit drift:

- **`text-embedding-ada-002`** — strips `dimensions` + `encoding_format` from the wire body (ada-002 returns 400 if either is set). Rewrite stamps: `dimensions→removed (ada-002: unsupported field)`.
- **Cohere v3 models** — require `input_type`. Set via `nexus.ext.cohere.input_type` from non-Cohere ingress.
- **Gemini `:embedContent` vs `:batchEmbedContents`** — chosen automatically by canonical `input` cardinality (single string → single endpoint; array → batch).

### Token-array input (legacy OpenAI feature)

The OpenAI canonical accepts `input` as `[]int` (single token sequence) or `[][]int` (batch of token sequences). When routing to a non-OpenAI target (Cohere, Gemini), the codec safety-net rejects with `400 invalid_request_error` (`reason: token_array_unsupported_by_cohere` / `…by_gemini`). Use string / `[]string` inputs for cross-format routing.

### Streaming embeddings — N/A

No supported upstream streams embeddings today. `stream=true` on `/v1/embeddings` is rejected with `400 invalid_request_error`. Reserved for a future epic if a provider ships streaming embeddings.

---

## 16. Migration cheatsheet

### From OpenAI SDK

```diff
- client = OpenAI()
+ client = OpenAI(base_url="https://api.example.com/v1", api_key="nvk_…")
```

Everything else stays the same. Use any model the admin wired up
(including non-OpenAI). Add `extra_body={"nexus": {…}}` for Nexus
extensions.

### From Anthropic SDK

```diff
- client = Anthropic()
+ client = Anthropic(base_url="https://api.example.com", api_key="nvk_…")
```

Use any model. Use `extra_body` (or the SDK's equivalent) to pass
`nexus.dry_run` / `nexus.ext.*`.

### From Gemini SDK

The Gemini SDK reads the API key from `GEMINI_API_KEY` env var by
default. Set it to your VK and point the SDK at Nexus's base URL:

```python
import google.genai as genai
client = genai.Client(api_key="nvk_…", api_endpoint="https://api.example.com")
```

### From cURL

Three minimum-viable examples:

```bash
# OpenAI shape
curl https://api.example.com/v1/chat/completions \
  -H "Authorization: Bearer nvk_…" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'

# Anthropic shape (note x-api-key)
curl https://api.example.com/v1/messages \
  -H "x-api-key: nvk_…" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}'

# Gemini shape (note ?key= + URL-encoded model)
curl "https://api.example.com/v1beta/models/gemini-2.5-pro:generateContent?key=nvk_…" \
  -H "Content-Type: application/json" \
  -d '{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}'
```

---

## Appendix — Where to find more

- **OpenAPI specs**: `docs/users/api/openapi/ai-gateway/ai-gateway-v1.yaml` (full surface) + per-feature specs (`e58-s3-dry-run.yaml`, `e58-s4-estimate-compare.yaml`, etc.)
- **Customer flow docs**: `docs/users/features/flows/dry-run.md`, `docs/users/features/flows/v1-estimate-compare.md`
- **Cost model**: `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md`
- **Provider adapter contract** (extension namespace rules): `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` § 3a
- **Caching architecture**: `docs/developers/architecture/services/ai-gateway/prompt-cache-architecture.md`, `docs/developers/architecture/services/ai-gateway/response-cache-architecture.md`
