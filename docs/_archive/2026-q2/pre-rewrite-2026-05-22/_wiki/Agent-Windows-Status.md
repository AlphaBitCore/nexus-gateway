# Agent Windows Status

*Audience: operators evaluating Windows endpoint coverage; contributors who will implement or extend Windows-platform agent functionality.*

The Windows Desktop Agent intercepts outbound traffic using WinDivert — a user-mode packet capture driver that hands packets to the daemon process for inspection and forwarding. The Windows variant ships a Go daemon + WinDivert kernel driver + a Wails-based UI, packaged as a WiX MSI with a full install/uninstall wizard. This page covers the interception model, service structure, MSI contents, and the build path for Windows distributions.

---

## Interception layer

**WinDivert** is a kernel-mode mini-filter (vendor: basil00, `basinet.io`) that accepts a filter expression from user-mode and diverts matching packets to a user-mode receiver. The Nexus Agent daemon runs WinDivert with a filter scoped to outbound TCP on port 443 destined to provider IPs:

```
outbound and tcp.DstPort == 443 and (ip.DstAddr == <provider-ip-1> or ...)
```

Matching packets are diverted to the daemon, which proxies them through the same Go forwarder pipeline used on Linux and macOS. The daemon includes full TLS bump capability, giving the Windows agent content-aware hook execution.

Unlike the macOS NE, WinDivert requires the kernel driver (`WinDivert.sys`) to be installed and started as a Windows service. The MSI installer handles this automatically; the kernel driver is a dependency (`ServiceDependency`) of the `NexusAgent` service.

## Process model

| Process | Account | Notes |
|---|---|---|
| `NexusAgent` (Windows Service) | `LocalSystem` (or a dedicated service account with `SeDebugPrivilege`) | The daemon. Owns WinDivert and the Go forwarder. |
| `WinDivert` (kernel service) | System | The kernel-mode driver. `sc query WinDivert` should show `STATE: 4 RUNNING`. |
| `nexus-agent-tray.exe` | Logged-in user | Tray UI. Started via `HKLM Run` key at user login. |
| `nexus-dashboard.exe` | Logged-in user | Dashboard UI. Launched from tray "Open Dashboard". |

The service runs at `LocalSystem` account; the tray and dashboard run in the user session. On multi-user machines, each user gets their own UI session; the service is shared.

## What the MSI installs

| Artifact | Destination |
|---|---|
| `nexus-agent.exe` | `C:\Program Files\Nexus Agent\` |
| `nexus-agent-tray.exe` | `C:\Program Files\Nexus Agent\` |
| `nexus-dashboard.exe` | `C:\Program Files\Nexus Agent\` |
| `WinDivert64.sys` | `%SystemRoot%\System32\drivers\` |
| `WinDivert.dll` | `C:\Program Files\Nexus Agent\` |
| `%ProgramData%\NexusAgent\` | Empty directory (daemon writes `device-ca.pem`, `agent.yaml`, `Logs\` here on first boot) |

The MSI also sets four HKLM environment variables so SDK and browser processes automatically pick up the device CA certificate:

- `NEXUS_DEVICE_CA_PEM` — path to `%ProgramData%\NexusAgent\device-ca.pem`
- `NODE_EXTRA_CA_CERTS` — same (Node.js SDK auto-trust)
- `REQUESTS_CA_BUNDLE` — same (Python `requests` auto-trust)
- `SSL_CERT_FILE` — same (OpenSSL-based tools)

These variables are set with `Action="create"` — they are not overwritten if the user has already set them.

## Build path

**Windows builds must run on a Windows host.** The Wails dashboard requires Windows-only WebView2 host headers and CGO; WiX v4 currently lacks a Linux MSI codepath that produces a valid Windows Installer database. Cross-building from macOS or Linux is not supported.

```powershell
$env:VERSION = '1.0.0'
pwsh -NoProfile -File packages/agent/platform/windows/scripts/build.ps1
pwsh -NoProfile -File packages/agent/platform/windows/scripts/sign.ps1
pwsh -NoProfile -File packages/agent/platform/windows/scripts/package.ps1
```

For signed releases, set `WINDOWS_CERT_PATH` and `WINDOWS_CERT_PASSWORD` before running `sign.ps1`. The CI workflow (`.github/workflows/agent-release.yml`) runs the full chain on `windows-2022` runners for tag pushes and produces a signed MSI as a workflow artifact.

## WinDivert driver signing and EDR considerations

WinDivert ships pre-signed by its vendor (WHQL-signed). Antivirus or EDR products (CrowdStrike, SentinelOne, Defender for Endpoint) occasionally flag or block WinDivert because it is a packet-capture driver. Operators may need to add a per-vendor allowlist entry for WinDivert v2.2.2 (published by basil00, cross-cert chain `A6:62:F3:6C:...`).

If the MSI install hangs at "Starting services", check Event Viewer → Applications and Services Logs → Microsoft → Windows → CodeIntegrity for EDR block events.

If `WinDivert` service is stuck in `STOP_PENDING` after a crashed uninstall, do not force-kill the driver process — that has caused `DRIVER_POWER_STATE_FAILURE` BSODs at next boot. Instead, reboot first (drops the kernel driver cleanly), then run the MSI uninstaller.

## Path conventions

All agent filesystem paths come from `paths.DefaultPaths()` in `packages/agent/internal/platform/paths/paths_windows.go`, following Windows Application Data conventions:

| Field | Windows path |
|---|---|
| `StateDir` | `%ProgramData%\NexusAgent` |
| `ConfigDir` | `%ProgramData%\NexusAgent` |
| `ConfigFile` | `%ProgramData%\NexusAgent\agent.yaml` |
| `LogDir` | `%ProgramData%\NexusAgent\Logs` |
| `SocketPath` | `\\.\pipe\nexus-agent-status` (named pipe) |
| `FlagsDir` | `%ProgramData%\NexusAgent\Flags` |
| `UserQuitFlagPath` | `%ProgramData%\NexusAgent\Flags\user-quit` |
| `DaemonUnitPath` | `C:\Program Files\NexusAgent\nexus-agent.exe` |

Hardcoding paths in agent code is forbidden. See `agent-paths-abstraction-architecture.md` for the binding rule.

---

## Canonical docs

- [`agent-windows-platform-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-windows-platform-architecture.md) — WinDivert model, service registration, Credential Manager, named-pipe IPC
- [`agent-windows-build.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/agent-windows-build.md) — MSI build, sign, package scripts; CI workflow; install/verify/uninstall procedures; fail-recovery
- [`agent-paths-abstraction-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-paths-abstraction-architecture.md) — Per-platform path values and the forbidden-hardcode rule

**Adjacent wiki pages**: [Agent Overview](Agent-Overview) · [Agent macOS NE Architecture](Agent-macOS-NE-Architecture) · [Agent Linux Platform](Agent-Linux-Platform) · [Agent Enrollment Attestation](Agent-Enrollment-Attestation) · [Installing The Desktop Agent](Installing-The-Desktop-Agent)
