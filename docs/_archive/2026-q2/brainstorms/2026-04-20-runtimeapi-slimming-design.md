# Runtime API Slimming & Thing Model Alignment — Design

**Date:** 2026-04-20
**Author:** nexus
**Status:** Draft — pending plan
**Scope:** `packages/compliance-proxy`, `packages/ai-gateway`, `packages/control-plane`, `packages/control-plane-ui`, `packages/nexus-hub` (Hub is passive — no protocol change)
**Non-negotiable:** Pre-GA; no backwards-compatibility shims, no phased rollouts, no deprecation markers — legacy paths are deleted in the same commits that introduce replacements.

---

## 1. Goal & Principles

### 1.1 Goal

Bring the Compliance Proxy `runtimeapi` and the AI Gateway runtime HTTP surface into alignment with the platform's Thing Model. Today, the Compliance Proxy runtime API exposes a mix of mutating admin endpoints (kill switch toggling, alert channel CRUD, exemption CRUD, threshold edits) whose writes are either non-persistent (in-memory) or bypass the Thing shadow. The AI Gateway has no equivalent ops surface at all. Both are wrong for different reasons.

### 1.2 Principles (enforced everywhere in this redesign)

1. **One write path for data-plane config.** All mutable configuration for every data-plane service flows through:
   ```
   Admin UI → CP admin API → thing_config_template (UPSERT) + config_change_event (INSERT)
            → Hub push (per-key delta over WebSocket, HTTP fallback on disconnect)
            → thingclient.OnConfigChanged(desired) → local subsystem reload
            → thingclient shadow_report(reported) → Hub
   ```
2. **One read path for ops observability.** Every data-plane service (proxy, ai-gateway) exposes a small read-only `/runtime/*` surface that returns the currently applied configuration, health, and sync status. This is the "did the desired change actually land" confirmation tool for operators.
3. **One emergency path, white-listed and auditable.** A small whitelist of `config_key` values (`killswitch`, `active_exemptions`) accept a `PUT /runtime/config/{key}` break-glass write on the data-plane service. Every such write MUST atomically: apply locally, persist a local event log, bump a local version, and (best-effort + durable buffer) report back to Hub so the recovered desired state does not roll the emergency change back.

### 1.3 Non-goals

- Replacing the `shared/thingclient` shadow protocol (shape, retries, WS fallback remain untouched).
- Introducing a loopback admin HTTP server on the desktop agent (attack-surface cost, no business driver).
- Changing the CP admin-API auth model (`x-admin-key` + admin JWT stays; runtime endpoints keep the elevated bearer model).
- Extending AG/proxy config schemas beyond what exists today (no new policy knobs). Migration is structural, not functional.
- Touching `hook_config`, `interception_domains`, `domain_allowlist` — they already use the target pattern.

---

## 2. Persistence — Reuse Existing Machinery, No New Tables

The platform already has the right schema:

```prisma
model ThingConfigTemplate {
  type       String   // "compliance-proxy" | "ai-gateway" | "agent" | ...
  config_key String   // "killswitch" | "hook_config" | "alert_channels" | ...
  state      Json     // full desired payload
  version    BigInt   // monotonic, increments on every admin write
  updated_at DateTime
  updated_by String?
  @@id([type, config_key])
}

model ConfigChangeEvent {
  id          String
  timestamp   DateTime
  thing_type  String
  config_key  String
  action      String     // "enable" | "disable" | "update" | "force_close" | ...
  actor_id    String
  actor_name  String
  new_state   Json
  new_version BigInt
  source_ip   String?
  // (extended below)
}
```

### 2.1 Schema additions

`ConfigChangeEvent` gains one boolean column:

```prisma
  emergency_override Boolean @default(false) @map("emergency_override")
```

Purpose: flag writes that landed via the data-plane break-glass PUT (not through CP admin API). Value `true` means Hub learned of the change via `shadow_report` rather than having authored it. Ops dashboards and SIEM bridges filter on this column to surface emergency interventions for post-mortem.

No other schema changes. No new tables.

### 2.2 New `config_key` rows

All rows are keyed by `(thing_type, config_key)`. The `state` JSONB shape is owned by the owning admin API handler; proxy/ag trust the shape once it reaches `OnConfigChanged`.

**compliance-proxy:**

