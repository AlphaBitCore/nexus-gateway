# E52-S2 — Smart / dynamic groups

**Epic:** E52 — Enterprise Device Group capabilities
**Story:** S2 — Smart / dynamic groups (predicate-driven membership)
**Requirements:** [e52-device-group-enterprise.md](../../../../docs/developers/specs/e52/e52-device-group-enterprise.md) §S2

## User story

> As a fleet admin, I want to define a DeviceGroup by predicate
> ("all macOS devices in the 10.32.0.0/16 subnet whose bound user
> belongs to org-path `corp/finance/*`") so membership stays accurate
> as devices enroll, get reassigned, or change network, without me
> hand-editing membership rows.

## Architecture impact

Adds a `membershipQuery` field to `DeviceGroup`. When non-empty, the
group is "smart": membership is computed from the predicate at read
time (and cached) instead of read from `DeviceGroupMembership`. Both
modes coexist — static groups continue to work unchanged.

The predicate evaluator is shared with the routing-rule
matchConditions matcher in `packages/shared/match`. No new
predicate language; no user-supplied SQL; no script execution.
Surface area for review is small.

## Tasks

### T2.1 — Schema: `membershipQuery` + cache

```sql
ALTER TABLE "DeviceGroup" ADD COLUMN membership_query JSONB;
-- non-null => smart group; null => static (uses DeviceGroupMembership rows)

CREATE TABLE device_group_membership_cache (
    group_id    TEXT REFERENCES "DeviceGroup"(id) ON DELETE CASCADE,
    device_id   TEXT REFERENCES thing(id) ON DELETE CASCADE,
    computed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (group_id, device_id)
);
CREATE INDEX device_group_membership_cache_device_idx
    ON device_group_membership_cache(device_id);
```

The cache is the materialized result of evaluating each smart group's
predicate. It exists so reads (config resolution in S1, IAM
resolution in S3) don't re-run the predicate engine — they just JOIN
this table.

### T2.2 — Predicate shape (reuses routing-rule matcher)

```json
{
  "all": [
    { "field": "os", "op": "in", "value": ["darwin", "linux"] },
    { "field": "agentVersion", "op": "ge", "value": "1.5.0" },
    { "field": "primaryIp", "op": "cidr", "value": "10.32.0.0/16" },
    { "field": "boundUserOrgPath", "op": "prefix", "value": "corp/finance/" }
  ]
}
```

Closed attribute set:
`os`, `osVersion`, `agentVersion`, `hostname`, `primaryIp`,
`physicalId`, `status`, `enrolledAt`, `lastHeartbeat`,
`boundUserId`, `boundUserOrgPath`, `metadata.<key>` (escape hatch).

Operators: `eq`, `ne`, `in`, `nin`, `prefix`, `regex`, `cidr`, `lt`,
`le`, `gt`, `ge`, `relative_seconds_within` (for time fields).

`packages/shared/match.EvaluateDevice(predicate, device)` is the
shared call. Re-used by routing rules; same code path; same tests.

### T2.3 — Membership recompute

Three triggers, lightest-touch first:

1. **Per-heartbeat** (on the Hub WS heartbeat handler): for each
   smart group, evaluate predicate against the just-updated thing
   row; UPSERT/DELETE the cache row. <1ms per device, bounded by
   number of smart groups.
2. **On device-attribute change** (hostname / os / primary_ip /
   bound_user_id / metadata): emit `thing_changed` event; cache
   evaluator subscribes.
3. **Hub job `device_group_membership_recompute`** every 60s as a
   safety net for devices that haven't heartbeated.

### T2.4 — Predicate validator + dry-run

CP admin endpoint:

```
POST /api/admin/device-groups/preview-membership
{ "membershipQuery": {...} }
→ { "matched": 17, "sample": ["agent-abc", "agent-def", ...] }
```

Used by the UI to render "this query matches 17 devices" before
saving. Returns up to 50 sample IDs so the operator can sanity-check.

IAM: `admin:device-group.read` (same as listing groups). Doesn't
mutate state.

### T2.5 — CRUD wire-up

`POST/PUT /api/admin/device-groups` accept `membershipQuery` in body.

Switching a group from static→smart deletes its
`DeviceGroupMembership` rows in the same transaction.
Switching smart→static seeds `DeviceGroupMembership` from the
current cache (operator can then prune manually).

### T2.6 — Audit

Every membership add/remove (static OR smart) emits the same
`device-assignment.update` audit action that exists today, with a
`source` field: `manual` / `predicate` / `predicate-removed`.

### T2.7 — CP UI

`GroupListPage`:
- New filter chip: "Smart only" / "Static only" / "Both".
- Column "Type": Badge "Smart" / "Static".
- Column "Members" (existing): count comes from cache for smart, from
  membership rows for static; semantics identical for the UI.

`GroupDetailPage`:
- Mode toggle: Static / Smart radio at top.
- Smart mode → JSON predicate editor + "Preview matches" button →
  calls dry-run endpoint and renders count + sample list.
- Static mode → existing manual add/remove UI.

## Acceptance criteria

- AC-1: Creating a smart group with `os=darwin` predicate yields
  membership = every existing darwin device at next recompute tick.
- AC-2: Enrolling a new darwin device adds it to the cache within
  one heartbeat (≤15s).
- AC-3: Switching a device's `os` (impossible in practice, but
  modeled via test fixture) removes it on next recompute.
- AC-4: A static group's behavior is identical to today (golden test
  asserts byte-equivalence on the membership endpoint).
- AC-5: Preview endpoint returns matched count + ≤50 IDs without
  mutating any DB state.
- AC-6: Invalid predicate (unknown field, unsupported op) returns
  400 with a clear error pointing at the offending node.
- AC-7: A predicate that matches 0 devices is accepted (zero-count
  groups are valid, e.g. quarantine groups that fill as incidents
  fire).
- AC-8: Recompute job idempotent — running it twice in succession
  produces identical cache state.

## Risk + rollout

- **Risk:** A misconfigured predicate could include or exclude the
  wrong devices and silently mis-target group-scoped config from S1.
  Mitigated by the dry-run preview + audit trail on every membership
  change.
- **Performance:** Per-heartbeat evaluation is O(smart_groups). With
  10 smart groups and 10k devices at 15s heartbeat interval, that's
  ~7k predicate eval/s sustained — well under matcher capacity.
- **Schema:** additive only; backward compat is the identity case
  when `membership_query` is NULL.

## Out of scope

- Nested predicates beyond `all` / `any` (no `not`, no arbitrary
  recursion depth). Defer until a customer requests it.
- Multi-attribute predicates on `metadata` jsonb beyond simple
  key/value equals.
- Predicate-driven IAM scoping — IAM scopes by group *id*, not by
  predicate (S3).
