# E52-S3 — Group-scoped IAM

**Epic:** E52 — Enterprise Device Group capabilities
**Story:** S3 — Group-scoped IAM enforcement
**Requirements:** [e52-device-group-enterprise.md](../../../../docs/developers/specs/e52/e52-device-group-enterprise.md) §S3

## User story

> As a security admin, I want to grant our Singapore helpdesk team
> the ability to manage devices in the "Singapore" group but not
> Frankfurt's devices, so a single helpdesk admin role is safe to
> hand out without lock-down regret.

## Architecture impact

The IAM NRN grammar already allows path segments after the resource
type — the catalog just hasn't been using them on `agent-device`. The
addition is:

```
nrn:nexus:agent:*:agent-device/group:<group-id>/*
```

A policy with this Resource grants the action only when the target
device is a member of `<group-id>`. The existing unscoped form
`agent-device/*` continues to mean "any device".

Enforcement happens in `packages/shared/security/iam` at request time:
`iamMW` resolves `:id` from the route, looks up the device's group
memberships (via the cache S2 maintains, or the static membership
table), and checks the policy's permitted Resource set against
`agent-device/*` ∪ `agent-device/group:<g>/*` for each `g` the
device belongs to.

List endpoints (`GET /agent-devices`) need the inverse: project the
caller's effective Resource set down to a SQL filter so the listing
query returns only devices the caller can see.

## Tasks

### T3.1 — NRN grammar + catalog

`packages/shared/security/iam` already parses NRN path segments. Add a
documented convention in `nrn.go`:

```
nrn:nexus:agent:<account>:agent-device/group:<group-id>/<device-id-or-*>
nrn:nexus:agent:<account>:agent-device/<device-id-or-*>   (unscoped, today)
```

Catalog (`catalog_data.go`) gets a comment block documenting the
two valid Resource shapes for `agent-device`. No new Verb.

### T3.2 — Membership lookup helper

`packages/shared/security/iam/scope.go` adds a small helper interface:

```go
type DeviceGroupLookup interface {
    GroupsOfDevice(ctx context.Context, deviceID string) ([]string, error)
}
```

Implemented by `*store.DB` in CP — pulls from
`DeviceGroupMembership` (static) UNION ALL
`device_group_membership_cache` (smart). Hot path; cache hits on
30s TTL keyed by `deviceID`.

### T3.3 — `iamMW` extension

`iamauth.IamMW` already builds a request NRN from
`(account, service, resourceType, deviceID)`. Extend it so when the
catalog resource is `agent-device` AND a `:id` path-param is
present, it ALSO builds the group-scoped NRNs:

```
nrn:nexus:agent:*:agent-device/<id>
nrn:nexus:agent:*:agent-device/group:<g1>/<id>
nrn:nexus:agent:*:agent-device/group:<g2>/<id>
...
```

Engine match succeeds if **any** of these NRNs match a policy
Statement Resource pattern. No change to Engine — it already
supports multi-NRN evaluation under the hood (this is how
wildcard policies work today).

### T3.4 — List-endpoint filter projection

New helper `iam.AllowedDeviceScopes(principal)` returns one of:
- `Unrestricted` (caller has `agent-device/*` or `admin:*`),
- `RestrictedToGroups([]string)` (caller has scoped grants only).

`ListThingNodes` (CP store) accepts an optional
`AllowedDeviceScopes` and adds the `JOIN device_group_membership_*`
+ `WHERE group_id IN (...)` clause when restricted.

Same shape for `ListAgentTrafficEvents`, `ListDeviceGroupMemberships`
(only show groups the caller is scoped to).

### T3.5 — Seed: `NexusRegionalDeviceAdmin` canned policy

`packages/control-plane/internal/iam/managed.go` and
`tools/db-migrate/seed/seed.ts` ship a worked example:

