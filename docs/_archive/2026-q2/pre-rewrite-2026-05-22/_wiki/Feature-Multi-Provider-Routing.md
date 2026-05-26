# Feature Multi Provider Routing

Multi-provider routing is the core dispatch mechanism of the AI Gateway. Administrators define declarative routing rules — strategy trees with match conditions — that determine which provider and model handles each request. The routing engine evaluates rules in priority order, records every evaluation step in a `routing_trace` field on the traffic event, and surfaces that trace in the Control Plane UI for debugging.

---

## What Nexus does

The routing engine answers one question per `/v1/*` request: which provider, model, and credential should serve this?

Before the routing engine evaluates, every request is normalised into a **canonical payload** — a provider-agnostic JSON representation with stable field names. This means a Conditional or Smart rule matching on prompt content works the same regardless of whether the request arrived via the OpenAI, Anthropic, or Gemini ingress.

## Strategy types

A routing rule is composed of **match conditions** and a **strategy tree**. The match conditions filter which requests the rule applies to; the strategy tree determines the dispatch.

| Strategy | Behavior |
|---|---|
| Single | Routes to a specific (provider, model). |
| Fallback | Tries the head; on classified failure, falls through to the next entry in the chain. |
| LoadBalance | Weighted random across N candidates. Supports sticky routing on a request header. |
| Conditional | Evaluates a match expression against the canonical request payload; branches into sub-strategies. |
| A/B Split | Weighted random across two (provider, model) pairs for controlled experimentation. |
| PolicyNarrowing | Restricts which (provider, model) pairs the inner strategy may produce. Enforces "only models X, Y, Z for this org or virtual key". |
| Smart | LLM-dispatch sub-strategy — see [Feature Smart Routing](Feature-Smart-Routing). |

Strategy nodes compose. A typical production rule might be: PolicyNarrowing → Fallback[Single(openai/gpt-5), Single(anthropic/claude-sonnet-4-6)].

## Match conditions

Each routing rule carries a `matchConditions` object. Fields:

| Field | Matches on | Example |
|---|---|---|
| `models` | UUID set of registered `Model` rows | `["<uuid>"]` |
| `requestedModelLiterals` | Exact string in the request's `model` field | `["gpt-4o", "auto"]` |
| `modelTypes` | Model modality | `["chat", "embedding"]` |
| `providers` | UUID set of registered `Provider` rows | `["<uuid>"]` |
| `virtualKeys` | Glob patterns against VK name | `["engineering-*"]` |
| `projects` | UUID set of project rows | `["<uuid>"]` |

Empty or omitted conditions mean "any". If any populated condition fails for a request, the rule is skipped. Rules are evaluated in priority order; the first matching rule's strategy tree runs.

## Fallback chains

A leaf strategy can attach an inline fallback chain. Each entry specifies the next (provider, model) to try and the failure classes that trigger it:

```yaml
single:
  primary: { provider: openai, model: gpt-5 }
  fallback:
    - { provider: anthropic, model: claude-sonnet-4-6, onClass: [5xx, timeout, rate_429] }
    - { provider: openai, model: gpt-4o-mini, onClass: [5xx, timeout] }
```

Failure classes (`5xx`, `timeout`, `rate_429`, and others) are defined in the error taxonomy. Each fallback attempt is recorded in the routing trace. If the credential pool health feed shows that a primary target has crossed the failure threshold, the executor short-circuits to the fallback without attempting the primary first.

## Admin guard

The Control Plane admin API validates routing rules before persisting:

- Every (provider, model) pair referenced must exist in the Model Catalog.
- PolicyNarrowing filters that result in an empty effective set are rejected.
- A rule whose match conditions can never overlap with any cataloged model is rejected.

This prevents the failure mode "the UI accepted this rule, but no request will ever match it" from reaching production.

## Where it sits

Routing runs entirely inside the **AI Gateway** service:

- Strategy types and tree evaluation: `packages/ai-gateway/internal/routing/strategies/`
- Match conditions and rule ordering: `packages/ai-gateway/internal/routing/matcher/`
- Model Catalog interface: `packages/ai-gateway/internal/routing/core/smart_store.go`
- Request context and `ResolvedRequest` wrapper: `packages/ai-gateway/internal/policy/requestcontext/`
- Admin CRUD + guard: `packages/control-plane/internal/ai/routing/handler/routing.go`

## Routing trace visibility

Every traffic event carries a `routing_trace` JSONB column:

```json
{
  "rule_id": "rule-abc",
  "strategy_path": ["Conditional[orgMatch]", "Fallback[head]", "Single[openai/gpt-5]"],
  "fallback_attempts": [
    { "target": "openai/gpt-5", "outcome": "5xx", "latency_ms": 1200 }
  ],
  "smart_routing": { "enabled": false }
}
```

The Control Plane Traffic Monitor renders this trace inline on each request row. Use it to diagnose why a request went to an unexpected provider, or why a fallback chain was triggered.

## How to enable and configure

Routing rules are managed from **AI Gateway → Routing Rules** in the Control Plane UI:

1. Select **New Rule** and choose a strategy type.
2. Set match conditions (model, virtual key, project, provider, or model literal).
3. For Fallback strategies, add chain entries with `onClass` failure classifiers.
4. Optionally apply PolicyNarrowing as an outer wrapper to restrict eligible models.
5. Set the rule priority relative to other rules.
6. Save. The admin guard rejects rules that reference non-existent models or produce empty effective sets.

The rule takes effect on the next matching request. The Routing Rules page also shows a live "Request simulator" that tests a hypothetical request against the rule set and shows which rule would match and which strategy would fire.

---

## Canonical docs

- [`routing-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/routing-architecture.md) — strategy tree, match conditions, canonical payload, ResolvedRequest, fallback chain, Model Catalog, admin guard, emergency passthrough integration
- [`smart-routing-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/smart-routing-architecture.md) — LLM-dispatch strategy detail

**Adjacent wiki pages**: [Feature Smart Routing](Feature-Smart-Routing) · [AI Gateway Routing Rules](AI-Gateway-Routing-Rules) · [AI Gateway Overview](AI-Gateway-Overview) · [Feature Cost Tracking](Feature-Cost-Tracking) · [Features Index](Features-Index)
