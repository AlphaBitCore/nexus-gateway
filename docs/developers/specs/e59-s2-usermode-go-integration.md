# E59-S2 — User-Mode Go Integration

**Status:** Design — pending implementation
**Date:** 2026-05-24
**Epic:** [E59](e59-windows-wfp-migration.md)
**Architecture:** [agent-windows-wfp-driver.md](../architecture/agent-windows-wfp-driver.md)
**Depends on:** E59-S1 (driver skeleton with frozen IOCTL contract)

---

## 1. User story

> **As** the agent Windows runtime team
> **I want** the Go user-mode side to drive the NexusWFP driver via
> `DeviceIoControl` — including HELLO handshake, policy push, audit
> pump, and original-destination lookup
> **so that** the WinDivert code path (`windivert_windows.go` and
> `imgk/divert-go` import) can be deleted in this same PR, leaving a
> single Windows interception code path that is cross-arch and
> WinDivert-free.

## 2. Goal

Land a Go user-mode WFP client at
`packages/agent/internal/platform/windows/wfp_*.go` that:

1. Implements the architecture §6 IOCTL contract.
2. Drives an inverted-call audit pump that survives driver disconnect
   (driver Unload, agent reload) without leaking IRPs or goroutines.
3. Replaces every WinDivert call site enumerated in §4.
4. Compiles under `GOOS=windows GOARCH={amd64,arm64}` (the latter
   becomes meaningful after E59-S4).
5. Passes `go test -race -count=1 ./packages/agent/internal/platform/windows/...`
   on the host where the driver is loadable.

## 3. Files

### 3.1 New files

| Path | Purpose |
|---|---|
| `packages/agent/internal/platform/windows/wfp_windows.go` | Main WFP client — opens `\\??\\NexusWFP`, holds the device handle, dispatches IOCTLs |
| `packages/agent/internal/platform/windows/wfp_flowtable.go` | Per-source-port → original-destination lookup, populated by audit-pump and read by GetOrigDst |
| `packages/agent/internal/platform/windows/wfp_audit_pump.go` | Inverted-call IRP pump goroutine — keeps N outstanding `IOCTL_NEXUS_WFP_AUDIT_PUMP` IRPs, drains completions into the FlowTable + audit channel |
| `packages/agent/internal/platform/windows/wfp_policy.go` | Marshalling for the policy wire format (architecture §7) |
| `packages/agent/internal/platform/windows/wfp_ioctl.go` | Low-level wrappers around `windows.DeviceIoControl` with the five IOCTL codes |
| `packages/agent/internal/platform/windows/wfp_windows_test.go` | Race-safe unit tests using a mock device |

### 3.2 Files deleted

| Path | Why |
|---|---|
| `packages/agent/internal/platform/windows/windivert_windows.go` | Replaced |
| `packages/agent/internal/platform/windows/windivert_windows_test.go` | Replaced |
| `packages/agent/internal/platform/windows/windivert_integration_windows_test.go` | Replaced (new integration test against the WFP driver in E59-S6) |
| `packages/agent/cmd/agent/platformshim/install_windivert_check_windows.go` | The "install-windivert-check" CLI subcommand becomes "install-wfp-check" with the same operator-facing semantics |
| `packages/agent/cmd/agent/platformshim/install_windivert_check_other.go` | Renamed accordingly |

### 3.3 Files modified

| Path | Change |
|---|---|
| `packages/agent/go.mod` | Remove `github.com/imgk/divert-go` |
| `packages/agent/cmd/agent/main.go` | Update subcommand name to `install-wfp-check`; references the new `platformshim` symbol |
| `packages/agent/cmd/agent/wiring/network.go` | Swap the call site that constructs the WinDivert capturer for a WFP client constructor |
| `packages/agent/internal/observability/diagnostics/diagnostics.go` | Diagnostic surface: report WFP driver version (from HELLO), driver service state, audit pump depth |
| `packages/agent/internal/platform/api/api.go` | Internal platform-API contract — interception backend is now "wfp" instead of "windivert" |
| `packages/agent/internal/platform/platform.go` | Same surface update |

