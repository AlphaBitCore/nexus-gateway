# Agent NE fail-open architecture

The macOS agent intercepts traffic with a `NETransparentProxyProvider` system
extension (`packages/agent/platform/darwin/NexusAgent/NexusAgentExtension`). That
provider sits in the host's **outbound packet path**: every outbound flow from
every process on the Mac is offered to it. A hang, a panic, or a flow that the
provider claims but cannot relay does not just break the agent — it takes down
the whole machine's networking: DNS, DHCP, mDNS, NTP, Apple Push, and VPNs all
ride the same path. Recovery from that state is manual (`launchctl unload` plus
deleting the extension), so the provider is engineered to **fail open**: when in
doubt, decline the flow and let macOS route it natively, or relay it
un-inspected — never block or hang.

This document describes the five binding fail-open rules and the exact code paths
that enforce them. It is the document the `ne-fail-open` editing rule points at;
read it before changing anything under
`packages/agent/platform/darwin/NexusAgent/NexusAgentExtension`. For the broader
macOS interception design (system-extension lifecycle, the MITM bridge, decision
application) see [agent-macos-platform-architecture.md](agent-macos-platform-architecture.md)
and [agent-forwarder-architecture.md](agent-forwarder-architecture.md); for build
and signing see [macos-build-signing-architecture.md](macos-build-signing-architecture.md).

## What "fail open" means here

Two distinct decisions must never be confused:

- **Infrastructure failure** — the daemon is down, an IPC call times out, a flow
  cannot be opened. The provider responds by passing the flow through
  un-inspected (or declining it so macOS routes natively). It never blocks.
- **Policy decision** — the daemon explicitly returns `deny` for a flow. The
  provider honours it and closes the flow. This is admin policy, not a failure,
  and is the *only* path that intentionally blocks a flow.

Every rule below protects the first case. The provider's principal class is
registered through `NEProvider.startSystemExtensionMode()`
(`NexusAgentExtension/main.swift`); the implementation is `NexusProxyProvider` in
`NexusAgentExtension/TransparentProxyProvider.swift`.

### Daemon-absent self-heal (anti-zombie)

macOS `sysextd` owns the extension's lifecycle independently of the daemon and
the menu app: the provider keeps running after the daemon dies (user quit,
crash, upgrade). With no daemon to bridge to, *claiming* a flow would strand it —
`requestDecision` times out, the bridge write fails, and the host's traffic
degrades into a flood of relay-write failures. To prevent that "zombie" state the
provider tracks its IPC connection liveness (`AgentIPCClient`): once the daemon
has been unreachable past a short grace window (`daemonAbsentBeyondGrace`, ~6 s),
`handleNewFlow` declines **every** flow so macOS routes it natively — clean
passthrough, no claim, no bridge. A background reconnect loop on a dedicated
queue re-dials the daemon socket and, on success, re-arms the receive loop so
interception resumes automatically when the daemon returns. The grace window
tolerates a brief daemon restart (config reload / upgrade) without flapping
interception off. `stopProxy` uses `shutdown()` (not `disconnect()`) so a
deliberate teardown suppresses the reconnect loop. This is fail-open by
construction: a missing daemon yields *less* interception, never a blocked host.

## Two-layer protection for system-critical UDP

Critical system UDP must never reach the provider, because UDP is the path with
no relay implementation — claiming it risks dropping DNS/DHCP/NTP. Two
independent layers enforce this.

**Layer 1 — OS-level rule exclusion.** `startProxy` installs a single catch-all
`includedNetworkRules` entry (`protocol: .any`, outbound) so the proxy can see
browser QUIC, then a set of `excludedNetworkRules` covering the critical UDP
ports — `53` (DNS), `5353` (mDNS), `67`/`68` (DHCP), `123` (NTP), `500`/`4500`
(IKE), `1900` (SSDP), `5355` (LLMNR) — with both an IPv4 (`0.0.0.0`) and IPv6
(`::`) rule per port. macOS routes those ports natively and they never enter
`handleNewFlow`.

