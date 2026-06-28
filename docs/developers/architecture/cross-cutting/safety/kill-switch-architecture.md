# Kill switch architecture

The kill switch is the platform's last-resort "stop intercepting everything" lever. When an operator engages it, both Things that perform TLS bumping — the Compliance Proxy and the desktop Agent — drop into a transparent passthrough mode: TCP frames flow end-to-end, no MITM certificate is presented, no compliance hooks run, no payloads are captured. The Control Plane carries the toggle on a single boolean shadow key, fans it out through Hub to every receiver, and records the flip on the admin audit log. The toggle is binary, fleet-wide, immediate, and persisted on the Hub shadow — there is no per-rule scoping, no auto-revert timer, and no client-side override.

This is the brake for "a hook is rejecting healthy traffic" or "TLS bump is breaking a critical vendor". For surgical bypass of a single AI Gateway layer (cache, hooks, normalize) against a single provider, see `emergency-passthrough-architecture.md`; that subsystem is independent, lives only inside the AI Gateway, and never touches TLS interception.

Anchor packages:

- `packages/shared/schemas/configkey/configkey.go` — registers the `killswitch` shadow key.
- `packages/shared/schemas/configtypes/interception/killswitch.go` — the wire shape `{engaged: bool}`.
- `packages/control-plane/internal/governance/killswitch/handler/` — admin API + Hub fan-out.
- `packages/compliance-proxy/internal/runtime/killswitch/` — proxy-side receiver + history.
- `packages/compliance-proxy/internal/proxy/forward/forward.go` — bump-bypass gate.
- `packages/agent/internal/lifecycle/killswitch/` — agent-side receiver.
- `packages/agent/cmd/agent/wiring/bridge.go` — agent connection bridge passthrough gate.
- `packages/shared/identity/iam/catalog_data.go` — IAM verbs.
- `packages/shared/policy/pipeline/audit_emitter.go` — per-connection bypass audit event.

## 1. Shadow contract

The kill switch is stored as a Category A inline `thing_config_template` row keyed by `config_key = "killswitch"`. The template state is the canonical Go struct:

```go
type Killswitch struct {
    Engaged bool `json:"engaged"`
}
```

`Engaged = true` means the kill switch is **engaged**: TLS bump must stop on this receiver. `Engaged = false` (the fail-safe default) means normal operation — bump is active. The wire semantic is identical on both `compliance-proxy` and `agent` template rows; a single CP toggle writes both legs.

The configkey constant is declared in the shared registry under Type A keys ("config blob — state IS the config"), alongside `log_level` and `ai_guard`. There is no parallel `system_metadata` row; the template state IS the config and receivers read it directly off the shadow tick.

## 2. Admin API

Kill-switch admin uses a single dedicated write endpoint plus the generic config-sync surface for reads:

| Method | Path | IAM action | Body / Query | Response |
|---|---|---|---|---|
| POST | `/api/admin/compliance/killswitch` | `admin:kill-switch.toggle` | `{engaged: bool}` (required) | `{engaged, version, thingsNotified, thingsOnline}` |
| GET | `/api/admin/nodes?type=compliance-proxy` and `?type=agent` | `admin:node.read` | — | per-node `appliedConfig.killswitch.engaged` |
| GET | `/api/admin/config-sync/history?nodeType=…&configKey=killswitch` | `admin:settings.read` | `nodeType`, `configKey`, optional `page`/`pageSize` | newest-first toggle history |

The POST lives on a dedicated `/compliance/killswitch` route (not the generic `/api/admin/config-sync/update`) because it owns three things the generic surface cannot: (a) atomic fan-out across both compliance-proxy AND agent template rows in one call, (b) a dedicated `kill-switch.toggle` admin-audit action label the SIEM bridge keys off, and (c) the narrow `admin:kill-switch.toggle` IAM verb (granting "manage hooks" must not implicitly grant "engage the kill switch").

POST validates the body (`engaged` must be present — explicit `false` is required to disengage), authenticates the actor from the admin auth middleware, then performs the fan-out described in §3. CP never writes the template directly — Hub owns the `thing_config_template` UPSERT, the per-leg `config_change_event` row, and the WebSocket broadcast.

Read-side surfaces share the generic config-sync API because the same UI already binds those endpoints for every other admin config key. Operators see the live per-node applied state through `listNodes` (one row per compliance-proxy and agent, each carrying `appliedConfig.killswitch.engaged`) and the unified toggle history through `listConfigHistory` (two calls — one per node type — merged client-side on `createdAt`).

## 3. Hub fan-out

CP's handler fans out across the canonical Thing-type list: `["compliance-proxy", "agent"]`, in that order. Compliance-proxy is the primary leg because browser-side AI traffic exits through it; agent follows so a partial Hub failure on the agent leg still gets the compliance-proxy fleet into a safe state.

