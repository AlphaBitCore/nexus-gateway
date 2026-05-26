# E74-S1 — pf anchor + `rdr` rule installation

> Epic: 74
> Story: 1
> Status: Planning (Step 3 SDD)
> Date: 2026-05-21
> FR mapping: FR-1.1 .. FR-1.8 (from `docs/developers/specs/e74-macos-pf-intercept.md`)
> Source decisions: DEC-001 (NE Swift gated, not removed), DEC-002 (redirect all TCP 443; domain decision in daemon), DEC-005 (listener port 13443 default), DEC-006 (atomic anchor reload), DEC-008 (interface seam for 100% coverage), DEC-009 (panic auto-removal sequencing), DEC-011 (sub-package layout), DEC-013 (CP/Agent share by default — intercept boundary is darwin-only).
> Architecture impact: covered in Story S9 (doc lockstep) — `agent-macos-platform-architecture.md` gains §"Interception path (pf)" with the rule shape this story implements.
> Dependencies: none upstream. Blocks S2 (listener consumes the redirected flows), S4 (defensive system-services pass-rule), S5 (wiring installs/removes anchor on mode flip), S6 (build target ships installer scripts), S7 (gap-closure tests verify rule shape), S9 (doc lockstep).

---

## 1. User story

As a macOS agent operator running `interceptMode="pf"`, I want the agent daemon to install a private pf anchor with `rdr` rules that redirect target outbound flows to the daemon's loopback listener, so that captured traffic flows through the same MITM + hook pipeline used by Linux pf path and Compliance Proxy, without me having to manage pf rules by hand and without the rules surviving a daemon crash.

---

## 2. Tasks

### T1.1 — Create `pfintercept/pfrules/` sub-package skeleton

- Files: `packages/agent/internal/platform/darwin/pfintercept/pfrules/rules.go` (production), `rules_test.go` (test).
- Build tag: `//go:build darwin`.
- Per DEC-011 — this is a sub-package under `pfintercept/`; it has exactly one concern: render + install + remove pf rules. No listener glue, no libproc calls, no `domain.Engine` here.
- Package `package pfrules`.

### T1.2 — Define the `RuleSet` value object

```go
type RuleSet struct {
    Anchor            string   // "nexus-agent/transparent"
    ListenerHost      string   // "127.0.0.1"
    ListenerPort      int      // 13443 by default per DEC-005
    DaemonUID         uint32   // own-uid exclusion per FR-1.5
    QuicFallbackUIDs  []uint32 // pre-resolved from bundle list per DEC-004 / FR-1.4
    LoopbackExclusion bool     // true per FR-1.6
    RedirectTCP443    bool     // FR-1.3
    RedirectTCP80     bool     // optional admin-controlled per FR-1.3
    SystemDaemonUIDs  []uint32 // mdnsresponder/configd/dhcpcd/apsd/nsurlsessiond/kdc/ntpd — defensive pass-rule per FR-4.5 + FR-1.7
}
```

- Pure value object — no methods that hit the kernel. Used as input to `Render()`.

### T1.3 — `Render(ruleset) ([]byte, error)` — render the pf rules file

- Generates the `pf.conf`-syntax body for one anchor (the daemon owns the anchor scope; nothing else under `nexus-agent/transparent` exists).
- Output shape (in this order — pf evaluates first-match, last-match-wins per the `quick` semantic; we use last-match):
  ```
  # Defensive pass — system daemons NEVER redirected (FR-4.5 / FR-1.7)
  pass out quick proto udp from any to any port 53 user <each system uid>
  pass out quick proto udp from any to any port 67 user <dhcpcd uid>
  # … one pass-rule per system uid we know about
  
  # Own-process exclusion (FR-1.5) — daemon's upstream uTLS dials
  pass out quick proto tcp from any to any user <daemonUID>
  
  # Loopback exclusion (FR-1.6) — no double-redirect on localhost
  pass out quick proto tcp from any to lo0
  pass out quick proto tcp from any to 127.0.0.0/8
  pass out quick proto tcp from any to ::1
  
  # TCP 443 catchall redirect (FR-1.3)
  rdr pass on lo0 inet proto tcp from any to any port 443 -> 127.0.0.1 port 13443
  
  # Optional TCP 80 if RedirectTCP80
  rdr pass on lo0 inet proto tcp from any to any port 80 -> 127.0.0.1 port 13443
  
  # QUIC fallback UDP/443 — only for uids in QuicFallbackUIDs (FR-1.4)
  # one `block` rule per uid in QuicFallbackUIDs
  block drop out quick proto udp from any to any port 443 user <each quic uid>
  ```
