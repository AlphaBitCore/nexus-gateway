---
doc: agent-macos-platform-architecture
area: service
service: agent
tier: 1
updated: 2026-05-21
---

# Agent macOS Platform Architecture

> **Tier 3 architecture doc.** Read when touching `packages/agent/platform/darwin/` or `packages/agent/internal/platform/darwin/pfintercept/`. The fail-open invariants for both the NE path and the pf path live in `agent-ne-fail-open-architecture.md` (Tier 1 — read that first). This doc covers the broader macOS-specific platform layer.

---

## 1. Platform-specific components

| Component | Path |
|---|---|
| NE provider (Swift) | `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/` |
| pf intercept (Go, E74) | `packages/agent/internal/platform/darwin/pfintercept/` |
| Daemon entry (Go) | `packages/agent/cmd/agent/` (`cmd_run.go` and siblings; the `platform/darwin/` tree holds the Swift host + extension + `Package.swift` only) |
| Agent UI (native Swift) | `packages/agent/platform/darwin/NexusAgentUI/` (NOT Wails on macOS; the Wails subtree under `packages/agent/ui/build/darwin/` is build output for non-macOS platforms) |
| LaunchDaemon plist (source) | `packages/agent/platform/darwin/installer/LaunchDaemon.plist` (canonical bundle id `com.nexus-gateway.agent`; `dist/macos/` is build output only) |
| Installer pkg | `dist/macos/NexusAgent-<VERSION>.pkg` (built via `build-agent` skill) |

## 2. Process model

- **LaunchDaemon** (root): the persistent daemon process. Loads at boot.
- **NE provider** (system-extension, `interceptMode="ne"`): kernel-level transparent proxy via NetworkExtension framework. Owned by the daemon.
- **pf listener** (root, `interceptMode="pf"`): daemon-owned loopback listener (`127.0.0.1:13443`) that receives flows redirected by pf anchor `nexus-agent/transparent`. No system extension required.
- **Native Swift UI** (user-mode): the tray + window app under `platform/darwin/NexusAgentUI/`. Optional; UI may not be running.

The daemon owns the active intercept path (NE or pf); the UI talks to the daemon over IPC (cross-ref `agent-tray-ipc-architecture.md`).

## 3. macOS-specific path conventions

Per `agent-paths-abstraction-architecture.md` §2.

## 3a. Interception path (pf) (E74)

This section describes the BSD Packet Filter intercept path introduced in E74 (`interceptMode="pf"`).

### Anchor and parent slot

The daemon installs a private pf anchor named `nexus-agent/transparent`. This occupies a slot in the system pf anchor table without touching the top-level ruleset or any other anchor. The anchor is entirely owned by the daemon — no admin configuration of pf itself is required.

### pf rule shape

The rules file installed into the anchor has the following shape (generated from the Go template in `packages/agent/internal/platform/darwin/pfintercept/pfrules/`):

```pf
# Exclude loopback — never redirect local services
pass out quick on lo0 all

# Exclude the daemon's own outbound traffic (infinite-loop guard, FR-1.5)
pass out quick proto tcp from any to any port {443, 80} user nexus-agent

# Exclude known system services by uid to protect DNS/DHCP/mDNS/NTP/APNS
pass out quick proto tcp from any to any port {443, 80} user mdnsresponder
pass out quick proto tcp from any to any port {443, 80} user configd
pass out quick proto tcp from any to any port {443, 80} user apsd
pass out quick proto tcp from any to any port {443, 80} user ntpd

# Redirect all other outbound TCP 443 to the daemon listener (FR-1.3)
rdr pass on en0 proto tcp from any to !<loopback> port 443 -> 127.0.0.1 port 13443

# QUIC/UDP redirect — only for uids on the quicFallbackUIDs set (FR-1.4)
# One rdr line per resolved uid; omitted when quicFallbackUIDs is empty
# rdr pass on en0 proto udp from any to !<loopback> port 443 user <uid> -> 127.0.0.1 port 13443
```

### pf rule lifecycle

- **Installed** at daemon start via `pfctl -a nexus-agent/transparent -f -` (stdin pipe — atomic anchor replacement; avoids temp-file races).
- **Replaced** on `agent_settings` push: same `pfctl -a … -f -` call; pf replaces anchor contents atomically between kernel ticks; established sockets are unaffected.
- **Removed** at daemon stop via `pfctl -a nexus-agent/transparent -F all`; also runs as the first action on daemon restart (cleanup-before-reinstall).

### Redirect scope

