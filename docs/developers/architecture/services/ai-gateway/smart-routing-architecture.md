# Smart routing architecture

Smart routing is one of the routing engine's strategies (see [routing-architecture.md](routing-architecture.md)). What makes it distinct: instead of resolving a fixed target, it asks a **router LLM** to read the user's prompt and pick the best model from the catalog of routable models. It lives in `packages/ai-gateway/internal/routing/strategies/strategy_smart.go`, with the LLM-call half in `packages/ai-gateway/internal/routing/llm` and the catalog access in `packages/ai-gateway/internal/routing/core`.

## 1. When smart routing runs

A `smart` strategy node fires when an operator authors a rule whose strategy resolves to it — typically the rule matching `model: auto`. The strategy is registered only when its dependencies are wired (`cacheLayer` and the provider-target resolver both present); without them the smart strategy is absent and a rule referencing it resolves to no targets.

**Chat vs. non-chat endpoints.** The LLM task-router below runs only for chat/responses endpoints. On a non-chat endpoint (`model: auto` against `/v1/images/generations`, `/v1/audio/*`, `/v1/rerank`, …) `SmartStrategy.Evaluate` short-circuits to `modalityAutoTargets` (`strategy_smart_modality.go`): it enumerates that endpoint modality's enabled models via `ListEnabledCandidates(kind)` and returns them in a deterministic cheapest-first order — no LLM call. Token sizing, catalog-JSON prompting, and recency ordering are chat concepts that don't apply to an image or audio request, and asking a router LLM to "pick an image model" adds latency and cost for no signal. The modality guard (see [routing-architecture.md](routing-architecture.md) §4) still runs afterward, so even this path can never emit a cross-modality target.

The node carries a `SmartConfig`: the router provider and model, an optional system-prompt override, and tuning knobs. Unset knobs fall back to built-in defaults — temperature `0`, max tokens `1024`, timeout `3000`ms — plus an optional default provider/model used as the safety net.

**The router LLM must be a trusted provider.** Routing runs before the request-hook stage (hooks gate on the resolved target), so the router receives the customer's conversation content *before* any redaction hook has rewritten it — the same trust position as the AI-Guard judge, which classifies raw content by design. Operators must point `routerProviderId` at a provider trusted with unredacted traffic (self-hosted or same-trust-domain); a third-party router provider sees content that egress redaction rules would otherwise have masked.

## 2. The decision pipeline

`SmartStrategy.Evaluate` runs a sequential pipeline. Every failure step falls open to `smartFallback`, which resolves the configured default provider/model — or returns no targets when no default is set. The router LLM is never the single point of failure: a missing config, an empty catalog, an un-routable request, an unwired or erroring router, or an unresolvable selection all degrade gracefully to the default.

