# E74-S3 — libproc PID-from-socket Attribution

> Story: e74-s3
> Epic: 74
> Status: Planning
> Date: 2026-05-21
> Requirements: `docs/developers/specs/e74-macos-pf-intercept.md` §FR-3 (FR-3.1 – FR-3.5)
> Source decisions: DEC-003 (DIOCNATLOOK seam shape), DEC-008 (test seam for 100% coverage),
>   DEC-010 (reuse `darwin/proc/processmeta_darwin.go`), DEC-011 (`pfintercept` sub-package layout)
> Architecture: `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md`
> Blocked by: none
> Blocks: S2 (pf loopback listener consumes `SocketPIDLookup`), S5 (wiring injects the production impl)

---

## 1. User Story

As the **pf loopback listener** (Story S2) running inside the macOS agent daemon, I want a
`SocketPIDLookup` interface that resolves the originating PID for each redirected TCP connection
from the source-port + uid combination recoverable via libproc, so that `proxy.FlowProcess{Name,
Bundle, User}` is populated with accurate process attribution before `BumpFlow` is called — and so
that every audit row in the admin UI shows the real application bundle (e.g. `com.google.Chrome`,
not `com.google.Chrome.helper`) rather than the drift-prone `NEAppProxyFlow.metaData.sourceAppSigningIdentifier`
that the current NE path produces for helper-process apps.

---

## 2. Tasks

### T3.1 — New file `pfintercept/pidlookup/pidlookup_darwin.go` (cgo glue, allowlisted)

Add `packages/agent/internal/platform/darwin/pfintercept/pidlookup/pidlookup_darwin.go` with
build tag `//go:build darwin`. This file contains the cgo wrapper for the macOS `proc_listpids`
+ `proc_pidfdinfo` / `proc_pidsocketinfo` syscalls. The implementation:

1. Calls `proc_listpids(PROC_UID_ONLY, uid, buf, buflen)` to enumerate PIDs owned by the
   redirected socket's source uid.
2. For each PID in the list, calls `proc_pidinfo(pid, PROC_PIDFDINFO, 0, &fdinfo, size)` to
   iterate the process's open file descriptors, then for each fd of type `PROX_FDTYPE_SOCKET`
   calls `proc_pidfdinfo(pid, fd, PROC_PIDFDSOCKETINFO, &sockinfo, size)` to read the socket
   tuple (local address + port).
3. Matches the socket whose local port equals the redirected connection's source port (the
   original client-side ephemeral port, known from the DIOCNATLOOK result produced by the
   `natlook` sub-package per DEC-003).
4. Returns the matching PID, or 0 if no match.

The file is ≤60 lines of cgo boilerplate. It is listed in `.coverage-allowlist` under category D
("OS-bound — requires root + `/dev/pf` access") per DEC-008 with a one-line rationale.

### T3.2 — Interface seam `SocketPIDLookup` in `pfintercept/pidlookup/lookup.go`

Add `packages/agent/internal/platform/darwin/pfintercept/pidlookup/lookup.go` (pure Go, no cgo):

```go
// SocketPIDLookup resolves the originating PID from a redirected TCP
// connection's source port and the uid recovered from the accepting socket.
// The production implementation calls proc_listpids + proc_pidfdinfo via cgo;
// tests substitute a struct mock.
type SocketPIDLookup interface {
    // LookupPID returns the PID owning the TCP socket whose local port is
    // srcPort and whose owning uid is srcUID. Returns 0 when no match is
    // found (process already exited or permission denied).
    LookupPID(srcPort int, srcUID uint32) (pid int, err error)
}
```

The production struct `LibprocLookup` (in `pidlookup_darwin.go`) implements this interface by
delegating to the cgo glue in T3.1. A `MockLookup` test helper (in
`pidlookup/pidlookup_test.go`) implements the same interface via a map-based fixture, allowing
table-driven tests with no kernel access.

### T3.3 — Adapter `pidlookup/attribution.go`: bridge `SocketPIDLookup` → `proc.ProcessInfo` → `proxy.FlowProcess`

Add `packages/agent/internal/platform/darwin/pfintercept/pidlookup/attribution.go` (pure Go):