The architecture comments in `NexusAgent.wxs:226-259` are
preserved as historical context but updated to reference the new
WFP-side fallback path (already in E59-S3).

## 4. Public Go API

```go
// Package windows is built only on GOOS=windows; the rest of the
// agent depends on this package via the platform-API contract.
package windows

import (
    "context"
    "errors"
    "net/netip"
)

type FlowAuditEvent struct {
    ProcessID    uint32
    ParentPID    uint32
    SrcAddr      netip.AddrPort  // host-byte-order port
    OrigDstAddr  netip.AddrPort
    Protocol     uint8           // IPPROTO_TCP / IPPROTO_UDP
    Decision     Decision        // Allow / Block / Redirect
    TimestampUs  uint64
}

type Decision uint8

const (
    DecisionRedirect Decision = 1
    DecisionPermit   Decision = 2
    DecisionBlock    Decision = 3
)

type WFPClient interface {
    // Start opens the driver handle, does HELLO + SET_PROXY_PORT,
    // launches the audit-pump goroutine. Idempotent — subsequent
    // calls are no-ops while the client is running.
    Start(ctx context.Context, opts StartOptions) error

    // PushPolicy serialises the policy and submits one PUSH_POLICY
    // IOCTL. Returns ErrDriverUnavailable if the driver handle is
    // not open. Atomic — partial pushes never happen.
    PushPolicy(ctx context.Context, p Policy) error

    // GetOriginalDestination looks up the original destination for a
    // redirected flow. Returns netip.AddrPort{} and false if the
    // local port is not in the FlowTable.
    GetOriginalDestination(ctx context.Context, localPort uint16, isUDP bool) (netip.AddrPort, uint32, bool)

    // AuditEvents returns the channel of FlowAuditEvent emitted by
    // the audit pump. Closed when Stop returns.
    AuditEvents() <-chan FlowAuditEvent

    // Stop drains outstanding IRPs, closes the device handle,
    // releases the audit-pump goroutine. Returns errors only on
    // unexpected leaks (any orphan IRP is a bug).
    Stop(ctx context.Context) error
}

type StartOptions struct {
    AgentPID    uint32
    TCPProxyPort uint16
    UDPProxyPort uint16
}

type Policy struct {
    Generation  uint32
    KillSwitch  bool
    BypassPIDs  []uint32
    BypassCIDRs []netip.Prefix
}

var (
    ErrDriverUnavailable = errors.New("wfp: driver service not running")
    ErrVersionMismatch   = errors.New("wfp: driver protocol version mismatch")
    ErrAuditPumpStarved  = errors.New("wfp: audit-pump IRP queue empty for > 30s")
)
```

The interface mirrors the existing platform-API contract used by
the macOS NE proxy and the WinDivert path, so the higher-level
glue (`packages/agent/cmd/agent/wiring/network.go`) needs minimal
changes — just swap the constructor and the named backend.

## 5. Audit pump design

Inverted-call pattern per architecture §6 IOCTL_NEXUS_WFP_AUDIT_PUMP:

1. On `Start`, post `N=8` overlapped IOCTLs to the device handle
   (each carries a 4 KB user-mode buffer for batched flow records).
2. A goroutine `auditPumpLoop` waits on the IO completion port for
   any of the 8 IRPs to complete.
3. On completion: parse the buffer into `[]FlowAuditEvent`, push
   each event into the `auditCh` channel **non-blocking**
   (drop-with-counter if the channel is full — the consumer's
   responsibility to drain).
4. Re-post the same IRP slot immediately. Sustained at 8 outstanding.
5. On `Stop`: `CancelIoEx` on each IRP, wait for completion,
   close the IO completion port.

`auditCh` capacity = 1024; if the channel ever fills, a
`wfp.audit.dropped` counter increments. The wfp_windows_test must
exercise drop semantics.

## 6. Tasks

### T1 — Skeleton + Start/Stop (~2 days)

- `wfp_ioctl.go` with the five low-level IOCTL wrappers using
  `golang.org/x/sys/windows.DeviceIoControl`.
- `wfp_windows.go` `Start` → `Open` + HELLO + SET_PROXY_PORT +
  spawn `auditPumpLoop`.