- Lines are deterministic for stable diffs. Trailing newline. UTF-8.
- Unit-tested 100% via table-driven cases — empty list, full list, edge cases (zero UIDs → no QUIC rules), unicode chars in nothing because pf rules are ASCII (defensive validation: reject non-ASCII).
- Pure Go function, no syscall — 100% coverage trivially.

### T1.4 — `Installer` interface seam (the cgo/syscall boundary per DEC-008)

```go
type Installer interface {
    // Install replaces the anchor's contents atomically (DEC-006 — pfctl -a … -f -).
    // Returns the loaded byte count and any pfctl stderr lines on failure.
    Install(ctx context.Context, anchor string, rules []byte) error
    // Flush empties the anchor (idempotent — flushing an absent anchor is OK).
    Flush(ctx context.Context, anchor string) error
    // Diagnostic — `pfctl -a <anchor> -s rules` output. For Story S7 gap-closure tests.
    Show(ctx context.Context, anchor string) ([]byte, error)
}
```

- Production impl: `RealInstaller` shells out to `/sbin/pfctl` with the appropriate args. Single struct, ~30 LOC.
- Mock impl: `MockInstaller` records call sequences for tests. Simple.
- Pre-flight check in `RealInstaller.Install`: `/sbin/pfctl` exists, daemon is uid 0 (or `CAP_NET_ADMIN`-equivalent), otherwise return a structured error that the listener can log distinctly.
- pfctl is invoked with stdin (`-f -`) — no temp file, no race window. Per DEC-006.
- The cgo/exec boundary is allowlisted per DEC-008 (category D — OS-bound). Coverage of the seam interface is 100% via mock; the ≤30-LOC `RealInstaller` is exempt per the same allowlist policy.

### T1.5 — `Controller` orchestration type

```go
type Controller struct {
    installer Installer
    anchor    string
    logger    *slog.Logger
    mu        sync.Mutex
    lastRules []byte // for idempotency check + cleanup verification
}

func New(installer Installer, anchor string, logger *slog.Logger) *Controller
func (c *Controller) Apply(ctx context.Context, rs RuleSet) error
func (c *Controller) Remove(ctx context.Context) error
func (c *Controller) Snapshot() RuleSet // for diagnostics + reported state
```

- `Apply`: renders ruleset, compares to `lastRules` — if identical, no-op (idempotent per FR-1.2). Otherwise calls `installer.Install`.
- `Remove`: calls `installer.Flush`; idempotent (FR-1.2).
- Logs on every transition. `lastRules` is the source of truth for "current installed state".

### T1.6 — Pre-install cleanup hook (DEC-009 sequencing)

- `Controller.Apply`'s first action is **always** to call `installer.Flush(anchor)` BEFORE installing the new ruleset.
- Rationale: if the daemon crashed previously without invoking `Remove`, the anchor may still hold stale rules. Atomic re-load via `pfctl -f -` does not require pre-flush, but flush-then-load gives us a deterministic empty-then-populated transition and zero ambiguity in logs.
- Pre-flush cost: one pfctl invocation per daemon start (~50ms). Negligible.

### T1.7 — Daemon-exit cleanup integration

- Story S5 (wiring) registers a `defer` + signal handler that calls `Controller.Remove(ctx)` on graceful exit.
- Story S4 (fail-open invariants) verifies the auto-cleanup gates (launchd `KeepAlive=true` + 5s throttle + the next-launch pre-install Flush from T1.6).
- This story's responsibility ends at exposing `Remove(ctx) error`. The wiring contract is documented here so S5 cannot accidentally re-implement the flush logic.

### T1.8 — Unit tests — 100% coverage of `pfrules` package logic

Per DEC-008 — interface seams allow logic tests to hit 100% via mocks.