| Traffic class | Behaviour |
|---|---|
| Outbound TCP 443 (general) | Redirected to `127.0.0.1:13443` (FR-1.3) |
| Outbound TCP 80 (when admin enables HTTP capture) | Redirected to `127.0.0.1:13443` (FR-1.3) |
| Outbound UDP 443 — uid in `quicFallbackUIDs` | Redirected to `127.0.0.1:13443` (FR-1.4) |
| Outbound UDP 443 — uid NOT in `quicFallbackUIDs` | Passed through untouched (FR-1.4) |
| Loopback (`127.0.0.0/8`, `::1`) | Excluded — never redirected (FR-1.6) |
| Daemon's own uid (`nexus-agent`) | Excluded — prevents infinite-loop self-interception (FR-1.5) |
| System services (mdnsresponder, configd, apsd, ntpd, …) | Excluded — DNS/DHCP/mDNS/NTP/APNS MUST never be intercepted (FR-1.4, fail-open Rule 5) |

`quicFallbackUIDs` is resolved eagerly from `agent_settings.forceQUICFallbackBundles` at daemon start and on each `agent_settings` push — uids are written directly into pf rules rather than evaluated per-flow.

### Original destination recovery

After pf redirects a flow to the loopback listener, the listener recovers the original destination using the `DIOCNATLOOK` ioctl on `/dev/pf` (`<net/pfvar.h>`), keyed by `(saddr, sport, daddr, dport, PF_OUT)`. The ioctl returns `rdaddr` / `rdport` — the original destination IP and port before pf rewrote them.

The cgo seam is in `packages/agent/internal/platform/darwin/pfintercept/natlook/` (≤80 lines including cgo boilerplate). It exposes a Go interface `OriginalDstResolver.Resolve(localPort, remoteAddr, remotePort) (dstIP, dstPort, error)` so tests can mock the ioctl boundary without requiring root or `/dev/pf`.

### Hot-path call chain

```
kernel rdr (pf)
  → daemon loopback listener (127.0.0.1:13443)
    → DIOCNATLOOK: recover original (dstHost, dstPort)
    → SNI peek (500 ms deadline, FR-2.2)
    → domain.Engine.MatchHost(host)
        → nil: opaqueRelay (passthrough — fail-open)
        → non-nil: BumpFlow(ctx, conn, peekedHello, dstHost, dstPort, flowID, proc, deps)
          → shared/transport/tlsbump.BumpConnection (unchanged MITM pipeline)
```

Per-flow decisions (`inspect → BumpFlow`, `passthrough → opaqueRelay`) are made in-process — no IPC round-trip. Path-level `DENY` runs inside `tlsbump.BumpConnection` after HTTP parse, which is identical to the Compliance Proxy path.

### Process attribution

Per-flow process attribution uses `proc_pidpath` + `proc_pidinfo` via the reused `packages/agent/internal/platform/darwin/proc/processmeta_darwin.go`. A thin `pidfromsocket_darwin.go` bridge recovers the source PID from the socket's uid + port using `proc_listpids` + `proc_pidsocketinfo`. Helper-process attribution targets ≥90% accuracy (NFR-9 in the E74 spec).

## 4. Native APIs the daemon uses

- **Security.framework** (Keychain) — credential storage.
- **NetworkExtension.framework** — NE provider lifecycle (`interceptMode="ne"` only).
- **pf** (`/dev/pf`, pfctl) — pf anchor management (`interceptMode="pf"` only).
- **launchd APIs** (`SMAppService` / `launchctl`) — daemon registration.
- **IOKit** — system identification for device fingerprinting.
- **CoreFoundation** — runloop / event handling in Swift.

These are accessed via cgo from the Go daemon where needed.

## 5. The system-extension lifecycle

### NE path (legacy, `interceptMode="ne"`)

1. Pkg installer drops the extension bundle into `/Library/SystemExtensions/`.
2. User approves in System Settings → Privacy & Security → Extension.
3. macOS activates the extension; daemon registers callbacks.
4. NE intercepts flows.

User approval is **one-time per machine** (until reset). The agent UI nudges the user to approve if it's not yet active.

### pf path (`interceptMode="pf"`, E74)

1. Pkg installer completes without any system extension.
2. No Privacy & Security approval required (DEC-007, NFR-7).
3. Daemon starts, installs pf anchor `nexus-agent/transparent`, binds listener on `127.0.0.1:13443`.
4. pf redirects flows to the daemon listener.

The pf-only build (shipped as `--target=pf-only` `.pkg`) skips the "Allow System Software" gate entirely.

## 6. macOS 26 launch-constraints