```go
// ResolveFlowProcess calls lookup.LookupPID for the given source port and uid,
// then delegates to proc.ProcessInfo(pid) to resolve the executable path,
// bundle identifier, and OS user. Returns proxy.FlowProcess populated from
// proc.Meta. On any failure (LookupPID returns 0, proc.ProcessInfo returns
// error) returns an empty proxy.FlowProcess so BumpFlow proceeds without
// attribution — per FR-3.3.
func ResolveFlowProcess(
    ctx context.Context,
    lookup SocketPIDLookup,
    srcPort int,
    srcUID uint32,
) proxy.FlowProcess
```

This function is the only caller of `proc.ProcessInfo` in the pf path. It does NOT duplicate
the `proc_pidpath` / `proc_pidinfo` cgo calls that already live in
`packages/agent/internal/platform/darwin/proc/processmeta_darwin.go` (DEC-010 reuse invariant).
The mapping from `proc.Meta` to `proxy.FlowProcess` is:

- `FlowProcess.Name` ← `proc.Meta.Name` (already handles the Electron helper version-string
  fallback via `LooksLikeVersionString` + `BundleDisplayNameFromPath`)
- `FlowProcess.Bundle` ← `proc.Meta.BundleID` (populated by `proc.DetectBundleID` walking up
  to the nearest `.app` ancestor)
- `FlowProcess.User` ← `proc.Meta.User`

### T3.4 — Source-uid recovery helper `pidlookup/uid_darwin.go` (cgo, allowlisted)

Add `packages/agent/internal/platform/darwin/pfintercept/pidlookup/uid_darwin.go` (build tag
`//go:build darwin`, ≤30 lines). On macOS the listener's accepted socket arrives from the kernel
redirect (not from a real peer process), so `getpeercred` / `SO_PEERCRED` is not available.
The source uid is recovered by opening `/dev/pf` and issuing `DIOCNATLOOK` (already done by the
`natlook` sub-package per DEC-003) to obtain the source IP + port, then resolving uid via
`getpwnam_r` / `stat` on `/proc` — or, more precisely, enumerating PIDs via
`proc_listpids(PROC_ALL_PIDS)` and filtering by `proc_pidinfo PROC_PIDTBSDINFO` to find the
owning uid for the matching source-port socket. Since `proc_listpids(PROC_UID_ONLY, uid)` requires
knowing the uid first, the implementation uses a two-pass approach:

- Pass 1: enumerate all PIDs (`PROC_ALL_PIDS`), read `bsdinfo.pbi_uid` via `proc_pidinfo`, and
  for each PID scan its open sockets for a match on `srcPort`. This yields both PID and uid in
  one pass.
- The uid is then available for T3.2's `LookupPID` signature.

Open question (defer to Code phase): determine whether `proc_listpids(PROC_ALL_PIDS)` vs the
uid-scoped `PROC_UID_ONLY` is measurably faster on a developer machine with O(200) processes.
If the all-PIDs scan is acceptable (expected ≤1 ms at 200 processes), the two-pass simplifies
to one call.

This file is also listed in `.coverage-allowlist` under category D with its own one-line rationale.
The combined allowlist entries for `natlook/` + `pidlookup/` cgo files MUST total ≤4 entries and
≤120 LOC (DEC-008 budget).

### T3.5 — LRU cache for PID lookups `pidlookup/cache.go`

Add `packages/agent/internal/platform/darwin/pfintercept/pidlookup/cache.go` (pure Go). The
`proc_listpids` + `proc_pidfdinfo` scan is O(processes × fds). On a developer machine with
O(200) processes and O(50) fds each, the worst case is 10 000 `proc_pidfdinfo` calls per
accepted connection. Cache PID results keyed on `(srcPort, srcUID)` with a 2-second TTL (matching
the Linux analogue at `linux_linux.go:613-614`). Evict expired entries when cache length exceeds
1024. Wrap the production `LibprocLookup` in a `CachedLookup` struct that implements
`SocketPIDLookup` and checks the cache before delegating.

### T3.6 — Table-driven tests `pidlookup/attribution_test.go`

Add `packages/agent/internal/platform/darwin/pfintercept/pidlookup/attribution_test.go` (pure
Go, no cgo). All five cases from FR-3.5 as named sub-tests:

- `bundle_app`: `MockLookup` returns a PID whose `Meta.BundleID` is `com.google.Chrome`;
  `ResolveFlowProcess` must return `FlowProcess{Bundle: "com.google.Chrome", ...}`.
- `cli_binary`: `Meta.BundleID` is empty (a CLI binary with no `.app` bundle); `FlowProcess.Bundle`
  must be empty.
