# E74-S5 — `interceptMode` wiring + agent_settings integration

> Epic: 74
> Story: 5
> Status: Planning (Step 3 SDD)
> Date: 2026-05-21
> FR mapping: FR-5.1 .. FR-5.6 (from `docs/developers/specs/e74-macos-pf-intercept.md`)
> Source decisions: DEC-001 (enum `ne | pf`, hybrid retired), DEC-005 (port default), DEC-007 (build-stamped default), DEC-009 (panic auto-removal sequencing), DEC-012 (shared `domain.Engine`), DEC-013 (CP/Agent reuse boundary).
> Resolves Code-phase Open Questions: CODE-OQ-009 (reported-state field path).
> Architecture impact: documented under Story S9.
> Dependencies: blocks S6 (build stamps ldflag this story reads), S8 (admin UI consumes shadow key); upstream depends on S1 (Controller), S2 (Listener), S3 (libproc seam), S4 (fail-open signal handlers).

---

## 1. User story

As a macOS agent operator, I want a single `interceptMode` shadow key whose value (`ne` or `pf`) deterministically selects which interception stack the daemon runs, with reversibility (flip back to `ne` removes pf rules cleanly) and an audit row on every change, so that I can roll out pf with confidence, gather telemetry, and recover quickly if the new path misbehaves.

---

## 2. Tasks

### T5.1 — Extend `agent_settings` shadow blob with `interceptMode`

- **File**: `packages/agent/cmd/agent/configappliers.go` lines 87-105 (current `agent_settings` decode struct).
- Add field:
  ```go
  InterceptMode  string  `json:"interceptMode"`  // "ne" | "pf"  per DEC-001
  ```
- Schema validation: enum check at decode time. Unknown value (e.g. `"hybrid"`) is rejected with a structured log line and **does NOT crash** the daemon — falls back to the build-stamped default (DEC-007) and logs a warning.
- Add to the `remoteOverlay` block so the value flows into the daemon's runtime config snapshot.
- Add to `packages/agent/internal/sync/schema/config.go` `AgentSettings` struct (line 70 region — joins `ForceQUICFallbackBundles`); apply same yaml-tag conventions.

### T5.2 — Build-stamped default via Go ldflag (DEC-007)

- **File**: `packages/agent/cmd/agent/main.go`.
- Add package-level variable:
  ```go
  var defaultInterceptMode = "ne"  // overridden by ldflag in pf-only build
  ```
- Story S6's `build-agent --target=pf-only` builds with `-ldflags "-X 'main.defaultInterceptMode=pf'"`.
- On daemon start, if shadow blob is empty / shadow not yet pulled, use `defaultInterceptMode` as the bootstrap value. As soon as the first shadow pull lands, the shadow value wins.

### T5.3 — Mode resolver — single source of truth

- **New file**: `packages/agent/internal/platform/darwin/pfintercept/mode.go`.
- Defines:
  ```go
  type Mode string
  const (
      ModeNE Mode = "ne"
      ModePF Mode = "pf"
  )
  
  func Parse(s string) (Mode, error)  // accepts "ne" / "pf" only; "hybrid" → error
  ```
- Used everywhere that reads the runtime mode — no string literal `"pf"` or `"ne"` anywhere else in pfintercept code.

### T5.4 — Mode dispatcher in daemon wiring

- **File**: extend `packages/agent/cmd/agent/wiring/network.go` (`InitPlatform`).
- Current behaviour: `return platform.NewPlatform(bridgeAddr, relayClient)` (line 31). On darwin this always constructs the NE-bridge platform.
- New behaviour:
  ```go
  func InitPlatform(cfg PlatformConfig) platform.Platform {
      switch cfg.InterceptMode {
      case "pf":
          return platform.NewPFPlatform(cfg)  // new constructor on darwin only
      case "ne":
          return platform.NewPlatform(cfg.BridgeAddr, cfg.RelayClient)
      default:
          // Unknown / empty — fall back to build-stamped default.
          if defaultInterceptMode == "pf" {
              return platform.NewPFPlatform(cfg)
          }
          return platform.NewPlatform(cfg.BridgeAddr, cfg.RelayClient)
      }
  }
  ```
