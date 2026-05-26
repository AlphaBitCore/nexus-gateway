---
doc: agent-paths-abstraction-architecture
area: service
service: agent
tier: 1
updated: 2026-05-21
---

# Agent Platform Paths Abstraction Architecture

> **Tier 2 architecture doc.** Read before touching any code that constructs a filesystem path in `packages/agent/`. The binding rule lives in `.cursor/rules/agent-runtime-invariants.mdc` Rule 1; this doc is the implementation reference.

Agents run on three platforms (macOS / Windows / Linux) with three different filesystem conventions. Hardcoding paths in cross-platform code is a recurring class of build-breakage — every absolute path the agent uses must flow through the single seam below.

---

## 1. The single seam

```go
package paths

// Paths returns the OS-idiomatic locations the agent reads from and
// writes to. Each platform implementation lives in paths_<goos>.go.
type Paths struct {
    StateDir         string // persistent state (certs, audit DB, token files)
    ConfigDir        string // where agent.yaml lives
    ConfigFile       string // ConfigDir + "/agent.yaml"
    LogDir           string // structured slog json + supervisor stdout/stderr
    SocketPath       string // IPC socket / named pipe for tray ↔ daemon
    FlagsDir         string // user-space signal files (e.g. user-quit)
    UserQuitFlagPath string // FlagsDir + "/user-quit"
    DaemonUnitPath   string // launchd plist / systemd unit / Windows service exe
}

// DefaultPaths returns the canonical paths for the current OS.
// Implementations live in paths_darwin.go / paths_linux.go / paths_windows.go.
var DefaultPaths = defaultPaths
```

`paths.DefaultPaths()` is the **single source** for any filesystem path the agent uses. All other code reads from this struct by value. `DefaultPaths` is a package-level `var` bound to the per-platform `defaultPaths()` constructor selected by build tags — there is no `GetDefaultPaths()` constructor and no pointer return.

Source: `packages/agent/internal/platform/paths/paths.go`.

## 2. Per-platform implementations

### macOS (`paths_darwin.go`)

Follows Apple's File System Programming Guide — system-wide third-party state lives under `/Library/Application Support/<bundle-id>/`; logs live under `/Library/Logs/<bundle-id>/`; the LaunchDaemon plist is always `/Library/LaunchDaemons/<bundle-id>.plist`. `bundleID = "com.nexus-gateway.agent"`.

```go
StateDir:         "/Library/Application Support/com.nexus-gateway.agent"
ConfigDir:        "/Library/Application Support/com.nexus-gateway.agent"
ConfigFile:       "/Library/Application Support/com.nexus-gateway.agent/agent.yaml"
LogDir:           "/Library/Logs/com.nexus-gateway.agent"
SocketPath:       "/var/run/nexus-agent-status.sock"
FlagsDir:         "/Library/Application Support/com.nexus-gateway.agent/flags"
UserQuitFlagPath: "/Library/Application Support/com.nexus-gateway.agent/flags/user-quit"
DaemonUnitPath:   "/Library/LaunchDaemons/com.nexus-gateway.agent.plist"
```

The socket file is created under `/var/run/` so the root LaunchDaemon (writer) and any logged-in user's tray binary (connector) can both reach it; the daemon sets mode `0666` on the socket file and origin checks happen inside the IPC handler.

### Windows (`paths_windows.go`)