**Layer 2 — bundle-ID fast-decline.** `handleNewFlow` declines any UDP flow whose
source bundle matches `systemNetworkServiceBundles` — the extension-side mirror
of the shared protected set in `packages/shared/policy/systembundles`
(`com.apple.mdnsresponder`, `com.apple.configd`, `com.apple.dhcpcd`,
`com.apple.bootpd`, `com.apple.apsd`, `com.apple.nsurlsessiond`,
`com.apple.identityservicesd`, `com.apple.rapportd`,
`com.apple.networkserviceproxy`, `com.apple.kerberos` (namespace),
`com.apple.kdc`, `com.apple.timed`, `com.apple.locationd`,
`com.apple.symptomsd`, `ntpd`, `mdnsresponder`, `launchd`) — regardless of
destination port. The match (`isProtectedSystemBundle`) is case-insensitive and
exact-OR-prefix: a bundle equal to a protected entry or a helper/child under a
protected namespace (`<protected>.<anything>`, e.g. `com.apple.kerberos.kdc`)
is declined. The prefix arm deliberately uses the same matching shape as the
QUIC kill check, so the protection can never be narrower than the kill — a
kill-list entry cannot reach a protected daemon or its helpers. This catches
the long tail a port rule cannot — a system service sending UDP to a
non-standard port (for example DNS-over-HTTPS on UDP/443, or Apple Push on
ports that vary across macOS releases). The two layers are deliberately
redundant: Layer 1 is the primary guarantee, Layer 2 is the belt-and-suspenders
if NECP does not honour the UDP port match.

## The five fail-open rules

### Rule 1 — `handleNewFlow` decides synchronously and claims only what it can fully relay

`handleNewFlow` returns `true`/`false` synchronously and never throws — an
uncaught Swift error would make macOS drop the flow without native routing, which
the user sees as a network outage. Because Swift cannot catch runtime traps, the
safety contract is **structural, not a wrapper**: every lookup in the body is a
`guard` / `as?` that declines the flow on failure, and the body carries no
force-unwraps — a single `!` could trap the provider on a malformed flow and take
the host's networking down with it. Reads of raw flow metadata use the unaligned
accessor for the same reason: `extractPID` decodes the audit-token PID with
`loadUnaligned`, because `Data`'s backing buffer carries no alignment guarantee
and an aligned `load(as:)` would trap in the packet path. The decision order is:

1. **Daemon self-intercept** — if the flow's source PID is the agent daemon (see
   `DaemonPIDFilter`), `return false`. The daemon's own upstream connections must
   not re-enter the interception loop. (Declining means macOS routes the daemon's
   traffic natively; an early-boot window before the daemon PID is known lets a
   request through, which the daemon's HTTP client simply retries.)
2. **UDP** (`NEAppProxyUDPFlow`): a system-service bundle returns `false`
   (Layer 2 above); a bundle on the QUIC-fallback allowlist has its read and
   write closed (`closeReadWithError` + `closeWriteWithError`) and returns `true`,
   which forces the client to fall back from HTTP/3-over-QUIC to HTTP/2-over-TCP
   where the TCP path can inspect it; **any other UDP returns `false`**. This last
   default is the load-bearing safety branch — the provider never claims UDP it
   has no relay for.
3. **Non-TCP, non-UDP** flow classes return `false`.
4. A TCP flow whose remote endpoint is not an `NWHostEndpoint` returns `false`.
5. Otherwise the flow is a relayable TCP flow: the provider records it, returns
   `true`, and hands off asynchronously to `peekSNIThenDecide`.

Protocol and bundle checks all happen *before* the flow is claimed, so a flow is
only claimed once the provider knows it can carry it.

### Rule 2 — every async daemon callback has a fail-open timeout

No flow may hang waiting on the daemon. Each asynchronous hop is time-bounded and
falls through to a non-blocking outcome:

- **`requestDecision`** (`AgentIPCClient`, `IPCProtocol.swift`) arms a 2-second
  timer. If the daemon has not answered, it fires a synthetic `passthrough`
  decision so the flow proceeds un-inspected. A hung decision would freeze the
  user's application.
- **`peekSNIThenDecide`** bounds the TLS-ClientHello peek at 500ms. Server-speaks-
  first protocols (SSH, SMTP, IMAP) never send a ClientHello; on timeout the
  provider requests a decision with the original (IP / pre-resolved) host instead
  of waiting forever.
- **`peekSNIThenRelay`** applies the same 500ms bound on the relay path and falls
  straight through to a plain relay on timeout.
- **IPC teardown** (`disconnect`, and the receive-loop error / peer-close paths)
  drains every pending decision callback with a synthetic **`passthrough`** — not
  `deny`. Draining with `deny` would reject every in-flight flow the instant
  daemon IPC dropped, which presents as a wholesale outage.

The race between a timeout firing and the real callback arriving is resolved by
`TimeoutGuard` (`QUICFallbackBundles.swift`): whichever calls `tryFire()` first
wins, so the *decision* is dispatched exactly once and a flow is never both
timed-out and separately relayed. The timed-out peek does not discard the app's
bytes, though: when the 500ms timer wins, the still-outstanding `flow.readData`
is the app's real first payload on a server-speaks-first protocol (SMTP / FTP /
MySQL first post-greeting write), and macOS permits only one read in flight per
flow. `LatePeekRead` adopts that late completion and hands it to whichever relay
the decision selects — `startAppToRemoteRelay` consumes it as the first
app→remote chunk, both for direct passthrough and for the bridge path after the
`BRIDGE` header write; it is forwarded un-consumed through every
bridge→passthrough fallback and dropped on `deny` (the flow is closing). Issuing
a second `readData` instead would discard those first bytes and break the
app→upstream direction.

