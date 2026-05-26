---
doc: emergency-passthrough-architecture
area: cross-cutting
service: safety
tier: 1
updated: 2026-05-20
---

# Emergency Passthrough Architecture (E48)

> **Tier 2 architecture doc.** Read when touching emergency-passthrough code paths in the AI Gateway, kill-switch shadow keys, or the `passthrough.expiry` Hub job. Sister doc: `kill-switch-architecture.md` (focused on the activation surface). Multi-endpoint context: `multi-endpoint-coordination-architecture.md` §4.

When the compliance pipeline can't run — hook outage, mass mis-config, dependency failure — emergency passthrough lets traffic flow **without enforcement** while the audit trail stays complete. Designed to be safe (auto-reverts, fail-closed cold-start, mandatory audit) so the cure doesn't outlast the disease.

---

## 1. Three tiers

| Tier | Scope | Schema table | Key |
|---|---|---|---|
| Global | All `/v1/*` traffic | `GatewayPassthroughConfigGlobal` | singleton row (`id="singleton"`) |
| Adapter | One adapter family (e.g., openai, anthropic) | `GatewayPassthroughConfigAdapter` | `adapter_type` |
| Provider | One specific provider row | `GatewayPassthroughConfigProvider` | `provider_id` (FK CASCADE to `Provider`) |

Anchor: `tools/db-migrate/schema.prisma` (`model GatewayPassthroughConfigGlobal/Adapter/Provider`). Tiers compose: if Global is active, Adapter / Provider tiers are moot. Tier resolution is per-request, evaluated most-broad to most-specific.

## 2. Three independent bypass flags per tier

Each tier carries an `enabled` boolean **and** a JSONB `config` blob with three independent flags:

```json
{ "bypassHooks": false, "bypassCache": false, "bypassNormalize": false }
```

| Flag | What it bypasses |
|---|---|
| `bypassHooks` | Request + response hook stages (the historic "kill the enforcement pipeline" knob) |
| `bypassCache` | Response cache reads + writes; every request goes to upstream |
| `bypassNormalize` | Wire-format normalization layer (canonical-bridge); raw provider bytes forwarded |

The three are orthogonal — admins can flip hooks off while keeping cache + normalize intact (or vice versa) per tier. There is no single "enabled = everything off" toggle; the row's top-level `enabled` only controls whether that tier participates at all.

Each row also carries `enabledBy`, `reason`, `expiresAt`, and `updatedAt` for audit + safety (§4).

## 3. Persistence model — DB rows + Cat A shadow

The passthrough state is **persisted in three DB tables** (one per tier) and propagated to data-plane Things as a Cat A inline shadow blob. The blob bundles the active rows from all three tables so each Thing has a single in-memory view it can consult per request — no Hub round-trip on the hot path.

Cat A inline (cross-ref `thing-config-sync-architecture.md` §3) means the change-signal carries the value; sub-second propagation across all Things. The DB is the source of truth; the shadow blob is a denormalized read view.

`GatewayPassthroughConfigProvider.providerId` is a **CASCADE FK** to `Provider.id` — deleting a Provider deletes its passthrough row, so an orphan row can't keep a deleted provider in bypass.

## 4. `ResolvedRequest` bypass carriers

The routing engine still resolves a `ResolvedRequest` for the request (so analytics + cost attribution still work). The engine reads the shadow blob and, if a matching tier is active, copies the three flags onto the resolved request:

```go
// from packages/ai-gateway/internal/execution/passthrough/cache.go
type ResolvedRequest struct {
    // ...
    BypassHooks     bool `json:"bypassHooks"`
    BypassCache     bool `json:"bypassCache"`
    BypassNormalize bool `json:"bypassNormalize"`
}
```

Composition is OR across the three tiers — Global, Adapter, and Provider rows contribute their flags into the resolved request via the merge in the same file. The tier-source attribution lands on the emitted `traffic_event` instead of on `ResolvedRequest`.

The executor then:

- Skips hook invocation when `BypassHooks` is set (request + response stages).
- Skips quota gates when `BypassHooks` is set (cross-ref `quota-architecture.md` §8).
- Skips cache lookup + write when `BypassCache` is set.
- Skips normalize when `BypassNormalize` is set (raw provider bytes pass through).
- Forwards to the upstream provider.
- Emits a `traffic_event` with `passthrough_flags` (canonical-order slice of `{bypassHooks, bypassCache, bypassNormalize}`) + `passthrough_reason` populated; all other fields normal.

The L4 view (provider, model, credential, RequestContext) stays accurate. We just skip enforcement on the bypassed layers.

## 5. Mandatory expiry

`expiresAt` is **bounded**:

