---
doc: agent-tray-ipc-architecture
area: service
service: agent
tier: 1
updated: 2026-05-21
---

# Agent Tray IPC Architecture

> **Tier 2 architecture doc.** Read when touching the IPC channel between the agent daemon and the agent UI. The Go client lives in `packages/agent/internal/host/trayipc/`; the daemon-side statusapi server lives in `packages/agent/internal/sync/status/`; the macOS UI is a Wails app at `packages/agent/ui/`.

The agent has two processes: the **daemon** (privileged, does interception + audit) and the **UI** (user-mode tray + Wails window). They communicate over an OS-native local IPC channel. The wire protocol is intentionally minimal: one ASCII command per line, one JSON response per command, no envelope, no event stream.

---

## 1. Why two processes

The daemon needs root / system-level privileges (NETransparentProxyProvider on macOS launched via LaunchDaemon, kernel-level on Linux/Windows). The UI runs as the logged-in user — it should NOT have those privileges. Splitting the processes:

- Limits the privilege blast radius.
- Lets the UI crash or be killed without affecting interception.
- Lets the UI update independently of the daemon.

The IPC channel is the only seam.

## 2. Platform-specific transport

| Platform | Transport | Source of truth |
|---|---|---|
| macOS | Unix domain socket `/var/run/nexus-agent-status.sock` | `paths_darwin.go` |
| Windows | Named pipe `\\.\pipe\nexus-agent-status` | `paths_windows.go` |
| Linux | Per-user Unix socket: `$XDG_RUNTIME_DIR/nexus-agent-status.sock` (fallback chain: `~/.nexus/agent-status.sock` → `/tmp/nexus-agent-status.sock`) | `paths_linux.go` (`linuxStatusSocketPath`) |

All three paths are resolved via `paths.DefaultPaths().SocketPath` (cross-ref `agent-paths-abstraction-architecture.md`). The transport differences are hidden inside `trayipc.dialDeadline`: Windows takes the named-pipe path, the other two dial `net.Dialer.Dial("unix", path)`.

## 3. The protocol — one-line ASCII command, one JSON response

The client (`trayipc.Client.send` in `client.go`) writes a single line to the socket and reads a single JSON line back. There is no `{type, id, method}` envelope and no JSON-RPC framing.

Example exchanges:

```
→  GET_STATUS\n
←  {"state":"connected","stateReason":"","agent":{"deviceID":"...","ssoEmail":"..."},"paused":false,"pausedUntil":""}\n

→  PAUSE_PROTECTION?seconds=300\n
←  {"paused":true,"resumes_at":"2026-05-21T18:05:00Z"}\n

→  SHUTDOWN\n
←  {"acknowledged":true}\n
```

Single-line text on the way in, single JSON object on the way out. The client's `send` method also enforces a 2 s dial deadline (capped) and propagates the caller's context deadline (default 5 s if absent) so a stuck daemon cannot hang the UI's poll loop.

**No server-pushed events.** The agent does NOT push `TrafficEventEmitted` / `ConnectionStateChanged` / `CertNearingExpiry` / `PolicyChanged` / `KillSwitchStateChanged` over this channel. The UI polls `GET_STATUS` on a ticker; changes surface on the next tick.

## 4. The four methods on `trayipc.Client`

| Method | Wire command | Response shape | Purpose |
|---|---|---|---|
| `GetStatus(ctx)` | `GET_STATUS\n` | `Snapshot{ state, stateReason, agent{deviceID, ssoEmail}, paused, pausedUntil }` | Surface daemon health + SSO identity to the tray |
| `PauseProtection(ctx, seconds)` | `PAUSE_PROTECTION` or `PAUSE_PROTECTION?seconds=N\n` | `PauseResponse{ paused, resumes_at, error }` | User-initiated bounded pause |
| `ResumeProtection(ctx)` | `RESUME_PROTECTION\n` | `PauseResponse{...}` | Cancel an active pause |
| `Shutdown(ctx)` | `SHUTDOWN\n` | `ShutdownResponse{ acknowledged, error }` | Request graceful daemon exit (used at logout / uninstall) |

Other statusapi commands (e.g., `OPEN_BROWSER?url=...`) are dispatched by the daemon's statusapi server (`packages/agent/internal/sync/status/statusapi_server.go`) but are **not** surfaced through the typed `trayipc.Client` — they are invoked by other components (the Wails Dashboard for SSO flows) directly via the socket.

## 5. Authentication

