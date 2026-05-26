# E75 — Three-Platform Agent End-to-End Verification

> **Goal**: bring all three Agent platforms (macOS NE, Linux pf/iptables, Windows WinDivert) under one comprehensive synthetic verification programme that exercises the full install → intercept → hook → audit → uninstall flow on clean VM images, so every platform is provably correct before any one platform ships. Pure verification — no platform-code rewrites are in scope here; rewrites that follow from verification failure are tracked separately.

> Epic: 75
> Status: Planning (Step 2 Requirements)
> Date: 2026-05-21
> Architecture impact: none — E75 is verification work. The platform architecture docs (`agent-macos-platform-architecture.md`, `agent-linux-platform-architecture.md`, `agent-windows-platform-architecture.md`, `agent-ne-fail-open-architecture.md`, `agent-forwarder-architecture.md`, `agent-enrollment-architecture.md`, `agent-keystore-architecture.md`, `agent-autoupdater-architecture.md`, `agent-tray-ipc-architecture.md`, `agent-paths-abstraction-architecture.md`, `agent-backpressure-rollup-architecture.md`) are READ-ONLY references during E75. Verification-uncovered bugs that demand architectural revision spawn their own follow-up epic — they are NOT bundled into E75.
> SDD: pending (Step 3 — to be drafted as `docs/developers/specs/e75/e75-s{N}-*.md` for the 20 stories outlined in `roadmap.md §E75`; this epic-level Requirements doc covers all 20).
> OpenAPI: pending — likely only needed if cross-platform arm stories add a CP admin-facing test-trigger / result-upload endpoint (Q1 in §8). If introduced, lives at `docs/users/api/openapi/agent/e75-s{N}-*.yaml`.
> Memory anchors: `project_e75_three_platform_verification` (to be created on epic kickoff); `project_ne_cursor_streamchat_verification` (the existing deferred-verification entry that this epic operationalises across all platforms); `project_cursor_capture_investigation` (the prior investigation that motivated the macOS arm).
> Blocked by: none.
> Blocks: any future epic that depends on confidence that all three Agent platforms work end-to-end (in particular, prod rollout of the agent to non-developer Macs / Linux fleets / Windows fleets at scale).

---

## 1. Background

### 1.1 What "development-complete, verification-incomplete" means

The three Agent platforms each have working code:

- **macOS** — `packages/agent/internal/platform/darwin/` + the Swift NE provider at `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/`. NE forwards inspect-mode flows to the Go daemon's `127.0.0.1:9443` bridge; `tlsbump` does MITM on the daemon side.
- **Linux** — `packages/agent/internal/platform/linux/`. iptables NAT REDIRECT to a daemon-owned listener; `tlsbump` does MITM. systemd unit; libsecret or encrypted-file fallback for keystore.
- **Windows** — `packages/agent/internal/platform/windows/`. WinDivert kernel-mode capture; Service Control Manager-managed service; named-pipe IPC to a tray UI.

The shared forwarder (`packages/agent/internal/`) runs uniformly across all three. Each platform has its own intercept mechanism, install/uninstall flow, IPC contract, and OS-specific safety constraints.

What's missing: **no platform has a comprehensive synthetic test suite exercising the full install → intercept → hook → audit → uninstall flow** on a clean OS image. Today's verification is ad-hoc — a developer machine, a manual install, a curl-the-AI-provider check. That's not a release gate for a fleet rollout. The categories of bugs E75 catches:

- **macOS NE**: fail-open invariants under daemon stalls. The 2026-05-15 incident required manual `launchctl unload` + plist deletion to recover (`agent-ne-fail-open-architecture.md` §1). A regression of that class would be invisible until a customer hit it.
- **Linux pf / iptables**: rule installation correctness across distros (Ubuntu / RHEL / Arch). systemd unit start-order vs nftables conflicts. Orphan rules on uninstall.
- **Windows WinDivert**: kernel-mode capture stability under sustained high-load IDE traffic. SCM state machine (Stopped → StartPending → Running → StopPending → Stopped) under concurrent installs / restarts / upgrades. Named-pipe IPC race conditions.

The shared invariants (mTLS enrollment, auto-updater signature verification, keystore migration, kill-switch reachability, tray IPC parity) cross all three.

### 1.2 The coverage requirement (binding)

**Every platform must pass. No exceptions.** A platform that fails any verification story is a release blocker until fixed. Partial-platform coverage is NOT acceptable at close-out review. Either all three platforms ship together (i.e., E75 marked ✅ Shipped) or the epic stays 🟢 Planned. This is the user-stated constraint in `roadmap.md §E75` line 365 and is binding for E75 close-out.

The implication: parallelisation across platforms is encouraged (3 VM environments running concurrently); merging an "almost shipped except for one platform" state is forbidden.

### 1.3 Relationship to E74

E74 (`e74-macos-pf-intercept.md`) replaces the macOS NE intercept with pf. E75 verifies the **current** NE-based macOS code. The coupling:

- **E75 macOS arm (stories S1 – S5)** runs against the NE codebase as it exists today. Verification proceeds in parallel with E74 planning.
- **Once E74 lands**, the macOS arm stories that touch the intercept path (S3 IDE intercept, S4 consumer-web intercept, S5 audit drain — by virtue of the audit pipeline depending on intercept output) re-run on the pf path. The same acceptance criteria apply; the test fixtures don't change.
- **S1 (install/uninstall) and S2 (fail-open synthetic stress test)** mostly re-run too, but with NE-specific assertions softened to platform-agnostic ones (e.g., "Allow System Software" gate no longer required if E74 ships pf-only). E74's FR-7 acceptance gate is the macOS-specific gap-class closure; E75's macOS arm is broader end-to-end coverage that just happens to fall on the pf path post-E74.
- **E75 does NOT block on E74**. If E74 slips, E75 ships against NE-current. If E74 ships first, E75 ships against pf and the NE re-verification work disappears.

### 1.4 Locked decisions (DO NOT RE-DEBATE)

These four decisions were settled in the planning session (2026-05-21) for E74 and apply to E75 by transitivity (because E75 verifies what E74 ships). They are reproduced verbatim from `e74-macos-pf-intercept.md §1.4` so SDD reviewers do not re-litigate. SDD MAY refine sub-decisions inside each; SDD MAY NOT reverse the top-level direction.

