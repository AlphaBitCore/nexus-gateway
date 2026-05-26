# AI Gateway Routing Rules

Routing rules answer the question: given an incoming `/v1/*` request, which provider, model, and credential should serve it? Rules are evaluated in priority order against the canonical payload of every request. When a rule's match conditions pass, its strategy tree resolves to a `(provider, model, credential)` triple. The result — including the full evaluation trace — is recorded on every `traffic_event` row for debugging.

---

## Match conditions

Each rule carries a `matchConditions` object. All populated conditions must pass for the rule to apply; empty or omitted conditions match anything.

| Field | Type | Meaning |
|---|---|---|
| `models` | UUID list | Matches when the request resolves to one of these Model rows |
| `requestedModelLiterals` | string list | Exact match on the raw `model` string from the request body (e.g., `["gpt-4o"]`) |
| `modelTypes` | enum list | `chat`, `embedding`, `image` |
| `providers` | UUID list | Matches when the resolved provider is in this list |
| `virtualKeys` | glob list | Glob patterns (`*`) against the VK name |
| `projects` | UUID list | Matches when the request's project is in this list |

Rules are evaluated in priority order; the first rule whose match conditions all pass runs its strategy tree. If no rule matches, the gateway falls back to the admin-configured default route; if no default is configured, the request returns `503`.

## Strategy types

A routing rule's strategy is a composable tree. Internal nodes combine; leaf nodes resolve to a (provider, model) pair.

| Strategy | Behaviour |
|---|---|
| `single` | Routes to a specific (provider, model). The simplest strategy. |
| `fallback` | Tries the head; on a classified failure, falls through to the next option. Uses `ErrorClass` from the error taxonomy. |
| `loadbalance` | Weighted random across N candidates. Supports sticky routing on a request header. |
| `conditional` | Evaluates a match expression against the canonical payload; branches into sub-strategies. |
| `absplit` | Weighted random across two (provider, model) pairs for controlled experimentation. |
| `policynarrowing` | Restricts which (provider, model) pairs the inner strategy may produce — enforces org policy at routing time. |
| `smart` | LLM-based dispatch; `model: "auto"` required. See [AI Gateway Smart Routing](AI-Gateway-Smart-Routing). |

Strategies compose: a `fallback` node can wrap a `loadbalance` head and a `single` tail; a `conditional` node can branch to different `fallback` chains per org or VK.

## Fallback chains and error classes

The `fallback` strategy tries its head first. On failure, it classifies the error against `ErrorClass` (from the error taxonomy architecture) and walks the `onClass` list:

```yaml
single:
  primary: { provider: openai, model: gpt-4o }
  fallback:
    - { provider: anthropic, model: claude-3-5-sonnet, onClass: [5xx, timeout, rate_429] }
    - { provider: openai, model: gpt-4o-mini, onClass: [5xx, timeout] }
```

Each fallback entry fires only when the upstream error matches one of the listed error classes. Credential-pool health also feeds the chain: if a (provider, model) has crossed the failure threshold, the executor short-circuits to the fallback without attempting the primary.

Every fallback attempt is recorded in `traffic_event.routing_trace` (JSONB), and the CP UI "Routing trace" panel renders the strategy path for any given `request_id`.

## Creating and updating rules

The admin workflow at `/ai-gateway/routing` (`admin:routing-rule.read` / `admin:routing-rule.write` IAM actions):

1. Navigate to Routing Rules → "New Rule" in the Control Plane UI.
2. Choose a strategy type and set match conditions.
3. Configure the strategy tree (targets, weights, fallback `onClass` entries).
4. Submit. The admin guard validates the rule synchronously.
5. The Control Plane persists the rule, signals Nexus Hub, which signals the AI Gateway.
6. The AI Gateway swaps its in-memory routing rule set atomically. In-flight requests complete on the old set; new requests see the new rule immediately.

## Admin guard

The Control Plane validates every rule before persisting it. A rule is rejected if:

- Any referenced `(provider, model)` pair does not exist in the catalog.
- A `policynarrowing` filter produces an empty effective set.
- Match conditions can never overlap with any currently enabled model (the "zero-match" case).

The guard enforces correctness at rule creation time, not at request time. This prevents the failure mode where a rule is accepted by the UI but never matches any real request — a pattern that had been the cause of multiple support escalations before the guard was introduced.

## Routing trace

Every request records the routing evaluation in `traffic_event.routing_trace` (JSONB):

```json
{
  "rule_id": "rule-abc",
  "strategy_path": ["Conditional[orgMatch]", "Fallback[head]", "Single[openai/gpt-4o]"],
  "smart_routing": {"chosen": "anthropic/claude-3-5-sonnet", "confidence": 0.91},
  "policy_narrowed_from": [...],
  "policy_narrowed_to": [...],
  "fallback_attempts": [...]
}
```

The CP analytics UI renders this trace inline on the traffic event detail panel. To query it directly in development:

```sql
SELECT routing_trace FROM traffic_event WHERE request_id = '<id>';
```

## Interaction with the kill switch

When the Hub-pushed kill switch is active at the org, provider, or route level, the routing engine still computes the route but stamps `BypassHooks = true` on the `ResolvedRequest`. The executor forwards the request without running hooks. The audit record always captures `bypass_hooks = true` and `bypass_reason`, regardless of the bypass state.

---

## Canonical docs

- [`routing-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/routing-architecture.md) — Strategy tree, match conditions, canonical payload, admin guard, failure modes
- [`routing-rule-lifecycle.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/flows/routing-rule-lifecycle.md) — End-to-end flow: admin → CP → Hub → AI Gateway → traffic event
- [`ai-gateway.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/cp-ui/ai-gateway.md) — Admin UI surface: Routing Rules page, common workflows, key API endpoints

**Adjacent wiki pages**: [AI Gateway Overview](AI-Gateway-Overview) · [AI Gateway Smart Routing](AI-Gateway-Smart-Routing) · [AI Gateway Hooks](AI-Gateway-Hooks) · [Control Plane Infrastructure Pages](Control-Plane-Infrastructure-Pages) · [Feature Multi Provider Routing](Feature-Multi-Provider-Routing)
