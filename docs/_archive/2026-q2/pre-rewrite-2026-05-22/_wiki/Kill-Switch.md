# Kill Switch

*Audience: operators who need to activate, monitor, or investigate the kill-switch surface in the admin UI.*

The kill switch is the admin activation surface for emergency passthrough — the mechanism that lets traffic flow without enforcement when the compliance pipeline cannot run. Kill-switch state is stored as a Category A inline shadow key, so changes propagate to all affected services via Hub WebSocket within seconds. Three alert rules fire on the lifecycle: activation (critical), expiring-soon (warning), and auto-revert (info). The Hub reconcile job is the authoritative expiry gate — it auto-reverts passthrough windows regardless of whether the admin remembers to revert manually.

---

## IAM gating

The IAM resource is `passthrough` with three distinct verbs:

| Action | Grants |
|---|---|
| `admin:passthrough.read` | View current tier state and activation history |
| `admin:passthrough.write` | Edit passthrough config (provider lists, expiry policy) without flipping `enabled=true` |
| `admin:passthrough.emergency-enable` | Flip a tier to `enabled=true` — the actual kill-switch lever |

`emergency-enable` is intentionally separate from `write` so an admin can prepare the config ahead of time without holding the bigger lever. By default `emergency-enable` is granted to `NexusSuperAdmin` and `NexusAdmin`. The `NexusComplianceOfficer` role explicitly excludes it — kill-switch activation is an emergency operational decision, not a compliance one.

Attempting to access the passthrough API without `emergency-enable` returns a Hub-enforced 403 via `iamMW`.

## Activation workflow

Both UI surfaces lead to the same API path:

- **CP UI → AI Gateway → Passthrough** — primary surface, full tier control.
- **CP UI → Infrastructure → Kill Switch** — operator-facing duplicate, same API.

The admin chooses:
1. Tier (global / adapter / provider).
2. Scope (none for global; `adapter_type` string for adapter tier; `provider_id` FK for provider tier).
3. Which bypass flags to flip (`bypassHooks`, `bypassCache`, `bypassNormalize` — independently).
4. A free-text **reason** (required; empty reason is rejected at the CP handler).
5. Duration (max 8 hours, default 1 hour; UI and server both enforce the cap).

The CP handler (`packages/control-plane/internal/governance/passthrough/handler/handler.go`) performs IAM check → validates inputs → upserts the relevant `GatewayPassthroughConfig*` DB row → forwards to Hub → Hub updates the Cat A shadow blob → Hub signals affected Things.

## Shadow blob and propagation

The shadow blob bundles the active rows from all three tier tables into a single in-memory view that each Thing holds. Each Thing consults this blob per request — no Hub round-trip on the hot path.

Cat A inline delivery means the change-signal carries the value body; propagation to all Things is typically under 5 seconds. The DB row is the source of truth; the shadow blob is the denormalized read view.

## Activation history

There is no dedicated `kill_switch_activation` table. Current state lives in the three tier tables (`gateway_passthrough_config_global`, `_adapter`, `_provider`) — one row per scope, mutated in place on each activation or revert.

The full activation history lives in `admin_audit`. Every activation, edit, and revert emits an audit row with:
- `resource = passthrough`
- `verb` (`emergency-enable` / `write`)
- Actor `userId`
- Tier + scope
- `reason`
- Before / after snapshot of the row

The Hub auto-revert path emits with `system:passthrough.auto_reverted` as the actor.

The CP UI "History" tab queries `admin_audit` filtered to `resource = passthrough`, joined to user names. History depth equals the audit-retention horizon.

## Auto-revert (60-second reconcile)

The `kill_switch.reconcile` Hub scheduled job runs every minute:

1. Scan all three tier tables for `enabled=true` and `expiresAt < now`.
2. Set `enabled=false`, emit `system:passthrough.auto_reverted` audit row.
3. Signal all affected Things via Hub WS change-signal.
4. Things re-pull the shadow and resume enforcement.

Manual revert before expiry is the common path; the reconcile job is the safety net for forgotten windows.

## Alert rules

| Alert | Severity | Trigger |
|---|---|---|
| `kill_switch.activated` | critical | Any tier activation |
| `kill_switch.expiring_soon` | warning | 15 minutes before expiry |
| `kill_switch.auto_reverted` | info | Hub reconcile-driven revert |

The activation alert payload includes the tier, scope, reason, and expiry time so responders can click through to the UI.

## Cold-start safety

A Thing that boots with no shadow defaults to **enforced**. It cannot enter passthrough mode until the shadow loads successfully. This is the fail-closed cold-start invariant — a Hub-down boot does not silently disable enforcement. The kill switch can only be active after a successful shadow load.

## Tier overlap

When multiple tiers are simultaneously active:
- If Global is active, all traffic bypasses per the Global row's flags — Adapter and Provider rows are moot for matching traffic.
- Else, the most-specific matching tier's flags apply.
- Flags do not union across tiers; the broadest active tier wins for its traffic.

---

## Canonical docs

- [`kill-switch-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/kill-switch-architecture.md) — IAM gating, activation flow, history model, local override
- [`emergency-passthrough-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/emergency-passthrough-architecture.md) — runtime bypass mechanics, ResolvedRequest carriers, failure modes
- [`kill-switch-and-passthrough.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/flows/kill-switch-and-passthrough.md) — end-to-end flow with verification steps

**Adjacent wiki pages**: [Emergency Passthrough](Emergency-Passthrough) · [Fail Open Posture](Fail-Open-Posture) · [Configuration Architecture](Configuration-Architecture) · [Control Plane Infrastructure Pages](Control-Plane-Infrastructure-Pages) · [Hub Coordination](Hub-Coordination)