Localhost-only socket; no bearer / cookie / token on the IPC channel itself. The macOS socket is created with mode `0666` (see `paths_darwin.go` inline comment) so any logged-in user's tray binary can connect — the daemon performs origin / capability checks **inside the IPC handler** before performing privileged operations (e.g., `SHUTDOWN`). On Linux the per-user socket sits in `$XDG_RUNTIME_DIR` (private to the user already); on Windows the named-pipe ACL restricts to the daemon-user's SID.

For **user SSO**, the Wails UI triggers `OPEN_BROWSER` to start the OAuth+PKCE flow (cross-ref `agent-sso-enrollment-architecture.md`). The IPC channel never carries OAuth secrets — the secret material flows back through the daemon's separate localhost callback server (see `agent-browser-opener-architecture.md` §3).

## 6. Reconnection

The UI opens a fresh connection per request (the `trayipc.Client` is not connection-persistent; each `send` dials, sends, reads, closes). Polling the daemon is therefore stateless from the IPC's perspective:

- Dial failure (daemon down) → propagate the error to the UI, which shows "Daemon not running".
- Socket replaced (daemon restart) → the next poll dials the new socket file successfully.

No "you reconnected" event is needed because there is no persistent connection.

## 7. Versioning

Both processes ship from the same agent binary `.pkg` / `.msi` / `.deb`, so a normal install never has version skew. During an autoupdater rollout the daemon may be briefly newer than the UI. The protocol is intentionally minimal — adding a new field to `Snapshot` is backward-compatible (Go's `json.Unmarshal` ignores unknown fields), and the UI simply doesn't surface anything it doesn't know about. For breaking shape changes the daemon bumps a `schema_version` field in the `GET_STATUS` response and the UI surfaces "Agent update available; relaunch UI".

## 8. Socket file permissions (per platform)

- **macOS**: mode `0666` on `/var/run/nexus-agent-status.sock` (see §5 — the daemon enforces origin checks inside the handler, not via file ACL).
- **Linux**: created inside the per-user XDG runtime dir (or `~/.nexus/` fallback at mode 0700); only the user can reach it.
- **Windows**: named-pipe ACL grants the daemon-user's SID; other users get `ACCESS_DENIED`.

## 9. Failure modes

| Failure | Behaviour |
|---|---|
| Daemon down | Dial fails; UI shows "Daemon not running"; offers restart link. |
| Socket file stale (daemon crashed without cleanup) | New daemon boot recreates the file. |
| Slow IPC (daemon overloaded) | Client deadline (default 5s from ctx) fires; UI shows "Daemon slow"; retry on user action. |
| UI panic | Wails supervisor restarts the UI; daemon unaffected (no persistent connection to break). |
| Daemon panic | Daemon's launchd / systemd supervisor restarts it; next UI poll succeeds. |

## 10. Sources

- `packages/agent/internal/host/trayipc/client.go` — `Client`, `Snapshot`, `PauseResponse`, `ShutdownResponse`, the four methods, `send`, `dialDeadline`.
- `packages/agent/internal/host/trayipc/client_windows.go` — named-pipe dialer.
- `packages/agent/internal/sync/status/statusapi_server.go` — daemon-side server, `GET_STATUS` / `PAUSE_PROTECTION` / `RESUME_PROTECTION` / `SHUTDOWN` / `OPEN_BROWSER` handlers, `SetPauseProtectionFn` / `SetResumeProtectionFn` / `SetOpenBrowserFn` wiring seams.
- `packages/agent/internal/sync/status/status.go` — `StatusSnapshot` shape (superset of `Snapshot`).
- `packages/agent/internal/platform/paths/paths_<goos>.go` — socket-path source of truth.
- `packages/agent/ui/main.go` + `packages/agent/ui/bridge.go` — Wails frontend entry + Go-side `AgentBridge` that the React UI talks to (the Wails JS bridge calls into Go; for `trayipc` it dials the daemon socket via the typed `trayipc.Client`).
- `packages/agent/ui/frontend/src/api/agent.ts` — React-side API surface.

## 11. Cross-references

- `agent-paths-abstraction-architecture.md` — `SocketPath` source.
- `agent-forwarder-architecture.md` — daemon-side forwarder that the UI observes via `GET_STATUS`.
- `agent-sso-enrollment-architecture.md` — OAuth flow that uses `OPEN_BROWSER`.
- `agent-browser-opener-architecture.md` — `OPEN_BROWSER` dispatch + localhost callback server.
- `agent-keystore-architecture.md` — daemon-only keystore access; UI never touches it.