- Max: 8 hours from now (`maxExpiry = 8 * time.Hour` in `packages/control-plane/internal/governance/passthrough/handler/handler.go`).
- The migration enforces the same ceiling (`expires_at <= NOW() + 8h`) at the DB level — see the doc-comment on `model GatewayPassthroughConfigGlobal` in `schema.prisma`.

The admin UI offers a short suggested window per activation and rejects manual input > 8h. The handler validates server-side. A persistent passthrough would defeat compliance; the expiry is non-negotiable.

## 6. Fail-closed cold-start

A Thing that boots fresh and finds NO shadow defaults to **enforced**, not passthrough. It waits for the shadow to load before serving traffic. This prevents a Hub-down boot from silently disabling enforcement.

The "fail-open at the proxy level" pattern (`compliance-pipeline-architecture.md`) is about hook failures; this is about config absence. They're different fail modes.

## 7. Hub expiry-reconcile loop

The `passthrough.expiry` Hub job (`packages/nexus-hub/internal/jobs/defs/expiry/passthrough_expiry.go`) runs on a 60s tick (`60 * time.Second`, configurable for tests). On each tick it scans the three tier tables for rows where `enabled=true AND expires_at < now()`, flips `enabled=false`, advances `updated_at`, and emits an audit event so the admin-UI history records the auto-revert.

After expiry the shadow flips back to enforced, change-signal fires, all Things re-pull, enforcement resumes. **Admin forgetting to revert is fine** — Hub does it.

Belt-and-suspenders: even before the job ticks, the AI Gateway's `passthrough.Snapshot.active()` filters tiers with `expires_at < now()` at lookup time — so the runtime kill-switch is structurally bounded by `expires_at` regardless of when the job runs.

## 8. Mandatory audit trail

Every bypassed request emits a `traffic_event` with `passthrough_flags` + `passthrough_reason` populated. Hook decisions are absent (the hooks didn't run) but everything else (routing, cost, latency phases) is present. The DB CHECK constraint requires `passthrough_reason` ≥ 20 chars when any tier is enabled.

Activation emits an admin-audit row against the `passthrough` resource (verb `emergency-enable`). Auto-revert emits a system-actor audit row. Both land in `admin_audit`. This is non-optional — compliance review of passthrough windows depends on the audit being complete.

## 9. Alerting integration

The activation + auto-revert events land on the admin-audit stream (§8). A dedicated aggregator-driven alerting rule for the passthrough lifecycle is not wired today; SIEM / ops dashboards typically watch the audit stream directly.

## 10. UI surface

Admin → CP UI → `/ai-gateway/passthrough` (`docs/users/features/cp-ui/ai-gateway.md`) + `/infrastructure/kill-switch` (`docs/users/features/cp-ui/infrastructure.md`). Both pages show:

- Currently-active tiers + scope + expiry countdown.
- Activation history with `reason` per row.
- Reset button (manual revert).

## 11. Failure modes

| Failure | Behaviour |
|---|---|
| Hub down during activation | CP UI surfaces; local kill switch (compliance-proxy `runtimeapi`) is the per-instance fallback. |
| Thing slow to apply | Change-signal fans out in seconds; apply can take a few more; UI shows expected propagation window. |
| Expiry job stalled | `job.stalled` alert on `passthrough.expiry`; manual revert via UI. |
| Drift (Thing reports `bypassHooks=true` after admin reverted) | Drift alert fires. |
| Shadow corrupt | Cat A keys validate server-side; corrupt write rejected at Hub. |

## 12. Sources

- `packages/ai-gateway/internal/routing/` — `ResolvedRequest` resolution.
- `packages/ai-gateway/internal/execution/passthrough/` — `BypassHooks` / `BypassCache` / `BypassNormalize` carriers + cache integration.
- `packages/ai-gateway/internal/execution/executor/` — bypass-aware dispatch.
- `packages/nexus-hub/internal/jobs/defs/expiry/passthrough_expiry.go` — `passthrough.expiry` 60s auto-revert job.
- `packages/control-plane/internal/governance/passthrough/handler/handler.go` — admin API + `maxExpiry` constant.
- `tools/db-migrate/schema.prisma` — `model GatewayPassthroughConfigGlobal/Adapter/Provider` and CHECK constraints.

## 13. Cross-references

- `kill-switch-architecture.md` — activation surface mechanics.
- `multi-endpoint-coordination-architecture.md` §4 — full end-to-end flow.
- `routing-architecture.md` §9 — engine integration.
- `quota-architecture.md` §8 — quota bypass behaviour.
- `alerting-architecture.md` — activation alert.
- `audit-pipeline-architecture.md` — audit row shape.
