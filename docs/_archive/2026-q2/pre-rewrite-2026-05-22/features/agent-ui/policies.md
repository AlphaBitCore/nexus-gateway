# Policies page — Agent UI feature doc

## Purpose

**Admin-pushed config transparency.** The agent is configured remotely; users on the endpoint sometimes need to understand *why* a request was blocked or flagged. This page surfaces the active config in a read-only way.

The Policies page is intentionally informational — it does not allow editing. All changes go through the admin in CP UI.

## Cards

| Card | Shows |
|---|---|
| Active hooks | List of hooks currently enabled with their `onMatch.action` |
| Interception domains | Domain patterns currently in scope |
| Exemptions | Exemption entries delivered via shadow (see "Reality" note below) |
| Rule packs | Rule packs delivered via shadow (see "Reality" note below) |
| Kill switch status | Active / inactive; tier + scope + expiry if active |
| Traffic upload level | Current `agent_settings.trafficUploadLevel` |

## Reality note (binding, surfaced in UI)

Admin exemptions are consulted at hook-decide time and rule packs are applied via shadow state. `core.Engine.Evaluate` short-circuits exempt hosts to passthrough before the hook pipeline; `AgentPipeline.ApplyRulePacksShadowState` injects rule-pack hooks before the pipeline runs.

The remaining narrower gap is `auto_exempt_cert_pinned`, which is not yet implemented. The Policies page surfaces all configured exemptions, rule packs, and hooks as authoritative.

## Data sources

The flattened policy view is read over the daemon statusapi (Unix socket / named pipe, `agent-tray-ipc-architecture.md`). The Wails UI calls into the bridge, which sends a single-line IPC command and reads the JSON snapshot back. No HTTP endpoint is exposed.

## Terminology

- "Hooks", "Interception domains", "Exemptions", "Rule packs" map directly to admin-side names.
- "Kill switch" and "Emergency passthrough" — match admin terminology.

## Failure modes

- **Target config not yet pulled** — show "Initializing..."; auto-refresh after the first pull completes.
- **Out-of-sync with target config** — show a warning if the agent has not yet applied the latest version of a key (user-facing terminology for the underlying desired/reported gap; see CLAUDE.md "IoT terminology boundary").
- **`traffic_upload_level` mid-flip** — banner notes "Policy changed at HH:MM; latest activity reflects new level."

## Architecture references

- `docs/developers/architecture/services/ai-gateway/hook-architecture.md` — Active hooks card
- `docs/developers/architecture/services/agent/agent-forwarder-architecture.md` §7 — reality note canonical source
- `docs/developers/architecture/cross-cutting/foundation/thing-config-sync-architecture.md` — desired/reported state surface
- `docs/developers/architecture/cross-cutting/foundation/multi-endpoint-coordination-architecture.md` §4 — kill-switch propagation
