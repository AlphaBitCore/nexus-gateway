# E52 — Enterprise Device Group Capabilities

## Why this exists

Today `DeviceGroup` is essentially "a named bag of device IDs with a
domain allow/inspect/deny list". That covers the bottom rung of what
enterprise MDM/UEM products (Intune, Jamf, Workspace ONE, SCCM) do
with groups. The three missing capabilities that turn DeviceGroup from
a list-organizing primitive into a real policy-targeting unit are:

1. **Group-scoped config resolution** — different `agent_settings` /
   `hook_config` / `payload_capture` / `interception_domains` /
   `kill_switch` for different groups.
2. **Smart / dynamic groups** — auto-membership by attribute predicate
   so groups scale past the manual-add ceiling (~50 devices).
3. **Group-scoped IAM** — helpdesk admin can only manage devices in
   the groups their role grants; regional-IT pattern.

This requirements doc covers all three. SDDs live at
`docs/developers/specs/e52/e52-s1-group-scoped-config.md`,
`docs/developers/specs/e52/e52-s2-smart-groups.md`,
`docs/developers/specs/e52/e52-s3-group-scoped-iam.md`.

## Functional Requirements

### S1 — Group-scoped config resolution

FR-S1-1: Operators can attach a `(configKey, state)` payload to any
existing DeviceGroup, for any of the Cat A or Cat B keys currently
managed by `thing_config_template`.

FR-S1-2: When a device requests config via the existing Hub pull path
(`GET /api/internal/things/config[/:key]`), resolution is:
  1. Direct per-Thing override (`thing_config_override`) wins absolute.
  2. Group config wins next — pick the assignment for the highest-
     `priority` group the device is a member of.
  3. Fleet default from `thing_config_template` wins last.

FR-S1-3: Group config is versioned the same way as per-thing config —
a change bumps `desired_ver` on every affected device, the existing
config-push loop fans it out, and `traffic_event` / config history
audit the change.

FR-S1-4: Operators can see, for any device, which group an assigned
config came from ("inherited from group: Finance-Devices").

FR-S1-5: Deleting a group falls every affected device back to fleet
default (or to a higher-priority group it's also in), with a single
audit row per device.

### S2 — Smart / dynamic groups

FR-S2-1: A DeviceGroup may opt into "smart" mode by setting a non-
empty `membershipQuery` predicate. Smart groups have no manual
membership rows; static groups continue to use manual `DeviceGroupMembership`.

FR-S2-2: The predicate language reuses the existing routing-rule
`matchConditions` shape — JSON over a closed attribute set:
  - `os` ∈ {darwin, linux, windows}
  - `agentVersion` (semver comparison)
  - `hostname` (regex / glob / equals)
  - `primaryIp` (CIDR membership)
  - `boundUserId` (equals / IN)
  - `boundUserOrgPath` (prefix)
  - `enrolledAt` (relative-time comparison)
  - `lastHeartbeat` (relative-time comparison)
  - `status` ∈ {online, offline, …}
  - `physicalId` (equals)
  - `metadata.<key>` (string equals — escape hatch for ad-hoc labels)

FR-S2-3: Membership is recomputed on every device heartbeat (cheap
per-device check; <1ms expected) and on a 60s Hub job for devices
that haven't heartbeated recently.

FR-S2-4: Membership changes emit the same audit events as manual
changes (so SOC consumers see one stream).

FR-S2-5: An operator can preview "which devices would match this
predicate" before saving (dry-run endpoint).

### S3 — Group-scoped IAM

FR-S3-1: An IAM policy Statement Resource can scope to a specific
group: `nrn:nexus:agent:*:agent-device/group:<group-id>/*`. (Backward
compat: existing `agent-device/*` resources continue to mean "any
device".)

FR-S3-2: Request-time enforcement: when a handler calls `iamMW` on a
route that operates on a specific device (`/agent-devices/:id/...`),
the middleware resolves the device's group memberships at request
time and the policy match succeeds only if the policy grants either
the unscoped resource OR one of the device's group-scoped resources.

FR-S3-3: List endpoints (`GET /agent-devices`) filter to the union of
devices reachable under any group resource the caller's policy
grants — i.e. scoped helpdesk admins see only their devices.

FR-S3-4: `admin:device-group.update` continues to gate edits to the
group itself (membership management, predicate changes). It does NOT
imply management of the devices in the group — those still need the
device-scoped grant.

FR-S3-5: A new canned policy `NexusRegionalDeviceAdmin` ships in
`packages/control-plane/internal/iam/managed.go` as a worked example.

## Non-Functional Requirements

NFR-1: Config resolution adds at most one indexed lookup per
heartbeat. The hot path stays under 10ms p99 even with 10k devices
and 50 groups.

NFR-2: Smart-group predicate evaluation is bounded — the matcher is
the same one routing rules use today; no new query languages, no
user-supplied SQL, no script execution.

NFR-3: Group-scoped IAM resolution caches membership for 30s per
device per request principal. Cache is invalidated on group
membership change events.

NFR-4: All three stories ship behind no feature flag — pre-GA dev
policy. New columns + indexes only; no destructive schema changes.

## User Roles & Personas

- **Fleet admin** (`NexusFleetAdmin`, super-admin) — full CRUD on
  groups, predicates, and group-scoped configs.
- **Regional IT admin** (`NexusRegionalDeviceAdmin`, new canned
  policy) — read + manage devices in their region's groups only.
  Cannot read or manage other regions' devices.
- **Compliance admin** — read all groups (to attest scope), edit
  hook_config + payload_capture group assignments. Cannot touch
  device-level overrides.

## Constraints & Assumptions

- C-1: Smart group predicates evaluate against attributes already on
  `thing.*` after the Phase 1 identity refactor (hostname, os,
  os_version, primary_ip, physical_id, bound_user_id). No new
  collector code on the agent.
- C-2: Hierarchical groups are explicitly **not** in this epic — flat
  groups + smart-group composition cover the use case at our current
  scale. Hierarchy can land as a later epic if a customer demands it.
- C-3: IamGroup ↔ DeviceGroup binding (smart-group predicate that
  matches an IdP group) is in S2 (via `boundUserOrgPath`); a more
  direct `IamGroup -> DeviceGroup` shortcut is deferred to a follow-on.

## Glossary

- **Smart group**: a DeviceGroup whose membership is computed from a
  predicate, not stored as static rows.
- **Membership query / predicate**: the JSON expression that defines
  a smart group's contents.
- **Group-scoped resource (IAM)**: an NRN whose path segment encodes a
  DeviceGroup id — narrows policy effect to devices in that group.
- **Resolved config**: the per-device config payload produced by the
  override → group → fleet cascade.

## Priority (MoSCoW)

- **Must**: FR-S1-1 through FR-S1-5; FR-S2-1, FR-S2-2, FR-S2-3,
  FR-S2-4; FR-S3-1, FR-S3-2, FR-S3-3.
- **Should**: FR-S2-5 (smart-group preview); FR-S3-5 (canned policy);
  FR-S3-4 (split group-CRUD from device-management permission).
- **Could**: Per-group observability filters (covered as Tier 2 in
  the brainstorm; not in this epic).
- **Won't**: Hierarchical groups; free-form device tags;
  configuration-version pinning per group.
