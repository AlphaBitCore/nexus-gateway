# E52 — Device Group Follow-ups (Tier 2 + Tier 3 + S1 Out-of-Scope)

Brainstorm-deferred work that builds on the E52 Tier 1 foundation
(S1 group-scoped config / S2 smart groups / S3 group-scoped IAM)
already shipped at `prod-20260514-hotfix6`. None of these have
current customer demand — they're queued so a future session can
pick whichever lands on a real prospect's ask.

This is one doc per follow-up rather than a sprawling
`docs/developers/specs/e52-sN.md` for each, because (a) each piece is small enough
that a sketch + acceptance criteria is sufficient, and (b) collecting
them keeps the trade-offs visible side-by-side.

## Tier 2 — Customer-asked features beyond Tier 1

### S4 — IdP-group ↔ DeviceGroup binding (AD/SCIM)

**User story:** As a security admin, I want a DeviceGroup to auto-track
membership of an IdP group (Azure AD / Okta / Google Workspace) so
when an employee joins Finance, their enrolled device joins
`finance-devices` without a separate workflow.

**Sketch:**
- Reuse existing `IamGroup` (`source = "scim"`) — the SCIM consumer
  already populates these tables.
- Extend smart-group `membershipQuery` with a new operator `idp_group`
  (`{"field": "boundUserId", "op": "idp_group", "value": "<iam-group-id>"}`)
  that resolves via `IamGroupMember.userId = device.boundUserId`.
- Predicate evaluator gains the lookup; the recompute job already
  re-evaluates everything on its 60s tick.

**Acceptance:** Setting `idp_group: "scim-finance"` on a smart group's
predicate auto-includes every device whose bound user is in that
IdP group, and removes them when SCIM revokes membership.

**Estimate:** ~3-5 days. Risk: low (additive on the smart-group
predicate evaluator; no new schema).

### S5 — Per-group observability dashboards

**User story:** As ops, I want fleet KPIs (latency / hook decisions /
traffic volume) filtered by DeviceGroup so I can answer "are
Singapore devices slower than Frankfurt".

**Sketch:**
- `thing_metric_rollup_5m` already has `thing_id` (E51). Add a
  background materialization that joins membership and writes
  `device_group_metric_rollup_5m (group_id, bucketStart, metricName,
  dimensionKey, value)`. Same shape, partitioned by group.
- New analytics endpoints: `GET /api/admin/analytics/group-summary?groupId=X`.
- UI: new "Groups" tab on Analytics page; filter dropdown.

**Acceptance:** Per-group filter on Analytics returns aggregates
matching the device-list intersection.

**Estimate:** ~1 week. Risk: medium (rollup pipeline change; would
need careful watermark handling for backfill).

### S6 — Per-group compliance reporting

**User story:** As compliance, I need an attestation report showing
"100% of finance-devices have payload_capture=enabled" for SOC2
evidence.

**Sketch:**
- Build on E52-S1 ResolveConfig: for each device in a group,
  evaluate the resolved config for an audited set of keys
  (`payload_capture`, `kill_switch`, `hook_config.enabled`,
  `agent_settings.autoUpdateEnabled`, etc.).
- New endpoint: `GET /api/admin/device-groups/:id/compliance-report?keys=...`
  returns `{total, compliant, byKey: {key → {compliant, nonCompliant}}}`.
- Export as CSV for evidence packs.

**Acceptance:** Report shows accurate compliant/non-compliant counts
matching device-by-device verification.

**Estimate:** ~1 week. Risk: low (read-only aggregation over
ResolveConfig).

### S7 — Hierarchical groups (parent → child policy inheritance)

**Explicitly "Won't" in original E52 MoSCoW.** Documented for
completeness but should not be built without explicit customer ask
because:

- Flat groups + smart-group composition (already shipped) cover ~90%
  of hierarchy use cases without the priority-resolution edge cases.
- Cascade ordering (does child override parent, or merge? per-key
  rules?) gets thorny fast.
- The migration to undo a wrong hierarchy decision is painful.

**Sketch (if forced):**
- Add `parent_group_id` self-FK on DeviceGroup; ON DELETE SET NULL.
- ResolveConfig walks the parent chain bottom-up after evaluating
  direct group memberships.