| `config_key` | `state` shape | Notes |
|---|---|---|
| `killswitch` | `{ "enabled": bool }` | Already exists. No change. |
| `hook_config` | (existing) | Out of scope. |
| `interception_domains` | (existing) | Out of scope. |
| `domain_allowlist` | (existing) | Out of scope. |
| `active_exemptions` | `{ "entries": [{"id", "sourceIP", "targetHost", "expiresAt", "reason", "approvedBy"}] }` | New. See §4.2. |
| `alert_channels` | `{ "channels": [Channel] }` where Channel matches `alerting.Channel` (id, name, type, url/slackBotToken/smtp\*, headers, timeoutSec, severities) | New. Replaces in-memory runtime writes. |
| `alert_thresholds` | `{ "thresholds": {metric: value, ...} }` | New. |
| `alert_custom_checks` | `{ "checks": [CustomCheck] }` | New. |

**ai-gateway:** The AG side is primarily about **exposing** applied state. Mutations are already done via existing CP admin-API CRUD for providers/models/routes/quotas/ratelimits. The shadow-backed config keys for AG are:

| `config_key` | Source of truth | Purpose |
|---|---|---|
| `killswitch` | New — same shape as proxy | AG traffic kill switch (pre-existed as non-shadow; shadow-ize it). |
| `hook_config` | (existing) | AG already applies. Out of scope. |

For the other config surfaces (providers/models/routes/quotas/rate_limits), the CP admin API already writes them to their respective Prisma tables and AG's internal config cache reloads by bumping a global `config_version` key. The exact `config_key` naming and cache-invalidation fan-out for AG is deferred to the implementation plan (because it depends on the current `configcache` layout in AG, which needs to be audited at plan time). For the purposes of this spec, it is sufficient that AG exposes **applied** state for every category it already loads — whether via one composite `/runtime/config` endpoint or per-category is covered in §4.

### 2.3 Audit authorship rules

- Writes from CP admin API → `actor_id` = NexusUser id or admin API key id; `emergency_override` = false.
- Writes from data-plane break-glass (Hub receives shadow_report with `reason='break_glass'`) → `actor_id` = `"break-glass:<token_id>"`; `emergency_override` = true; `source_ip` = the data-plane service's IP.

The `<token_id>` comes from a mandatory hash of the elevated bearer token's suffix — this is not user identity (the token is shared), but it distinguishes proxy-A from proxy-B in multi-instance deployments.

---

## 3. API Surface Changes

### 3.1 Compliance Proxy `runtimeapi`

Prefix-rooted reorganization: everything that survives moves under `/runtime/*` (read-only + break-glass). Everything that does not survive is deleted, not preserved under a deprecation flag.

| Current | Method | Action | Target |
|---|---|---|---|
| `/healthz` | GET | **keep** | (unchanged — liveness probe) |
| `/metrics` | GET | **keep** | (unchanged — Prometheus scrape) |
| `/connections` | GET | **move** | `GET /runtime/connections` |
| `/killswitch` | GET | **move** | `GET /runtime/config/killswitch` |
| `/killswitch` | POST | **delete** | Use CP admin API `POST /api/admin/compliance/killswitch` |
| `/killswitch/force-close` | POST | **move** | `PUT /runtime/config/killswitch` (break-glass; see §5) |
| `/killswitch/history` | GET | **move** | `GET /runtime/killswitch/history` (local event log only) |
| `/exemptions` | GET | **move** | `GET /runtime/config/active_exemptions` |
| `/exemptions` | POST | **delete** | Use CP admin API |
| `/exemptions/{id}` | DELETE | **delete** | Use CP admin API |
| `/alerts` | GET | **move** | `GET /runtime/alerts` (live alerts, not config) |
| `/alerts/webhook` | GET | **delete** | Superseded by `/runtime/config/alert_channels`; UI uses channels page |
| `/alerts/webhook` | PUT | **delete** | Superseded entirely |
| `/alerts/thresholds` | GET | **move** | `GET /runtime/config/alert_thresholds` |
| `/alerts/thresholds` | PUT | **delete** | Use CP admin API |
| `/alerts/channels` | GET | **move** | `GET /runtime/config/alert_channels` |
| `/alerts/channels` | POST | **delete** | Use CP admin API |
| `/alerts/channels/{id}` | PUT/DELETE | **delete** | Use CP admin API |
| `/alerts/custom-checks` | GET | **move** | `GET /runtime/config/alert_custom_checks` |
| `/alerts/custom-checks*` | POST/PUT/DELETE | **delete** | Use CP admin API |

