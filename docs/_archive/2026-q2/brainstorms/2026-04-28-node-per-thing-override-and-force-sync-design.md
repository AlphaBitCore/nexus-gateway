# Per-Thing Config Override + Force-Sync — Design

**Date:** 2026-04-28
**Status:** Approved design, pending implementation
**Author:** nexus@alphabitcore.com + Claude
**Related:**
  - `docs/dev/thing-model.md` (§5 templates, §6 status, §10 terminology)
  - `docs/dev/service-call-framework.md` (config sync pipeline)
  - `packages/control-plane-ui/src/pages/infrastructure/InfraNodeDetailPage.tsx` (current detail page)
  - `packages/nexus-hub/internal/handler/internal_things.go` (existing template→thing.desired merge)

---

## 1. Problem & Motivation

Today the platform has only one configuration tier: per-(`type`, `config_key`) **templates** in
`thing_config_template`. When admin edits a template, every Thing of that type receives the
same payload, with no per-Thing differentiation. This is fine for uniform fleets but breaks down
for five recurring enterprise patterns:

| | Use case | Why per-Thing matters |
|---|---|---|
| **A** | **Canary / staged rollout** | Try new routing rules or hook chain on one node before fleet-wide |
| **B** | **Region / regulatory difference** | EU node has stricter PII redaction; APAC node has different provider list |
| **C** | **Capacity differentiation** | Larger host carries higher rate-limit / cache size; smaller host stays conservative |
| **D** | **Incident / break-glass** | One node temporarily relaxes a hook or extends an exemption while ops investigates |
| **F** | **Diagnostics** | One node runs verbose logging / sample-rate=1.0 while others stay normal |

Per-Thing override case `E` (dedicated tenant pinning) is **out of scope** — Nexus is a single-tenant
on-prem product and per-tenant node dedication is not a confirmed customer pattern.

In addition, even today's fleet-wide config push has a gap: there is no admin-facing way to
**force a Thing to re-apply config when it claims to already be in sync**. The drift detection
job repushes only when `reported_ver < desired_ver`, but operators routinely need "make this node
re-run its config callbacks NOW" (cache invalidation, suspected silent drift, post-restart sanity).

This spec covers both: per-Thing override CRUD + an explicit force-sync that bypasses the
version-equality short-circuit.

---

## 2. Decisions Snapshot

| Dimension | Decision |
|---|---|
| **Scope** | Per-Thing override only. No group / fleet / type-bulk override |
| **Granularity** | Whole `config_key` JSON replacement (no deep-merge of sub-fields) |
| **Cascade** | 2 layers: `thing_config_template` → per-Thing override |
| **Blacklist** | `credentials` and `virtual_keys` are non-overridable; admin API rejects with 400 |
| **TTL** | Optional per override; `expires_at` range 5 minutes – 30 days; auto-cleanup job clears expired rows |
| **Stale detection** | Snapshot template version at override creation; UI flags when current > snapshot |
| **Force-sync** | Two-tier endpoint: per-key (existing) + per-Thing whole (new). Buttons always visible |
| **Audit** | All writes / clears / auto-expiries / force-syncs go to `admin_audit_log`; **not** `config_change_event` |
| **RBAC** | New IAM actions `admin:WriteThingOverride`, `admin:ForceResyncThing`; type-filtered by role |
| **UI — list page** | New `Overrides` column on `/infrastructure/nodes` + filter chip |
| **UI — detail page** | Merge "Config Sync" + "Applied Config" tabs into one **Configuration** tab (5 tabs total) |
| **UI — editor** | Right-side drawer, two-pane (read-only template / editable JSON) + TTL + reason |
| **UI — global registry** | New `/infrastructure/overrides` page with filters and per-row actions |

---

## 3. Use-Case Walkthrough

Concrete scenarios the implementation must support:

### 3.1 Canary (A)
Operator clones routing_rules template into override on `ai-gateway-canary-01` with a single
new rule. Sets TTL 7 days. After validation either clears override (rolls back) or promotes
the override payload to the template (rolls forward to fleet).

