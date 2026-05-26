# Agent macOS NE Architecture

*Audience: contributors touching `packages/agent/platform/darwin/`; security reviewers auditing the fail-open posture of the macOS NE provider.*

The macOS Desktop Agent intercepts outbound network flows using Apple's `NETransparentProxyProvider` — a Network Extension that the OS routes every outbound packet through. Because this extension sits in the host's outbound packet path, a misbehaving provider does not slow down the machine; it takes down the entire Mac's network: DNS, DHCP, mDNS, NTP, Apple Push, VPNs. This page covers the five fail-open invariants that govern every change to `TransparentProxyProvider.swift`, plus the build/signing chain required to ship a correct binary.

---

## The five fail-open invariants (safety-critical)

These five rules are binding on every change to `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/TransparentProxyProvider.swift` and its dependents. Violation of any rule can brick the network on an affected machine; recovery requires manual uninstall and a reboot.

### Rule 1 — `handleNewFlow` decides synchronously

`handleNewFlow` is the OS callback that decides whether the NE claims a flow. It must decide synchronously and return `true` only for flows it can fully relay. For UDP flows with no relay implementation, `return false` for unknown bundles.

Never `return true` "to look at it later" — once claimed, the flow belongs to the NE forever. If the NE claims a flow and does not relay it, the flow stalls and the user loses connectivity. The OS routes unclaimed flows (`return false`) through the default path; the agent loses visibility but the user keeps their network.

### Rule 2 — every async daemon callback has a fail-open timeout

Some flows must ask the daemon for a policy decision. The daemon may be slow or absent. Every async callback must have a fail-open timeout rather than blocking indefinitely:

- `requestDecision` defaults to `passthrough` after **2 s** — verified at `IPCProtocol.swift:328-351` where a `queue.asyncAfter(deadline: .now() + 2.0)` fires a synthetic `passthrough` if the daemon does not respond.
- `peekSNIThenRelay` falls through to plain relay after **500 ms** — verified at `TransparentProxyProvider.swift:309-317, 631-639`.

A degraded agent with an unresponsive daemon still relays all flows. Degraded visibility is acceptable; broken networking is not.

### Rule 3 — no hardcoded enforcement lists in NE Swift code

Bundle-ID allowlists, domain allowlists, and any admin-controlled enforcement sets must not appear as literals in Swift source. The correct pattern:

1. Hub pushes the list in the `agent_settings` shadow blob.
2. The daemon writes the list to a file (for example, `/var/run/nexus-agent/quic-bundles.json`).
3. The NE provider reads the file **file-only**, with **empty-as-fail-safe**: an empty file means enforcement off, not enforcement everywhere.

A hardcoded fallback would silently override admin policy. When an admin removes a bundle in the CP UI, the file becomes empty; the NE reads empty and stops enforcing. Anything else breaks the admin's mental model of what the UI controls.

### Rule 4 — `isLikelyXyz = true` patterns are banned

A flag named `isLikelyXyz` set unconditionally to `true` with a comment to "make it real later" is a time bomb. Write the real condition or explicitly return `false`:

```swift
// Real condition:
let isLikelyQUIC = (proto == .udp && (dst == 443 || dst == 80) && payload.matches(.quicInitial))

// Explicit not-yet-implemented:
let isLikelyQUIC = false
// QUIC detection not yet implemented; default to false.
```

A `= true` flag with no real condition looks intentional in review but breaks the moment the NE encounters a flow matching the unmarked-true case.

### Rule 5 — system DNS/DHCP/Push services must never have UDP closed

The following process names must never appear in any kill-list, deny-bundle-from-egress, or blocked-process set:

- `mdnsresponder`
- `configd`
- `dhcpcd`
- `apsd`
- `nsurlsessiond`
- `kdc`
- `ntpd`

Validation happens in the daemon (which controls the file-only enforcement list written per Rule 3), not in the NE Swift code. Any process added to a kill-list must be validated against this list before shipping. If a flow originates from any of these processes, always relay — even if the destination appears in a deny list.

## Process model and component layout

| Component | Path |
|---|---|
| NE provider (Swift) | `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/` |
| Daemon entry (Go) | `packages/agent/cmd/agent/` |
| Native Swift UI | `packages/agent/platform/darwin/NexusAgentUI/` |
| LaunchDaemon plist | `packages/agent/platform/darwin/installer/LaunchDaemon.plist` |
| Installer pkg | `dist/macos/NexusAgent-<VERSION>.pkg` (built via `build-agent` skill) |

Three processes run on a deployed macOS agent: the **LaunchDaemon** (root, loads at boot), the **NE provider** (system extension, owned by the daemon), and the **native Swift UI** (user-mode, optional). The daemon owns the NE; the UI communicates to the daemon over IPC.

The system-extension lifecycle requires one-time user approval in System Settings → Privacy & Security → Extensions. The agent UI prompts for this approval if the extension is not yet active.

