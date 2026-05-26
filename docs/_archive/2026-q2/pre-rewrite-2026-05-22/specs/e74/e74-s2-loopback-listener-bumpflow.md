# E74-S2 — Loopback listener + SNI peek + `BumpFlow` handoff

> Epic: 74
> Story: 2
> Status: Planning (Step 3 SDD)
> Date: 2026-05-21
> FR mapping: FR-2.1 .. FR-2.7 (from `docs/developers/specs/e74-macos-pf-intercept.md`)
> Source decisions: DEC-002 (single-stack daemon-side decision), DEC-003 (DIOCNATLOOK seam), DEC-005 (port 13443), DEC-008 (interface seam for 100% coverage), DEC-012 (shared `domain.Engine`), DEC-013 (CP / Agent reuse boundary).
> Resolves Code-phase Open Questions: CODE-OQ-007 (UDP-blocked flow signal), CODE-OQ-008 (Prometheus metric names).
> Architecture impact: documented under Story S9.
> Dependencies: blocks S4 (fail-open invariants verify listener properties), S5 (wiring constructs listener), S7 (gap-closure tests exercise listener end-to-end), S9 (doc lockstep). Upstream: depends on S1 (rules redirect into listener) and S3 (libproc gives FlowProcess).

---

## 1. User story

As a macOS agent operator running `interceptMode="pf"`, I want a daemon-owned loopback listener that accepts every redirected flow, recovers the original destination via `DIOCNATLOOK`, peeks the TLS SNI, looks up the originating process, asks the shared `domain.Engine` for the decision, and hands inspect-mode flows directly to `tlsbump.BumpConnection` — without any code duplication relative to the Compliance Proxy path — so that the same content-aware hook pipeline that runs on the gateway runs on my Mac, with byte-identical NormalizedPayload semantics.

---

## 2. Tasks

### T2.1 — Create `pfintercept/listener/` sub-package skeleton

- Files: `packages/agent/internal/platform/darwin/pfintercept/listener/listener.go` (production), `listener_test.go`, `metrics.go` (Prometheus counters per CODE-OQ-008).
- Build tag: `//go:build darwin`.
- Per DEC-011 — listener concern is exactly: accept TCP conn from pf `rdr` → recover original dst → peek SNI → resolve PID → decide via shared engine → dispatch (BumpFlow / opaque relay / deny). No domain rules, no pf rule install (S1), no libproc cgo (S3 owns).

### T2.2 — Define `Listener` struct + `Config` value object

```go
type Config struct {
    Addr           string             // "127.0.0.1:13443" per DEC-005
    DaemonUID      uint32             // own-uid; used in self-intercept guard
    NATLooker      natlook.Resolver   // DEC-003 seam (S3-adjacent? — actually owned here; natlook is part of pfintercept/, in a separate sub-package per DEC-011)
    PIDLookup      pidlookup.Resolver // S3 seam
    DomainEngine   *domain.Engine     // SHARED — per DEC-012 / DEC-013, this is shared/policy/domain.Engine, NOT a copy
    BridgeDeps     proxy.BridgeDeps   // existing struct (bridge.go:34) — bumpflow consumes this
    Logger         *slog.Logger
    Metrics        *Metrics           // T2.3 below
    // Per-flow ceilings for fail-open + safety
    SNIPeekTimeout time.Duration      // default 500ms (FR-2.2, DEC-009 Rule 2 transfer)
    AcceptBacklog  int                // default 512, matches Linux maxConcurrentConns
}

type Listener struct {
    cfg      Config
    ln       net.Listener
    wg       sync.WaitGroup
    done     chan struct{}
    sem      chan struct{}
    stopOnce sync.Once
}

func New(Config) (*Listener, error)
func (*Listener) Start(ctx) error
func (*Listener) Stop(ctx) error
```

### T2.3 — Prometheus metrics (resolves CODE-OQ-008)

Listener owns exactly four metric families. Registered at `New()` time via the shared Prometheus registry (no listener-private registry — per DEC-013 metrics are shared infrastructure):

```go
type Metrics struct {
    FlowsAccepted        *prometheus.CounterVec  // {decision="inspect|passthrough"}  ← listener-layer only; per-path "deny" happens inside tlsbump
    UDPBlocked           *prometheus.CounterVec  // {bundle="…"} — populated by S1 pf-rule counter reader
    AcceptErrors         *prometheus.CounterVec  // {reason="natlook|sni_peek|domain_eval|bumpflow_dial"}
    NATLookErrors        *prometheus.CounterVec  // {stage="open|ioctl|parse"}
}
```