### 3.2 Regional / Compliance (B)
Operator overrides `domain_allowlist` on each EU `compliance-proxy` to add EU-only providers.
Permanent (no TTL). Reason: "EU-allowed providers per compliance MoU 2026-Q1."

### 3.3 Capacity (C)
Operator overrides `quota_overrides` on the `ai-gateway-large-*` instances to raise per-VK
limits. Permanent. Reason: "Capacity tier L."

### 3.4 Break-Glass (D)
SOC operator uses break-glass entry to override `killswitch` to `false` on one
forensic node during a global engagement of killswitch. TTL 4–8h. emergency_override flag
forces a red badge on detail + global page; all admins see it.

### 3.5 Diagnostics (F)
Operator overrides `observability` on a misbehaving node to set `log_level=debug` +
`sample_rate=1.0`. TTL 6h. Reason: "Investigating issue NXS-1234."

---

## 4. Data Model

### 4.1 New table: `thing_config_override`

Source of truth for per-Thing overrides. Carries metadata for stale detection, audit, and TTL.

```sql
CREATE TABLE thing_config_override (
    thing_id            TEXT        NOT NULL REFERENCES thing(id) ON DELETE CASCADE,
    config_key          TEXT        NOT NULL,
    state               JSONB       NOT NULL,
    template_ver_at_set BIGINT      NOT NULL,
    set_by              TEXT        NOT NULL,
    set_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    reason              TEXT,
    expires_at          TIMESTAMPTZ,
    emergency_override  BOOLEAN     NOT NULL DEFAULT FALSE,
    PRIMARY KEY (thing_id, config_key),
    CONSTRAINT chk_tco_reason_len  CHECK (reason IS NULL OR length(reason) <= 500),
    CONSTRAINT chk_tco_expires_set CHECK (expires_at IS NULL OR expires_at > set_at)
);

CREATE INDEX idx_tco_expires ON thing_config_override (expires_at)
    WHERE expires_at IS NOT NULL;
CREATE INDEX idx_tco_thing   ON thing_config_override (thing_id);
CREATE INDEX idx_tco_set_at  ON thing_config_override (set_at DESC);
```

Column semantics:

| Column | Semantics |
|---|---|
| `(thing_id, config_key)` | Primary key — at most one override per (thing, key) pair |
| `state` | Whole-key JSON payload that fully replaces the template's `state` for this Thing |
| `template_ver_at_set` | `thing_config_template.version` at the moment of override creation. Stale flag = `template.version > override.template_ver_at_set` |
| `set_by` | Actor identifier (user id / email / system actor `system:override-expiry-job`) |
| `reason` | Human-readable rationale, max 500 chars (DB-level CHECK + handler-level validation) |
| `expires_at` | NULL = permanent; non-NULL = TTL job auto-clears when `NOW() > expires_at` |
| `emergency_override` | Set when override key is `killswitch`, or `reason` matches `^break-glass:` prefix. Lives as a column on `thing_config_override`; for the audit trail it is mirrored inside `AdminAuditLog.afterState`/`beforeState` JSONB (the audit table has no dedicated column — see §9.1) |

### 4.2 Existing `thing.desired` JSONB — semantic clarification

`thing.desired` remains the **wire-format cache** of effective desired state, computed as:

```
thing.desired = ⟨ for each k where template[t.type, k] exists:
                    override[t.id, k] if present, else template[t.type, k] ⟩
```

This means the existing `thingclient`, `BulkConfigPull` HTTP fallback, and WebSocket
`config_changed` push paths require **zero changes** — they already read the merged result.
The override write path is the only place that recomputes and bumps `desired_ver`.

`packages/nexus-hub/internal/handler/internal_things.go:128-146` already does the right merge
when called with `id=`. The override write/clear path makes that merge proactive instead of lazy.

### 4.3 Blacklist — code-level enforcement

```go
// packages/shared/schemas/configtypes/override_policy.go
package configtypes

// NonOverridableConfigKeys lists per-Thing-override-forbidden keys.
// CP admin handler MUST reject 400 BadRequest on attempts.
var NonOverridableConfigKeys = map[string]bool{
    "credentials":  true, // provider credentials are governed centrally; per-Thing
                          // divergence multiplies leak surface and breaks rotation
    "virtual_keys": true, // VK is tenant identity / billing principal; must be
                          // globally consistent for product semantics to hold
}

// IsOverridable returns false if the key is in the blacklist.
func IsOverridable(configKey string) bool {
    return !NonOverridableConfigKeys[configKey]
}
```

