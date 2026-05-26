---
doc: agent-protection-pause-architecture
area: service
service: agent
tier: 1
updated: 2026-05-20
---

# Agent Protection Pause Architecture

> **Tier 3 architecture doc.** Read when touching the user-side Protection Pause logic. Implementation lives in `packages/agent/internal/lifecycle/protectionpause/pause.go`. Protection Pause is built on top of the **killswitch**, not the exemption store — sister doc `agent-exemption-grants-architecture.md` covers the unrelated exemption surface.

---

## 1. What it is

A user-initiated, bounded temporary pause of agent interception. Triggered from the agent UI. Useful when a user is doing something the agent shouldn't intercept (a personal call, a one-off use that the admin policy doesn't yet cover).

Critically: it's **time-bounded** and **admin-controllable**. An admin can disable user-initiated Pause for org policy compliance.

## 2. The lifecycle

```
User clicks "Pause" → UI sends IPC StartProtectionPause(seconds) → daemon:
  Pauser.Pause(seconds) → killswitch.Toggle(false, actor="user-paused")
                       → resumesAt = now + seconds (or zero for indefinite)
                       → schedule one-shot auto-resume timer
   │
   ▼
Connection bridge already consults killswitch.IsEnabled() per flow;
no separate policy-snapshot update needed.
UI shows countdown timer (read from Pauser.ResumesAt())
   │
   ▼
[pause window: every intercepted flow short-circuits to passthrough]
   │
   ▼
At expiry OR user clicks Resume:
Pauser.Resume() (or autoResume() callback)
  → killswitch.Toggle(true, actor="user-resumed")
  → cancel pending timer
UI hides countdown
```

(Source: `packages/agent/internal/lifecycle/protectionpause/pause.go:30-35, 61-79`.)

## 3. Bounds (planned)

`Pauser.Pause(seconds int)` accepts any non-negative integer; **no admin-side max-duration bound is enforced in `pause.go` today**. A `maxProtectionPauseDuration` shadow key + 60-min default + 240-min max + 0-means-disabled policy is the planned admin control, but its enforcement (if any) would live in the IPC handler before it calls `Pauser.Pause`, not in this package. Treat the bounds described in feature docs as aspirational until the IPC validator lands.

`seconds == 0` means **indefinite pause** (no auto-resume timer scheduled) — not "disabled". Disable / re-enable of user-initiated pause is an admin policy that would gate the IPC handler itself.

## 4. Audit & visibility

What is wired today: every `Pause` / `Resume` writes an **actor string** into the killswitch snapshot (`"user-paused"` / `"user-resumed"`; `pause.go:30-35, 61-79`). Operators inspecting the killswitch snapshot can tell whether the device is paused locally (by the user) vs globally (admin shadow push, actor `"hub-shadow"`).

What is NOT wired today: distinct `agent.protection_pause.started` / `ended` / `denied` audit events. Searching the agent tree (`git grep "agent.protection_pause"`) returns no hits — those event names are aspirational. The structured audit-event emission (with duration, user_id, optional reason) is a planned addition; until it lands, the actor string + killswitch snapshot history is the available audit surface.

## 5. Why bounded

Without a bound, Pause becomes a permanent opt-out — defeating the agent's purpose. A bounded pause forces re-engagement; the user has to explicitly re-pause if they want longer.

The design intent (once §3's IPC-side validator lands) is a short default (single-digit minutes) with an admin-set hard cap — short enough to be intentional, long enough for a typical interactive scenario.

## 6. What Pause does NOT do

- Does **not** disable the agent daemon. The daemon keeps running.
- Does **not** disable Hub connectivity. The daemon still receives shadow updates.
- Does **not** persist across daemon restarts. The Pauser state lives in the killswitch + an in-memory timer (`Pauser.timer`, `Pauser.resumesAt`); both are lost on restart.
- Does **not** scope to a specific user. Pause toggles the device-wide killswitch — every flow on this device short-circuits to passthrough for the pause window, regardless of which OS user issued the request. (Per-user scoping would require an exemption-store integration that isn't wired today.)

The user sees in the UI: "Protection Pause active for HH:MM:SS more. Click Resume to end early."

## 7. Cross-references

- `agent-exemption-grants-architecture.md` — Pause is a temporary exemption.
- `agent-policy-eval-architecture.md` — exemption evaluation in the agent.
- `docs/users/features/agent-ui/settings.md` — user-facing surface.
- `audit-pipeline-architecture.md` — agent_audit events.
- `thing-config-sync-architecture.md` — agent shadow keys (the planned `maxProtectionPauseDuration` admin bound would land here).
