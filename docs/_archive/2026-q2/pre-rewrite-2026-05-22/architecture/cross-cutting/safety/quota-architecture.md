---
doc: quota-architecture
area: cross-cutting
service: safety
tier: 1
updated: 2026-05-20
---

# Quota System Architecture

> **Tier 1 architecture doc.** Read this before touching `packages/ai-gateway/internal/policy/quota/`, `packages/nexus-hub/internal/quota/{store,rollup}/`, or the quota CRUD UI. Organisation hierarchy that determines parent-cap rollup lives in `tenancy-architecture.md`. The 429 response shape this engine produces is in `error-taxonomy-architecture.md`.

Quota enforces budgets at organization / project / virtual-key / user scope. The engine accumulates per-period cost (in cents) and tokens against the relevant ancestor chain and rejects, downgrades, notifies, or track-only-records based on each level's `enforcementMode`.

---

## 1. The quota model

Two Prisma models in `tools/db-migrate/schema.prisma`:

- **`QuotaPolicy`** (line 556) — the base policy. Carries `costLimitUsd`, `tokenLimit`, `enforcementMode` (`reject | downgrade | notify-and-proceed | track-only`), `alertThresholds`, `priority`, `enabled`, `periodType` (`daily | weekly | monthly`). Resolution is keyed on the target shape inside the engine's `policyCache`.
- **`QuotaOverride`** (line 578) — per-target overrides keyed by `(targetType, targetId)`. Lets an admin raise or lower the effective limit for a specific user / VK / project / org without editing the shared policy. Any subset of `costLimitCents`, `tokenLimit`, `enforcementMode`, `periodType` may be set; missing fields fall back to the matching `QuotaPolicy`. Overrides carry a free-text `reason` for audit.

The engine resolves the effective limit per request by reading the `QuotaPolicy` for the scope (and `vkType` for VK rows), then layering any matching `QuotaOverride` on top (`enforcement.go` `Check` resolution loop).

Limits are stored and accumulated as integer cents (`CostLimitCents`) rather than floating-point dollars; token limits are integer counts.

## 2. Period model

Periods are fixed calendar buckets, not sliding windows. `CurrentPeriodKey(periodType)` returns:

- `daily` → `YYYY-MM-DD` (UTC)
- `weekly` → `YYYY-Www` (ISO week)
- `monthly` → `YYYY-MM` (default)

Each `(targetType, targetId, periodKey)` triple has a running counter served from `usage_cache.GetUsage`. The cache is the live source of truth on the hot path; Hub's rollup view (`§7`) is advisory.

## 3. Hierarchical check chain

The `Engine.Check(ctx, chain, estimate, vkMeta)` API accepts a `[]CheckLevel` walked in the order the resolver passes — typically VK → project → org. For each level the engine:

1. Resolves the effective `(limitCents, enforcementMode, periodType)` from override → policy.
2. Reads the current period's usage via `usageCache.GetUsage`.
3. Decides per-level: allow / reject / downgrade / notify-and-proceed / track-only.
4. Tracks the most restrictive action across all levels (priority: reject > downgrade > notify-and-proceed > track-only > allow) and returns it as the `Decision`.

Parent caps **bind**: a request that would succeed against the project cap but exceeds the parent org cap is rejected at the parent level.

## 4. Estimate + reconcile pattern

The request carries a pre-routing `CostEstimate` (input cost + reserved response cost). `Engine.Check` evaluates the chain against `current + estimate`. After the response completes, the executor reconciles the counter to actual cost so over-reserved (or under-reserved) requests self-correct on the next call.

## 5. Enforcement gates

Quota gates fire pre-routing for each level in the chain (§3). The first level to land on `reject` short-circuits with a Nexus-side 429; `downgrade` rewrites the request to a cheaper model; `notify-and-proceed` lets the request through while emitting an alert-trigger event; `track-only` records usage without affecting the request.

## 6. Enforcement modes

Each policy / override carries an `enforcementMode`:

- `reject` — at the limit, requests fail with a Nexus-side 429.
- `downgrade` — at the limit, the engine substitutes a cheaper model from the chain's downgrade policy (`downgrade.go`).
- `notify-and-proceed` — usage continues; alert thresholds fire from `policy.alertThresholds`.
- `track-only` — usage is recorded for analytics; no enforcement action.

The most restrictive mode across the chain wins (priority order in §3).

## 7. Hub coordination

Per-Thing Redis counters are local to the AI Gateway instance running the request. For multi-instance deployments, the counters are shared via Redis (each AI Gateway connects to the same Redis cluster).

Hub maintains a periodic **aggregate** view in `packages/nexus-hub/internal/quota/store/` (with the rollup job in `packages/nexus-hub/internal/quota/rollup/`) for analytics and alerting:

- Per-policy current usage snapshot (sampled every N seconds).
- Per-policy projected exhaustion time (linear extrapolation).
- Alerts on `usage > threshold` (cross-ref `alerting-architecture.md`).

The aggregate is **advisory** — enforcement is always against the live Redis counters.

## 8. Emergency passthrough (E48) interaction

When the kill switch is active and the gateway is in passthrough mode (cross-ref `multi-endpoint-coordination-architecture.md` §4) with `BypassHooks` set, the executor skips the quota gate alongside hook enforcement. The request is **still recorded** in the audit pipeline with `passthrough_flags` populated so the bypassed window is auditable.

This is intentional — passthrough is for emergencies where enforcement isn't possible. The audit trail compensates by being complete.

## 9. Failure modes

| Failure | Behaviour |
|---|---|
| Usage store unreachable | Engine surfaces the read error to the caller; the executor's response policy decides fail-open vs fail-closed per deployment. |
| Quota policy mis-configured (negative limit) | Admin guard rejects on save. |
| Reconcile-miss after success | Counter slightly under-charged; self-corrects on the next request when the actual cost lands. |
| Period boundary | New `CurrentPeriodKey` starts a fresh counter; no overlap. |

## 10. Adding a new quota dimension

Checklist (if you wanted to add e.g. "per-model" quota):

1. Extend the chain shape in `enforcement.go` to include the new `CheckLevel`.
2. Extend the `policy_cache` resolver to find policies for the new target type.
3. Add the admin UI surface.
4. Add per-dimension reporting + analytics.
5. Add the alert rules (`alerting-architecture.md`).

## 11. Sources

- `packages/ai-gateway/internal/policy/quota/` — quota engine (chain, enforcement, downgrade, policy/usage caches; Lua scripts loaded from this package).
- `packages/nexus-hub/internal/quota/store/` — aggregate view of per-policy usage.
- `packages/nexus-hub/internal/quota/rollup/` — rollup job that populates the aggregate view.
- `packages/shared/storage/redisfactory/` — Redis client factory.
- `packages/shared/storage/configcache/` — config snapshot cache used by the policy resolver.
- `tools/db-migrate/schema.prisma:556` — `QuotaPolicy` model.
- `tools/db-migrate/schema.prisma:578` — `QuotaOverride` model.

## 12. Cross-references

- `tenancy-architecture.md` — ancestor path that drives rollup.
- `error-taxonomy-architecture.md` — Nexus-side 429 envelope.
- `provider-adapter-architecture.md` — token-stamping flow that feeds `tokens` unit.
- `alerting-architecture.md` — quota-usage alert rules.
- `multi-endpoint-coordination-architecture.md` — E48 passthrough interaction.
- `cache-multi-tier-architecture.md` — Redis counter tier.
