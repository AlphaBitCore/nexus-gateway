# Cost estimation architecture

> Full cost-estimation architecture write-up is queued in the docs-backfill program. This page currently captures (1) the endpoint-type vocabulary the cost layer consumes and (2) the streaming mode × cost stamping interaction. Full cost-formula registry / metering / pricing pipeline follows in a later commit.
>
> Source-of-truth code: [`packages/ai-gateway/internal/cache/layer/pricing.go`](../../../../../packages/ai-gateway/internal/cache/layer/pricing.go), [`packages/ai-gateway/internal/execution/estimator/`](../../../../../packages/ai-gateway/internal/execution/estimator/).

## Endpoint-type vocabulary

Cost formulas are keyed by the canonical `typology.EndpointKind` string. Eight kinds have registered formulas (`chat`, `embeddings`, `rerank`, `stt`, `tts`, `image_generation`, `video_generation`, `realtime`); kinds without one (e.g. `batch`) take the warned chat-formula fallback described below. The `audit.Record.EndpointType` field on a finalised traffic event carries this string verbatim, and the cost estimator looks it up against the registered formula registry by exact match.

The string is derived once per request — at handler-dispatch time via `string(typology.KindFromWireShape(resolved.WireShape))` for the wire-shape-dispatched routes, or stamped directly by the handler for kinds that don't ride wire-shape dispatch (the `responses` label override, `guardrail`, and the parallel-handler modalities whose non-submit routes carry no body) — and flows unchanged into `audit.Record.EndpointType`, `TrafficEventMessage.EndpointType`, `traffic_event.endpoint_type`, and the AI Gateway Prometheus `endpoint` label — no translation hop, no per-consumer vocabulary. See [endpoint-typology-architecture.md §7](../../cross-cutting/foundation/endpoint-typology-architecture.md) for the shared path-segment helper that hooks + audit both delegate to.

**Empty `endpoint_type` — non-AI forwards (binding for readers).** Only the AI Gateway route-classifies, so only AI-Gateway rows carry a kind. The compliance-proxy and agent are transparent forwarders that do not classify the request; they leave `EndpointType` unset, which the Hub consumer persists as the empty string. `traffic_event.endpoint_type` is `TEXT NOT NULL DEFAULT ''`, so empty — never `NULL` — is the canonical "unclassified" value (the consumer field is a value-type `string` and the INSERT binds it verbatim via `stripNul`, so empty cannot raise a `NOT NULL` violation). Two consequences:

- The estimator's `Lookup` treats an unknown/empty kind as `chat` and emits a one-time WARN log naming the unknown endpoint (`sync.Map` dedup prevents log spam). This makes the silent fallback visible rather than continuing to silently misprice still-unregistered kinds (e.g. `batch`) at chat rates. `stt` / `tts` / `image_generation` / `video_generation` / `rerank` / `realtime` no longer hit this fallback — each has a registered modality formula (see `BillableUnits` below).
- Any analytics, rollup, or UI that groups / filters / displays `traffic_event.endpoint_type` MUST treat `''` as "unclassified / non-AI", not assume every row has a kind. In particular `WHERE endpoint_type = 'chat'` silently excludes **all** compliance-proxy and agent traffic; a group-by yields an `''` bucket that should render as "Other / unclassified", not blank-or-error.

**`BillableUnits` fields (live production set).** `estimator.BillableUnits` carries the two token fields every chat/embeddings cost site stamps (`PromptTokens`, `CompletionTokens`) plus five modality fields consumed by their own registered formulas: `Images` (`image_generation`), `AudioSeconds` (`stt`, fractional), `InputChars` (`tts`), `SearchUnits` (`rerank`), and `VideoSeconds` (`video_generation`, fractional — seconds of generated video), plus four realtime token-split fields (`AudioInputTokens`, `AudioOutputTokens`, `CachedTextReadTokens`, `CachedAudioReadTokens` — see the realtime paragraph below). No field exists without a consuming formula (locked by `TestBillableUnits_EveryFieldConsumed`). `ReasoningTokens` is folded into `CompletionTokens` at the provider-adapter layer (priced at output rate) and is not a separate `BillableUnits` field. `Requests` / `CachedTokens` remain removed — no formula consumes them.

