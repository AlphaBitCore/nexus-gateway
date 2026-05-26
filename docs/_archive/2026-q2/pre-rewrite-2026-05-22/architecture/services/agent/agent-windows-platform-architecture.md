---
doc: agent-windows-platform-architecture
area: service
service: agent
tier: 1
updated: 2026-05-20
---

# Agent Windows Platform Architecture

> **Tier 3 architecture doc.** Read when touching `packages/agent/platform/windows/`. Covers WinDivert-based capture + Windows Service registration + Credential Manager integration.

---

## 1. Interception layer

**WinDivert** — a user-mode packet capture driver for Windows. Kernel-mode mini-filter that hands packets to user-mode the daemon process.

The daemon runs `WinDivert.dll` with a filter expression like:

```
outbound and tcp.DstPort == 443 and (ip.DstAddr == <provider-ip-1> or ip.DstAddr == <provider-ip-2> or ...)
```

Matching packets are diverted; the daemon proxies them.

## 2. Process model

- **Windows Service** (`NexusAgent`) — the daemon, runs as `LocalSystem` (or a dedicated service account with `SeDebugPrivilege` for driver access).
- **WinDivert driver** (`WinDivert.sys`) — kernel-mode mini-filter; signed.
- **UI** — Wails app; runs in the user session (NOT the service session).

## 3. Service registration

Canonical install: the WiX-based installer (`packages/agent/platform/windows/installer/NexusAgent.wxs` + `service-config.xml`, driven by WinSW/WiX) registers the `NexusAgent` service, configures Failure Actions, and lays down the binaries under `C:\Program Files\NexusAgent\`.

For diagnostic / dev-only manual registration the equivalent SCM command is:

```
sc create NexusAgent binPath= "C:\Program Files\NexusAgent\nexus-agent.exe run" start= auto
sc description NexusAgent "Nexus Agent — AI traffic governance."
```

Never use raw `sc.exe` for production installs — WiX/WinSW carry the canonical Failure Actions, dependency ordering, and SidType policy.

## 4. Path conventions

Per `agent-paths-abstraction-architecture.md` §2.

## 5. Native APIs the daemon uses

- **WinDivert** (via cgo) — packet capture.
- **DPAPI** (`CryptProtectData` via `crypt32.dll`, `keystore_windows.go`) — wraps the audit DB key; ciphertext lives in `~/.nexus/secrets/<key>.dpapi`.
- **Service Control Manager** (`golang.org/x/sys/windows/svc`) — service lifecycle.
- **Event Log** (`golang.org/x/sys/windows/svc/eventlog`) — log integration.
- **Named pipes** — IPC to the UI via `\\.\pipe\nexus-agent-status` (per `paths_windows.go`).

## 6. WinDivert driver signing

The WinDivert driver itself is signed by its vendor (basinet.io). Windows 10+ requires:

- **WHQL signed** — vendor-signed; out of the box.
- OR **test mode** with self-signed driver — dev only.

The installer pkg includes the signed `WinDivert.sys`. Antivirus software occasionally flags it; the vendor maintains a public reputation registry. Operators may need to whitelist the binary.

## 7. Per-arch builds

- **amd64** — Intel / AMD x64.
- **arm64** — Windows on ARM (Surface Pro X etc.).

The installer pkg detects arch at install time and drops the matching `WinDivert.sys`.

## 8. UAC + admin install

Pkg requires elevation (UAC prompt). The service runs as LocalSystem; the UI runs as the logged-in user.

For multi-user machines, each user gets their own UI session; the service is shared.

## 9. Windows Service Hardening

The service is configured with:

- `Failure actions`: restart on crash (1st failure: restart after 1s; 2nd: restart after 30s; 3rd: alert).
- `SidType: Unrestricted` (default; could be hardened to `Restricted` if no Credential Manager access is needed).

## 10. Cross-references

- `agent-forwarder-architecture.md` §2 — platform intercept layers.
- `agent-paths-abstraction-architecture.md` — Windows paths.
- `agent-keystore-architecture.md` §2 — DPAPI envelope for the audit DB key.
- `agent-tray-ipc-architecture.md` — named-pipe IPC.
- `.claude/skills/build-agent/` — build flow (Windows variant TBD; macOS is the canonical reference today).