- UI tree view on Groups page.

**Estimate:** ~2 weeks + ongoing edge-case maintenance.

## Tier 3 — Lower-priority features

### S8 — Version pinning / rollout rings per group

**User story:** As ops, I want canary → beta → stable rollout rings.
Devices in `canary-ring` get new agent builds first; promotion to
`stable-ring` is a separate action.

**Sketch:** Reuse E52-S1. Set `agent_settings.autoUpdateChannel` per
group via group-scoped config. `canary-ring` gets `channel: beta`,
`stable-ring` gets `channel: stable`. No new code — just operator
documentation + a worked example seed.

**Estimate:** ~2 days (mostly docs + verifying the existing
autoUpdate path honors per-group settings).

### S9 — Free-form device tags

**User story:** Ad-hoc labels (`contractor`, `byod`, `executive`)
that compose into smart-group predicates.

**Sketch:** Add `thing.tags TEXT[]` column. Predicate evaluator gains
`tags / contains` operator. UI adds a tag editor on Device detail.

**Estimate:** ~3 days. Risk: low (additive).

### S10 — Bulk-by-group operations

**User story:** Force-resync / rotate-cert / diag-mode entire group
in one call.

**Sketch:** New endpoints `POST /api/admin/device-groups/:id/{force-refresh,rotate-cert,diag-mode}`
that resolve group members then call the existing per-device
admin endpoints concurrently with bounded parallelism. Returns a
per-device result map.

**Estimate:** ~2 days. Risk: low.

### S11 — Lifecycle states

**User story:** Device states (`staging` / `active` / `quarantine` /
`retiring`) with auto-move on event (e.g. compromised → quarantine
group → blocks egress).

**Sketch:** Add `thing.lifecycle_state TEXT` column. Smart groups
predicate on it. Auto-move requires event-driven workflow (e.g.
"on AlertRule X firing → set state Y") which is substantial new
infrastructure.

**Estimate:** ~1 week. Risk: medium (event-driven workflows are
their own can of worms).

### S12 — Per-group alert routing

**User story:** Devices in `finance` alert → `finance-secops` Slack
channel.

**Sketch:** Extend AlertRule with `routing.group_id_filter` →
`destination_id`. Alerting dispatcher checks rule's group filter
against the firing device's memberships before sending.

**Estimate:** ~3 days. Risk: low.

## E52-S1 Out-of-Scope Follow-ups

### S13 — Per-key priority overrides within groups

Currently one `DeviceGroup.priority` ranks ALL config keys. Some
use cases want different rankings per key (e.g. "hook_config from
compliance group, agent_settings from regional group"). Add
`device_group_config.priority_override INT?` column; ResolveConfig
ORDER BY clauses use COALESCE(per-key override, group priority).

**Estimate:** ~2 days.

### S14 — Time-bounded group assignments

Memberships that auto-expire (`releasedAt`-style on
DeviceGroupMembership). Recompute job already considers smart-group
predicates per-tick; extend to also evict expired static rows.

**Estimate:** ~3 days.

### S15 — Time-bounded IAM scope grants ("4-hour incident-response access")

IAM policy attachment with `expires_at`. Engine evaluator filters
attached policies at request time. Critical for break-glass admin
patterns.

**Estimate:** ~1 week. Risk: medium (interacts with policy cache —
need TTL bounded to min(cacheTTL, expiresAt)).

### S16 — Cross-resource IAM scope joins

Single Statement that grants both providers AND devices in
`group:finance`. Today this requires two separate Statements.
Requires evaluator support for compound conditions across
resource types.

**Estimate:** ~1 week. Risk: medium-high (compound conditions are
deep IAM territory).

## Priority signal

None of these have current customer demand. The right next move is
to wait for a real prospect or operator ask, then pick whichever
piece their use case maps to.

If forced to rank without customer signal, my recommendation:

1. **S4 (IdP-group binding)** — highest leverage; composes with
   the SCIM consumer already in production.
2. **S15 (time-bounded IAM)** — break-glass is a common enterprise
   ask; we're one customer's audit away from this being urgent.
3. **S8 (version pinning)** — almost free given E52-S1 is shipped.
4. **S5 (per-group observability)** — natural extension once a
   customer has multiple regions.

Everything else: wait for the ask.