- `chrome_helper_child`: `Meta.Name` looks like a version string (e.g. `"2.1.141"`);
  `BundleDisplayNameFromPath` (called inside `proc.ProcessInfo`) returns `"Google Chrome"`;
  `FlowProcess.Name` must be `"Google Chrome"` not `"2.1.141"`.
- `process_exited`: `MockLookup.LookupPID` returns `(0, nil)`;
  `ResolveFlowProcess` must return an empty `FlowProcess{}` without error.
- `permission_denied`: `MockLookup.LookupPID` returns `(0, syscall.EACCES)`;
  `ResolveFlowProcess` must return an empty `FlowProcess{}` and log a warning (observable via
  a `slog.Handler` test sink).

Add `pidlookup/cache_test.go` to cover the TTL eviction logic and the `CachedLookup.LookupPID`
pass-through / cache-hit paths.

Add `pidlookup/lookup_test.go` to cover the `MockLookup` helper's own contract (returns
configured values, panics on unexpected keys if `StrictMode=true`).

### T3.7 — `.coverage-allowlist` entries

Append to `scripts/.coverage-allowlist`:

```
packages/agent/internal/platform/darwin/pfintercept/pidlookup # D: OS-bound cgo — proc_listpids+proc_pidfdinfo require root access; logic coverage via MockLookup seam in attribution_test.go
packages/agent/internal/platform/darwin/pfintercept/natlook    # D: OS-bound cgo — DIOCNATLOOK ioctl requires /dev/pf access; logic coverage via OriginalDstResolver mock in listener tests (S2)
```

Both entries require explicit user approval at PR review time per CLAUDE.md unit-test-coverage
binding ("`Adding to the allowlist requires explicit user approval`").

### T3.8 — Documentation update

Update `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md` to
add a sub-section under the new §3a "pf interception path (E74)" (introduced by S1) describing
the PID-from-socket attribution chain: DIOCNATLOOK → source port → `proc_listpids` scan →
`proc.ProcessInfo(pid)` → `proxy.FlowProcess`. Cite DEC-010's reuse decision. This update lands
in the same PR as the S3 code per CLAUDE.md code/doc lockstep (FR-9.1).

---

## 3. Acceptance Criteria

- AC3.1 (FR-3.1): On each accepted pf-redirected connection, `ResolveFlowProcess` is called with
  the source port and uid recovered from the DIOCNATLOOK result. The returned `proxy.FlowProcess`
  is passed to `BumpFlow` as the `proc` argument. Verified by the S2 listener integration test
  that uses `MockLookup` and asserts the `FlowProcess` passed to a mock `BumpFlow`.

- AC3.2 (FR-3.2): When the originating process lives inside a `.app` bundle, `FlowProcess.Bundle`
  is the `CFBundleIdentifier` read from the bundle's `Info.plist`. For a CLI binary with no `.app`
  ancestor, `FlowProcess.Bundle` is empty. Both cases covered by `bundle_app` and `cli_binary`
  sub-tests in T3.6.

- AC3.3 (FR-3.2, FR-7.5): For a Chrome-helper-style process whose executable basename looks like
  a version string (e.g. `"2.1.141"`), `FlowProcess.Name` is the containing `.app` bundle's
  `CFBundleDisplayName` (e.g. `"Google Chrome"`), not the version string. Covered by
  `chrome_helper_child` sub-test in T3.6.

- AC3.4 (FR-3.3): When `LookupPID` returns 0 (process already exited) or returns a `EACCES`
  error, `ResolveFlowProcess` returns `proxy.FlowProcess{}` and `BumpFlow` is called with the
  empty struct — the flow continues and produces an audit row with empty `source_process` /
  `source_bundle` columns. Covered by `process_exited` and `permission_denied` sub-tests in T3.6.

- AC3.5 (FR-3.5, NFR-4): Per-package statement coverage for `pfintercept/pidlookup/` is ≥95%
  via `go test -cover -count=1`. The cgo glue files (`pidlookup_darwin.go`, `uid_darwin.go`) are
  the only lines below the threshold and are covered by the two allowlist entries added in T3.7.

- AC3.6 (DEC-010): The `proc` package (`darwin/proc/processmeta_darwin.go`) is not modified.
  `proc.ProcessInfo` is called by `attribution.go` without reimplementation. Verified by `git diff
  --name-only` showing no changes under `packages/agent/internal/platform/darwin/proc/`.

- AC3.7 (DEC-011): The new code lives under `packages/agent/internal/platform/darwin/pfintercept/
  pidlookup/` as per the four-sub-package layout mandated by DEC-011. No files are added at the
  top-level `pfintercept/` package for this story's concerns.