UI grays out blacklisted rows with tooltip: "global-only, override forbidden by policy."

### 4.4 New IAM actions

Added to the IAM action seed:

| Action | Subject |
|---|---|
| `admin:WriteThingOverride` | Set / update / clear per-Thing override |
| `admin:ForceResyncThing` | Trigger force-sync (single key or whole Thing) |

Role assignment matrix:

| Role | `admin:ReadSettings` | `admin:WriteThingOverride` | `admin:ForceResyncThing` |
|---|:---:|:---:|:---:|
| `super_admin` | ✓ | ✓ all types | ✓ all types |
| `provider_admin` | ✓ | ✓ service types only | ✓ service types only |
| `compliance_officer` | ✓ | ✓ agent type only | ✓ agent type only |
| `viewer` | ✓ | — | — |

Type-scope filtering happens in CP handler layer (consistent with `thing-model.md` §11): handler
fetches the target Thing, checks `thing.type` against caller role's allowed types, returns 403
on mismatch.

---

## 5. Write / Clear / Expire Pipelines

### 5.1 Set / update override (admin write)

Single transaction in CP admin handler:

1. Validate inputs:
   - `configKey` not in `NonOverridableConfigKeys`
   - `state` is a JSON object (top-level type), not array / scalar / null
   - `reason` length ≤ 500
   - `expiresAt - NOW() ∈ [5m, 30d]` if non-null
2. Look up `template_ver_at_set` from `thing_config_template (type=thing.type, config_key)` —
   if no template exists for that key, reject 400 (cannot override what was never templated).
3. UPSERT into `thing_config_override` with `set_by = caller`, `emergency_override = (configKey == "killswitch") OR reason starts-with "break-glass:"`
4. Recompute `thing.desired` JSONB by overlaying override state for this key over the
   previous merged snapshot, increment `thing.desired_ver`, write back.
5. Insert `admin_audit_log` row: action=`thing_override_set`, actor=caller, target=`thing/<id>`, new_value=`{configKey, state, reason, expiresAt}`, emergency_override flag mirrored.
6. Invoke `Hub.RePushConfigKey(thing_id, configKey)` (force=true) to deliver immediately.

The Hub call is **after** transaction commit to avoid pushing a state that may roll back.

### 5.2 Clear override (admin DELETE)

1. DELETE row from `thing_config_override`
2. Recompute `thing.desired` (key now reverts to template) and bump `desired_ver`
3. Insert `admin_audit_log`: action=`thing_override_cleared`, actor=caller, before_value=`{state, set_by, set_at}`
4. Hub `RePushConfigKey` (force=true) to deliver the now-template state

### 5.3 Auto-expire (system job)

Extends existing scheduler in `nexus-hub`:

```go
// packages/nexus-hub/internal/jobs/scheduler/jobs/override_expiry.go
// Runs every 60s. SELECT * FROM thing_config_override WHERE expires_at < NOW();
// For each row: same pipeline as 5.2, but with actor = "system:override-expiry-job".
```

Job emits `admin_audit_log` with action=`thing_override_cleared` and
`actorId='system:override-expiry-job'` (same row shape as admin clears, since
the job reuses `Manager.ClearOverride`; the actor is the discriminator that
lets operators audit unexpected expirations). See §9.1.

Job is registered alongside the existing drift-detection / config-sync jobs and shares the
same `job_run` history pattern.

### 5.4 Force-sync (no DB write to override table)

| Endpoint | Behavior |
|---|---|
| `POST /api/admin/things/:id/resync` body=`{}` | For each key in `thing.desired`: send `config_changed` with `force=true`. Returns `{ok, thingId, keyCount}` |
| `POST /api/admin/things/:id/resync` body=`{configKey}` | Single-key force-sync (existing semantics, exposed under admin route) |

Force-sync writes one `admin_audit_log` row per call (action=`thing_force_resync` or
`thing_force_resync_all`) but does **not** write to `config_change_event` — it is redelivery,
not config change.