- `Stop` → cancel all IRPs, close handle.
- Unit test: mock device returns canned HELLO; Start/Stop run clean.

### T2 — FlowTable + GetOriginalDestination (~1 day)

- `wfp_flowtable.go`: `map[uint16]*Entry` with `sync.RWMutex`,
  plus an LRU sweeper that evicts entries older than 5 min.
- `GetOriginalDestination` does the in-memory lookup first; on miss
  issues `IOCTL_NEXUS_WFP_GET_ORIG_DST` for an authoritative answer
  (the driver might have a flow we never saw audit pump for, on
  fresh boot).

### T3 — Audit pump (~2 days)

- `wfp_audit_pump.go` per §5 design.
- Drop counter via prometheus `wfp_audit_dropped_total`.
- Re-post-on-completion guaranteed via per-slot state machine.

### T4 — Policy push (~1 day)

- `wfp_policy.go` marshals the architecture §7 wire format.
- `PushPolicy` validates generation > last-pushed generation
  client-side (defence-in-depth; driver also validates).

### T5 — Delete WinDivert files + swap wiring (~1 day)

Single PR atomically:
- Delete the four WinDivert files listed in §3.2 — but for the two
  platformshim files (`install_windivert_check_windows.go` and
  `install_windivert_check_other.go`), **replace** them with
  `install_wfp_check_windows.go` and `install_wfp_check_other.go`
  carrying the same per-platform shape: the Windows variant calls
  out to `sc query NexusWFP` + reads the WFP driver version via
  HELLO; the non-Windows variant is a no-op returning
  "wfp-check: not applicable on this OS".
- Remove `imgk/divert-go` from `go.mod` + `go.sum`.
- Swap `wiring/network.go` to construct WFP client.
- Update diagnostic surfaces.
- `go build ./packages/agent/...` MUST be green at every commit on
  the branch — atomic rename via one commit avoids a half-broken
  intermediate state.

### T6 — Tests (~2 days)

- Unit: every IOCTL wrapper, drop semantics, race tests at
  `-race -count=20`.
- Integration: run against a real loaded driver (E59-S1 build)
  under `//go:build windows && wfpintegration` tag. CI conditional.
- Coverage: hit ≥95% (CLAUDE.md unit-test-coverage-95 binding).

## 7. Acceptance criteria

1. `go build -tags wfpintegration ./packages/agent/...` green on
   windows/amd64 with the E59-S1 driver loaded.
2. `go test -race -count=1 -tags wfpintegration ./packages/agent/internal/platform/windows/...`
   green on the same host.
3. Coverage report for the new package ≥95% statements (CLAUDE.md
   `unit-test-coverage-95` binding).
4. `git grep -i 'windivert\|imgk/divert' packages/agent/` returns
   empty. Hits elsewhere — `docs/_archive/**`,
   `docs/developers/specs/e59-*.md`, and commit messages — are
   expected and ignored (per E59-S3 AC6 same scope).
5. The `install-wfp-check` CLI subcommand prints the same operator-
   useful information the previous `install-windivert-check` did:
   driver service state, driver version, ETW provider GUID.
6. `wiring/network.go` constructs exactly one interception backend
   (WFP), with the macOS NE and Linux nftables paths unchanged.
7. Architecture doc unchanged unless an IOCTL field shape needs
   updating; in that case the change lands in the same PR.

## 8. Risks

- **R-S2.1:** Audit pump back-pressure — if the upstream consumer of
  `AuditEvents()` is slow, drops accumulate and the audit trail
  has holes. Mitigation: drop counter alarms at >100/s sustained
  for 30s; documented in E59-S6 testing.
- **R-S2.2:** Removing WinDivert atomically — until E59-S3 swaps
  the MSI, an agent shipped from this branch would try to open the
  WFP driver on a host where the WinDivert driver is installed.
  Mitigation: the swap is sequenced — S2 lands user-mode code that
  refuses to start if the WFP service is absent; S3 ships the
  matching MSI in the same release.

## 9. Out of scope

- Driver implementation (E59-S1).
- MSI install changes (E59-S3).
- Cross-arch build (E59-S4).
- Signing (E59-S5).
- Full integration test matrix (E59-S6).