Each iteration calls `Hub.NotifyConfigChange{ThingType, ConfigKey: "killswitch", State: {engaged}, Action: "engage"|"disengage", ActorID, ActorName, SourceIP}`. Hub then:

1. UPSERTs `thing_config_template` for `(thingType, "killswitch")`, bumping the version.
2. Inserts a `config_change_event` row (in the same transaction).
3. Broadcasts the desired state via WebSocket to every connected Thing of that type.

Failure semantics differ by leg:

- **Compliance-proxy leg fail** → HTTP 502 `HUB_UNAVAILABLE`. The admin sees a real error and retries; the agent leg has not been touched yet.
- **Agent leg fail** → log + continue; the primary response is still returned with the compliance-proxy counts. The Hub drift reconciler re-pushes on its next tick. Rolling back compliance-proxy because agent failed would leave the fleet in a worse state than the partial fan-out.

The response carries `thingsNotified` + `thingsOnline` summed across both legs so the UI can show "engaged on 7 of 8 nodes online" without a second round-trip.

CP itself writes the admin audit row via `audit.EntryFor(c, ResourceKillSwitch, VerbToggle)` with `AfterState = {engaged, version, intent: "engage"|"disengage"}`. Hub's per-leg `config_change_event` carries the technical change; CP's audit row carries the actor + intent. Both flow into the SIEM bridge under the stable action label `kill-switch.toggle`.

## 4. Compliance Proxy receiver

The receiver lives in `packages/compliance-proxy/internal/runtime/killswitch/killswitch.go`. The atomic `IsEngaged()` accessor is read once per CONNECT on the hot path, lock-free.

Shadow application is wired through `configdispatch.registerKillSwitch`: the handler decodes the `{engaged}` JSON, calls `KillSwitch.Toggle(v.Engaged, "hub-shadow")` when the value differs from the live state, then reports the **live** snapshot back to Hub. Echoing the live state (rather than the desired one) is deliberate — a local rejection or a lagging shadow tick must surface the actually-applied state, otherwise the Nodes page would show a false "in sync".

`Toggle` updates the atomic flag, stamps `lastChanged` + `changedBy`, and appends a `KillSwitchHistoryEntry` to a bounded in-memory ring (capacity 100, not persisted across restart — durable history lives on the Hub side). Two `changedBy` values are canonical: `"hub-shadow"` for normal Hub-driven flips and `"break-glass"` for the runtime API path.

Once engaged, the bump bypass fires inside `forward.Run`:

```go
if cfg.KillSwitchChecker != nil && cfg.KillSwitchChecker() {
    logger.Warn("TLS bump disabled via kill switch, using passthrough")
    metrics.PinningPassthroughTotal.With("BUMP_DISABLED_EMERGENCY").Inc()
    cfg.AuditEmitter.EmitKillSwitchPassthrough(cfg.SourceAddr, cfg.TargetHost)
    tlsbump.PassThrough(ctx, conn, cfg.TargetHost)
    return
}
```

The result is a raw TCP relay between client and origin: no certificate is minted, no compliance pipeline runs, no payload is captured, no normalization happens. The CONNECT still succeeds and the client sees a working HTTPS tunnel — it just isn't inspected. This is the entire point: a regression in a downstream hook cannot block traffic when the switch is engaged.

`KillSwitch` also carries a `ForceClose(changedBy)` method that drops bumped connections currently in flight via a callback `forceCloseFn` wired in `main.go`. Engaging the switch via the shadow does **not** automatically force-close in-flight bumped connections — those drain naturally. Force-close is a separate operator action invoked from the runtime API for incidents that need an immediate cut.

## 5. Agent receiver

The agent receiver lives in `packages/agent/internal/lifecycle/killswitch/killswitch.go`. The internal `engaged atomic.Bool` matches the wire and the compliance-proxy receiver verbatim — `engaged=true` means the switch is engaged, the bridge must passthrough. No inversion lives in the agent runtime or in `bridge.go`; both sides of the fleet read the same boolean with the same meaning. The connection bridge consults the state directly:

```go
func (b *ConnectionBridge) IsKillSwitchEngaged() bool {
    return b.KillSwitch != nil && b.KillSwitch.IsEngaged()
}
```

`ApplyShadowState` ignores an empty / `null` raw payload — an initial shadow tick before Hub aggregation must not flip the switch. Subsequent ticks compare the desired `engaged` flag against the atomic state and only `Toggle` on change, so an idempotent re-push from Hub is a no-op.

When the switch is engaged, `HandleConnection` short-circuits before any policy or hook evaluation:

```go
if b.KillSwitch != nil && b.KillSwitch.IsEngaged() {
    b.recordKillSwitchPassthrough(conn.DstHost, conn.FlowID)
    return platform.DecisionPassthrough
}
```

