# Emergency passthrough architecture

## 1. Intent

Emergency passthrough is the AI Gateway's runtime kill-switch system for selectively bypassing one or more compliance layers (hooks, response cache, normalize) for a chosen scope of traffic, under operator control, and only for a bounded time window. When a hook regresses on a specific provider, when normalize trips a parse loop on a new wire format, or when the cache pins a poisoned response, an operator can flip a tier on, name a reason, and route through with the offending layer skipped — without taking the rest of the fleet off the compliance path.

Two properties make this distinct from the global kill-switch:

- **Scoped**. Bypass applies per provider, per adapter family, or globally — not as a single fleet toggle. Three independent flags (`bypassHooks`, `bypassCache`, `bypassNormalize`) let an operator silence exactly the layer that is misbehaving.
- **Time-bounded**. Every tier write demands a future `expiresAt` no more than eight hours out, validated at the admin API and again as a DB CHECK. A Hub-side reconcile job auto-flips `enabled=false` on rows whose expiry has passed, so a forgotten passthrough does not become a permanent compliance gap.

See [kill-switch-architecture.md](./kill-switch-architecture.md) for the unscoped, indefinite global cutoff.

## 2. Anchor packages

| Path | Role |
|------|------|
| `packages/ai-gateway/internal/execution/passthrough/` | Effective `Config` + `Snapshot` + 3-tier merge `Cache` |
| `packages/ai-gateway/internal/policy/requestcontext/resolved.go` | `ResolvedRequest` wrapping L3 + route + effective passthrough |
| `packages/ai-gateway/internal/ingress/proxy/proxy.go` | Phase 4.5 resolve + Phase 5 / 5.5 / response bypass gates |
| `packages/ai-gateway/cmd/ai-gateway/configdispatch/configdispatch.go` | Receives `gateway_passthrough` shadow blob, drives `Cache.SetSnapshot` |
| `packages/control-plane/internal/governance/passthrough/handler/` | Admin REST endpoints, write validators, Hub propagation |
| `packages/control-plane/cmd/control-plane/wiring/reconcile.go` | CP-side config reconciler watch over `gateway_passthrough` |
| `packages/nexus-hub/internal/jobs/defs/expiry/passthrough_expiry.go` | Hub job that auto-reverts expired rows |
| `tools/db-migrate/migrations/20260517000000_e48_gateway_passthrough_config_3tier/migration.sql` | Three tier tables + `_effective` view + thing_config_template seed |
| `tools/db-migrate/migrations/20260517000010_e48_traffic_event_passthrough_columns/migration.sql` | `traffic_event.passthrough_flags` + `passthrough_reason` columns + partial index |
| `packages/control-plane-ui/src/pages/ai-gateway/passthrough/PassthroughPage.tsx` | Admin UI page |

## 3. The three tiers — scope of bypass

Passthrough is configured at three scopes, all carrying the same flag set. The runtime cache resolves a single effective `*Config` per `(provider_id, adapter_type)` pair by OR-merging active tiers.

| Tier | Table | Key | Scope of bypass | When to use |
|------|-------|-----|-----------------|-------------|
| Global | `gateway_passthrough_config_global` | singleton row `id = 'singleton'` | All gateway traffic regardless of provider | Cross-cutting layer outage (e.g. normalize broken for every adapter) |
| Adapter | `gateway_passthrough_config_adapter` | `adapter_type` (e.g. `anthropic`, `openai`) | All providers with that adapter family | Adapter-shape regression affecting many providers but not the whole fleet |
| Provider | `gateway_passthrough_config_provider` | `provider_id` (FK to `Provider`, ON DELETE CASCADE) | A single Provider row | Single upstream's wire response broke a specific hook |

The merge is strictly additive on flags and strictly conservative on expiry: any active tier turning a flag on turns it on in the effective config; among the active tiers, the tightest `ExpiresAt` wins. Attribution fields (`EnabledBy`, `Reason`) come from the most-specific active tier so the audit log records who triggered the bypass that actually ran. There is no per-routing-rule bypass — scope below the provider level is intentionally not exposed.

