# E74-S4 — pf-Path Fail-Open Invariants, Tests, and Panic-Recovery

> Story: e74-s4
> Epic: 74
> Status: Planning
> Date: 2026-05-21
> FR mapping: FR-4.1, FR-4.2, FR-4.3, FR-4.4, FR-4.5, FR-4.6, FR-4.7, FR-4.8, FR-7.3
> Source decisions: DEC-001 (pf is the steady-state default; hybrid retired), DEC-006 (atomic pf anchor re-load via `pfctl -f`), DEC-009 (panic auto-removal = launchd KeepAlive + Go signal handler; no sibling LaunchDaemon)
> Architecture: `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md` (binding; this story does NOT modify it — FR-9.2 doc-lockstep is Phase 6 work)
> Blocked by: S1 (pf rule package installs the defensive pass-rule above the rdr rule), S2 (listener implements sync-decision + 500 ms SNI read deadline), S5 (wiring registers signal handlers), S6 (build injects launchd KeepAlive plist)
> Blocks: E74 close (all fail-open invariant tests must pass before E74 is gated stable)

---

## 1. User Story

As a **macOS agent operator** responsible for a fleet of developer Macs, I need confidence that the pf interception path degrades to transparent passthrough — never to a stuck or broken network — whenever the agent daemon misbehaves (panic, `kill -9`, anchor stuck mid-reload), so that the five fail-open rules that protect the existing NE path are equally enforced on the new pf path, and I can flip `interceptMode=pf` without carrying new catastrophic-failure risk.

**Failure scenario this story prevents**: the 2026-05-15 NE incident (`agent-ne-fail-open-architecture.md §1`) required manual `launchctl unload` + plist deletion to restore networking because the NE provider claimed flows it couldn't relay. The pf-equivalent failure mode is an anchor that stays loaded after the daemon dies, redirecting TCP/443 to a port nothing is listening on — every HTTPS connection from every user process stalls with `ECONNREFUSED` until the anchor is cleared. This story ensures that scenario is mechanically impossible: the anchor is cleared by signal handler (sub-second) or by launchd restart cleanup (≤ ThrottleInterval = 5 s), and every failing flow exits promptly via `opaqueRelay` or `ECONNREFUSED`-then-TCP-retry rather than hanging indefinitely.

---

## 2. Tasks

### T4.1 — Rule 1 transfer: verify synchronous decision in the listener

- T4.1.1 Review the `pfintercept/listener` package (delivered by S2) and confirm that `domain.Engine.Evaluate` is called synchronously on the hot path — no goroutine spawn, channel receive, or IPC call between `Accept()` and the `inspect / passthrough / deny` branch. Add a code comment at the call site citing FR-4.1 and Rule 1.
- T4.1.2 Write a table-driven unit test `TestListenerDecisionIsSync` in `pfintercept/listener/listener_test.go`. The test instantiates the listener with a mock `domain.Engine` that records its invocation timestamp relative to the `Accept` timestamp; asserts the gap is sub-millisecond (i.e., the decision was not deferred to a goroutine). Uses the `OriginalDstResolver` mock from DEC-003's interface seam.
- T4.1.3 Add a `FailOpenAuditor.AssertSyncDecisionPath()` call (see §4 Interface contract) at listener startup so the assertion runs once per daemon start and logs a structured line `fail_open_audit rule=1 status=ok` (or `status=violated` + `os.Exit(1)` — see T4.8.3 for the choice).

### T4.2 — Rule 2 transfer: 500 ms SNI peek read deadline enforcement

- T4.2.1 Confirm (in code review of S2 output) that `pfintercept/listener` sets a `conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))` before peeling the TLS ClientHello. The deadline MUST be reset to `time.Time{}` (no deadline) before the connection is handed to `BumpFlow` or `opaqueRelay`. Add a code comment citing FR-4.2 and Rule 2.
- T4.2.2 Write unit test `TestSNIPeekTimeoutFallsThrough`: inject a mock connection whose `Read` never returns; assert that the listener falls through to passthrough (calls `opaqueRelay`, does NOT close the connection) within 600 ms of `Accept`. Uses a net.Conn mock with a blocking `Read`.
- T4.2.3 Write unit test `TestSNIPeekMalformedClientHello`: inject a connection that returns garbage bytes immediately; assert the listener falls through to passthrough (not deny) when the ClientHello parse fails — consistent with NE's `peekSNIThenRelay` fall-through behaviour.
- T4.2.4 Add `FailOpenAuditor.AssertSNIDeadlineConfigured()` startup assertion that reads the listener's configured read-deadline duration from its config struct and asserts it is ≤ 500 ms.

