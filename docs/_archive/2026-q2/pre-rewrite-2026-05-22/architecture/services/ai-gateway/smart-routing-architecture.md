---
doc: smart-routing-architecture
area: service
service: ai-gateway
tier: 1
updated: 2026-05-20
---

# Smart Routing Architecture

> **Tier 2 architecture doc.** Read when touching `packages/ai-gateway/internal/routing/strategies/strategy_smart.go`, the `SmartCatalog` interface (`packages/ai-gateway/internal/routing/core/smart_store.go`), the dispatch-model prompt, or the smart-routing confidence threshold. Parent doc: `routing-architecture.md` (the broader routing engine).

Smart Routing is an **optional secondary router** that uses an LLM to pick the best provider for a request based on the canonical payload + request context. Layered on top of the deterministic strategy tree — never replacing it, always subject to PolicyNarrowing.

---

## 1. When smart routing fires

Smart routing is not a per-route YAML flag. It is a routing **strategy** — a `RoutingRule` row with `strategyType = "smart"`, evaluated by the same strategy tree as `single`, `fallback`, `loadbalance`, `conditional`, `absplit`, and `policy`.

To opt in, an admin creates a routing rule whose `matchConditions.requestedModelLiterals = ["auto"]` and whose `strategyType = "smart"`. The admin guard (`validateSmartRuleMatchConditions` in `packages/control-plane/internal/ai/routing/handler/routing.go`) rejects any smart rule that does not pin the literal to exactly `"auto"` — empty or unrestricted matchConditions would route non-auto traffic into LLM dispatch and produce non-grounded decisions.

Wire shape of a smart strategy node (`SmartConfig` in `packages/ai-gateway/internal/routing/strategies/strategy_smart.go:18-27`):

```json
{
  "strategyType": "smart",
  "matchConditions": { "requestedModelLiterals": ["auto"] },
  "strategyJson": {
    "routerProviderId":  "<providerUUID>",
    "routerModelId":     "<modelUUID>",
    "systemPrompt":      "<optional override; default DefaultSystemPrompt>",
    "temperature":       0,
    "maxTokens":         1024,
    "timeoutMs":         10000,
    "defaultProviderId": "<fallback providerUUID>",
    "defaultModelId":    "<fallback modelUUID>"
  }
}
```

When the strategy tree reaches a smart node, the engine builds a compact model catalog from `SmartCatalog.ListEnabledChatModels`, substitutes it into the system prompt's `{modelCatalog}` placeholder, hands the canonical user messages to `llm.Decider.Decide`, and parses the router's JSON output `{"modelId":"<exact code from catalog>","reason":"..."}`. The strategy then resolves `modelId` back to the catalog's UUID + provider and returns the `RoutingTarget`. Any failure along the chain (no candidates, router LLM unwired, router timeout, malformed JSON, modelId not in catalog) falls back to the `DefaultProviderID/DefaultModelID` pair configured on the node — there is no separate "confidence threshold" mechanism.

## 2. The dispatch call

The actual `DefaultSystemPrompt` (`packages/ai-gateway/internal/routing/llm/prompt.go:20-35`) is shape:

```
You are an AI model router for an enterprise gateway. Select the best model for the user's request.

## Available Models
The catalog is compact JSON: p = provider id, m = models for that provider; each model has i = the model code (Model.code …); optional ip/op = USD per 1M tokens, f = capability tags, mx/mo = max context / max output.

{modelCatalog}

## Selection Rules
1. Analyze the task: coding, analysis, creative writing, Q&A, translation, math, reasoning
2. Match capabilities …
3. Cost: simple → cheapest capable; complex → most capable
4. If uncertain, prefer the most capable model
5. modelId must match some catalog entry's i exactly.

## Output Format
Return ONLY valid JSON: {"modelId":"<exact ID from list>","reason":"<brief explanation>"}
```

User-role messages from the canonical request are passed as user-role messages on the router LLM call (system / assistant / tool roles are dropped). `inputstaging.Plan` (`StrategyLastUser`) truncates to fit `routerContextLimit=8192` minus `routerReserveOutput=256` (`prompt.go:53-63`). The dispatch model is itself called through the gateway (recursive but bounded — the dispatch path skips smart routing on the router LLM's own request to prevent infinite loops).

## 3. The `SmartCatalog` interface

Real interface (`packages/ai-gateway/internal/routing/core/smart_store.go`):

```go
type SmartCatalog interface {
    ListEnabledModels(ctx context.Context) ([]store.Model, error)
    GetProvider(ctx context.Context, id string) (*store.Provider, error)
}
```

The smart strategy wraps this narrow surface in `smartStoreDB` (same file), which joins `ListEnabledModels` output with `GetProvider` to produce per-row `SmartModelRow` records (`internal/routing/core/smart_types.go`):

```go
type SmartModelRow struct {
    ModelID          string    // UUID PK — used for FK + final RoutingTarget
    ModelCode        string    // customer-facing identifier ("gpt-4o") — sent to LLM
    ModelName        string
    ProviderID       string
    ProviderName     string
    ProviderModelID  string
    InputPricePM     *float64
    OutputPricePM    *float64
    Features         []string
    MaxContextTokens *int
    MaxOutputTokens  *int
}
```

E32-S1 anchor: the LLM never sees the 36-char UUID `ModelID` (LLMs frequently mistype UUIDs and burn token budget on them); it sees the short `ModelCode` and the engine maps the LLM's choice back to `ModelID` afterwards.

Candidates are pre-filtered by PolicyNarrowing AND by the credential pool's health rollup — models with no healthy credentials are excluded entirely (the LLM should never pick a model that can't actually serve).