- **`Render` tests** — table-driven, 12+ cases covering: empty RuleSet (rendering should still be valid pf syntax with no rdr rules), single uid in QuicFallbackUIDs, multi-uid, system-daemon uids list shape, TCP-80 toggle, deterministic byte-for-byte output for a fixed input, ASCII-only validation, anchor name validation (only `[a-zA-Z0-9_/-]`).
- **`Controller.Apply` tests** — use MockInstaller; verify call sequence (Flush → Install); verify idempotency (second Apply with same RuleSet → no Install call); verify error propagation (MockInstaller returns error → Apply returns wrapped error).
- **`Controller.Remove` tests** — verify single Flush call; idempotency (second Remove → still one Flush each call, mock counts).
- **Concurrency test** — Apply + Remove from concurrent goroutines under `-race`; verify mutex serialises. The `mu sync.Mutex` is the only state.
- **Snapshot test** — verifies returned RuleSet is deep-equal to the last applied state, including post-Remove (where Snapshot returns zero-value RuleSet).
- Target: `go test -cover -count=1 ./packages/agent/internal/platform/darwin/pfintercept/pfrules/...` reports `coverage: 100.0% of statements`.

### T1.9 — `.coverage-allowlist` entry for the cgo/exec boundary

- `RealInstaller` shells out to `pfctl`. Even though `os/exec` is plain Go, the **`pfctl` exit codes + parsing of stderr** are inherently OS-bound — they exist only on macOS with root, not in CI.
- Add to `scripts/.coverage-allowlist`:
  ```
  packages/agent/internal/platform/darwin/pfintercept/pfrules:installer_real_darwin.go  # category D — OS-bound (requires root + /sbin/pfctl)
  ```
- File scope ≤30 LOC. Reviewer mechanically inspects.

### T1.10 — Empirical pf-rule syntax verification

- Phase 4 (Code) will run `pfctl -nf <rendered-rules-file>` on a developer Mac (root) to syntax-check the output without loading rules. This is one verification step in the SDD acceptance gate.
- Defer the actual run to Phase 4; this story records that the verification command exists.

---

## 3. Acceptance criteria

- **AC1.1** — `pfrules.Render(<typical RuleSet>)` produces a syntactically valid pf rules file. Verified by `pfctl -nf` on a dev Mac (no kernel load) — exit 0.
- **AC1.2** — `pfrules.Render` is deterministic: same RuleSet → byte-identical output across 100 runs.
- **AC1.3** — `Controller.Apply` is idempotent: calling Apply twice with the same RuleSet produces exactly one `Install` call to the mock.
- **AC1.4** — `Controller.Apply` first calls `Flush` then `Install` on every distinct ruleset (DEC-009 sequencing).
- **AC1.5** — `Controller.Remove` is idempotent: calling Remove twice produces exactly two `Flush` calls to the mock with no error on the second.
- **AC1.6** — Concurrent Apply + Remove under `-race` is safe; no goroutine sees a torn `lastRules`.
- **AC1.7** — pf rule shape includes: defensive `pass out quick` rules for `mdnsresponder / configd / dhcpcd / apsd / nsurlsessiond / kdc / ntpd` (resolved to uids at daemon start) BEFORE the `rdr` rule (FR-4.5 enforcement).
- **AC1.8** — pf rule shape excludes daemon's own uid (FR-1.5) and loopback (FR-1.6).
- **AC1.9** — `go test -cover -count=1` on `pfintercept/pfrules` reports `100.0% of statements`.
- **AC1.10** — `scripts/check-go-coverage.sh --strict-allowlist` returns zero entries that can be removed from `pfintercept/pfrules` (the only allowlist entry is the cgo/exec glue, which is justified).
- **AC1.11** — On daemon panic (simulated via `kill -KILL`), launchd respawns within ≤5s and the next `Controller.Apply` first call is `Flush` (the pre-install hook from T1.6), so any stale rules from the crashed predecessor are wiped before new rules load. Verified by S4 gap-closure stress test.
- **AC1.12** — Story S9 (doc lockstep) is updated in the same PR: `agent-macos-platform-architecture.md` gains the pf-rule-shape description, the recovery command (`sudo pfctl -a nexus-agent/transparent -F all`), and a cross-reference to this story.

---

## 4. Interface contract

The package exports exactly three identifiers (per CLAUDE.md "less is more" — minimum admin-facing surface):