- AC3.8 (NFR-9): The `chrome_helper_child` attribution case in T3.6 demonstrates that the
  production code path (using real `proc.ProcessInfo` on a live helper process) would attribute
  the flow to the user-facing bundle, not the helper binary. This covers the FR-7.5 acceptance
  gate requirement.

- AC3.9 (DEC-008): `go test -race -count=1 ./packages/agent/internal/platform/darwin/pfintercept/
  pidlookup/...` passes on CI (Linux runner with `GOOS=darwin` cross-compilation gating build;
  `//go:build darwin` files excluded from Linux CI run). The pure-Go tests (`attribution_test.go`,
  `cache_test.go`, `lookup_test.go`) run on any platform.

- AC3.10: `scripts/.coverage-allowlist` contains exactly the two new entries from T3.7, formatted
  per the allowlist convention. Running `scripts/check-go-coverage.sh --strict-allowlist` returns
  0 removable entries that include the new `pidlookup` or `natlook` entries.

---

## 4. Interface Contract

The canonical interface exposed by this story is:

```go
// Package pidlookup implements PID-from-socket attribution for the pf
// loopback listener. All non-cgo logic is covered by table-driven mock
// tests per DEC-008.
package pidlookup

// SocketPIDLookup resolves the originating PID from a redirected TCP
// connection's source port and owning uid.
//
// The pf loopback listener (S2, packages/agent/internal/platform/darwin/
// pfintercept/listener/) calls ResolveFlowProcess (see attribution.go),
// which calls LookupPID here as the first step.
// Story S5 (wiring) injects either LibprocLookup (production) or
// MockLookup (tests) via the pfintercept top-level Deps struct.
type SocketPIDLookup interface {
    // LookupPID returns the PID of the process that owns a TCP socket
    // whose local port equals srcPort and whose owning uid equals srcUID.
    // Returns pid=0 when no match is found (process exited before lookup,
    // or no socket matches). Returns a non-nil error only for hard
    // failures (permission denied, /dev/pf inaccessible); callers treat
    // all error cases as pid=0 and proceed with empty FlowProcess.
    LookupPID(srcPort int, srcUID uint32) (pid int, err error)
}
```

Inputs: `srcPort` is the ephemeral source port of the original client connection, recovered from
the `DIOCNATLOOK` result (DEC-003, produced by `pfintercept/natlook/`). `srcUID` is the OS user
ID of the process that opened the socket, also recovered from the DIOCNATLOOK or the per-PID scan
in T3.4.

Output: a PID suitable for passing to `proc.ProcessInfo(pid)` from
`packages/agent/internal/platform/darwin/proc/processmeta_darwin.go`.

The listener (S2) does not call `SocketPIDLookup` directly; it calls
`pidlookup.ResolveFlowProcess(ctx, lookup, srcPort, srcUID)` which encapsulates the
`LookupPID` → `proc.ProcessInfo` → `proxy.FlowProcess` pipeline. This keeps the listener's
dependency surface narrow and fully mockable.

Production impl: `LibprocLookup` (in `pidlookup_darwin.go`, cgo). Wrapped by `CachedLookup` (in
`cache.go`, pure Go) before injection.
Test impl: `MockLookup` (in `pidlookup_test.go`, pure Go, map from `(port, uid)` → `pid`).

---

## 5. Dependencies

- **Produces**: `SocketPIDLookup` interface + `ResolveFlowProcess` function consumed by Story S2
  (pf loopback listener, `pfintercept/listener/`).
- **Consumed by**: Story S5 (wiring) injects `CachedLookup{LibprocLookup{}}` into the listener's
  deps struct.
- **Reuses without modification** (DEC-010): `packages/agent/internal/platform/darwin/proc/
  processmeta_darwin.go` — `proc.ProcessInfo`, `proc.Meta`, `proc.LooksLikeVersionString`,
  `proc.DetectBundleID`, `proc.BundleDisplayNameFromPath`.
- **Reads result from** (DEC-003): the `srcPort` value produced by `pfintercept/natlook/
  OriginalDstResolver.Resolve` — the DIOCNATLOOK result is available to the listener before
  `ResolveFlowProcess` is called. S3 does not call `natlook` directly; that coupling lives in S2.
- **No upstream blockers**: S3 code can be written and fully tested (via mocks) independently of
  S1 (pf rules), S2 (listener), and S4 (admin-facing QUIC uid resolution). All four sub-packages
  are decoupled at interface boundaries.