```json
{
  "Version": "2026-05-01",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "admin:agent-device.read",
        "admin:agent-device.update",
        "admin:agent-device.delete",
        "admin:agent-device.force-resync",
        "admin:agent-device.rotate",
        "admin:diagnostic-mode.read",
        "admin:diagnostic-mode.update"
      ],
      "Resource": [
        "nrn:nexus:agent:*:agent-device/group:${nexus:GroupId}/*",
        "nrn:nexus:platform:*:diagnostic-mode/*"
      ]
    }
  ]
}
```

`${nexus:GroupId}` is a placeholder the admin substitutes at
attachment time — same pattern the IAM redesign uses for
`${nexus:OrgId}`. The seed ships the policy; the per-region
substitution is a deployment concern.

### T3.6 — Audit + transparency

When a request is denied due to scope, the audit row records:
`outcome=denied`, `reason=device_not_in_scoped_group`,
`scopedGroups=[g1,g2]`, `deviceGroups=[g7,g8]`.

The Allow path records `matchedResource` so SOC consumers can see
which Statement granted the access.

### T3.7 — CP UI: scope hint

`InfraNodeDetailPage` / `FleetDeviceDetailPage` render a small badge
when the caller's access to this device came via a group scope (vs
unrestricted). Helps regional admins understand why they can see a
device.

### T3.8 — Tests

- Unit: `iam.MatchNRN` covers the two grammar forms.
- Unit: `iamMW` with mock `DeviceGroupLookup` covers
  Allow/Deny across 4 cases (unscoped policy + unscoped target,
  scoped policy + member target, scoped policy + non-member target,
  super-admin always allows).
- Integration: a `NexusRegionalDeviceAdmin` user with scope
  `group:singapore-helpdesk` can GET / POST `/agent-devices/:id` for
  members; receives 403 for non-members; sees only members on `GET
  /agent-devices`.

## Acceptance criteria

- AC-1: A policy with Resource
  `nrn:nexus:agent:*:agent-device/group:sg/*` grants action only on
  devices that are members of group `sg`.
- AC-2: Same policy denies the same action on devices not in group
  `sg`, with 403 + clear error message.
- AC-3: `GET /agent-devices` for a scope-restricted principal returns
  only devices the principal's policies match. Total count reflects
  the filtered set (not the full fleet count).
- AC-4: A device in two scoped groups, where the caller has access
  to only one of them, is still visible (any-match semantics).
- AC-5: Smart-group membership flips (a device leaves group `sg` due
  to a predicate change) revoke the scoped principal's access on
  the next 30s cache TTL.
- AC-6: Super-admin's `admin:*` wildcard continues to bypass scope.
- AC-7: Audit row on denied request includes `scopedGroups` and
  `deviceGroups` so SOC can debug "why was this denied".
- AC-8: No regression on existing IAM tests — the unscoped resource
  form continues to work identically.

## Risk + rollout

- **Risk:** A misconfigured policy could lock out legitimate ops.
  Mitigated by AC-6 (super-admin bypass) and AC-7 (clear deny
  reason in audit).
- **Performance:** One extra indexed lookup per
  `agent-device`-scoped request. 30s cache absorbs the burst pattern
  of UI page loads.
- **Schema:** zero changes; uses S1's existing `priority` column and
  S2's `device_group_membership_cache`.
- **Rollout sequence:** must ship after S2 (needs the membership
  cache for fast lookups). Can ship before S1 (no dependency on
  group-scoped config), but the value story is weaker without S1 —
  scoping management without scoping config is half a feature.
- **Backwards compat:** zero. All existing policies use the
  unscoped `agent-device/*` form, which continues to match.

## Out of scope

- Time-bounded scope grants ("Helpdesk has access to this group for
  4 hours during the incident").
- Cross-resource scope joins ("this admin can manage providers AND
  the devices in `group:finance`" — current model handles by listing
  both Statements; not a join).
- Scope-based observability filtering (Tier 2 brainstorm item; not
  in this epic).