**New endpoints:**

| Path | Method | Auth | Purpose |
|---|---|---|---|
| `/runtime/config` | GET | Standard | Full applied state across all `config_key` values |
| `/runtime/config/{key}` | GET | Standard | Single category |
| `/runtime/config/killswitch` | PUT | Elevated | Break-glass (§5) |
| `/runtime/config/active_exemptions` | PUT | Elevated | Break-glass (§5) |
| `/runtime/health` | GET | Standard | Detailed health (DB, Hub WS, MQ, upstream TLS) |
| `/runtime/sync-status` | GET | Standard | `{desiredVer, reportedVer, lastSyncAt, drift}` |

**Auth unchanged:** `COMPLIANCE_PROXY_API_TOKEN` (standard) + `COMPLIANCE_PROXY_ELEVATED_TOKEN` (elevated) bearer tokens. No migration to auth-server JWT in this work.

### 3.2 AI Gateway runtime HTTP surface (new)

AG currently exposes only `/healthz`, `/metrics`, `/internal/provider-test`, `/v1/*` (traffic), and `/v1/usage*`. It has no admin/runtime face.

**New endpoints (additive — nothing to delete on AG):**

| Path | Method | Auth | Purpose |
|---|---|---|---|
| `/runtime/config` | GET | Standard | Full applied state (providers, models, routes, quotas, rate limits, hooks, killswitch) |
| `/runtime/config/{key}` | GET | Standard | Single category |
| `/runtime/config/killswitch` | PUT | Elevated | Break-glass (§5) |
| `/runtime/health` | GET | Standard | Detailed health |
| `/runtime/sync-status` | GET | Standard | `{desiredVer, reportedVer, lastSyncAt, drift}` |

