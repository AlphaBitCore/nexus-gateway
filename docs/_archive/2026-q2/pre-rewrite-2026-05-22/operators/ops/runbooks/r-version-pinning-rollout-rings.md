# Version Pinning / Rollout Rings (E52-S8)

**Status:** mechanism shipped via E52-S1 group-scoped config + the
existing `agent_settings.autoUpdateChannel` field. No new code; this
runbook documents how operators use the existing primitives to
implement canary → beta → stable rollout rings.

## Why

Default agent updates flow through one global channel
(`agent_settings.autoUpdateChannel`, currently `"stable"`). For
fleet-wide releases this is fine. For sensitive operator workflows
("validate the new build on 10 macs before letting everyone get it")
you want per-group channel pinning.

## Mechanism

E52-S1's per-group `agent_settings` cascade does the heavy lifting:

1. Create three DeviceGroups: `canary-ring`, `beta-ring`,
   `stable-ring`.
2. For each ring, attach a `device_group_config` row keyed on
   `agent_settings` with the appropriate `autoUpdateChannel`.
3. Hub's `SingleConfigPull` for `agent_settings` runs through
   `ResolveConfig`: per-thing override > highest-priority group >
   fleet template. The cascade already exists; rings just sit in
   the group tier.
4. Members are either manually added (static) or picked up by a
   smart-group predicate (E52-S2). Common smart predicate:
   `{"any": [{"field": "tags", "op": "tags_contains", "value": "canary"}]}`
   so ops marks a device as canary by setting a tag.

## Worked example — three rings via curl

Assumes the four DeviceGroups exist (`canary-ring`, `beta-ring`,
`stable-ring` plus the implicit fleet default).

```bash
# 1. Canary ring → beta channel (gets new builds first)
cp_curl /api/admin/device-groups/canary-ring/configs/agent_settings -X PUT \
  -H 'Content-Type: application/json' \
  -d '{"state": {"autoUpdateChannel": "beta"}, "priorityOverride": 300}'

# 2. Beta ring → beta channel, lower priority
cp_curl /api/admin/device-groups/beta-ring/configs/agent_settings -X PUT \
  -H 'Content-Type: application/json' \
  -d '{"state": {"autoUpdateChannel": "beta"}, "priorityOverride": 200}'

# 3. Stable ring → stable channel, lowest priority (acts as
#    fallback for devices in zero rings via the template default).
cp_curl /api/admin/device-groups/stable-ring/configs/agent_settings -X PUT \
  -H 'Content-Type: application/json' \
  -d '{"state": {"autoUpdateChannel": "stable"}, "priorityOverride": 100}'
```

Promotion is just moving a device between rings:

```bash
# Promote dev-mac-01 from canary to beta — remove from canary, add to beta.
cp_curl /api/admin/device-groups/canary-ring/members/agent-mac-01 -X DELETE
cp_curl /api/admin/device-groups/beta-ring/members -X POST \
  -H 'Content-Type: application/json' \
  -d '{"deviceId": "agent-mac-01"}'
```

Or for smart-group-driven rings, just edit the device's tags:

```bash
cp_curl /api/admin/agent-devices/agent-mac-01/tags -X PUT \
  -H 'Content-Type: application/json' \
  -d '{"tags": ["beta"]}'
```

The 60s smart-group recompute job picks up the change; the
cascade fan-out (PUT/DELETE on `/configs/...`) already bumps
`thing.desired_ver` so Hub's config-push loop ships the new
`agent_settings` payload on its next tick.

## Verification

After ring config is set, verify a member device resolves the
right channel:

```bash
# Inside Hub (which reads thing.desired) — or query the DB:
ssh ${PROD_SSH_TARGET} \
  "PGPASSWORD=... psql -h localhost -U nexus -d nexus_gateway -c \
   \"SELECT desired->'agent_settings'->>'autoUpdateChannel' AS channel
       FROM thing WHERE id = 'agent-mac-01';\""
```

Or end-to-end via the Hub pull endpoint:

```bash
curl "https://hub.example.com/api/internal/things/config/agent_settings?type=agent" \
  -H "Authorization: Bearer <device-token>" | jq '.source, .state.autoUpdateChannel'
# Expected: "group:canary-ring", "beta"
```

The `source` field (E52-S1) lets you verify the cascade winner
without comparing payloads.

## When NOT to use

- Sub-day promotions: the auto-update poll cadence is hourly by
  default. For "ship this fix to canary in 5 min", use the
  per-device `thing_config_override` table directly — it wins over
  the group tier.
- Per-platform pinning: this approach pins per-group, not per-OS.
  For "Windows agents stay on stable while macOS goes to beta",
  combine S2 smart groups + this pattern: predicate
  `{"all": [{"field": "os", "op": "eq", "value": "windows"}]}` on
  `stable-ring`.

## Out of scope

- Automatic ring promotion based on telemetry (canary health
  thresholds). The Tier 3 SDD entry covers this — needs an
  event-driven workflow that doesn't exist yet (see E52-S11
  lifecycle states).
- Rollback automation. Reverting a ring's `autoUpdateChannel` row
  via DELETE drops devices back to fleet default; agents pick up
  on next config tick. No special mechanism needed.