- Names: `nexus_agent_pf_flows_accepted_total`, `nexus_agent_pf_udp_blocked_total`, `nexus_agent_pf_listener_accept_errors_total`, `nexus_agent_pf_natlook_errors_total`.
- These names are **locked** by this story (per CODE-OQ-008 resolution); S7 tests assert against them.
- Per the existing Prometheus conventions in `packages/agent/internal/observability/` — read at Phase 4 and align.

### T2.4 — `handleConn` — the core flow handler

Sequence:

```go
func (l *Listener) handleConn(ctx context.Context, c net.Conn) {
    defer c.Close()
    tcpConn, ok := c.(*net.TCPConn)
    if !ok { return }
    
    // 1. Recover original destination via DIOCNATLOOK (DEC-003 seam)
    dstIP, dstPort, err := l.cfg.NATLooker.Resolve(tcpConn)
    if err != nil {
        l.cfg.Metrics.NATLookErrors.WithLabelValues(natlookStage(err)).Inc()
        l.cfg.Metrics.AcceptErrors.WithLabelValues("natlook").Inc()
        return  // fail-open: no rule, no flow, no audit. Synchronous Rule 1 transfer.
    }
    
    // 2. Peek SNI with 500ms timeout (FR-2.2 / Rule 2 transfer)
    sni, peeked, peekErr := proxy.PeekSNI(c, l.cfg.SNIPeekTimeout)
    dstHost := sni
    if dstHost == "" {
        dstHost = dstIP  // FR-2.2 fallback
    }
    if peekErr != nil {
        l.cfg.Metrics.AcceptErrors.WithLabelValues("sni_peek").Inc()
        // fall through with empty SNI — passthrough path can still serve the user
    }
    
    // 3. Resolve source PID via libproc seam (S3)
    srcAddr := c.RemoteAddr().(*net.TCPAddr)
    pid := l.cfg.PIDLookup.LookupPID(srcAddr.IP, srcAddr.Port, dstIP, dstPort)
    var procMeta proxy.FlowProcess
    if pid > 0 {
        if m, err := pidlookup.ResolveFlowProcess(pid); err == nil {
            procMeta = m
        }
    }
    
    // 4. SELF-INTERCEPT GUARD: if uid matches DaemonUID, drop early.
    //    This is the structural equivalent of TransparentProxyProvider.swift:151-167.
    //    Listener already excluded by pf own-uid pass rule (FR-1.5), but defence in depth.
    
    // 5. Shared domain.Engine HOST-level decision — DEC-012 binding.
    //    NOTE: this is `packages/shared/policy/domain.Engine` — the SAME pointer
    //    that CP wires into its forwarder. No agent-private decision logic.
    //    The engine's real API has TWO layers:
    //       - MatchHost(host) → *InterceptionDomain (or nil)
    //       - PathAction(domain, path) → PathAction
    //    Listener sees only host+port (no HTTP path); it makes the
    //    host-level decision here. Path-level decision (PROCESS /
    //    PASSTHROUGH / DENY per-path) happens INSIDE BumpFlow →
    //    tlsbump.BumpConnection after HTTP parse. The listener's job
    //    ends at "host is in inspect list → inspect; otherwise → passthrough".
    flowID := makeFlowID(srcAddr, dstIP, dstPort)
    interceptDomain := l.cfg.DomainEngine.MatchHost(dstHost)
    
    if interceptDomain != nil {
        // Host matches an interception rule → inspect path.
        // tlsbump will run domain.Engine.PathAction(interceptDomain, parsedPath)
        // per HTTP request inside the TLS-bumped connection; that decision
        // chooses PROCESS (run hooks) vs PASSTHROUGH (forward verbatim) vs
        // DENY (return 403) per path.
        l.cfg.Metrics.FlowsAccepted.WithLabelValues("inspect").Inc()
        if err := proxy.BumpFlow(ctx, c, peeked, dstHost, dstPort, flowID, procMeta, l.cfg.BridgeDeps); err != nil {
            l.cfg.Metrics.AcceptErrors.WithLabelValues("bumpflow_dial").Inc()
            // BumpFlow already logs internally
        }
        return
    }
    
    // Host is NOT in the inspect set → opaque passthrough.
    l.cfg.Metrics.FlowsAccepted.WithLabelValues("passthrough").Inc()
    l.doPassthrough(ctx, c, dstIP, dstPort, peeked)
}
```