## 4. PolicyNarrowing enforcement (post-LLM)

Even though the candidate set is pre-filtered, the LLM might still hallucinate a (provider, model) outside the list. After receiving the LLM's choice:

1. Verify the choice is in the candidate set (`SmartModelRow` slice produced by `smartStoreDB`).
2. If NOT, log a warning and fall through to deterministic routing.
3. If YES, proceed.

This "post-LLM verification" makes smart routing **safe by default** — a misbehaving dispatch model can't bypass admin policy.

## 5. Failure / fallback semantics (no confidence threshold)

Smart routing does NOT implement a numerical confidence threshold. The router LLM returns `{"modelId":"<code>","reason":"<text>"}` (no `confidence` field is requested or honoured). The strategy resolves `modelId` against the candidate catalog and:

- Match found → return the corresponding `RoutingTarget`.
- No match (LLM hallucinated a code outside the catalog), router LLM errored, exceeded `timeoutMs`, no candidates after VK `AllowedModels` filtering, request not normalisable for AI smart routing, or no user-role messages in the request → fall back to the node's `DefaultProviderID/DefaultModelID` via `smartFallback`. If no default is configured, the strategy returns an empty target slice and the parent strategy proceeds to its own fallback.

## 6. Latency budget

Smart routing adds dispatch-model latency to every request that uses it. The order-of-magnitude expectation is **sub-second p99 in observed prod traffic** (Haiku-class or GPT-4o-mini-class router models against catalogs of <20 candidate rows); concrete numbers vary by router model, prompt size, candidate-list length, and upstream weather. The `TraceEntry.DurationMs` stamped on the smart-strategy entry (see §7) is the authoritative real-time number for a given deployment.

Dispatch-call timeout: default **10 000 ms** (`packages/ai-gateway/internal/routing/strategies/strategy_smart.go:43-48` — `timeoutMs()` returns `10000` when `TimeoutMs <= 0`), configurable per smart node via `SmartConfig.TimeoutMs`. On expiry the `RouterLLM.Decide` call surfaces a context-deadline error; the strategy stamps the error as the trace decision and falls back to `DefaultProviderID/DefaultModelID` via `smartFallback`.

## 7. Smart-routing trace fields

The smart strategy appends one or more entries to the request's routing trace as `core.TraceEntry` records (`packages/ai-gateway/internal/routing/core/types.go:224-229`):

```go
type TraceEntry struct {
    StrategyType string `json:"strategyType"` // "smart"
    Decision     string `json:"decision"`     // free-form human-readable string
    DurationMs   int    `json:"durationMs"`
}
```

The `Decision` field is a free-form string: the happy path stamps `"selected <model> [<providerID>/<modelID>] — <router reason>"`; failure paths stamp the specific failure reason (`"missing smart config on node"`, `"no candidate models available"`, `"router LLM client not wired"`, `"router returned unknown model …"`, `"target lookup failed for …"`, the router error string, etc.). There is no structured nested object — the trace is the chronological list of `TraceEntry` records the strategy tree appended on its walk, and the CP Routing-trace UI renders the `decision` string verbatim.

## 8. The canonical payload guarantee

Smart routing depends on the canonical payload (`provider-adapter-architecture.md` §3a Rule 1) — the canonical OpenAI-shape form of the request. Without it, smart routing would need per-provider code paths to read the prompt + tools. The canonical payload eliminates that — the smart strategy filters `rctx.Request.Messages` by `Role == RoleUser` (`strategy_smart.go:151-156`) before handing user content to the router LLM, and that filter only works because `rctx.Request` is the unified `*normcore.NormalizedPayload` regardless of ingress format.

## 10. Cross-references

- `routing-architecture.md` §5 — parent routing engine.
- `provider-adapter-architecture.md` §3a — canonical payload.
- `credentials-architecture.md` §3 — pool health feeds `SmartCatalog`.
- `error-taxonomy-architecture.md` — fallback chain interaction.
- `prompt-cache-architecture.md` — smart routing's choice affects which cache tier serves.