### T4.3 — Rule 3 transfer: empty-list fail-safe for `quicFallbackUIDs` and other admin-pushed lists

- T4.3.1 In `pfintercept/pfrules`, confirm that when `quicFallbackUIDs` is empty (either not yet pushed by Hub, or admin cleared the bundle list), the generated pf rules contain **no** UDP rdr lines — i.e., an empty list means no UDP redirect, not "redirect all UDP/443". Add an assertion comment citing FR-4.3 and Rule 3.
- T4.3.2 Write unit test `TestEmptyQuicFallbackUIDsProducesNoUDPRdrRule`: call the rule template renderer with an empty UID slice; assert the rendered ruleset contains zero `rdr` lines matching `proto udp`. Uses the `pfrules.Render(PFRuleConfig{...})` function directly (no cgo, pure Go template logic).
- T4.3.3 Write unit test `TestEmptyDomainEngineAllowsAllFlows`: configure the listener with a `domain.Engine` whose `Evaluate` always returns `passthrough` (equivalent to an empty/permissive policy); assert every accepted connection reaches `opaqueRelay`, none are denied. Confirm this is the correct fail-safe direction.
- T4.3.4 Add `FailOpenAuditor.AssertEmptyListFailSafe(listName string, list []string)` method: if list is empty, log `fail_open_audit rule=3 list=<listName> status=empty-safe` at INFO level; if list is non-empty, log `status=populated count=<n>`.

### T4.4 — Rule 4 transfer: ban `isLikelyXyz = true` placeholder pattern (lint gate)

- T4.4.1 Add a grep-based check to `scripts/check-fail-open-placeholders.sh` (new script, ~15 lines) that fails if any file under `packages/agent/internal/platform/darwin/pfintercept/` contains the pattern `isLikely[A-Z][a-zA-Z]* *= *true`. The script exits non-zero if any match is found, zero otherwise.
- T4.4.2 Wire `check-fail-open-placeholders.sh` into the `npm run lint` step (or a new `npm run check:fail-open`) so it runs in CI on every PR that touches `pfintercept/**`. Document the hook in `scripts/README.md` (or the existing scripts section of `conventions.md`).
- T4.4.3 Write a test `TestNoIsLikelyPlaceholdersInPfIntercept` in a dedicated `pfintercept/failopen_lint_test.go` that performs the same grep programmatically (walks the `pfintercept/` directory tree, parses each `.go` file with `go/ast`, and asserts no identifier matching `isLikely.*` is assigned a literal `true` at its declaration site). This test is redundant with the shell script but provides a Go-native gate that survives a `Makefile`-free CI setup.
- T4.4.4 Rule 4 has no runtime `FailOpenAuditor` assertion (it is a static-analysis gate, not a runtime invariant). Note this explicitly in the `FailOpenAuditor` interface doc (§4).

### T4.5 — Rule 5 transfer: system DNS/DHCP/Push UID exclusion via defensive pass-rule

- T4.5.1 Confirm (in code review of S1 output) that `pfintercept/pfrules` emits a `pass out quick proto { tcp udp } user { mdnsresponder configd dhcpcd apsd nsurlsessiond kdc ntpd }` (or equivalent macOS-specific UID-based syntax) as the **first** rule in the generated anchor, above every `rdr` rule. Add a comment citing FR-4.5 and Rule 5.
- T4.5.2 Write unit test `TestSystemServicePassRuleIsFirst`: render the pf rules for a non-empty `quicFallbackUIDs` list; parse the rendered ruleset line-by-line; assert the first non-comment, non-blank line is the system-service pass rule. The system daemons (`mdnsresponder`, `configd`, `dhcpcd`, `apsd`, `nsurlsessiond`, `kdc`, `ntpd`) MUST all appear in that rule.
- T4.5.3 Write unit test `TestSystemServicePassRuleExcludesUDPRedirect`: render rules with a `quicFallbackUIDs` list that accidentally includes the UID of `mdnsresponder` (injected via test fixture); assert the generated ruleset's net effect (pass-rule before rdr-rule) means `mdnsresponder`'s UDP is never redirected. This test validates the "two lines of defence" property from FR-4.5.
- T4.5.4 Add `FailOpenAuditor.AssertSystemServicesExcluded(renderedRules string)` method: parse the rendered anchor text and verify all seven daemon names appear in the leading pass rule. Log `fail_open_audit rule=5 status=ok` or panic with a clear message (see T4.8.3).