A separate global kill-switch (Category-A `killswitch` config key) exists for the unscoped, indefinite cutoff case. Its wire shape is `{enabled: bool}` only — no flag set, no expiry, no scope. Both keys are covered by the CP config reconciler under "same operational class" reasoning because a WebSocket reconnect during an active window must not silently revert.

## 4. Flag set — what each bypass disables

Three independent toggles guarded by an `Enabled` master and `ExpiresAt`:

| Flag | Layer disabled | Notes |
|------|----------------|-------|
| `bypassHooks` | Request-stage hooks pipeline AND response-stage hooks pipeline (including SSE live compliance) | `Record.HookDecision` stamped `"BYPASSED"` so audit consumers can filter |
| `bypassCache` | Response-cache lookup AND write for matched traffic | Takes precedence over the `x-nexus-aigw-no-cache` client header so an operator-forced bypass cannot be overridden by an end-user |
| `bypassNormalize` | Response-side `normalize.Registry.Normalize` + emission to `traffic_event_normalized` | Request-side normalize still runs (it precedes resolution and the canonical payload aids triage). Per admin/UX invariant `bypassNormalize=true` requires `bypassCache=true` because the cache key derives from the canonical normalized payload |

`AnyBypassActive()` returns true only when `Enabled=true` AND at least one flag is on. A nil `*Config`, a disabled config, and an empty flag set are all "no bypass" — every consumer call site is nil-safe so cold-start sees the empty cache as "no bypass" rather than crashing.

`Flags()` returns the canonical-order slice `[bypassHooks, bypassCache, bypassNormalize]` — the literal strings that land on `traffic_event.passthrough_flags`. Operator triage queries grep these literals directly.

## 5. ResolvedRequest — the L4 effective layer

The ai-gateway request path is layered:

```
L1 Transport         HTTP server, raw read/write
L2 Wire-format ingress  body bytes -> NormalizedPayload
L3 Request context   immutable per-request artefact
L4 Policy plane      routing, hooks, audit
L5 Wire-format egress  NormalizedPayload -> wire format
L6 Response normalize  wire format -> NormalizedPayload
```

`*RequestContext` is built once after VK auth + rate-limit admit, before routing. Routing produces a `RouteResult` whose primary target identifies the upstream that will be dispatched. `*ResolvedRequest` is the L4 view that bundles the immutable L3 context with the post-routing decisions and the effective passthrough configuration for the chosen target.

`Resolve(rc, route, ptc)` retains all three pointers as-is. Any input may be nil; the wrapper preserves nil and every getter is nil-safe. The result is stashed on the request `context.Context` via `WithResolved`; downstream consumers (hooks pipeline, cache classifier, audit Writer, executor, response normalize) read it back via `ResolvedFrom(ctx)`.

The pre-routing vs post-routing boundary is encoded in the type system: pre-routing layers take `*RequestContext`; post-routing consumers take `*ResolvedRequest` and explicitly opt into the resolved data. Adding fields to `RequestContext` after Build is not an option — the wrapper exists exactly to avoid that mutability.

## 6. Per-request resolution flow

At Phase 4.5 in the proxy handler (immediately after routing produces `routeResult.Targets`):

1. Determine the primary target's `provider_id` and `adapter_type`.
2. Call `Cache.Effective(providerID, adapterType)`. The cache holds a `*Snapshot` behind an `atomic.Pointer`; the lookup is lock-free.
3. The snapshot's `Effective` method walks global → adapter → provider, accepts only tiers where `Enabled=true` AND `ExpiresAt > now()`, OR-merges the flag set, and picks the tightest expiry plus most-specific attribution.
4. Bundle the result into `ResolvedRequest` via `Resolve(rctxFull, routeResult, passthroughCfg)`.
5. Stash on the request context via `WithResolved`.
6. If `AnyBypassActive()`, stamp `rec.PassthroughFlags` and `rec.PassthroughReason` on the audit record so every downstream branch writes a row tagged with the bypass set.

