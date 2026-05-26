# Settings page — Agent UI feature doc

## Purpose

User-level preferences for the running agent. Carefully limited to choices the **user** makes; policy decisions remain admin-controlled via shadow.

## Settings

| Setting | Effect |
|---|---|
| Notifications | Tray notifications on blocked traffic / connection lost / cert expiring. On by default. |
| Auto-start at login | macOS LaunchAgent / Windows Startup / Linux desktop autostart entry. On by default. |
| Debug logging | Promote agent log level to DEBUG (verbose; off by default). |
| Theme | System / Light / Dark (UI cosmetic). |
| Language | Display language for the agent UI. Reflects detected OS locale by default. |
| Protection Pause | Temporary, user-initiated pause of interception. Duration is whatever the UI passes (in seconds); `seconds == 0` means indefinite until Resume. |

## What is NOT in Settings

- **Hooks / exemptions / interception domains** — admin-configured; surfaced read-only on the Policies page.
- **Quota** — agents have NO quota concept; quotas are server-side.
- **Hub endpoint** — set at enrollment; changing requires re-enrollment.
- **Traffic upload level** — admin-controlled via shadow; surfaced on the Policies page.

## Protection Pause

Detail because it's the most operationally sensitive user-side toggle:

- Pause accepts any non-negative `seconds` value; the agent has no admin-configurable cap today (`packages/agent/internal/lifecycle/protectionpause/pause.go`). `seconds == 0` pauses indefinitely until Resume; positive values arm a one-shot auto-resume timer with the absolute resume time surfaced via `ResumesAt`.
- The UI is the only ceiling on the duration — admins who want a hard cap should narrow the UI selector; a server-side `maxProtectionPauseDuration` shadow key is the planned admin control (see `agent-protection-pause-architecture.md` §3).
- Pause / Resume record actor strings (`user-paused` / `user-resumed`) into the killswitch snapshot so the protection-pause source is observable. A dedicated structured `agent.protection_pause.*` audit event is not wired today (see `agent-protection-pause-architecture.md` §4).
- Pause does NOT bypass admin policies; it pauses **interception** locally, so traffic flows around the agent.
- The agent UI shows a countdown timer driven by `ResumesAt`.
- An admin can disable user-initiated Pause once the IPC validator + shadow key land (planned).

## Data sources

User preferences persist under `platform.DefaultPaths().ConfigDir`. The UI reads/writes them and drives Protection Pause through the daemon statusapi (Unix socket / named pipe, `agent-tray-ipc-architecture.md`) — the `PAUSE_PROTECTION` / `RESUME_PROTECTION` IPC commands. No HTTP `/local/...` API is exposed.

## Terminology

- "Protection Pause" not "Disable agent". The semantic difference matters: the agent is still running, just not intercepting.

## Failure modes

- **Pause duration too large** — there is no server-side cap today; the agent will accept and arm the timer for whatever the UI submits. The UI selector is the only ceiling.
- **Settings persist failure** — UI surfaces; settings revert to last-known-good on restart.

## Architecture references

- `docs/developers/architecture/services/agent/agent-forwarder-architecture.md` — what "intercept" means; what Pause skips
- `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md` — Pause must not put the network into a worse state than failing-open
- `docs/developers/architecture/services/agent/agent-protection-pause-architecture.md` — Pause lifecycle + the open audit-event follow-up
