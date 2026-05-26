# Agent — Windows WFP Interception Layer

**Status:** Design — pending implementation (E59)
**Date:** 2026-05-24
**Author:** brainstorm — nexus + Claude
**Supersedes:** WinDivert-based interception layer (E42 Phase B) — the WinDivert
kernel service and `imgk/divert-go` Go binding are deleted by E59-S2 and
E59-S3 in their entirety.

---

## 1. Goal

Replace the Windows agent's outbound-traffic interception layer — currently
based on **WinDivert v2.2.2** — with an in-house **Windows Filtering
Platform (WFP) callout driver**. Outcomes:

1. **Cross-arch support.** A single driver source tree compiles for both
   `amd64` and `arm64` using the WDK. ARM64 customers (Surface Pro 11,
   Snapdragon X Elite laptops) get the same interception fidelity as
   `amd64` customers — no more `SystemProxyFallback` degraded mode.
2. **No external kernel-driver dependency.** WinDivert upstream
   (`basil00/WinDivert`) has no ARM64 build, an open ARM64 request since
   2022 (issues #236, #379), and is licensed dual BSD/LGPLv3 — neither
   tracker activity nor licence model is acceptable for shipping to
   regulated customers indefinitely. The WFP API is a Microsoft
   first-party kernel framework; long-term support is guaranteed.
3. **Higher-level interception primitive.** WFP's
   `FWPM_LAYER_ALE_CONNECT_REDIRECT_V4/V6` callout fires at the *socket
   connect* boundary and supports first-class redirection of the
   connection target — no need for network-layer packet capture,
   checksum recomputation, or TCP segment reassembly that WinDivert
   forces us to do today.

This document is the source of truth for the new interception layer.
Code changes in `packages/agent/platform/windows/nexus-wfp-driver/`,
`packages/agent/internal/platform/windows/wfp_*.go`, and
`packages/agent/platform/windows/installer/wfp.wxi` must conform to it.

---

## 2. Why this is binding

Per CLAUDE.md → Pre-edit reading (3-doc rule), any change touching the
Windows interception layer or the agent platform shim **must** open and
follow this document. The WinDivert paragraph in
`packages/agent/platform/windows/installer/NexusAgent.wxs:226-259`
references the "first-packet gap" requirement; that requirement is
preserved here (see §5.3).

Specific binding decisions in this doc:

- **D1 — WFP layers used.** Only the layers listed in §4. Adding a new
  layer requires updating this doc in the same PR (CLAUDE.md code/doc
  lockstep).
- **D2 — IOCTL contract.** The IOCTL codes and their request/response
  layouts in §6 are versioned. Any change is a contract break —
  bump `NEXUS_WFP_PROTOCOL_VERSION` in `Common.h` and update the
  user-mode Go side in the same PR.
- **D3 — Cross-arch source-tree shape.** One driver source, one INF,
  two `.sys` outputs. No per-arch `#ifdef` branches in callout logic
  (only in WDK-provided headers).
- **D4 — Fail-open under daemon disconnect.** If the user-mode daemon
  is not running or its IOCTL queue is starved, the driver permits the
  flow (passthrough). This matches the macOS NE proxy fail-open
  invariant in CLAUDE.md.

---

## 3. Scope

| In scope | Out of scope |
|---|---|
| Outbound TCP connect interception (IPv4 + IPv6) | Inbound (listener-side) interception — agent does not need it |
| Outbound UDP first-packet interception (QUIC discovery, DNS) | Raw L2/L3 packet inspection (use ETW or pktmon for diagnostics) |
| Connect-time redirect to local proxy `127.0.0.1:proxyPort` | Modifying packet contents in flight (the proxy does that at L7) |
| Per-process attribution via `FWPS_INCOMING_VALUES0.processId` | Per-thread attribution (not needed for compliance gating) |
| Cross-arch build (amd64 + arm64) | x86 32-bit (Windows 11 has no 32-bit edition; not a target) |
| Driver attestation signing via Microsoft Hardware Dev Center | Test-signed builds (dev only, never shipped) |

---

## 4. WFP Layers and Callout Roles

WFP exposes ~80 layers across IPv4/IPv6 × inbound/outbound × transport
stage. We use **four**:

| Layer | Direction | Purpose | Action |
|---|---|---|---|
| `FWPM_LAYER_ALE_CONNECT_REDIRECT_V4` | Outbound TCP/UDP IPv4 | Connect-time redirect | Change destination to `127.0.0.1:proxyPort`, stamp original dest in a `FWPS_CONNECTION_REDIRECT_STATE` for later lookup |
| `FWPM_LAYER_ALE_CONNECT_REDIRECT_V6` | Outbound TCP/UDP IPv6 | Same, IPv6 | Same |
| `FWPM_LAYER_ALE_AUTH_CONNECT_V4` | Outbound TCP/UDP IPv4 | Decision gate (allow/block) | `FWP_ACTION_PERMIT` or `FWP_ACTION_BLOCK` based on user-mode policy |
| `FWPM_LAYER_ALE_AUTH_CONNECT_V6` | Outbound TCP/UDP IPv6 | Same, IPv6 | Same |

**Why the AUTH layer is separate from REDIRECT:** WFP fires REDIRECT
*before* AUTH. The redirect callout cannot block; only AUTH can. By
splitting the two, we keep redirect logic atomic and put block decisions
in their own callout that can return `FWP_ACTION_BLOCK` directly. This
also matches the standard Microsoft sample
([WFPSampler](https://github.com/microsoft/Windows-driver-samples/tree/main/network/trans/WFPSampler))
layout.

**Why we skip flow-established layers:** A flow becomes established
after the redirect already happened. Once the connection is to
`127.0.0.1:proxyPort`, there's nothing to do at the established layer —
the proxy is now in control.

---

## 5. End-to-End Flow

### 5.1 Outbound TCP connect path

```
┌────────────────┐
│ App process P  │  connect(socket, dst=10.0.0.5:443)
└────┬───────────┘
     │
     ▼
┌──────────────────────────────────────────────────────────────┐
│ Kernel: TCP/IP stack invokes WFP ALE_CONNECT_REDIRECT_V4     │
│   callout = NexusConnectRedirectV4                           │
│                                                              │
│   1. Read FWPS_INCOMING_VALUES0 → processId, dst IP/port,    │
│      src IP/port, protocol                                   │
│                                                              │
│   2. STUB(e59-s1): consult policy cache (filled by IOCTL     │
│      from user-mode at startup; refreshed on shadow push).   │
│      If policy says "do not intercept this process/dst" →    │
│      return FWP_ACTION_PERMIT, no redirect.                  │
│                                                              │
│   3. Otherwise:                                              │
│        a. Store {src_port, orig_dst_ip, orig_dst_port,       │
│           processId} in FlowTable (keyed by src_port).       │
│        b. Use FwpsRedirectHandleCreate0 → handle             │
│        c. FwpsApplyModifiedLayerValues0 to rewrite dst to    │
│           127.0.0.1 + proxyPort                              │
│        d. Return FWP_ACTION_PERMIT (so the connect proceeds  │
│           against the rewritten dst)                         │
│                                                              │
│   4. Kernel proceeds with TCP SYN to 127.0.0.1:proxyPort     │
└──────────────────────────────────────────────────────────────┘
     │
     ▼
┌──────────────────────────────────────────────────────────────┐
│ Proxy (user-mode, nexus-agent.exe) accepts on proxyPort      │
│                                                              │
│   1. accept() returns (conn_fd, peer_addr=127.0.0.1:xxxx)    │
│   2. DeviceIoControl(IOCTL_NEXUS_WFP_GET_ORIG_DST,           │
│                       in={local_port=peer_addr.port},        │
│                       out={orig_dst_ip, orig_dst_port,       │
│                             processId})                      │
│   3. Now proxy knows the connection was originally meant     │
│      for 10.0.0.5:443; performs L7 MITM as usual             │
└──────────────────────────────────────────────────────────────┘
```

### 5.2 Outbound UDP first-packet path

UDP is connectionless; WFP still fires ALE_CONNECT_REDIRECT on the first
sendto() per (5-tuple). Same redirect mechanics:

- Store {src_port, orig_dst_ip, orig_dst_port, processId} in UdpFlowTable
- Redirect to `127.0.0.1:proxyPort` (proxy listens UDP on the same
  numeric port as TCP — **binding constraint on E59-S2**: the
  user-mode proxy must bind a UDP socket and a TCP socket to the same
  port number on the loopback address, so the driver only needs one
  configured proxyPort value via SET_PROXY_PORT below)
- Proxy's `recvfrom()` returns the redirected source; proxy looks up
  orig_dst via the same IOCTL

QUIC discovery (HTTP/3 Alt-Svc) is covered because Chromium and Edge
issue an initial UDP/443 sendto that fires REDIRECT.

### 5.3 First-packet gap (binding requirement)

The driver must be loaded and policy initialised **before** the agent
user-mode process binds to `proxyPort` and starts accepting. If the
order is inverted:

- Driver active + no policy → fail-open (passthrough; CLAUDE.md NE
  invariant).
- Driver inactive + user-mode active → app traffic bypasses the
  agent entirely (silent compliance failure).

Sequencing guarantees in §10 — MSI ServiceDependency + ordered Start.

---

## 6. IOCTL Contract

Driver exposes a single device object: `\\Device\\NexusWFP` (symbolic
link `\\??\\NexusWFP`). All communication is via `DeviceIoControl`.
Codes defined in `Common.h`:

```c
#define NEXUS_WFP_PROTOCOL_VERSION 1

// METHOD_BUFFERED for short messages; METHOD_OUT_DIRECT where the
// kernel writes large buffers (audit events). FILE_ANY_ACCESS because
// the device DACL restricts access to LocalSystem + the agent token.

#define IOCTL_NEXUS_WFP_HELLO \
    CTL_CODE(FILE_DEVICE_NETWORK, 0x800, METHOD_BUFFERED, FILE_ANY_ACCESS)

#define IOCTL_NEXUS_WFP_SET_PROXY_PORT \
    CTL_CODE(FILE_DEVICE_NETWORK, 0x801, METHOD_BUFFERED, FILE_ANY_ACCESS)

#define IOCTL_NEXUS_WFP_PUSH_POLICY \
    CTL_CODE(FILE_DEVICE_NETWORK, 0x802, METHOD_BUFFERED, FILE_ANY_ACCESS)

#define IOCTL_NEXUS_WFP_GET_ORIG_DST \
    CTL_CODE(FILE_DEVICE_NETWORK, 0x803, METHOD_BUFFERED, FILE_ANY_ACCESS)

#define IOCTL_NEXUS_WFP_AUDIT_PUMP \
    CTL_CODE(FILE_DEVICE_NETWORK, 0x804, METHOD_OUT_DIRECT, FILE_ANY_ACCESS)
```

Request/response layouts:

- **HELLO** — in: `{ uint32 protocolVersion; uint32 agentPid }`; out:
  `{ uint32 driverProtocolVersion; uint32 capabilities }`. Used to
  detect version mismatch on agent startup. `capabilities` is a
  bit-set of optional features the driver supports
  (`NEXUS_CAP_IPV6_REDIRECT = 0x1`, `NEXUS_CAP_UDP_REDIRECT = 0x2`,
  `NEXUS_CAP_KILL_SWITCH = 0x4`); a v1 driver returns
  `0x1 | 0x2 | 0x4`. New capabilities are added in higher bits
  without bumping `protocolVersion` as long as the wire layouts in §6
  and §7 are unchanged.
- **SET_PROXY_PORT** — in: `{ uint16 tcpPort; uint16 udpPort }`. One
  call at agent boot; driver caches values for the redirect callouts.
- **PUSH_POLICY** — in: serialised policy table (process allowlist,
  destination match rules, kill-switch flag). Wire format defined in
  §7.
- **GET_ORIG_DST** — in: `{ uint16 localPort; bool isUdp }`; out:
  `{ uint8 origDstAddr[16]; uint16 origDstPort; uint8 family;
      uint32 processId }`. Looks up by localPort in FlowTable.
- **AUDIT_PUMP** — inverted-call pattern. Agent posts N overlapped
  IRPs; driver completes one IRP per redirect event with the flow
  metadata. Agent immediately re-posts. Buffer size 4 KB per IRP,
  packed `NexusFlowAuditEntry` records. Buffer drain rate ~10k
  events/sec sustained on a typical Surface Pro 11.

The audit pump (E84) is the kernel→user telemetry path; the policy
push (PUSH_POLICY) is the user→kernel control path. The two channels
are independent — a stalled audit pump must not block a policy push.

---

## 7. Policy wire format (PUSH_POLICY body)

```
+--------+--------+--------+--------+
| u32 version    (== NEXUS_WFP_     |
|                  PROTOCOL_VERSION,|
|                  driver rejects   |
|                  mismatched body) |
+--------+--------+--------+--------+
| u32 generation (monotonic)        |
+--------+--------+--------+--------+
| u8  killSwitch  (0 or 1)          |
+--------+--------+--------+--------+
| u32 processBypassCount            |
+-----------------------------------+
| processBypass[]: u32 pid each     |  // self-pid + tray + dashboard
+-----------------------------------+
| u32 destBypassCount               |
+-----------------------------------+
| destBypass[]: { u8 family;        |
|                  u8 prefixLen;    |
|                  u8 addr[16] }    |  // CIDR allowlist
+-----------------------------------+
```

Driver swaps the active policy atomically (`InterlockedExchangePointer`
on a `NEXUS_POLICY*` pointer). Old policy freed at next IRQL=PASSIVE
visit via a deferred work-item — never freed inside a callout (callouts
run at DISPATCH and cannot deref a freed alloc).

**Kill switch behavior.** When `killSwitch=1`, the redirect callout
returns `FWP_ACTION_PERMIT` without redirecting (passthrough),
matching macOS kill-switch semantics. User-mode-side enforcement
(refusing to accept on proxyPort) is also done independently — defense
in depth.

---

## 8. Cross-Arch Build

```
packages/agent/platform/windows/nexus-wfp-driver/
├── Driver.c
├── Callouts.c
├── Ioctl.c
├── Filter.c              # user-mode-style filter registration via FwpmEngine
├── Common.h
├── nexus-wfp.inf         # NT$ARCH$ sections for x64 + arm64
├── nexus-wfp.vcxproj     # PlatformToolset = WindowsKernelModeDriver10.0
└── build.bat             # invokes msbuild for x64 then arm64
```

`build.bat` invokes msbuild twice:

```
msbuild nexus-wfp.vcxproj /p:Configuration=Release /p:Platform=x64
msbuild nexus-wfp.vcxproj /p:Configuration=Release /p:Platform=ARM64
```

Outputs:

```
bin/x64/Release/nexus-wfp.sys      (~80 KB after strip)
bin/ARM64/Release/nexus-wfp.sys    (~80 KB)
nexus-wfp.inf                       single INF, both arch sections
```

`bin/x64/Release/nexus-wfp.cat` and `bin/ARM64/Release/nexus-wfp.cat`
are generated by the signing step (§9), not the compile step. The INF
references both `.sys` paths so a single signed CAT covers both arches.

**INF NT$ARCH$ sections:**

```inf
[Manufacturer]
%ManufacturerName% = Standard,NT$ARCH$

[Standard.NTamd64]
%DeviceDescription% = NexusWfpInstall, Root\NexusWFP

[Standard.NTarm64]
%DeviceDescription% = NexusWfpInstall, Root\NexusWFP

[NexusWfpInstall.NTamd64]
CopyFiles = NexusWfpFiles.amd64

[NexusWfpInstall.NTarm64]
CopyFiles = NexusWfpFiles.arm64
```

---

## 9. Signing

Three-stage Microsoft attestation flow:

1. **EV code signing cert** (Authenticode) — DigiCert or Sectigo,
   ~$300-500/year. Signs the `.cat`/`.sys` for upload.
2. **Microsoft Hardware Dev Center** registration — one-time $99
   programme fee plus company verification (1-2 weeks). Allows
   submission of unsigned drivers for attestation.
3. **Attestation submission** — upload `.cat` + `.sys` + signed
   `nexus-wfp.inf` to the Hardware portal; receive a Microsoft-signed
   CAT back in 1-3 hours. Embed the returned CAT in the MSI.

Repeat per release tag. Submission scripted in
`packages/agent/platform/windows/scripts/sign-driver.ps1` (E59-S5).

**Test-signed builds** for development: drivers compiled in debug mode
are signed with a local Authenticode cert, and dev machines run with
`bcdedit /set testsigning on`. Test-signed binaries never ship.

---

## 10. MSI Install Sequencing

`wfp.wxi` replaces `windivert.wxi` (E59-S3). Install order:

1. `InstallFiles` — `.sys` files into `%SystemRoot%\System32\drivers\`,
   `.inf` + `.cat` into a staging dir under `%SystemRoot%\inf\OEM`.
2. **CA `NexusWfpDriverInstall`** (deferred no-impersonate, after
   InstallFiles): `pnputil /add-driver nexus-wfp.inf /install`.
   pnputil resolves NT$ARCH$ at install time, picking the correct
   `.sys` for the running OS architecture.
3. **CA `NexusWfpServiceStart`** (deferred no-impersonate, after
   `NexusWfpDriverInstall`, before `InstallServices`):
   `sc.exe start NexusWFP`. We don't rely on auto-start because INF
   StartType ServiceStart=2 is honoured only at next boot — we want
   the driver up *now* so the agent service can use it.
4. `InstallServices` — NexusAgent user-mode service created with
   `ServiceDependency Id="NexusWFP"`. SCM will refuse to start the
   user-mode service unless the kernel service is running, preserving
   the first-packet-gap invariant.
5. `StartServices` — starts NexusAgent. Driver is already up by §10.3,
   so dependency resolves immediately.

Uninstall order is the reverse, with `pnputil /delete-driver
nexus-wfp.inf /uninstall` removing the driver package.

---

## 11. Comparison vs. WinDivert (what we're throwing away)

| Concern | WinDivert (today) | WFP (new) |
|---|---|---|
| Layer | Network (raw IP packets) | Transport (connect metadata) |
| Action on intercept | Capture+rewrite+checksum+re-inject every packet | Stamp destination once at connect; kernel does the rest |
| IPv6 | Manual segment reassembly + checksum | Native — separate V6 callout layer, same logic |
| Fragmentation | Hand-maintained reassembly buffer | Not an issue (transport layer is above fragmentation) |
| QUIC/UDP | Per-packet capture | First-packet only via REDIRECT |
| CPU cost | Per-packet copy + checksum | Per-connection metadata read |
| ARM64 support | None upstream; LGPLv3 fork required | Native |
| Driver origin | 3rd party (basil00) | In-house |
| Attestation signing | Already done by upstream | We do our own (§9) |

The performance delta is significant in theory (per-packet capture cost
vs per-connection cost). E59-S6 establishes a baseline measurement and
sets pass/fail criteria.

---

## 12. Failure modes and fall-throughs

### 12.1 Driver load fails (e.g. signing rejection on a customer box)

`sc start NexusWFP` returns Win32 error 577 ("Windows cannot verify the
digital signature"). MSI install fails — but the user-mode binaries are
already on disk. Without the driver, the agent's E59-S2 platform shim
detects `OpenSCManager` + `OpenService("NexusWFP")` = failure and
transitions to `SystemProxyFallback` mode (set system proxy to
`127.0.0.1:proxyPort`, tray icon yellow, dashboard surface alert).

This is the same fallback the WinDivert path already implements (see
`NexusAgent.wxs:226-259`). The fallback is degraded but functional —
exactly the ARM64 story we have today before E59 ships, except now
caused by a one-off signing problem on amd64 instead of a permanent
arch limitation.

### 12.2 Daemon disconnects (user-mode crashes, exit, IO stall)

Driver's audit-pump IRP queue drains; no more inverted-call IRPs
available. Driver enters fail-open per D4 — `NexusConnectRedirectV4`
returns `FWP_ACTION_PERMIT` without redirecting until a new agent
process posts HELLO + audit-pump IRPs.

### 12.3 IOCTL protocol-version mismatch

`HELLO` returns `driverProtocolVersion != NEXUS_WFP_PROTOCOL_VERSION`.
Agent refuses to push policy or post audit-pump IRPs — driver stays
fail-open. Surface a "driver/daemon version mismatch" alert in the
dashboard. Resolution: MSI upgrade.

### 12.4 Kill switch

Agent flips `killSwitch=1` in the next PUSH_POLICY. Driver swaps
policy atomically; subsequent connect callouts return
`FWP_ACTION_PERMIT` (passthrough). User-mode proxy port stops
accepting in parallel. Both layers must be passthrough for kill switch
to be a real kill switch.

---

## 13. Open questions (to resolve before code lands)

| Q | Owner | Resolution by |
|---|---|---|
| Q1: Process attribution for sandboxed apps — does WFP report the host process or the AppContainer child? | E59-S1 implementer | First spike on Edge + Store-app traffic |
| Q2: WSL2 traffic — does it hit the WFP outbound layer or the Hyper-V vSwitch? | E59-S6 testing | Empirical on a WSL2-Ubuntu VM |
| Q3: Wireshark interaction — Wireshark uses Npcap (NDIS LWF). Do our callouts see Wireshark-captured traffic differently? Should not matter (different layer) but verify. | E59-S6 testing | Empirical |
| Q4: Boot-start vs auto-start. Currently designing for `Start="auto"` (kernel driver type, auto). Boot-start would close even the gap from `winload.exe` handing off to the SCM, but boot-start drivers cannot rely on user-mode IPC until SCM has come up (so HELLO + PUSH_POLICY land later than the first network activity, which would force longer fail-open windows). | Drift to E59-S6 perf/security review | Before GA |

Each open question becomes a story-level acceptance criterion in
`docs/developers/specs/e59-s*.md`.

---

## 14. Memory anchors

- `[[project_e59_wfp_migration]]` — Epic-level tracking
- `[[feedback_kernel_driver_attestation_cost]]` — operational cost of
  Microsoft Hardware Dev Center attestation, learned during E59
