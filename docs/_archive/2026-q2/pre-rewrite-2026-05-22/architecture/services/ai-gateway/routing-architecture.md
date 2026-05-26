---
doc: routing-architecture
area: service
service: ai-gateway
tier: 1
updated: 2026-05-20
---

# Routing Architecture

> **Tier 1 architecture doc.** Read this before touching `packages/ai-gateway/internal/routing/**`, `packages/ai-gateway/internal/execution/executor/**`, any routing-rule handler, the canonical payload structure (E47-S2), `ResolvedRequest`, smart routing, the Model Catalog, or model fallback chain semantics. Provider format translation lives in `provider-adapter-architecture.md`; error / 429 handling lives in `error-taxonomy-architecture.md`.

The routing engine answers one question per `/v1/*` request: **which provider + model + credential should serve this?** Plus, when E48 emergency passthrough is active: **should we forward at all without enforcement?**

---

## 1. The strategy tree

A routing rule is a tree of strategies. Internal nodes combine; leaves resolve.

| Strategy | Behaviour |
|---|---|
| **Single** | Routes to a specific (provider, model). |
| **Fallback** | Tries the head; on classified failure, falls through to the next. Fallback classification uses `error-taxonomy-architecture.md`. |
| **LoadBalance** | Weighted random across N candidates. Supports sticky routing on a header. |
| **Conditional** | Evaluates a match expression against the canonical payload (§3); branches into sub-strategies. |
| **A/B Split** | Weighted random across two (provider, model) pairs, for experimentation. |
| **PolicyNarrowing** | Restricts which (provider, model) pairs the inner strategy may produce. Used to enforce "only models X, Y, Z allowed for this org". |

The tree is evaluated top-down; the first leaf reached wins.

Every evaluation step is recorded in a `routing_trace` field on the traffic_event for debugging. The CP UI surfaces this trace inline.

## 2. Match conditions

Each rule carries a `matchConditions` JSON object. Real shape (`MatchConditions` struct in `packages/ai-gateway/internal/routing/core/types.go:251`; validated in `packages/control-plane/internal/ai/routing/handler/routing.go`):

```json
{
  "models":                 ["<modelUUID>", "..."],
  "requestedModelLiterals": ["auto", "gpt-4o", "..."],
  "modelTypes":             ["chat", "embedding", "image"],
  "providers":              ["<providerUUID>", "..."],
  "virtualKeys":            ["engineering-*"],
  "projects":               ["<projectUUID>", "..."]
}
```

- `models` — UUID set of `Model.id` rows the rule applies to (matched against the resolved Model UUID).
- `requestedModelLiterals` — exact string match against the raw `model` field on the inbound request. Smart-strategy rules MUST pin this to `["auto"]` (see §5 + admin guard `validateSmartRuleMatchConditions`).
- `modelTypes` — model-type set (`chat | embedding | image`).
- `providers` — UUID set of `Provider.id` rows.
- `virtualKeys` — glob patterns (`*`) matched against the VK name.
- `projects` — UUID set of project rows. `validateMatchConditions` rejects the legacy `organizations` field name.

Empty / omitted entries mean "any". If any populated condition fails, the rule is skipped. Rules are evaluated in priority order; the first matching rule's strategy tree runs.

## 3. Canonical payload (E47-S2)

Before routing evaluates, the request is normalised into a **canonical payload** — provider-agnostic JSON with stable field names and shape:

```
canonicalPayload = {
  model_id: "...",
  model_type: "chat",
  messages: [...],          // canonical role/content
  tools: [...],
  stream: true,
  metadata: {...}           // headers, vk, org, project
}
```

Smart-routing match runs on the canonical payload, so a single routing rule covers every provider — OpenAI's `messages[].role`, Gemini's `contents[].parts[].text`, and Anthropic's `messages[].content[].text` all reduce to the same canonical shape before evaluation.

Adapters produce the canonical payload via the normalize path (cross-ref `provider-adapter-architecture.md`).

## 4. `ResolvedRequest` — the L4 dispatch object

Once routing decides, the handler wraps the (already-built L3) `RequestContext` plus the post-routing decisions into a `ResolvedRequest`. Real shape (`packages/ai-gateway/internal/policy/requestcontext/resolved.go:59`):

