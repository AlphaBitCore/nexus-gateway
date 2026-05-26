# E74 — macOS pf-intercept replacement of NETransparentProxyProvider

> **Goal**: bring macOS Agent traffic interception under the BSD Packet Filter (`pf`) anchor + `rdr` rule path so the five inherent gaps of the current `NETransparentProxyProvider` architecture (opt-in surface, QUIC blind spot, fail-open visibility loss, per-hop latency, process attribution drift) are structurally closed, while preserving the existing `tlsbump` MITM pipeline as the single content-aware engine.

> Epic: 74
> Status: Planning (Step 2 Requirements)
> Date: 2026-05-21
> Architecture impact: `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md` (UPDATED in the same PR as code per CLAUDE.md code/doc lockstep — new "Interception path" section describing pf anchor + rdr rules); `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md` (UPDATED — fail-open invariants Rule 1-5 carried over to the pf path, with macOS-pf-specific wording for Rule 1 "synchronous decision" and Rule 2 "fail-open timeout"); `docs/developers/architecture/README.md` (one new trigger row mapping `packages/agent/platform/darwin/**` pf code to the macOS platform doc).
> SDD: pending (Step 3 — to be drafted as `docs/developers/specs/e74/e74-s{N}-*.md` after this Requirements doc is approved).
> OpenAPI: not applicable — E74 is internal macOS intercept code with no admin-facing endpoint.
> Memory anchors: `project_e74_macos_pf_intercept` (to be created on epic kickoff), `project_ne_cursor_streamchat_verification` (deferred verification that motivated the architectural review), `project_cursor_capture_investigation` (the prior investigation that named pf as the right fix).
> Blocked by: none.
> Blocks: full closure of E73 "macOS NE content-aware coverage" item (`roadmap.md §E73 Out of scope` line 326); E75 macOS arm stories S3 / S4 / S5 (those re-run on the new pf path once E74 lands — see cross-reference in `e75-three-platform-agent-verification.md` §FR-3).

---

## 1. Background

### 1.1 Today's reality

macOS today **does** capture HTTPS content. The Swift `NETransparentProxyProvider` at `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/TransparentProxyProvider.swift` forwards inspect-mode flows to a localhost `:9443` bridge socket; the Go daemon's `packages/agent/internal/network/proxy/bridge.go` (`BumpFlow`) accepts the bridged connection and hands it to the shared `tlsbump.BumpConnection` pipeline. Content-aware hooks (PII detector, keyword filter, redact, …) run end-to-end on plaintext between the admin-trusted leaf cert and the upstream uTLS dial.

E74 is **NOT** about restoring missing TLS bump. It is about closing five inherent gaps that the NE-extension architecture imposes — gaps that the NE Apple framework itself cannot close, because they are properties of how `NETransparentProxyProvider` integrates with the OS:

### 1.2 The five gap classes (motivation)

1. **NE opt-in surface gap** — `NETransparentProxyProvider.handleNewFlow` only fires for flows the OS routes through it. Raw sockets, app-bundled DoH/DoT, certain helper-process designs, and VPN-on-VPN configurations bypass NE entirely. Empirically observed: `project_cursor_capture_investigation` — Cursor's StreamChat tunnels via `http2.connect()` and bypasses CONNECT proxy entirely; the NE doesn't see the streaming RPC. pf hooks at the BSD packet-filter layer, so coverage is **universal at the packet level**.
2. **QUIC/UDP blind spot** — `NETransparentProxyProvider` cannot reliably MITM QUIC. Today the agent works around this with `forceQUICFallbackBundles`, a manually-maintained 8-bundle allowlist (seeded in `system_metadata.agent.settings`) that closes UDP/443 specifically for known QUIC-capable bundles. Every new Electron AI desktop app, every new browser, every Chromium fork is a manual list edit. pf can drop UDP/443 selectively based on packet inspection and uid, letting happy-eyeballs do the TCP fallback for **any** process — no allowlist maintenance.
3. **Fail-open visibility loss** — because an NE hang takes down the whole Mac's network (incident 2026-05-15 documented in `agent-ne-fail-open-architecture.md` §1), every inspect path carries a fail-open timeout: `peekSNIThenDecide` falls through at 500 ms; `requestDecision` defaults to passthrough at 2 s. Every timeout → that flow is metadata-only. pf's kernel-mode redirection makes the trade-off differently — there is no `requestDecision` round-trip on the hot path because the rdr rule already steered the flow to a localhost port the daemon owns.
4. **Per-hop latency** — current path is NE Swift handler → IPC over Unix socket → Go daemon (`AgentIPCClient`) → bridge socket `127.0.0.1:9443` → `BumpFlow` → `tlsbump.BumpConnection`. That's two extra user-space crossings (NE Extension sandbox ↔ daemon). pf reduces this to: kernel `rdr` → daemon listener → `BumpFlow` directly.
5. **Process attribution drift** — NE bundle-ID attribution via `NEAppProxyFlow.metaData.sourceAppSigningIdentifier` is shaky for helper-process apps (Chrome helpers, Electron utility processes, Java JVMs spawned from a wrapper bundle). The audit row's `source_bundle` then mis-attributes traffic to the wrapper bundle. pf + `libproc` (`proc_pidpath`) attributes by **uid + parent process from the socket origin**, which matches the actual process making the syscall.

`E62` already extended `traffic_event` to embeddings; the surface keeps growing. Each new endpoint family added to the gateway re-exposes the macOS gap proportionally — the further this is left, the harder the eventual cutover.

### 1.3 What stays the same

- The existing `tlsbump` MITM pipeline (admin-trusted leaf cert, terminate client TLS, run hook pipeline on plaintext, re-encrypt to upstream via uTLS Chrome fingerprint) is unchanged. E74 reuses it without modification — there is no second MITM stack.
- `BumpFlow(ctx, clientConn, peekedClientHello, dstHost, dstPort, flowID, proc, BridgeDeps)` in `bridge.go:153` is the contract; pf path must produce the same 7-argument tuple.
- Per-request audit emission (`pipeline.NewAuditEmitter` → `auditqueue.Queue` → SQLite → Hub upload) is unchanged.
- Hook pipeline endpoint + modality awareness from E62 still applies — embeddings, chat, future audio/image all flow through `tlsbump.BumpConnection` regardless of which kernel-layer intercept fed the bytes in.

### 1.4 Locked decisions (DO NOT RE-DEBATE)

