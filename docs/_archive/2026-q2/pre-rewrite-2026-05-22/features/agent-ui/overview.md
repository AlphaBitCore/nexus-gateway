# Agent UI — Overview & Information Architecture

> Audience: anyone working on the Agent's local UI (Wails + React shell over the Go daemon) or product folks designing flows that surface on the endpoint.

The Agent's UI runs on the user's workstation (system-tray + a window UI). It is intentionally **smaller** than the CP UI — it shows what's happening on **this device** and what an **admin has configured**, not how to administer the platform.

Key product principles:

- **Terminology** — use "traffic event" or "request" (NEVER "AI call" / "LLM call"). The agent is provider-agnostic at this surface.
- **Agent has NO quota concept** — quotas are server-side; the agent does not track or display quota.
- **Read-mostly** — the agent UI surfaces what was configured by the admin. Local toggles are restricted (only Protection Pause and a few user-level prefs).

## Information architecture (memory `project_agent_ui_ia_redesign`)

```
Tray menu (always available)
└── Open dashboard ┐
└── Quick status   │
└── Protection pause (countdown)
                   │
Dashboard window   ▼
├── Overview            (default page; status + recent activity; route `/overview`)
├── Activity            (traffic event log + filters; route `/activity`)
├── Traffic             (cross-cut traffic explorer; route `/traffic`)
├── Stats               (rollup activity summary; route `/stats`)
├── Policies            (admin-pushed config transparency; route `/policies`)
├── Diagnostics         (agent + connectivity + cert + queue depth; route `/diagnostics`)
└── Settings            (user-level preferences; route `/settings`)
```

The dashboard window is the primary surface; the tray menu is the always-on lightweight pulse.

## Section docs in this folder

- `dashboard.md` — main landing (renders the Overview page at `/overview`)
- `activity.md` — traffic event log (`/activity`)
- `policies.md` — admin-pushed policies, exemptions, interception domains (`/policies`)
- `health-diagnostics.md` — agent + connectivity + cert + queue depth (`/diagnostics`)
- `settings.md` — user-level preferences (`/settings`)

## Architecture references

- `docs/developers/architecture/services/agent/agent-forwarder-architecture.md` — what the agent runtime does
- `docs/developers/architecture/services/agent/agent-enrollment-architecture.md` — cert lifecycle visible in Health
- `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md` — macOS safety
- `docs/developers/architecture/cross-cutting/foundation/thing-config-sync-architecture.md` — Policies page reflects pulled config