A related fail-open shape covers a flow that is claimed but then cannot be used:
when `flow.open` fails, or `createTCPConnection` returns nil, the provider closes
the flow's read and write (a reset) and completes it. `flow.open` itself is
watchdogged — the 500ms peek timer is armed only inside the open callback, so an
`open` that never calls back would hang the claimed flow with no timeout at all.
A `relayEstablishTimeout` (5s) `TimeoutGuard` fires `failOpenResetFlow` if the
open never completes, and a late open completion after the watchdog has fired is
dropped (the guard is one-shot). The client sees an immediate reset and retries —
far better than the ~75-second SYN timeout it would otherwise sit through.

### Rule 3 — no hardcoded enforcement lists in the extension

The list of bundles whose QUIC is killed is **admin policy**, not a constant. It
originates in the Hub-pushed `agent_settings.forceQUICFallbackBundles` shadow
value; the daemon writes it to `/var/run/nexus-agent/quic-bundles.json`
(`packages/agent/cmd/agent/platformshim/quic_fallback_darwin.go`, fed from the
`agent_settings` applier in `packages/agent/cmd/agent/configappliers.go`); and
`QUICFallbackBundles` reads that file only, refreshing on a timer.

Because the kill-list is admin policy, it is gated against a canonical
**protected-system-bundle set** on every Go path that can reach that file — this
is rule 5 (validate before any process is added to a UDP kill-list). The set is
one shared package, `packages/shared/policy/systembundles`, and an entry is
rejected iff it is equal to / an over-broad ancestor of / a child of a protected
bundle (so a bare `com.apple` that the NE's prefix-capable kill would fan out
across every `com.apple.*` daemon is caught, while a real QUIC app like
`com.apple.Safari` passes). Two gates, because two trust boundaries reach the
file: the Control Plane write handler rejects the request (the low-priv admin
path), and the daemon writer `WriteQUICFallbackBundlesFile` strips protected
entries (the path an already-compromised node takes by pushing the
`agent_settings` shadow directly, bypassing the CP handler). The extension's
`systemNetworkServiceBundles` mirrors this shared set entry-for-entry (keep the
two in sync when either changes), so the same protected bundles are guarded at
all three trust boundaries: CP write, daemon write, and the NE flow decision
itself. The read is
empty-as-fail-safe: a missing or unreadable file yields an empty allowlist (no
UDP is killed), and an undecodable file keeps the previous list. There is no
hardcoded fallback allowlist — a hardcoded list would silently override an admin
who removed a bundle, and a brief no-enforcement window at first boot is the
intended safe default.