Cold-start fail-closed is structural: `NewCache()` installs an empty `Snapshot{}` at boot. Until Hub pushes the first real blob, every `Effective` lookup returns nil, every `AnyBypassActive()` returns false, and the full compliance path runs.

## 7. Per-phase enforcement

Each bypass flag short-circuits exactly the phase named in its key:

- **Request hooks (Phase 5)** — `BypassHooks` causes the handler to skip `runRequestHooks` entirely and stamp `rec.HookDecision = "BYPASSED"`. The outer-scope `rewrittenBody / reqHookResult / rejected` zero-values are reused downstream without further branching.
- **Cache (Phase 5.5)** — `BypassCache` short-circuits the cache-key build and Redis lookup. `classifyCachePreLookup` returns `(Skipped, "passthrough")` instead of `(Skipped, "no_cache")` so operators can SQL-distinguish "incident-forced bypass" from "client-opted-out". The bypass takes precedence over the `x-nexus-aigw-no-cache` request header.
- **Response hooks** — `BypassHooks` is re-checked in the response stage via `requestcontext.ResolvedFrom(r.Context())`. The response pipeline build + execute is skipped; `rec.ResponseHookDecision` is left empty.
- **Response normalize** — Inside the audit Writer, when `rec.PassthroughFlags` contains `"bypassNormalize"`, the response-side normalize emit is skipped (request-side still runs as noted above).

The `Enabled` master gate and the tier-active `expires_at > now()` filter both run inside `Snapshot.Effective`, so an expired row contributes nothing even before the auto-revert job sees it.

## 8. Time-bound invariants

Three layers cooperate to enforce the eight-hour ceiling:

| Layer | Enforcement |
|-------|-------------|
| Admin API write validator | Rejects writes where `enabled=true` AND any of: `ExpiresAt` is null, in the past, or beyond `now() + 8h`; or `reason` shorter than 20 chars. Distinct typed error codes: `passthrough_invalid_expiry`, `passthrough_invalid_reason`, `passthrough_no_bypass_selected`, `passthrough_normalize_requires_cache_bypass` |
| DB CHECK constraints (each tier) | `<tier>_expires_required_when_enabled`, `<tier>_expires_max_8h`, `<tier>_reason_min_20` — fire on direct SQL writes that bypass the admin API |
| Runtime cache `TierEntry.active(now)` | A tier with `Enabled=true` but nil `ExpiresAt`, or `ExpiresAt` in the past, is treated as inactive at lookup time — defence-in-depth against a snapshot that somehow bypassed the DB CHECK |

The cross-toggle invariant `bypassNormalize ⇒ bypassCache` is enforced at the admin API and mirrored in the UI form; it is not a DB CHECK because JSONB-column inspection does not generalise cleanly to a column-level constraint.

## 9. Auto-revert — the 60-second Hub job

`PassthroughExpiryJob` (Hub) ticks every 60 seconds (configurable, default `60 * time.Second`) and runs on startup. Each tick issues three UPDATEs:

```sql
UPDATE gateway_passthrough_config_global
   SET enabled = false, updated_at = NOW()
 WHERE enabled = true AND expires_at <= NOW();

UPDATE gateway_passthrough_config_adapter ... (same pattern)
UPDATE gateway_passthrough_config_provider ... (same pattern)
```

When any row was reverted the job logs an `Info` event `passthrough_expiry_revert` with per-tier counts.

Critically, the job is **not** in the request path. Runtime safety is owned by `TierEntry.active(now)` inside the cache snapshot — an expired row stops contributing to `Effective` the instant `now > expiresAt`, even if the DB row still reads `enabled = true`. The auto-revert job exists for **audit + admin-UI correctness**: it ensures the row's `enabled` column reflects reality, the `updated_at` timestamp records the auto-revert event, and the next admin-API read does not show a stale "enabled" state.

A separate, complementary loop runs on the Control Plane side: the `configreconcile` watch over the `gateway_passthrough` key ticks every 60 seconds and re-pushes the assembled blob to Hub if `thing.desired` has drifted from the canonical source. This covers the case where a WebSocket reconnect during an active window otherwise risked silently reverting to the pre-passthrough state.

