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

`GetEnabledRoutingRules` selects enabled rules ordered by `pipelineStage ASC, priority DESC, createdAt ASC, id ASC`, caches the result in memory for thirty minutes, and coalesces concurrent cache-miss refreshes with a singleflight group. The stage and priority keys express operator intent; the trailing two make the order TOTAL, because the resolver takes the first matching rule and a tie decided by database return order would let the winner change on a cache refresh nobody triggered. Creation time settles a tie in the way an operator can reason about, and the id guarantees a total order behind it. `InvalidateRuleCache` forces a re-fetch when an operator edits a rule. In production the resolver's catalog source is the in-memory `cachelayer.Layer` (provider/model snapshots plus the rule cache); the resolver depends only on a narrow `routingStore` interface, so `*store.DB` also satisfies it directly for tests and degraded paths.

## 3. Resolving the model string

The request `model` field is a customer-facing string (`Model.code` such as `gpt-4o`, or a request-side sentinel such as `auto`), not a UUID. `Resolver.hydrateRequestedModel` resolves it through `ResolveModelCandidates`, which returns every enabled `Model` row whose `code` matches exactly or whose `aliases` list contains the string. The matching `Model.id` UUIDs are recorded on `RequestedModel.CandidateIDs`, and the provider, type, and provider-model id are filled from the first candidate when empty.

The `auto` sentinel is intentionally left without candidates: a rule cannot accidentally route an `auto` request through a UUID-keyed `matchConditions.models`. Such requests must be authored against `matchConditions.requestedModelLiterals`, which matches the raw request string.

## 4. The resolution pipeline

`Resolver.Resolve` runs the staged pipeline and produces a `RoutingPlan`; `ResolveTargets` flattens and health-ranks it into the `RouteResult` the handler consumes.

### Stage 0 — not implemented

The `RoutingRule` table carries a `pipelineStage` column and `StrategyNode` defines four narrowing fields (`allowModelIds`, `denyModelIds`, `allowProviderIds`, `denyProviderIds`), but no engine consumes them. The resolver skips every rule whose stage is not 1, so a rule authored at stage 0 is **inert**: nothing evaluates it and nothing reports that.

The narrowing vocabulary is sound; the engine that would carry it was never built. Until one is, the fields are configuration with no effect, which is the one shape the admin surface should not offer.

### Stage 1 — route decision

The resolver orders the stage-1 rules deterministically and takes the first matching rule **that actually resolves something** as the primary. Every matching rule below it is collected as recovery, in priority order — there is no separate species of backup rule. A rule an admin ranked lower IS the alternative for when the rule above cannot serve the request, and the walk advances to it only when every target of the rule above has been ELIMINATED (never on budget exhaustion), so an ordinary transient failure does not leak traffic across a rule boundary. The winning rule's own inline chain is tried before any lower rule's answer. Claiming the slot and producing nothing would disable every rule beneath, so a rule with nothing to offer yields to the next match.

Four things count as producing nothing, and each is recorded rather than merely logged. A configuration that does not parse, a strategy that cannot be evaluated, an evaluation that resolves no target, and an evaluation whose every target lies outside the calling key's allowed models. The virtual key's allowlist is therefore applied BEFORE the rule is judged: a rule holding the slot on the strength of targets that will be filtered away would disable the rules below it for exactly the keys that needed them.

Each yield appends a stage-1 `PipelineTrace` entry naming the rule and the reason, and every `TraceEntry` a rule's evaluation produced is stamped with that rule's id and name. Both ride `traffic_event.routing_trace`. Without the attribution a losing rule's "resolved …" line sits in a trace shared with the winner and reads as the plan's own decision — a target that was never dispatched to, presented as the route taken.

