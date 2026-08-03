# Routing architecture

The routing engine turns the client-supplied `model` string into an ordered list of concrete `provider+model` targets — narrowed by policy, filtered by virtual-key access, and reordered by provider health — that the executor then dispatches to upstream. It lives in `packages/ai-gateway/internal/routing` and reads its rules from the `RoutingRule` table, which operators author on the Control Plane admin side.

## 1. Where routing sits in the request lifecycle

The proxy handler builds a `routingcore.RoutingContext` (`packages/ai-gateway/internal/routing/core`) describing the request — the requested model, the canonical endpoint kind, the virtual key, a read-only header projection, the canonical normalized body, and, for embeddings, the parsed embedding parameters — and calls `Router.ResolveTargets`. The result is a `RouteResult`: a flat, ordered, health-ranked target list plus trace metadata.

The handler then wraps the L3 request context, the `RouteResult`, and the effective passthrough config into a `ResolvedRequest` (`packages/ai-gateway/internal/policy/requestcontext`). `ResolvedRequest` is the read-only L4 view every post-routing consumer receives — the hooks pipeline, the audit writer, the executor, and response normalization. It is built once and treated as immutable; all three of its members are nil-safe, so a cold-start path that has not yet populated the passthrough cache resolves correctly with a nil passthrough.

## 2. Rule storage and catalog source

A `RoutingRule` (`packages/ai-gateway/internal/platform/store`) carries:

- `StrategyType` and `Config` — the strategy tree, stored as a `StrategyNode` JSON document.
- `MatchConditions` — the JSON predicate that decides which requests the rule applies to. It is the sole rule-matching truth source; an empty value matches every request.
- `Priority` and `PipelineStage` — ordering and stage membership (stage 0 = policy narrowing, stage 1 = route decision).
- `FallbackChain` — an inline `[{providerId, modelId}]` recovery list.
- `RetryPolicy` — a per-rule JSONB override for executor retry behavior; null means "use the YAML default as-is".

`GetEnabledRoutingRules` selects enabled rules ordered by `pipelineStage ASC, priority DESC`, caches the result in memory for thirty minutes, and coalesces concurrent cache-miss refreshes with a singleflight group. `InvalidateRuleCache` forces a re-fetch when an operator edits a rule. In production the resolver's catalog source is the in-memory `cachelayer.Layer` (provider/model snapshots plus the rule cache); the resolver depends only on a narrow `routingStore` interface, so `*store.DB` also satisfies it directly for tests and degraded paths.

## 3. Resolving the model string

The request `model` field is a customer-facing string (`Model.code` such as `gpt-4o`, or a request-side sentinel such as `auto`), not a UUID. `Resolver.hydrateRequestedModel` resolves it through `ResolveModelCandidates`, which returns every enabled `Model` row whose `code` matches exactly or whose `aliases` list contains the string. The matching `Model.id` UUIDs are recorded on `RequestedModel.CandidateIDs`, and the provider, type, and provider-model id are filled from the first candidate when empty.

The `auto` sentinel is intentionally left without candidates: a rule cannot accidentally route an `auto` request through a UUID-keyed `matchConditions.models`. Such requests must be authored against `matchConditions.requestedModelLiterals`, which matches the raw request string.

## 4. The resolution pipeline

`Resolver.Resolve` runs the staged pipeline and produces a `RoutingPlan`; `ResolveTargets` flattens and health-ranks it into the `RouteResult` the handler consumes.

### Stage 0 — policy narrowing

The narrowing engine evaluates every matching stage-0 rule whose strategy node is of type `policy` and accumulates a `NarrowingState`. Allow-lists intersect (each policy makes the set more restrictive); deny-lists union (each policy adds to the blocked set). The state is later applied as a filter — deny wins over allow, and a non-nil allow-list excludes anything not in it.

### Stage 1 — route decision

The resolver iterates the stage-1 rules in cache order. The first non-`fallback` matching rule becomes the **primary** rule; rules of type `fallback` are collected separately as recovery sources. The primary rule's `Config` is unmarshalled into a `StrategyNode` and evaluated by the strategy registry, yielding candidate targets. Those targets are then filtered by the stage-0 narrowing state and by the virtual key's allowed-models list; survivors are tagged `Source = "primary"`.