### T4.6 — Recovery procedure: documentation and verification command

- T4.6.1 The pf recovery command `sudo pfctl -a nexus-agent/transparent -F all` MUST appear in `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md` §"Recovery procedure" before E74 closes (FR-4.6). This task tracks that the command is tested end-to-end at least once during development, not just written in a doc.
- T4.6.2 Write a manual verification checklist entry in the S4 acceptance criteria (AC4.8) that requires an engineer to: (a) start the daemon, (b) verify `pfctl -s rules -a nexus-agent/transparent` shows active rules, (c) run `sudo pfctl -a nexus-agent/transparent -F all`, (d) verify the output is empty, (e) verify DNS still resolves (`dig @8.8.8.8 example.com` succeeds). Record the result in the PR description.
- T4.6.3 No code change in this task — the recovery command is a plain `pfctl` invocation with no daemon involvement. The value of this task is the verification run and the checklist entry.

### T4.7 — 24-hour developer-machine stability test plan

- T4.7.1 Define the stability test protocol (to be executed manually before E74 merges to `main`):
  - Install the pf-only build (`build-agent --target=pf-only`) on a developer's daily-driver macOS machine.
  - Run for 24 continuous hours of normal use: web browsing, Slack, IDE (VS Code or Cursor), video calls (Zoom or Google Meet), background system updates.
  - Checklist items sampled at 0 h, 4 h, 8 h, 16 h, 24 h: (a) `curl -s https://api.openai.com/v1/models` succeeds within normal latency; (b) `dig @8.8.8.8 example.com` succeeds; (c) no manual recovery action taken; (d) daemon PID has not changed (no crash-restart); (e) `pfctl -s rules -a nexus-agent/transparent` shows expected rules.
  - Result documented in the E74 PR description as a table: timestamp × checklist item × pass/fail.
- T4.7.2 Write a lightweight Go test `TestStabilityChecklist` in `pfintercept/integration_test.go` (build-tagged `//go:build darwin && integration`) that automates the 0 h checklist items: (a) pf rules parseable and non-empty after daemon start, (b) loopback listener is accepting connections, (c) a synthetic passthrough flow completes without hanging. This test is run by the engineer at each manual checklist interval. It does not replace the 24 h human observation but reduces the checklist from visual to command-driven.
- T4.7.3 Record the test binary invocation in the PR checklist so each tester knows the exact command: `go test -tags darwin,integration -run TestStabilityChecklist ./packages/agent/internal/platform/darwin/pfintercept/...`.

### T4.8 — Daemon-panic auto-removal: signal handler, launchd KeepAlive, first-run `pfctl -F`

Per DEC-009 (B + C in concert):