`routing_trace` carries a third array beside those two: `attempts`, the WALK, where the other two are the PLAN. The walk itself — failure classes, per-class selection, the two ceilings, and what the caller gets when nothing succeeded — is [recovery-engine-architecture.md](recovery-engine-architecture.md). The plan says which targets were considered; only this says which were tried, in what order, and why that order — each entry carrying `selectionReason` (next-in-list, largest-window, different-provider, and deprioritised-retry — which requires an explicitly configured call budget and so does not occur under the shipped defaults; see [recovery-engine-architecture.md](recovery-engine-architecture.md)) and `errorClass` (the failure's class, in the same vocabulary the neighbouring `code` uses). It became necessary when selection stopped being positional: a chain that jumped over three entries to reach the fourth is either a deliberate move or a bug, and from the plan alone those look identical.

When every rule that matched resolves nothing, the request is REFUSED with `503 ROUTING_RULES_RESOLVED_NOTHING` rather than passed through to the model the caller named — serving that model is the decision the rule existed to prevent. The trace is stamped onto the record before the refusal, because the error's hint sends the operator to read it. A request no rule matched still passes through unchanged; the two are told apart at resolution time, before any filter, because a filter emptying the plan is our fact about the targets and not a rule refusing anything.

The primary rule's `Config` is unmarshalled into a `StrategyNode` and evaluated by the strategy registry, yielding candidate targets; survivors of the allowlist are tagged `Source = "primary"`.

Only the primary rule's `RetryPolicy` is carried forward (as `RuleRetryPolicyJSON`); fallback rules' retry policies are deliberately ignored, since the primary rule alone determines L2/L3 retry behavior for the routed targets. The handler field-merges this per-rule policy on top of the YAML default before invoking the executor.

Recovery targets come from two sources, appended in order: the primary rule's inline `FallbackChain` (each entry looked up directly in the catalog, filtered by the virtual key's allowed-models list, and tagged `Source = "fallback"`), then the separately collected `fallback`-type rules (evaluated, filtered by the same allowlist, and tagged `Source = "recovery"`). The virtual-key allowlist is the only filter either source passes, so no failover target can escape the per-VK `allowedModels` allowlist.

That allowlist is also the **only** thing they share with the primary path. A `smart` rule's primary targets clear a capability filter and a context-window filter inside the strategy; the same rule's `FallbackChain` entries clear neither, because they are built outside the strategy that applied them. A capability filter over recovery targets exists in the resolver to compensate, which is why the question is answered in two places with two implementations.

### Stage 1.4 — the passthrough lookup answers two questions, not one

`resolveNoMatchPassthrough` asks the catalog index for the requested code or
alias, and that call can fail for two opposite reasons which must not share a
status. A **miss against a loaded index** means the model is not in the catalog:
`404 ROUTING_NO_MATCH`, permanent, stop asking. An **unloaded index**
(`cachelayer.ErrIndexUnavailable`, returned when the atomic pointer is still nil
because no load has succeeded) means the lookup could not run at all:
`503 MODEL_CATALOG_UNAVAILABLE`, transient, retry.

The distinction is load-bearing because the index is nil for a real window —
before the first successful `ReloadModels`. Until 2026-08-19 both cases returned
the same `404 … / Ensure the model exists and is enabled`, and on 2026-08-11
staging served that answer for 34 minutes to every request for six models
(claude-sonnet-4-6, claude-opus-4-6, claude-haiku-4-5-20251001, gpt-4o,
text-embedding-3-large, gemini-embedding-001) that existed, were enabled, and
sat on enabled providers throughout. `404` is the one status an SDK treats as
final, so the gateway spent that window telling clients its own catalog was
empty in the most permanent terms available.

The same split applies to the credential index (`GetCredentialForProvider`),
whose failure the executor reports as a target it could not prepare — see the
`PROVIDER_TARGET_UNAVAILABLE` note in the AI-gateway error taxonomy.

### Stage 1.5 — capability pre-filter (embeddings only)

When the endpoint is embeddings, a capability cache is wired, and the request carries embedding parameters, the resolver filters primary targets against each model's capability descriptor. `capability.Compatible` rejects a target when the model has no embeddings capability block, or when the request's `dimensions`, batch size, `encoding_format`, Cohere `input_type`, or Gemini `taskType` is unsupported. `dimensions` is checked against a **range first and an enumeration second**: when the descriptor declares `max_dimension`, any value in `[min_dimension or 1, max_dimension]` is forwarded and only a value outside the bound is refused; only when no range is declared does the value have to appear in `supported_dimensions`. The two forms describe genuinely different models and both are needed. A Matryoshka model — OpenAI's `text-embedding-3-*`, Gemini's `gemini-embedding-001` — truncates to *any* width up to its native size, so no list describes it honestly and every list is a set of values we refuse for no reason the provider would give; a Cohere v3 model emits 1024 and nothing else, and the list is exactly right for it. Encoding format defaults to `["float"]` alone when the descriptor omits it: `float` is the only encoding every embedding codec emits unconditionally, and `base64` must be declared explicitly because the voyage, gemini and bedrock codecs always emit float and never read the field while the Cohere codec rejects it outright. Defaulting to both would let a `base64` request pass the filter and be silently downgraded — a wrong answer with no error attached. If every candidate is rejected, `ResolveTargets` returns a `NoCompatibleProviderError` carrying each candidate's supported capabilities, which the handler surfaces as a `400` with an `available_capabilities` body.

