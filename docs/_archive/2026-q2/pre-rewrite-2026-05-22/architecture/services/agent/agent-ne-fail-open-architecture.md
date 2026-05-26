---
doc: agent-ne-fail-open-architecture
area: service
service: agent
tier: 1
updated: 2026-05-21
---

# Agent macOS Intercept Fail-Open Architecture (Safety-Critical)

> **Tier 1 architecture doc — SAFETY-CRITICAL.** Read this before any change to `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/` (NE path) or `packages/agent/internal/platform/darwin/pfintercept/` (pf path, E74). The CLAUDE.md rule "macOS NE proxy must fail-open, never fail-closed" is the binding statement; this doc is the canonical expansion for **both** intercept paths.

## 0. Applies to both modes

The five fail-open invariants in this document (Rules 1–5) are binding for **both** `interceptMode="ne"` and `interceptMode="pf"`. The mechanism by which each rule is enforced differs between the two paths, but the safety guarantee is identical: the Mac's network MUST remain functional regardless of what the Nexus daemon does. Each rule section below has an NE explanation followed by a sub-block that describes the pf-path enforcement for the same invariant, with citation to the relevant E74 FR-4.x clause.

---

The `NETransparentProxyProvider` (`interceptMode="ne"`) and the pf listener (`interceptMode="pf"`) are both in the **host's outbound packet path**. Any code path in either that hangs, panics, or silently claims a flow without relaying it takes down the **entire Mac's network** — DNS, DHCP, mDNS, NTP, Apple Push, VPNs, the works.

Recovery for the NE path requires the user to manually `launchctl unload` the daemon and **delete its plist**. The pf path fails-open more gracefully (anchor flush is fast and targeted — see §3), but the same principles apply. Treat both paths accordingly.

---

## 1. The blast-radius reality

A misbehaving NE provider does not "slow down" — it **kills the network**. The OS hands every outbound flow to the provider; if the provider claims a flow (`return true` from `handleNewFlow`) and then doesn't relay, the flow stalls forever. Inbound DNS replies route through different paths but outbound DNS queries die. The user sees Wi-Fi connected, no internet, no recovery via "Forget network".

Recovery on the user's Mac:

```bash
sudo bash packages/agent/platform/darwin/Scripts/uninstall.sh
sudo reboot   # or simply restart the network service
```

The canonical uninstall script unloads the LaunchDaemon (`com.nexus-gateway.agent`), removes the system extension, and cleans residual state. If `uninstall.sh` is unavailable on the host (e.g., shipped artefacts only), fall back to:

```bash
sudo launchctl unload /Library/LaunchDaemons/com.nexus-gateway.agent.plist
sudo rm /Library/LaunchDaemons/com.nexus-gateway.agent.plist
sudo reboot
```

The recovery procedure is validated and documented. **Memory `project_e48_emergency_passthrough` and `project_ne_cursor_streamchat_verification` are load-bearing for prod NE work.**

## 2. The five fail-open rules (binding)

### Rule 1 — `handleNewFlow` decides synchronously

`handleNewFlow` is the OS callback that decides "do we want this flow". It MUST:

- Decide **synchronously**.
- Only `return true` for flows we can **fully relay**.
- For UDP flows where we have no relay implementation: `return false` for **unknown bundles**.

Apply protocol detection (e.g., SNI for TLS) or bundle-ID checks **before** claiming. Never `return true` "to look at it later" — once claimed, the flow is ours forever; if we don't relay, it dies.

A flow we don't know how to relay must `return false`. The OS will then route it through the default path. This is correct behaviour; we lose visibility but the user keeps their network.

**pf path equivalent (FR-4.1):** The pf listener's in-process `domain.Engine.MatchHost` call is synchronous and in-memory. There is no IPC round-trip and no `requestDecision` 2-second timeout class. The listener calls `net.Conn.SetDeadline` at accept time; if `BumpFlow` or `opaqueRelay` fails, the socket is closed with a policy reason. No redirected flow can hang indefinitely — a deadline error closes the socket and returns the flow to the OS's TCP RST path, which is the fail-open equivalent of `return false` in the NE path.

### Rule 2 — every async callback has a fail-open timeout