AG exemption PUT is **not** added — AG does not enforce IP/host exemptions (that's a proxy concept).

**New env vars (symmetric with proxy):** `AI_GATEWAY_API_TOKEN` (standard), `AI_GATEWAY_ELEVATED_TOKEN` (elevated).

### 3.3 Control Plane admin API

**New endpoints** (under `/api/admin/compliance/*` for proxy-facing, `/api/admin/ai-gateway/*` for AG-facing):

| Path | Method | IAM action | Effect |
|---|---|---|---|
| `/api/admin/compliance/killswitch` | GET | `compliance:ReadKillSwitch` | Read desired + latest reported from DB |
| `/api/admin/compliance/killswitch` | POST | `compliance:ToggleKillSwitch` | `{enabled: bool}` → write `thing_config_template` + `config_change_event` → notify Hub |
| `/api/admin/compliance/killswitch/history` | GET | `compliance:ReadKillSwitch` | Paginated `config_change_event` where `config_key='killswitch'` |
| `/api/admin/compliance/exemptions` | GET | `compliance:ReadExemptions` | List active exemptions (from `active_exemptions` template) and pending `ExemptionRequest` rows |
| `/api/admin/compliance/exemptions` | POST | `compliance:GrantExemption` | Create a new active entry (either approve an `ExemptionRequest` or create ad-hoc) |
| `/api/admin/compliance/exemptions/{id}` | DELETE | `compliance:RevokeExemption` | Remove an entry from `active_exemptions` |
| `/api/admin/compliance/alert-channels` | GET/POST | `compliance:*AlertChannels` | CRUD over `alert_channels` template |
| `/api/admin/compliance/alert-channels/{id}` | PUT/DELETE | `compliance:UpdateAlertChannels` | Per-channel mutation |
| `/api/admin/compliance/alert-thresholds` | GET | `compliance:ReadAlertThresholds` | — |
| `/api/admin/compliance/alert-thresholds` | PUT | `compliance:UpdateAlertThresholds` | Full replacement |
| `/api/admin/compliance/custom-checks` | GET/POST | `compliance:*CustomChecks` | — |
| `/api/admin/compliance/custom-checks/{id}` | PUT/DELETE | `compliance:UpdateCustomChecks` | — |
| `/api/admin/ai-gateway/killswitch` | GET/POST | `aigateway:*KillSwitch` | Symmetric to proxy |

**Deleted endpoints** (replaced by above):

| Path | Reason |
|---|---|
| `GET /api/admin/proxy/alerts/webhook` | Functionality covered by `/alerts/channels`, which is now CP-authoritative |
| `PUT /api/admin/proxy/alerts/webhook` | Same |
| `GET/POST /api/admin/proxy/exemptions` | Moved to `/api/admin/compliance/exemptions` |
| `DELETE /api/admin/proxy/exemptions/{id}` | Moved |
| `GET/PUT /api/admin/proxy/alerts/thresholds` | Moved |
| `GET/POST/PUT/DELETE /api/admin/proxy/alerts/channels*` | Moved |
| `GET/POST/PUT/DELETE /api/admin/proxy/alerts/custom-checks*` | Moved |
| `GET /api/admin/proxy/compliance/killswitch-history` | Moved to `/api/admin/compliance/killswitch/history` |

**Hub-notify hook:** After every successful CP admin write, the handler calls the existing Hub "config update" notifier (already used by the in-place-working `killswitch` / `hook_config` flow). No new Hub API endpoints are added.

---

## 4. Data Flow Details

### 4.1 Standard write (CP-originated)

```
1. Admin clicks "Save" in CP UI
2. UI POST /api/admin/compliance/alert-channels {channel payload}
3. CP handler (pgx txn):
   - UPSERT thing_config_template SET state = $payload, version = version + 1
   - INSERT config_change_event (actor=admin_user, new_state, new_version, emergency_override=false, source_ip)
4. CP calls Hub /api/internal/config/notify with (type, config_key, new_version)
5. Hub reads thing_config_template, for each online thing of matching type:
   - Send WS hubMessage {type="config_changed", configKey, state, desiredVer: new_version}
   - Offline things fetch on next reconnect (existing behavior)
6. proxy/ag thingclient receives → applyConfig → OnConfigChanged(desired)
7. OnConfigChanged switch{case "alert_channels": channelMgr.Rebuild(state); reported[key] = state}
8. thingclient sendShadowReport(reported, desiredVer)
9. Hub upsert thing_shadow_state; CP read-side sees reported match desired
```

### 4.2 `active_exemptions` lifecycle

- **Approval**: Admin approves an `ExemptionRequest` (status transitions `PENDING → APPROVED`). CP handler, in the same txn:
  - Updates `ExemptionRequest.status` and `reviewedBy/reviewedAt`.
  - Reads current `active_exemptions.state.entries`, appends `{id, sourceIP, targetHost, expiresAt=now+durationMinutes, reason, approvedBy}`, filters out any already-expired entries, UPSERTs the template (version bump).
  - Inserts `config_change_event`.
- **Revocation**: CP admin deletes an entry → same UPSERT-minus-entry pattern.
- **Expiration**:
  - Proxy side: `ExemptionStore` enforces TTL in-memory. Expired entries stop matching without shadow involvement.
  - Garbage collection: a CP background goroutine (spawned by control-plane main, reusing the existing `packages/control-plane/internal/jobs` scheduler) runs every 5 minutes. It scans `active_exemptions` templates, drops expired entries, bumps version only if anything was removed. `emergency_override=false`, `actor='system:exemption-gc'`.

### 4.3 Read path (CP Nodes detail — "Applied Configuration" tab)

- Data sources (all already present):
  - `thing_config_template` for **desired**.
  - `thing_shadow_state` for **reported**.
  - `config_change_event` for **last change metadata** (actor, ts, emergency_override).
- One new BFF endpoint: `GET /api/admin/things/{id}/applied-config` — JSON per `config_key` with desired, reported, drift, last_change.
- UI consumes it. No direct hit to the data-plane runtimeapi from a user's browser.

### 4.4 Read path (operator shell)

```bash
# Full snapshot
curl -H "Authorization: Bearer $COMPLIANCE_PROXY_API_TOKEN" http://proxy:3041/runtime/config

# Single key
curl -H "Authorization: Bearer $COMPLIANCE_PROXY_API_TOKEN" http://proxy:3041/runtime/config/killswitch

# Drift check
curl -H "Authorization: Bearer $COMPLIANCE_PROXY_API_TOKEN" http://proxy:3041/runtime/sync-status
```

The payload format for `/runtime/config`:

```json
{
  "thingId": "proxy-host-abc",
  "thingType": "compliance-proxy",
  "desiredVer": 42,
  "reportedVer": 42,
  "inSync": true,
  "reportedAt": "2026-04-20T10:15:00Z",
  "configs": {
    "killswitch":          { "version": 3, "state": {"enabled": false} },
    "active_exemptions":   { "version": 7, "state": {"entries": [...]} },
    "alert_channels":      { "version": 2, "state": {"channels": [...]} },
    "alert_thresholds":    { "version": 1, "state": {"thresholds": {...}} },
    "alert_custom_checks": { "version": 1, "state": {"checks": [...]} }
  }
}
```

Secret-looking header values and credentials (SMTP password, Slack bot token, webhook headers matching `auth/token/key/secret/password/credential`) are masked as `"***"` in the response — reuse `alerting.MaskHeaders` plus an equivalent for Slack/SMTP fields.

---

## 5. Break-Glass Protocol

Whitelisted keys: **`killswitch`** (proxy + ai-gateway), **`active_exemptions`** (proxy only).

### 5.1 Request shape

```
PUT /runtime/config/killswitch HTTP/1.1
Authorization: Bearer $COMPLIANCE_PROXY_ELEVATED_TOKEN
Content-Type: application/json

{
  "state": {"enabled": true},
  "reason": "upstream provider SEV1; operator X initiated"
}
```

The `reason` is required (≥ 8 chars, ≤ 500 chars) and ends up in the local event log + the `config_change_event` row once Hub gets the shadow_report.

### 5.2 Atomic 5-step handler (data-plane side)

```
1. Apply to local subsystem
   - killswitch: killSwitch.Toggle(newState, "break-glass")
   - active_exemptions: exemptionStore.Rebuild(newEntries)
2. Append to local break-glass event log (append-only JSONL file)
   path: <data_dir>/break_glass_events.jsonl
   body: {ts, configKey, newState, reason, actorTokenHash, tokenID, newVer}
3. new_ver = max(thingclient.ReportedVer(), thingclient.DesiredVer()) + 1
4. Best-effort shadow_report
   - WS connected → sendShadowReportWS(singleKey:state, newVer, reason="break_glass")
   - HTTP fallback → httpShadowReport(... reason="break_glass")
5. On any failure in step 4 → append to pending buffer:
   path: <data_dir>/break_glass_pending.jsonl
   flushed on next successful reconnect before normal shadow_report
```

**Failure semantics:**
- Steps 1-3 must all succeed before the HTTP handler returns 200. If step 1 fails, return 500 and do not log an event (the local state did not change).
- Step 4 is best-effort. The HTTP response includes `"reported": "pending"` when step 4 failed (so the operator knows to verify once Hub is back).
- Step 5 is guaranteed: the pending file is fsync'd before the handler returns.

### 5.3 Hub-side reconciliation

On receiving `shadow_report` with `reason="break_glass"`:

```go
if msg.ReportedVer > currentTemplate.Version {
    UPSERT thing_config_template SET state=msg.State, version=msg.ReportedVer, updated_by='break-glass'
    INSERT config_change_event (actor_id='break-glass:<token_id>',
                                actor_name='break-glass',
                                new_state=msg.State,
                                new_version=msg.ReportedVer,
                                source_ip=msg.SourceIP,
                                emergency_override=true,
                                action='emergency_override')
    // Optional: emit ops-alert via SIEM bridge
}
```

Concurrent CP-admin writes resolve via the monotonic `version`:
- CP admin writes at v=5, proxy break-glass writes at v=6 concurrently → whoever hits first sets v=5 or v=6; the other's txn reads `current.version` and takes `+1`. Both end up in `config_change_event`; desired ends at whichever had the higher final version.

### 5.4 Replay / idempotency

Each entry in `break_glass_pending.jsonl` has a deterministic `ver`. Hub dedupes by `(thing_id, config_key, version)` so duplicate flushes (e.g. process restart during flush) are safe.

### 5.5 Token ID derivation

```go
tokenHash := sha256(elevatedBearerToken)
tokenID   := hex.EncodeToString(tokenHash[:4])  // 8-char identifier
```

Suitable as an opaque identifier in `actor_id='break-glass:<tokenID>'`. No actual token content leaks.

---

## 6. UI Changes

### 6.1 Control Plane (React)

**New pages:**
- `/compliance/exemptions` — list view (active + pending approvals), approve/revoke actions. Combines `ExemptionRequest` table + `active_exemptions` template.
- `/compliance/alerts/channels` — replaces in-place channel mgmt from `AlertHistoryPage`; full CRUD.
- `/compliance/alerts/thresholds` — threshold editor.
- `/compliance/alerts/custom-checks` — custom check CRUD.

**Node detail extension** (`/infrastructure/nodes/{id}`):
- New tab: **Applied Configuration**
- Content: one card per `config_key` row for this Thing, each showing:
  - Desired JSON (masked) + version
  - Reported JSON (masked) + version
  - Sync status (green if match; yellow "pending" if desired > reported < 60s; red "drift" if > 60s)
  - Last change metadata: timestamp, actor name, "Emergency" badge if `emergency_override=true`

**Deleted UI:**
- `packages/control-plane-ui/src/pages/proxy/AlertHistoryPage.tsx` lines 110-186 (webhook config form block).
- `packages/control-plane-ui/src/api/services/proxy.ts` functions:
  - `getWebhookConfig`, `updateWebhookConfig` — deleted outright (feature superseded by alert channels).
  - `getExemptions`, `createExemption`, `deleteExemption`, `getKillSwitchHistory` — relocated to a new `complianceApi` module that talks to the new `/api/admin/compliance/*` endpoints.
- i18n keys under `pages:proxy.alertHistory.webhook*` in `en/zh/es`.

### 6.2 Agent (macOS + Windows menu bar)

**New panel:** "Configuration" (under the existing status panel).

**Shown:**
- `Config version: {reportedVer}` + sync indicator (✓ in sync, ⟳ syncing, ⚠ drift)
- `Last synced: {relative time}`
- Category summary rows (computed from reported state locally):
  - `Hooks: N enabled` (count of `hook_config.hooks` with `enabled=true`)
  - `Interception: N domains` (count of entries in `interception_domains`)
  - `Exemptions: N active` (count of non-expired in `active_exemptions`)

**Not shown:** full JSON. End-user machines must not see operator secrets. Support operations retrieve detailed state from CP (authenticated).

---

## 7. Testing Strategy

### 7.1 Unit tests

**CP (`packages/control-plane/internal/handler/**/*_test.go`):**
- Each new handler: happy-path + request validation (empty `enabled`, oversized reason, etc.).
- `thing_config_template` UPSERT + `config_change_event` INSERT are within one txn (rollback-on-failure test).
- IAM action mapping (permission denied paths).

**proxy / ai-gateway (`*_test.go` in each subsystem):**
- `OnConfigChanged` new cases (`alert_channels`, `alert_thresholds`, `active_exemptions`, `alert_custom_checks`) — apply success, apply partial failure (reported state reflects only what succeeded).
- `/runtime/config` composer — correctness with 0 keys, 1 key, all keys.
- `/runtime/config/{key}` — 404 for unknown keys, 200 + payload for known.
- Break-glass handler — all 5 steps in order, pending buffer on Hub-down, pending flush on reconnect.
- Masking — every secret-like field in the response is `"***"`.

**shared/thingclient (`shadow_test.go` extension):**
- `sendShadowReport` with `reason="break_glass"` is accepted and emits correct WS / HTTP payload.
- Dedup on Hub-side replay (tested against a `httptest` Hub stub).

### 7.2 Integration tests (`packages/nexus-hub/test/e2e/*`)

Extend the existing `config_push_loop_test.go` pattern with new table-driven cases:

- `TestAlertChannelsPushLoop` — CP API write → Hub push → proxy OnConfigChanged → shadow_report → CP read sees match.
- `TestExemptionApprovalFlow` — `ExemptionRequest` → approve → `active_exemptions` bump → proxy applies → expiry → GC removes.
- `TestBreakGlassKillswitch_HubDown` — stop Hub WS, PUT `/runtime/config/killswitch`, local state toggles, buffer grows, restart Hub, buffer flushes, CP reads `emergency_override=true` row.
- `TestBreakGlassVersionConflict` — concurrent CP write + proxy break-glass; final `version` is max+1 of whichever arrived last; both `config_change_event` rows exist.

All e2e tests gated behind `e2e` build tag + `RUN_E2E=1` (existing convention).

### 7.3 Manual smoke checklist

- Local 4-service bootstrap (`./scripts/dev-start.sh`), UI toggles kill switch → proxy applies → CP Nodes page shows in-sync.
- Add an exemption via CP UI → proxy `GET /runtime/config/active_exemptions` shows it → wait for TTL expiry → proxy log shows local removal → CP GC job on next cycle drops it from desired.
- Stop Hub, break-glass PUT kill switch on proxy → local traffic is blocked → start Hub → CP page shows `emergency_override=true` entry in history.

---

## 8. Implementation Dependencies & Order of Work

(This is ordering, not phasing — everything lands before merge; order is about avoiding broken intermediate states on `main`.)

1. **Schema migration** — add `emergency_override` column to `config_change_event`. Seed empty rows for each new `(type, config_key)` in `thing_config_template`.
2. **Shared types** — extend `packages/shared/schemas/configtypes` or add a sibling package for the new config_key payload shapes (Go structs + JSON validation tags).
3. **CP admin API handlers** — compliance/* + ai-gateway/* endpoints. Land with unit tests + IAM action wiring.
4. **Hub-notify plumbing** — confirm existing notify path works for new keys (should; it's key-agnostic).
5. **proxy `OnConfigChanged` switch cases** — add `alert_channels`, `alert_thresholds`, `active_exemptions`, `alert_custom_checks`. Remove mutable runtimeapi handlers in same commits.
6. **proxy new runtimeapi surface** — `/runtime/*` read endpoints + break-glass PUTs + event log + pending buffer.
7. **proxy `AlertChannelManager` / `ExemptionStore` / `ThresholdStore` / `CustomCheckRegistry`** — make them shadow-driven; rip out the in-memory-only update paths.
8. **ai-gateway runtimeapi** — new addition; plus ensure AG already reacts to shadow for any categories we advertise.
9. **CP Nodes detail UI** — Applied Configuration tab.
10. **New compliance UI pages** — channels, thresholds, custom-checks, exemptions.
11. **Delete legacy UI** — webhook form, old proxyApi functions, i18n keys.
12. **Agent menu bar Configuration panel** — macOS + Windows.
13. **Integration tests** — cover new e2e cases.

Each step commits independently with passing tests. Step 5 removes functionality that step 3 replaces — the `delete` commits MUST land after the replacing CP handlers are live to avoid a window where no one can toggle kill switch.

---

## 9. Security Considerations

- **Elevated token reuse**: static bearer tokens for break-glass are a tradeoff (any operator with the token can force-close). Mitigated by: ops rotates tokens on a cadence, tokens live only in a physical runbook / sealed secret manager, and every use generates a `config_change_event` with `emergency_override=true` that SIEM monitors on a near-real-time alert.
- **Secret masking in `/runtime/config`**: every response that could carry a header/credential gets run through the masking helper. Unit tests assert this on each shape.
- **Agent UI privacy**: summary-only (counts + versions); no raw JSON ever leaves the agent to the end-user view.
- **No loopback HTTP on agent**: attack surface deliberately not opened.
- **Replay safety**: Hub dedupes break-glass replays by `(thing_id, config_key, version)` — duplicate flush after crash is idempotent.

---

## 10. Explicitly NOT Deferred

Per CLAUDE.md "pre-GA: no phased rollouts":

- No `@deprecated` markers kept alive alongside new endpoints.
- No "Phase 1 keeps old behavior" wording anywhere in the plan.
- No compatibility shims in CP UI to fall back to the old proxyApi functions when a new endpoint 404s — the old functions are deleted, period.
- No feature flag gating the new runtime surface — it ships or it doesn't.

---

## 11. Open Questions (to close during planning)

1. **AG config_key enumeration**: Plan phase must audit `packages/ai-gateway/internal/configcache` and list the concrete `config_key` values AG should expose via `/runtime/config`. Spec keeps this as "whatever AG's config layer loads today".
2. **CP background GC cadence for `active_exemptions`**: Spec says 5 minutes. Final value settled in plan; likely keep 5m.
3. **Hub notify API endpoint path**: Spec assumes existing Hub `/api/internal/config/notify` (or equivalent) is reused; plan phase confirms exact path or adds it if missing.
4. **IAM action naming**: Spec proposes `compliance:*AlertChannels` etc.; plan phase normalizes against existing IAM action catalog (there may already be some of these).

None of these is a design-level question — they're enumeration / naming details that surface at plan time.