### Health-aware ordering

`ResolveTargets` flattens primary plus recovery targets, then the `HealthRanker` reorders them — healthy providers first, degraded next, unavailable last — using a stable sort that preserves relative order within each health band. Unhealthy targets are reordered, never removed, because they may have recovered. A nil health tracker is a no-op.

**Position zero is exempt.** The head of the list is the strategy's answer, reached with information health does not have: a router LLM's read of the request, a conditional's branch, a weighted draw an admin configured. Health knows which providers have been failing lately. Letting the second overrule the first serves the request from a model chosen for its uptime rather than its fitness — measured once as a document request whose selected model was displaced and answered "no inline document part".

The exemption is about PREFERENCE, and it has one stated exception: a provider whose health is `unavailable` is not answering at all, so opening the walk against it spends an attempt and a full timeout to learn what the tracker already knows. That is a fact about whether the call can happen, not a preference, and the head is demoted for it. A `degraded` provider — answering, just worse lately — keeps position zero.

A target armed for context overflow (`ContextUpgradeOnly`) is held behind every ordinary target regardless of its provider's health: it was chosen for window size, not for the ability to serve this request.

A plan is marked `Substituted` when the first resolved target's model differs from the requested model; `OriginalModelID` preserves what the client asked for.

### Modality guard (all endpoints)

After the strategies produce targets, `applyModalityGuard` drops every target whose catalog model type cannot serve the request's endpoint modality. It runs once over the flattened primary+recovery set, uniformly across **every** strategy (`single`, `loadbalance`, `conditional`, `ab_split`, `latency`, `smart`, `fallback`) and the requested-model passthrough path — so no rule authoring mistake or auto pick can dispatch an image model for a chat request, or a chat model onto `/v1/images/generations`. The decision is `typology.EndpointKindAcceptsModelType(kind, target.ModelType)`: each endpoint kind accepts the catalog `type` of the models that can serve it (`chat`→`chat`, `embeddings`→`embedding`, `image_generation`→`image`, `tts`→`tts`|`audio`, `stt`→`stt`|`audio`, `realtime`→`realtime`|`audio`, `video_generation`→`video`|`image`, `rerank`→`rerank`). Audio endpoints dual-accept the precise sub-type (`tts`/`stt`/`realtime`) and the coarse `audio` fallback, so a catalog that types its audio models precisely (the shipped catalog does) gets clean cross-sub-modality rejection while an older coarse-`audio` catalog still routes. Kinds that bind to no catalog model (`guardrail`, `batch`, `job`, `models`) and targets with an empty `ModelType` impose no constraint — the guard only ever removes a positively-mismatched modality. A drop is **non-silent**: a WARN plus a stage-2 `PipelineTrace` entry records it so an operator debugging a downstream 404 sees the guard as the cause. The filter is in place, one string comparison per target, no allocation on the hot path. When the guard empties the set, `ResolveTargets` surfaces `no_compatible_provider`; on the explicit-requested-model passthrough path (which the resolver never re-runs) the handler applies the same check and returns `400 MODEL_MODALITY_MISMATCH` rather than forwarding a wrong-modality model to the provider for a 502.

### Required-modality floor, input-modality ceiling, and who owns the verdict

Two further checks read the catalog's per-model modality declarations rather than its `type`. The **floor** (`Model.RequiredModalities`) asks whether a model *requires* a modality the request does not carry — a text-only request to a model that requires audio cannot succeed. The **ceiling** (`Model.InputModalities`) asks the opposite — whether the request carries an input modality the model does not accept, and enforces only modalities some catalog row describes, so a modality no row declares constrains nothing. The floor travels every selection path: `filterByFloor` over the resolver's primary targets, `filterRecoveryByCapability` over its recovery list, and `floorGuard` on the passthrough. The ceiling travels two of them — `filterRecoveryByCapability` over the recovery list and `inputModalityGuard` on the passthrough (`400 MODEL_INPUT_MODALITY_UNSUPPORTED`) — but not the resolver's primary targets, which get the floor only: the ceiling is a claim about the catalog, and an explicit pick that survived to a primary target is honored rather than second-guessed.

