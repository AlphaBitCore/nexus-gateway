# Agent Linux Platform

*Audience: operators deploying the agent on Linux endpoints or servers; contributors touching `packages/agent/platform/linux/`.*

The Linux Desktop Agent intercepts outbound TCP using `iptables` NAT-redirect, runs the shared Go forwarder pipeline (including full TLS bump and content-aware hooks), and manages itself as a systemd unit. Unlike the macOS variant, the Linux agent performs actual TLS termination on intercepted flows, giving it the same content-aware compliance capability as the Compliance Proxy. This page covers the iptables interception model, the systemd unit shape, path conventions, and per-distro packaging.

---

## Interception layer

The agent installs rules into a dedicated `NEXUS_AGENT` chain in the `nat` table and jumps to it from `OUTPUT`. This chain structure cleanly uninstalls (one chain delete restores the host) and avoids touching the default rules.

Per `packages/agent/internal/platform/linux/iptables_linux.go` and `reconciler_linux.go`, the chain looks like:

```text
-N NEXUS_AGENT
-A NEXUS_AGENT -m mark --mark 0x4e58 -j RETURN      # short-circuit the agent's own egress
-A NEXUS_AGENT -d 127.0.0.0/8 -j RETURN             # leave loopback alone
-A NEXUS_AGENT -p tcp -j REDIRECT --to-ports 19080   # redirect to the agent proxy port

-A OUTPUT -j NEXUS_AGENT                             # hook OUTPUT → NEXUS_AGENT
```

The proxy port is **19080** (not 8443).

The primary self-exclusion mechanism is an SO_MARK socket marker: the agent stamps every outbound socket it opens with mark `0x4E58` ("NX" in ASCII), and the chain's first rule returns that mark immediately so the agent's own dials never loop back. A uid-owner fallback rule exists but is not the primary mechanism.

Domain resolution is handled by the daemon; the IP set in the redirect rules is refreshed as DNS answers change, keeping rule churn bounded.

## systemd unit

The agent runs as a systemd unit. Source unit at `packages/agent/platform/linux/installer/systemd/nexus-agent.service`, installed to `/usr/lib/systemd/system/nexus-agent.service` (vendor unit dir).

Key properties of the unit:

```ini
[Service]
Type=simple
User=nexus-agent
Group=nexus-agent
ExecStart=/usr/lib/nexus-agent/nexus-agent run -config /etc/nexus-agent/agent.yaml
Restart=on-failure
RestartSec=10
LimitNOFILE=65536
OOMScoreAdjust=-500
ExecStopPost=/usr/lib/nexus-agent/iptables-cleanup.sh
```

Load-bearing design choices:

- Daemon runs as a dedicated `nexus-agent` user, not root. Privileged work happens in the postinstall script during package installation.
- Capabilities granted: `CAP_NET_ADMIN` (iptables manipulation), `CAP_NET_RAW` (diagnostics), `CAP_NET_BIND_SERVICE` (low ports). Everything else is locked out via `ProtectSystem=full`, `ProtectHome=true`, `PrivateTmp=true`, `NoNewPrivileges=true`.
- `OOMScoreAdjust=-500` protects the daemon from the OOM killer; without this, the `NEXUS_AGENT` iptables chain would outlive the daemon that was supposed to clean it up.
- `ExecStopPost=/usr/lib/nexus-agent/iptables-cleanup.sh` guarantees the chain is unhooked even on a hard kill (SIGKILL or OOM).

Admin overrides live in `/etc/systemd/system/nexus-agent.service.d/` and are preserved on package upgrades.

## Platform paths

All filesystem paths come from `paths.DefaultPaths()` in `packages/agent/internal/platform/paths/paths_linux.go`. Following FHS 3.0:

| Field | Linux path |
|---|---|
| `StateDir` | `/var/lib/nexus-agent` |
| `ConfigDir` | `/etc/nexus-agent` |
| `ConfigFile` | `/etc/nexus-agent/agent.yaml` |
| `LogDir` | `/var/log/nexus-agent` |
| `FlagsDir` | `/var/lib/nexus-agent/flags` |
| `UserQuitFlagPath` | `/var/lib/nexus-agent/flags/user-quit` |
| `DaemonUnitPath` | `/etc/systemd/system/nexus-agent.service` |
| `SocketPath` | `$XDG_RUNTIME_DIR/nexus-agent-status.sock` (with fallbacks) |

The socket path uses a layered fallback: `$XDG_RUNTIME_DIR` (systemd-logind provisioned) → `~/.nexus/agent-status.sock` → `/tmp/nexus-agent-status.sock`. These three options cover both interactive desktop sessions and headless server installs.

Never hardcode these paths in agent code. Any path construction must read from `paths.DefaultPaths()`. See [Agent Enrollment Attestation](Agent-Enrollment-Attestation) for the binding rule and the 2026-05-13 incident that made it explicit.

## Headless mode

On server installs without a desktop environment, `libsecret` (D-Bus Secret Service API) is unavailable. The keystore falls back to an encrypted file at `${ConfigDir}/secrets.enc`, with a boot warning logged.

In headless mode, ops manage the agent via:

- `nexus-agent status` — prints current state.
- `nexus-agent enroll --token <token>` — enrolls the device.
- `systemctl status nexus-agent` and `journalctl -u nexus-agent` — service health and logs.

The agent UI is not needed on headless installs; traffic interception and audit reporting work independently of the UI.

## Per-distro packaging

| Target | Package format |
|---|---|
| Debian / Ubuntu | `.deb` |
| RHEL / CentOS / Fedora | `.rpm` |
| Generic | `.tar.gz` with `install.sh` |

The `build-agent` skill on a Linux host produces all three. The `.deb` and `.rpm` packages drop the systemd unit into the vendor unit directory and run the postinstall script that sets up the `nexus-agent` user, applies capabilities, and starts the service.

---

## Canonical docs

- [`agent-linux-platform-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-linux-platform-architecture.md) — iptables chain shape, systemd unit verbatim, headless mode, self-exclusion via SO_MARK
- [`agent-paths-abstraction-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-paths-abstraction-architecture.md) — Per-platform path values and the forbidden-hardcode rule

**Adjacent wiki pages**: [Agent Overview](Agent-Overview) · [Agent macOS NE Architecture](Agent-macOS-NE-Architecture) · [Agent Windows Status](Agent-Windows-Status) · [Agent Enrollment Attestation](Agent-Enrollment-Attestation) · [Installing The Desktop Agent](Installing-The-Desktop-Agent)