| ID | Decision | Rationale |
|---|---|---|
| D1 | **Architectural direction: pf path on top of existing `tlsbump`** (E74). Use a pf anchor + `rdr` rules to redirect target outbound flows directly to a daemon listener that hands off to `BumpFlow`. **Deferred to SDD**: full replacement of NE vs hybrid (NE for default app traffic + pf only for the five gap-class flows). | Reuses the proven MITM stack; no second copy of cert minting / pinning / streaming policy. Hybrid-vs-full call needs prototype telemetry before being settled. |
| D2 | **Zero new Apple submissions / certifications** (E74). Apple Developer Program (`39U3X3FFVK`), Developer ID Application + Installer certs, and the `xcrun notarytool` notarization workflow continue unchanged. The existing Network Extensions Entitlement Request approval becomes unused but **is not revoked**. The `com.apple.developer.networking.networkextension` entitlement is **removed from `.entitlements`** in the pf-only build target. The "Allow System Software" gate at first launch is **removed**. | The NE entitlement is hard to get; we keep the approval inert as insurance. Endpoint Security (D3) is **harder** than NE — refuse to pay that cost. Removing the user gate is net friction reduction. |
| D3 | **Per-process attribution: `libproc` / `proc_pidpath`, NOT Endpoint Security** (E74 macOS). Default for the pf path; SDD MAY revisit. | Endpoint Security would close the helper-process attribution gap but requires `com.apple.developer.endpoint-security.client`, harder to obtain than the NE entitlement. Residual attribution gap documented explicitly. |
| D4 | **Install-flow friction reduction is a Requirements-level NFR** (E74). pf path removes the user-facing "Allow System Software" approval gate. | Surface in product-side review and release notes, not as a buried implementation detail. |

E75 inherits D1-D4 in the sense that the macOS arm's pass criteria are designed to work both pre-E74 (NE current) and post-E74 (pf). E75 does NOT add new locked decisions of its own — verification doesn't introduce architectural commitments.

---

## 2. Functional Requirements

### FR-1: Per-platform synthetic test framework

| ID | Requirement | Priority |
|---|---|---|
| FR-1.1 | A new test harness lives at `tests/agent/` (extension of existing `tests/` layout that today carries `tests/scripts/smoke-gateway.py`, `tests/scenarios/`, etc.). The harness is composed of three platform-specific drivers + one cross-platform driver, all sharing one common test-orchestration core. | Must |
| FR-1.2 | Each platform driver spawns a clean VM (multipass / Vagrant / equivalent — final choice per Q3 in §8), runs the install → intercept → hook → audit → uninstall sequence as a stateful test, and reports per-story pass/fail with structured output (machine-parseable plus markdown). | Must |
| FR-1.3 | The harness reads test target config from `tests/.env.<target>` per CLAUDE.md "Test/skill env files" binding. `target=verify-macos`, `target=verify-linux`, `target=verify-windows` are the three new targets; each carries VM image name, snapshot baseline, agent binary URL, Hub mTLS test creds (test creds only — never prod). | Must |
| FR-1.4 | The harness fails closed on misconfigured targets: missing VM image, missing creds, prod-hostname VK / Hub URL all abort with a clear error per CLAUDE.md "scenario test env isolation" binding (`feedback_scenario_test_env_isolation`). | Must |
| FR-1.5 | Each story produces a structured assertion log. For acceptance, story passes only when every assertion in its acceptance criteria reports `ok`. "Assertion skipped because precondition failed" is NOT a pass — it surfaces as `incomplete`. | Must |
| FR-1.6 | The harness produces a unified per-run markdown report at `/tmp/test-agent-verification-<target>-<UTC-ts>.md` mirroring existing skills (`/test-cursor-adapter`, `/test-compliance-proxy`, `/smoke-gateway`). Per-platform reports plus an aggregate cross-platform summary. | Must |
| FR-1.7 | Harness unit-test coverage per CLAUDE.md binding (≥95% for any new Go/Python packages the harness adds). The synthetic-flow tests themselves are integration tests not subject to the per-package gate; they live behind a build tag / harness opt-in per the existing `tests/scenarios/` discipline. | Must |
| FR-1.8 | A new skill `/test-agent-verification` exposes the harness for one-command invocation per platform. Args: `--target=macos | linux | windows | all`. Mirrors existing skill patterns. | Must |
| FR-1.9 | The harness operates on **synthetic data only** — Agent UI test users, dedicated test VKs, dedicated test domains. Per CLAUDE.md `feedback_tests_only_own_data` (binding), the harness MUST NEVER DELETE/UPDATE rows it didn't create. Seed-prefix discipline (`e75-verify-`) applies to all DB writes the harness or the agent under test makes. | Must |

### FR-2: macOS arm — 5 stories (S1 – S5)

These map 1:1 to the macOS-arm stories outlined in `roadmap.md §E75` lines 389-396. Acceptance criteria here drive the SDD's per-story task breakdown.

