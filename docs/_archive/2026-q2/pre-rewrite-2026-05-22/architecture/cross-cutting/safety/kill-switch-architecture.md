---
doc: kill-switch-architecture
area: cross-cutting
service: safety
tier: 1
updated: 2026-05-20
---

# Kill Switch Architecture

> **Tier 2 architecture doc.** Read when touching kill-switch CRUD, admin handlers, or the kill-switch UI. Companion doc: `emergency-passthrough-architecture.md` (focused on the runtime bypass mechanics). The reconcile loop lives in `jobs-architecture.md` §7.

The kill switch is the **admin surface** for activating emergency passthrough. This doc covers the activation lifecycle: who can activate, how it propagates, how it's revoked.

---

## 1. Who can activate

The IAM resource is `passthrough` (declared in `packages/shared/identity/iam/catalog_data.go`), with three verbs: `read`, `write`, and `emergency-enable`. The canonical action strings (format `admin:<resource>.<verb>`) are:

- `admin:passthrough.read` — view current tier state + history.
- `admin:passthrough.write` — edit the passthrough config (provider lists, expiry policy) without flipping `enabled=true`.
- `admin:passthrough.emergency-enable` — the gate that actually flips a tier to `enabled=true`. Distinct from `write` so an admin can hold `write` (configure ahead of time) without holding the bigger lever.

By default `emergency-enable` is granted to `NexusSuperAdmin` and `NexusAdmin` — explicitly excluded from `NexusComplianceOfficer` (kill-switch activation is an emergency operational decision, not a compliance one).

A non-admin user attempting to access `/ai-gateway/passthrough` or `/infrastructure/kill-switch` is gated at the route level; clicking through the UI's API call returns 403 via `iamMW`.

## 2. The activation flow

```
Admin → CP UI: /ai-gateway/passthrough  OR  /infrastructure/kill-switch
   → choose tier (global / adapter / provider)
   → choose scope (none for global; adapter_type; provider_id)
   → choose which bypass flags to flip (bypassHooks / bypassCache / bypassNormalize)
   → reason (free text, REQUIRED)
   → duration (max 8h per `maxExpiry`; UI offers a short default)
   → submit
CP backend (packages/control-plane/internal/governance/passthrough/handler/handler.go):
   → IAM check (admin:passthrough.emergency-enable)
   → validate (tier+scope combination, duration ≤ 8h, reason non-empty)
   → upsert the corresponding GatewayPassthroughConfig{Global,Adapter,Provider} row
   → forward to Hub
Hub:
   → update shadow blob (Cat A inline) bundling the three tables
   → signal affected Things
Data plane:
   → receive change-signal
   → ResolvedRequest.{BypassHooks,BypassCache,BypassNormalize} populated per row
   → traffic_event stamps passthrough_flags (canonical-order slice of {bypassHooks,bypassCache,bypassNormalize}) + passthrough_reason
```

End-to-end propagation: typically < 5 seconds. The DB row is the source of truth; the shadow blob is a denormalized view (see `emergency-passthrough-architecture.md` §3).

## 3. Reason is REQUIRED

Activation without a reason is rejected. The free-text reason becomes the `bypass_reason` on every traffic event during the window — investigators reviewing the audit later can answer "why was enforcement off?" without guessing.

The reason is shown in the alert that fires on activation.

## 4. Manual revert

Admin → UI → "Revert" button → CP API DELETE → Hub flips shadow → signal → Things resume enforcement. Same propagation timing as activation.

Manual revert before expiry is the common case; the reconcile job is the safety net.

## 5. Activation history (reconstructed from admin audit)

There is **no dedicated `kill_switch_activation` table**. The three tier tables (`gateway_passthrough_config_global/adapter/provider`) carry only the *current* state — each row has `enabled / config / enabledBy / reason / expiresAt / updatedAt` and nothing else. When a tier is reverted, those columns are mutated in place; no per-activation history row is created.

The historical record lives in `AdminAuditLog`. Every activation, edit, and revert emits an admin-audit row with the resource type `passthrough`, the verb (`emergency-enable` / `write`), the actor's `userId`, the tier + scope, the `reason`, and the `before` / `after` snapshot of the row. The Hub auto-revert path emits a system-actor audit (`system:passthrough.auto_reverted`).