The bridge skips the domain engine, the policy engine, the connection-stage compliance pipeline, and the platform MITM relay. `DecisionPassthrough` returns control to the platform NE/proxy adapter, which forwards the connection unmodified.

A throttled INFO log fires per destination host at most once per minute (`recordKillSwitchPassthrough`) so a busy fleet does not flood the audit pipeline while the switch is engaged. The same `IsKillSwitchEngaged()` hook is consulted by the macOS NE plumbing before tracking a flow in `activeFlows`, so the proxy provider can fail-open at the OS layer too.

## 6. Break-glass: runtime API fallback

Compliance Proxy and (selectively) other receivers ship a token-authenticated `PUT /runtime/config/{key}` endpoint that lets an operator flip the kill switch when Hub is unreachable. The handler:

1. Authenticates the bearer token from `COMPLIANCE_PROXY_API_TOKEN`.
2. Calls `KillSwitch.ApplyBreakGlass(ks)` with `changedBy = "break-glass:<token-id>"`.
3. Appends a `BreakGlassEvent` JSONL line to `break_glass_events.jsonl` under the data directory.
4. Reports the override to Hub so Hub adopts it as authoritative. Delivery uses a **dedicated break-glass wire**, not the normal shadow-report path: over WebSocket the client sends a `shadow_report_break_glass` frame; the HTTP fallback (and the deferred `ReplayPending` retry that drains `pending_break_glass.json` after an outage) POSTs to `/api/internal/things/shadow/break-glass`. Both carry the per-key version map plus the audit context (`reason`, `sourceIp`, `actorTokenId`). The route/frame type *is* the break-glass signal — Hub stamps the reconciliation marker itself rather than trusting a body-supplied `reason`, and the normal `/shadow` path rejects any `reason` so it cannot be used to smuggle a break-glass adoption.

### Hub-side adoption (authority gate + scope)

Hub does **not** blindly adopt whatever keys a break-glass report carries. Two server-side rules apply before any write:

- **Allowlist + schema validation.** Only `killswitch` and `exemptions` are writable via break-glass — the same set the data-plane client allows — and each reported state must decode cleanly into its canonical configtypes struct (`interception.Killswitch` / `identity.ActiveExemptions`) with no type mismatch and no unknown fields. A key outside the allowlist or a malformed state is **rejected** (400 on the HTTP route; rejected and logged on the WS path), never silently adopted. This keeps server-side privilege no broader than the only client that exercises it: a device/service token cannot rewrite an arbitrary `thing_config_template[type, *]` row through break-glass.
- **Per-Thing scope only — a node break-glass NEVER writes the fleet template.** Every key a node adopts via break-glass — `killswitch` included — is written as a **per-Thing desired override** on the reporting node ONLY; a node-initiated report cannot touch the type-level `thing_config_template` for any key. A node holds only its own device/service token, and a fingerprint/token match is not operator authority — letting one compromised node flip a *fleet-wide* safety control (engage killswitch → drop every Thing of the type into uninspected passthrough, or disengage an operator-set killswitch) is a cross-node blast radius an attacker would abuse. So a node may emergency-engage `killswitch` **on itself** (Hub-outage self-protection), but genuine fleet-wide killswitch stays EXCLUSIVELY on the operator admin endpoint `POST /api/admin/compliance/killswitch` (`admin:kill-switch.toggle` IAM verb). Every per-Thing write takes the same per-type advisory lock as the admin override writers so its `desired_ver` bump cannot collide with a concurrent type-fan-out, and emits a chain-hashed admin-audit row scoped to that Thing.
- **Per-Thing break-glass auto-revert.** A per-Thing break-glass override (now including a node's self-`killswitch`) is stamped with `expires_at = now + 8h` (matching the emergency-passthrough ≤8h bound — §9), so the override-expiry job reverts it automatically instead of leaving an emergency bypass as a permanent silent state nobody remembers to clear. The operator-driven fleet-wide killswitch (admin endpoint) keeps the no-auto-revert property — disengaging it stays an explicit operator action.

Both adoption paths are version-guarded: a stale report (an admin write already advanced the template/desired past the emergency point) is skipped rather than downgrading current state.

Break-glass is a fallback, not the normal path. The standard tooling is the CP admin endpoint described in §2 — that's the surface that fans out, audits, and persists durably. The runtime PUT is for the case where the operator's only reachable surface is the Thing itself (network partition, Hub outage, isolated dev box).

## 7. Observability

Compliance-proxy emits two primary signals when the switch is engaged:

- `pinning.passthrough_total{status="BUMP_DISABLED_EMERGENCY"}` — counter, incremented once per CONNECT routed to passthrough by the kill-switch gate. Shares the `pinning.passthrough_total` series with cert-pinning passthrough events, distinguished by the `status` label (other values: `BUMP_EXEMPT_CONFIGURED`, `BUMP_EXEMPT_PINNED`, `BUMP_FAILED_PASSTHROUGH`).
- `killswitch.active` — gauge `0|1`, mirrors the live `IsEngaged()` state. Set by the runtime API surface on every toggle.

Per-connection audit row (`AuditEmitter.EmitKillSwitchPassthrough`):

```
BumpStatus            = "BUMP_DISABLED_EMERGENCY"
TrafficSource         = "COMPLIANCE_PROXY"
IngressType           = "COMPLIANCE_PROXY"
RequestHookDecision   = "PASSTHROUGH"
RequestHookReason     = "kill switch engaged — TLS bump bypassed"
RequestHookReasonCode = "KILLSWITCH_ENGAGED"
```

This row is the audit equivalent of "we let this traffic through unverified" — it preserves the compliance trail even while the pipeline is bypassed. SIEM dashboards alarm on a sustained non-zero rate of this row because it means a fleet bypass is currently active and traffic is flowing un-inspected.

Agent-side observability is intentionally lighter: the throttled INFO log carries `event=killswitch_passthrough` with `host` and `flowId`. There is no per-flow audit row from the agent's bypass path because the bridge short-circuits before the audit pipeline is wired into the flow.

## 8. Recovery

Disengage is symmetric: `POST /api/admin/compliance/killswitch` with `{engaged: false}`. Hub re-pushes the shadow, each receiver toggles back, `killswitch.active` drops to 0, and the next CONNECT runs the full pipeline again. There is no auto-revert timer — the kill switch stays engaged until an operator explicitly disengages it. (Emergency passthrough overrides on the AI Gateway side, in contrast, carry a mandatory `ExpiresAt ≤ 8h` — that's a per-provider compliance lever; the kill switch is a fleet-wide cutoff and the operator is on the hook for disengaging it when the incident is over.)

In-flight bumped connections from before the disengage are unaffected; they continue under the policies that were live when they started. Force-closing them would punish active users for a state they didn't choose. Operators who want a clean cut can call `ForceClose` on each compliance-proxy node through the runtime API.

## 9. Relation to emergency passthrough

The two subsystems answer different questions and intentionally do not share state:

| Concern | Kill switch (this doc) | Emergency passthrough |
|---|---|---|
| Scope | Fleet-wide | Per provider / per adapter / global on the AI Gateway side |
| Receivers | Compliance Proxy + Agent | AI Gateway only |
| What it bypasses | TLS bump (entire compliance pipeline + payload capture) | Some combination of hooks / cache / normalize |
| Granularity | One bit | Three bypass toggles + 3-tier scope |
| Auto-revert | No | Mandatory `ExpiresAt ≤ 8h` |
| Shadow key | `killswitch` (Cat A inline) | `gateway_passthrough` (Cat A inline) |
| Audit action | `kill-switch.toggle` | `passthrough.emergency_enable` |
| IAM resource | `kill-switch` | `passthrough` |

Operators reach for the kill switch when interception itself is the problem (a hook is dropping healthy traffic at the proxy or agent layer, or TLS bump is breaking a critical vendor). They reach for emergency passthrough when one of the AI Gateway's L4 policy plane layers is the problem and they want to keep TLS bump + the rest of the pipeline running. The two can be active simultaneously without conflict.

## References

- `packages/shared/schemas/configkey/configkey.go`
- `packages/shared/schemas/configtypes/interception/killswitch.go`
- `packages/shared/identity/iam/catalog_data.go`
- `packages/shared/policy/pipeline/audit_emitter.go`
- `packages/control-plane/internal/governance/killswitch/handler/handler.go`
- `packages/compliance-proxy/internal/runtime/killswitch/killswitch.go`
- `packages/compliance-proxy/cmd/compliance-proxy/configdispatch/configdispatch.go`
- `packages/compliance-proxy/cmd/compliance-proxy/wiring/listener.go`
- `packages/compliance-proxy/internal/proxy/server/server.go`
- `packages/compliance-proxy/internal/proxy/forward/forward.go`
- `packages/compliance-proxy/internal/metrics/prometheus.go`
- `packages/compliance-proxy/internal/runtime/server/server.go`
- `packages/compliance-proxy/internal/runtime/breakglass/break_glass.go`
- `packages/agent/internal/lifecycle/killswitch/killswitch.go`
- `packages/agent/cmd/agent/configdispatch.go`
- `packages/agent/cmd/agent/configappliers.go`
- `packages/agent/cmd/agent/wiring/bridge.go`
- `packages/nexus-hub/internal/fleet/manager/override.go`
- `packages/control-plane-ui/src/pages/infrastructure/kill-switch/InfraKillSwitchPage.tsx`
- `packages/control-plane-ui/src/api/services/compliance/compliance.ts`
- `packages/control-plane-ui/src/hooks/usePermission.ts`