The one hardcoded set in the extension, `systemNetworkServiceBundles`, is **not**
an enforcement list and does not contradict this rule: it only ever causes the
provider to *decline* (never to claim or block), so it can only reduce
interception. It is hardcoded because macOS system-service bundle IDs are stable;
adding to it is a deliberate, security-reviewed change because a user application
placed on it would become invisible to interception.

The **source-bundle exemption list** (`bypassBundles`) follows the identical
file-bridged, admin-policy, no-hardcoded-entry pattern. It originates in the
Hub-pushed `agent_settings.bypassBundles` shadow; the daemon writes
`/var/run/nexus-agent/bypass-bundles.json`
(`packages/agent/cmd/agent/platformshim/bypass_bundles_darwin.go`, fed from the
same `agent_settings` applier); and `BypassBundles` reads that file only with the
same empty-as-fail-safe contract (missing/unreadable → exempt nothing → inspect
everything). A flow whose `sourceAppSigningIdentifier` matches an entry is
declined in `handleNewFlow` (native routing, no bump), letting a trusted
developer tool whose pinned TLS would break under bump — e.g. a local
`claude-code` CLI to `api.anthropic.com` — stay off the inspection path while the
**same destination host reached from any other app stays inspected** (matching is
by source bundle, never by host). Unlike the QUIC kill-list there is no
protected-system-bundle reject: a bypass entry can only *remove* a bundle from
inspection (it cannot close any UDP), so even a system-daemon entry is a harmless
no-op rather than a fleet-wide hazard. The list ships empty; it is populated only
by deliberate admin config.

### Rule 4 — no placeholder `true` conditions

Every decision is a real predicate or an explicit `return false`; the extension
ships no `isLikely… = true` stand-ins. For example `isLikelyIPLiteral` actually
inspects the string (dotted-quad or colon-hex) rather than assuming a result, and
the bundle / protocol / endpoint checks in `handleNewFlow` each evaluate a
concrete condition before the flow is claimed.

### Rule 5 — system DNS / DHCP / Push UDP is never closed

Closing UDP is only ever done by the QUIC-fallback branch in Rule 1, and that
branch fires solely for bundles on the admin-controlled allowlist. System network
services are protected three times over: the `excludedNetworkRules` keep their
standard ports out of the provider entirely (Layer 1), the
`systemNetworkServiceBundles` fast-decline catches them on any port (Layer 2),
and the "any other UDP returns `false`" default means an unknown or unsigned
process's UDP is declined rather than closed. The result is that
`mdnsresponder`, `configd`, `dhcpcd`, `apsd`, `nsurlsessiond`, `kdc`, and `ntpd`
keep their UDP no matter what the provider is doing.

## Decision and relay flow (summary)

Once a TCP flow is claimed, the provider opens it, peeks the first chunk for a TLS
SNI hostname (so callers that pre-resolve DNS still yield a real hostname to the
policy engine), requests a decision from the daemon, and applies it:

- **`deny`** — close the flow with a 403-coded error (the explicit policy block).
- **`inspect`** — relay through the Go MITM bridge on `127.0.0.1:9443` using a
  `BRIDGE <host>:<port> <flowId>` header; if the bridge is unreachable or the
  header write fails, fall back to a direct relay so the flow keeps working.
- **`passthrough`** — relay directly to the remote.

The SNI peek and the bridge / relay machinery are detailed in
[agent-forwarder-architecture.md](agent-forwarder-architecture.md) and
[agent-macos-platform-architecture.md](agent-macos-platform-architecture.md). The
SNI parser itself (`SNIParser`) is pure byte-level TLS-ClientHello walking and
returns nil on any short or malformed buffer.

## Self-intercept guard

`DaemonPIDFilter` reads the daemon's PID from `/var/run/nexus-agent/daemon.pid`
(written by the daemon at startup, `packages/agent/cmd/agent/cmd_run.go`) and
caches it with a short refresh so a daemon restart is picked up without restarting
the extension. Flows whose source PID matches are declined in Rule 1. The filter
is fail-safe: a missing or unparseable PID file disables it (the provider just
loses the extra loop protection; it never blocks a flow because of it).

## Invariants any change must preserve

- A fresh macOS boot can browse the web (DNS, DHCP, HTTPS) with the extension
  active.