### Native APIs

The daemon uses the following macOS-specific APIs (accessed via cgo from Go where needed):

| API | Purpose |
|---|---|
| `Security.framework` (Keychain) | Credential and device cert storage |
| `NetworkExtension.framework` | NE provider lifecycle management |
| `SMAppService` / `launchctl` | Daemon registration with launchd |
| `IOKit` | System identification for device fingerprinting |
| `CoreFoundation` | Runloop and event handling in Swift |

### macOS limitations vs Linux/Windows

The macOS NE only surfaces connection metadata to the Go daemon — host, IP, port, and process bundle ID. The agent on macOS performs policy decisions and metadata-level audit but does **not** TLS-bump and does not run content-aware hooks. Linux and Windows both perform full TLS termination and content-aware hook execution on intercepted flows.

This is a platform constraint, not a roadmap gap. The macOS NE architecture (kernel-mode transparent proxy) does not provide the ability to read application-layer bytes without a MITM TLS implementation separate from what the NE itself provides. The compliance-proxy covers the content gap for macOS endpoints by sitting network-side.

## Build and signing chain

The macOS agent binary is the highest-blast-radius component in the system. Improvising around the signing pipeline has bricked Network Extensions in past incidents. The `build-agent` skill (`.claude/skills/build-agent/`) is the single source of truth for every build step — never run `codesign`, `pkgbuild`, `productbuild`, or `xcrun notarytool` directly.

The artefact chain:

```
Go source             → daemon binary (signed)     ──┐
Swift NE source       → .appex bundle (signed)     ──┤
Wails frontend        → .app bundle (signed)       ──┼─→ combined .app
                        + launch-constraints            │
                                                       ▼
                                                     .pkg (signed by Developer ID Installer)
                                                       │
                                                       ▼
                                                     notarytool (Apple notarisation + staple)
```

Signing identities in use:

| Identity | Used for |
|---|---|
| Developer ID Application | `.app`, `.appex` (daemon, NE provider, UI) |
| Developer ID Installer | `.pkg` |
| Provisioning Profile (Network Extension) | NE entitlement |

macOS 26 introduced launch constraints requiring system extensions to be launched only by signed parent processes. The agent bundle embeds launch-constraint blobs declaring the allowed parent (`com.nexus-gateway.agent`) and the required Developer ID signature. The `build-agent` skill carries the canonical constraint XML.

## Test invariants before any NE change

Before merging any change to `TransparentProxyProvider.swift` or its dependents, the following tests must pass:

- Boot the agent on a fresh macOS machine. Verify Wi-Fi browsing works (DNS, DHCP, basic HTTPS).
- Disable the Hub (simulate daemon disappearing). Verify Wi-Fi still works. Rule 2 compliance.
- Send malformed flows (random UDP, unknown TCP). Verify the OS does not see them get stuck. Rule 1 compliance.
- Watch QUIC handshake handling (`udp.dst == 443`). Verify QUIC traffic either passes through or is captured per the file-only list, never both. Rules 3 + 4 compliance.
- Run for 24 h on a developer machine without any human intervention. No "did I lose internet?" reports.

These tests are non-optional. A change that passes unit tests but skips these integration checks cannot be considered safe for production deployment.

## Recovery procedure

When a user reports "Wi-Fi is connected but nothing works" after installing or updating the agent:

1. Confirm the NE provider is the culprit: Activity Monitor → check for `NexusAgentExtension` consuming high CPU or with many pending I/O entries.
2. Run the canonical uninstall script:
   ```bash
   sudo bash packages/agent/platform/darwin/Scripts/uninstall.sh
   ```
   If the script is unavailable on the host:
   ```bash
   sudo launchctl unload /Library/LaunchDaemons/com.nexus-gateway.agent.plist
   sudo rm /Library/LaunchDaemons/com.nexus-gateway.agent.plist
   sudo reboot
   ```
3. Verify network is restored before capturing a diagnostic bundle.

Never use raw `launchctl unload` + `rm` recipes in place of the script — they skip the system-extension teardown and leave the host in a partial-uninstall state that breaks the next install. For an unrecoverable host, `sudo systemextensionsctl reset` clears all system extensions on the machine.

---

## Canonical docs

- [`agent-ne-fail-open-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md) — Canonical expansion of the five fail-open rules; blast-radius reality; test invariants
- [`agent-macos-platform-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-macos-platform-architecture.md) — Broader macOS platform layer: process model, native APIs, path conventions
- [`macos-build-signing-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/macos-build-signing-architecture.md) — Build artefact chain, entitlements, notarisation, adhoc vs prod fork

**Adjacent wiki pages**: [Agent Overview](Agent-Overview) · [Agent Enrollment Attestation](Agent-Enrollment-Attestation) · [Agent Auto Update](Agent-Auto-Update) · [Fail Open Posture](Fail-Open-Posture) · [Security Network Safety](Security-Network-Safety)