Key reuse points (per DEC-013):

- `proxy.BumpFlow` (existing — `packages/agent/internal/network/proxy/bridge.go:153`) is the **same** entry that the NE bridge uses today. Listener does NOT inline a parallel MITM path.
- `proxy.PeekSNI` is the shared helper (already exists for the Linux platform per `linux_linux.go:247`).
- `domain.Engine` is `packages/shared/policy/domain.Engine` — shared with CP.
- `proxy.FlowProcess` is the existing `bridge.go:123-127` value type.

### T2.5 — Passthrough path (`doPassthrough`)

- Direct TCP dial to `dstIP:dstPort` (no upstream marking like Linux's `SO_MARK` — macOS has no kernel SO_MARK equivalent; loopback exclusion in pf (FR-1.6) is sufficient to break the loop).
- Replay peeked bytes first, then bidirectional `proxy.Relay`.
- Same shape as `linux_linux.go:346-366` — verbatim adaptation.

### T2.6 — `natlook.Resolver` seam — DIOCNATLOOK ioctl on `/dev/pf`

Per DEC-003 + DEC-008 + DEC-011 — owned by sibling sub-package `pfintercept/natlook/`.

Interface:

```go
type Resolver interface {
    Resolve(*net.TCPConn) (dstIP string, dstPort int, err error)
}
```

Implementation strategy:

- `RealResolver` opens `/dev/pf` once at construction (`O_RDWR`, root required) — cached file descriptor. Each `Resolve` issues one ioctl `DIOCNATLOOK` with a populated `pfioc_natlook` struct, parses the `rdaddr` / `rdport` fields, returns. Total cgo glue ≤80 LOC including struct mirroring.
- `MockResolver` is a struct-table; tests inject one keyed by source-port → dstIP/dstPort.

`.coverage-allowlist` entry per DEC-008: `pfintercept/natlook:resolver_darwin.go  # category D — OS-bound (requires root + /dev/pf)`.

### T2.7 — Concurrency + lifecycle

- `Start(ctx)`: bind on `cfg.Addr`; spawn accept loop in `wg.Add(1)`-tracked goroutine.
- Per accepted conn: acquire `sem`, spawn `wg.Add(1)` handler goroutine.
- `Stop(ctx)` (called from S5 wiring on shutdown): `stopOnce.Do(close(done))`; close `ln`; wait for `wg` with 10s timeout (same as Linux platform `Stop`); log timeout if exceeded.
- All concurrency state goes through `sync.Mutex` if any (currently none required — accept loop owns ln, handlers own their conn).

### T2.8 — UDP-blocked flow signal (resolves CODE-OQ-007)

- UDP/443 redirect rules from S1 are BLOCK rules (pf `block drop`), not `rdr`. **No listener connection is ever opened for UDP-blocked flows.** Therefore: no `traffic_event` row, no listener path, no goroutine cost.
- Visibility: the pf-rule-hit counter (read via `pfctl -a … -sr -v -v` parser on a sidecar goroutine) feeds the `nexus_agent_pf_udp_blocked_total{bundle}` counter. This is the **only** observability surface for UDP blocks.
- This resolves CODE-OQ-007. S7 amends its Gap-2 assertion to "Prometheus delta on `udp_blocked_total` matches expected count, AND Chrome's subsequent TCP/443 flow IS captured in `traffic_event`".

### T2.9 — Self-intercept guard (defence-in-depth)

- Mirrors `TransparentProxyProvider.swift:151-167` daemon-PID exclusion.
- Implementation: on every `handleConn`, after libproc PID resolution, compare resolved uid to `cfg.DaemonUID`. If match, increment a debug counter and return. The pf own-uid exclusion (FR-1.5) is the primary defense; this is belt-and-braces.

### T2.10 — Reported state stamp (resolves CODE-OQ-009)

- At `Start(ctx)` and on every successful pf rule reload, listener calls `reportedstate.Set("agent_settings.interceptMode", "pf")` (or whatever the reporting helper exposes; resolve exact API in Phase 4 by reading `packages/agent/internal/sync/`). On `Stop(ctx)`, sets `""` (empty = "not running").
- S8's "currently applied" UI indicator reads from this same Hub reported-state path.

### T2.11 — Unit tests — 100% logic coverage per DEC-008

Test fixtures use `MockResolver` (NATLOOK) + `MockPIDLookup` (libproc) + an in-memory `domain.Engine` constructed from synthetic rules.

- **Table-driven `handleConn` tests**, 12+ scenarios covering: NATLOOK error → metrics inc, fail-open return; SNI present → host = SNI; SNI absent → host = dstIP; PID resolved → procMeta populated; PID resolution fails → procMeta empty + dispatch still proceeds; decision = inspect → BumpFlow called with right args; decision = passthrough → doPassthrough called; decision = deny → conn closed; self-intercept (uid = daemon) → early return.
- **`doPassthrough` tests** using `net.Pipe` for both sides — verify peeked bytes replayed first; verify Relay called.
- **Concurrency** — Start + accept N=100 conns in parallel + Stop under `-race`.
- **Backpressure** — sem full, accepted conn waits for slot, no leak.
- **Metric assertions** — every counter increments on the right path; no double-counting; labels match the locked names from T2.3.
- **Lifecycle** — Stop with active flows; flows complete within 10s; Stop again is no-op.
- Target: `go test -cover -count=1 ./packages/agent/internal/platform/darwin/pfintercept/listener/...` → `100.0% of statements`.

### T2.12 — Coverage gate

- Add to `scripts/.coverage-allowlist` only the cgo-glue paths (`natlook/resolver_darwin.go`). Listener.go itself is mockable end-to-end and must reach 100%.

---

## 3. Acceptance criteria

- **AC2.1** — `Listener.Start` binds successfully on `127.0.0.1:13443` (default); returns clear error on bind failure (port in use).
- **AC2.2** — On a redirected connection (mocked via `net.Pipe` + MockResolver), `handleConn` calls `domain.Engine.MatchHost(dstHost)` exactly once with the correctly-recovered (dstHost) and dispatches based on whether the result is nil (passthrough) or non-nil (inspect).
- **AC2.3** — `MatchHost ≠ nil` → `proxy.BumpFlow` called with the same `BridgeDeps` struct the daemon constructed (DEC-013 reuse — same struct, same shared pipeline). Per-path PROCESS/PASSTHROUGH/DENY happens inside tlsbump after HTTP parse — that path is covered by existing tlsbump tests, not by this story.
- **AC2.4** — `MatchHost = nil` → opaque relay (passthrough); client receives upstream bytes; peeked bytes replayed exactly once.
- **AC2.5** — REMOVED — listener does not emit per-path DENY; that decision lives inside tlsbump. The listener-layer outcomes are exactly two: inspect (non-nil host match) or passthrough (nil host match).
- **AC2.6** — SNI peek bounded by 500 ms; on timeout, fall through with `dstHost=dstIP` (Rule 2 transfer / FR-2.2).
- **AC2.7** — NATLOOK failure: metric increments, fail-open return (no panic, no hang).
- **AC2.8** — Self-intercept guard: when libproc returns daemon uid, handler returns without dialing upstream.
- **AC2.9** — `Stop(ctx)` completes within 10 s on a clean shutdown; outstanding flows drain.
- **AC2.10** — Concurrent 100-conn accept + Stop under `-race` — no race detected.
- **AC2.11** — `go test -cover` on `pfintercept/listener` reports `100.0%`; the only `.coverage-allowlist` entry pertaining to listener code is `pfintercept/natlook/resolver_darwin.go`.
- **AC2.12** — All four metric families (T2.3) registered and observably incrementing on the right code paths.
- **AC2.13** — Per DEC-013 reuse: `grep 'packages/shared/policy/domain' packages/agent/internal/platform/darwin/pfintercept/listener/listener.go` returns ≥1 hit; `grep 'type Engine' packages/agent/internal/platform/darwin/pfintercept/` returns ZERO hits (no agent-private engine).
- **AC2.14** — Per DEC-013 reuse: `proxy.BumpFlow` is the dispatch entry for inspect-mode flows; `grep 'tls\.Server\|tls\.Conn' packages/agent/internal/platform/darwin/pfintercept/listener/` returns ZERO hits (no parallel MITM stack).
- **AC2.15** — Reported state field `agent_settings.interceptMode` is stamped on Start, cleared on Stop. S8's UI indicator reads this path.

---

## 4. Interface contract

Exported identifiers from `pfintercept/listener`:

```go
type Config struct { ... }
type Metrics struct { ... }
type Listener struct { ... }

func New(Config) (*Listener, error)
func (*Listener) Start(ctx) error
func (*Listener) Stop(ctx) error
func (*Listener) Addr() string
```

Exported from `pfintercept/natlook`:

```go
type Resolver interface { Resolve(*net.TCPConn) (string, int, error) }
type RealResolver struct { ... }
func NewRealResolver() (*RealResolver, error)
```

**Consumed by**: only S5 (wiring) constructs `Listener`. Listener consumes (a) `domain.Engine` from shared/policy/domain, (b) `proxy.BumpFlow` from agent/internal/network/proxy, (c) `proxy.PeekSNI` + `proxy.Relay` shared helpers, (d) `pidlookup.ResolveFlowProcess` from S3.

**Non-consumers**:
- `packages/shared/**` — listener is darwin-specific intercept boundary per DEC-013. Shared code consumes listener only via the `BridgeDeps` interface that's already used by the NE bridge.
- `packages/compliance-proxy/**` — CP listens on its own port; has no use for this listener.

---

## 5. Dependencies

**Upstream (blocks this story)**:

- **S1** — pf `rdr` rules redirect TO this listener; without S1, listener accepts nothing.
- **S3** — `pidlookup.Resolver` interface + impl; without S3, FlowProcess fields are empty.

**Downstream (this story blocks)**:

- **S4** (fail-open) — verifies synchronous-decision, 500 ms SNI timeout, self-intercept guard, defensive metrics emission.
- **S5** (wiring) — constructs `Listener`; injects shared `domain.Engine`, `BridgeDeps`, `natlook` + `pidlookup` resolvers; calls `Start` / `Stop` on mode flip.
- **S7** (gap-closure tests) — asserts against locked metric names from T2.3; verifies cross-service consistency (DEC-012/013) by exercising the same `interception_domain` rule via listener and via CP listener and observing identical decisions.
- **S8** (admin UI) — reads `agent_settings.interceptMode` reported-state path T2.10 stamps.
- **S9** (doc lockstep) — documents listener position in macOS arch doc + "Relationship to Compliance Proxy" section.

---

## 6. Out of scope

- Performance benchmarking — correctness + 100% coverage only; latency budget is FR-7.4 observability (S7), not gated here.
- IPv6 redirect handling — initial scope is IPv4 (`inet`). IPv6 `rdr` rules require different pf syntax (S1 deferred); listener already handles `srcAddr` as `*net.TCPAddr` which supports both, so no listener changes needed when S1 adds IPv6.
- HTTP/3 detection — pf blocks UDP/443 at the firewall layer; listener never sees an H3 attempt by the time it reaches here.
- Cross-platform extraction — DEC-013 places the intercept boundary in `packages/agent/internal/platform/darwin/pfintercept/`. The listener is darwin-only; Linux has its own listener in `platform/linux/` (`linux_linux.go`).
- HTTP/HTTPS path inspection inside the listener — that's `tlsbump.BumpConnection`'s job (shared); listener only routes bytes.
- Per-flow audit emission — handled by `tlsbump`'s `pipeline.AuditEmitter` per existing contract; listener doesn't touch audit.

---

## 7. References

- **Requirements**: `docs/developers/specs/e74-macos-pf-intercept.md` §FR-2 + §FR-4 (fail-open transfer).
- **Decisions**: `docs/developers/specs/e74/DECISIONS.md` DEC-002, DEC-003, DEC-005, DEC-008, DEC-012, DEC-013; resolves CODE-OQ-007 + CODE-OQ-008.
- **Reuse source — Linux structural analogue**: `packages/agent/internal/platform/linux/linux_linux.go` lines 230-275 (`handleConn`), 240-244 (getOriginalDst), 246-251 (SNI peek), 253-258 (PID resolve), 261-275 (InterceptedConn → decision → dispatch). The macOS listener is functionally identical except: NATLOOK ioctl replaces `getsockopt(SO_ORIGINAL_DST)`, libproc replaces `/proc` parsing.
- **Reuse source — bridge entry**: `packages/agent/internal/network/proxy/bridge.go:153` (`BumpFlow`).
- **Reuse source — shared domain.Engine**: `packages/shared/policy/domain/engine.go`.
- **Reuse source — CP listener (for symmetry verification)**: `packages/compliance-proxy/internal/proxy/server/server.go` — CP also calls `domain.Engine.Evaluate` then dispatches; the agent listener and CP server differ only at the byte-source layer (DEC-013 binding).
- **Existing NE bridge contract**: `packages/agent/cmd/agent/wiring/bridge.go` (Story S5 will compose listener in parallel with this).
- **CLAUDE.md**: macOS NE proxy must fail-open (rules transferred per FR-4), unit-test coverage ≥95% (user binding 100% for this critical path), code/doc lockstep (S9).
