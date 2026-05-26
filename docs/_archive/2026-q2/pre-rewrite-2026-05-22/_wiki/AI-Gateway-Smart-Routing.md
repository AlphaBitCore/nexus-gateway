# AI Gateway Smart Routing

Smart Routing is an optional routing strategy that uses an LLM to pick the best provider and model for a request based on prompt content, request context, and the current model catalog. It is layered on top of the deterministic strategy tree — never replacing it, always subject to policy narrowing — so a misbehaving dispatch model cannot route traffic to a provider outside the admin-approved set. Smart routing fires only for requests where `model: "auto"` and a smart routing rule is configured.

---

## How to opt in

Smart routing is a routing **strategy type**, not a per-route toggle. To enable it, an admin creates a routing rule with:

- `strategyType: "smart"`
- `matchConditions.requestedModelLiterals: ["auto"]`

The admin guard (`validateSmartRuleMatchConditions`) rejects any smart rule that does not pin the model literal to exactly `"auto"`. Without this constraint, non-auto traffic would flow into LLM dispatch and produce non-grounded decisions.

```json
{
  "strategyType": "smart",
  "matchConditions": {
    "requestedModelLiterals": ["auto"]
  },
  "strategyJson": {
    "dispatchModel": { "providerId": "<providerUUID>", "modelId": "<modelUUID>" },
    "confidenceThreshold": 0.75,
    "promptTemplate": "<built-in or admin-provided>"
  }
}
```

Callers use `model: "auto"` on any `/v1/chat/completions` request and the gateway handles the rest.

## The dispatch call

When the strategy tree reaches a smart node, the engine:

1. Fetches enabled model candidates from `SmartCatalog` — pre-filtered by policy narrowing and credential-pool health. Models with no healthy credentials are excluded entirely; the dispatch LLM should never pick a model that cannot actually serve.
2. Calls the dispatch model (typically Claude Haiku or GPT-4o-mini for cost efficiency) with a structured prompt carrying the candidate list, request context, and the first N tokens of the caller's prompt.
3. Receives a structured response: `{best_provider, best_model, confidence}`.
4. Verifies the choice is in the candidate set (post-LLM PolicyNarrowing check).
5. If `confidence >= threshold` AND the choice is in the set: smart routing wins.
6. Otherwise: the engine abstains and the caller falls through to the next node in the strategy tree (typically a deterministic fallback chain).

The dispatch prompt carries:

```
Candidates: [...]              ← from SmartCatalog, filtered by policy
Request context:
  modality: chat | embedding | image
  max_context_tokens: ...
  has_tool_calls: true | false
  cost_sensitivity: low | medium | high
  preferred_traits: [fast | long-context | tool-calling | cheap]
Prompt (first N tokens): ...
```

The dispatch model is called through the gateway itself, but smart routing is disabled on the dispatch model's own request to prevent infinite loops.

## Safety: PolicyNarrowing enforcement

Even with pre-filtered candidates, the dispatch LLM can hallucinate a (provider, model) pair outside the list. After receiving the LLM's choice, the engine verifies the choice is in the `SmartModelRow` slice produced by `smartStoreDB`. If not, the engine logs a warning and falls through to deterministic routing. This makes smart routing safe by default.

## Confidence threshold

The dispatch model self-reports a confidence score. Below the configured threshold, smart routing abstains.

- Default threshold: `0.75`.
- Per-rule admin tuning via `strategyJson.confidenceThreshold`.
- Lower thresholds increase smart-routing influence; higher thresholds keep it as a tiebreaker only.

## Latency budget

Smart routing adds dispatch-model latency to every request that uses it.

| Dispatch model | Typical latency |
|---|---|
| Claude Haiku | ~100-300ms |
| GPT-4o-mini | ~100-400ms |
| Local model | ~50-150ms |

The dispatch call has a 1s timeout. Timeout causes smart routing to abstain (fall through to deterministic); a `smart_routing_timeout` counter increments.

## Tracing

Every `traffic_event.routing_trace` records the smart routing outcome:

```json
{
  "smart_routing": {
    "enabled": true,
    "called": true,
    "dispatch_model": "claude-haiku-4-5-20251001",
    "chosen": "anthropic/claude-3-5-sonnet",
    "confidence": 0.91,
    "candidates_count": 5,
    "policy_narrowed_from": 8,
    "duration_ms": 245
  }
}
```

The CP UI renders this in the Routing trace panel for every traffic event. Post-hoc audit of smart routing decisions is available without any additional tooling.

## Cost of smart routing

Each dispatch call costs approximately `$0.001`. The CP Analytics smart routing dashboard shows call count, dispatch cost, win rate (smart vs. deterministic), and average confidence. If smart routing is not winning enough (low confidence dominates), the overhead is not justified and the rule can be disabled without touching application code.

## Canonical payload dependency

Smart routing depends on the canonical payload — the canonical OpenAI-shape form of the request produced by `canonicalbridge.IngressChatToCanonical`. Without it, the dispatch prompt would need per-provider code paths to read the prompt and tool calls. The canonical payload eliminates that: smart routing is provider-agnostic by construction. See [Canonical Vs Wire Format](Canonical-Vs-Wire-Format) for the full canonical bus explanation.

---

## Canonical docs

- [`smart-routing-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/smart-routing-architecture.md) — SmartCatalog interface, dispatch call structure, PolicyNarrowing enforcement, trace fields
- [`routing-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/routing-architecture.md) — §5 parent routing engine; §6 model catalog; §8 admin guard
- [`ai-gateway.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/cp-ui/ai-gateway.md) — Routing Rules admin surface; smart routing dashboards

**Adjacent wiki pages**: [AI Gateway Routing Rules](AI-Gateway-Routing-Rules) · [AI Gateway Providers And Models](AI-Gateway-Providers-And-Models) · [AI Gateway Overview](AI-Gateway-Overview) · [Feature Smart Routing](Feature-Smart-Routing) · [Control Plane Vs Data Plane](Control-Plane-Vs-Data-Plane)