---

## 6. Stale Detection

A row is **stale** when:

```
thing_config_override.template_ver_at_set <
    thing_config_template.version
        WHERE type = thing.type AND config_key = thing_config_override.config_key
```

Computed by the global-list / per-thing-list query via JOIN (no separate column, no background job).

Surfaced in three places:
- Detail page Configuration tab — yellow ⚠ badge on the override row + tooltip "template updated v=X → v=Y since override was set on {date}"
- List page Overrides column — append `⚠ stale` chip to the count
- Global override page — Status column shows "stale" badge

Stale never blocks anything; it only nudges. Operator decides: re-set the override (re-snapshot
template_ver_at_set), or clear (revert to template).

---

## 7. API Contracts

All endpoints under `/api/admin/things/...`, sit on existing CP admin Echo group with session
auth + IAM middleware. Hub-to-CP bridging follows the existing internal HTTP pattern.

### 7.1 List overrides for one Thing
```
GET /api/admin/things/:id/overrides
→ 200 { overrides: [{configKey, state, templateVerAtSet, currentTemplateVer, stale, setBy, setAt, reason, expiresAt, emergencyOverride}, ...] }
```
IAM: `admin:ReadSettings`. Uses by detail page Configuration tab.

### 7.2 Set / update override
```
PUT /api/admin/things/:id/overrides/:configKey
Body: { state: object, reason?: string, expiresAt?: ISO8601 string }
→ 200 { configKey, state, setBy, setAt, expiresAt, emergencyOverride }
→ 400 { error: "config key not overridable" | "state must be a JSON object" | "reason exceeds 500 chars" | "expiresAt out of range [5m, 30d]" | "no template exists for this key" }
→ 403 { error: "role cannot override this thing type" }
→ 404 { error: "thing not found" }
```
IAM: `admin:WriteThingOverride`.

### 7.3 Clear override
```
DELETE /api/admin/things/:id/overrides/:configKey
→ 200 { ok: true }
→ 404 { error: "no active override for this key" }
```
IAM: `admin:WriteThingOverride`. Returns 404 (not idempotent 200) so callers that expected an
override to exist can detect drift instead of silently succeeding.

### 7.4 Global override list
```
GET /api/admin/things/overrides?type=...&actor=...&hasTtl=true&stale=true&limit=50&offset=0
→ 200 {
    overrides: [{thingId, thingName, thingType, configKey, setBy, setAt, expiresAt, stale, emergencyOverride}, ...],
    total: number,
    summary: { totalNodes, totalOverrides, staleCount, expiringSoonCount }
  }
```
IAM: `admin:ReadSettings`. Powers `/infrastructure/overrides`.

### 7.5 Force-sync
```
POST /api/admin/things/:id/resync
Body: {} | { configKey: string }
→ 200 { ok: true, thingId, keyCount }                       (whole-Thing form)
→ 200 { ok: true, thingId, configKey }                       (single-key form)
→ 404 { error: "thing not found" | "config key not present in thing desired state" }
```
IAM: `admin:ForceResyncThing`.

### 7.6 Extend `applied-config` response with override metadata

The existing `GET /api/admin/things/:id/applied-config` is extended (not replaced) so the
detail page makes one call instead of two. New per-entry fields:

```ts
type AppliedConfigEntry = {
  // existing
  desired: unknown;
  reported: unknown;
  desiredVer: number;
  reportedVer: number;
  inSync: boolean;
  lastChange?: { actor, action, timestamp, emergencyOverride };
  // added
  override?: {
    state: unknown;
    setBy: string;
    setAt: string;        // ISO8601
    reason?: string;
    expiresAt?: string;   // ISO8601, optional
    templateVerAtSet: number;
    stale: boolean;       // currentTemplateVer > templateVerAtSet
    emergencyOverride: boolean;
  };
  templateState: unknown; // template default for the cascade explainer
  templateVer: number;
};
```

`override === undefined` means the key is using template default. UI uses `templateState` for
the read-only left pane in the editor drawer.

---

## 8. UI Changes

### 8.1 `/infrastructure/nodes` — list page

Add **Overrides** column (Option A from brainstorming):