- T4.8.1 **Signal handler (C — fast path)**: in `packages/agent/cmd/agent/cmd_run.go`, the existing signal-handling block at line 608 (`case sig := <-sigCh:`) already calls `cancel()` and eventually reaches `defer plat.Stop()`. Confirm that `plat.Stop()` (in S5 wiring) calls `pfintercept.Teardown()` which runs `pfctl -a nexus-agent/transparent -F all`. Add a `defer pfintercept.Teardown()` call near the top of `cmdRun` (after pf rules are first installed) so SIGTERM, SIGINT, and panic-recovery (`shareddiag.Recover`) all hit the cleanup path. Do NOT use `--no-verify` or skip signal hooks.
- T4.8.2 **Restart-time cleanup (B — kill-9 path)**: in the pf intercept startup sequence (within `pfintercept.Setup()`), before installing new rules, always run `pfctl -a nexus-agent/transparent -F all` first. This ensures a prior-crash's stale anchor is cleared on every launchd restart. Produces the sequence: launchd restart → daemon starts → `pfctl -F` (clears stale rules) → `pfctl -f` (installs fresh rules). Worst-case anchor-stuck window = launchd `ThrottleInterval` (5 s from DEC-009).
- T4.8.3 **launchd plist (B — KeepAlive)**: confirm (in S6 build output) that `dist/macos/com.nexus.agent.plist` contains `<key>KeepAlive</key><true/>` and `<key>ThrottleInterval</key><integer>5</integer>`. This task verifies the plist shape; the plist itself is authored in S6.
- T4.8.4 Write unit test `TestSetupRunsPfctlFBeforeInstall`: use a mock `PFRuleInstaller` (from DEC-008's interface seam) and assert that `Setup()` calls `Flush()` before `Install()` — regardless of whether prior rules exist. Order assertion uses a sequenced mock call recorder.
- T4.8.5 Write unit test `TestTeardownRunsPfctlFOnSignal`: use a mock `PFRuleInstaller` and assert that `Teardown()` calls `Flush()`. Wire a fake signal delivery through the `pfintercept.Interceptor` shutdown path. Verify `Flush()` is called even when the underlying `pfctl` subprocess exits with non-zero (i.e., "anchor absent" is not an error).
- T4.8.6 **Stress test**: define the acceptance procedure for FR-4.8 ("simulate daemon panic mid-flow, verify anchor clears within 30 s"):
  - Start the daemon with `interceptMode=pf`.
  - Establish 5 concurrent persistent TCP connections through the listener (use a synthetic loopback test client).
  - `kill -9 <daemon-pid>`.
  - Start a stopwatch. Poll `pfctl -s rules -a nexus-agent/transparent` every 2 s.
  - Assert anchor is empty within 30 s (expected: ≤ ThrottleInterval + startup time ≈ 10 s in practice).
  - Assert DNS resolves during the stuck window (a `dig @8.8.8.8 example.com` in a tight loop must not return SERVFAIL).
  - Document the measured time-to-clear in the PR description.

### T4.9 — FR-7.3 gate: fail-open visibility improvement measurement

- T4.9.1 Run the FR-7.3 protocol: 10 concurrent IDE sessions (VS Code + Copilot or Cursor + Claude) hitting AI providers for 5 minutes under pf path. Collect `traffic_event` rows for the test window; compute `inspect-mode rows with non-null request_normalized` / `total rows`.
- T4.9.2 Run the same protocol under the legacy NE path (same Mac, same 5-minute window, flip `interceptMode=ne`). Record the NE baseline ratio.
- T4.9.3 Assert pf path ratio ≥ 95% AND pf ratio ≥ NE baseline ratio (no regression). If pf ratio < NE baseline, investigate root cause before merging — this would indicate the listener's 500 ms SNI timeout is firing more than expected, or `opaqueRelay` is being triggered for flows that were `inspect` under NE.
- T4.9.4 Record both ratios in the PR description. The acceptance threshold (95%) is from FR-7.3; if the E62 baseline measurements show the NE path is already above 95%, the pf gate tightens to match.

### T4.10 — `FailOpenAuditor` startup wiring

- T4.10.1 In `pfintercept/setup.go` (the `Setup()` function delivered by S5 wiring), construct a `ProductionFailOpenAuditor` and call all four assertion methods in order immediately after pf rules are installed. The call sequence must be: `AssertSyncDecisionPath()`, `AssertSNIDeadlineConfigured(cfg.SNIPeekTimeout)`, `AssertEmptyListFailSafe("quicFallbackUIDs", len(cfg.QuicFallbackUIDs))`, `AssertSystemServicesExcluded(renderedAnchorText)`. On any non-nil error from methods 1, 2, or 4: call `Teardown()` and return the error to the caller — daemon startup aborts cleanly rather than running a misconfigured pf anchor.
- T4.10.2 Write unit test `TestSetupAbortsOnAuditViolation`: use a mock `FailOpenAuditor` whose `AssertSystemServicesExcluded` returns a non-nil error; assert that `Setup()` returns an error AND that `Teardown()` was called exactly once (via mock call recorder). Verifies the fail-safe abort path.
- T4.10.3 Write unit test `TestSetupSucceedsOnCleanAudit`: use a `NoopFailOpenAuditor` (all methods return nil); assert `Setup()` returns nil when pf rule installation succeeds. Baseline coverage for the happy path.

### T4.11 — Cross-check with Linux `Stop()` cleanup shape

- T4.11.1 Read `packages/agent/internal/platform/linux/linux_linux.go` `Stop()` method to confirm the pattern: call iptables REDIRECT rule removal unconditionally, log any error but do not propagate it (cleanup is best-effort on shutdown). Mirror this pattern in `pfintercept.Teardown()`: run `pfctl -a nexus-agent/transparent -F all`, log any error at WARN level, return nil. Rationale: a cleanup error on shutdown should not prevent the daemon from exiting cleanly.
- T4.11.2 Add a code comment in `pfintercept/setup.go` at the `Teardown()` implementation: `// Mirrors linux_linux.go Stop() cleanup shape: best-effort, log-on-error, never fatal.`

---

## 3. Acceptance Criteria

- **AC4.1**: DNS resolution to `8.8.8.8` succeeds throughout a 60-second simulated daemon panic (`kill -9` while connections are active). Verified by `dig @8.8.8.8 example.com` running in a tight loop during T4.8.6 stress test — zero `SERVFAIL` responses observed.
- **AC4.2**: pf anchor `nexus-agent/transparent` is empty within 30 seconds of `kill -KILL <daemon-pid>`. Verified by T4.8.6 stopwatch measurement; measured time recorded in the PR description.
- **AC4.3**: Fresh-install Chrome from a non-QUIC-allowlisted user uid has UDP/443 NOT redirected. Verified by: render pf rules with an empty `quicFallbackUIDs` list; inspect rendered ruleset; assert zero `rdr` lines matching `proto udp`. Unit test T4.3.2 is the automated gate.
- **AC4.4**: The seven system daemons (`mdnsresponder`, `configd`, `dhcpcd`, `apsd`, `nsurlsessiond`, `kdc`, `ntpd`) appear in the leading `pass` rule of every generated anchor ruleset, above all `rdr` rules. Verified by unit test T4.5.2 passing in CI.
- **AC4.5**: `grep -rn 'isLikely[A-Z][a-zA-Z]* *= *true' packages/agent/internal/platform/darwin/pfintercept/` returns zero matches. Verified by `check-fail-open-placeholders.sh` (T4.4.1) and Go AST test T4.4.3 both passing in CI.
- **AC4.6**: The SNI read deadline is ≤ 500 ms and the listener falls through to `opaqueRelay` (not close) on timeout. Verified by unit test T4.2.2 passing with a blocking mock connection.
- **AC4.7**: `domain.Engine.Evaluate` is invoked synchronously per accepted connection — no goroutine spawn between `Accept()` and `Evaluate()`. Verified by unit test T4.1.2 timestamp-gap assertion.
- **AC4.8**: Recovery procedure verified manually: after `sudo pfctl -a nexus-agent/transparent -F all`, `pfctl -s rules -a nexus-agent/transparent` returns empty output AND `dig @8.8.8.8 example.com` succeeds within 2 s. Checklist from T4.6.2 completed and recorded in PR description.
- **AC4.9**: 24-hour developer-machine stability test (T4.7.1) passes with no manual recovery action and no daemon crash restart. Result table (5 × 5 checklist items) recorded in PR description.
- **AC4.10**: pf-path inspect-mode content-capture rate ≥ 95% over the FR-7.3 load test (T4.9), and pf rate ≥ NE baseline rate. Both measurements recorded in PR description.
- **AC4.11**: Unit-test coverage for `pfintercept/listener`, `pfintercept/pfrules`, `pfintercept/failopen`, and any other helper packages introduced in this story is ≥ 95% per package (CLAUDE.md binding). Cgo-only glue files (`natlook/natlook_darwin.go`, `pidlookup/pidlookup_darwin.go`) are in `.coverage-allowlist` under category D per DEC-008. Verified by `scripts/check-go-coverage.sh --staged`.
- **AC4.12**: `FailOpenAuditor.AssertSystemServicesExcluded` and `AssertSyncDecisionPath` abort `Setup()` on violation — verified by unit test T4.10.2 (mock auditor returns error; `Setup()` returns error and calls `Teardown()` exactly once).
- **AC4.13**: `pfintercept.Setup()` runs `pfctl -a nexus-agent/transparent -F all` before installing new rules (T4.8.2). Verified by unit test T4.8.4 (mock call recorder asserts `Flush()` precedes `Install()`).
- **AC4.14**: `check-fail-open-placeholders.sh` exits non-zero when the pattern `isLikely[A-Z][a-zA-Z]* *= *true` is present in any file under `pfintercept/`. Verified by T4.4.1 script and T4.4.3 Go AST test, both passing in CI.

---

## 4. Interface Contract

### `FailOpenAuditor` interface

The listener package exposes a `FailOpenAuditor` interface, called once at daemon startup (after pf rules are installed) and once per `Setup()` call. Its purpose is to assert the five rules are encoded in current state and emit structured log lines that operators can grep for `fail_open_audit` during support investigations.

```go
// FailOpenAuditor checks that the pf-path fail-open invariants are
// satisfied at startup. Implementations MUST be safe to call concurrently.
// All methods log a structured "fail_open_audit" line at INFO level.
// If a check fails, the method logs at ERROR level and — depending on
// the severity of the violation — either returns an error (Rules 1, 2, 5)
// or continues (Rules 3, 4 are static-analysis gates, not runtime checks).
type FailOpenAuditor interface {
    // AssertSyncDecisionPath verifies that domain.Engine.Evaluate is on
    // the synchronous call path of the listener. Returns nil if the
    // listener's constructor received a non-nil Engine (Rule 1).
    AssertSyncDecisionPath() error

    // AssertSNIDeadlineConfigured verifies that the listener's configured
    // SNI peek read deadline is ≤ 500 ms (Rule 2).
    AssertSNIDeadlineConfigured(deadline time.Duration) error

    // AssertEmptyListFailSafe logs the current state of an admin-pushed
    // list. Empty list → log "status=empty-safe". Does not return an error
    // because an empty list is a valid operational state (Rule 3).
    AssertEmptyListFailSafe(listName string, count int)

    // AssertSystemServicesExcluded parses the rendered pf anchor text and
    // verifies all seven system daemons appear in the leading pass rule
    // (Rule 5). Returns an error if any daemon is missing.
    AssertSystemServicesExcluded(renderedAnchorText string) error
}
```

**Log fields emitted by every method**:

| Field | Value |
|---|---|
| `msg` | `"fail_open_audit"` |
| `rule` | `1` / `2` / `3` / `5` (Rule 4 is static-only) |
| `status` | `"ok"` / `"empty-safe"` / `"violated"` |
| `detail` | human-readable description of what was checked |
| `daemon_pid` | `os.Getpid()` |
| `anchor` | `"nexus-agent/transparent"` |

**Violation handling**: Rules 1 and 5 violations log at `ERROR` and return a non-nil error. The caller (`pfintercept.Setup`) treats this as a fatal condition: it calls `Teardown()` and returns the error to `cmdRun`, which logs and exits — preventing a misconfigured pf path from running. Rule 2 violation (deadline too large) returns an error and prevents daemon startup. Rule 3 is informational only (empty is safe).

**Sample log lines emitted at startup (INFO level on success)**:

```
INFO fail_open_audit rule=1 status=ok detail="domain.Engine non-nil; decision path is synchronous" daemon_pid=1234 anchor=nexus-agent/transparent
INFO fail_open_audit rule=2 status=ok detail="SNI peek deadline=500ms" daemon_pid=1234 anchor=nexus-agent/transparent
INFO fail_open_audit rule=3 list=quicFallbackUIDs status=empty-safe daemon_pid=1234 anchor=nexus-agent/transparent
INFO fail_open_audit rule=5 status=ok detail="system daemons [mdnsresponder configd dhcpcd apsd nsurlsessiond kdc ntpd] present in pass rule at line 1" daemon_pid=1234 anchor=nexus-agent/transparent
```

An operator or support engineer can search daemon logs for `fail_open_audit status=violated` to immediately identify which rule was tripped and why.

**`ProductionFailOpenAuditor`**: the production implementation lives in `pfintercept/failopen/auditor.go` and satisfies the interface. A `NoopFailOpenAuditor` (all methods return nil, `AssertEmptyListFailSafe` is a no-op) is available for unit tests that do not need the audit assertions.

**Rule 4 note**: Rule 4 ("no `isLikelyXyz = true` placeholders") is a **static-analysis invariant**, not a runtime one. It intentionally does not appear as a method on `FailOpenAuditor`. The interface comment on the type declaration reads: `// Rule 4 is enforced at review time via check-fail-open-placeholders.sh and the Go AST test in failopen_lint_test.go; no runtime assertion is possible for a "code pattern was not written" property.`

**Anchor-text argument to `AssertSystemServicesExcluded`**: the caller passes the rendered anchor text produced by `pfrules.Render(cfg)` — the same string that was just written to `pfctl`'s stdin via atomic re-load (DEC-006). This avoids a second `pfctl -s rules` subprocess call during startup and makes the assertion deterministic (it reads the just-installed text, not a live kernel read-back that might lag by a tick).

---

## 5. Dependencies

| Story | What S4 depends on it for |
|---|---|
| S1 (pf rule package, `pfintercept/pfrules`) | Delivers the rule template + `Render(PFRuleConfig)` + the defensive pass-rule for system daemons (T4.5.1). S4 unit tests call `pfrules.Render` directly. |
| S2 (loopback listener, `pfintercept/listener`) | Delivers `Accept` loop + `domain.Engine.Evaluate` call site + SNI peek with 500 ms deadline. S4 tests wrap the listener with mock seams (T4.1.2, T4.2.2). |
| S5 (daemon wiring — `cmd/agent/cmd_run.go`) | Registers the SIGTERM/SIGINT signal handlers and wires `plat.Stop()` → `pfintercept.Teardown()`. S4 task T4.8.1 specifies the `defer pfintercept.Teardown()` placement; S5 implements it. |
| S6 (build — `dist/macos/com.nexus.agent.plist`) | Adds `KeepAlive=true` + `ThrottleInterval=5` to the launchd plist. S4 task T4.8.3 verifies the plist shape; S6 authors it. |

S4 does NOT depend on S3 (process attribution / `libproc`) — fail-open invariants are independent of how attribution is resolved. The `FailOpenAuditor` startup checks run before any attribution lookup; a libproc failure never trips a fail-open violation.

---

## 6. Out of Scope

- **Incident-response runbook update** (`docs/operators/ops/runbooks/agent-recovery.md`): the pf recovery command addition and the doc-lockstep update to `agent-ne-fail-open-architecture.md` are Phase 6 deliverables per FR-9.2 and FR-9.5. This story only tests the invariants; Phase 6 generalises the docs.
- **Windows / Linux fail-open changes**: Linux already uses iptables REDIRECT + `Stop()` cleanup; its fail-open story is separate. Windows is WinDivert, out of E74 scope entirely.
- **Endpoint Security entitlement adoption** (per D3): no change to the attribution layer or its fail-open properties.
- **Per-IDE / per-web adapter verification**: E73 covers adapters; E75's macOS arm re-runs against the new pf path once E74 lands.
- **Latency measurement** (FR-7.4): that is a Should-priority FR tracked in S5's smoke run, not here.
- **NFR-13 / E86 e2e-coverage-matrix row**: per NFR-13, a new row for the `/test-macos-pf-agent` skill must be added to `docs/developers/specs/e86-e2e-coverage-matrix.md` (Phase 6 doc-lockstep task). S4 specifies the tests that skill will exercise; it does not create the matrix row.
- **`interceptMode` admin UI change** (FR-8.5): the dropdown widget and i18n keys are S5 / S6 scope.
- **`agent_intercept_mode_change` audit event** (FR-5.4): emitted by S5 wiring on mode flip; not in scope here.
- **`check-doc-lockstep.mjs` trigger map update**: adding `packages/agent/internal/platform/darwin/pfintercept/**` → the macOS platform architecture doc trigger row (FR-9.3) is a Phase 6 step. No trigger-map change in S4.
- **`scripts/doc-lockstep.config.mjs` update**: same as above — deferred to Phase 6 doc-lockstep story per FR-9.1 / FR-9.2.

---

## 8. Implementation Notes

- **Rule 1 vs Rule 2 asymmetry on the pf path**: Under NE, Rule 2 (async-callback timeout) was the primary guard because `requestDecision` was genuinely async over IPC. Under pf, `domain.Engine.Evaluate` is in-process and synchronous; the 500 ms read deadline (Rule 2) is therefore the last remaining async surface. The `FailOpenAuditor` reflects this asymmetry: `AssertSyncDecisionPath` is a constructor-time structural check, while `AssertSNIDeadlineConfigured` is a config-value check. Both are still needed — their failure modes are different.

- **Why `Teardown()` is best-effort on error**: mirroring the Linux `Stop()` idiom (T4.11.1), a `pfctl -F` error on shutdown must not block the daemon from exiting. If `pfctl` itself is broken (unusual), the launchd restart loop (DEC-009 path B) provides the recovery. Propagating the error would block `cmd_run.go`'s deferred shutdown sequence and could leave other cleanup (`auditQueue.Close()`, `tc.Close()`) from running.

- **Test seam for cgo-free unit tests**: all unit tests in this story use the `OriginalDstResolver`, `PFRuleInstaller`, and `BundleResolver` mock interfaces from DEC-003, DEC-006, and DEC-004 respectively. No test in S4 needs root, `/dev/pf`, or a running pf subsystem. The manual acceptance tests (T4.6.2 recovery procedure, T4.7.1 24 h stability, T4.8.6 kill-9 stress) are explicitly manual because they require real pf state — they are described in prose acceptance criteria, not automated Go tests.

- **Ordering of `Setup()` steps**: the correct install sequence is (1) `Teardown()` to flush any prior stale anchor, (2) `BundleResolver.ResolveUIDs` to compute the QUIC fallback uid set, (3) `pfrules.Render(cfg)` to generate the anchor text, (4) `FailOpenAuditor.AssertSystemServicesExcluded(renderedText)` to gate on Rule 5, (5) `PFRuleInstaller.Install(renderedText)` to atomically load the anchor. If step 4 returns an error, step 5 never runs — the anchor remains empty (fail-open) and `Setup()` returns the error.

- **`ECONNREFUSED` vs hang on stuck anchor**: when the daemon is dead and the anchor is still loaded, TCP flows are redirected to `127.0.0.1:13443` where nothing is listening. macOS TCP stack returns `ECONNREFUSED` immediately (the port is closed). The user's HTTPS client retries on a different path (most TLS stacks retry or fall through); the flow is delayed by one retry, not hung forever. This is meaningfully better than the NE failure mode (flow claimed, no relay → stall until socket timeout, typically 75 s on macOS). Document this in the Phase 6 recovery runbook update.

- **Integration test build tag**: T4.7.2's `TestStabilityChecklist` uses `//go:build darwin && integration`. It does not run in standard `go test ./...`; it runs only when the engineer explicitly sets `-tags integration`. This follows the existing pattern for network-bound integration tests and keeps the default CI run fast.

---

## 9. References

- Requirements `docs/developers/specs/e74-macos-pf-intercept.md` §FR-4 (FR-4.1 – FR-4.8) and §FR-7.3
- Decision log `docs/developers/specs/e74/DECISIONS.md` — DEC-001 (hybrid retired; pf is steady-state), DEC-006 (atomic anchor re-load via `pfctl -f`), DEC-008 (interface seams for cgo coverage), DEC-009 (panic auto-removal = launchd KeepAlive + signal handler, no sibling LaunchDaemon), DEC-011 (package decomposition into `pfintercept/pfrules`, `pfintercept/listener`, `pfintercept/natlook`, `pfintercept/pidlookup`)
- Architecture doc `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md` §2 "The five fail-open rules (binding)" — verbatim rules this story transfers to the pf path; §3 "Recovery procedure" — the `launchctl unload` recovery that S4 accompanies with a pf-equivalent; §4 "Test invariants" — the NE checklist this story extends with pf-specific items
- `packages/agent/cmd/agent/cmd_run.go` lines 306–617 — existing signal handling block (`sigCh`, `cancel()`, `emitShutdownGracefully`); `defer plat.Stop()` at line 589; location for `defer pfintercept.Teardown()` per T4.8.1; `emitShutdownGracefully` pattern at lines 58–63
- `packages/agent/cmd/agent/main.go` — daemon entry point; confirms no top-level deferred cleanup exists today, establishing the placement contract for the S4-specified `defer pfintercept.Teardown()` in `cmdRun`
- `packages/agent/internal/platform/linux/linux_linux.go` `Stop()` — reference implementation for best-effort cleanup on daemon shutdown; T4.11.1 mirrors this shape in `pfintercept.Teardown()`
- CLAUDE.md "macOS NE proxy must fail-open, never fail-closed (safety-critical)" — the binding rule that S4 enforces on the pf path
- CLAUDE.md "Unit test coverage ≥ 95% per Go package" — coverage target for all new `pfintercept/` sub-packages (enforced by `scripts/check-go-coverage.sh --staged`)