**Rerank cost formula (`rerank`).** `rerankCostFormula` is a single seam over two provider billing models. Cohere reports its authoritative billable unit in the canonical response (`meta.billed_units.search_units`; 1 query + up to 100 documents = 1 search unit); `stampModalityUnits` reads that field with a gjson re-parse and stamps `BillableUnits.SearchUnits`, priced as `SearchUnits × InputUsdPerM / 1e6` (Cohere $2.00 per 1,000 searches → `InputUsdPerM = 2000` → $0.002/search-unit). Voyage rerank prices by tokens instead: its codec stamps `usage.total_tokens` as `DecodeResult.Usage.PromptTokens` (NOT `TotalTokens` — the formula reads only prompt/completion tokens, so a bare `TotalTokens` would price rerank at $0), and the formula's token branch fires first, so Voyage flows through the token path automatically and a missing `search_units` is expected, not an error. The gateway does not reconstruct provider billing math (Cohere's per-100-document bucketing stays inside Cohere's reported count). `warnUnderivableModalityUnits` flags a rerank 2xx that produced neither a token count nor a search unit as a genuine $0 case.

**Modality pricing semantic.** A model's `InputUsdPerM` is *USD per million billable input units*, where the unit is the modality's own: tokens for token-usage models, images / characters / audio-seconds / video-seconds for per-unit-priced models (dall-e-3 standard at $0.04/image → `InputUsdPerM = 40000`; tts-1 at $15/1M chars → `15`; whisper-1 at $0.006/min → `100`; sora-2 at $0.10/s of generated video → `100000`). Per-size / per-quality / per-resolution tiers are separate catalog model entries (e.g. a dall-e-3 HD row, or sora-2-pro resolution aliases), not a pricing-schema extension. Inside each modality formula, provider-reported usage tokens win when present (authoritative for token-priced models such as gpt-image-1 — their `InputUsdPerM` means per-1M-tokens); the modality unit is the fallback, and zero units price to zero with the stamping site owning the underivable-units WARN.

**Realtime cost formula (`realtime`) — the one multi-rate exception to the one-unit-per-row semantic.** A single realtime response (one in-band `response.done`) bills six components simultaneously at different rates — uncached text in, cached text read, uncached audio in, cached audio read, text out, audio out — which the one-unit-per-model-row reinterpretation cannot express, so `metrics.ModelPrices` carries three additive audio-rate fields (`AudioInputUsdPerM`, `AudioOutputUsdPerM`, `CachedAudioInputReadUsdPerM`). For realtime rows, `BillableUnits.PromptTokens` = UNCACHED text input and `CompletionTokens` = text output (per-endpoint unit reinterpretation, same pattern as `AudioSeconds`/`InputChars`); the four split fields carry the rest from `response.done.usage`'s `input/output_token_details`. Rate fallbacks: nil cached-text rate → `InputUsdPerM`, nil cached-audio rate → `AudioInputUsdPerM` (the shipped "no discount" cache-column semantics); the PRIMARY audio rates have no fallback — nil-or-zero primary rates make the model not realtime-priced (the relay refuses the session at upgrade under an enforced cost quota; un-enforced VKs get $0-priced audio components plus a deduped WARN — a spend-visibility gap, never a quota bypass). Components map into `Cost` as `UncachedInput` (text-in + audio-in), `CacheRead` (cached text + cached audio), `Output` (text-out + audio-out). **Realtime price rows are a deploy dependency**: the reference seed ships no realtime model rows — an operator enabling realtime creates the model rows and sets all four primary rates per deployment (a realtime model without them fail-closes under a cost quota until priced, exactly like Veo).

**STT metering runs off the `ServeProxy` cost stage.** Speech-to-text is served by the parallel `ServeSTT` streaming-proxy handler (see [ingress-api.md](ingress-api.md) §2), which does NOT run `ServeProxy`'s `stampModalityUnits` cost stage. Instead the handler meters inline: provider usage tokens (when a transcribe model reports `usage.{prompt,completion}_tokens` / `{input,output}_tokens`) win via the shared `sttCostFormula`; `AudioSeconds` derives from the response top-level `duration` (verbose_json carries it) as the per-second-model fallback. When neither is present (a `json`/`text` caller against a duration-less model), the request prices $0 with the same deduped underivable-units WARN — the audio **byte-count is never priced as seconds** (that would misbill catastrophically). The model's `InputUsdPerM` comes from the same `Models` catalog the quota stage reads.

**A price row's unit MUST match what the model's leg stamps at runtime — this is an operator obligation, not a validated field.** Token-usage image models (**Gemini Nano Banana models on the cross-shape leg** — the codec always stamps `usageMetadata` tokens — and `gpt-image-1`) are priced per 1M tokens; per-image rates are only for usage-less models (`dall-e-*`). Configuring a token-usage model with a per-image rate misprices silently: tokens are present, so the token branch fires against the wrong unit and no underivable-units WARN ever triggers. The gateway smoke's usage/cost cross-check catches this at verification time; when configuring a new image model row, check whether the model reports usage before choosing the unit.