Only the primary rule's `RetryPolicy` is carried forward (as `RuleRetryPolicyJSON`); fallback rules' retry policies are deliberately ignored, since the primary rule alone determines L2/L3 retry behavior for the routed targets. The handler field-merges this per-rule policy on top of the YAML default before invoking the executor.

Recovery targets come from two sources, appended in order: the primary rule's inline `FallbackChain` (each entry looked up, **filtered** by the stage-0 narrowing state and the virtual key's allowed-models list, and tagged `Source = "fallback"`), then the separately collected `fallback`-type rules (evaluated, filtered, and tagged `Source = "recovery"`). Both recovery sources pass through `NarrowingEngine.Filter` exactly like the primary path, so no failover target can escape the per-VK `allowedModels` allowlist (SEC-C1-01) — a `FallbackChain` entry pointing at a provider/model outside the VK's allowlist is dropped before it can be dispatched on primary failure.

### Stage 1.5 — capability pre-filter (embeddings only)

When the endpoint is embeddings, a capability cache is wired, and the request carries embedding parameters, the resolver filters primary targets against each model's capability descriptor. `capability.Compatible` rejects a target when the model has no embeddings capability block, or when the request's `dimensions`, batch size, `encoding_format`, Cohere `input_type`, or Gemini `taskType` is unsupported (encoding format defaults to `["float","base64"]` when the descriptor omits it). If every candidate is rejected, `ResolveTargets` returns a `NoCompatibleProviderError` carrying each candidate's supported capabilities, which the handler surfaces as a `400` with an `available_capabilities` body.

### Health-aware ordering

`ResolveTargets` flattens primary plus recovery targets, then the `HealthRanker` reorders them — healthy providers first, degraded next, unavailable last — using a stable sort that preserves relative order within each health band. Unhealthy targets are reordered, never removed, because they may have recovered. A nil health tracker is a no-op.

A plan is marked `Substituted` when the first resolved target's model differs from the requested model; `OriginalModelID` preserves what the client asked for.

### Modality guard (all endpoints)

After the strategies produce targets, `applyModalityGuard` drops every target whose catalog model type cannot serve the request's endpoint modality. It runs once over the flattened primary+recovery set, uniformly across **every** strategy (`single`, `loadbalance`, `conditional`, `latency`, `smart`, `fallback`) and the requested-model passthrough path — so no rule authoring mistake or auto pick can dispatch an image model for a chat request, or a chat model onto `/v1/images/generations`. The decision is `typology.EndpointKindAcceptsModelType(kind, target.ModelType)`: each endpoint kind accepts the catalog `type` of the models that can serve it (`chat`→`chat`, `embeddings`→`embedding`, `image_generation`→`image`, `tts`→`tts`|`audio`, `stt`→`stt`|`audio`, `realtime`→`realtime`|`audio`, `video_generation`→`video`|`image`, `rerank`→`rerank`). Audio endpoints dual-accept the precise sub-type (`tts`/`stt`/`realtime`) and the coarse `audio` fallback, so a catalog that types its audio models precisely (the shipped catalog does) gets clean cross-sub-modality rejection while an older coarse-`audio` catalog still routes. Kinds that bind to no catalog model (`guardrail`, `batch`, `job`, `models`) and targets with an empty `ModelType` impose no constraint — the guard only ever removes a positively-mismatched modality. A drop is **non-silent**: a WARN plus a stage-2 `PipelineTrace` entry records it so an operator debugging a downstream 404 sees the guard as the cause. The filter is in place, one string comparison per target, no allocation on the hot path. When the guard empties the set, `ResolveTargets` surfaces `no_compatible_provider`; on the explicit-requested-model passthrough path (which the resolver never re-runs) the handler applies the same check and returns `400 MODEL_MODALITY_MISMATCH` rather than forwarding a wrong-modality model to the provider for a 502.

## 5. The strategy tree