Some flows need to ask the daemon for a decision (e.g., "is this domain in scope?"). The daemon may be slow or absent. Every async callback into the daemon MUST have a fail-open timeout:

- `requestDecision` defaults to `passthrough` after 2 s — verified at `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/IPCProtocol.swift:328-351` (`queue.asyncAfter(deadline: .now() + 2.0)` fires a synthetic `passthrough` if the daemon does not respond).
- `peekSNIThenRelay` falls through to plain relay after 500 ms — verified at `TransparentProxyProvider.swift:309-317, 631-639`.

Never let a flow hang waiting on an absent daemon. If the daemon is gone, the NE provider must still relay — degraded visibility is acceptable; broken networking is not.

**pf path equivalent (FR-4.2):** The pf path replaces the `requestDecision` 2-second timeout with a 500 ms SNI peek deadline (matching `peekSNIThenRelay`). If the SNI peek times out, the listener on `127.0.0.1:13443` falls through to `opaqueRelay` — traffic passes uninspected (fail-open). The upstream dial timeout is bounded separately. Critically, if the daemon itself is absent, the pf anchor `nexus-agent/transparent` is also absent (DEC-009 cleanup-before-reinstall): with no `rdr` rules loaded, the kernel does not redirect flows at all — normal routing resumes automatically. A dead daemon cannot produce a stuck network in the pf path.

### Rule 3 — no hardcoded enforcement lists in NE Swift code

Bundle-ID allowlists (e.g., the QUIC fallback set), domain allowlists, or any admin-controlled enforcement set MUST NOT be hardcoded in Swift.

The pattern:

1. Hub pushes the list in the `agent_settings` shadow.
2. The daemon writes the list to a file (`/var/run/nexus-agent/quic-bundles.json` or equivalent).
3. The NE provider reads the file **file-only**, with **empty-as-fail-safe** (empty file → enforcement off, not enforcement everywhere).

A hardcoded fallback would silently override admin policy. The admin disables a bundle in the UI; the file becomes empty; the NE reads empty and stops enforcing. Anything else breaks the admin's mental model.

**pf path equivalent (FR-4.3):** The pf path honours this invariant via the `deny` branch in the listener's decision dispatch. A `deny` decision closes the socket with a policy reason but does not silently discard or hold packets. No pf-level blocking occurs outside the daemon listener's decision — the pf `rdr` rules redirect flows to the listener, but only the listener makes the intercept/deny call. The QUIC uid set in pf rules is derived exclusively from the Hub-pushed `agent_settings.forceQUICFallbackBundles` blob (DEC-006 atomic reload); no hardcoded enforcement list exists in the generated pf rule template.

### Rule 4 — `isLikelyXyz = true` patterns are banned

This is a concrete code-smell: a flag named `isLikelyXyz` set to `true` "for now" with a comment to "make it real later". **Don't do this.**

Either write the real condition:

```swift
let isLikelyQUIC = (proto == .udp && (dst == 443 || dst == 80) && payload.matches(.quicInitial))
```

…or return `false`:

```swift
let isLikelyQUIC = false
// QUIC detection not yet implemented; default to false.
```

A `= true` flag with no real condition is a **time bomb**. It looks intentional in review but breaks the moment NE encounters a flow that matches the unmarked-true case.

**pf path equivalent (FR-4.4):** The pf path derives the QUIC intercept scope from the same Hub-pushed `agent_settings` blob that the NE path reads (DEC-006 atomic reload at each `agent_settings` push). The daemon resolves `forceQUICFallbackBundles` → uid set eagerly (DEC-004) and writes the result directly into the pf rule template. No speculative "likely QUIC" flags exist in the pf rule generator; if `quicFallbackUIDs` is empty, no UDP `rdr` rules are emitted — fail-open by absence.

### Rule 5 — system DNS / DHCP / Push services list MUST NEVER be killed

The following process names MUST NEVER have their UDP closed under any circumstance:

- `mdnsresponder`
- `configd`
- `dhcpcd`
- `apsd`
- `nsurlsessiond`
- `kdc`
- `ntpd`

