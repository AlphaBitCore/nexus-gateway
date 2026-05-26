# Feature Smart Routing

Smart Routing is an optional LLM-dispatch layer that selects the best provider and model for each request based on the canonical prompt content, request context, and available model capabilities. It is a routing strategy inside the deterministic strategy tree — it augments, rather than replaces, admin-defined rules. Smart Routing always runs inside a PolicyNarrowing fence so the LLM cannot choose a provider or model the admin has not approved.

---

## What Nexus does

When a request matches a routing rule with `strategyType = "smart"`, the AI Gateway:

1. Assembles a candidate list from the Model Catalog, pre-filtered by PolicyNarrowing and the credential pool health rollup. Models with no healthy credentials are excluded before the LLM sees them.
2. Calls a configured dispatch model (typically a lightweight model for cost efficiency, e.g., Claude Haiku) with a structured prompt describing the request context and candidate list.
3. Receives `{ best_provider, best_model, confidence }` from the dispatch model.
4. If `confidence ≥ threshold` (default 0.75) and the chosen model is in the candidate list, Smart Routing's choice wins.
5. Otherwise, the strategy abstains and the deterministic tree handles the request — no client-visible failure.

The dispatch call has a 1s hard timeout. On timeout, Smart Routing abstains and the deterministic strategy tree takes over.

## The dispatch prompt

The dispatch model receives a structured context derived from the canonical payload — not the raw user message:

```text
Candidates: [{ modelCode, providerName, features, inputPrice, maxContextTokens }, ...]
Request context:
  modality: chat | embedding | image
  has_tool_calls: true | false
  cost_sensitivity: low | medium | high
  preferred_traits: [fast | long-context | tool-calling | cheap]
Prompt (first N tokens): ...
```

The prompt content comes from the canonical payload — the provider-agnostic normalised form of the request. This means the same dispatch prompt works correctly regardless of whether the request arrived via the OpenAI, Anthropic, or Gemini ingress.

## Safety guarantees

**Candidate pre-filtering**: the candidate list given to the LLM is already restricted by PolicyNarrowing and credential health. The LLM cannot pick a provider that is administratively prohibited or currently unhealthy.

**Post-LLM verification**: even within the pre-filtered list, an LLM might hallucinate a choice outside it. After receiving the LLM's choice, the gateway verifies the model is in the candidate set. A choice not in the set is treated as an abstention — the deterministic tree handles the request. A misbehaving dispatch model cannot bypass admin policy.

**Confidence threshold**: the dispatch model self-reports confidence. Below threshold, Smart Routing abstains and deterministic routing decides. This prevents Smart Routing from overriding the deterministic tree on ambiguous requests where it has low confidence.

## Cost of Smart Routing

The dispatch model call adds latency (typically 100–300ms for a lightweight model) and cost (~$0.001/call for Claude Haiku). This overhead is only justified when Smart Routing's selection of a better-fit model saves more than it costs.

The per-route Smart Routing dashboard helps administrators decide:

- Smart Routing call count and dispatch cost
- Win rate (Smart Routing vs deterministic)
- Average confidence
- Latency added (p50, p95)

If Smart Routing's win rate is low or the dispatch latency is unacceptable, disable it for that rule.

## Routing trace fields

Every `traffic_event` for a Smart-routed request records the full dispatch decision:

```json
{
  "smart_routing": {
    "enabled": true,
    "called": true,
    "dispatch_model": "claude-haiku-4-5-20251001",
    "chosen": "anthropic/claude-sonnet-4-6",
    "confidence": 0.91,
    "candidates_count": 5,
    "policy_narrowed_from": 8,
    "duration_ms": 245
  }
}
```

Investigators can audit every smart-routing decision after the fact from the Traffic Monitor's routing trace panel.

## Where it sits

- Strategy implementation: `packages/ai-gateway/internal/routing/strategies/strategy_smart.go`
- Model Catalog interface: `packages/ai-gateway/internal/routing/core/smart_store.go` (`SmartCatalog` — `ListEnabledModels`, `GetProvider`)
- Admin guard (enforces `requestedModelLiterals = ["auto"]`): `packages/control-plane/internal/ai/routing/handler/routing.go` (`validateSmartRuleMatchConditions`)

The dispatch model is called through the gateway's own Provider/Adapter infrastructure — no separate framework. Smart Routing is disabled on the dispatch request to prevent recursive loops.

## How to enable and configure

Smart Routing activates when a routing rule has `strategyType = smart` and `matchConditions.requestedModelLiterals = ["auto"]`. The admin guard rejects smart rules that do not pin the literal to `"auto"` — this prevents the strategy from accidentally capturing non-auto traffic.

Configuration fields on the strategy:

| Field | Default | Purpose |
|---|---|---|
| `dispatchModel.providerId` | (required) | Provider to use for the dispatch LLM call |
| `dispatchModel.modelId` | (required) | Model to use for the dispatch call |
| `confidenceThreshold` | 0.75 | Minimum confidence for Smart Routing to win over deterministic |
| `promptTemplate` | built-in | Override the dispatch prompt (rarely needed) |

To set up Smart Routing in the Control Plane:
1. Navigate to **AI Gateway → Routing Rules** and create a new rule.
2. Set strategy type to **Smart**.
3. Set match condition `requestedModelLiterals = ["auto"]` so only requests explicitly asking for automatic routing use this rule.
4. Choose the dispatch model (a fast, inexpensive model is recommended).
5. Set the confidence threshold. Higher = Smart Routing applies only to high-confidence decisions; lower = applies more broadly.
6. Save. Monitor the Smart Routing dashboard for win rate and cost.

---

## Canonical docs

- [`smart-routing-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/smart-routing-architecture.md) — full strategy detail, SmartCatalog interface, dispatch prompt, confidence threshold, latency budget, PolicyNarrowing post-LLM enforcement, trace fields
- [`routing-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/routing-architecture.md) — parent routing engine; Smart is one of seven strategy types

**Adjacent wiki pages**: [Feature Multi Provider Routing](Feature-Multi-Provider-Routing) · [AI Gateway Smart Routing](AI-Gateway-Smart-Routing) · [AI Gateway Routing Rules](AI-Gateway-Routing-Rules) · [AI Gateway Overview](AI-Gateway-Overview) · [Features Index](Features-Index)