1. **Config check** — a node missing the router provider or model yields no targets.
2. **Candidate enumeration** — `ListEnabledChatModels` lists the routable models (this LLM-router pipeline runs for chat/responses only; non-chat `auto` took the deterministic `modalityAutoTargets` path in §1). When the virtual key carries an allowed-models list, candidates are filtered to it. An empty candidate set falls back.
3. **Context-window filter** — candidates whose declared `maxContextTokens` provably cannot hold the request are dropped before the catalog is built, so the router LLM never sees a model the request overflows. The required size is the estimated input tokens of the **full** canonical payload — text in every role plus tool-use input, tool-result output, and tool definitions; the router LLM is shown only a bounded slice of the recent conversation, so it cannot judge the true request size itself — plus an output reserve: the request's `maxTokens` when present, a built-in 1024 otherwise. Estimation uses `inputstaging.EstimateTokensConservative`, the shared upper-bound heuristic: because under-counting admits a model the request then overflows (a hard upstream 400) while over-counting only selects a larger-context model, the estimate is biased high. It anchors non-ASCII to UTF-8 byte length — a BPE tokenizer emits at most one token per byte under byte fallback, so a run of out-of-vocabulary characters (a real prompt of Chinese + symbol text was charged ~3.5 tokens per character) is bounded, where the average per-character heuristic under-counted it by an order of magnitude. Image blocks are counted for the metadata line but not token-estimated. Candidates without a declared `maxContextTokens` always pass (fail-open). When nothing fits, the largest-context candidates are kept instead of emptying the pool — the estimate is coarse and the largest model gives the request its best chance, whereas the configured default (typically a small model) would guarantee the overflow; the trace records the overflow risk. A capability hard filter runs in the same pass: a candidate that declares a feature list but lacks `vision` when the request carries image blocks, or `function_calling` when it declares tools, is dropped; undeclared feature lists pass, and a dimension that would empty the pool is skipped (both fail-open, trace-recorded). These filters are smart-strategy-local: no other routing strategy consults them.
4. **Catalog build** — the surviving candidates are rendered into a compact catalog JSON for the prompt. Separately, a scan over the raw (pre-VK-filter) rows — the router model routes traffic and need not be VK-allowed — resolves the router model's own declared `maxContextTokens`, which feeds the router-input budget (§6).
5. **Prompt assembly** — the system prompt is the operator override or the built-in `DefaultSystemPrompt`, with the `{modelCatalog}` placeholder substituted, plus an appended request-metadata line (`~N input tokens, X images, Y tool definitions`) so the router can match capability needs (vision, function calling, long context) it cannot see from the bounded text slice.
6. **Conversation projection** — the canonical request is projected to its user and assistant turns, in order. Client system messages are excluded (large, low-signal for routing; the metadata line carries the request shape) and tool-role plumbing is excluded the same way. A request that is nil or not an AI payload, or that carries no user turn, falls back — the router LLM is never called with empty or non-AI content.
7. **Decision** — the prepared prompt, the conversation, and the router model's context window go to the `Decider`; any error falls back, with the error text recorded in the trace.
8. **Selection resolution** — the router's returned token is mapped to an internal model UUID. An unknown selection falls back.
9. **Target lookup** — the selected provider/model is resolved into a `RoutingTarget`; a lookup failure falls back.
10. **Context-upgrade arming** — the size estimate feeding the context filter is coarse, so the picked model can still overflow upstream. When a candidate in the same filtered pool declares a strictly larger `maxContextTokens` than the pick, the largest such candidate is returned as a second target marked `ContextUpgradeOnly`: the executor fails over to it exactly when the upstream verdict is a context overflow (`context_overflow` in [error-taxonomy-architecture.md](../../cross-cutting/safety/error-taxonomy-architecture.md)) and never for transient failures — spilling 5xx/429 traffic onto the larger, typically pricier, model would be a cost surprise. No arming when the pick is already the largest declared window or windows are undeclared.

Each step appends a `TraceEntry` — the selected model and the router's reason on success, or the failure cause otherwise — so the audit `routing_trace` and the simulate surface can replay the decision.

## 3. The catalog shown to the router

`SmartStore.ListEnabledChatModels` returns only enabled chat models joined with their enabled providers; embedding models and disabled providers are excluded, since smart routing is a chat-completion concern. In production the store is backed by the in-memory `cachelayer.Layer`, so the per-request enumeration hits memory rather than PostgreSQL.

`buildModelCatalog` renders the candidates into compact JSON grouped by provider, using short keys to conserve prompt tokens: `p` (provider), `m` (models), and per model `i`, `ip`/`op` (input/output USD per million tokens), `f` (capability tags), `mx`/`mo` (max context and output tokens). The `i` key is the model's **`Model.code`** — not the UUID and not the provider's wire model id. The router is shown the customer-facing code because it is a short, recognizable token; 36-character UUIDs inflate the token budget and LLMs frequently mistype them.

The `mx` value in the catalog is advisory prose for the router LLM; the binding enforcement of `maxContextTokens` is the code-level context-window filter (pipeline step 3), which removes undersized models before the catalog is rendered.

Within each provider group the models are ordered **newest-generation-first**. There is no release-date column and `createdAt` ties across a seed batch, so the only reliable recency signal is the version digits in `Model.code` — `buildModelCatalog` sorts each group by a digit-aware natural order on the code, descending (`claude-opus-4-8` before `-4-7` before `-4-6`; `gpt-5.5` before `gpt-5.4`). This gives the router a primacy cue that pairs with the `DefaultSystemPrompt` recency selection rule (§6): a small router model tends to pick an older same-tier generation when a plain-text "prefer the newest" instruction is the only signal, so the ordering makes the newest model the first one it reads. Ordering is presentation only — `resolveSelectedModelID` maps the returned code back to a `Model.id` regardless of position.

## 4. Mapping the router's answer back to a model

The router returns a code-like token. `resolveSelectedModelID` maps it to an internal `Model.id` UUID suitable for target lookup:

- When the router also returned a provider id, matching is restricted to that provider's rows, so an ambiguous code under the wrong provider does not silently land on a different vendor.
- It then tries an exact `Model.code` match (the canonical happy path), then a UUID match (for prompts that reference the internal id directly), then a unique `providerModelId` match (for outputs that lifted the upstream vendor name verbatim — accepted only when exactly one candidate matches).

