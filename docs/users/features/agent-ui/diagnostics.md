# Agent UI — Diagnostics

Diagnostics is the self-service recovery and health page for the device user. It polls the daemon's diagnostics every five seconds and lays out recovery actions, a health table, and the tail of the agent log. The page is `packages/agent/ui/frontend/src/pages/diagnostics/Diagnostics.tsx`.

## Recovery actions

Three buttons:

- **Restart daemon** — behind a confirmation dialog, asks the daemon to shut down; launchd respawns it within a few seconds, and the Dashboard's reconnecting screen covers the gap. Inline toasts distinguish the request being kicked off, blocked by policy, or failed, and the page refetches once the daemon is back.
- **Copy support bundle** — copies a JSON snapshot to the clipboard: the agent and its state, gateway connectivity, the interception mode, Hub reachability, the config sync state, and the log tail — everything needed to attach to a support request.
- **Copy log path** — copies the agent log's path to the clipboard (the webview cannot open files directly, so the user pastes it into a terminal or Console).

Reinstalling the network extension is not offered here — it lives on the menu-bar app, which carries the system entitlement this view does not.

## Health table

Three rows: whether the Hub is reachable, the certificate path, and the active interception mechanism — `NETransparentProxy` on macOS, `iptables` on Linux, `NexusWFP` on Windows, or `SystemProxyFallback` (a degraded Windows fallback, flagged with a warning).

## Log tail

The last lines of the agent log, rendered inline for quick scanning without leaving the app.

## When the agent isn't running

If the Dashboard can't reach the daemon at all — the bridge is missing, the socket dial fails, or the daemon crashed — the whole UI is replaced by a full-window "agent not running" screen with a Retry button and a copyable recovery command for the one case the daemon can't self-recover from (a deliberate manual stop). Retry re-checks the daemon and hands back to the normal shell once it answers.

## Where the data comes from

`agentApi.getDiagnostics` calls the daemon's diagnostics query over the local bridge every five seconds; **Restart daemon** calls `agentApi.restartDaemon`; and the support bundle composes `getStatus`, `getDiagnostics`, and `getAppliedConfig`. All of it is local to the agent — none of this page talks to the Control Plane admin API.

## References

- `packages/agent/ui/frontend/src/pages/diagnostics/Diagnostics.tsx` — recovery actions, health table, log tail
- `packages/agent/ui/frontend/src/pages/diagnostics/AgentNotRunning.tsx` — the agent-not-running fallback screen
- `packages/agent/ui/frontend/src/api/agent.ts` — `agentApi.getDiagnostics` / `restartDaemon` and the `Diagnostics` shape
