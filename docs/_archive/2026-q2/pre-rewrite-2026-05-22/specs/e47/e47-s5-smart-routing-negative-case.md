# E47 S5 — Smart routing negative-case short-circuit

**Epic:** E47
**Requirements:** [e47-routing-canonical-payload.md](../../../../docs/developers/specs/e47/e47-routing-canonical-payload.md) — Must M5, Should S2
**OpenAPI:** none (internal logic; admin-API contract unchanged)
**Status:** Approved (user-approved plan 2026-05-13)
**Date:** 2026-05-13
**Builds on:** [e47-s4-router-llm-client-interface.md](e47-s4-router-llm-client-interface.md)

---

## Architecture summary

Post-S2 the smart strategy reads `rctx.Request.Messages`, filters for `role=user`, and hands the slice to `routerllm.Decider.Decide`. When the filter produces zero messages — because the request payload is non-AI (e.g. `/v1/models`), normalize failed, or the request legitimately has no user role (assistant-only resume, tool-only payload) — Decide still gets called with an empty UserMessages slice, the router LLM is invoked with system-only messages, the upstream codec rejects (Anthropic) or silently degrades (OpenAI), and only then does the strategy reach `smartFallback`.

S5 closes that gap with two short-circuits inserted between the user-message filter and the Decider call:

1. **Non-AI payload guard** — if `rctx.Request == nil` or `!rctx.Request.Kind.IsAI()`, emit trace `"request payload not normalizable for smart routing; using default"` and fall back. This catches `/v1/models`-style traffic that happens to match a smart rule with broad `matchConditions`, plus normalize-failure paths where the payload kind is `unsupported`.
2. **Empty-user-content guard** — if the filtered `userMsgs` slice is empty (zero `role=user` entries), emit trace `"smart routing: no user content in request; using default"` and fall back. This catches AI-shape payloads whose roles are all `system` / `assistant` / `tool`, and the legitimate "operator forgot to send messages" case the filed bug report singled out for `model=auto` with empty body.

Neither short-circuit invokes the router LLM, eliminating both the wasted upstream cost and the codec-level error that originally surfaced this bug class on Anthropic-shape routers. The router LLM call still happens for the common case where the strategy has user content to ask about.

### Trace vocabulary

The two new decision strings are exact-match grep targets for operators triaging audit rows. They are distinct from any pre-S5 trace string the smart strategy emits, so an SQL filter for either substring isolates exactly the requests that hit the negative-case fall-through:

```sql
SELECT timestamp, model_name, routing_trace->'trace'->0->>'decision' AS first_decision
  FROM traffic_event
 WHERE routing_trace::text LIKE '%request payload not normalizable for smart routing%'
    OR routing_trace::text LIKE '%smart routing: no user content in request%'
 ORDER BY timestamp DESC LIMIT 50;
```

### Why two strings, not one

The two conditions reach the same outcome (fall back to default) but have different root causes:

- **Not normalizable** points to ingress / wire-format / non-AI traffic — the operator's question is "why is `/v1/models` hitting my smart rule?" and the fix is to narrow `matchConditions`.
- **No user content** points to AI traffic from a client that genuinely doesn't have user content — the operator's question is "why is this client calling smart-routed `model=auto` with an empty body?" and the fix is client-side.

Collapsing them into one string forces operators to substring-match on `routing_trace` to triage, which doesn't scale. The two-string design lets a dashboard count each cause separately.

### Decision: Decider stays naive

S5 keeps the responsibility of "should we even ask?" inside the strategy, not inside the Decider. The Decider's contract is "given user messages, decide" — empty-user-messages is a strategy-level policy. This preserves the option of a future Decider (local classifier, rule engine) that legitimately accepts empty user content for some predicate.

---

## Story

### S5 — Smart strategy negative-case short-circuit

**User story:** As a gateway operator, when smart routing fires on a request with no normalisable user content, I want the strategy to fall back to the configured default immediately with an explicit trace string telling me which class of negative case fired — so I can tell at a glance whether the operator-configurable knob (matchConditions narrowing) or the client (missing messages) is at fault.

**Tasks:**

- **T5.1** — `packages/ai-gateway/internal/router/strategy_smart.go`:
  Insert two `if` blocks between the user-message filter and the `s.deps.RouterLLM.Decide` call.
  - First block: `if rctx.Request == nil || !rctx.Request.Kind.IsAI()` → append TraceEntry with decision `"request payload not normalizable for smart routing; using default"` → return smartFallback.
  - Second block: `if len(userMsgs) == 0` → append TraceEntry with decision `"smart routing: no user content in request; using default"` → return smartFallback.
  - Update the comment block above the filter to describe the new short-circuit paths.

- **T5.2** — `packages/ai-gateway/internal/router/strategy_smart_test.go`:
  Add two new tests:
  - `TestSmart_NoNormalizedPayload_FallsBackWithExplicitTrace` — `rctx.Request == nil`. Assert: decider.calls == 0; trace contains the "not normalizable" string.
  - `TestSmart_NonAIKindPayload_FallsBackWithExplicitTrace` — `rctx.Request.Kind = KindHTTPJSON` (non-AI). Same assertions.
  - `TestSmart_NoUserContent_FallsBackWithExplicitTrace` — `rctx.Request.Kind = KindAIChat` with `Messages = []{Role: system}{Role: assistant}` (no role=user). Assert: decider.calls == 0; trace contains the "no user content" string.

- **T5.3** — Build and test:
  - `go build ./packages/ai-gateway/...`
  - `go test -race -count=1 ./packages/ai-gateway/internal/router/...`

**Acceptance:**

- The smart strategy calls `s.deps.RouterLLM.Decide` only when the canonical payload is AI-kind AND at least one role=user message survived the filter. Three unit tests pin the negative branches.
- Each negative trace string is distinct (`"request payload not normalizable for smart routing; using default"` vs `"smart routing: no user content in request; using default"`).
- The original bug-report's reproduction step 1-7 (verifying that `model=auto` with empty body falls back gracefully) now produces the "no user content" trace string instead of a codec error.
- No new TODO / FIXME / stubs in production code.

**Validation script:**

```bash
go test -race -count=1 ./packages/ai-gateway/internal/router/... -run TestSmart_
# Manual: send a smart-routed request with empty messages body and inspect
# the audit routing_trace's first entry; expect "no user content".
```
