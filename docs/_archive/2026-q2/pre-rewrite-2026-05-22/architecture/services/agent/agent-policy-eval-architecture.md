---
doc: agent-policy-eval-architecture
area: service
service: agent
tier: 1
updated: 2026-05-20
---

# Agent Policy Evaluation Architecture

> **Tier 3 architecture doc.** Read when touching `packages/agent/internal/policy/`. The hook evaluation seam is in `hook-architecture.md` (Tier 1); this doc focuses on the agent-specific policy lookup that decides **whether** a flow even enters the hook pipeline.

---

## 1. The decision the agent makes

For every intercepted flow:

1. Is this destination **in scope** for inspection?
2. If yes, is there an **exemption** for this specific (device, destination, user) combo?
3. If still proceeding, **what intercept policy** applies (`inspect` or `passthrough`)?

The hook pipeline runs only after these three decisions resolve to "in scope, no exemption, intercept needed". Policy evaluation **before** hooks is a perf optimisation — hooks are expensive; skip them for out-of-scope traffic.

## 2. Data sources

The agent pulls from Hub shadow:

- **Interception domain rules** — list of (domain pattern, intercept policy, hook scope).
- **Exemption grants** — per-(domain × device × user) bypass rules.
- **Local policy overrides** — agent-side cached rules (rare; for advanced use).

All three are Cat B shadow keys (cross-ref `thing-config-sync-architecture.md` §3).

## 3. The evaluation order

```
Inbound flow: destination=api.openai.com
   │
   ▼
1. Domain matcher (shared/domain): does any interception_domain rule match?
   ✗ no  → relay unmodified; emit minimal traffic_event with "out-of-scope"
   ✓ yes → continue
   │
   ▼
2. Exemption check: is there an exemption for (device, this destination, user)?
   ✓ yes → relay unmodified; emit traffic_event with "exempted"
   ✗ no  → continue
   │
   ▼
3. Intercept policy: which mode applies?
   - `inspect`     → bump TLS and run the full hook pipeline
   - `passthrough` → relay; emit minimal traffic_event
   │
   ▼
4. Run pipeline per chosen mode.
```

Each branch emits a `traffic_event` so the audit trail is complete regardless of decision.

## 4. Caching

The matcher state (compiled glob / regex / IP-range structures) is built once at config-pull time and reused for every flow. Atomic-pointer swap on shadow change.

A typical agent evaluates millions of flows per day; the hot path is O(1) average for domain match + O(N) for exemption rule count (typically N < 100).

## 5. Local consumption status

Verified against current code:

- **Exemptions ARE consumed locally.** `core.Engine.Evaluate(host)` (`packages/agent/internal/policy/core/engine.go:79-89`) calls `exemptionStore.Load().IsExempt(host)` FIRST and returns `passthrough` on match — exempt traffic short-circuits BEFORE the hook pipeline. Wired in `packages/agent/cmd/agent/wiring/compliance.go:57` (`policyEngine.SetExemptionStore(exemptionStore)`) and invoked at `wiring/bridge.go:145` (`b.PolicyEngine.Evaluate(conn.DstHost)`).
- **Rule packs ARE consumed locally.** `AgentPipeline.ApplyRulePacksShadowState` (`packages/agent/internal/compliance/pipeline.go:279`) consumes the `installed_rule_packs` Cat B shadow key; `injectRulePacks` (pipeline.go:259, 341) injects `_rulePackInstalls` into each matching `HookConfig.Config` map before the pipeline runs, routing to `NewRulePackEngine`.

Remaining gap: the `auto_exempt_cert_pinned` knob on `exemption.Store.ApplyShadowState` (`store.go:223-224`) is accepted but not yet consumed at runtime — auto-exempt-on-cert-pin is still proxy-side only. The Policies page surfaces all three (admin exemptions, denylist, installed rule packs) as authoritative.

## 6. Tracing each decision

Every traffic event records the decision:

```json
{
  "agent_decision": "inspect",             // inspect | passthrough | exempted | out-of-scope
  "agent_decision_reason": "matched rule X" // for forensic clarity
}
```

Admins reviewing why traffic was / wasn't inspected can query these fields.

## 7. Cross-references

- `hook-architecture.md` — hooks that run AFTER policy eval passes.
- `agent-forwarder-architecture.md` §3 — pipeline overall.
- `agent-exemption-grants-architecture.md` — exemption mechanics.
- `domain-device-predicate-architecture.md` — matchers used here.
- `thing-config-sync-architecture.md` — shadow keys consumed.