Who owns that verdict depends on who chose the model, because the catalog that supplies it can be incomplete. When the **gateway** chose (`auto`, an empty model, a code fanning out to several rows — `callerNamedTheModel` is false), the choice is the gateway's and it must hand the executor a target that can serve, so the floor is enforced unconditionally. When the **caller named** the model (a direct model id, or a rule with a single explicit target), the modality verdict is the upstream's by default: the caller owns the model they named, and a stale catalog cell must not turn a request the model can serve into a local refusal. The fleet flag `routing.enforceNamedModelModality` (`config.Routing.EnforceNamedModelModality`, default false) flips the named case to enforce the floor and the input-modality ceiling locally, trading the upstream round-trip for a `400` that trusts the catalog. The resolver applies this to its primary and recovery floor together; the passthrough applies it through `namedModelModalityGuard`, which wraps `floorGuard` and `inputModalityGuard` behind the same flag. The embeddings capability pre-filter (Stage 1.5) is not governed by it — a dimensions value a model cannot emit is a parameter fact, not a modality the catalog might have mislabeled. That holds only while the descriptor states the fact truthfully, and for a Matryoshka model an enumeration cannot: a catalog listing `[256,512,1024,3072]` for `text-embedding-3-large` made every `dimensions: 1536` caller a local `400` that OpenAI would have served. This is why the dimensions check prefers the range form — the bound is a fact the catalog can state correctly, whereas the list invites exactly the staleness the named-model carve-out above exists to protect against.

## 5. The strategy tree

`Config` is a `StrategyNode` — a discriminated union whose `Type` selects which fields apply. Child entries (a fallback chain's links, a load balancer's weighted entries, a conditional's branches) name a provider and a model directly and are resolved as leaves; the registry evaluates one node and does not follow a node into another. The wire shape is unchanged, so a stored configuration still parses and still means what it meant; what is gone is the evaluator that would have followed it anywhere, and with it the depth limit that bounded the descent.

The admin write path refuses a nested entry, naming what a child must be and what would happen if it were accepted. The simulate walker agrees rather than descending: a stored nested entry is listed as UNREACHABLE with the reason, so an operator sees the branch they wrote and that it routes nothing — publishing a distribution over branches no live request can take is worse than publishing none, because simulate is what an admin uses to check a rule before trusting it. Rows stored while nesting was still accepted are disabled by an upgrade migration rather than flattened; guessing which branch the admin meant to keep is not the migration's business.

The registry is frozen after registration so the live set is immutable. `RegisterAllStrategies` registers six strategies; the seventh, `smart`, is registered only when its dependencies are wired.

| Type | Behavior |
|---|---|
| `single` | Resolves one `providerId`/`modelId` pair. A lookup failure is soft — it yields no targets rather than an error. |
| `fallback` | Concatenates the targets of all child nodes in order; each gets a full chance on retry. |
| `loadbalance` | Weighted-random selection across `weightedTargets`; a non-positive total weight yields no targets. |
| `conditional` | Evaluates branches in order and resolves the leaf of the first whose `when` predicate matches, else the `default`. It returns exactly one target: the branches it did not take are other requests' answers, not fallbacks for this one. |
| `ab_split` | Weighted-random selection across inline `abTargets` (`{providerId, modelId, weight}`). It returns exactly one target, for the same reason as `conditional` — a request counted as A that is served by B leaves the experiment measuring nothing. |
| `latency` | Orders inline `latencyTargets` (`{providerId, modelId}`) by measured p95 latency — the fastest tier first, with bounded exploration of low-sample targets. See "Latency-aware ordering" below. |
| `smart` | LLM-dispatch routing; the router model picks the target from the request content, and two other members of the pool it chose from ride behind it so a failure has somewhere to go inside the rule. Detailed in [smart-routing-architecture.md](smart-routing-architecture.md). |

Each evaluation appends a `TraceEntry` describing its decision, so the simulate endpoint and audit trace can replay the path.

### Latency-aware ordering

The `latency` strategy ranks its targets by a **windowed per-target p95 latency** so an operator can route to the measured-fastest provider instead of hand-setting weights. It reuses the health tracker's existing per-provider sample window (`HealthTracker` already records `{success, latencyMs}` per upstream attempt over a 5-minute / 100-sample window); a read-side `GetLatencyP95` computes the nearest-rank p95 over the **successful** in-window samples (a slow timeout or fast 4xx must not move the latency ranking — error behavior is the health band's job) plus the total sample count.

