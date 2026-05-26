# Devices section — CP-UI feature doc

> Audience: admins managing the desktop agent fleet. This section enrolls, groups, and configures defaults for agents.

## Pages in this section

| Page | Path | IAM action | Purpose |
|---|---|---|---|
| Devices | `/devices` | (device.read) | List of enrolled agents: device id, status, version, last-seen, drift |
| Device Groups | `/devices/groups` | (device-group.read) | Logical grouping for targeted policy + config |
| Device Auth | `/devices/device-auth` | (device-auth.read) | Enrollment tokens, current device cert validity |
| Device Defaults | `/devices/device-defaults` | (device-defaults.read) | Default agent_settings applied to new devices |

## Common workflows

- **Enroll a new device** — Device Auth → "Issue enrollment token" → choose org / default role / OS constraint → token plaintext shown ONCE. Hand to user. User installs agent → on first boot, agent submits CSR with the token → Hub validates + signs cert. Cross-ref `agent-enrollment-architecture.md`.
- **Group devices** — Device Groups → new → name + selector criteria → save. Policies can target the group.
- **Revoke a device** — Devices → select → "Revoke". CRL updated; agent enters minimal-functionality mode on next call. Cross-ref `agent-enrollment-architecture.md` §7.
- **Set agent defaults** — Device Defaults → edit. Changes propagate via Hub change-signal to all agents in scope. Cat B keys carry `needsPull: true`.
- **Investigate drift** — Devices → select agent → "Config Sync" tab → see desired vs reported per key + per-key applyError.

## Key API endpoints

```
/api/admin/devices                    [GET/PUT/DELETE]; POST /:id/revoke
/api/admin/device-groups              [GET/POST/PUT/DELETE]
/api/admin/enrollment-tokens          [GET/POST]; POST /:id/revoke
/api/admin/device-defaults            [GET/PUT]
```

## Failure modes & gotchas

- **Enrollment token plaintext shown ONCE** — UI carefully highlights this. Token can be re-issued (revoking the old).
- **Auto-update channel** — agent autoupdater pulls signed releases; revoked devices can't auto-update (binary refuses).
- **Long-offline agents** — `agent.offline_reaper` job (cross-ref `jobs-architecture.md`) marks status; UI surfaces "offline since".
- **Stale enrollment token** — admin issued and forgot; the `unused | expired` distinction surfaces in UI. Tokens auto-expire — the default applied when the caller omits `expiresIn` is 24h (`packages/nexus-hub/internal/identity/enrollment/enrollment.go:49` — `expiresIn := 24 * time.Hour`).
- **macOS Network Extension safety** — any change to agent_settings keys consumed by the NE provider needs verification; cross-ref `agent-ne-fail-open-architecture.md` for the binding fail-open contract.

## Architecture references

- `docs/developers/architecture/services/agent/agent-enrollment-architecture.md` — enrollment + CA + cert lifecycle
- `docs/developers/architecture/services/agent/agent-forwarder-architecture.md` — what the enrolled agent does
- `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md` — macOS NE safety contract
- `docs/developers/architecture/cross-cutting/foundation/thing-model.md` — agents are Things
- `docs/developers/architecture/cross-cutting/foundation/thing-config-sync-architecture.md` — config sync mechanics