| ID | Requirement | Priority |
|---|---|---|
| FR-2.1 | **S1 — macOS install / uninstall flow verification**. On a clean macOS VM (Apple Silicon + Intel both required — see NFR-3): (a) `build-agent` skill produces a signed + notarized `.pkg`; (b) running the `.pkg` installs the daemon LaunchDaemon, the system extension (only if pre-E74 / `interceptMode="ne"`), the tray UI app bundle; (c) on first launch the system-extension approval prompt appears (pre-E74) and is satisfied via UI automation OR by `systemextensionsctl developer on` if test mode allows; (d) the daemon enrols against the test Hub via mTLS; (e) Activity Monitor shows `NexusAgentExtension` (pre-E74) or pf rules present via `pfctl -a nexus-agent/transparent -s rules` (post-E74); (f) the agent UI tray icon is reachable; (g) the uninstaller path (`uninstall` subcommand or the `.pkg`'s removal script) leaves no orphan `plist`, no orphan kext, no helper, no rogue pf anchor — verified by an explicit deny-list scan. Per-platform unit tests not applicable (this is integration); harness assertions cover. | Must |
| FR-2.2 | **S2 — NE fail-open synthetic stress test**. Pre-E74: (a) DNS / DHCP / mDNS / NTP / APNS flows are validated to pass through unmodified during the test (Rule 5 in `agent-ne-fail-open-architecture.md` §2.5); (b) the harness simulates daemon stall by `kill -STOP <daemon-pid>` and verifies inspect flows time out to passthrough at 2 s (`requestDecision`) and 500 ms (`peekSNIThenRelay`); (c) the harness kills the daemon (`kill -KILL`) and verifies pf rules / NE state recovers automatically — for NE this means the system extension stops claiming flows; for pf it means the anchor is removed (E74 FR-4.8); (d) QUIC fallback verified by spawning Chrome (or a Chrome-like uTLS test client) and observing UDP→TCP fallback for known bundles; (e) emergency recovery procedure `launchctl unload` / `pfctl -F` is documented in the harness report and runs cleanly. | Must |
| FR-2.3 | **S3 — macOS IDE intercept**. `/test-cursor-adapter` runs against the macOS-installed agent end-to-end (not against the prod compliance proxy as the skill currently does — the harness redirects the skill's HTTPS_PROXY to the local macOS agent's intercept). Acceptance: (a) Cursor IDE traffic to `api2.cursor.sh` is bumped successfully (MITM happens; tlsbump stamps `BUMP_SUCCESS`); (b) the protobuf payload is decoded; (c) `traffic_event_normalized` row carries `kind=ai-chat`, `detectedSpec=cursor`, `model=claude-sonnet-4-6`, `confidence≈0.95`, three decoded user/assistant/user messages; (d) `traffic_event.source='agent'`. **Post-E74**: same arm runs against pf-bumped traffic; same assertions apply. | Must |
| FR-2.4 | **S4 — macOS consumer-web intercept**. Synthetic browser flows to `chatgpt.com`, `claude.ai`, `gemini.google.com` via Chrome / Safari on the macOS VM. Pre-E74: macOS NE path is **metadata-only** for consumer web (TLS-bump on browser traffic is technically supported by the NE bridge but disabled-by-default per the existing rule set; per `roadmap.md §E75` S4 the acceptance is "verify metadata capture is correct"). Acceptance pre-E74: `traffic_event` rows with `source='agent'`, populated `target_host`, `target_ip`, byte counts, `bump_status` reflecting the policy decision; **no content extraction expected** on the pre-E74 NE-default-off path. Post-E74: full content extraction expected (pf can MITM browser flows directly, no NE-bridge gating); the harness flips its assertion mode based on the configured `interceptMode`. | Must |
| FR-2.5 | **S5 — macOS audit drain**. The harness emits 100 traffic events through the agent's intercept, then validates Hub upload: (a) primary mTLS WebSocket upload succeeds; (b) HTTP fallback exercised by `tc qdisc add dev <iface> root netem loss 100%` simulating WS unreachable for 30 s, then restoring — agent retries via HTTP; (c) SQLCipher queue is empty after drain (no leftover rows); (d) reconnect after network blip — `tc qdisc del` + verify queue drains in <60 s; (e) audit events arrive at Hub in their submitted order (within per-flow ordering — global ordering across flows is not required). | Must |

### FR-3: Linux arm — 5 stories (S6 – S10)

Maps to `roadmap.md §E75` lines 398-403.

| ID | Requirement | Priority |
|---|---|---|
| FR-3.1 | **S6 — Linux install / uninstall on Ubuntu 22.04 LTS + RHEL 9 + Arch**. On each distro's fresh VM: (a) `.deb` (Ubuntu) / `.rpm` (RHEL) / `.tar.gz + install.sh` (Arch) installs the daemon + systemd unit per `agent-linux-platform-architecture.md` §3; (b) systemd unit reaches `active (running)` within 10 s; (c) iptables rules placed correctly: `iptables -t nat -L OUTPUT -nv` shows the expected REDIRECT rule with `! --uid-owner nexus-agent` exclusion per §8; (d) the daemon enrols against the test Hub via mTLS; (e) uninstall removes the systemd unit (`systemctl status nexus-agent` returns `not-found`), removes all iptables rules (zero matching `iptables -t nat -L OUTPUT -nv | grep nexus`), removes the SQLCipher DB and cert files. | Must |
| FR-3.2 | **S7 — Linux pf / iptables intercept correctness**. Per distro: (a) tlsbump succeeds against `api.openai.com`, `api.anthropic.com`, `generativelanguage.googleapis.com` with admin-trusted cert pre-installed via the agent's enrollment flow; (b) content-aware hooks (PII detector, keyword filter) run on chat + embedding traffic; (c) `traffic_event` rows with `source='agent'`, populated content fields, correct `endpoint_type` for chat vs embedding (per E62); (d) the SNI-peek fallback handles non-TLS port traffic via opaque relay (smoke a non-TLS port flow — e.g. github.com:22 ssh — verify no crash, no rule mismatch, no audit row stamped). | Must |
| FR-3.3 | **S8 — Linux fail-open behavior**. (a) kill the daemon with `kill -KILL`; observed: iptables NAT rules removed within 5 s by an on-exit hook OR by an external watchdog; (b) DNS resolution continues working throughout (`dig @8.8.8.8 example.com` succeeds during and after the kill); (c) no flows stuck — `ss -t state established | wc -l` returns to baseline within 10 s; (d) on systemd `Restart=on-failure` re-triggering the daemon, rules are re-installed cleanly. | Must |
| FR-3.4 | **S9 — Linux audit drain**. Same shape as FR-2.5 (macOS) but on Linux: mTLS WS primary, HTTP fallback, retry-with-backoff, SQLCipher local queue rotation under sustained load. Specifically validate that under sustained 100 req/s for 60 s the queue does not exceed `agent_settings.audit.queueMaxRows` (per `agent-backpressure-rollup-architecture.md`) and does not lose events. | Must |
| FR-3.5 | **S10 — Linux uninstall completeness**. After uninstall: (a) `iptables -t nat -S | grep -i nexus` returns empty; (b) `systemctl list-unit-files | grep nexus` returns empty; (c) the agent's data directory (`/var/lib/nexus-agent/` or platform default per `agent-paths-abstraction-architecture.md`) is removed; (d) the cert + keystore files removed; (e) the dedicated `nexus-agent` OS user account removed (or, if `userdel` is platform policy, the user is at least disabled and home directory wiped). | Must |

### FR-4: Windows arm — 5 stories (S11 – S15)

Maps to `roadmap.md §E75` lines 405-411.

| ID | Requirement | Priority |
|---|---|---|
| FR-4.1 | **S11 — Windows install / uninstall on Windows 11 22H2+**. (a) MSI installer installs the daemon as a Windows Service, the WinDivert kernel driver, the tray UI; (b) WinDivert driver activates — `sc query nexus-agent-divert` returns `STATE: RUNNING`; (c) the daemon Service reaches `STATE: RUNNING` per the Service Control Manager state machine (Stopped → StartPending → Running) within 10 s; (d) the daemon enrols against the test Hub via mTLS; (e) tray UI launches; (f) uninstall reverses cleanly: Service deregistered (`sc query` returns "service does not exist"), WinDivert driver unloaded (`sc query nexus-agent-divert` returns "not exist"), no orphan registry keys under `HKLM\SYSTEM\CurrentControlSet\Services\nexus-agent*`, no orphan files in `%ProgramData%\nexus-agent\`. | Must |
| FR-4.2 | **S12 — Windows WinDivert intercept correctness**. (a) sustained high-load IDE traffic (Cursor + Cline + Continue running for 10 minutes, ~50 req/s aggregate) is captured stably without driver panics, kernel hangs, or BSODs; (b) TLS bump succeeds on the captured flows via the daemon's tlsbump pipeline; (c) content-aware hooks run; (d) `traffic_event` rows with `source='agent'` produced; (e) WinDivert handle count + memory usage during the run stays bounded (no leak). | Must |
| FR-4.3 | **S13 — Windows fail-open behavior**. (a) driver unload safety — `sc stop nexus-agent-divert` while flows are active does not BSOD; in-flight flows complete or fail cleanly with the user's app seeing a TCP-level error (NOT a frozen network); (b) under capture failure (e.g. force-unload via PowerShell mid-flow), the daemon's pf-equivalent (WinDivert ruleset) is auto-removed; (c) network connectivity restored within 10 s of daemon death; (d) the Service's `RecoveryAction` per SCM is set to `Restart` and triggers cleanly within 30 s. | Must |
| FR-4.4 | **S14 — Windows named-pipe IPC stability**. Race-condition stress: (a) overlap install + start + tray-UI launch in three concurrent threads — verify no half-state where the daemon is running but the named pipe is not yet bound; (b) restart the daemon while the tray UI is connected — tray UI reconnects within 5 s; (c) upgrade scenario — install a newer pkg over a running daemon; verify the daemon stops, upgrades, restarts, tray reconnects. | Must |
| FR-4.5 | **S15 — Windows audit drain + uninstall completeness**. Combined with FR-4.1's uninstall assertions: same audit-drain shape as macOS / Linux (FR-2.5, FR-3.4) plus cleanup. Specifically: (a) SQLCipher DB cleanup; (b) cert cleanup from Windows Cert Store (`certutil -store -user MY`); (c) SCM service deregistration; (d) no orphan registry keys; (e) no orphan ProgramData files. | Must |

### FR-5: Cross-platform arm — 5 stories (S16 – S20)

Maps to `roadmap.md §E75` lines 413-419. These arms run after at least one platform's per-platform arm is green, then run on each ready platform.

| ID | Requirement | Priority |
|---|---|---|
| FR-5.1 | **S16 — mTLS enrollment flow** per `agent-enrollment-architecture.md`. (a) CSR generation + cert issuance from the Hub on first enrol; (b) cert rotation — rotate the cert mid-session and verify the next mTLS handshake uses the new cert without manual intervention; (c) revocation — Hub revokes the cert; agent observes a 403 on the next call and re-enrols cleanly; (d) re-enrollment after cert expiry — fast-forward the cert clock OR issue a short-TTL cert (per `Q4` in §8), agent re-enrols within the configured window. All three platforms. | Must |
| FR-5.2 | **S17 — Auto-updater** per `agent-autoupdater-architecture.md`. (a) Ed25519 signature verification on the release manifest — happy path passes; (b) rollback on bad signature — agent refuses to install a tampered manifest; (c) staged rollout via release channels — agent on `channel=stable` does NOT pick up `channel=canary` releases. All three platforms. | Must |
| FR-5.3 | **S18 — Keystore migration** per `agent-keystore-architecture.md`. (a) Migrate across SQLCipher DB-key rotation; data preservation verified (audit queue, cert, enrollment state all readable post-migration); (b) Failure-mode test: simulate lost DB key — agent MUST wipe local queue (no leakage), report the incident audit-event, and re-enrol from scratch. (c) Cross-platform parity: macOS Keychain + Linux libsecret + Windows DPAPI fallback paths all exercised. | Must |
| FR-5.4 | **S19 — Kill-switch reachability** per `feedback_thing_config_pull_model` + the existing E48 emergency-passthrough work. (a) Hub-pushed shadow → local kill within ≤30 s on all three platforms (the latency target in `roadmap.md §E75` S19); (b) emergency-disable UI button (admin-side) triggers the same path; (c) verify that hooks are bypassed on the next intercepted flow within the latency window; (d) verify the recovery — remove the kill flag, observe hooks resume on the next flow. | Must |
| FR-5.5 | **S20 — Tray IPC protocol cross-platform parity** per `agent-tray-ipc-architecture.md`. Same wire shape across all three platforms — macOS Unix socket, Windows named pipe, Linux Unix socket. Test fixtures capture the platform-specific differences (path format, ACL handling, ownership) explicitly. Round-trip a representative set of IPC messages (status query, config push notification, manual flush-audit-queue command) on each platform and verify identical observable behaviour. | Must |

### FR-6: Reporting + close-out gate

| ID | Requirement | Priority |
|---|---|---|
| FR-6.1 | The harness's aggregate report (FR-1.6) contains a top-level **Coverage matrix table**: rows = stories S1 – S20, columns = (macOS, Linux, Windows where applicable, cross-platform). Each cell is one of {`pass`, `fail`, `incomplete`, `not-applicable`}. | Must |
| FR-6.2 | E75 closes only when the Coverage matrix has zero `fail` and zero `incomplete` cells. `not-applicable` cells are recorded by SDD upfront (e.g., S1 has separate macOS / Linux / Windows specialisations, so the row has three populated cells, never a single one). Per the binding "every platform must pass" rule (§1.2). | Must |
| FR-6.3 | A platform with `fail` or `incomplete` cells blocks the entire epic. The remediation path (fix the underlying bug, re-run the affected story) is the only forward path; closing with a partial green is NOT permitted. | Must |
| FR-6.4 | The harness produces a **rerun stability metric** — same story, same platform, run 3 times back-to-back, all 3 runs must `pass` (or all 3 `fail` consistently, which is also signal). Per-run flake rate >0% is itself a `fail`. The harness records the 3 runs in the report. | Must |
| FR-6.5 | A baseline run (against the codebase at the time of E75 SDD start) is recorded so that "this story was already broken before E75 work" is distinguishable from "E75 work introduced the bug". The baseline is **read-only**; only the post-E75 baseline counts for the gate. | Should |
| FR-6.6 | The harness's report is committed alongside the close-out PR for E75 (as a markdown artifact under `docs/developers/specs/e75/reports/`). This preserves the close-out evidence in git history. | Should |

### FR-7: Doc lockstep

| ID | Requirement | Priority |
|---|---|---|
| FR-7.1 | E75 is **read-only** with respect to the platform architecture docs. The harness produces verification artefacts that document **as-built behaviour**; architectural docs are NOT modified by E75. If a verification story uncovers a bug that requires an architectural change, the architectural change is its own follow-up epic — not bundled into E75. This keeps E75 a pure verification gate. | Must |
| FR-7.2 | The skill source (`.claude/skills/test-agent-verification/`) is created in the same PR as the harness; CLAUDE.md "code/doc lockstep" applies via the existing `scripts/check-doc-lockstep.mjs` config. | Must |
| FR-7.3 | The E86 doc-lockstep entry `e2e-coverage-matrix` (at `docs/developers/specs/e86-e2e-coverage-matrix.md` in the E86 worktree at the time of E75's SDD drafting) gains rows for each of stories S1 – S20, ensuring future feature work cannot silently bypass these gates. | Must |
| FR-7.4 | The agent platform docs gain a single new cross-reference each pointing to `docs/developers/specs/e75-three-platform-agent-verification.md` as the verification authority. No content changes inside those docs. | Should |
| FR-7.5 | Memory anchor `project_e75_three_platform_verification` is created during epic kickoff per CLAUDE.md memory-anchor convention. | Should |

---

## 3. Non-Functional Requirements

| ID | Requirement | Notes |
|---|---|---|
| NFR-1 | **Total verification runtime on a developer machine MUST be ≤90 minutes** for a single platform's full arm (S-arm + S16-S20 share). The 3-platform total may run in parallel (3 VMs) for ≤120 minutes wall-clock; serial fallback is acceptable but slow. | The harness is a release gate, not a per-PR gate. Long runtime is acceptable; flake rate is not. |
| NFR-2 | **Zero-flake target** — over 10 consecutive harness runs against a known-good build, the pass rate MUST be 100%. Per-story flake rate >0% surfaces in FR-6.4 and is itself a verification failure. | Flake-free is the binding rule. A flaky test is worse than no test — it teaches operators to ignore failures. |
| NFR-3 | **macOS coverage spans both Apple Silicon and Intel architectures**. Per-arch VM image, separate harness run per arch; aggregate report distinguishes the two. The acceptance gate requires both to pass. | Both arches are in the install base. |
| NFR-4 | **Linux coverage spans Ubuntu 22.04 LTS + RHEL 9 + Arch** (3 distros). systemd vs OpenRC variation is out of scope; all 3 in-scope distros ship systemd. | The 3 distros cover the common enterprise-Linux variety. |
| NFR-5 | **Windows coverage: Windows 11 22H2 + 23H2 + 24H2**. Earlier versions (10 22H2 LTSC, 11 21H2) are out of scope for E75 release gating; they are tracked as a separate follow-up if customer demand surfaces. | Per `roadmap.md §E75` S11 line 407. |
| NFR-6 | **VM images are reproducible** — the harness's VM image construction (multipass image / Vagrant box / equivalent) is in version control under `tests/agent/vm/`. Image rebuilds match SHAs across runs. | Without reproducible images, the harness's results are not comparable across runs. |
| NFR-7 | **No prod data, no prod creds, no prod hostnames** — per CLAUDE.md `feedback_scenario_test_env_isolation`. The harness's hostname allowlist contains only localhost + the dedicated test Hub URL (read from `tests/.env.verify-*`). Prod hostnames in the env file abort with a clear error. | Binding rule; protects against a stray prod write incident (memory `feedback_tests_only_own_data`). |
| NFR-8 | **Seed-prefix discipline** — all DB writes (audit events, traffic events, normalized payloads) the harness or the agent under test produces carry a `e75-verify-` prefix in any identifier-bearing column. The harness MUST NEVER read or modify rows that do not carry this prefix per `feedback_tests_only_own_data`. | Binding rule. |
| NFR-9 | **Harness logs are self-contained per run** — `/tmp/test-agent-verification-<target>-<UTC-ts>/` carries the markdown report, the per-story structured JSON output, VM serial console captures (where applicable), and Hub-side log excerpts. This is the single artefact required to debug a failure. | One-shot debug, no out-of-band hunting. |
| NFR-10 | **English-only doc + commit messages** per CLAUDE.md binding. | Standard. |
| NFR-11 | **Code/doc lockstep** (CLAUDE.md binding) — FR-7.2 enumerates the doc + skill artifacts that land in the same PR as the harness code. | Standard. |
| NFR-12 | **Per-package unit-test coverage ≥95%** for any new Go / Python packages the harness adds (CLAUDE.md binding). Integration synthetic-flow tests behind build tags / opt-in are exempt from per-package gating but their parent harness packages are not. | Standard. |
| NFR-13 | **No regression in existing scenario / smoke / scenario-harness pass rates** during the SDD's code-phase sessions — the agent harness is additive. | Standard. |

---

## 4. User Roles & Personas

- **Agent platform engineer** — primary persona. Owns one or more of macOS / Linux / Windows platform code. Uses the harness as the regression gate before merging changes. Cares: clear pass/fail signal; structured assertion logs to debug failures; per-run artefacts in a known location.
- **Release engineer** — runs `/test-agent-verification --target=all` as the pre-release gate. Reviews the Coverage matrix (FR-6.1) and approves the release only on all-green. Cares: zero-flake target (NFR-2); deterministic VM images (NFR-6); clear close-out evidence (FR-6.6).
- **Incident responder** — when a customer reports an Agent issue in prod, consults the most recent harness report for the affected platform to triage whether the failure mode is covered by an existing story. Cares: report's structured JSON (NFR-9); per-story assertion logs.
- **Support engineer** — uses the harness's documented uninstall completeness assertions (FR-2.5, FR-3.5, FR-4.5) as the canonical "clean uninstall" definition when guiding customers. Cares: explicit deny-list scans; reproducible cleanup.
- **Compliance reviewer (internal)** — audits the harness's audit-drain stories (FR-2.5 macOS, FR-3.4 Linux, FR-4.5 Windows) to verify that the agent does NOT leak data on uninstall, does NOT lose data under load, does NOT bypass kill-switch. Cares: SQLCipher DB cleanup; kill-switch latency (S19); revocation behaviour (S16).
- **Future agent-architecture epic implementer** — relies on E75 having shipped before launching new agent-level features (e.g., a future E63 audio adapter that requires agent-side audio normalisation). Cares that: the three-platform invariant is enforceable; the harness is reusable to verify new features without rewriting it.

---

## 5. Constraints & Assumptions

### Constraints

- C-1 (binding): **All three platforms must pass** for E75 to close (per §1.2). No partial-platform close-out. No "we'll fix Windows later" path.
- C-2 (binding, CLAUDE.md): Test/skill env files live under `tests/.env.<target>` per the env-files binding. Targets: `verify-macos`, `verify-linux`, `verify-windows`.
- C-3 (binding, CLAUDE.md): Scenario test env isolation — hostname allowlist localhost only + the dedicated test Hub URL. Fail-closed on any prod hostname.
- C-4 (binding, CLAUDE.md): Tests must only touch own data. Seed-prefix `e75-verify-` discipline.
- C-5 (binding, CLAUDE.md): Per-package unit-test coverage ≥95% (NFR-12).
- C-6 (binding, CLAUDE.md): English-only repository text.
- C-7 (binding, CLAUDE.md): macOS Agent builds go through `Skill('build-agent')`. The macOS arm's S1 explicitly invokes this skill — no improvised codesign / pkgbuild / notarytool.
- C-8 (binding, CLAUDE.md): NE proxy fail-open invariants on macOS. The macOS S2 explicitly verifies them.
- C-9 (binding, CLAUDE.md): Pre-edit reading per the 3-doc rule — the SDD's per-story sessions read the matching platform doc + workflow conventions + relevant feature doc before code-phase work.
- C-10 (binding, CLAUDE.md): Code/doc lockstep — FR-7.2 enumerates.
- C-11 (binding, CLAUDE.md): Pre-GA "no defer" policy. Verification-uncovered bugs that demand architectural revision are tracked as follow-up epics; no `@deprecated` markers added inside E75 PRs.
- C-12 (binding): Verification is **read-only** with respect to platform architecture docs (FR-7.1). E75 does not own architectural rewrites.
- C-13 (binding): Macros — the harness MUST be invocable in CI eventually. SDD's later stories MAY add a GitHub Actions job that runs `--target=linux` on each PR; full 3-platform sweep runs nightly on a dedicated runner. CI integration is "should" priority, not "must", for E75 close-out.

### Assumptions

- A-1: VM provisioning toolchain (multipass / Vagrant / equivalent) is available on the developer machines that run the harness. Final tool choice per Q3 in §8.
- A-2: Apple Silicon + Intel macOS VMs are both achievable on the testing fleet (NFR-3). If only one arch is achievable in CI, the other runs on a developer machine on a regular cadence.
- A-3: Windows 11 VMs (NFR-5) license cost is acceptable / handled via existing MSDN access. If not, the SDD records the constraint and the harness's Windows arm runs on a manually-maintained physical / virtual fleet rather than ephemeral cloud VMs.
- A-4: The test Hub URL (`tests/.env.verify-*`) is a dedicated instance — separate from local dev Hub and from prod. Provisioning of this test Hub is a prerequisite captured in the SDD's first story.
- A-5: Existing skills (`/test-cursor-adapter`, `/test-compliance-proxy`, `/build-agent`) can be invoked from inside the harness without modification. (Verified by reading the skill descriptions; if any skill requires non-trivial extension, the SDD records that as its own task.)
- A-6: Linux distro choices (Ubuntu 22.04 LTS + RHEL 9 + Arch — NFR-4) cover the common enterprise + community spread. Debian 12 + Ubuntu 24.04 are added as follow-ups if customer demand surfaces.
- A-7: The Agent's existing telemetry (`audit_event`, `traffic_event`, Prometheus metrics) is sufficient for harness assertions. No new agent-side telemetry is required for E75. (Verified by reading harness assertion specs against agent telemetry surface; if a story requires new telemetry, the SDD records it as a dependency.)
- A-8: WinDivert kernel driver licensing / signing is already handled — the harness reuses the signing infra; no new Windows-side signing work.
- A-9: The audit-drain timing target (≤60 s queue drain after network blip, FR-2.5 / FR-3.4 / FR-4.5) is achievable with current agent code. If not, the gap is a verification failure and the underlying bug is filed against the agent-forwarder backpressure code per `agent-backpressure-rollup-architecture.md`.

---

## 6. Glossary

- **NE / NETransparentProxyProvider** — Apple's NetworkExtension framework's user-space transparent proxy. The current macOS intercept layer; pre-E74 only. Defined fully in `e74-macos-pf-intercept.md §6` Glossary.
- **pf** — BSD Packet Filter, the kernel-level firewall / NAT facility shipped with macOS. Post-E74 macOS intercept path. See `e74-macos-pf-intercept.md §6` Glossary.
- **tlsbump** — the shared MITM pipeline at `packages/shared/transport/tlsbump/`. Terminates client TLS, runs hooks on plaintext, re-encrypts to upstream via uTLS Chrome fingerprint. Used unchanged by all three platforms.
- **WinDivert** — the Windows kernel-mode user-space packet capture / divert framework. The Windows-platform intercept layer.
- **iptables NAT REDIRECT** — Linux netfilter NAT mode used by the Linux agent to steer outbound traffic to a daemon-owned localhost port. Documented in `agent-linux-platform-architecture.md` §8.
- **mTLS enrollment** — the cert-issuance flow at first-boot between the Agent and the Hub. Documented in `agent-enrollment-architecture.md`.
- **SQLCipher queue** — the Agent's encrypted local SQLite audit-queue, drained to the Hub via mTLS WS / HTTP fallback. Documented in `agent-backpressure-rollup-architecture.md` + `agent-keystore-architecture.md`.
- **gap class** — one of the five inherent NE-architecture coverage / ergonomics gaps closed by pf (E74 §6 Glossary): opt-in surface, QUIC blind spot, fail-open visibility loss, per-hop latency, process attribution drift.
- **fail-open** — invariant from `agent-ne-fail-open-architecture.md`: an intercept malfunction MUST degrade to passthrough, never to a stuck or blocked flow. Network connectivity > inspection coverage. E75 macOS S2 verifies this explicitly.
- **kill-switch** — the Hub-pushed "disable all hooks" shadow flag. Tested by S19 (FR-5.4).
- **release channel** — `stable | canary | …`, used by the auto-updater. Tested by S17 (FR-5.2).
- **Coverage matrix** — the close-out report table mapping each of S1 – S20 to (macOS, Linux, Windows, cross-platform) cells with per-cell pass/fail status (FR-6.1).
- **Rerun stability metric** — the 3-back-to-back pass requirement for every story (FR-6.4); guards against flaky tests counting as a pass.
- **Seed prefix** — the `e75-verify-` discipline for all DB writes the harness or agent-under-test makes (NFR-8); enforces the "tests only touch own data" rule.
- **Apple Silicon / Intel** — the two macOS architectures both required to pass per NFR-3. M-series ARM64 + x86_64 respectively.

---

## 7. MoSCoW Priority Summary

**Must (in scope for E75):**

- All 20 stories — macOS S1-S5 (FR-2.1 – FR-2.5), Linux S6-S10 (FR-3.1 – FR-3.5), Windows S11-S15 (FR-4.1 – FR-4.5), Cross-platform S16-S20 (FR-5.1 – FR-5.5).
- Harness shape (FR-1.1 – FR-1.8): platform drivers, structured assertion logs, env-file isolation, fail-closed misconfiguration, markdown reports, harness package coverage ≥95%.
- Harness operates on synthetic data only with seed-prefix discipline (FR-1.9 + NFR-8).
- Coverage matrix close-out gate (FR-6.1 – FR-6.4): all-platform pass binding, zero flake.
- Doc lockstep on harness + skill artefacts (FR-7.2 – FR-7.3); read-only on architecture docs (FR-7.1).

**Should (nice to have, in scope if time permits):**

- Pre-E75 baseline run recorded for distinguishing pre-existing breakage (FR-6.5).
- Close-out report committed under `docs/developers/specs/e75/reports/` (FR-6.6).
- Cross-references added in platform docs to E75 verification authority (FR-7.4).
- Memory anchor `project_e75_three_platform_verification` (FR-7.5).
- CI integration of `--target=linux` (C-13).

**Could (deferred to follow-up work):**

- Apple Silicon + Intel parity beyond manual / nightly cadence — i.e., per-PR CI coverage on both arches (NFR-3 limits this to "both must pass at gate time", not "both must run on every PR").
- Additional Linux distros beyond Ubuntu / RHEL / Arch (A-6).
- Older Windows variants (Windows 10 22H2 LTSC, Windows 11 21H2) — out of scope for E75 release gating (NFR-5).
- BSD / FreeBSD agent verification (no agent BSD code exists today).
- Performance benchmarking beyond per-story latency assertions (FR-2.5 / FR-3.4 / FR-4.5 drain timing only).

**Won't (out of scope for E75):**

- New platform support (BSD, ChromeOS, mobile) — per `roadmap.md §E75 Out of scope` line 425.
- Performance benchmarking under sustained load beyond correctness — per `roadmap.md §E75 Out of scope` line 429.
- Per-IDE / per-web adapter verification (those are E73) — per `roadmap.md §E75 Out of scope` line 430.
- Architectural rewrites of the platform code (E74 is one such rewrite, separately tracked).
- Migration of the existing developer-machine ad-hoc test patterns into the harness — the harness is greenfield; legacy ad-hoc tests stay as-is or are deleted per CLAUDE.md "no backward compatibility" pre-GA policy.

---

## 8. Open questions for SDD

These are unsettled sub-decisions that the SDD must resolve.

- **Q1**: Cross-platform arm — admin-facing endpoint? Some cross-platform stories (S16 cert rotation, S19 kill-switch) MAY need a CP-side test-trigger endpoint so the harness can deterministically initiate the action. SDD decides: dedicated test endpoint (yes / no), and if yes, an OpenAPI spec for `docs/users/api/openapi/agent/e75-s*.yaml`. Default position: **no new endpoint** — drive the actions via existing admin APIs that the test creds can call.
- **Q2**: Coverage matrix format — markdown table only vs JSON + markdown. Markdown is human-readable; JSON is machine-parseable for downstream automation (release gate). SDD picks; recommended: both.
- **Q3**: VM provisioning toolchain — multipass (lightweight, macOS-native) vs Vagrant (cross-platform, mature) vs raw QEMU / Hyper-V. SDD picks per platform. macOS may need a different tool than Linux/Windows. Recommended: multipass for macOS + Linux; Hyper-V (Windows-native) for Windows.
- **Q4**: Cert TTL for re-enrollment test (FR-5.1.d) — fast-forward clock vs issue short-TTL cert. Clock fast-forward is fragile (some Apple frameworks fight system clock changes); short-TTL is cleaner. SDD picks; recommended: short-TTL.
- **Q5**: Baseline-run granularity (FR-6.5) — one baseline per E75 SDD start, or one per platform arm? SDD picks; recommended: one per platform arm + one cross-platform baseline.
- **Q6**: Storage of harness markdown reports under `docs/developers/specs/e75/reports/` (FR-6.6) — per close-out only, or every harness run? Every run is noisy; close-out only is sparse. SDD picks; recommended: every PR-merge run on `feature/E75` (so the close-out PR's report has historical context).
- **Q7**: Windows VM licensing constraint (A-3) — if MSDN access is not available, the harness's Windows arm runs on a manually-maintained fleet. SDD picks the operational model.
- **Q8**: Pre-E74 vs post-E74 macOS arm pass conditions (S3 / S4 / S5) — FR-2.3 / FR-2.4 / FR-2.5 already document the bimodal assertions. SDD confirms the toggle mechanism (read `interceptMode` from the test Hub's `agent_settings` shadow at harness start) and locks the assertion-flip logic.

---

## 9. Acceptance criteria summary

E75 closes when all of the following hold:

1. All 20 stories' acceptance criteria pass on the harness for every applicable platform.
2. The Coverage matrix (FR-6.1) has zero `fail` and zero `incomplete` cells.
3. The rerun stability metric (FR-6.4) shows zero flake across 3 back-to-back runs per story.
4. All harness code lands with ≥95% per-package coverage (NFR-12).
5. The harness's `/test-agent-verification` skill is productised and documented.
6. E86 doc-lockstep entry (FR-7.3) is updated with one row per E75 story.
7. Read-only invariant on architecture docs (FR-7.1) is preserved — no platform-doc rewrites land in any E75 PR.
8. SDD's open questions Q1-Q8 have recorded decisions in the SDD doc.
9. The close-out report (FR-6.6) is committed under `docs/developers/specs/e75/reports/`.
10. The harness operates strictly on synthetic data with seed-prefix discipline (FR-1.9 + NFR-8).
11. The harness fails closed on any misconfigured (prod-hostname / missing creds) target (FR-1.4 + NFR-7).

---

## 10. Out of scope (explicit)

- Architectural rewrites of platform code — E74 is one such (separately tracked); any other rewrite required by E75 verification failures spawns its own follow-up epic, not bundled into E75.
- New platform support (BSD, ChromeOS, mobile) — per `roadmap.md §E75 Out of scope`.
- Per-IDE / per-web adapter verification (E73's job) — per `roadmap.md §E75 Out of scope`.
- Performance benchmarking under sustained load beyond correctness gates — per `roadmap.md §E75 Out of scope`.
- Migration of legacy ad-hoc test patterns into the harness (greenfield only).
- Endpoint Security entitlement adoption on macOS — per E74 D3 (separate decision).
- Older Windows / Linux distro coverage beyond NFR-4 + NFR-5.
- Apple Silicon + Intel parity beyond manual / nightly cadence — per NFR-3.
- Stress / fuzz testing of the agent's hook pipeline (E62's work, not E75).
- Cross-tenant or multi-Hub topology testing (Hub-level work, not Agent-platform-level).

---

## 11. Cross-references

- `e74-macos-pf-intercept.md` — the macOS pf replacement epic. E75 macOS arm stories S3 / S4 / S5 re-run on the pf path once E74 lands (per FR-2.3 / FR-2.4 / FR-2.5 bimodal assertions). E74's FR-7 acceptance gate is the macOS-specific gap-class closure; E75's macOS arm is broader end-to-end coverage that falls on the pf path post-E74.
- `docs/developers/roadmap.md` §E75 — original scoping entry; lines 363-431 with the 20-story outline.
- `docs/developers/architecture/services/agent/agent-forwarder-architecture.md` — cross-platform forwarder + phase model; harness assertions on `traffic_event` shape derive from here.
- `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md` — macOS S2 (FR-2.2) verification authority.
- `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md` — macOS install / uninstall + path conventions authority.
- `docs/developers/architecture/services/agent/agent-linux-platform-architecture.md` — Linux distros, systemd unit, iptables rules authority.
- `docs/developers/architecture/services/agent/agent-windows-platform-architecture.md` — Windows SCM + WinDivert + named-pipe authority.
- `docs/developers/architecture/services/agent/agent-enrollment-architecture.md` — S16 mTLS enrollment authority.
- `docs/developers/architecture/services/agent/agent-keystore-architecture.md` — S18 keystore-migration authority.
- `docs/developers/architecture/services/agent/agent-autoupdater-architecture.md` — S17 auto-updater authority.
- `docs/developers/architecture/services/agent/agent-tray-ipc-architecture.md` — S20 tray IPC parity authority.
- `docs/developers/architecture/services/agent/agent-paths-abstraction-architecture.md` — uninstall completeness path scans (FR-2.5 / FR-3.5 / FR-4.5) derive from here.
- `docs/developers/architecture/services/agent/agent-backpressure-rollup-architecture.md` — audit drain queue + retry behaviour authority (FR-2.5 / FR-3.4 / FR-4.5).
- `.claude/skills/build-agent/` — invoked by S1 (FR-2.1) for macOS .pkg build.
- `.claude/skills/test-cursor-adapter/` — invoked by S3 (FR-2.3) for macOS IDE intercept verification.
- `.claude/skills/test-compliance-proxy/` — pattern source for the new `/test-agent-verification` skill (FR-1.8).
- CLAUDE.md binding `Test/skill env files live under tests/.env.<target>` — applies to `verify-macos | verify-linux | verify-windows` targets.
- CLAUDE.md binding `Tests must only touch own data` — applies to seed-prefix discipline (NFR-8).
- CLAUDE.md binding `Scenario test env isolation` — applies to FR-1.4 + NFR-7 fail-closed-on-prod-hostname rule.
- Memory `feedback_scenario_test_env_isolation` — operational source for the env-isolation rule.
- Memory `feedback_tests_only_own_data` — operational source for the seed-prefix rule.
- Memory `project_ne_cursor_streamchat_verification` — the existing deferred verification that this epic operationalises across all platforms.
- Memory `project_cursor_capture_investigation` — the prior investigation that motivated the macOS arm's S3 story.