| Existing | New |
|---|---|
| Name, Type, Status, Version, Role, Last seen, Sync | + **Overrides** (count + ⚠ stale chip if any are stale) |

Filter toolbar gets a new chip: **Has overrides** (boolean filter). Sort: by override count.

Click on the count badge → navigate to detail page Configuration tab.

### 8.2 `/infrastructure/nodes/:id` — detail page IA

**Old:** 6 tabs (Overview, Config Sync, Applied Config, Runtime, Metrics, Logs).
**New:** 5 tabs (Overview, **Configuration**, Runtime, Metrics, Logs). "Config Sync" + "Applied Config" merge.

Configuration tab content:

```
[Top toolbar]
  Target v=N · Applied v=N · 2 keys overridden · 1 stale
  [Force resync all]   [+ Add override ▾]   (last action: alice cleared X 4m ago)

[Table — one row per templated config_key]
  Key                     Template default    Override (active)    Applied         Actions
  domain_allowlist  ⚠     {…12}               {…14}                = override      [Edit] [Clear] [Force resync]
    Override set by alice on 2026-04-26 14:22 — template updated v=4→v=6
  hook_config             {…}                 —                    = template      [+ Override] [Force resync]
  credentials  [global-only]  {…}             —                    = template      (disabled, blacklist)
  virtual_keys [global-only] {…}              —                    = template      (disabled, blacklist)
```

- Override row: light purple background, left border purple
- Stale: yellow ⚠ badge inline on key
- Out-of-sync: red ⚠ inline on key (existing behavior)
- Force resync button always visible: text is **"Force resync"** when in-sync, **"Sync now"** when out-of-sync (same endpoint, same `force=true`)
- "+ Add override" / "Edit" / "Clear" all open the right-side drawer

### 8.3 Override editor — right-side drawer

Width: 55% (collapses to 80% on narrow screens). Two-pane body:

```
┌──────────────────────────────────────────────────────────────┐
│ Override · domain_allowlist · compliance-proxy-eu-1       × │
├──────────────────────────────────────────────────────────────┤
│ Template default (read-only · v=6)                          │
│ ┌──────────────────────────────────────────────────────────┐│
│ │ {                                                        ││
│ │   "allowed": ["openai.com", "anthropic.com"]             ││
│ │ }                                                        ││
│ └──────────────────────────────────────────────────────────┘│
│                                                              │
│ Override (editable JSON)        [Reset to template] [Diff]   │
│ ┌──────────────────────────────────────────────────────────┐│
│ │ {                                                        ││
│ │   "allowed": ["openai.com", "anthropic.com",             ││
│ │               "deepseek.com", "zhipu.cn"]                ││
│ │ }                                                        ││
│ └──────────────────────────────────────────────────────────┘│
│                                                              │
│ TTL [Permanent ▾]   Reason [EU additional providers       ] │
│                                                              │
│                                  [Cancel]  [Save override]   │
└──────────────────────────────────────────────────────────────┘
```

Implementation notes:
- JSON editor: reuse the codemirror integration already present in routing-rules / hooks edit UI
- "Reset to template": populates editor with `templateState` JSON
- "Diff view": toggle to side-by-side JSON diff (template vs override)
- TTL picker: presets `4h`, `8h`, `24h`, `7d`, `Permanent`, plus custom; validates `[5m, 30d]`
- `+ Add override` from row → opens drawer with editor pre-filled with `templateState`
- `Edit` from override row → opens drawer with editor pre-filled with current override `state`

### 8.4 New page: `/infrastructure/overrides`

Top-level menu item under Infrastructure (sibling of Nodes / Config Sync / Jobs / Kill Switch).

Sections:
- Header: aggregate counters (`{n} nodes · {m} keys · {p} stale · {q} expiring within 1h`)
- Filter bar: type chips + has-TTL / stale / set-in-last-24h chips + search box (node name or actor)
- Table: see §10b mockup. Columns: Node, Type, Overridden keys, Set by, Set / Expires, Status, Actions
- Per-row actions: View (jump to detail), Force resync, Clear, Extend (expiring soon only)
- No bulk mutation in v1 (single-row only)

