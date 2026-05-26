# E59-S1 — WFP Driver Skeleton + Callout Registration

**Status:** Design — pending implementation
**Date:** 2026-05-24
**Epic:** [E59](e59-windows-wfp-migration.md)
**Architecture:** [agent-windows-wfp-driver.md](../architecture/agent-windows-wfp-driver.md)

---

## 1. User story

> **As** the agent platform team
> **I want** a WFP callout driver that registers the four callouts
> defined in the architecture (§4) and exposes the IOCTL contract
> defined there (§6)
> **so that** subsequent stories can plug a real user-mode policy
> engine, real MSI install, real cross-arch build, real signing, and
> real test matrix on top of a working kernel skeleton — without
> rewriting the driver each time.

This story is "the driver runs, registers callouts, accepts IOCTLs,
returns sensible answers from a stub policy." It is NOT a feature-
complete driver. Production interception logic lands in subsequent
stories on top of the skeleton.

## 2. Goal

Land a compilable, loadable, callout-registering kernel driver under
`packages/agent/platform/windows/nexus-wfp-driver/` that:

1. Builds successfully under WDK 11 for `amd64` (cross-arch in E59-S4).
2. Loads on a Windows 11 24H2 amd64 machine via `pnputil /add-driver`
   without errors.
3. Registers the four callouts named in architecture §4 and verifies
   them via `netsh wfp show state`.
4. Implements the five IOCTLs from architecture §6 with stub
   handlers that return sensible defaults (HELLO returns the version;
   GET_ORIG_DST returns `STATUS_NOT_FOUND` for any port; the rest
   accept the input and return `STATUS_SUCCESS`).
5. Passes a 5-minute Driver Verifier (Standard + DDI Compliance Checking)
   run with zero violations under the stub workload.

Feature-complete redirect logic, real FlowTable, real policy storage,
and real audit pump are explicitly **not** in this story — they're
covered in the implementation pass that follows (a future story
E59-S1.5 or absorbed into S2 depending on review).

## 3. Tasks

### T1 — Project scaffolding (~1 day)

- Create `packages/agent/platform/windows/nexus-wfp-driver/` with:
  - `nexus-wfp.vcxproj` — WDK kernel-mode driver project (KMDF
    framework, target OS Windows 10 RS5+, target platform x64 in
    this story; arm64 added in E59-S4).
  - `nexus-wfp.sln` — solution that wraps the vcxproj.
  - `Common.h` — shared definitions (IOCTLs, structs, version).
  - `Driver.c` — DriverEntry + Unload stubs.
  - `Callouts.c` — empty callout function shells (registered but
    immediately return `FWP_ACTION_PERMIT`).
  - `Filter.c` — WFP filter registration via user-mode FwpmEngine
    helpers used from inside DriverEntry's session.
  - `Ioctl.c` — IRP dispatch table for the five IOCTL codes; all
    stubs.
  - `nexus-wfp.inf` — minimal install manifest.
  - `nexus-wfp.rc` — version resource (binding: file version =
    semantic version of the agent release; matches MSI ProductVersion).
- Wire `vcxproj` to use `WindowsKernelModeDriver10.0` PlatformToolset
  and `KMDF` driver type.

### T2 — DriverEntry / Unload (~0.5 day)

- `DriverEntry`:
  - Create symbolic link `\\??\\NexusWFP`.
  - Open a WFP session (FwpmEngineOpen0) with `FWPM_SESSION_FLAG_DYNAMIC` —
    callouts are auto-deleted when the engine handle closes, no need
    to explicitly unregister at unload.
  - Register the four callouts (§T3).
  - Initialise the IOCTL device object.
- `Unload`:
  - Cancel all outstanding IRPs (audit-pump queue drain).
  - Close the WFP engine handle (auto-unregisters callouts).
  - Delete the symbolic link and device object.
- Verifier-clean shutdown semantics (constraint C-1 from epic §6).

### T3 — Callout registration (~1 day)

Register each of the four WFP layers from architecture §4:

| Callout symbol | WFP layer | Stub behaviour |
|---|---|---|
| `NexusConnectRedirectV4` | `FWPM_LAYER_ALE_CONNECT_REDIRECT_V4` | Permit, no redirect, no flow stored |
| `NexusConnectRedirectV6` | `FWPM_LAYER_ALE_CONNECT_REDIRECT_V6` | Same |
| `NexusAuthConnectV4` | `FWPM_LAYER_ALE_AUTH_CONNECT_V4` | Permit |
| `NexusAuthConnectV6` | `FWPM_LAYER_ALE_AUTH_CONNECT_V6` | Permit |

Each callout `classifyFn` increments a per-callout ETW counter so the
test in T6 can prove the registration is live.

