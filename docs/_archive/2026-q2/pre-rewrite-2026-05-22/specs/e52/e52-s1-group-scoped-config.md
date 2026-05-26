# E52-S1 — Group-scoped config resolution

**Epic:** E52 — Enterprise Device Group capabilities
**Story:** S1 — Group-scoped config resolution
**Requirements:** [e52-device-group-enterprise.md](../../../../docs/developers/specs/e52/e52-device-group-enterprise.md) §S1

## User story

> As a fleet admin, I want to attach different agent_settings,
> hook_config, payload_capture, interception_domains, and kill_switch
> payloads to different device groups, so that compliance policy can
> follow organizational boundaries (Finance gets stricter DLP,
> Engineering gets verbose logs, BYOD gets a different audit policy)
> without forking the agent build per region.

## Architecture impact

Single new resolution layer slotted between
`thing_config_override` (per-device) and `thing_config_template`
(fleet default) in the existing config pull path. No changes to the
shape of what arrives at the agent — the `state` payload is
byte-identical to today's fleet-default payload, just sourced from a
different row.

Existing surfaces that stay unchanged:
- Hub's `GET /api/internal/things/config[/:key]` wire contract.
- Agent's `OnConfigChanged` callback.
- The Cat A / Cat B inline-vs-pull distinction.
- The shadow `desired_ver` bump-and-fan-out flow.

`docs/users/product/architecture.md` gets a 1-paragraph note in the
"Thing config resolution cascade" section. No service-call-framework
change.

## Tasks

### T1.1 — Schema: `device_group_config`

New table mirroring `thing_config_override`:

```sql
CREATE TABLE device_group_config (
    group_id    TEXT REFERENCES "DeviceGroup"(id) ON DELETE CASCADE,
    config_key  TEXT NOT NULL,
    state       JSONB NOT NULL,
    version     BIGINT NOT NULL DEFAULT 1,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by  TEXT,
    PRIMARY KEY (group_id, config_key)
);
CREATE INDEX device_group_config_key_idx ON device_group_config(config_key);
```

Per-group `priority INT NOT NULL DEFAULT 100` lands on `DeviceGroup`
itself (one priority per group, not per config-key — keeps the
resolution rule simple and predictable).

```sql
ALTER TABLE "DeviceGroup" ADD COLUMN priority INTEGER NOT NULL DEFAULT 100;
```

Higher number = higher priority; FleetAdmin can re-rank by editing
this single integer.

### T1.2 — Store: `ResolveConfig` cascade

`packages/nexus-hub/internal/storage/store/config_resolve.go`:

```go
// ResolveConfig returns the effective (state, version, source) for
// (thingID, configKey) using the override → group → fleet cascade.
//
// source ∈ "override" | "group:<group-id>" | "template" so callers
// (CP detail page, audit) can surface provenance.
func (s *Store) ResolveConfig(ctx context.Context, thingID, configKey string)
    (state json.RawMessage, version int64, source string, err error)
```

Implementation = one SQL query with three LEFT JOINs and a COALESCE.

### T1.3 — Hub: rewire pull paths to use `ResolveConfig`

Touch points:
- `internal_things.BulkConfigPull` (`/api/internal/things/config`)
- `internal_things.SingleConfigPull` (`/api/internal/things/config/:key`)
- `thingmgr.RegisterThing` (the `desired = ...` block that seeds
  initial state)

All three call `ResolveConfig` per-key instead of reading directly
from `thing_config_template`.

### T1.4 — Group config bump → desired_ver fan-out

When `device_group_config` is mutated:
1. UPDATE `device_group_config.version = (SELECT MAX(version) FROM
   ... thing_config_template) + 1` (single global counter as today).
2. UPDATE every affected `thing.desired_ver` so the existing Hub
   config-push loop picks them up.
3. Emit one `config_change_event` per affected thing for audit.

The fan-out lives in `internal/store/device_group_config_writer.go`.

### T1.5 — CP admin API

New endpoints under `/api/admin/device-groups/:groupId/config/*`:

| Method | Path | IAM |
|---|---|---|
| GET    | `/configs` | `admin:device-group.read` |
| PUT    | `/configs/:configKey` | `admin:device-group.update` |
| DELETE | `/configs/:configKey` | `admin:device-group.update` |

Implementation in `packages/control-plane/internal/handler/admin_device_group_config.go`.

### T1.6 — CP admin API: provenance on per-thing detail

Extend the existing `/admin/agent-devices/:id/config` response so
each key returns `{ key, state, version, source }` where `source` is
the string from `ResolveConfig`. Renders in the UI as a small
"inherited from $group" badge per row.

### T1.7 — Control Plane UI

`packages/control-plane-ui/src/pages/devices/groups/GroupDetailPage.tsx`
gets a new "Configuration" tab:
- List of config keys with override / inherited badges.
- Click row → JSON editor (existing JSON edit primitive).
- Save → PUT; toast confirms; affected device count shown ("Will
  re-push to 17 devices").

`FleetDeviceDetailPage.tsx` Configuration tab gains a "Source" column
on each rendered key (rendered as small Badge: `inherited`,
`group: Finance`, `device override`).

### T1.8 — Audit + observability

- Each PUT/DELETE on group config emits an `audit_log` row with
  `before` / `after` state.
- `config_change_event` gets a `source` column (`override` / `group` /
  `template`) so SIEM consumers can correlate.

## Acceptance criteria

- AC-1: A new `priority=100` device group with `hook_config` set
  routes that hook payload to every device in the group on the next
  config-push tick. Devices not in the group continue to receive the
  fleet default unchanged.
- AC-2: A device in two groups (priorities 100 and 200) receives the
  payload from the priority-200 group.
- AC-3: Per-device override (set via existing
  `thing_config_override`) wins over any group config.
- AC-4: `GET /admin/agent-devices/:id/config` returns a `source`
  field per key. UI renders the "Inherited from group" badge.
- AC-5: Deleting a group fans `desired_ver` bumps to every former
  member with the new resolved state.
- AC-6: Unit tests cover the 3-way cascade matrix
  (`override?`, `group?`, `template?` — 8 cases, expect 8 distinct
  outcomes).
- AC-7: Integration test stands up 3 devices in 2 groups, mutates
  group config, asserts the Hub config-push log shows the right
  payload landing on the right device.
- AC-8: No regression on the existing `BulkConfigPull` /
  `SingleConfigPull` happy path (cascade is identity when no
  override + no group config).

## Risk + rollout

- **Schema:** additive only (new table + new column on DeviceGroup).
  No backfill required — empty `device_group_config` is the identity
  case.
- **Hot path:** ResolveConfig is one JOIN with three indexed
  lookups. Bench it; reject the PR if p99 > 10ms over a 10k-thing
  table.
- **Backwards compat:** zero. With no rows in `device_group_config`,
  resolution returns the same value as today's
  `thing_config_template` read.
- **Rollout:** deploy Hub first (the new pull-path code), then CP.
  Agents pick up new states on next heartbeat. No agent code change.

## Out of scope

- Group hierarchy (parent→child inheritance) — flat groups only.
- Per-key priority overrides — one `priority` per group.
- Time-bounded group assignments — assignments live until manually
  removed.
- Group-scoped IAM enforcement — covered by E52-S3.
- Smart / dynamic group membership — covered by E52-S2.