- New struct `PlatformConfig` aggregates: `BridgeAddr`, `RelayClient`, `InterceptMode`, `ListenerAddr`, `DomainEngine`, `BridgeDeps`, `Logger`, `Metrics`. Constructed once per daemon main; passed in.

### T5.5 — `platform.NewPFPlatform` — darwin-only constructor

- **File**: `packages/agent/internal/platform/darwin/platform_pf_darwin.go` (NEW).
- Build tag: `//go:build darwin`.
- Constructs:
  1. `natlook.NewRealResolver()` — DEC-003 cgo seam (Story S2's adjacent sub-package).
  2. `pidlookup.NewRealResolver()` — Story S3's libproc seam.
  3. `pfrules.New(installer, "nexus-agent/transparent", logger)` — Story S1's Controller.
  4. `pfrules.RuleSet{…}` derived from current `agent_settings` snapshot (daemon uid, bundle-list-resolved quicFallbackUIDs per DEC-004, system-daemon uids).
  5. `listener.New(listener.Config{… Addr: cfg.ListenerAddr (default 13443 per DEC-005), DomainEngine: cfg.DomainEngine, BridgeDeps: cfg.BridgeDeps, NATLooker, PIDLookup, Logger, Metrics, SNIPeekTimeout: 500*time.Millisecond …})`.
- Returns a `platform.Platform`-conforming struct (`PFPlatform`) that satisfies the existing interface (`api.Platform` — `Start`, `Stop`, `InterceptionMode`).
- `InterceptionMode()` returns a new enum value `api.ModePF` (alongside existing `api.ModeIPTables` for Linux and `api.ModeNetworkExtension` for darwin NE).

### T5.6 — Mode change handling at runtime (live flip)

- When `agent_settings` shadow pushes a NEW `interceptMode` value (different from current):
  1. Emit audit row of kind `agent_intercept_mode_change` carrying `{oldMode, newMode, requestedAt, source: "shadow_push"}`.
  2. Call `currentPlatform.Stop(ctx)` — clean teardown (NE → unloads system extension activation? OR pf → `pfrules.Controller.Remove`).
  3. Reconstruct via `InitPlatform(cfg with new mode)`.
  4. Call `newPlatform.Start(ctx, handler)`.
  5. Stamp reported state `agent_settings.interceptMode` = newMode (resolves CODE-OQ-009).
  6. If any step fails: revert to oldMode + emit `agent_intercept_mode_change_failed` audit row.

### T5.7 — Reported state symmetry (resolves CODE-OQ-009)

- Reported-state path: `agent_settings.interceptMode` — symmetric with desired-state path.
- Hub-side reading code: existing `system_metadata` reader path that S8's admin UI uses.
- Write API: `packages/agent/internal/sync/reportedstate/` (resolve exact API at Phase 4; default helper signature `reportedstate.SetField(path, value)`).
- Stamped:
  - On Start: with the currently-applied mode.
  - On Stop (graceful): cleared to empty string (indicates "not running").
  - On Stop (panic) — not stamped (daemon is dead). Hub infers "stale" from missing recent heartbeat.

### T5.8 — Signal handler registration (DEC-009)

- **File**: `packages/agent/cmd/agent/cmd_run.go`.
- On startup:
  1. Register `signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)`.
  2. On signal: cancel daemon ctx → triggers `currentPlatform.Stop(ctx)` → which for pfPlatform calls `pfrules.Controller.Remove(ctx)` → clears pf anchor.
  3. The Go-side `defer` clause is the **fast path** — sub-second cleanup (Rule 1 + Rule 2 transfer per S4 invariants).
- Coordinate with launchd `KeepAlive=true` + 5s `ThrottleInterval` (S6 ships the plist) for the slow-path coverage (kill -9).

### T5.9 — Daemon-start pre-install cleanup hook (DEC-009 part B)

- **File**: `packages/agent/cmd/agent/cmd_run.go` startup sequence.
- Before constructing the platform (whether pf or NE), call a one-shot `pfrules.NewRealInstaller().Flush(ctx, "nexus-agent/transparent")` BEST-EFFORT — log on failure, continue.
- Rationale: if previous daemon process crashed and left stale anchor rules, this guarantees a clean state before new rules load (per S1 T1.6 + DEC-009 sequencing).
- Even when mode is `ne` (no pf rules being installed this run), still flush — defence against a previous pf run that didn't unwind.

### T5.10 — Bundle→UID resolution at config load (DEC-004 — eager)

- **File**: `packages/agent/cmd/agent/platformshim/quic_fallback_darwin.go` (extend existing).
- Existing: takes bundle list, writes `/var/run/nexus-agent/quic-bundles.json` (NE Swift reads this).
- New (additive, NE backwards-compatible): in parallel, resolve each bundle to its installed uid set via cgo `LSCopyApplicationURLsForBundleIdentifier` → file `stat(2)` → uid. Produces `[]uint32` for the pf path's `RuleSet.QuicFallbackUIDs`.
- Bundle-list-to-UID resolution interface in `pfintercept/bundleresolve/` sub-package (DEC-011 layout); allowlisted cgo glue.

### T5.11 — Audit emission for mode changes

- Audit row shape: kind `agent_intercept_mode_change`, payload `{oldMode, newMode, requestedAt, appliedAt, success}`.
- Visible in admin UI audit log via existing `auditqueue` → Hub `audit_event` table.
- S8 lists this kind in its "Audit columns" reference table.

### T5.12 — Unit tests — 100% logic coverage on wiring

- `mode.Parse` table-driven: 6 cases including unknown, empty, mixed case, `"hybrid"`-rejected.
- `InitPlatform` dispatch table-driven across `(mode value × default value × expected platform type)` — 8+ rows.
- `agent_intercept_mode_change` flow tested with a fake platform that records Start/Stop call sequence; verify on flip, sequence is Stop → reconstruct → Start; verify audit emitted; verify reported-state stamped.
- Failure recovery: when new platform Start fails, verify revert to old + failed audit row.
- 100% statement coverage. Target: `go test -cover -count=1 ./packages/agent/cmd/agent/wiring/...` (existing tests + new wiring tests).

---

## 3. Acceptance criteria

- **AC5.1** — `agent_settings.interceptMode` decoded from shadow blob; enum-validated to `{ne, pf}`; unknown values logged as warning, fall back to build-stamped default.
- **AC5.2** — Build-stamped default flips with ldflag `-X 'main.defaultInterceptMode=pf'`. Verified by inspecting two binaries.
- **AC5.3** — `InitPlatform` returns `PFPlatform` when mode=`pf`; returns existing `DarwinPlatform` when mode=`ne`. Round-trip with empty mode + each build default produces the right type.
- **AC5.4** — Live flip (shadow pushes new value): old platform `Stop`s, new platform `Start`s, audit row emitted, reported state stamped. End-to-end ≤30 s wall-clock per FR-5.3 reversibility.
- **AC5.5** — Failure during flip (Start returns error): revert to oldMode + failed-audit row + reported state preserved as old value.
- **AC5.6** — Signal handler runs `pfrules.Controller.Remove` on SIGTERM/SIGINT before daemon process exits.
- **AC5.7** — Daemon-start pre-flush runs `pfctl -F` on the anchor even when starting in `ne` mode (defence against previous pf run that crashed).
- **AC5.8** — Mode resolver: only the `mode` package contains string literals `"ne"` and `"pf"`; grep returns 1 file outside `mode.go`/tests (the `configappliers.go` JSON tag is the exception, which is data not code-flow).
- **AC5.9** — Reported state `agent_settings.interceptMode` matches the currently-running platform after Start; cleared after Stop.
- **AC5.10** — Bundle→UID resolution runs at config-load (eager per DEC-004); resulting `QuicFallbackUIDs` slice is non-empty when `ForceQUICFallbackBundles` was non-empty AND those bundles are installed on the host.
- **AC5.11** — Audit row of kind `agent_intercept_mode_change` visible in `audit_event` table after each flip.
- **AC5.12** — `go test -cover` on `cmd/agent/wiring` reports 100% statement coverage for the new code paths (existing coverage unaffected or improved).
- **AC5.13** — Reuse verification per DEC-013: `InitPlatform` does NOT construct its own `domain.Engine` — the engine pointer is **passed in** via `cfg.DomainEngine` from the daemon's shared wiring (which already provides the same pointer to the NE bridge). `grep 'domain.NewEngine' packages/agent/cmd/agent/` returns ZERO new occurrences from this story.

---

## 4. Interface contract

```go
// In packages/agent/internal/platform/darwin/pfintercept/mode.go
type Mode string
const ( ModeNE Mode = "ne"; ModePF Mode = "pf" )
func Parse(string) (Mode, error)

// In packages/agent/cmd/agent/wiring/network.go (extended)
type PlatformConfig struct {
    BridgeAddr     string  // existing NE bridge addr
    ListenerAddr   string  // pf listener addr (default 127.0.0.1:13443)
    InterceptMode  string  // "ne" | "pf" (empty → build-default)
    RelayClient    *relay.Client
    DomainEngine   *domain.Engine  // SHARED — per DEC-012/013
    BridgeDeps     proxy.BridgeDeps
    Logger         *slog.Logger
    Metrics        *listener.Metrics
}
func InitPlatform(PlatformConfig) platform.Platform
```

Consumed by: daemon `cmd/agent/main.go` constructs once, passes to wiring.

---

## 5. Dependencies

**Upstream**:
- **S1** — `pfrules.Controller` + `RealInstaller`.
- **S2** — `listener.New` + `listener.Config` + `listener.Metrics`.
- **S3** — `pidlookup.Resolver` + `NewRealResolver`.
- **S4** — fail-open invariants (this story wires the signal handlers S4 specifies).

**Downstream**:
- **S6** — `build-agent --target=pf-only` injects the ldflag this story reads at startup.
- **S7** — gap-closure tests use this wiring to flip mode mid-test.
- **S8** — admin UI consumes the shadow key + reported state.
- **S9** — doc lockstep documents this wiring in the macOS arch doc.

---

## 6. Out of scope

- Per-host or per-bundle interceptMode override (FR-8.1 — single global dial per Agent Thing).
- Migration of in-flight NE flows to pf at the moment of the flip (FR-5.3 says "the flip drops in-flight inspect-mode flows when the platform stops; user-visible impact is at most one retried HTTP request per IDE / browser session — acceptable per planning session"). New flows after the flip use the new mode.
- `hybrid` mode plumbing (DEC-001 retired the hybrid value).
- Dev / test mode where the daemon doesn't actually invoke pfctl (covered via mock Installer in tests).
- Cross-platform InterceptMode dispatch (Linux + Windows have their own intercept stacks; mode field is darwin-only; Linux / Windows wiring ignores the field).

---

## 7. References

- **Requirements**: `docs/developers/specs/e74-macos-pf-intercept.md` §FR-5.
- **Decisions**: `docs/developers/specs/e74/DECISIONS.md` DEC-001, DEC-005, DEC-007, DEC-009, DEC-012, DEC-013; resolves CODE-OQ-009.
- **Existing wiring**: `packages/agent/cmd/agent/wiring/network.go` (InitPlatform), `packages/agent/cmd/agent/configappliers.go` lines 87-145 (agent_settings decode), `packages/agent/cmd/agent/cmd_run.go` (signal handling, lifecycle).
- **Reuse source — Linux platform constructor**: `packages/agent/internal/platform/linux/linux_linux.go` lines 60-78 (`NewPlatform`). Pattern: constructor returns interface-conforming struct, platform handles its own lifecycle.
- **Existing build-stamped flags pattern**: search `packages/agent/cmd/agent/main.go` for existing `var version = ...` flag injection — same idiom.
- **CLAUDE.md**: Thing config pull-only model (interceptMode is a Cat B shadow key per the pull-model binding); code/doc lockstep (S9); IAM impact review (S8 covers — no new IAM action).
