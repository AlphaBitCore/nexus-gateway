# Security Network Safety

*Audience: security reviewers and contributors working on macOS agent network extension code.*

The macOS Desktop Agent uses `NETransparentProxyProvider` to intercept AI-bound network traffic at the OS level. This extension is in the host's outbound packet path for all processes: any hang, panic, or silent flow claim without a relay takes down the entire Mac's network — DNS resolution, DHCP, mDNS, NTP, Apple Push Notification, VPN, and all application connectivity. Recovery requires a manual `launchctl unload` and plist deletion. Because of this blast radius, five fail-open invariants govern the NE implementation as a hard binding. This page states all five invariants, identifies the system processes that must never be killed, and describes the broader network-safety mechanisms (emergency passthrough and kill switch) that apply to all traffic paths.

---

## The five NE fail-open invariants

These invariants are binding across all changes to `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/TransparentProxyProvider.swift`. The full architectural rationale is in [`agent-ne-fail-open-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md).

### Invariant 1 — synchronous flow decisions

`handleNewFlow` decides synchronously and returns `true` only for flows the extension can fully relay. For UDP flows from unknown bundles, the extension returns `false` immediately.

Claiming a flow with `return true` is a permanent commitment. The OS hands the flow to the extension and removes it from the default path. If the extension claims a flow it cannot relay, that flow stalls forever. Any protocol detection (SNI for TLS, bundle-ID checks) must complete before claiming — never claim speculatively to inspect later.

### Invariant 2 — fail-open timeout on every async callback

Some flows require asking the daemon for a routing decision. The daemon may be slow or absent. Every async callback must have a fail-open timeout:

- `requestDecision` defaults to passthrough after **2 seconds** if the daemon does not respond.
- `peekSNIThenRelay` falls through to plain relay after **500 milliseconds**.

No flow may block indefinitely waiting on an absent daemon. If the daemon disappears, the NE extension continues relaying all flows — visibility is degraded, but the user's network remains functional.

### Invariant 3 — no hardcoded enforcement lists in NE Swift code

Bundle-ID allowlists, domain allowlists, and any admin-controlled enforcement set must not be hardcoded in Swift. The pattern:

1. Hub pushes the list in the `agent_settings` shadow blob.
2. The daemon writes the list to a file (e.g. `/var/run/nexus-agent/quic-bundles.json`).
3. The NE extension reads the file only, with **empty-as-fail-safe** — an empty file means enforcement is off, not enforcement everywhere.

A hardcoded fallback would silently override admin policy. When an admin removes a bundle from the enforcement list in the Control Plane UI, the file becomes empty, and the NE extension stops enforcing for that bundle.

### Invariant 4 — `isLikelyXyz = true` patterns are banned

A flag named `isLikelyXyz` set to `true` as a placeholder is prohibited. Either write the real condition:

```swift
let isLikelyQUIC = (proto == .udp && (dst == 443 || dst == 80) && payload.matches(.quicInitial))
```

…or explicitly return false:

```swift
let isLikelyQUIC = false
// QUIC detection not yet implemented; default to false.
```

A `= true` flag with no real condition looks intentional in review but breaks the moment NE encounters a flow that triggers the unmarked-true case.

### Invariant 5 — system processes must never have their UDP closed

The following process names must never appear on any deny-bundle-from-egress list:

- `mdnsresponder`
- `configd`
- `dhcpcd`
- `apsd`
- `nsurlsessiond`
- `kdc`
- `ntpd`

These processes are how macOS maintains DNS, DHCP, Apple Push, NTP, and other critical OS functions. Closing their UDP path breaks the OS's ability to stay connected even when the NE extension is otherwise functioning correctly. Validation against this list occurs in the daemon (which writes the enforcement file); the NE extension obeys the file-only enforcement list from Invariant 3.

## Emergency passthrough

When the compliance pipeline cannot run — hook outage, dependency failure, mass misconfiguration — Emergency Passthrough allows traffic to flow without enforcement while preserving the audit trail. Key properties:

- The audit trail is non-optional. Bypassed requests emit a full `traffic_event` with `passthrough=true` and a mandatory `bypass_reason`.
- Maximum expiry is 8 hours (default 1 hour). Each passthrough row carries an `expiresAt` timestamp.
- Hub reconciles every 60 seconds and auto-reverts expired rows.
- Cold-start is fail-closed: a service that boots without a shadow blob defaults to enforced, not passthrough.
- Three independent bypass flags per tier — `bypassHooks`, `bypassCache`, `bypassNormalize` — are orthogonal. Enforcement can be disabled selectively.

Emergency passthrough is gated on `admin:passthrough.emergency-enable` IAM action, which is separate from `admin:passthrough.write`. This separation allows an admin to pre-configure passthrough parameters without holding the broader activation lever. Full detail: [Emergency Passthrough](Emergency-Passthrough).

## Kill switch

The kill switch provides a three-tier toggle (global, adapter family, provider) for emergency traffic control. State is stored as a Category A inline shadow blob in Hub, giving sub-second propagation to all running services. The kill-switch surface in the Control Plane UI provides activation history and expiry countdown. Full detail: [Kill Switch](Kill-Switch).

---

## Canonical docs

- [`docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md) — macOS NE fail-open rules, blast-radius analysis, recovery procedure, and test invariants
- [`docs/developers/architecture/cross-cutting/safety/emergency-passthrough-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/emergency-passthrough-architecture.md) — emergency passthrough tiers, flags, reconcile loop, and audit invariants
- [`docs/developers/architecture/cross-cutting/safety/kill-switch-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/kill-switch-architecture.md) — kill-switch activation lifecycle and IAM gating

**Adjacent wiki pages**: [Fail Open Posture](Fail-Open-Posture) · [Agent macOS NE Architecture](Agent-macOS-NE-Architecture) · [Emergency Passthrough](Emergency-Passthrough) · [Kill Switch](Kill-Switch) · [Security Threat Model](Security-Threat-Model)