These four decisions were settled in the planning session (2026-05-21) and are pasted here so that SDD reviewers do not re-litigate them. SDD MAY refine sub-decisions inside each (e.g., D1's full-replacement vs hybrid choice is intentionally deferred to SDD); SDD MAY NOT reverse the top-level direction.

| ID | Decision | Rationale |
|---|---|---|
| D1 | **Architectural direction: pf path on top of existing `tlsbump`**. Use a pf anchor + `rdr` rules to redirect target outbound flows directly to a daemon listener that hands off to `BumpFlow`. **Deferred to SDD**: full replacement of NE vs hybrid (NE for default app traffic + pf only for the five gap-class flows). Prototype data picks. | Reuses the proven MITM stack; no second copy of cert minting / pinning / streaming policy. Hybrid-vs-full call needs prototype telemetry before being settled. |
| D2 | **Zero new Apple submissions / certifications**. Apple Developer Program (`39U3X3FFVK`), Developer ID Application + Installer certs, and the `xcrun notarytool` notarization workflow continue unchanged. The existing Network Extensions Entitlement Request approval becomes unused but **is not revoked** (preserves the NE-fallback option). The `com.apple.developer.networking.networkextension` entitlement is **removed from `.entitlements`** in the pf-only build target (local edit; no Apple round trip). The "Allow System Software" gate at first launch is **removed**. | The NE entitlement is hard to get; we keep the approval inert as insurance. Endpoint Security entitlement (D3) is **harder** than NE — refuse to pay that cost. Removing the user gate is a net friction reduction at install time. |
| D3 | **Per-process attribution: `libproc` / `proc_pidpath`, NOT Endpoint Security**. Default for the pf path. SDD MAY revisit if attribution accuracy falls short, but only by re-opening the Apple-entitlement cost discussion explicitly. | Endpoint Security would close the helper-process attribution gap but requires `com.apple.developer.endpoint-security.client`, which is harder to obtain than the NE entitlement. Accept the libproc accuracy ceiling to keep D2's "zero new Apple submissions" promise. Residual attribution gap documented explicitly in §10 Out of Scope. |
| D4 | **Install-flow friction reduction is a Requirements-level NFR**, not a side effect. pf path removes the user-facing "System Settings → Privacy & Security → Allow System Software" approval gate (no system extension to load). Capture this as NFR-7 so it surfaces in product-side review and release notes. | NFR-7 makes it visible to the product reviewer; not a buried implementation detail. |

---

## 2. Functional Requirements

### FR-1: pf interception path (anchor + rdr rules)

| ID | Requirement | Priority |
|---|---|---|
| FR-1.1 | The agent daemon installs a private `pf` anchor under `nexus-agent/transparent` containing `rdr` rules that redirect target outbound flows to a daemon-owned localhost listener. Anchor name + parent ruleset slot are recorded in `agent-macos-platform-architecture.md` §"Interception path". | Must |
| FR-1.2 | Anchor + rules are installed at daemon start and removed at daemon stop. Installation uses `pfctl -a nexus-agent/transparent -f <rules-file>`; removal uses `pfctl -a nexus-agent/transparent -F all`. Install + remove paths are idempotent — re-running install replaces the previous ruleset; re-running remove with an absent anchor is a no-op. | Must |
| FR-1.3 | Rules redirect TCP destination port 443 (and 80 on hosts where admin policy requires plain-HTTP capture) to the daemon's loopback listener. The listener port is configurable per `agent_settings` (default `13443`, separate from the legacy NE bridge port `9443` so both paths can coexist during the hybrid evaluation per D1). | Must |
| FR-1.4 | Rules redirect UDP destination port 443 **only when** the source uid is on the agent-managed QUIC-fallback set (which lives in the same Hub-pushed `agent_settings` blob as today's `forceQUICFallbackBundles`, generalised to a `quicFallbackUIDs` list resolved from the bundle list at install/refresh time). Other UDP traffic is **not** touched — fail-open invariant (DNS / DHCP / mDNS / NTP / APNS untouched per FR-7 below). | Must |
| FR-1.5 | Rules exclude the daemon's own outbound traffic by source uid (`! user nexus-agent`) so the daemon's upstream uTLS dials are not re-intercepted (otherwise: infinite loop, same shape as the E55 self-intercept guard in `TransparentProxyProvider.swift:151-167`). | Must |
| FR-1.6 | Loopback (`127.0.0.0/8`, `::1`) is excluded. Local services that bind to localhost (the daemon itself, IDE local servers, local docker port-forwards) MUST not be redirected. | Must |
| FR-1.7 | Per-host scope: domain-scoped interception is implemented in the daemon, not in pf. pf redirects **all** non-excluded outbound 443; the daemon's TCP listener consults the existing `domain.Engine` (`packages/shared/policy/domain/`) on each connection to decide `inspect` vs `passthrough` vs `deny`, exactly as the daemon's IPC `requestDecision` did for the NE path. This keeps domain rule semantics identical across the two paths and removes a class of "the rule changed but pf cache is stale" bugs. | Must |
| FR-1.8 | pf rule file is generated from a Go template owned by the daemon (not a hand-edited file on disk that an admin could drift). Source of truth lives in `packages/agent/internal/platform/darwin/pfintercept/` (new package). Coverage ≥95% per CLAUDE.md unit-test binding. | Must |

### FR-2: Daemon loopback listener + tlsbump reuse

| ID | Requirement | Priority |
|---|---|---|
| FR-2.1 | The daemon binds a TCP listener on `127.0.0.1:<port>` (default 13443) for the lifetime of the pf intercept. Listener accepts every redirected connection and immediately recovers the **original** destination IP + port using `getsockopt(SO_ORIGINAL_DST)` (macOS equivalent: `PF_IOC_NATLOOK` ioctl on `/dev/pf` keyed by source/dest tuple). The recovered IP + port is the only authoritative dst — the client's destination address on the redirected socket is the loopback listener, which is uninformative. | Must |
| FR-2.2 | Listener peeks the TLS ClientHello off the redirected socket (bounded by a 500 ms read deadline matching the existing `peekSNIThenRelay` timeout in `TransparentProxyProvider.swift:631-638`) and extracts SNI. SNI is the authoritative `dstHost` for hook + audit; the recovered IP is the fallback when SNI is absent (non-TLS, server-speaks-first protocols). | Must |
| FR-2.3 | Listener invokes existing `proxy.BumpFlow(ctx, clientConn, peekedClientHello, dstHost, dstPort, flowID, proc, BridgeDeps)` from `bridge.go:153`. No new bridge variant, no parallel MITM stack. The `proc FlowProcess` is populated per FR-3. | Must |
| FR-2.4 | Listener path honours the existing `domain.Engine` decision: `inspect` → `BumpFlow`; `passthrough` → opaque relay via `proxy.opaqueRelay`; `deny` → close with policy reason. Decisions are made in-process — no IPC round-trip — so the listener has no async-callback timeout class to worry about. | Must |
| FR-2.5 | Listener uses one goroutine per accepted connection (matches existing `bridge.go` per-flow model). Backpressure / connection-count caps reuse the existing daemon-wide limits applied to the legacy `:9443` bridge. | Must |
| FR-2.6 | Non-TLS port traffic redirected by pf (rare: an admin redirects port 80 for explicit plain-HTTP capture) hits the existing `BumpFlow` port-filter (`bridge.go:188-207`) and falls into `opaqueRelay`. No new code path. | Must |
| FR-2.7 | Per-package unit-test coverage ≥95% for the new `pfintercept` listener package per CLAUDE.md binding. Tests cover: SO_ORIGINAL_DST recovery, SNI peek, decision dispatch (inspect / passthrough / deny), self-intercept rejection, malformed ClientHello fall-through. | Must |

### FR-3: Process attribution via `libproc`

| ID | Requirement | Priority |
|---|---|---|
| FR-3.1 | On each accepted connection, the listener resolves the originating process using `libproc` (`proc_pidpath` for executable path, `proc_pidinfo` for parent pid + start time, `pwd.h` `getpwuid_r` for owning OS user) from the source port + uid combination on `/dev/pf` lookups. Recovered values populate `proxy.FlowProcess{Name, Bundle, User}` per `bridge.go:123-127`. | Must |
| FR-3.2 | `Bundle` field is populated from `proc_pidpath` + `LSCopyBundleIdentifierFromExecutable` (or equivalent CoreServices lookup) when the executable resolves to an `.app` bundle; left empty for non-bundle executables (CLI binaries, scripts) — that is correct behaviour, not a bug, and matches today's NE path for unsigned/non-bundle apps. | Must |
| FR-3.3 | When `libproc` lookup fails (process exited before resolution, permission error), the listener stamps `proc = FlowProcess{}` and proceeds — `BumpFlow` accepts empty process info per existing contract (`bridge.go:120-122` documents the empty-FlowProcess case). The audit row's `source_process` / `source_bundle` columns are empty for that flow; this is observable to operators in the UI and acceptable. | Must |
| FR-3.4 | Residual attribution gap (helper-process apps where `proc_pidpath` returns the helper executable but the user expected the parent bundle) is documented explicitly in §10 Out of Scope. Endpoint Security would close this gap; D3 rejects that cost. | Must |
| FR-3.5 | Attribution unit tests: table-driven cases for (a) bundle app, (b) CLI binary, (c) Chrome-helper-style child of a known bundle, (d) process-exited-before-lookup, (e) permission-denied. Coverage ≥95%. | Must |

### FR-4: Fail-open invariants on the pf path

The five fail-open rules from `agent-ne-fail-open-architecture.md` §2 are binding and must transfer to the pf path. The pf path's mechanics differ from NE's, so each rule re-states what the equivalent invariant looks like under pf:

| ID | Requirement | Priority |
|---|---|---|
| FR-4.1 | **Rule 1 transfer — synchronous decision**: the loopback listener decides the path (`inspect`, `passthrough`, `deny`) synchronously inside `domain.Engine.Evaluate` (which is in-memory, non-blocking, returns within microseconds). No async daemon callback class exists on the hot path — Rule 2's timeout-fallback is therefore vacuously satisfied. The pf rule is the only thing that "claims" a flow; once a flow lands on the listener, the listener owns it for the duration. | Must |
| FR-4.2 | **Rule 2 transfer — fail-open timeout**: the only async surface that could hang is the 500 ms SNI peek read (FR-2.2). On timeout, the listener falls through to passthrough with the recovered IP as `dstHost` — exactly the same behaviour as `TransparentProxyProvider.swift:633-638`. No other async path exists. | Must |
| FR-4.3 | **Rule 3 transfer — no hardcoded enforcement lists in Swift / Go pf glue**: the QUIC-fallback uid set (FR-1.4), domain rules, deny lists, exemption lists all arrive via Hub-pushed `agent_settings` shadow → daemon-managed JSON file → loaded into in-memory snapshots. Empty-as-fail-safe: an empty `quicFallbackUIDs` list means no UDP rules are installed (NOT "redirect everyone's UDP/443"). Equivalent to the existing `quic-bundles.json` empty-as-fail-safe contract. | Must |
| FR-4.4 | **Rule 4 transfer — no `isLikelyXyz = true` placeholders**: pf rule generation and `libproc` attribution code MUST NOT contain placeholder booleans flipped to `true` for "we'll wire this up later" semantics. Reviewer mechanically rejects any such PR (grep `isLikely.*= true` returns zero in pf code). | Must |
| FR-4.5 | **Rule 5 transfer — system DNS/DHCP/Push UID exclusion**: `mdnsresponder`, `configd`, `dhcpcd`, `apsd`, `nsurlsessiond`, `kdc`, `ntpd` MUST NEVER have their UDP redirected. Enforced two ways: (a) pf rules redirect only when source uid is in the `quicFallbackUIDs` allowlist (so system services are excluded by default because they don't appear on the bundle-derived list); (b) defensive denylist of the seven daemons' uid range applied as a `pass` rule above the `rdr` rule, so even if the allowlist accidentally included one of them, pf would still pass instead of redirect. | Must |
| FR-4.6 | Recovery procedure: when pf rules are stuck (rare: daemon panic between `pfctl -f` and `pfctl -F`), `sudo pfctl -a nexus-agent/transparent -F all` removes the anchor. This is documented in `agent-macos-platform-architecture.md` §"Recovery procedure" alongside the existing NE recovery commands. Tier-1 doc `agent-ne-fail-open-architecture.md` cross-references this section. | Must |
| FR-4.7 | 24-hour developer-machine stability test: install the pf path on a developer's daily-driver Mac; verify no manual recovery action is required for 24 h of normal usage (browsing, video calls, IDE, Slack, the works). Same gate as the existing NE 24-hour gate per `agent-ne-fail-open-architecture.md` §4. Recorded in the SDD acceptance criteria. | Must |
| FR-4.8 | Stress test: simulate daemon panic mid-flow (kill -9 the daemon while connections are active). pf rules MUST be auto-removed within 30 s of daemon death. Implementation: the daemon registers a launchd `KeepAlive=true` + a separate `nexus-agent-pfclean` LaunchDaemon (or equivalent on-exit hook) that runs `pfctl -a nexus-agent/transparent -F all` whenever the main daemon is not running. Verified by killing the daemon and observing pf rules cleared. | Must |

### FR-5: Migration from the existing NE path

| ID | Requirement | Priority |
|---|---|---|
| FR-5.1 | The agent daemon ships a single binary that supports both intercept modes; mode selection is via `agent_settings.interceptMode = "ne" | "pf" | "hybrid"` (default `ne` for the install-base at the time of E74 cutover; flips to `pf` after E74-S5 finishes the parallel-run stability period). Cat B shadow key per `thing-config-pull-model` binding. | Must |
| FR-5.2 | `interceptMode="hybrid"` is the prototype mode used during evaluation: NE handles default app traffic, pf handles the QUIC + raw-socket + helper-process gap classes (FR-7 below). The decision rule for which path handles which flow is implemented in the daemon, not split across NE Swift + pf code — single source of truth. SDD picks between full replacement and hybrid based on hybrid-mode prototype data. | Must |
| FR-5.3 | Migration path for existing NE-installed agents: a daemon upgrade installs the new pf code but leaves `interceptMode="ne"` until the operator explicitly flips it via `agent_settings` in the admin UI. The flip is reversible — flipping back to `"ne"` removes the pf anchor and re-engages NE. No re-install or user-visible re-approval required. | Must |
| FR-5.4 | Mode flip telemetry: when `interceptMode` changes, the daemon emits an `audit_event` row of kind `agent_intercept_mode_change` carrying old + new mode, before/after rule-load timestamps, and rule install success/failure. Operator-visible in the admin UI audit log. | Must |
| FR-5.5 | `interceptMode="pf"` mode (the eventual default) removes the system-extension activation requirement and the user-facing "Allow System Software" gate from the install flow (NFR-7 below). Once admin flips to `pf`, fresh installs of the same agent build skip the system extension entirely. | Must |
| FR-5.6 | Installer pkg (`.pkg`) detects whether the target Mac already has the NE system extension activated; if yes, the pkg leaves it installed (so flipping back to `"ne"` is a config change, not a re-install). If no, the pkg can skip the system-extension activation step entirely when the default config is `pf`. | Must |

### FR-6: Build + signing impact

| ID | Requirement | Priority |
|---|---|---|
| FR-6.1 | The `pf-only` build target removes `com.apple.developer.networking.networkextension` from `NexusAgentExtension.entitlements`. The NE bundle target retains it (so a single pkg can ship both modes during transition). New build artefact: `NexusAgent-pf-only.pkg` (no NE bundle inside). | Must |
| FR-6.2 | All builds go through the `build-agent` skill per CLAUDE.md macOS build binding. The skill is updated to support a `--target=pf-only` argument that emits the slimmer pkg. No improvised `codesign` / `pkgbuild` / `productbuild` / `xcrun notarytool` invocations. | Must |
| FR-6.3 | Notarization: pf-only build is notarized through the same `xcrun notarytool` workflow as the NE build. No new Apple-side keys, profiles, or submissions. | Must |
| FR-6.4 | macOS 26 launch-constraint XML is unaffected — pf code runs inside the daemon binary, which already has the correct launch constraint per `macos-build-signing-architecture.md` §4. | Must |
| FR-6.5 | Existing Network Extensions Entitlement Request approval (Apple-side) remains active but unused on the pf-only build. The approval is **not revoked** — it preserves the option to flip a future build back to NE if pf has a fundamental gap. | Must |
| FR-6.6 | Code signing identity (`Developer ID Application: <team>`) and team identifier (`39U3X3FFVK`) unchanged. | Must |

### FR-7: Gap-class closure demonstration (acceptance gate)

This is the **critical gate before closing E74** (per `roadmap.md §E74` lines 357). At least one flow per gap class must be empirically demonstrated to work under pf where it did not under NE. The five gaps are concrete; each gets a named synthetic test.

| ID | Requirement | Priority |
|---|---|---|
| FR-7.1 | **Gap 1 closure — raw socket**: a small synthetic test app (Go binary, no NE-aware code) opens a raw TCP socket to `api.openai.com:443` and sends a hand-rolled HTTPS request. Under NE, this flow MAY bypass NE (depending on how Apple's traffic-classifier categorises the test bundle). Under pf, the request MUST be intercepted, MITM-bumped, and produce a `traffic_event` row with `source='agent'`, `endpoint_type='chat'` (or whichever the payload requests), and populated `request_normalized`. Synthetic test source lives in `packages/agent/internal/platform/darwin/pfintercept/testfixtures/`. | Must |
| FR-7.2 | **Gap 2 closure — QUIC fallback without bundle list**: Chrome on a fresh user account (no `forceQUICFallbackBundles` admin push) accesses `chatgpt.com` (or any QUIC-preferring AI host). Under NE, the flow stays on UDP/443 and is invisible. Under pf, the UDP redirect rule kicks in (uid in the QUIC fallback set generalised from "bundle list" to "uid list at install time"), Chrome falls back to TCP, and the flow is captured. `traffic_event` row records `endpoint_type='chat'`, source uid attribution is correct. | Must |
| FR-7.3 | **Gap 3 closure — fewer fail-open metadata-only flows**: under load (10 concurrent IDE sessions hitting AI providers for 5 minutes), the pf path produces ≥95% inspect-mode `traffic_event` rows with populated content fields (request/response normalized JSON). The NE baseline on the same load is recorded as the reference; the test gate is "pf does not regress; in practice, the absence of the 2s `requestDecision` timeout class should improve content-capture rate". The exact threshold (95%) is the practical target; the SDD MAY adjust based on E62 baseline measurements. | Must |
| FR-7.4 | **Gap 4 closure — latency**: synthetic measurement compares NE-path latency vs pf-path latency on the same Mac, same upstream, same request shape. The pf path SHOULD show ≤80% of the NE-path p95 user-space-crossing overhead (excluding the upstream RTT, which is identical). This is reported in the E74-S5 smoke run as a single observability number, not a hard gate. | Should |
| FR-7.5 | **Gap 5 closure — helper-process attribution**: a Chrome flow that NE attributed to `com.google.Chrome.helper` (the generic helper bundle) is, under pf + `libproc`, attributed to `com.google.Chrome` (the actual user-facing app) via `proc_pidinfo`'s parent-pid walk. Verified by spot-checking 10 helper-process flows. The residual gap (deeply nested helper trees where `libproc` cannot recover the user-facing bundle) is documented in §10 Out of Scope per D3. | Must |
| FR-7.6 | Each FR-7.1 — FR-7.5 test is wired into a new `/test-macos-pf-agent` skill (mirrors the existing `/test-cursor-adapter`, `/test-compliance-proxy` patterns) so the acceptance gate can be re-run on every prod release. Skill source: `.claude/skills/test-macos-pf-agent/`. The skill produces a markdown report at `/tmp/test-macos-pf-agent-<UTC-ts>.md`. | Should |

### FR-8: Configuration surface (admin-facing)

Per CLAUDE.md "less is more / delete instead of add": the admin surface MUST be minimal.

| ID | Requirement | Priority |
|---|---|---|
| FR-8.1 | The only admin-facing dial added by E74 is `interceptMode` (FR-5.1) — a single global setting per Hub Thing (the agent), three values: `ne` | `pf` | `hybrid`. Default per build: `ne` during transition, flips to `pf` once gap-class closure (FR-7) is signed off in prod. No per-host overrides. No per-bundle overrides. | Must |
| FR-8.2 | The existing `forceQUICFallbackBundles` setting (today a bundle-ID list) is generalised in shape to `quicFallbackUIDs` for the pf path's needs. To avoid an admin-facing rename + migration, **the admin still sees a bundle list in the UI**; the daemon does the bundle → uid resolution at install time using `LSCopyApplicationURLsForBundleIdentifier`. The admin's mental model is unchanged. | Must |
| FR-8.3 | No new admin route or sidebar nav entry. `interceptMode` lives on the existing Agent Thing's Detail page under the existing "Interception settings" card (or whatever the post-E74 UI calls it; this is a Tier-3 UI detail). | Must |
| FR-8.4 | IAM impact review per CLAUDE.md binding: `interceptMode` is read/written via the existing `system_metadata.agent.settings` Hub admin API. No new IAM action needed; existing `admin:settings.read` + `admin:settings.update` policies cover it. Documented in the SDD commit message per CLAUDE.md "API / menu / route changes require IAM impact review". | Must |
| FR-8.5 | Admin-UI changes required: the Agent Thing's Detail page gains a single dropdown ("Intercept mode: NE legacy / pf (recommended) / Hybrid prototype"). The text labels are i18n keys per CLAUDE.md i18n binding. No other UI changes. | Must |

### FR-9: Documentation lockstep

| ID | Requirement | Priority |
|---|---|---|
| FR-9.1 | `agent-macos-platform-architecture.md` is updated in the **same PR** as the code that introduces the pf path. New §3a "pf interception path (E74)" replaces / augments §3 "macOS-specific path conventions". | Must |
| FR-9.2 | `agent-ne-fail-open-architecture.md` is updated in the **same PR** as the code. Each of Rules 1-5 gets a "pf path equivalent" subsection citing the FR-4.x clauses. The doc remains Tier-1 + SAFETY-CRITICAL — the title generalises to "Agent macOS Intercept Fail-Open Architecture" (NE-specific framing softened). | Must |
| FR-9.3 | `docs/developers/architecture/README.md` gains one new trigger row mapping `packages/agent/internal/platform/darwin/pfintercept/**` → the updated macOS platform doc per CLAUDE.md `arch-doc-trigger-check`. | Must |
| FR-9.4 | `docs/users/features/agent-ui/` is updated for the install-flow change (no more "Allow System Software" gate). Specifically the Agent install-and-enrol flow doc gains a note that the system extension prompt is only shown for legacy NE installs. | Must |
| FR-9.5 | The agent support runbook (`docs/operators/ops/runbooks/agent-recovery.md` or equivalent — the doc that today carries "incident 2026-05-15" recovery) is updated with the pf recovery command (`pfctl -a nexus-agent/transparent -F all`). | Must |
| FR-9.6 | Memory anchor `project_e74_macos_pf_intercept` is created during epic kickoff per CLAUDE.md memory-anchor convention. | Should |

---

## 3. Non-Functional Requirements

| ID | Requirement | Notes |
|---|---|---|
| NFR-1 | pf-path latency: p95 user-space-crossing overhead per intercepted flow ≤80% of the NE-path baseline. Verified by FR-7.4 measurement. | Hot path is now kernel `rdr` → daemon listener → `BumpFlow` directly; one fewer IPC than the NE → daemon → `:9443` → `BumpFlow` chain. |
| NFR-2 | Content-capture rate under load: ≥95% of inspect-mode flows produce populated `request_normalized` + `response_normalized` content (vs metadata-only). The NE baseline on the same load is the reference; the bar is "no regression + observable improvement". Verified by FR-7.3. | The 2 s `requestDecision` timeout-to-passthrough class disappears under pf; the only remaining timeout is the 500 ms SNI peek (FR-4.2). |
| NFR-3 | Fail-open: under daemon panic mid-flow, pf rules MUST be auto-removed within 30 s (FR-4.8). Network connectivity restored for non-intercepted flows immediately when the daemon dies (pf is in the daemon's anchor — anchor removal restores full pass behaviour). | Equivalent to the NE rule's "every async callback has a fail-open timeout" — there are no async callbacks on the pf hot path so the rule is structurally satisfied. |
| NFR-4 | Per-package Go unit-test coverage ≥95% on every new / modified package (CLAUDE.md binding): `packages/agent/internal/platform/darwin/pfintercept/`, plus any helper packages added under `packages/agent/internal/platform/darwin/`. | Per the existing 95% binding. |
| NFR-5 | Adapter / hook framework unchanged — E74 does not modify `shared/tlsbump`, `shared/policy/hooks/`, `shared/transport/normalize/`, or any provider adapter. Verified by clean `git diff` on those paths in the E74 PR. | Single MITM stack, single hook framework, single normalize. |
| NFR-6 | Backward compatibility within the agent install base: see FR-5 migration. Flipping `interceptMode` is reversible. Pre-GA "no defer" policy applies — no `@deprecated` markers added to NE Swift code; once SDD picks "full replacement" the NE Swift code is deleted in that PR. | The NE code stays alive while `interceptMode="ne"` / `"hybrid"` is supported, which it is until SDD picks. |
| NFR-7 | Install-flow friction reduction (per D4): pf-only builds remove the user-facing "System Settings → Privacy & Security → Allow System Software" approval gate. Time-to-working-install on a fresh Mac drops from "user must approve + restart" to "pkg installs + daemon runs". | NFR explicitly so the UX win surfaces in release notes + product review. |
| NFR-8 | Zero new Apple submissions (per D2). The Apple Developer Program, Developer ID certs, notarization workflow, NE entitlement (kept inert), and existing launch constraints are unchanged. No new Apple-side cost, no new approval round-trip. | Hard constraint — if SDD discovers a sub-decision that violates this, the SDD pauses and asks the user. |
| NFR-9 | Process attribution accuracy: `libproc`-based attribution correctly identifies the user-facing bundle for ≥90% of intercepted flows on the developer-machine 24 h test (FR-4.7). The residual ≤10% is the helper-process attribution gap documented in §10 Out of Scope (per D3). | Quantified for review. If <90% on the prototype, the SDD MUST re-open D3. |
| NFR-10 | Stability — no regressions in existing chat, embedding, IDE, web-consumer flows on the macOS agent. Measured by re-running the E73 Tier-1 IDE adapter tests + `/test-cursor-adapter` + a smoke pass through the agent UI against `chatgpt.com` and `claude.ai`. | These are E73 / E62 / E75 smoke surfaces; E74 must leave them green. |
| NFR-11 | English-only doc + commit messages (CLAUDE.md binding). | Chat language can vary; committed text stays English. |
| NFR-12 | Code/doc lockstep (CLAUDE.md binding): FR-9 enumerates the doc updates that MUST land in the same PR as the code. | Enforced by the existing `check-doc-lockstep.mjs` once the trigger map is updated per FR-9.3. |
| NFR-13 | The SDD MUST include a step that adds an entry to the E86 `e2e-coverage-matrix` doc-lockstep file at `docs/developers/specs/e86-e2e-coverage-matrix.md` (in the E86 worktree at the time of E74's SDD drafting) for the new `/test-macos-pf-agent` skill, so future feature work cannot land without exercising the pf-path acceptance gate. | Cross-reference to the E86 binding `doc-lockstep` entry. |

---

## 4. User Roles & Personas

- **macOS agent operator (admin)** — flips `interceptMode` from `ne` → `pf` → `hybrid` via the admin UI. Reads the audit log for `agent_intercept_mode_change` events. Cares about: reversibility, no user-visible re-prompt, no extension reactivation, smooth install on new Macs.
- **End user (developer / employee with the agent installed)** — installs the agent via the .pkg, ideally with **no** "Allow System Software" approval (NFR-7). Does NOT touch intercept-mode config. Cares about: their Mac's network stays up, their AI workflows (chat / IDE / browser) keep working, no surprise prompts.
- **Compliance-proxy / shared maintainer** — does not change behaviour. The `shared/tlsbump`, `shared/policy/hooks/`, and adapter code is unchanged; E74 only swaps the kernel-level intercept feeding bytes in. Cares that: contract-level guarantees (`BumpFlow` signature, per-request audit emission, hook pipeline ordering) are preserved.
- **Apple compliance reviewer (Anthropic-internal abstraction)** — D2 commits us to "no new Apple submissions". This persona is the conscience of D2; SDD reviewers wear this hat to challenge any sub-decision that would require a new entitlement or a re-notarization with new keys.
- **Incident responder** — when the pf path malfunctions, executes `pfctl -a nexus-agent/transparent -F all` to clear rules. Cares about: recovery procedure documented in the runbook (FR-9.5), audit-log breadcrumbs in the moments before the malfunction.
- **Future endpoint-typology epic implementer (E63 audio / E64 image / E66 video)** — relies on `tlsbump` + `BumpFlow` staying unchanged. E74 explicitly does not touch the hook framework or the normalize pipeline.

---

## 5. Constraints & Assumptions

### Constraints

- C-1 (binding, D2): Zero new Apple submissions. No new entitlements applied for, no new certificate types acquired, no new notarization keys generated. The existing NE entitlement is kept inert but not revoked.
- C-2 (binding, D3): No Endpoint Security entitlement. `libproc` is the attribution tool. Residual helper-process attribution gap is accepted and documented.
- C-3 (binding, CLAUDE.md): macOS NE proxy must fail-open, never fail-closed — the rule generalises to "macOS agent intercept must fail-open" and applies in full to the pf path (FR-4).
- C-4 (binding, CLAUDE.md): All macOS builds go through the `build-agent` skill. No improvised `codesign` / `pkgbuild` / `productbuild` / `xcrun notarytool` invocations.
- C-5 (binding, CLAUDE.md): Per-package unit-test coverage ≥95% (NFR-4).
- C-6 (binding, CLAUDE.md): Code/doc lockstep — architecture + feature + runbook doc updates land in the same PR as code (FR-9).
- C-7 (binding, CLAUDE.md): IAM impact review for the `interceptMode` admin UI change (FR-8.4).
- C-8 (binding, CLAUDE.md): No backward compatibility in pre-GA semantics. Once SDD picks "full replacement" the NE Swift code is deleted in that PR; no parallel "legacy" path retained beyond the migration window.
- C-9 (binding, CLAUDE.md): English-only repository text.
- C-10 (binding, CLAUDE.md): Secrets are env-only. E74 does not introduce new secrets; cert minting + uTLS dialing reuse the existing `TLSEngine` + `UpstreamTransport`.
- C-11 (binding, CLAUDE.md): AI Gateway / `traffic_event` changes require a smoke run before "done". E74 does not modify AI Gateway code or `traffic_event` schema directly, but it changes the upstream content flow into the audit pipeline; the SDD's acceptance criteria include re-running `/smoke-gateway` (or a scoped subset that exercises agent-emitted traffic_event rows) as a regression check.
- C-12 (binding): pf is a BSD facility maintained by Apple. If a future macOS release deprecates pf (no current signal as of 2026-05-21), the NE-inert path is the fallback — see C-1 — but planning for that contingency is out of scope.
- C-13 (binding): Synthetic gap-class closure (FR-7) is the named acceptance gate; E74 does not close until all five are demonstrated.

### Assumptions

- A-1: macOS 14 / 15 / 26 all support pf with anchor + `rdr` rules in the manner described. The `pfctl` command-line tool is shipped with the OS at `/sbin/pfctl` and remains available without additional install.
- A-2: `getsockopt(SO_ORIGINAL_DST)` or its macOS equivalent (`PF_IOC_NATLOOK` ioctl on `/dev/pf`) returns the pre-redirect destination for connections that landed via `rdr`. (Verified at SDD start with a kernel-level prototype.)
- A-3: `libproc` (`proc_pidpath`, `proc_pidinfo`) accuracy on macOS 14+ is sufficient for ≥90% bundle attribution (NFR-9). (Verified by the 24 h developer-machine test in FR-4.7.)
- A-4: Hub-pushed `agent_settings` shadow already carries the bundle list infrastructure; generalising to `quicFallbackUIDs` (FR-1.4) is an additive shadow-key change, not a redesign. Reuses `thing-config-pull-model` per the binding.
- A-5: The `build-agent` skill can accept a new `--target=pf-only` argument without disturbing the existing build flow (FR-6.2). (Verified at SDD start; if the skill needs a non-trivial extension, the SDD records that work as its own task.)
- A-6: `LSCopyApplicationURLsForBundleIdentifier` (FR-8.2) is available on macOS 14+ via the LaunchServices framework. (Confirmed in Apple developer docs as of 2026-05-21.)
- A-7: The existing `BridgeDeps` struct (`bridge.go:34-56`) requires no shape changes for the pf path — the listener constructs the same struct and calls `BumpFlow` with it. (Verified by reading `bridge.go` 2026-05-21.)
- A-8: pf rule scope on macOS supports `rdr` to a non-listening localhost port + same-process listener (vs the FreeBSD-classic two-process model). This is the basis for FR-1.3's "daemon-owned loopback listener". (Verified at SDD start.)
- A-9: pf's UID-based filtering on macOS supports the equivalent of `! user nexus-agent` syntax (FR-1.5). The exact pfctl syntax may differ from FreeBSD canonical; SDD records the macOS-specific form.

---

## 6. Glossary

- **pf** — the BSD Packet Filter, the kernel-level firewall / NAT facility shipped with macOS, managed via `/sbin/pfctl`. Used here for `rdr` (redirect) rules that steer outbound TCP/UDP to a daemon-owned localhost listener.
- **anchor** — a named sub-ruleset in pf. The agent's rules live under anchor `nexus-agent/transparent`, isolated from any other admin-managed pf state.
- **rdr** — pf's redirect-on-NAT rule type. Used to send packets to a destination other than what the client addressed.
- **NE / NETransparentProxyProvider** — Apple's NetworkExtension framework's user-space transparent proxy. The current macOS intercept layer (`packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/TransparentProxyProvider.swift`); to be replaced or complemented by pf per E74.
- **tlsbump** — the shared MITM pipeline at `packages/shared/transport/tlsbump/`. Terminates client TLS with an admin-trusted leaf cert, runs the hook pipeline on plaintext, and re-encrypts to upstream via uTLS Chrome fingerprint. Reused unchanged by E74.
- **bridge socket** — the existing localhost listener on `:9443` that NE forwards inspect-mode flows to. E74 introduces a parallel listener on `:13443` for pf-redirected flows; both feed into the same `BumpFlow`.
- **`BumpFlow`** — the Go entry point at `packages/agent/internal/network/proxy/bridge.go:153`. The contract pf must satisfy: `(ctx, clientConn, peekedClientHello, dstHost, dstPort, flowID, proc, BridgeDeps) → error`.
- **fail-open** — invariant from `agent-ne-fail-open-architecture.md`: an intercept malfunction MUST degrade to passthrough (uninspected traffic still flows), never to a stuck or blocked flow. Network connectivity > inspection coverage.
- **gap class** — one of the five inherent NE-architecture coverage / ergonomics gaps closed by pf: (1) opt-in surface, (2) QUIC blind spot, (3) fail-open visibility loss, (4) per-hop latency, (5) process attribution drift.
- **libproc** — macOS userspace library for process introspection (`proc_pidpath`, `proc_pidinfo`, etc.). Default attribution tool for E74 (per D3).
- **ES (Endpoint Security)** — Apple's higher-privilege process-event framework requiring `com.apple.developer.endpoint-security.client`. Explicitly rejected as the attribution tool for E74 (per D3 — harder to obtain than NE).
- **SO_ORIGINAL_DST / PF_IOC_NATLOOK** — the syscall that recovers the original destination IP+port from a redirected socket. Required because the redirected socket's `getpeername` returns the listener's loopback address.
- **`interceptMode`** — the single E74-introduced admin-facing dial: `ne` | `pf` | `hybrid`. Default flips from `ne` to `pf` after FR-7 acceptance.
- **hybrid mode** — `interceptMode="hybrid"`: NE handles default app traffic, pf handles the five gap-class flows. Used as a prototype to gather data informing the SDD's "full replacement vs hybrid" choice.

---

## 7. MoSCoW Priority Summary

**Must (in scope for E74):**

- pf anchor + rdr installation, removal, and idempotency (FR-1.1, FR-1.2, FR-1.6, FR-1.8).
- pf rule shape for TCP 443 redirect (FR-1.3, FR-1.5) and UID-scoped UDP 443 redirect for QUIC fallback (FR-1.4).
- Domain-engine scoping in the daemon (not in pf) (FR-1.7).
- Daemon loopback listener with SO_ORIGINAL_DST / NATLOOK recovery (FR-2.1), SNI peek + 500 ms timeout (FR-2.2), `BumpFlow` reuse (FR-2.3), decision dispatch (FR-2.4), per-flow goroutine model (FR-2.5), non-TLS port handling (FR-2.6).
- `libproc`-based process attribution including FlowProcess population (FR-3.1, FR-3.2, FR-3.3), residual gap documentation (FR-3.4), attribution unit tests (FR-3.5).
- Fail-open Rules 1-5 transfer to pf path (FR-4.1 – FR-4.5), pf recovery procedure (FR-4.6), 24 h stability test (FR-4.7), daemon-panic auto-removal (FR-4.8).
- `interceptMode` shadow key (FR-5.1), `hybrid` prototype mode (FR-5.2), reversible migration (FR-5.3), mode-flip audit (FR-5.4), system-extension activation conditional on legacy mode (FR-5.5, FR-5.6).
- pf-only build target removing NE entitlement (FR-6.1), `build-agent` skill extension (FR-6.2), notarization unchanged (FR-6.3 – FR-6.6).
- Gap-class closure tests: Gap 1 raw socket (FR-7.1), Gap 2 QUIC without bundle list (FR-7.2), Gap 3 fail-open visibility (FR-7.3), Gap 5 helper-process attribution (FR-7.5).
- Admin configuration surface: single `interceptMode` dial, no per-host overrides, `quicFallbackUIDs` admin sees bundle list (FR-8.1 – FR-8.3, FR-8.5).
- IAM impact review (FR-8.4).
- Code/doc lockstep updates: macOS platform arch (FR-9.1), NE fail-open arch generalised (FR-9.2), trigger row (FR-9.3), agent UI install doc (FR-9.4), recovery runbook (FR-9.5).

**Should (nice to have, in scope if time permits):**

- Latency measurement reported as observability number (FR-7.4).
- `/test-macos-pf-agent` skill productisation (FR-7.6).
- Memory anchor `project_e74_macos_pf_intercept` created at kickoff (FR-9.6).

**Could (deferred to follow-up work):**

- Optimised pf rule update on hot domain-rule changes (today: agent re-evaluates per-connection in the listener; could be moved to pf-level rules on a per-IP basis if telemetry shows the listener is a bottleneck).
- pf rule introspection in admin UI (`pfctl -s rules -a nexus-agent/transparent` exposed read-only on the Agent Detail page). Defer until operators ask.
- BPF / `dtrace`-based intercept variant as a fallback if pf is deprecated by Apple (currently no signal).

**Won't (out of scope for E74):**

- Windows / Linux pf changes (already pf-equivalent on Linux per `agent-linux-platform-architecture.md`; Windows is WinDivert, untouched). Per `roadmap.md §E74 Out of scope` line 359.
- NetExtension fallback for incompatible kernels — that is a separate story, tracked separately. Per `roadmap.md §E74 Out of scope` line 359.
- Endpoint Security integration for deeper helper-process attribution (per D3 — explicit decision).
- Generic L4 packet capture (raw `BPF` ingest) — out of scope; pf `rdr` + daemon listener is the chosen mechanism.
- Performance benchmarking under sustained load (correctness + p95 user-space overhead measurement only, per FR-7.4).
- Per-IDE / per-web adapter verification on the pf path — that is E73's job; E75 macOS arm re-runs S3 / S4 / S5 against the new path once E74 lands.

---

## 8. Open questions for SDD

These are unsettled sub-decisions that the SDD must resolve. They are NOT D1-D4 reversals — they are choices within the locked direction.

- **Q1**: Full replacement of NE vs hybrid mode as the long-term default. D1 defers this to prototype data. SDD records the decision after FR-5.2 hybrid mode has produced at least 7 days of telemetry.
- **Q2**: pf rule scope — redirect all 443 in scope vs domain-IP-resolved per-IP rules. Trade-off: per-IP rules require IP refresh as DNS rotates (the agent already runs a domain → IP resolver — see `agent-linux-platform-architecture.md` §8); all-443 is simpler but the listener does more work for flows it ultimately passes through. SDD picks; the daemon-side `domain.Engine` consultation (FR-1.7) hides the choice from the rest of the codebase.
- **Q3**: Bundle → uid resolution timing (FR-8.2) — at admin push (eager, requires LaunchServices lookup at config-load time) vs at first-flow (lazy, less startup cost but UDP rule may be inactive for the first connection from a fresh-install app). SDD picks.
- **Q4**: Loopback listener port choice — fixed `13443` (NFR-1's example) vs dynamic per install. Fixed is simpler to debug; dynamic avoids conflicts with admin-managed services on the same Mac. SDD picks; if dynamic, the port is recorded in `agent_settings` reported state for ops visibility.
- **Q5**: `SO_ORIGINAL_DST` macOS equivalent — `PF_IOC_NATLOOK` ioctl on `/dev/pf` is the canonical answer; some implementations use a private socket option. SDD verifies which one works on macOS 14 / 15 / 26 and records.
- **Q6**: Hot-reload of pf rules on `agent_settings` shadow update — re-`pfctl -f` (atomic load) vs incremental edit. Atomic load is simpler and matches Linux iptables practice; trade-off is a single-millisecond window where no rules are active. SDD picks (default: atomic re-load).

---

## 9. Acceptance criteria summary

E74 closes when all of the following hold (the SDD acceptance criteria expand each):

1. All Must-priority FRs above are implemented and unit-tested with ≥95% coverage per package (CLAUDE.md binding).
2. The five gap-class closure tests (FR-7.1 – FR-7.5) all pass on a clean macOS VM image.
3. The 24 h developer-machine stability test (FR-4.7) passes without manual recovery action.
4. Daemon-panic auto-recovery (FR-4.8) demonstrated.
5. `agent-macos-platform-architecture.md` + `agent-ne-fail-open-architecture.md` + the trigger-row + UI install doc + recovery runbook are landed in the same PR sequence as the code (FR-9).
6. `interceptMode` admin UI dial works, flipping is reversible, audit log captures changes (FR-5).
7. Existing chat + embedding + IDE + web-consumer flows on the macOS agent regress no further than the pre-E74 baseline (NFR-10).
8. `build-agent --target=pf-only` produces a notarized `.pkg` (FR-6).
9. SDD's open questions Q1-Q6 have recorded decisions in the SDD doc.
10. The E86 `e2e-coverage-matrix` doc-lockstep file includes a row for `/test-macos-pf-agent` (NFR-13).

---

## 10. Out of scope (explicit)

- Windows / Linux pf-equivalent changes (per `roadmap.md §E74 Out of scope`).
- NetExtension fallback for incompatible kernels (separate story).
- Endpoint Security entitlement adoption (per D3 — deeper helper-process attribution is the residual gap).
- Adapter / hook framework changes (this is E62 + future epics, not E74).
- Performance benchmarking beyond the FR-7.4 p95 measurement.
- Per-IDE / per-web adapter coverage testing on the new path — E73 covers adapters; E75 covers platform-level re-verification of S3 / S4 / S5 macOS arm stories.
- Generic raw-BPF packet capture as a pf alternative.
- macOS pf for non-agent code paths (Hub / Control Plane / Compliance Proxy / AI Gateway are Linux-server services with no macOS pf surface).

---

## 11. Cross-references

- `e75-three-platform-agent-verification.md` §FR-3 — macOS arm stories S3 / S4 / S5 re-run on the pf path once E74 lands; E75's macOS arm verifies the current NE path, and E74's acceptance gate extends E75 coverage retroactively to the new path.
- `docs/developers/roadmap.md` §E74 — original scoping entry; lines 332-359.
- `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md` — binding fail-open invariants. E74 generalises this doc; updates land in the same PR per FR-9.2.
- `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md` — current macOS platform doc; updates per FR-9.1.
- `docs/developers/architecture/services/agent/agent-linux-platform-architecture.md` — structural analogue for pf-style intercept (iptables + REDIRECT); E74 follows its idioms.
- `packages/agent/internal/network/proxy/bridge.go` — `BumpFlow` entry point, reused unchanged.
- `packages/shared/transport/tlsbump/` — MITM pipeline, reused unchanged.
- `.cursor/rules/iam-impact-review.mdc` — IAM impact review for FR-8.4.
- `.claude/skills/build-agent/` — canonical build skill; FR-6.2 extends it.
- CLAUDE.md binding `macOS NE proxy must fail-open` — generalised to "macOS agent intercept must fail-open" per FR-4 + FR-9.2.
- Memory `project_cursor_capture_investigation` — empirical confirmation that pf is the right fix for the StreamChat capture gap.
- Memory `project_ne_cursor_streamchat_verification` — deferred verification that motivated this epic's architectural review.