Cross-ref `macos-build-signing-architecture.md` §4. The NE refuses to launch if launch-constraints are mis-configured. The `build-agent` skill carries the canonical XML. The pf path does not require NE launch-constraints; it only needs the daemon to run as root (LaunchDaemon, which is the existing deployment model).

## 7. Recovery procedure

### NE path recovery

When the NE deadlocks the network (network-path fail-closed — see `agent-ne-fail-open-architecture.md` §3):

```bash
sudo bash packages/agent/platform/darwin/Scripts/uninstall.sh
sudo reboot  # if network still broken
```

The canonical uninstall script unloads the LaunchDaemon (`com.nexus-gateway.agent`), removes the system extension, and cleans residual state. Avoid raw `launchctl unload` + `rm /Library/LaunchDaemons/...` recipes — they skip the system-extension teardown and leave the host in a partial-uninstall state.

Cross-ref `agent-ne-fail-open-architecture.md` §3.

### pf path recovery

When the pf anchor needs an emergency flush (e.g., daemon crashed and plist was removed before the cleanup-on-restart path could run):

```bash
# Inspect current anchor contents
sudo pfctl -a nexus-agent/transparent -s rules

# Flush anchor — removes all rdr rules; normal routing resumes immediately
sudo pfctl -a nexus-agent/transparent -F all

# Stop daemon and trigger cleanup-on-restart sequence
sudo launchctl unload /Library/LaunchDaemons/com.nexus-gateway.agent.plist
```

If `pfctl -a nexus-agent/transparent -F all` returns an error stating the anchor does not exist, the rules are already flushed — no further action required.

Cross-ref `docs/operators/ops/runbooks/agent-recovery.md` for the operator-facing runbook.

## 8. Cross-references

- `agent-ne-fail-open-architecture.md` — fail-open invariants for both NE and pf paths (Tier 1).
- `macos-build-signing-architecture.md` — build pipeline.
- `agent-paths-abstraction-architecture.md` — path conventions.
- `agent-tray-ipc-architecture.md` — daemon ↔ UI IPC.
- `.claude/skills/build-agent/` — canonical build.
- `docs/operators/ops/runbooks/agent-recovery.md` — operator recovery runbook.

## 8a. Relationship to Compliance Proxy

The macOS Agent and the Compliance Proxy are architecturally **the same forwarding proxy**. The only structural difference is the intercept boundary:

- **Compliance Proxy**: listens on a TCP port; clients route to it explicitly (via system proxy settings or explicit configuration).
- **Agent — macOS (pf path)**: installs pf anchor `nexus-agent/transparent`; the kernel `rdr` rule redirects OS-level traffic to the daemon's loopback listener.
- **Agent — macOS (NE path)**: `NETransparentProxyProvider.handleNewFlow` intercepts flows and forwards them to the Go bridge socket.
- **Agent — Linux**: iptables NAT REDIRECT + `getsockopt(SO_ORIGINAL_DST)` + listener.
- **Agent — Windows**: WinDivert kernel capture + listener.

After the intercept boundary hands a connection to the Go-side pipeline, **everything is shared**. Each intercept layer terminates by delivering a `(net.Conn, peekedClientHello, dstHost, dstPort, flowID, proc, BridgeDeps)` tuple to `shared/transport/tlsbump.BumpConnection`.

### Shared layers

| Layer | Shared package | Purpose |
|---|---|---|
| Decision engine | `packages/shared/policy/domain` | `inspect \| passthrough \| deny` for every flow |
| Hook pipeline | `packages/shared/policy/hooks/`, `packages/shared/policy/pipeline/` | PII / keyword / safety / redact hooks |
| MITM core | `packages/shared/transport/tlsbump/` | TLS termination, cert minting, uTLS upstream dial |
| Normalize / canonicalisation | `packages/shared/transport/normalize/`, `packages/shared/canonicalbridge/`, `packages/shared/canonicalext/` | Provider-agnostic payload extraction |
| Traffic adapters | `packages/shared/traffic/` | Per-provider request / response normalisation |
| Audit event format | `packages/shared/audit/` | Audit type definitions and emission helpers |

### Three-source consistency

This shared-layer model is what makes three-source consistency possible: the same `interception_domain` row produces the same `inspect | passthrough | deny` decision regardless of whether it is evaluated on the AI Gateway, the Compliance Proxy, or the Agent. Cross-ref `endpoint-typology-architecture.md §8.7`.

The structural rule: `shared/policy/domain` is the single decision engine. It MUST NOT be forked into an agent-private variant — any new decision logic lands in `shared/policy/domain` first and is available to all three Things.