**Adding a new cost formula for a new endpoint kind.** Three coordinated changes: (1) add the `EndpointKind*` constant in `packages/shared/transport/typology/endpointkind.go`; (2) register the cost formula keyed by the canonical kind string in `packages/ai-gateway/internal/execution/estimator/cost_formula_registry.go`; (3) add the rule to `packages/shared/transport/typology/defaults.go` so `ClassifyPath` recognises the request path.

## Single canonical price source

There is exactly **one** price authority for every cost in the gateway: the
**Model table** (`Model.inputPricePerMillion` / `outputPricePerMillion` /
`cachedInputReadPricePerMillion` / `cachedInputWritePricePerMillion`). The
former `provider_pricing` table is retired; `cache/layer/pricing.go::LookupCachePricing`
assembles the cache-cost rates from the in-memory Model snapshot, and the quota
engine resolves the same rows via `store.GetModel` / `store.FetchModelPricing`.

The per-request cost is computed **once**, cache-aware (prompt-cache read/write
tokens decomposed at their own rates in `computeCacheCosts`), into
`rec.EstimatedCostUsd`. That single value is the one number that flows everywhere:

- persisted to `traffic_event.estimated_cost_usd`;
- summed by the Hub rollup into `billed_cost_usd` — a **passthrough** of
  `estimated_cost_usd` for success + non-cache rows, never re-priced (see
  [metrics-rollup-architecture.md](../../cross-cutting/observability/metrics-rollup-architecture.md));
