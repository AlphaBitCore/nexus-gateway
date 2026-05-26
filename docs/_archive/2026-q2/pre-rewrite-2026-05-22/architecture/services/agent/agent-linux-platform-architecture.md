---
doc: agent-linux-platform-architecture
area: service
service: agent
tier: 1
updated: 2026-05-20
---

# Agent Linux Platform Architecture

> **Tier 3 architecture doc.** Read when touching `packages/agent/platform/linux/`. Covers traffic interception on Linux + systemd service + headless-mode considerations.

---

## 1. Interception layer

**`iptables` / `ip6tables` NAT redirect** — the only path on Linux. The daemon installs rules into a dedicated `NEXUS_AGENT` chain that REDIRECT outbound TCP to a localhost port; the daemon listens on that port and proxies. Source: `packages/agent/internal/platform/linux/iptables_linux.go` and the higher-level reconciler in `reconciler_linux.go`.

(pf is the BSD / macOS firewall; it does NOT run on stock Linux distros and is not used here.)

## 2. Process model

- **systemd unit** — the daemon. Source unit at `packages/agent/platform/linux/installer/systemd/nexus-agent.service`. The nfpm `.deb` / `.rpm` installer drops it into `/usr/lib/systemd/system/nexus-agent.service` (vendor unit dir); admin overrides live in `/etc/systemd/system/nexus-agent.service.d/`.
- **No NE-equivalent** on Linux; userspace proxy + iptables is the model.
- **UI** is optional; many Linux installs are server / CI use cases without a desktop UI.

## 3. systemd service shape

The actual unit (verbatim from `packages/agent/platform/linux/installer/systemd/nexus-agent.service`):

```ini
[Unit]
Description=Nexus Agent (compliance + traffic interception daemon)
Documentation=https://nexus-gateway.com/docs/agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=nexus-agent
Group=nexus-agent
ExecStart=/usr/lib/nexus-agent/nexus-agent run -config /etc/nexus-agent/agent.yaml
Restart=on-failure
RestartSec=10
LimitNOFILE=65536

# OOM-killer protection: the iptables NEXUS_AGENT chain otherwise
# outlives the daemon that was supposed to clean it up.
OOMScoreAdjust=-500

# Belt-and-suspenders cleanup on SIGKILL / OOM (idempotent).
ExecStopPost=/usr/lib/nexus-agent/iptables-cleanup.sh

WorkingDirectory=/var/lib/nexus-agent
StateDirectory=nexus-agent
StateDirectoryMode=0750
LogsDirectory=nexus-agent
LogsDirectoryMode=0750

# Sandbox + capability set.
NoNewPrivileges=true
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE
ProtectSystem=full
ProtectHome=true
PrivateTmp=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true

[Install]
WantedBy=multi-user.target
```

Load-bearing notes:

- Daemon runs as a dedicated `nexus-agent` user (not root).
- Capabilities: `CAP_NET_ADMIN` (iptables manipulation), `CAP_NET_RAW` (diagnostics), `CAP_NET_BIND_SERVICE` (low ports).
- `ExecStopPost=/usr/lib/nexus-agent/iptables-cleanup.sh` guarantees the `NEXUS_AGENT` chain is unhooked even on hard kill.
- Sandboxing: `ProtectSystem`, `ProtectHome`, `PrivateTmp`, `NoNewPrivileges` — install-time privileged work happens in postinstall.sh, never at runtime.

## 4. Path conventions

Per `agent-paths-abstraction-architecture.md` §2.

## 5. Native APIs the daemon uses

- **netfilter / iptables** (via shell-out + `golang.org/x/sys/unix` syscalls).
- **SO_MARK** socket option (`marker_linux.go`) for the agent's self-exclusion mark.
- **systemd journal** — slog records flow into journald via `Type=simple` stdout/stderr capture.

The audit DB key on Linux is stored as a `FileStore` 0600 file under `~/.nexus/secrets/` — there is no libsecret / Secret Service integration on the audit-DB path (see `agent-keystore-architecture.md` §2 Linux).

## 6. Headless mode

In headless mode the agent UI is irrelevant; ops manage the agent via:

- `nexus-agent run -config <path>` — the daemon (long-running, via systemd).
- `nexus-agent enroll --token <token>` — one-shot token enrolment (see `cmd_enroll.go`).
- `nexus-agent enroll-sso` — interactive SSO enrolment.
- `nexus-agent unenroll` — local deregistration.
- `journalctl -u nexus-agent` — logs.

## 7. Per-distro packaging

- **Debian / Ubuntu** — `.deb`.
- **RHEL / CentOS / Fedora** — `.rpm`.
- **Generic** — `.tar.gz` with a `install.sh`.

The `build-agent` skill on Linux runs build scripts that produce all three.

## 8. iptables rule shape

The daemon creates a dedicated `NEXUS_AGENT` chain in the `nat` table and jumps to it from `OUTPUT` (cleaner uninstall — one delete restores the host). Per `iptables_linux.go:73` and `reconciler_linux.go:233`, the chain looks like:

```text
-N NEXUS_AGENT
-A NEXUS_AGENT -m mark --mark 0x4e58 -j RETURN          # short-circuit our own egress
-A NEXUS_AGENT -d 127.0.0.0/8 -j RETURN                 # leave loopback alone
-A NEXUS_AGENT -p tcp -j REDIRECT --to-ports 19080      # redirect to the agent proxy port

-A OUTPUT -j NEXUS_AGENT                                # hook OUTPUT → NEXUS_AGENT
```

The redirect target is `proxyPort` (default **19080**, not 8443).

The primary self-exclusion mechanism is the **SO_MARK** socket marker (`marker_linux.go`): the agent stamps every outbound socket it creates with mark `0x4E58` ("NX" in ASCII), and the chain's first rule matches `-m mark --mark 0x4e58 -j RETURN` so the agent's own dials never bounce back into the redirect. The classic `-m owner ! --uid-owner` exclusion is available as a fallback but is not the primary mechanism — `marker_linux.go` is.

Domain resolution is done by the daemon and the rule set is refreshed as IPs change. The agent uses a domain → IP cache to keep rule churn bounded.

## 9. Cross-references

- `agent-forwarder-architecture.md` §2 — platform intercept layers.
- `agent-paths-abstraction-architecture.md` — Linux paths.
- `agent-keystore-architecture.md` §2 — libsecret + headless fallback.
- `agent-runtime-invariants.mdc` — runtime bindings.