```go
type ResolvedRequest struct {
    base        *RequestContext            // L3 immutable context (identity, headers, raw body, normalized payload, endpoint)
    route       *routingcore.RouteResult   // post-routing decision (targets, strategy_path, smart routing trace)
    passthrough *passthrough.Config        // effective merged passthrough config for the primary target's provider (E48 bypass flags ride here)
}
```

Constructed at Phase 4.5 via `Resolve(rc, route, ptc)`. All three pointers are retained as-is — nil is permitted on any of them and downstream accessors are nil-safe. Downstream consumers (hooks pipeline, audit Writer, executor, response normalize) receive a `*ResolvedRequest` and treat it as read-only; pre-routing layers (auth, rate-limit, routing engine itself) keep taking `*RequestContext`. The type wraps rather than extends because `RequestContext` is documented immutable after `Builder.Build()` — adding post-routing fields as nullable mutators would re-introduce after-Build coupling.

Accessors: `Base()`, `Route()`, `Passthrough()`, plus delegating helpers `Identity()`, `Normalized()`, `Endpoint()`, `Headers()`, `RawBody()`.

The executor (`packages/ai-gateway/internal/execution/executor/`) consumes the resolved object, decrypts the credential via `provtarget.Resolver`, hands the canonical payload to the per-adapter `SchemaCodec.EncodeRequest`, and dispatches.

When E48 emergency passthrough is active, the bypass flags ride on the `*passthrough.Config` returned by `Passthrough()`. The executor still runs but it skips hook invocation per the flags. The audit emitter records `bypass_hooks=true` and `bypass_reason` on the traffic_event.

## 5. Smart Routing (LLM-based dispatch)

Optional secondary router exposed as a `strategyType=smart` node in the strategy tree. When the strategy tree reaches a smart node, the engine:

1. Receives the canonical payload + the routing context.
2. Builds a compact candidate catalog from `SmartCatalog.ListEnabledChatModels` (filtered by VK `AllowedModels`).
3. Calls the configured router LLM (`SmartConfig.RouterProviderID` / `RouterModelID`) with the catalog substituted into the system prompt.
4. Parses the router's JSON output `{"modelId":"<code>","reason":"…"}` and resolves the code back to an internal `(providerID, modelID)` pair via `resolveSelectedModelID`.
5. On any failure (router LLM errored, exceeded `timeoutMs`, hallucinated a code outside the catalog, no candidates after filtering, …), falls back to the node's `DefaultProviderID/DefaultModelID`.

There is no separate confidence-threshold mechanism. The dispatch path is grounded in the candidate-catalog filter: a model the LLM "picks" but didn't appear in the catalog never serves traffic. Full details in `smart-routing-architecture.md`.

## 6. Model Catalog

The catalog is a provider × model × pricing × capabilities × routing-tags map. Sources:

- Provider table (admin-managed providers + endpoints).
- Model table (per-provider available models + pricing tiers).
- Tags on models (`fast`, `long-context`, `vision`, `tool-calling`, `cheap`, ...).

The `SmartCatalog` interface (`packages/ai-gateway/internal/routing/core/smart_store.go`) feeds the smart strategy (`packages/ai-gateway/internal/routing/strategies/strategy_smart.go`): given the canonical request, it returns enabled models (`ListEnabledModels`) and their provider rows (`GetProvider`) to populate the LLM dispatch candidate list within the policy filter.

The catalog also feeds the admin guard (§8) — only currently-cataloged (provider, model) pairs are eligible in routing rule UI.

## 7. Model fallback chain

A leaf strategy can attach an inline fallback chain:

```yaml
single:
  primary: { provider: openai, model: gpt-4o }
  fallback:
    - { provider: anthropic, model: claude-3-5-sonnet, onClass: [5xx, timeout, rate_429] }
    - { provider: openai, model: gpt-4o-mini, onClass: [5xx, timeout] }
```

`onClass` references the `ErrorClass` enum (cross-ref `error-taxonomy-architecture.md`). The executor classifies each upstream failure and walks the fallback chain. Each fallback attempt is recorded in `routing_trace`.

The provider/model health feed (cross-ref `credentials-architecture.md` for credential pool health rollup) can pre-empt the chain: if `(openai, gpt-4o)` has crossed the failure threshold, the executor short-circuits to the fallback before attempting the primary.

## 8. Admin guard (E47-S8)