An ambiguous or absent match is treated as an unknown selection and falls back.

## 5. The Decider and its production implementation

The LLM-call half is encapsulated behind the `Decider` interface — a pure decision function that takes a prepared system prompt, the projected conversation, the router model's context window, and routing metadata, and returns a `Decision` (`ModelID`, optional `ProviderID`, and a natural-language `Reason`). The smart strategy depends only on this interface; it does not import the provider adapter registry, the provider-target resolver, the canonical wire format, or the HTTP status vocabulary. A future local-classifier or rule-engine implementation plugs into the same seam with no change to the strategy.

The production implementation, `AdapterDecider`, makes the router-LLM call as an ordinary gateway provider call:

1. Resolve the router provider/model into a call target via the provider-target resolver.
2. Validate the target's wire format and select the matching provider adapter.
3. Build the request body in canonical OpenAI shape (`BuildRequestBody`); the adapter translates it to the upstream wire format.
4. Execute non-streaming, under the configured timeout.
5. Treat a `>= 400` status or a transport error as a decision failure; otherwise parse the response.

Because the call flows through the same adapter and target-resolution path as customer traffic, smart routing is provider-agnostic — the router LLM can be hosted on any configured provider. See [provider-adapter-architecture.md](provider-adapter-architecture.md).

## 6. Prompt construction and response parsing

`DefaultSystemPrompt` is the built-in router instruction. It documents the compact catalog legend, requires the router to return a `modelId` that exactly matches a catalog `i` (`Model.code`) value, and constrains output to a single JSON object `{"modelId": "...", "reason": "..."}`.

`BuildRequestBody` assembles the canonical OpenAI body — the system message plus the projected conversation. Content is flattened to text per message (`textOf` concatenates text content blocks and drops image, tool, and reasoning blocks — the router gains nothing from binary refs or tool plumbing), with roles preserved.

The conversation budget is `min(routerWindow − EstimateTokensConservative(systemPrompt) − 256, 4096)`, floored at 256 (the same upper-bound estimate as the context filter — the system prompt is subtracted from the window, so under-counting it would inflate the budget and risk overflowing the router model): `routerWindow` is the router model's declared `maxContextTokens` (8192 only when undeclared), the measured system prompt accounts for the catalog growing with the model table, 256 is reserved for the router's short JSON reply, and the 4096 cap keeps a giant router window from inflating per-request router cost — classification signal saturates quickly. Staging uses `StrategyRecentTurns` so follow-up turns ("continue", "try again") arrive with the context that defines them, and inputstaging's default budget enforcement guarantees the call never carries an over-limit prompt — an oversized newest turn is tail-truncated, never sent as-is. Overflow and budget-floor events are logged and counted in the `nexus_smart_router_input_overflow_total` metric.

`ParseResponse` extracts the `Decision` from the chat-completions envelope with three fallbacks: a direct JSON parse of the message content, then a markdown code-block extraction, then a last-resort regex that lifts `modelId` and `reason`. A response with no usable `modelId` is a parse failure and falls back.

## 7. Wiring

`InitRouter` (`packages/ai-gateway/cmd/ai-gateway/wiring/router.go`) assembles the smart dependencies only when both the cache layer and the provider-target resolver are present: the catalog store is backed by the cache layer, the target lookup reuses the resolver's lookup function, and the router LLM is an `AdapterDecider` over the provider-target resolver and the adapter registry. These dependencies are passed to `RegisterAllStrategies`, which registers the smart strategy only when they are non-nil.

## References

- `packages/ai-gateway/internal/routing/strategies/strategy_smart.go` — smart strategy pipeline, catalog build, selection resolution, fallback
- `packages/ai-gateway/internal/routing/llm/client.go` — `Decider` interface, `Request`, `Decision`
- `packages/ai-gateway/internal/routing/llm/adapter_decider.go` — production `AdapterDecider`
- `packages/ai-gateway/internal/routing/llm/prompt.go` — default system prompt, request body builder (conversation budget + staging), response parser
- `packages/ai-gateway/internal/routing/llm/metrics.go` — `nexus_smart_router_input_overflow_total`
- `packages/ai-gateway/internal/routing/core/smart_types.go` — `SmartStore`, `SmartModelRow`
- `packages/ai-gateway/internal/routing/core/smart_store.go` — catalog store adapter, enabled-chat-model enumeration
- `packages/ai-gateway/cmd/ai-gateway/wiring/router.go` — smart-routing dependency wiring