The UI's "History" tab queries `admin_audit` filtered to `resource = passthrough`, joined to user names. This means: history depth = audit-retention horizon (see `data-retention-purge-architecture.md`). Current state depth = always the live DB row.

Why no separate history table: the three tier tables are deliberately compact (one row per scope), and admin audit already carries the full before/after snapshot. A dedicated activation table would duplicate audit storage without adding query power.

## 6. Tier overlap

Tiers compose; the three tier tables are stored independently and the routing engine evaluates from most-broad to most-specific:

- If `gateway_passthrough_config_global.enabled=true`, all traffic bypasses (per the row's `bypassHooks/Cache/Normalize` flags), regardless of adapter/provider rows.
- Else, if `gateway_passthrough_config_adapter` has a row with `adapterType=openai` and `enabled=true`, all OpenAI-adapter traffic bypasses; non-OpenAI traffic enforced.
- Else, if `gateway_passthrough_config_provider` has a row with `providerId=<id>` and `enabled=true`, only that provider's traffic bypasses.

When multiple tiers are active simultaneously, the broadest tier's flags win for the matching traffic — they don't union or AND across tiers; the resolution is "first match from the top". This keeps the per-request decision a constant-time lookup.

## 7. Alerts

Every activation, edit, and revert emits an admin-audit row (`§5`). A dedicated kill-switch alerting rule set is not wired in the alerting pipeline today; downstream consumers (SIEM bridge, ops dashboards) typically build their own watchers off the `passthrough` admin-audit stream until aggregator-driven rules land.

## 8. Cold-start safety

`emergency-passthrough-architecture.md` §5 covers the binding: Things default to enforced, NOT passthrough, on cold-start with no shadow. The kill switch can only be in passthrough mode after a successful shadow load.

## 9. Persistence vs ephemeral

- Persistent: the three `gateway_passthrough_config_*` rows (only the *current* state) + the full historical audit trail in `admin_audit`.
- Ephemeral: the live shadow blob view (reset to enforced on revert / expiry, regenerated from the DB on Hub restart).

Querying "what was bypassed at time T" uses `admin_audit` filtered to `resource=passthrough`; querying "is anything bypassed right now" reads any of the three DB tables (or the shadow blob for read-fast).

## 10. Local override (compliance proxy runtime API)

In case Hub is unreachable and the admin needs a per-instance kill, each compliance-proxy instance exposes a break-glass runtime API endpoint:

```
PUT /runtime/config/killswitch
```

This sets a process-local kill via `runtime/config.ApplyBreakGlass`. It does NOT propagate via Hub — it's a same-instance escape hatch. The local kill is overridden by the next Hub shadow apply.

Use sparingly. Document the activation in the admin audit manually if used.

## 11. Sources

- `packages/control-plane/internal/governance/passthrough/handler/handler.go` — admin API.
- `packages/control-plane-ui/src/pages/ai-gateway/passthrough/PassthroughPage.tsx` — primary admin surface (under AI Gateway section).
- `packages/control-plane-ui/src/pages/infrastructure/kill-switch/InfraKillSwitchPage.tsx` — operator-facing duplicate surface (under Infrastructure section).
- `packages/nexus-hub/internal/jobs/defs/expiry/passthrough_expiry.go` — auto-revert job.
- `packages/compliance-proxy/internal/runtime/killswitch/` + `packages/compliance-proxy/internal/runtime/config/runtime_config.go` — local override (per-instance break-glass).
- `packages/shared/identity/iam/catalog_data.go` — `passthrough` resource + verb declarations.
- `tools/db-migrate/schema.prisma` — `model GatewayPassthroughConfigGlobal/Adapter/Provider`.

## 12. Cross-references

- `emergency-passthrough-architecture.md` — runtime bypass mechanics.
- `alerting-architecture.md` — alert rules.
- `audit-pipeline-architecture.md` — `admin_audit` row for activations.
- `iam-identity-architecture.md` — `admin:passthrough.*` actions (`read`, `write`, `emergency-enable`).
- `jobs-architecture.md` — `passthrough.expiry` auto-revert job.
- `multi-endpoint-coordination-architecture.md` §4 — end-to-end flow.