The CP admin API validates routing rules before persisting:

- Every `(provider, model)` referenced must exist in the catalog.
- PolicyNarrowing filters that result in an empty effective set are rejected.
- A rule whose match conditions can never overlap with any cataloged model is rejected.

The guard exists to prevent the failure mode "the UI accepted this rule, but no request will ever match it" — which had been the cause of multiple support tickets pre-E47. The guard is enforced in the handler, not in the UI, so any path that creates a rule (admin API, seed scripts, manual SQL) gets the check.

## 9. Emergency passthrough integration (E48)

When the Hub-pushed kill-switch is active (any of the three tiers: org / provider / route), the routing engine consults the kill-switch shadow before constructing the `ResolvedRequest`.

- **Org-level kill** — the engine still computes the route, but stamps `BypassHooks=true` on the `ResolvedRequest`. The executor forwards without hooks.
- **Provider-level kill** — same as above, scoped to a provider.
- **Route-level kill** — same as above, scoped to one route.

Cross-ref `multi-endpoint-coordination-architecture.md` §4 for the full E48 flow. The routing engine is the **second consumer** of the kill switch (after the dispatch entry middleware): even if a request reaches routing, the engine respects the kill state.

## 10. Failure modes & evaluation trace

| Failure | Behaviour |
|---|---|
| No matching rule | Default route applies (admin-configured default provider/model). If no default, 503. |
| Smart routing timeout | Falls through to deterministic tree. |
| All fallback chain exhausted | Returns 503 with `reason=fallback_exhausted` (cross-ref `error-taxonomy-architecture.md`). |
| PolicyNarrowing eliminates all candidates | Returns 403 with `reason=policy_narrowing_empty` (rule is admin-mis-configured; admin guard should have caught it). |
| Catalog out of sync | Route still works (catalog is advisory for smart routing); fallback uses provider-stated availability. |

Every strategy that participates in the evaluation appends a `core.TraceEntry` (`packages/ai-gateway/internal/routing/core/types.go:224-229`) to the request's trace; the trace is a chronological JSON array, not a nested object:

```json
[
  { "strategyType": "conditional", "decision": "matched org=acme", "durationMs": 0 },
  { "strategyType": "fallback",    "decision": "head: openai/gpt-4o", "durationMs": 0 },
  { "strategyType": "smart",       "decision": "selected gpt-4o [openai/<modelUUID>] — reasoning task with vision", "durationMs": 230 }
]
```

This is what the CP UI's "Routing trace" panel renders — each entry shown as one row with strategyType, free-form decision text, and duration.

## 11. Sources

- `packages/ai-gateway/internal/routing/` — strategy types (`core/types.go`), strategy implementations (`strategies/`), matcher (`matcher/`), LLM dispatch (`llm/`), capability pre-filter (`capability/`), resolver entry (`resolver.go`).
- `packages/ai-gateway/internal/routing/core/smart_store.go` — `SmartCatalog` interface (`ListEnabledModels`, `GetProvider`).
- `packages/ai-gateway/internal/execution/canonicalbridge/` — canonical payload (E47-S2): `IngressChatToCanonical` + `ResponseCanonicalToIngress`.
- `packages/ai-gateway/internal/execution/executor/` — fallback chain, error classification, dispatch.
- `packages/ai-gateway/internal/policy/requestcontext/` — `RequestContext` (`context.go`: identity, normalized, endpoint, headers, raw body) and `ResolvedRequest` (`resolved.go`: base context + route result + passthrough config).
- `packages/control-plane/internal/ai/routing/handler/routing.go` — admin CRUD + `matchConditions` validation + `validateSmartRuleMatchConditions` admin guard.
- `tools/db-migrate/schema.prisma` — `RoutingRule`, `Provider`, `Model` tables.

## 12. Cross-references

- `provider-adapter-architecture.md` — canonical payload comes from the normalize path; denormalize is consumed by the executor.
- `credentials-architecture.md` — credential pool + health rollup feeds the fallback chain.
- `error-taxonomy-architecture.md` — `ErrorClass` that fallback `onClass` references.
- `quota-architecture.md` — quota check is upstream of routing dispatch.
- `multi-endpoint-coordination-architecture.md` §2 (admin creates routing rule) and §4 (emergency passthrough).
- `hook-architecture.md` — hook pipeline runs around routing.
