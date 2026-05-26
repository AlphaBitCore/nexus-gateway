---
doc: agent-exemption-grants-architecture
area: service
service: agent
tier: 1
updated: 2026-05-20
---

# Agent Exemption Grants Architecture (E20)

> **Tier 3 architecture doc.** Read when touching exemption CRUD, pinning-driven auto-exemption, or the exemption UI. Local-side consumption is now wired (verified 2026-05-21) — `core.Engine.Evaluate` short-circuits exempt hosts to passthrough before the hook pipeline. The one remaining gap is the `auto_exempt_cert_pinned` knob, which is accepted via shadow but not yet consumed at runtime.

---

## 1. What's an exemption

A row that says "this destination BYPASSES the normal compliance pipeline on this device". The exemption is keyed on host pattern (exact host or `*.suffix.com` wildcard); per-user / per-device scoping is not modelled in the agent today.

Real `Entry` struct (`packages/agent/internal/policy/exemption/store.go:42-47`):

```go
type Entry struct {
    Host      string
    Reason    string
    Source    Source     // "auto" | "admin"
    ExpiresAt time.Time  // zero = no expiry
}
```

There is no `id`, no `device_id` / `user_id` / `org_id`, no `applied_count`, no `created_at`. The store is in-memory only; it's not a row in the agent's local DB.

Exemptions are admin-managed in the canonical case; auto-applied exemptions come from the agent's own TLS-handshake failure tracker (`Store.RecordFailure`, `store.go:115-170` — 3 failures inside a 60s window auto-exempts the host for 24h). User-side Protection Pause does **NOT** create an exemption entry — it's a killswitch toggle (cross-ref `agent-protection-pause-architecture.md`).

## 2. Two sources

The agent's `Source` enum (`store.go:18-22`) is only two values:

| Source | Created by | Lifetime |
|---|---|---|
| `admin` | Admin push via `agent_exempt_hosts` Cat B shadow key (CP UI → Compliance → Exemptions → Hub publish) | Permanent until admin removes |
| `auto` | Agent's own TLS-handshake failure tracker — 3 failures in 60s auto-exempts the host | Bounded; default 24h (`DefaultConfig.ExemptionDurationSec = 86400`) |

User Protection Pause is NOT a third source — it does not create an exemption entry. See `agent-protection-pause-architecture.md`.

## 3. Scope

Exemption keys are **host patterns**: either an exact host (`api.openai.com`) or a wildcard-suffix pattern (`*.openai.com`). Wildcard-suffix is the only wildcard form supported (`store.go:105-115`).

There is no per-device, per-user, or per-org scoping in the agent — every entry applies to every flow this agent sees. Per-device scoping happens at the Hub when it decides which agents to publish the `agent_exempt_hosts` payload to.

The `denylist` field on `ApplyShadowState` (`store.go:222-225`) lists hosts that may NEVER be auto-exempted — even after N failures, the failure tracker refuses to create an entry. This is the admin's escape hatch for "no matter what, keep inspecting these hosts".

## 4. Evaluation

Cross-ref `agent-policy-eval-architecture.md` §3. The agent evaluates exemptions BEFORE hook pipeline. An exempted flow relays unmodified; audit row records `agent_decision=exempted`.

## 5. Local consumption status

Verified against current code:

- `core.Engine.Evaluate(host)` (`packages/agent/internal/policy/core/engine.go:79-89`) calls `exemptionStore.Load().IsExempt(host)` FIRST and returns `passthrough` on match.
- Wiring: `wiring/compliance.go:57` calls `policyEngine.SetExemptionStore(exemptionStore)`; bridge invokes evaluation at `wiring/bridge.go:145`.
- Shadow ingestion: `Store.ApplyShadowState` (`store.go:215-240`) consumes the `{admin_exemptions, denylist}` fields out of the `agent_exempt_hosts` Cat B payload.

Result: an admin who grants an exemption for `api.openai.com` via CP UI sees that host short-circuit to passthrough on the agent BEFORE the hook pipeline. Server-side enforcement is one of two enforcement paths.

**Remaining gap:** the `auto_exempt_cert_pinned` flag on `ApplyShadowState` is parsed but not yet consumed at runtime (`store.go:223-224`: "accepted but ignored pending a future pipeline wiring task"). The runtime auto-exemption today comes from `Store.RecordFailure` (TLS handshake failure tracker), not from a cert-pin signal off the wire.

## 6. Auto-pinning exemption flow

When the compliance proxy can't TLS-bump a destination (client pins the cert):

1. Proxy sees TLS Alert `bad_certificate` N times.
2. Proxy creates an auto-exemption row.
3. Future flows to that destination relay unbumped from the proxy side.

This is **proxy-side**. The agent doesn't see this directly (the agent isn't doing bump on macOS at all; on Linux / Windows the agent has its own pinning detector that mirrors this pattern).

## 7. Audit & alerting

- Every exemption create / update / delete emits `admin_audit` (cross-ref `admin-audit-log-coverage.md`).
- Auto-pinning exemptions emit `system:exemption.auto_applied` events.
- Alerting can fire on auto-exemption count spikes (often signals an upstream change that broke our bump).

## 8. User Protection Pause is NOT an exemption

Earlier docs framed Protection Pause as a temporary entry in this store. That model does not match the code: `lifecycle/protectionpause/pause.go` toggles the **killswitch**, not the exemption store. While paused, every flow on the device short-circuits to passthrough via the killswitch check in the connection bridge — independently of any exemption row. See `agent-protection-pause-architecture.md` for the actual mechanics.

The separation matters: revoking an admin exemption does not un-pause; ending a Pause does not remove admin exemptions.

## 9. Cross-references

- `agent-policy-eval-architecture.md` — evaluation order.
- `agent-forwarder-architecture.md` §7 — forwarder integration (note: any "binding gap" wording in that section is stale per the 2026-05-21 audit).
- `compliance-pipeline-architecture.md` §3 — proxy-side auto-pinning.
- `docs/users/features/cp-ui/compliance.md` — admin Exemptions surface.
- `docs/users/features/agent-ui/settings.md` — user-side Protection Pause.
- `docs/developers/specs/e20/e20-agent-tls-exemptions.md` — original requirements.
