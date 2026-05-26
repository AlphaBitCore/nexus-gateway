# Agent Policy Evaluation

*Audience: contributors working on `packages/agent/internal/policy/`; operators understanding why specific flows were or were not inspected.*

The agent makes three decisions for every intercepted flow before running the hook pipeline: is the destination in scope, is there an exemption, and which intercept mode applies. This pre-hook evaluation is a deliberate performance gate — hooks are expensive and skipping them for out-of-scope traffic keeps the forwarder fast. Admin-pushed exemptions and rule packs are both consumed locally on the device, pulling from the Hub-pushed config shadow.

---

## Evaluation order

For each intercepted flow:

```
Inbound flow: destination = api.openai.com
   │
   ▼
1. Domain matcher: does any interception_domain rule match?
   ✗ no  → relay unmodified; emit traffic_event with agent_decision="out-of-scope"
   ✓ yes → continue
   │
   ▼
2. Exemption check: is there an exemption for this destination?
   ✓ yes → relay unmodified; emit traffic_event with agent_decision="exempted"
   ✗ no  → continue
   │
   ▼
3. Intercept policy: which mode applies?
   - inspect     → bump TLS (Linux/Windows) and run the full hook pipeline
   - passthrough → relay; emit minimal traffic_event
   │
   ▼
4. Run pipeline per chosen mode.
```

Each branch emits a `traffic_event` so the audit trail is complete regardless of the decision. Admins reviewing why traffic was or was not inspected can query the `agent_decision` and `agent_decision_reason` fields.

## Data sources (Hub shadow)

All three decision inputs are Category B shadow keys, delivered via the Hub-centric pull-only config sync:

| Shadow key | Content |
|---|---|
| `interception_domains` | List of (domain pattern, intercept policy, hook scope) |
| `agent_exempt_hosts` | Admin-pushed exemption entries + denylist |
| `installed_rule_packs` | Rule pack IDs to inject into hook configs |

The compiled matcher state (glob, regex, IP-range structures) is built once at config-pull time and reused for every flow. An atomic-pointer swap replaces the state when the shadow changes, with no lock contention on the hot path. A typical agent evaluates millions of flows per day; domain match is O(1) average; exemption check is O(N) where N is typically under 100 entries.

## Exemptions

An exemption is a host-pattern entry that causes the agent to relay the matching flow without running the compliance pipeline. Exemptions are keyed on exact host (`api.openai.com`) or wildcard-suffix pattern (`*.openai.com`). Per-device scoping is handled at Hub (which controls which agents receive a given `agent_exempt_hosts` payload); on the device, every entry applies to every flow.

Two sources create exemptions:

| Source | Created by | Lifetime |
|---|---|---|
| `admin` | Admin push via `agent_exempt_hosts` Cat B shadow (CP UI → Compliance → Exemptions) | Permanent until admin removes |
| `auto` | Agent's own TLS-handshake failure tracker — 3 failures in 60 s auto-exempts the host | 24 h default |

User Protection Pause is not an exemption source. It toggles the kill-switch, which is a separate mechanism that short-circuits all flows to passthrough independently of any exemption entry.

The denylist field in `agent_exempt_hosts` lists hosts that may never be auto-exempted, even after N TLS failures. This is the admin's escape hatch for hosts that must always be inspected regardless of connectivity issues.

The `auto_exempt_cert_pinned` flag in the shadow payload is accepted and parsed but not yet consumed at runtime — auto-exemption based on cert-pin signals from the wire is a planned enhancement. Runtime auto-exemption today comes from the TLS-handshake failure tracker only.

## Rule packs

Rule packs are sets of reusable hook rules delivered via the `installed_rule_packs` shadow key. The agent's `AgentPipeline.ApplyRulePacksShadowState` consumes this key and calls `injectRulePacks`, which rewrites the cached `core.HookConfig` slice so each matching `HookConfig.Config` map carries `_rulePackInstalls`. The pipeline then runs against this enriched config via `NewRulePackEngine`.

Source: `packages/agent/internal/compliance/pipeline.go` — `ApplyRulePacksShadowState` and `injectRulePacks`.

## The Policies page

The agent UI's Policies page surfaces the current evaluation inputs as a read-only view for end users: active hooks, interception domains, exemption entries, rule packs, kill-switch status, and the current `agent_settings.trafficUploadLevel`. Changes always go through the admin in CP UI; the Policies page is informational only.

One gap remains: the `auto_exempt_cert_pinned` shadow knob is visible on the Policies page under Exemptions but is not yet enforced at hook-decide time. The page reflects what the admin configured; agents honor the admin + auto sources described above.

## Wiring in code

The evaluation chain is wired at agent boot:

- `policyEngine.SetExemptionStore(exemptionStore)` in `packages/agent/cmd/agent/wiring/compliance.go:57`
- `b.PolicyEngine.Evaluate(conn.DstHost)` called at `packages/agent/cmd/agent/wiring/bridge.go:145`

`core.Engine.Evaluate(host)` in `packages/agent/internal/policy/core/engine.go:79-89` calls `exemptionStore.Load().IsExempt(host)` first and returns `passthrough` on match, short-circuiting before the interception-domains lookup.

Shadow ingestion: `Store.ApplyShadowState` in `packages/agent/internal/policy/exemption/store.go:215-240` consumes the `{admin_exemptions, denylist}` fields from the `agent_exempt_hosts` Cat B shadow payload.

## Traffic event fields for policy decisions

Every flow emits a `traffic_event` record that captures the policy decision for audit purposes:

```json
{
  "agent_decision": "inspect",
  "agent_decision_reason": "matched rule X"
}
```

The `agent_decision` field takes one of four values:

| Value | Meaning |
|---|---|
| `inspect` | Flow was in scope, no exemption, TLS bump and hooks ran |
| `passthrough` | Flow was in scope with a `passthrough` intercept policy |
| `exempted` | Flow matched an admin or auto-exemption entry; relayed unmodified |
| `out-of-scope` | Flow destination did not match any interception domain rule |

These fields appear in the Traffic Event list in the CP UI and are queryable for forensic analysis. An admin who sees unexpected `out-of-scope` decisions can review the interception domains config in CP UI → Compliance → Interception Domains.

---

## Canonical docs

- [`agent-policy-eval-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-policy-eval-architecture.md) — Evaluation order, data sources, caching, local consumption status
- [`agent-exemption-grants-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-exemption-grants-architecture.md) — Exemption entry model, two sources, denylist, auto-pinning, wiring verification

**Adjacent wiki pages**: [Agent Overview](Agent-Overview) · [Agent Privacy Data Flows](Agent-Privacy-Data-Flows) · [Agent Enrollment Attestation](Agent-Enrollment-Attestation) · [AI Gateway Hooks](AI-Gateway-Hooks) · [Thing Model And Config Sync](Thing-Model-And-Config-Sync)
