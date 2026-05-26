# Emergency Passthrough

*Audience: operators who need to understand, activate, or investigate the emergency passthrough mechanism.*

Emergency passthrough is Nexus Gateway's safety valve for compliance pipeline failures. When hooks can't run — due to an outage, mass misconfiguration, or dependency failure — an admin can activate passthrough to allow traffic through without enforcement, while the audit trail remains complete. Three safeguards prevent the cure from outlasting the disease: mandatory expiry (maximum 8 hours), automatic Hub-driven revert, and fail-closed cold-start behaviour.

---

## Three tiers

Passthrough can be activated at three granularities:

| Tier | Scope | Schema table |
|---|---|---|
| Global | All `/v1/*` traffic | `GatewayPassthroughConfigGlobal` |
| Adapter | One adapter family (e.g., `openai`, `anthropic`) | `GatewayPassthroughConfigAdapter` |
| Provider | One specific provider row | `GatewayPassthroughConfigProvider` |

Tiers compose: if Global is active, Adapter and Provider rows are moot. Resolution is most-broad to most-specific — constant-time per request. The `GatewayPassthroughConfigProvider` row has a CASCADE FK to `Provider.id` — deleting a provider deletes its passthrough row, preventing orphan bypass state.

## Three bypass flags

Each tier carries three independent bypass flags in its `config` JSONB column:

| Flag | What it bypasses |
|---|---|
| `bypassHooks` | Request + response hook stages — the enforcement pipeline |
| `bypassCache` | Response cache reads + writes (every request goes upstream) |
| `bypassNormalize` | Wire-format normalization (raw provider bytes forwarded) |

The three are orthogonal. An admin can disable hooks while keeping cache and normalization intact, or disable all three. There is no single "everything off" toggle — each flag is explicit.

## Activation flow

```mermaid
sequenceDiagram
    participant Admin as Admin (UI)
    participant CP as Control Plane
    participant Hub as Nexus Hub
    participant Svc as AI Gateway / Proxy / Agent

    Admin->>CP: POST /api/admin/passthrough (tier, scope, flags, reason, expiry ≤ 8h)
    CP->>CP: IAM check (admin:passthrough.emergency-enable)
    CP->>CP: validate reason non-empty, duration ≤ 8h
    CP->>Hub: upsert GatewayPassthroughConfig{Global,Adapter,Provider}
    Hub->>Hub: update Cat A inline shadow blob
    Hub->>Svc: WS change-signal
    Svc->>Svc: ResolvedRequest.{BypassHooks,BypassCache,BypassNormalize} populated
    Svc->>Svc: serve traffic; emit traffic_event with passthrough=true + bypass_reason
```

End-to-end propagation is typically under 5 seconds. The DB row is the source of truth; the shadow blob is a denormalized read view for hot-path queries.

The `reason` field is required — activation without a reason is rejected. The reason becomes the `bypass_reason` field on every traffic event emitted during the bypass window.

## Mandatory expiry and auto-revert

`expiresAt` is bounded:
- Maximum: 8 hours from activation time.
- Default if unspecified: 1 hour.

The admin UI rejects input over 8 hours. The Hub server validates server-side as well. The Hub reconcile job (`kill_switch.reconcile`) runs every minute:

1. Scan all three tier tables for rows where `enabled=true` and `expiresAt < now`.
2. Set `enabled=false`, emit `system:passthrough.auto_reverted` admin-audit row.
3. Signal affected Things via WS change-signal.
4. Things re-pull the shadow; enforcement resumes.

Admin forgetting to revert is fine — Hub does it automatically.

## ResolvedRequest bypass carriers

The routing engine resolves a `ResolvedRequest` for every request even during passthrough, so analytics and cost attribution remain accurate. The engine reads the shadow blob and populates the bypass fields:

```go
type ResolvedRequest struct {
    // ... routing fields ...
    BypassHooks     bool
    BypassCache     bool
    BypassNormalize bool
    BypassReason    string  // "passthrough.global" | "passthrough.adapter.<name>" | ...
}
```

The executor skips hook invocation when `BypassHooks` is set, quota gates when `BypassHooks` is set, cache when `BypassCache` is set, and normalization when `BypassNormalize` is set. Every bypassed request still emits a `traffic_event` with `passthrough=true`, `bypass_reason`, and all routing/cost/latency fields populated.

## Fail-closed cold-start

A service that boots with no shadow defaults to **enforced**, not passthrough. It waits for the shadow to load before serving traffic. This invariant prevents a Hub-down boot from silently disabling the compliance pipeline.

## Audit trail

Every activation emits `admin:kill_switch.activated` to `admin_audit`. Every auto-revert emits `system:passthrough.auto_reverted`. Every bypassed request emits a `traffic_event` with `passthrough=true`. The audit trail is non-optional — compliance review of passthrough windows depends on completeness.

When any tier activates, an alert fires at `severity=critical` (configurable). A follow-up alert fires on auto-revert.

## Local per-instance override

When Hub is unreachable and a per-instance kill is needed immediately, each Compliance Proxy instance exposes:

```
POST /runtime/killswitch/local
```

This sets a process-local bypass on that instance only. It does NOT propagate via Hub, does NOT expire automatically, and is overridden by the next Hub shadow apply. Use sparingly; document the activation in the admin audit manually.

---

## Canonical docs

- [`emergency-passthrough-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/emergency-passthrough-architecture.md) — tier schema, bypass carriers, reconcile loop, failure modes
- [`kill-switch-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/kill-switch-architecture.md) — activation surface, IAM, audit history

**Adjacent wiki pages**: [Kill Switch](Kill-Switch) · [Fail Open Posture](Fail-Open-Posture) · [Control Plane Vs Data Plane](Control-Plane-Vs-Data-Plane) · [Configuration Architecture](Configuration-Architecture) · [Hub Coordination](Hub-Coordination)