### T4 — IOCTL dispatch (~1 day)

Dispatch table for the IRPs in `IRP_MJ_DEVICE_CONTROL`. Five codes
from architecture §6:

| Code | Stub behaviour |
|---|---|
| `IOCTL_NEXUS_WFP_HELLO` | Validate input layout; return `{driverProtocolVersion=1, capabilities=0x7}` |
| `IOCTL_NEXUS_WFP_SET_PROXY_PORT` | Validate input layout; store both ports in a `g_State` struct; return success |
| `IOCTL_NEXUS_WFP_PUSH_POLICY` | Validate input layout; swallow the policy (do not yet apply it); return success |
| `IOCTL_NEXUS_WFP_GET_ORIG_DST` | Always return `STATUS_NOT_FOUND` (no flow table yet) |
| `IOCTL_NEXUS_WFP_AUDIT_PUMP` | Pend the IRP into an empty queue with cancellation routine; never complete (the real audit logic comes later) |

All IOCTLs MUST validate the user-mode buffer length BEFORE dereferencing
fields (NFR-5).

### T5 — Build + load + smoke (~0.5 day)

- `build.bat` (placeholder, expanded in E59-S4 for cross-arch):
  ```
  msbuild nexus-wfp.sln /p:Configuration=Release /p:Platform=x64
  ```
- On an amd64 Windows 11 24H2 test machine with `bcdedit /set testsigning on`:
  ```
  pnputil /add-driver nexus-wfp.inf /install
  sc start NexusWFP
  netsh wfp show state > wfp-state.txt
  # grep wfp-state.txt for "NexusConnectRedirectV4" etc.
  ```
- Smoke: a one-off C program (`tools/wfp-smoke/smoke.c`) opens
  `\\??\\NexusWFP` and issues HELLO + SET_PROXY_PORT + GET_ORIG_DST,
  asserts the returned values match expectations.

### T6 — Driver Verifier (~0.5 day)

- `verifier /standard /driver nexus-wfp.sys`
- `verifier /flags 0x09BB /driver nexus-wfp.sys` (Standard + DDI Compliance Checking + IRP Logging)
- Reboot, run the T5 smoke for 5 minutes, then `verifier /reset`.
- MUST report zero violations.

### T7 — Code review checklist (~0.5 day)

Pre-merge gate items:
- No `__try`/`__except` blocks in callouts (callouts run at DISPATCH; SEH is wrong there).
- No floating-point in driver code.
- No `KeAcquireSpinLock` held across a function call that could touch
  paged memory.
- All allocations tagged with `NXWF` pool tag (for poolmon visibility).
- Every `IoSetCancelRoutine` paired with an IRP completion path.

## 4. Acceptance criteria

This story is done when:

1. `msbuild nexus-wfp.sln /p:Configuration=Release /p:Platform=x64`
   produces `nexus-wfp.sys` with zero warnings at /W4 and zero
   warnings from the WDK static driver verifier (`/p:RunWdkSdv=true`).
2. `pnputil /add-driver` + `sc start NexusWFP` succeed on a fresh
   Windows 11 24H2 amd64 VM with `testsigning on`.
3. `netsh wfp show state` lists exactly the four callouts named in
   T3 with active filter conditions.
4. The T5 smoke utility runs to completion without error and the
   returned values match expectations.
5. Driver Verifier (T6) reports zero violations.
6. `irpcanc.exe` or equivalent confirms a clean Unload (no orphan
   IRPs in the audit-pump queue when the driver is stopped via
   `sc stop NexusWFP`).
7. The architecture doc is updated if any IOCTL field or callout
   name changes during implementation (CLAUDE.md doc lockstep).

## 5. Risks

- **R-S1.1:** WFP callout registration ordering — if NexusAuthConnect
  is registered AFTER NexusConnectRedirect, the redirect callout
  fires first (already true per WFP layer order) and any block
  decision in AUTH comes too late to undo the redirect. Mitigation:
  AUTH callouts also store the original dest in a "pending-block"
  set; if AUTH blocks, the audit pump emits a block record and the
  proxy refuses the redirected connection. Documented in
  architecture §4.
- **R-S1.2:** Cancellation routine race — `IoSetCancelRoutine`
  followed by `IoMarkIrpPending` has a well-known race window that
  must be closed with a spinlock acquire/release pattern. Use
  Microsoft's documented `IoCsqInsertIrpEx` pattern (a Cancel-Safe
  Queue) for the audit-pump IRP queue.

## 6. Out of scope

- Real FlowTable + redirect logic (next story or absorbed into S2).
- Real policy enforcement (stub accepts the policy but doesn't act).
- ARM64 build (E59-S4).
- Signing pipeline (E59-S5).
- Production test matrix (E59-S6).