## 10. traffic_event audit fan-out

Two columns persist the bypass set per request:

| Column | Type | Meaning |
|--------|------|---------|
| `passthrough_flags` | `TEXT[]` | Canonical-order subset of `{bypassHooks, bypassCache, bypassNormalize}`. NULL / empty when no bypass fired. |
| `passthrough_reason` | `TEXT` | Operator-supplied justification from the most-specific active tier (≥20 chars per DB CHECK). NULL when `passthrough_flags` is NULL. |

A partial index optimises the common operator triage query "show every request that bypassed compliance since timestamp X":

```sql
CREATE INDEX traffic_event_passthrough_active_idx
  ON "traffic_event" ("timestamp" DESC)
  WHERE "passthrough_flags" IS NOT NULL;
```

The path from gateway to row:

1. Gateway handler stamps `rec.PassthroughFlags = pt.Flags()` and `rec.PassthroughReason = pt.Reason` at Phase 4.5.
2. The audit `Writer` carries those fields through to the wire message `TrafficEventMessage` (JSON tags `passthroughFlags` / `passthroughReason`, both `omitempty` so untouched rows pay no wire cost).
3. Hub's traffic consumer reads the wire message and bound-parameters `passthroughFlagsParam` / `passthroughReasonParam` into the `INSERT` against `traffic_event` — empty slice / empty string normalise to NULL so the partial index stays compact.

## 11. Admin endpoints and IAM

All endpoints mount under `/api/admin/` in the Control Plane:

| Method + path | Action | Notes |
|---------------|--------|-------|
| `GET    /passthrough/global` | `admin:passthrough.read` | Single row |
| `PUT    /passthrough/global` | `admin:passthrough.emergency-enable` | Upserts the singleton |
| `GET    /passthrough/adapter/:adapter_type` | `admin:passthrough.read` | 404 if no row |
| `PUT    /passthrough/adapter/:adapter_type` | `admin:passthrough.emergency-enable` | Upserts by adapter_type |
| `DELETE /passthrough/adapter/:adapter_type` | `admin:passthrough.write` | Hard delete the tier row |
| `GET    /passthrough/provider/:provider_id` | `admin:passthrough.read` | |
| `PUT    /passthrough/provider/:provider_id` | `admin:passthrough.emergency-enable` | Upserts by provider_id |
| `DELETE /passthrough/provider/:provider_id` | `admin:passthrough.write` | Hard delete; FK CASCADE removes the row if the Provider is deleted upstream |
| `GET    /passthrough/effective/:provider_id` | `admin:passthrough.read` | Reads `gateway_passthrough_config_effective` view for the resolved per-Provider merge |
| `GET    /passthrough/snapshot` | `admin:passthrough.read` | Bulk read returning `{global, adapters, providers, providerNames}` for the UI |

The three actions on the `passthrough` resource are deliberately split:

- `VerbRead` — wide; any admin who needs to inspect state.
- `VerbWrite` — narrow; lets compliance/provider admins disable or delete an active passthrough during incident cleanup without granting the right to flip one on.
- `VerbEmergencyEnable` — narrowest; the gate for flipping any tier's `enabled=true`. Scoped to the incident-response role only because activating it silences compliance hooks for matched traffic.

Write payload shape:

```json
{
  "enabled": true,
  "bypassHooks": true,
  "bypassCache": false,
  "bypassNormalize": false,
  "expiresAt": "...ISO-8601, ≤ now()+8h...",
  "reason": "explanatory string, ≥ 20 chars"
}
```

The validator rejects `enabled=true` with all three flags off, `bypassNormalize=true` with `bypassCache=false`, missing/past/too-far `expiresAt`, and `reason` shorter than 20 chars. Validation messages reference the operator runbook for context.

After every write or delete the handler reassembles the full `{global, adapters, providers}` blob from the three tables and pushes it to Hub under the `gateway_passthrough` shadow key via `HubConfigChanger.NotifyConfigChange`. A Hub-propagation failure returns 502 with a typed `propagation_error` envelope instructing the operator that the row landed locally but the runtime may not reflect it until the next reconcile tick.