Ordering is stateless and per-request. Warm targets (≥ 20 in-window samples) are quantized into 100 ms latency buckets — targets within one bucket are treated as equally fast, so sub-bucket jitter never reorders them, and the smoothed multi-sample window keeps any tier change slow (this quantization-plus-smoothing is the anti-oscillation guard that stops thundering-herd flap onto the currently-fastest target). The fastest occupied bucket is shuffled so served load spreads across equally-fast providers. Cold targets (too few samples) earn a small, bounded share of served traffic via ε-greedy exploration so their speed can be learned without a slow-and-idle target being re-promoted to the fastest tier indefinitely. When no health tracker is wired, or on a cold start, every target reads cold and the strategy degrades to a safe random load-balance.

This ordering composes with — and is subordinate to — the health-aware band reorder described below: the strategy emits its targets in latency order, and `HealthRanker` then stable-sorts them by band, so **health band dominates and latency orders within a band** (a fast-but-failing provider is still tried last). The per-target p95 + bucket + explore/exploit decision is recorded on the `TraceEntry`, so it rides the existing `traffic_event.routing_trace` audit surface with no new observability plumbing.

## 6. Match conditions

`MatchConditions` decides which requests a rule applies to. Every non-empty dimension is AND'd; an empty set is a catch-all:

- `models` — `Model.id` UUIDs, matched by intersecting against the request's hydrated `CandidateIDs`.
- `requestedModelLiterals` — raw request strings (such as `auto`) that are not `Model.code` values.
- `modelTypes` — matched against the **endpoint the request arrived on**, through `typology.EndpointKindAcceptsModelType`. The stored vocabulary is the catalog `Model.type` set (`chat`, `embedding`, `image`, `audio`, `tts`, `stt`, `realtime`, `video`, `rerank`) and stays that way; the predicate translates it, so a stored `embedding` means the embeddings endpoint and audio models typed precisely (`tts`/`stt`/`realtime`) keep the coarse `audio` back-compat. The endpoint is a fact of the request and is present for every request, including `model: "auto"` — matching against the named model's catalog row instead left every `modelTypes` rule unable to match the requests it was written for.
- `providers` — matched against the requested model's provider, and **inapplicable when the caller named no model**. The condition asks whether the model the caller named belongs to a provider this rule handles, which has no answer for an `auto` request; reading the absent answer as a failed match made a provider-scoped rule invisible to exactly the requests that ask the gateway to route. A request that did name a model is still compared, so the rule leaves another provider's model alone.
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
- `packages/ai-gateway/internal/routing/strategies/` — strategy registry, leaf resolution, and the strategy implementations (`strategy_latency.go` reads the health tracker's windowed p95)
- `packages/ai-gateway/internal/platform/store/health.go` — `HealthTracker`, the shared p95 latency window (`GetLatencyP95`)
- `packages/ai-gateway/internal/routing/matcher/` — match-condition evaluation, MongoDB-style expressions, virtual-key access filtering, terminal-target enumeration
- `packages/ai-gateway/internal/routing/capability/` — embeddings capability descriptor and compatibility rules
- `packages/ai-gateway/internal/platform/store/routing.go` — `RoutingRule`, rule cache, `GetEnabledRoutingRules`
- `packages/ai-gateway/internal/platform/store/model.go` — `ResolveModelCandidates`
- `packages/ai-gateway/internal/policy/requestcontext/resolved.go` — `ResolvedRequest` L4 view
- `packages/ai-gateway/internal/ingress/debug/routing_simulate_endpoint.go` — `/internal/routing-simulate`
- `packages/ai-gateway/cmd/ai-gateway/wiring/router.go` — resolver and strategy-registry assembly