### 8.5 Kill-switch override visibility (special case)

When a Thing has an active override on `killswitch` AND the global killswitch is ENGAGED:
- Detail page Configuration tab: red banner at top — `"This node bypasses an active killswitch (engaged at {time}) by override set by {actor} at {time}"`
- List page row: red row stripe + alert badge in Overrides column
- Global override page: row gets red `break-glass` badge (already in §10b mockup)

---

## 9. Telemetry & Audit

### 9.1 Audit actions (admin_audit_log)

The actual `AdminAuditLog` schema (verified against `tools/db-migrate/schema.prisma`)
has columns `id, sequenceNumber, timestamp, actorId, actorLabel, actorRole,
sourceIp, action, entityType, entityId, beforeState (jsonb), afterState (jsonb),
nexusRequestId, clientRequestId, clientUserId, clientSessionId`. Notably it has
**no** `emergency_override` column and **no** `previousHash`/`integrityHash`
columns (the chain-compute code in `audit/writer.go` is wired but the
hub-side `consumer/admin_audit.go insertAdminAuditSQL` does not bind those
fields — see Out-of-scope #2 below). Override fields like `configKey`, `state`,
`emergencyOverride`, `templateVerAtSet` therefore live inside the `afterState`
(or `beforeState` for clears) JSONB blob, not as dedicated columns.

| Action | Triggered by | actor | entityType | Where override fields live |
|---|---|---|---|---|
| `thing_override_set` | Admin PUT | session user | `thing` | `afterState`: `{configKey, state, reason, expiresAt, templateVerAtSet, emergencyOverride}` |
| `thing_override_cleared` | Admin DELETE OR TTL auto-expiry | session user OR `system:override-expiry-job` | `thing` | `beforeState`: `{configKey, priorState, priorSetBy, priorSetAt, priorReason, priorExpiresAt, emergencyOverride}` |
| `thing_force_resync` | Admin POST single-key | session user | `thing` | `afterState`: `{configKey}` |
| `thing_force_resync_all` | Admin POST whole | session user | `thing` | `afterState`: `{keyCount}` |

**TTL auto-expiry uses the same `thing_override_cleared` action** (not a
separate `thing_override_auto_expired`) — implementation reuses
`Manager.ClearOverride`. The discriminator is `actorId =
"system:override-expiry-job"` which never appears as a session user.

**Hub writes the override mutation audit row IN-TX** alongside the
`thing_config_override` upsert/delete and the `thing.desired` recompute, so a
rollback un-writes all three atomically. CP admin handlers therefore MUST NOT
also call `audit.Log` for override mutations (would produce two rows). For
force-sync, Hub does NOT audit (redelivery, not config change), so CP DOES
write the audit row.

### 9.2 Metrics (Prometheus)

New metrics, all on the CP admin handler side, namespace `nexus_admin`:

```
nexus_admin_thing_override_set_total{thing_type, config_key}            counter
nexus_admin_thing_override_cleared_total{thing_type, config_key, by}    counter (by ∈ {admin, auto_expiry})
nexus_admin_thing_override_active{thing_type}                           gauge
nexus_admin_thing_override_stale{thing_type}                            gauge
nexus_admin_thing_force_resync_total{thing_type, scope}                 counter (scope ∈ {key, all})
```

Gauges refreshed by a 60s cron in CP that runs the global-list query — same shape as existing
`nexus_admin_drift_things` style gauges.

---

## 10. Migration / Rollout

Per the project's pre-GA "no backcompat" policy:

1. Single Prisma migration `20260428xxxxxx_thing_config_override`:
   - CREATE TABLE `thing_config_override` (§4.1)
   - INSERT new IAM actions `admin:WriteThingOverride`, `admin:ForceResyncThing`
   - INSERT role-action mappings (§4.4)
2. Single rollout — new endpoints, new UI, new job, new metrics all ship together
3. No data migration needed — `thing.desired` already exists; new table starts empty
4. No feature flag — rollback path is `git revert`

---

## 11. Out of Scope

| Item | Reason |
|---|---|
| Group / fleet / type-bulk override | User decision: case-by-case only |
| Per-tenant pinning (use case E) | Single-tenant product; no confirmed customer |
| Sub-field deep-merge override | Whole-key replacement is industry IoT standard; deep-merge introduces unauditable partial state |
| Bulk clear / bulk force-resync from global page | Misclick risk; v1 single-row only |
| Override approval workflow (2-person rule) | Existing IAM gate considered sufficient; can layer later |
| Override change history beyond audit log | `admin_audit_log` is the history; no separate `thing_config_override_history` table |
| Bulk by type force-resync (`POST /api/admin/things/:type/resync`) | User decision: case-by-case only |

---

## 12. Acceptance Criteria

A story / sub-story plan derived from this spec must, at minimum, satisfy:

1. **Schema**: `thing_config_override` exists with all columns, constraints, and indexes per §4.1
2. **Blacklist**: Setting override on `credentials` or `virtual_keys` returns 400 from CP and is impossible from UI (rows greyed)
3. **Cascade**: After setting override on `(thing X, key Y)`, `GET /api/internal/things/config?type=...&id=X` returns the override state for Y, template state for other keys, and `desiredVer` reflects the bump
4. **Force-sync per-key**: Calling `POST /api/admin/things/:id/resync {configKey}` on an in-sync Thing causes the client to re-run `OnConfigChanged` and emit a fresh shadow report (verifiable via test harness)
5. **Force-sync whole-Thing**: Calling `POST /api/admin/things/:id/resync {}` triggers per-key replay for every key in `thing.desired`
6. **TTL auto-expiry**: A row with `expires_at = NOW() - 1s` is cleared by the next 60s job tick, an `AdminAuditLog` row with action `thing_override_cleared` and `actorId='system:override-expiry-job'` is written, and `thing.desired` reverts the key to template
7. **Stale detection**: After setting an override and then bumping the template version for the same `(type, key)`, the override appears `stale: true` in `GET /api/admin/things/:id/applied-config`
8. **RBAC**: A `provider_admin` cannot override an agent Thing (403); a `compliance_officer` cannot override a service Thing (403)
9. **Audit**: Every set / clear / auto-expire / force-sync writes exactly one `admin_audit_log` row with the actions listed in §9.1
10. **List page**: `/infrastructure/nodes` shows the Overrides column with count + stale chip; `Has overrides` filter chip narrows the list
11. **Detail page**: Configuration tab shows 4-column layout (Key / Template default / Override / Applied) with override rows highlighted + stale badge + force-resync buttons always visible
12. **Editor drawer**: Opens from + Override / Edit row actions; pre-fills template state on add and override state on edit; validates JSON object top-level + TTL range + reason length client-side and re-validates server-side
13. **Global page**: `/infrastructure/overrides` lists all active overrides with filters; per-row View / Force resync / Clear / Extend actions work; no bulk mutation
14. **Killswitch override visibility**: Override on `killswitch=false` while global killswitch is engaged surfaces a red banner on detail page and red row on list page
15. **i18n**: All new user-facing strings have keys in `en/zh/es` locale files; technical terms (`override`, `template`, `TTL`, `JSON`) stay English

---

## 13. Implementation Phases (order of work, not compatibility layers)

Per the project's "no phased rollout" policy, this is **execution order**, not stages with
intermediate ship points:

1. **DB + types** — Prisma migration; Go store types; IAM seed updates
2. **Backend: override CRUD** — admin handlers (PUT / DELETE / GET single + GET global), Hub-side merge recompute on write/clear
3. **Backend: TTL job + force-sync whole-Thing endpoint** — scheduler integration + new resync route
4. **Backend: applied-config extension** — add override metadata to existing endpoint
5. **Frontend: list page column + filter** — minimal touch, lights up the Overrides surface
6. **Frontend: detail page Configuration tab merge + drawer editor** — biggest UI delta
7. **Frontend: global override page** — new route, new query, table
8. **Tests + verify** — `go test -race -count=1`, vitest, manual run against running services

Each phase is its own commit / PR within this single ticket; no phase ships independently to
production.

---

## 14. Open Questions

None — all design decisions captured in §2 and resolved during the brainstorming session
(2026-04-28). If new questions arise during implementation, the corresponding plan must call
them out and re-confirm with the user before silent default.