Follows Windows Application Data conventions: one system-wide directory under `%ProgramData%\NexusAgent\` holds both state and config (no separate Data/Cache split, no per-user cache). The Windows service is registered with SCM; there is no on-disk unit file analogous to systemd, so `DaemonUnitPath` points at the service executable as a stand-in.

```go
StateDir:         `%ProgramData%\NexusAgent`           // e.g. C:\ProgramData\NexusAgent
ConfigDir:        `%ProgramData%\NexusAgent`
ConfigFile:       `%ProgramData%\NexusAgent\agent.yaml`
LogDir:           `%ProgramData%\NexusAgent\Logs`
SocketPath:       `\\.\pipe\nexus-agent-status`
FlagsDir:         `%ProgramData%\NexusAgent\Flags`
UserQuitFlagPath: `%ProgramData%\NexusAgent\Flags\user-quit`
DaemonUnitPath:   `C:\Program Files\NexusAgent\nexus-agent.exe`
```

### Linux (`paths_linux.go`)

Follows FHS 3.0: `/var/lib/<pkg>` for persistent state, `/etc/<pkg>` for system-wide config, `/var/log/<pkg>` for logs, `$XDG_RUNTIME_DIR` (or fallbacks) for the per-user IPC socket, `/etc/systemd/system/<pkg>.service` for the service unit. `pkgName = "nexus-agent"`.

```go
StateDir:         "/var/lib/nexus-agent"
ConfigDir:        "/etc/nexus-agent"
ConfigFile:       "/etc/nexus-agent/agent.yaml"
LogDir:           "/var/log/nexus-agent"
SocketPath:       linuxStatusSocketPath()  // see §5
FlagsDir:         "/var/lib/nexus-agent/flags"
UserQuitFlagPath: "/var/lib/nexus-agent/flags/user-quit"
DaemonUnitPath:   "/etc/systemd/system/nexus-agent.service"
```

## 3. Forbidden patterns

```go
// ❌ all of these are forbidden
quitFlag := "/Library/Application Support/Nexus/.quit"
log.Open("/var/log/nexus-agent.log")
configPath := "C:\\ProgramData\\NexusAgent\\agent.yaml"

// ✅ the correct pattern
quitFlag := paths.DefaultPaths().UserQuitFlagPath
log.Open(filepath.Join(paths.DefaultPaths().LogDir, "agent.log"))
configPath := paths.DefaultPaths().ConfigFile
```

## 4. Why hardcoded paths break cross-platform builds

A hardcoded macOS-style path leaks into Linux and Windows builds three different ways:

- Linux build writes to a path that doesn't exist; any flag-file the path was meant to signal is never honoured.
- Windows build mis-handles the backslash escape OR writes to a non-existent C:\ subtree.
- macOS build "works" — masking the bug until the same code lands on a different platform.

Route every path through `paths.DefaultPaths().<field>`. The Cursor rule `.cursor/rules/agent-runtime-invariants.mdc` Rule 1 plus code review catch attempts.

## 5. Linux socket fallback chain

The single layered fallback in this package is for the per-user IPC socket only. `linuxStatusSocketPath()`:

1. `$XDG_RUNTIME_DIR/nexus-agent-status.sock` — systemd-logind-provisioned per-user runtime dir; the standard place for per-user sockets.
2. `~/.nexus/agent-status.sock` — when `XDG_RUNTIME_DIR` is empty; the helper also creates `~/.nexus` with mode 0700.
3. `/tmp/nexus-agent-status.sock` — last resort when the home dir is unreadable.

There is no general "system path else user fallback" chain for the other path fields — they are absolute and chosen once per platform.

## 6. Tests

`packages/agent/internal/platform/paths/` includes table-driven tests for all three platforms (using build tags to isolate platform-specific code). A new path field requires a test entry per platform.

The CI matrix runs `go test ./packages/agent/...` on macOS, Linux, Windows runners; missing-path tests fail there.

## 7. Adding a new path field

1. Add the field to `Paths` struct in `paths.go`.
2. Populate the field in `paths_darwin.go`, `paths_linux.go`, `paths_windows.go`.
3. Add tests for each platform.
4. Use the new field via `paths.DefaultPaths().NewField` in agent code.

Never define a path constant outside the `paths` package. The agent-runtime-invariants Cursor rule + code review will catch attempts.

## 8. Sources

- `packages/agent/internal/platform/paths/paths.go` — the `Paths` struct + `DefaultPaths` binding.
- `packages/agent/internal/platform/paths/paths_darwin.go` / `paths_linux.go` / `paths_windows.go` — per-platform constructors.
- `.cursor/rules/agent-runtime-invariants.mdc` — IDE binding.
- Memory: `feedback_agent_platform_paths_abstraction` — canonical incident.

## 9. Cross-references

- `agent-forwarder-architecture.md` §8 — consumer.
- `agent-keystore-architecture.md` — keystore stores its own files; not derived from `Paths`.
- `agent-tray-ipc-architecture.md` — `SocketPath` consumer.
- `audit-pipeline-architecture.md` — audit DB lives under `StateDir`.