`Config` is a tree of `StrategyNode` values — a discriminated union whose `Type` selects which fields apply. The `StrategyRegistry` evaluates the tree recursively up to a depth limit of ten, and is frozen after registration so the live set is immutable. `RegisterAllStrategies` always registers seven strategies; the eighth, `smart`, is registered only when its dependencies are wired.

| Type | Behavior |
|---|---|
| `single` | Resolves one `providerId`/`modelId` pair. A lookup failure is soft — it yields no targets rather than an error. |
| `fallback` | Concatenates the targets of all child nodes in order; each gets a full chance on retry. |
| `loadbalance` | Weighted-random selection across `weightedTargets`; a non-positive total weight yields no targets. |
| `conditional` | Evaluates branches in order and recurses into the first whose `when` predicate matches, else the `default`. |
| `ab_split` | Weighted-random selection across inline `abTargets` (`{providerId, modelId, weight}`). |
| `latency` | Orders inline `latencyTargets` (`{providerId, modelId}`) by measured p95 latency — the fastest tier first, with bounded exploration of low-sample targets. See "Latency-aware ordering" below. |
| `policy` | A no-op at evaluation time — policy nodes contribute only to stage-0 narrowing. |
| `smart` | LLM-dispatch routing; the router model picks the target from the request content. Detailed in [smart-routing-architecture.md](smart-routing-architecture.md). |

Each evaluation appends a `TraceEntry` describing its decision, so the simulate endpoint and audit trace can replay the path.

### Latency-aware ordering

The `latency` strategy ranks its targets by a **windowed per-target p95 latency** so an operator can route to the measured-fastest provider instead of hand-setting weights. It reuses the health tracker's existing per-provider sample window (`HealthTracker` already records `{success, latencyMs}` per upstream attempt over a 5-minute / 100-sample window); a read-side `GetLatencyP95` computes the nearest-rank p95 over the **successful** in-window samples (a slow timeout or fast 4xx must not move the latency ranking — error behavior is the health band's job) plus the total sample count.

Ordering is stateless and per-request. Warm targets (≥ 20 in-window samples) are quantized into 100 ms latency buckets — targets within one bucket are treated as equally fast, so sub-bucket jitter never reorders them, and the smoothed multi-sample window keeps any tier change slow (this quantization-plus-smoothing is the anti-oscillation guard that stops thundering-herd flap onto the currently-fastest target). The fastest occupied bucket is shuffled so served load spreads across equally-fast providers. Cold targets (too few samples) earn a small, bounded share of served traffic via ε-greedy exploration so their speed can be learned without a slow-and-idle target being re-promoted to the fastest tier indefinitely. When no health tracker is wired, or on a cold start, every target reads cold and the strategy degrades to a safe random load-balance.

This ordering composes with — and is subordinate to — the health-aware band reorder described below: the strategy emits its targets in latency order, and `HealthRanker` then stable-sorts them by band, so **health band dominates and latency orders within a band** (a fast-but-failing provider is still tried last). The per-target p95 + bucket + explore/exploit decision is recorded on the `TraceEntry`, so it rides the existing `traffic_event.routing_trace` audit surface with no new observability plumbing.

## 6. Match conditions

`MatchConditions` decides which requests a rule applies to. Every non-empty dimension is AND'd; an empty set is a catch-all:

- `models` — `Model.id` UUIDs, matched by intersecting against the request's hydrated `CandidateIDs`.
- `requestedModelLiterals` — raw request strings (such as `auto`) that are not `Model.code` values.
- `modelTypes`, `providers` — matched against the requested model's type and provider. The type vocabulary is the catalog `Model.type` set (`chat`, `embedding`, `image`, `audio`, `tts`, `stt`, `realtime`, `video`, `rerank`); audio models are typed precisely (`tts`/`stt`/`realtime`) in the shipped catalog, with the coarse `audio` retained for back-compat.
- `projects` — matched against the virtual key's project.
- `virtualKeys` — matched against the virtual key name, with `*` glob support.