```go
// RuleSet is the value object input. Constructed by Story S5 (wiring) from
// per-daemon config + agent_settings shadow.
type RuleSet struct { … }

// Installer is the cgo/exec seam — mockable for tests per DEC-008.
type Installer interface { Install(…); Flush(…); Show(…) }

// Controller orchestrates RuleSet × Installer with idempotency + concurrency
// safety. Constructed once per daemon process.
type Controller struct { … }
func New(Installer, anchor, *slog.Logger) *Controller
func (*Controller) Apply(ctx, RuleSet) error
func (*Controller) Remove(ctx) error
func (*Controller) Snapshot() RuleSet
```

Consumers:

- **Story S5 wiring** constructs a `pfrules.Controller` once per daemon (when `interceptMode="pf"`); calls `Apply` on each `agent_settings` shadow update and `Remove` on graceful exit.
- **Story S7 gap-closure tests** use `Controller.Snapshot()` for assertion shape (verify the rule installed matches the rule expected).
- **Story S4 fail-open invariants** verifies the `Apply→Flush-first` sequencing in tests.

**Non-consumers** — these packages MUST NOT import `pfrules`:

- `packages/shared/**` — pf rules are darwin-specific intercept boundary; per DEC-013 the boundary stays in `packages/agent/internal/platform/darwin/`.
- `packages/compliance-proxy/**` — CP listens on a TCP port; it has no kernel-rules layer.
- `packages/agent/internal/network/**` — proxy / relay code consumes the listener's accepted connections; it does not know about pf rules.

Verification: `grep -RIl 'pfintercept/pfrules' packages/` returns only `packages/agent/internal/platform/darwin/**` files.

---

## 5. Dependencies

**Upstream (blocks this story)**: none.

**Downstream (this story blocks)**:

- **S2 (listener)** — listener is what `rdr` rules redirect TO; without S1's rules installed, S2's listener accepts nothing.
- **S4 (fail-open)** — verifies S1's defensive pass-rule shape (system daemons untouched) + idempotent Apply / Remove sequencing.
- **S5 (wiring)** — constructs `pfrules.Controller` + invokes Apply on mode flip / agent_settings update + invokes Remove on shutdown.
- **S6 (build)** — pf-only `.pkg` runs `pfctl --version` as a postinstall sanity check.
- **S7 (gap-closure tests)** — uses `Snapshot()` for rule-shape assertions.
- **S9 (doc lockstep)** — `agent-macos-platform-architecture.md` updated to describe the rule shape.

---

## 6. Out of scope

- pf rules for non-TCP/443 (and non-UDP/443) traffic. The catchall `rdr` covers TCP 443 only; future epics MAY add TCP 80 (already toggle-supported via `RedirectTCP80`) or domain-IP-specific rules (DEC-002 deferred to "could / future").
- Per-host or per-bundle granularity in pf — DEC-002 places that logic in `domain.Engine` (shared with CP), not in pf.
- pf rule introspection in admin UI — Requirements §7 "Could" — deferred.
- IPv6 redirect rules — current scope is IPv4 (`inet`). IPv6 `rdr` rules in pf require slightly different syntax; deferred to a follow-up story (recorded as Code-phase open question if encountered).
- A separate `nexus-agent-pfclean` sibling LaunchDaemon — per DEC-009 rejected in favour of in-process + launchd `KeepAlive`.
- pf rules for the Linux platform — Linux already runs iptables NAT REDIRECT (`packages/agent/internal/platform/linux/`) per DEC-013.

---

## 7. References

- **Requirements**: `docs/developers/specs/e74-macos-pf-intercept.md` §FR-1.
- **Decisions**: `docs/developers/specs/e74/DECISIONS.md` DEC-001, DEC-002, DEC-005, DEC-006, DEC-008, DEC-009, DEC-011, DEC-013.
- **Linux structural analogue**: `packages/agent/internal/platform/linux/linux_linux.go` lines 1-60 (iptables-equivalent setup) — same shape, different kernel facility.
- **Compliance Proxy listener** (DEC-013 reuse boundary reference): `packages/compliance-proxy/internal/proxy/server/server.go` — CP listens on a TCP port directly; no kernel rules. The agent's pf path is the macOS-side equivalent of "getting bytes onto a listener socket".
- **CLAUDE.md bindings**: macOS NE proxy must fail-open (transferred to pf per FR-4), code/doc lockstep (S9), Test/skill env files (none introduced here), unit-test coverage ≥95% (this story targets 100% per user binding for critical paths).
- **Existing pfctl docs** (Apple): `man 8 pfctl`, `man 5 pf.conf`.