- With the daemon / Hub unreachable, networking still works (decisions fall
  through to passthrough).
- Malformed or unknown flows (random UDP, non-TCP classes) never hang the
  provider.
- A QUIC handshake is either killed to force TCP fallback or left alone — never
  both passed through and captured.
- With the daemon absent past the grace window the provider declines every flow
  (native routing) and keeps trying to reconnect — it never stays in a state
  where it claims flows it cannot bridge.
- A bundle on `bypassBundles` is declined (native routing) and never bridged; an
  empty/absent list exempts nothing.
- A relayed flow reaches its natural end. Half-close and connection lifetime are
  part of fail-open, not separate concerns — see below.

## Relay lifetime: the flow must be allowed to finish

Three properties of the relay itself belong with the fail-open rules, because
breaking any of them presents to the user as "the network is broken" even
though every decision above was correct.

**Half-close must propagate.** When the remote finishes writing, the relay
closes only its own read half; the app is never sent EOF, so it waits for data
that will never come and the request hangs until some higher-level timeout
fires. A protocol that signals completion by closing one direction — the shape
every plain HTTP/1.1 response without `Content-Length` relies on — cannot
complete at all. The remote's EOF has to reach the app.

**A watchdog must not outlive what it guards.** A timer that guards
*establishment* has to be cancelled once the connection is established.
Otherwise it fires later, mid-transfer, and resets a connection that was
working — the failure is indistinguishable from packet loss to the user, and it
gets worse the longer the connection is useful, which is exactly backwards.

**The IPC connection is touched from more than one thread.** The provider's
flow handlers run concurrently; a reference to the daemon connection that is
read and reassigned without a lock is a data race, and the observable symptom
is not a clean error but a crash of the extension — which takes the host's
networking down with it.

**The SNI peek must validate the TLS version byte, not just the record type.**
A record whose first byte happens to be 0x16 is not necessarily a TLS
handshake; checking the major-version byte as well is what stops arbitrary
traffic from being parsed as one and mis-attributed to a host it never
addressed.

## Source attribution and the per-flow verdict

The `flow_new` IPC frame carries the kernel-attested
`sourceAppSigningIdentifier` as `bundleId`
(`NexusAgentExtension/IPCProtocol.swift` → `ne.FlowMsg.BundleID`). The daemon
prefers it over its own PID→`Info.plist` lookup, which is racy (PID reuse) and
empty for sandboxed / CLI-helper processes (`decideFlow` in
`packages/agent/internal/platform/darwin/platform_flow_darwin.go`). On every
flow close the daemon emits one greppable `flow verdict` line — bundle, host,
decision, `bump_status`, and a `captured` flag (true only for a
`BUMP_SUCCESS` inspect flow) — so "I configured inspection but see no body" (a
pinned host → `opaqueRelay` fallback reports `decision=inspect captured=false`)
is answerable from `agent.log` alone. A 30 s `NE liveness heartbeat` line in
`handleNEConn` makes the daemon↔NE attach state visible without waiting on the
next flow.

Builds and signing for the extension go through the `build-agent` skill; never
invoke `codesign` / `xcrun notarytool` / `swift build` directly. See
[macos-build-signing-architecture.md](macos-build-signing-architecture.md).

## References

- `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/TransparentProxyProvider.swift` — provider, `handleNewFlow`, decision/relay paths, UDP exclusion
- `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/IPCProtocol.swift` — daemon IPC client, `requestDecision` timeout, disconnect drain
- `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/QUICFallbackBundles.swift` — file-only QUIC-fallback allowlist + `TimeoutGuard`
- `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/DaemonPIDFilter.swift` — self-intercept guard
- `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/SNIParser.swift` — TLS ClientHello SNI extraction
- `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/main.swift` — system-extension entry point
- `packages/agent/cmd/agent/platformshim/quic_fallback_darwin.go` — daemon writer for `quic-bundles.json`
- `packages/agent/cmd/agent/configappliers.go` — `agent_settings` applier feeding the QUIC allowlist
- `packages/agent/cmd/agent/cmd_run.go` — daemon PID file writer
- `.cursor/rules/ne-fail-open.mdc` — the editing rule that points here