---

## 6. Out of Scope

- **Endpoint Security** (per D3 and Requirements §10): the `com.apple.developer.endpoint-security.
  client` entitlement is explicitly rejected. The residual attribution gap — deeply-nested helper
  trees where `proc_pidpath` returns the helper executable and the user-facing bundle is ≥3 levels
  up — is documented in Requirements §10 and FR-3.4. No attempt is made to close this gap here.

- **Helper-process parent-chain walk beyond `BundleDisplayNameFromPath`**: the existing
  `processmeta_darwin.go` already walks up from the executable to the nearest `.app` bundle and
  reads `CFBundleDisplayName`. This covers the Electron/Chrome single-level helper case (FR-7.5).
  Walking the full parent PID chain (via multiple `proc_pidinfo PROC_PIDTBSDINFO` calls following
  `pbi_ppid`) to find the user-facing bundle when the nearest `.app` is still a sub-bundle is the
  Endpoint Security use case — out of scope per D3.

- **IPv6 socket matching**: `proc_pidsocketinfo` exposes both IPv4 and IPv6 socket tuples. The
  pf `rdr` rules in S1 target TCP 443 outbound, which is effectively always IPv4 or IPv4-mapped
  IPv6 in practice on macOS. Full dual-stack socket matching may be added later if empirical data
  shows IPv6-native apps producing misses.
  Open question (defer to Code phase): confirm whether `pf rdr` on macOS rewrites IPv4-mapped
  IPv6 addresses to plain IPv4 in the socket tuple visible to `proc_pidsocketinfo`.

- **`PROC_PIDLISTFDS` vs `PROC_PIDFDINFO` API choice**: DECISIONS.md DEC-010 names
  `proc_pidfdinfo` / `proc_pidsocketinfo` as the approach. If Code-phase prototyping finds a
  faster or more reliable macOS API (e.g. `proc_pidlistfds` returning a flat array without
  per-fd round-trips), the implementation may use it without a new decision entry — the interface
  shape of `SocketPIDLookup` is unaffected.

- **UDP socket attribution**: the pf UDP rdr path (FR-1.4, QUIC fallback) does not need
  per-connection PID attribution because the uid-based pf rule already scopes the redirect to
  known bundles' uid set. No `SocketPIDLookup` call is made for UDP flows.

- **Windows / Linux**: `pidlookup_darwin.go` is build-tag-gated to `//go:build darwin`. Linux
  already has `findPIDBySocket` via `/proc/net/tcp` + `/proc/[pid]/fd` (
  `linux_linux.go:622-713`). No changes to Linux or Windows code.

---

## 7. References

- Requirements §FR-3 (FR-3.1 – FR-3.5): `docs/developers/specs/e74-macos-pf-intercept.md`
- DECISIONS.md DEC-003: DIOCNATLOOK ioctl seam shape (OriginalDstResolver interface in
  `pfintercept/natlook/`)
- DECISIONS.md DEC-008: test seam architecture for 100% coverage on cgo / syscall code
  (interface + mock pattern; cgo glue allowlisted under category D)
- DECISIONS.md DEC-010: reuse of `darwin/proc/processmeta_darwin.go` rather than reimplementing
  `proc_pidpath` / `proc_pidinfo` in the pf path
- DECISIONS.md DEC-011: `pfintercept` four-sub-package layout (`pfrules`, `listener`, `natlook`,
  `pidlookup`)
- `packages/agent/internal/platform/darwin/proc/processmeta_darwin.go`: production-grade libproc
  attribution already shipped; `proc.ProcessInfo`, `proc.Meta`, version-string + bundle-walk
  logic at lines 33-83 and 119-155
- `packages/agent/internal/platform/linux/linux_linux.go` lines 622-713: structural Linux
  analogue (`findPIDBySocket` / `findSocketInode` / `findPIDByInode`) demonstrating the
  socket-tuple → PID lookup pattern, inode-based cache with 2-second TTL, and O(processes×fds)
  scan shape that the macOS implementation mirrors
- `packages/agent/internal/network/proxy/bridge.go` lines 117-127: `proxy.FlowProcess` struct
  definition (the output type `ResolveFlowProcess` must produce) and lines 153-162: `BumpFlow`
  signature showing how `FlowProcess` is consumed
- `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md`: to be
  updated in the same PR per FR-9.1 (T3.8 above)
