# Dashboard page — Agent UI feature doc

## Purpose

Default landing surface. Answers three questions at a glance:

1. **Is the agent healthy?** — connection status, cert validity, queue depth.
2. **Is policy active?** — kill-switch / emergency-passthrough banner if relevant.
3. **What's been happening?** — recent traffic event summary (last N, last 1h, last 24h).

## Layout

```
┌──────────────────────────────────────────────────────────────┐
│ Status: ● Connected   |   Cert valid: N days   |   Queue: 0  │
├──────────────────────────────────────────────────────────────┤
│ Banner (only when relevant):                                  │
│   ⚠ Emergency passthrough active until HH:MM (set by admin)   │
├──────────────────────────────────────────────────────────────┤
│ Recent activity (last 1h)                                     │
│   N traffic events processed                                  │
│   N blocked  •  N flagged  •  N allowed                       │
│   [Open Activity →]                                           │
├──────────────────────────────────────────────────────────────┤
│ Sparkline: traffic events / minute (24h)                       │
└──────────────────────────────────────────────────────────────┘
```

## Data sources

The UI fetches everything over the daemon's localhost statusapi (Unix socket / named pipe, see `agent-tray-ipc-architecture.md`):

- `GET_STATUS` — connection state, SSO identity, paused state. Cert validity + queue-depth values come from the local rollup tables (`agent-backpressure-rollup-architecture.md`) surfaced via the same IPC.
- Recent-activity counters are derived from the agent's localrollup 5m / 1h / 1d bins via `QueryRollup` on the same channel.

There is no separate `GET /local/...` HTTP API — every UI read is IPC.

## Terminology

- "Traffic event" (not "AI call"). Agent is provider-agnostic at this surface.
- "Blocked / Flagged / Allowed" map to the canonical Hook decisions (cross-ref `hook-architecture.md`).
- No "quota" anywhere on this page.

## Failure modes

- **Status: Disconnected** — show "since HH:MM"; auto-retry indicator. Recent activity still shows from local rollup.
- **Cert near expiry** — warning surfaced via the daemon's status snapshot; renewal today is operator-initiated (the daemon exposes `Manager.Renew`; an auto-rotation scheduler is not wired — see `agent-enrollment-architecture.md` §6).
- **Queue depth above the configured `HighWatermark` (default 500, `agent-backpressure-rollup-architecture.md` §2)** — warning, indicates audit upload backpressure.

## Architecture references

- `docs/developers/architecture/services/agent/agent-forwarder-architecture.md` — local pipeline emitting events
- `docs/developers/architecture/services/agent/agent-enrollment-architecture.md` — cert renewal
- `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md` — local queue + drain