When adding any process to a kill-list (deny-bundle-from-egress, blocked-process), validate the addition against this list. If a flow originates from any of these, **always relay**. Even if the destination is in a deny list, these processes are part of how macOS itself stays up.

The validation is in the daemon, not the NE provider; but the NE provider obeys the file-only enforcement list (Rule 3), so the daemon's validation is the only guard.

**pf path equivalent (FR-4.5):** The pf rule template explicitly emits `pass out quick` lines for `mdnsresponder`, `configd`, `apsd`, `nsurlsessiond`, `kdc`, and `ntpd` — each by process username — **above** the `rdr` rules in the anchor. These pass rules are unconditional and cannot be overridden by later `rdr` lines (pf processes rules first-match). The list is derived at build time from the same system-process allowlist that the NE `isProtectedSystemProcess` check uses; both must stay in sync when the list changes. Note: the `! user nexus-agent` self-exclusion in the pf `rdr` rule (FR-1.5) is the additional pf-layer guard against infinite-loop self-intercept — the daemon's own outbound connections to upstream AI providers are never redirected back to the daemon listener.

## 3. Recovery procedure (for support runbooks)

If a user reports "Wi-Fi is connected but nothing works" after installing or updating the Nexus Agent:

1. Confirm the NE provider is the culprit. Activity Monitor → check for `NexusAgentExtension` consuming high CPU or with many pending I/O.
2. Unload the daemon via the canonical uninstall script:
   ```bash
   sudo bash packages/agent/platform/darwin/Scripts/uninstall.sh
   ```
   (Or, if the script is unavailable, `sudo launchctl unload /Library/LaunchDaemons/com.nexus-gateway.agent.plist`.)
3. Verify network restored.
4. Capture diagnostic bundle (logs, plist, recent config) before remediation.
5. If the issue reproduces after a fresh install, ship the diagnostic to engineering.

This procedure is on the agent support runbook. Engineering's job is to make it never necessary.

## 4. Test invariants

Before merging any change to `TransparentProxyProvider.swift` or its dependents:

- [ ] Boot the agent on a fresh macOS. Verify Wi-Fi browsing works (DNS, DHCP, basic HTTPS).
- [ ] Disable the Hub (simulate daemon disappearing). Verify Wi-Fi still works.
- [ ] Send malformed flows (random UDP, unknown TCP). Verify the OS doesn't see them get stuck.
- [ ] Watch for QUIC handshake handling (`udp.dst == 443`). Verify QUIC traffic either passes through or is captured per the file-only list, never both.
- [ ] Run for 24h on a developer machine. No human action needed mid-day, no "did I lose internet?" question on Slack.

These tests are non-optional.

## 5. Build & sign integration

The NE provider is bundled inside the agent app and signed with the canonical macOS Developer ID. The `build-agent` skill (`.claude/skills/build-agent/`) is binding for builds — never run `wails build` / `codesign` / `xcrun notarytool` manually. Improvising around the skill has bricked Network Extensions in the past.

The skill encodes the exact signing identities, provisioning profiles, notarytool keychain profile, macOS 26 launch-constraint requirements, and the strict install/uninstall sequence. If a deviation is needed (e.g., `PATH="$HOME/go/bin:$PATH"` so the non-interactive shell finds the Wails CLI), keep the rest of the skill verbatim and call out the deviation.

Tier-2 doc `macOS Build & Signing architecture` is the planned design-side companion to the operational skill.

## 6. Sources

- `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/TransparentProxyProvider.swift` — the NE provider.
- `packages/agent/cmd/agent/` — daemon-side IPC and decision logic.
- `packages/agent/cmd/agent/platformshim/` (or the equivalent file-only allowlist writer) — Rule 3 file emitter.
- `.claude/skills/build-agent/` — canonical build procedure.

## 7. Cross-references

- `agent-forwarder-architecture.md` — what the agent does once a flow is claimed.
- `agent-enrollment-architecture.md` — enrollment / mTLS / device cert lifecycle.
- `thing-config-sync-architecture.md` — how `agent_settings` arrives.
- CLAUDE.md "macOS NE proxy must fail-open" — the binding rule.
- `.claude/skills/build-agent/` — operational build skill.
