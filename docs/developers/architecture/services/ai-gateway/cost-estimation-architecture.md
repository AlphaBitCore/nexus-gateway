# Cost estimation architecture

> Full cost-estimation architecture write-up is queued in the docs-backfill program. This page currently captures (1) the endpoint-type vocabulary the cost layer consumes and (2) the streaming mode × cost stamping interaction. Full cost-formula registry / metering / pricing pipeline follows in a later commit.
>
> Source-of-truth code: [`packages/ai-gateway/internal/cache/layer/pricing.go`](../../../../../packages/ai-gateway/internal/cache/layer/pricing.go), [`packages/ai-gateway/internal/execution/estimator/`](../../../../../packages/ai-gateway/internal/execution/estimator/).

## Endpoint-type vocabulary

Cost formulas are keyed by the canonical `typology.EndpointKind` string (`chat`, `embeddings`, `stt`, `tts`, `image_generation`, `batch`). The `audit.Record.EndpointType` field on a finalised traffic event carries this string verbatim, and the cost estimator looks it up against the registered formula registry by exact match.

The string is derived once per request at handler-dispatch time via `string(typology.KindFromWireShape(resolved.WireShape))` and flows unchanged into `audit.Record.EndpointType`, `TrafficEventMessage.EndpointType`, `traffic_event.endpoint_type`, and the AI Gateway Prometheus `endpoint` label — no translation hop, no per-consumer vocabulary. See [endpoint-typology-architecture.md §7](../../cross-cutting/foundation/endpoint-typology-architecture.md) for the shared path-segment helper that hooks + audit both delegate to.

**Empty `endpoint_type` — non-AI forwards (binding for readers).** Only the AI Gateway route-classifies, so only AI-Gateway rows carry a kind. The compliance-proxy and agent are transparent forwarders that do not classify the request; they leave `EndpointType` unset, which the Hub consumer persists as the empty string. `traffic_event.endpoint_type` is `TEXT NOT NULL DEFAULT ''`, so empty — never `NULL` — is the canonical "unclassified" value (the consumer field is a value-type `string` and the INSERT binds it verbatim via `stripNul`, so empty cannot raise a `NOT NULL` violation). Two consequences:

- The estimator's `Lookup` treats an unknown/empty kind as `chat` and emits a one-time WARN log naming the unknown endpoint (`sync.Map` dedup prevents log spam). This makes the silent fallback visible rather than continuing to silently misprice `stt`/`tts`/`image_generation` at chat rates.
- Any analytics, rollup, or UI that groups / filters / displays `traffic_event.endpoint_type` MUST treat `''` as "unclassified / non-AI", not assume every row has a kind. In particular `WHERE endpoint_type = 'chat'` silently excludes **all** compliance-proxy and agent traffic; a group-by yields an `''` bucket that should render as "Other / unclassified", not blank-or-error.

**`BillableUnits` fields (live production set).** `estimator.BillableUnits` carries `PromptTokens` and `CompletionTokens` only — the two fields all five production call sites actually set, and the only ones the two registered formulas price. Dead fields (`Images`, `AudioSeconds`, `VideoSeconds`, `Requests`, `CachedTokens`) have been removed. `ReasoningTokens` is folded into `CompletionTokens` at the provider-adapter layer (priced at output rate) and is not a separate `BillableUnits` field.

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

### Usage extraction on the shared normalize codecs

The usage extractor delegates to the shared normalize codecs (`packages/shared/transport/normalize/codecs`), so their stream-folding behavior is part of the cost path:

- **Anthropic SSE** is folded by the codec's stream state machine: input-side usage (including `cache_read_input_tokens` / `cache_creation_input_tokens`) comes from `message_start`, output-side from `message_delta`, merged into the canonical convention (`PromptTokens` = uncached + cache read + cache creation; `CompletionTokens` = output). Tool-use-only and thinking-only streams carry full usage like text streams.
- **Reasoning tokens**: a wire-explicit count (`output_tokens_details.thinking_tokens` on Anthropic; `completion_tokens_details.reasoning_tokens` on OpenAI-compatible wires) always wins; the character-based derivation is only a fallback when the wire omits the count. The OpenAI-compatible `reasoning` field is an accepted wire alias of `reasoning_content` and feeds the same reasoning text accounting.
- **`/v1/responses` native passthrough**: when a genuine Responses upstream streams verbatim on the non-enforced lane, the egress copier forwards each SSE frame byte-for-byte yet a usage tee still decodes the canonical `Usage` (Responses `input_tokens` / `output_tokens`) onto the same chunk, so cost and token stamping land unchanged — the raw-byte forwarding bypasses the re-encode, not the usage accounting. The two-signal egress model is described in [`provider-adapter-architecture.md`](./provider-adapter-architecture.md) (*Responses egress: two signals*).
 See [`sse-streaming-compliance-architecture.md`](../../cross-cutting/safety/sse-streaming-compliance-architecture.md) for the streaming dispatch contract.

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

## References

- `packages/ai-gateway/internal/execution/estimator/` — cost formula registry + heuristic tokenizer.
- `packages/ai-gateway/internal/ingress/proxy/proxy.go`, `packages/ai-gateway/internal/ingress/proxy/proxy_cache.go` — cost stamp sites and the embedding usage fallback.
- `packages/ai-gateway/internal/ingress/proxy/embedding_metadata.go` — `preStampEmbeddingRequestMeta`, `embeddingTokenFallback`.
- `packages/ai-gateway/internal/cache/layer/pricing.go` — usage extractor + pricing lookup.
- `packages/shared/transport/typology/endpointkind.go` — `EndpointKind` vocabulary.
- `packages/shared/transport/inputstaging/tokenize.go` — `EstimateTokens` heuristic.