## 12. Config dispatch on the gateway

`configdispatch` registers a raw decoder for the `gateway_passthrough` shadow key. On every push it:

1. Treats an empty payload as `SetSnapshot(nil)` — which the cache normalises to an empty `Snapshot{}`, so an empty blob means "fully disabled" (fail-closed).
2. Unmarshals the JSON into a `passthrough.Snapshot`. A parse error logs `gateway_passthrough parse failed; preserving prior snapshot` and returns an error to the loader — the cache is **not** wiped on a malformed push.
3. On success, swaps the cache pointer atomically via `Cache.SetSnapshot(&snap)` and logs the new global / adapter-count / provider-count summary.

`PassthroughCache` is constructed in `wiring/boot.go` and threaded into both the `configdispatch` Deps (so it can be written to) and the proxy handler Deps (so it can be read from).

## 13. Observability surface

Bypass activity is observable through three artefacts already covered above; there is no dedicated Prometheus counter for passthrough bypass.

- **Per-request audit row** — `traffic_event.passthrough_flags` + `passthrough_reason` are the primary forensic surface; the partial index `traffic_event_passthrough_active_idx` makes time-window scans cheap. The cache-skip reason `"passthrough"` distinguishes operator-forced bypass from `"no_cache"` (client header) in `traffic_event.gateway_cache_skip_reason`.
- **Auto-revert log** — Hub job emits a structured slog `event = passthrough_expiry_revert` with per-tier counts when any row was reverted in a tick.
- **Config dispatch log** — `passthrough config updated` slog line at `Info` level on every successful snapshot swap, with `global_enabled`, adapter count, provider count.

## References

- `packages/ai-gateway/internal/execution/passthrough/doc.go`
- `packages/ai-gateway/internal/execution/passthrough/config.go`
- `packages/ai-gateway/internal/execution/passthrough/cache.go`
- `packages/ai-gateway/internal/policy/requestcontext/doc.go`
- `packages/ai-gateway/internal/policy/requestcontext/resolved.go`
- `packages/ai-gateway/internal/ingress/proxy/proxy.go`
- `packages/ai-gateway/internal/platform/audit/audit.go`
- `packages/ai-gateway/cmd/ai-gateway/configdispatch/configdispatch.go`
- `packages/ai-gateway/cmd/ai-gateway/wiring/boot.go`
- `packages/ai-gateway/cmd/ai-gateway/wiring/routes.go`
- `packages/control-plane/internal/governance/passthrough/handler/handler.go`
- `packages/control-plane/cmd/control-plane/wiring/reconcile.go`
- `packages/control-plane/internal/platform/configreconcile/reconcile.go`
- `packages/control-plane/internal/identity/iam/managed.go`
- `packages/control-plane-ui/src/pages/ai-gateway/passthrough/PassthroughPage.tsx`
- `packages/control-plane-ui/src/api/services/ai-gateway/passthrough.ts`
- `packages/control-plane-ui/src/hooks/usePermission.ts`
- `packages/nexus-hub/internal/jobs/defs/expiry/passthrough_expiry.go`
- `packages/nexus-hub/internal/jobs/consumer/message.go`
- `packages/nexus-hub/internal/jobs/consumer/traffic.go`
- `packages/shared/transport/mq/messages.go`
- `packages/shared/identity/iam/catalog.go`
- `packages/shared/identity/iam/catalog_data.go`
- `packages/shared/schemas/configkey/configkey.go`
- `packages/shared/schemas/configkey/validation.go`
- `tools/db-migrate/schema.prisma`
- `tools/db-migrate/migrations/20260517000000_e48_gateway_passthrough_config_3tier/migration.sql`
- `tools/db-migrate/migrations/20260517000010_e48_traffic_event_passthrough_columns/migration.sql`
- `tools/db-migrate/migrations/20260520000000_fix_e48_passthrough_fk/migration.sql`
- [./kill-switch-architecture.md](./kill-switch-architecture.md)