- charged into the live quota counter by `QuotaEngine.Reconcile`
  (`ActualUsage.CostUSD = rec.EstimatedCostUsd`, **not** a second tokens × price
  recomputation) — see [quota-architecture.md §2a](../../cross-cutting/safety/quota-architecture.md#2a-single-canonical-price-source);
- re-seeded into the live counter on gateway boot by the quota Backfill, which
  reads `metric_rollup_1h.billed_cost_usd`.

Because enforcement, the persisted ledger, the rollup, and the boot seed all
read the same source and the same computed value, they cannot diverge for a
given model — including across a gateway reboot (audit F-0163).

## Cost stamp / per-record fields

The per-traffic-event cost is stamped onto `traffic_event` rows at audit-emit time and again on cache-hit short-circuits (cache HIT serves bypass the upstream call but still record a billable event at the cached cost). The five stamp sites in the AI Gateway hot path are documented inline in `packages/ai-gateway/internal/ingress/proxy/proxy.go` and `proxy_cache.go`.

### `reasoning_cost_usd` breakdown

`reasoning_cost_usd` is a breakdown subset of `EstimatedCostUsd` — the slice attributable to reasoning tokens, billed at the output rate (`reasoning_tokens × output price ÷ 1e6`). It is NOT an additional charge. Every cost-stamp path funnels through the shared `stampReasoningCost` helper (`proxy_cachecost.go`) so the field stays consistent with `reasoning_tokens` regardless of how the response was served: direct non-stream (`handleNonStream`), broker non-stream (`handleNonStreamWithSubscription`), streaming (`handleStreamWithSubscription`), and both cache-HIT sites (`handleNonStreamHit` / `handleStreamHit`). On a `hit_inflight` joiner the whole cost breakdown — including `reasoning_cost_usd` — is zeroed alongside `EstimatedCostUsd`, since the joiner paid no upstream cost (the leader owns the spend).

### Async video: single cost-bearing row + first-observer reconcile

Video generation (`/v1/videos`) is **async** — the cost is owed at submit but not known-final until the render completes, possibly hours later — so it deliberately breaks the "one request, one cost stamp" shape of every sync endpoint. The invariant (e88-s6 D-V6): **exactly one cost-bearing `traffic_event` row per job.**

- **Submit row** stamps `EstimatedCostUsd = requested seconds × the model's per-second price` (`InputUsdPerM` = USD per 1M seconds; e.g. sora-2 $0.10/s → `100000`). This is **estimate-as-floor** — an abandoned or failed job's cost of record stays this estimate (a rare, bounded, conservative *over*-count, never an under-count), so the rollup `SUM(estimated_cost_usd)` counts the job's real GPU spend even if it is never polled to completion. Per-resolution tiers (sora-2-pro, Veo) are **separate catalog model entries** (aliases), not a pricing-schema extension.
- **Poll / content / delete rows stamp $0** so the rollup counts each job exactly once (via the submit row).
- **Live quota** is reconciled by the FIRST poll that observes a terminal state: `QuotaEngine.Reconcile(ActualUsage{CostUSD: realized})` where `realized = the job row's requested seconds × price` — the SAME formula and price source as the submit estimate, and **never** a provider-reported usage figure (a gamed poll payload cannot reconcile below the estimate floor). An at-most-once `ReconcileOnce` row flag guards it; the price lookup runs BEFORE the claim so a transient lookup failure defers the whole reconcile to a later poll rather than consuming the claim. A `failed` render debits nothing; an unpriced-at-completion model skips the live debit with a WARN and the submit row remains the cost of record. Under an actively-enforced cost quota an **unpriced** routed model fails closed at submit (`503 QUOTA_MODEL_UNPRICED`, chat parity) — a $0 estimate on the job's single cost-bearing row would make seconds-of-GPU spend invisible to caps and rollups. **Veo price rows are a deploy dependency**: a Veo model without catalog pricing fail-closes under a cost quota until priced.
- **SDK poll loops** ride the VK RPM budget (each poll is a metered request); the render count itself is bounded by a separate non-terminal-jobs admission cap, not the cost path.

### Embedding usage fallback

Embedding cost is `prompt_tokens × input price` (embeddings have no completion tokens). Most providers report `prompt_tokens` in the response usage block and the estimator bills from that real count. Some providers return only the vector and no usage (Gemini `embedContent`), leaving `prompt_tokens` at zero. For those, the AI Gateway substitutes a request-side local token estimate so the cost formula still yields a non-zero embedding cost:

- At request time `preStampEmbeddingRequestMeta` (`packages/ai-gateway/internal/ingress/proxy/embedding_metadata.go`) counts the embedding `input` text(s) with `inputstaging.EstimateTokens` and stamps `metadata.embedding.estimated_prompt_tokens`.
- At cost-stamp time `embeddingTokenFallback` back-fills `rec.PromptTokens` from that estimate **only when** the endpoint is `embeddings` and the upstream reported zero usage. Real provider usage always wins.
- The fallback runs on every non-stream embeddings cost site — the live path (`handleNonStream`) and the broker-subscription path. There is no cache-HIT site for embeddings: the response cache is endpoint-scoped and the pre-lookup classifier short-circuits the embeddings endpoint with `gateway_cache_skip_reason = embeddings_endpoint` (F-0222), so embeddings always reach an upstream cost site.

The estimate is a heuristic count; for cheap providers with short inputs the resulting cost can be below the `estimated_cost_usd` column's six-decimal scale and round to zero, which is expected (a few embedding tokens cost sub-micro-dollar).

## Streaming mode × cost stamping interaction

ai-gateway's SSE handler dispatches between two streaming modes based on the admin policy in `*streampolicy.Store`:

| Mode | Cost-stamping point |
|---|---|
| `chunked_async` (live) | Usage / cost lands on `rec` per-checkpoint as deltas arrive; final usage from upstream's terminal frame seals the row. |
| `buffer_full_block` | Whole body buffers before the single hook checkpoint; final usage from the buffered terminal frame stamps `rec` once on the path back through replay. |

Both paths go through the same usage extractor and pricing lookup (`pricing.go`); the dispatch only changes *when* the rec fields land, not *what* they contain.

Two properties of the delivery path bound how much of a streaming request's budget goes on framing rather than on the cost stamp, both measured per SSE frame — i.e. roughly per token:

- `format.WriteTypedEvent` assembles each frame into a pooled buffer rather than going through `fmt`, costing ~39 ns per frame and allocating nothing itself; routing it through `fmt.Fprintf` plus `strings.Split` instead costs ~146 ns and three allocations. `packages/shared/transport/streaming`'s serializer uses the same technique; the two are separate because their callers pass different shapes.
- The connection write deadline resets once per frame rather than once per `Write`: writes inside a 1 ms window share a reset, putting a frame at ~285 ns against a ~98 ns floor with no deadline at all, where resetting on every `Write` costs ~470 ns. The deadline left in place is at most one window older than a strict per-`Write` reading, so a stalled stream can be cut up to 1 ms early out of an idle budget measured in seconds — early rather than late, so no stream that would have survived is cut.

The model catalog `GET /v1/models` serves is read from the same config snapshot the pricing lookup and the router use, not from the models table, so the catalog cannot advertise a model the router would reject or price differently. Reading the table directly costs one Postgres round trip and ~1,975 allocations per catalog request; the snapshot costs neither. That endpoint carries client-startup traffic rather than per-chat-turn traffic, so the saving is secondary to the consistency.

### Usage extraction on the shared normalize codecs

The usage extractor delegates to the shared normalize codecs (`packages/shared/transport/normalize/codecs`), so their stream-folding behavior is part of the cost path:

- **Anthropic SSE** is folded by the codec's stream state machine: input-side usage (including `cache_read_input_tokens` / `cache_creation_input_tokens`) comes from `message_start`, output-side from `message_delta`, merged into the canonical convention (`PromptTokens` = uncached + cache read + cache creation; `CompletionTokens` = output). Tool-use-only and thinking-only streams carry full usage like text streams.
- **Reasoning tokens**: a wire-explicit count (`output_tokens_details.thinking_tokens` on Anthropic; `completion_tokens_details.reasoning_tokens` on OpenAI-compatible wires) always wins; the character-based derivation is only a fallback when the wire omits the count. The OpenAI-compatible `reasoning` field is an accepted wire alias of `reasoning_content` and feeds the same reasoning text accounting.
- **`/v1/responses` native passthrough**: when a genuine Responses upstream streams verbatim on the non-enforced lane, the egress copier forwards each SSE frame byte-for-byte yet a usage tee still decodes the canonical `Usage` (Responses `input_tokens` / `output_tokens`) onto the same chunk, so cost and token stamping land unchanged — the raw-byte forwarding bypasses the re-encode, not the usage accounting. The two-signal egress model is described in [`provider-adapter-architecture.md`](./provider-adapter-architecture.md) (*Responses egress: two signals*).
 See [`sse-streaming-compliance-architecture.md`](../../cross-cutting/safety/sse-streaming-compliance-architecture.md) for the streaming dispatch contract.
- **Lenient whole-struct decode**: the codecs decode captured payloads field-by-field when the whole-struct unmarshal fails on a type mismatch (see *Decode leniency* in [`normalization-architecture.md`](./normalization-architecture.md)). For usage extraction this only ADDS coverage — a mistyped unrelated scalar no longer zeroes the usage block along with the rest of the payload; a well-typed `usage` object is read identically on both decode paths.

### Normalized projection is not on the cost path — cost/field neutral

The normalized projection (`request_normalized` / `response_normalized`) is
**not stamped on the audit write path**: no producer ships it, and the Control
Plane recomputes it at view time from the stored (already-redacted) body. This is
cost-neutral by construction — cost and token counts are extracted into dedicated
`traffic_event` columns (`estimated_cost_usd`, `prompt_tokens`,
`completion_tokens`, `reasoning_cost_usd`, …) from the response usage block, not
re-derived from the normalized projection. Because the row's cost/token/cache
fields do not depend on the projection, the absence of a write-path stamp is
field-neutral, and the audit path carries no normalize compute on the request
goroutine. The usage extractor and pricing path above are untouched.
See [`normalization-architecture.md`](normalization-architecture.md) §5.2 and
[`audit-pipeline-architecture.md`](../../cross-cutting/observability/audit-pipeline-architecture.md) §10.2.

## Token-estimate classes

The gateway keeps three char→token estimators, each tuned for its own question; do not collapse them or add a fourth:

- **Cost (this doc)** — `estimator.pickTokenizer` per-family divisors (≈3.5–4 chars/token). Wants the best *average-case* guess; the value is billed and reconciled against real usage post-call.
- **Quota pre-check** — `proxy.estimateTokens` (`bytes/3`). A cheap, marginally-high admission estimate, also reconciled post-call.
- **Fit / "will it fit the context window?"** — `inputstaging.EstimateTokensConservative`. An *upper bound* (non-ASCII weighted by UTF-8 byte length, the BPE byte-fallback ceiling), because under-counting admits a model the request overflows and hard-400s, while over-counting only picks a larger model. Used by smart routing, the router-LLM input budget, AI-Guard, and embedding staging.

## References

- `packages/ai-gateway/internal/execution/estimator/` — cost formula registry + heuristic tokenizer (average-case, cost class).
- `packages/ai-gateway/internal/ingress/proxy/proxy.go`, `packages/ai-gateway/internal/ingress/proxy/proxy_cache.go` — cost stamp sites and the embedding usage fallback.
- `packages/ai-gateway/internal/ingress/proxy/embedding_metadata.go` — `preStampEmbeddingRequestMeta`, `embeddingTokenFallback`.
- `packages/ai-gateway/internal/cache/layer/pricing.go` — usage extractor + pricing lookup.
- `packages/shared/transport/typology/endpointkind.go` — `EndpointKind` vocabulary.
- `packages/shared/transport/inputstaging/tokenize.go` — `EstimateTokens` (average-case) + `EstimateTokensConservative` (fit upper-bound).