The `conditional` strategy's `when` expression is a MongoDB-style predicate evaluated by the matcher: top-level fields are AND'd, with `$and` / `$or` / `$eq` / `$ne` / `$gt` / `$gte` / `$lt` / `$lte` / `$in` / `$nin` / `$regex` / `$not` operators. Fields resolve through dotted paths against the routing context — `requestedModel.*`, `endpointType`, `virtualKey.*`, and `headers.*`. Compiled regexes are bounded (length-limited and cleared when the cache fills) to keep rule evaluation cheap on the hot path.

## 7. Simulation and explain

The `/internal/routing-simulate` endpoint runs `Resolver.Explain`, which executes the full pipeline and additionally enumerates every terminal target reachable from the matched primary rule, each with the cumulative probability the live router would select it. Deterministic strategies report probability `1.0`; weighted strategies (`loadbalance`, `ab_split`) report `weight / sum`; `conditional` branches report `1.0` only when their predicate matches against the supplied context, otherwise `0.0` (and the default carries the full probability when no branch matched). Lookup failures do not abort enumeration — the affected branch is returned with an explanatory note and no resolved provider name, so operators still see "this branch would fire, but its target is currently unresolvable". The `smart` strategy cannot be enumerated without a live decision path, so it returns no branches; the simulate surface discloses this.

## 8. The routing target

A `RoutingTarget` is a resolved `provider+model` ready for dispatch. Beyond identifiers it carries:

- `AdapterType` — copied verbatim from `Provider.adapter_type`; downstream consumers read it as the authoritative wire format rather than deriving it from the provider name. See [provider-adapter-architecture.md](provider-adapter-architecture.md).
- `ModelType` — the catalog `Model.type` (`chat`/`embedding`/`image`/`tts`/`stt`/`realtime`/`video`/`rerank`), carried so the modality guard (§4) can reject a target whose modality does not match the request's endpoint without a second catalog lookup. Empty means unclassified and imposes no constraint.
- `ModelCode` — the customer-facing identifier, returned to clients in the `X-Nexus-Routed-Model` response header so they can correlate without seeing the internal UUID.
- `Region` — mirrors `Provider.region` and feeds the data-residency compliance hook; an empty string means the provider is unclassified and must be treated as "unknown region", not "any region".

The `RoutingContext.Request` field holds the canonical `NormalizedPayload` built once by the handler. Content-aware strategies (`smart`, content predicates) read `Request.Messages` directly rather than parsing raw bytes; it is nil for endpoints without a normalizable body (such as `/v1/models`) or when normalization failed, so consumers nil-check. See [normalization-architecture.md](normalization-architecture.md).

## References

- `packages/ai-gateway/internal/routing/resolver.go` — pipeline orchestration, model hydration, capability pre-filter
- `packages/ai-gateway/internal/routing/core/` — `StrategyNode`, `RoutingContext`, `RoutingTarget`, `RoutingPlan`, `RouteResult`, `HealthRanker`
- `packages/ai-gateway/internal/routing/strategies/` — strategy registry and the eight strategy implementations (`strategy_latency.go` reads the health tracker's windowed p95)
- `packages/ai-gateway/internal/platform/store/health.go` — `HealthTracker`, the shared p95 latency window (`GetLatencyP95`)
- `packages/ai-gateway/internal/routing/matcher/` — match-condition evaluation, MongoDB-style expressions, stage-0 narrowing, terminal-target enumeration
- `packages/ai-gateway/internal/routing/capability/` — embeddings capability descriptor and compatibility rules
- `packages/ai-gateway/internal/platform/store/routing.go` — `RoutingRule`, rule cache, `GetEnabledRoutingRules`
- `packages/ai-gateway/internal/platform/store/model.go` — `ResolveModelCandidates`
- `packages/ai-gateway/internal/policy/requestcontext/resolved.go` — `ResolvedRequest` L4 view
- `packages/ai-gateway/internal/ingress/debug/routing_simulate_endpoint.go` — `/internal/routing-simulate`
- `packages/ai-gateway/cmd/ai-gateway/wiring/router.go` — resolver and strategy-registry assembly
