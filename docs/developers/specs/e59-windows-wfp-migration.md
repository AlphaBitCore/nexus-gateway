# E59 — Windows WFP Interception Layer Migration

**Status:** Design — pending implementation
**Date:** 2026-05-24
**Author:** brainstorm — nexus + Claude
**Architecture:** `docs/developers/architecture/agent-windows-wfp-driver.md`
**Supersedes:** E42 Phase B (WinDivert kernel interception) — fully deleted
by this epic.

---

## 1. Goal

Replace the Windows agent's traffic interception layer — currently based
on the third-party WinDivert kernel driver — with an in-house Windows
Filtering Platform (WFP) callout driver. Restore feature parity for
ARM64 Windows customers (Surface Pro 11, Snapdragon X Elite laptops)
and remove the indefinite dependency on an external kernel driver whose
upstream has no ARM64 build and no published roadmap to add one.

## 2. Why

### 2.1 The triggering problem

WinDivert 2.2.2 ships only `x64/` and `x86/` artefacts. Upstream issues
#236 ("Build for ARM64 version") and #379 ("arm/arm64 Architecture")
have been open since 2022 and 2023 respectively, with no maintainer
response. Windows ARM64 enforces kernel-mode driver architecture
matching at load time: an x64 `.sys` cannot be loaded under an ARM64
kernel, period. The agent on an ARM64 customer machine therefore loses
WinDivert entirely and is forced into `SystemProxyFallback` mode —
which covers HTTPS-via-system-proxy clients but misses QUIC, custom
binary protocols, and any application that bypasses system proxy
configuration.

ARM64 Windows is no longer a niche surface: Microsoft Copilot+ PCs
ship Snapdragon-class ARM64 hardware as the default consumer laptop
line in 2026; enterprise customers in regulated verticals (finance,
legal) are increasing ARM64 procurement for high-mobility users
(executives, sales). Telling these customers "the compliance agent
runs in degraded mode on your fleet" is not a viable long-term answer.

### 2.2 Why fork WinDivert is the wrong fix

Rejected: maintain a forked WinDivert with our own ARM64 build.

- Requires building a Windows kernel driver from third-party C source
  inside our CI on every release tag (the WDK build).
- Every Windows kernel-mode ABI update requires re-validating the fork
  — we'd own a permanent maintenance load on driver code we did not
  design.
- The upstream BSD/LGPLv3 dual licence creates ambiguity about which
  half applies to derivative kernel drivers (the `.sys` is dynamically
  linked, but kernel drivers in NTKernel don't conform cleanly to
  "library" semantics).
- We still need Microsoft Hardware Dev Center attestation signing for
  our fork; the upstream signed-binary inheritance breaks the moment
  we change a byte.

### 2.3 Why WFP is the right fix

- **First-party Microsoft API.** WFP is built into every supported
  Windows release (Vista+); ARM64 support is automatic because
  Microsoft ships ARM64 Windows. Long-term support is not at risk.
- **Higher abstraction.** WFP exposes connect-time decision callouts
  (`ALE_CONNECT_REDIRECT_V4/V6`) that let us redirect by changing
  connect metadata — no need to capture, modify, recompute checksums,
  and re-inject packets the way WinDivert forces.
- **Single source tree, cross-arch.** The WDK supports building one
  driver project for both x64 and arm64 with a single `vcxproj`. The
  INF declares both arches; pnputil picks the right `.sys` at install
  time. No forked code paths.
- **Same fail-safe model.** The WFP callout layer can return
  `FWP_ACTION_PERMIT` (passthrough) in seconds when the user-mode
  daemon is absent — same failure-mode guarantee CLAUDE.md requires
  for the macOS NE proxy, expressed in identical primitives.

### 2.4 Sequencing

Two cost lines run in parallel:

- **Engineering** — this epic, six stories (S1-S6), estimated 8-10
  engineering weeks for the implementation + 2-3 weeks for end-to-end
  testing. See §4 for the story breakdown.
- **Operational** — Microsoft Hardware Dev Center registration ($99 +
  1-2 weeks company verification) and EV code-signing cert
  procurement ($300-500/year, 1-3 weeks issuance). These are not on
  the engineering critical path until S5 lands; they should be
  initiated by the product owner the same week S1 starts.

---

## 3. Functional requirements

### FR-1 — Outbound TCP interception (MUST)

The driver MUST intercept every outbound TCP connect by an
arbitrary process and decide:

1. **Permit + redirect** — rewrite the connect target to
   `127.0.0.1:proxyPort` and stamp the original destination into the
   FlowTable keyed by source port (per architecture §5.1).
2. **Permit** — let the connect proceed unchanged (e.g. for the
   agent's own outbound traffic to Hub, or for destinations in the
   bypass CIDR list).
3. **Block** — return `FWP_ACTION_BLOCK` (kill-switch + admin-block
   policies).

Coverage MUST include IPv4 and IPv6, in user-mode and elevated
processes, in default and AppContainer security contexts.

### FR-2 — Outbound UDP first-packet interception (MUST)

The driver MUST intercept the first outbound UDP packet per (5-tuple)
in the same way as FR-1 (same callout layer, same decision model). The
UDP table is keyed by source port; the FlowTable entry expires when
the proxy reports the flow as closed via IOCTL_NEXUS_WFP_FLOW_CLOSE
(spec'd in E59-S2).

QUIC (HTTP/3 over UDP/443) is the primary motivator; DNS-over-UDP/53
and discovery protocols (mDNS, SSDP) are also covered.

### FR-3 — Original-destination lookup (MUST)

The user-mode proxy MUST be able to retrieve the original destination
of any redirected flow given the local (proxy-side) port number, via
IOCTL_NEXUS_WFP_GET_ORIG_DST. The lookup MUST return within 1 ms in
steady state (no I/O, in-memory hash map under
EX_SPIN_LOCK acquire-release).

### FR-4 — Policy push (MUST)

The user-mode agent MUST be able to atomically replace the active
policy (process bypass list, destination bypass CIDR set, kill-switch
flag, generation counter) via IOCTL_NEXUS_WFP_PUSH_POLICY. The driver
MUST swap the active policy atomically (no half-applied window) and
free the old policy at PASSIVE_LEVEL via a deferred work-item.

### FR-5 — Audit pump (MUST)

The user-mode agent MUST receive a record for every redirected (or
explicitly blocked) connect, containing:
processId, processCreatorPid, sourceIp/port, originalDstIp/port,
protocol, decision, timestampUs. Throughput target: 10k events/sec
sustained on a Surface Pro 11 (E59-S6 baseline).

### FR-6 — Fail-open under daemon disconnect (MUST, safety-critical)

Per architecture D4: if the agent user-mode process has not posted at
least one inverted-call audit-pump IRP, OR if no PUSH_POLICY has been
received since boot, the driver MUST default to `FWP_ACTION_PERMIT`
(passthrough) without modifying any flow. This mirrors the macOS NE
proxy fail-open invariant (CLAUDE.md → mandatory rules → macOS NE).

### FR-7 — Cross-arch single source (MUST)

One driver source tree compiles for both `amd64` and `arm64`. One INF
declares both `Standard.NTamd64` and `Standard.NTarm64` sections. One
MSI ships both `.sys` outputs; pnputil at install time picks the
matching one based on `MsiAMD64` / `MsiARM64` properties. Per
architecture D3, no per-arch `#ifdef` is permitted in callout or
IOCTL logic — only in WDK-provided headers where Microsoft itself
already gates with `_ARM64_` / `_AMD64_`.

### FR-8 — MSI install/uninstall cleanliness (MUST)

`msiexec /i` installs and starts the driver + agent service in correct
order (architecture §10). `msiexec /x` stops + uninstalls both, leaves
no orphan registry entries, no orphan WFP filters, no orphan files
under `%SystemRoot%\System32\drivers\` or `%SystemRoot%\inf\OEM\`.

### FR-9 — Process bypass for agent self-loop (MUST)

The driver MUST recognise the agent's own user-mode process (by
PID, established at HELLO time) and never redirect that process's
outbound traffic. Otherwise the agent's connection to Hub would itself
be redirected to the local proxy, creating a loop.

This is **complementary to** the general `processBypass` list in FR-4
(policy push): FR-4's list is updated by PUSH_POLICY for tray /
dashboard / arbitrary admin-configured PIDs; FR-9's self-PID is
hard-bound at HELLO time and survives policy generations — it must
remain bypassed even if the user-mode side has not yet pushed a
policy (avoiding a HELLO→first-PUSH window where the agent could
loop on itself).

### FR-10 — Kill switch (MUST)

When PUSH_POLICY sets `killSwitch=1`, the driver MUST return
`FWP_ACTION_PERMIT` for every connect (passthrough) without altering
the flow. The user-mode proxy SHOULD also stop accepting on
`proxyPort` independently — defence in depth.

---

## 4. Non-functional requirements

### NFR-1 — Performance

- Redirect-callout overhead per connect: < 50 µs p50, < 200 µs p99,
  measured on **both** baseline rigs (E59-S6 runs the same test on
  each):
  - **amd64 baseline:** Lenovo ThinkPad X1 Carbon Gen 12 (Intel Core
    Ultra 7 165U, 32 GB).
  - **arm64 baseline:** Surface Pro 11 (Snapdragon X Elite, 32 GB).
- Steady-state CPU overhead (per host): < 0.5% of one core on each
  baseline rig.
- Baseline established in E59-S6 vs the WinDivert path on the
  amd64 rig running an identical workload (1000 connect/s). The
  arm64 rig has no WinDivert baseline — the comparison is against
  the `SystemProxyFallback` mode it ships with today.

### NFR-2 — Reliability

- Driver MUST NOT BSOD under any IOCTL input (including
  malformed/oversized buffers — `METHOD_BUFFERED` enforces a max).
- Driver Verifier with all standard options enabled MUST pass under
  a 30-minute synthetic stress run (E59-S6).
- IRP cancellation paths exercised — IO_REQUEST_CANCEL_FUNCTION
  installed on every audit-pump IRP, no orphan IRPs on agent exit.

### NFR-3 — Cross-arch fidelity

- Functional test matrix MUST pass on both amd64 and arm64 Windows
  11 24H2 (or later). Test list in E59-S6.

### NFR-4 — Observability

- Driver emits ETW events on:
  - DriverEntry / DriverUnload
  - Callout registration / unregistration
  - Each policy push (generation, killSwitch state)
  - Audit pump queue depth (sampled every second)
  - IOCTL errors (with the requesting agent PID)
- ETW provider GUID + manifest committed alongside the driver code.

### NFR-5 — Security

- Device DACL restricts open to LocalSystem + the SID we stamp on
  the agent's user-mode service account.
- No unbounded allocations from IOCTL input (every length-prefixed
  field is bounded by a constant declared in `Common.h`).
- All input buffers validated for length BEFORE any field
  dereference (defence against truncated IOCTL writes).

### NFR-6 — Signing

- Production builds carry a Microsoft Hardware Dev Center attestation
  signature embedded in the `.cat` and the `.sys` PE header.
- The submission, signing key custody, and CAT-back-in-MSI
  embedding flow is documented in E59-S5 (`sign-driver.ps1`).

### NFR-7 — Auditability

- Every IOCTL is logged at INFO level into the user-mode agent log
  with: code, requesting PID, bytes in, bytes out, NTSTATUS result.
- Driver-side ETW events feed the same audit trail when sampled.

---

## 5. User roles and personas

| Role | Why E59 matters | What changes for them |
|---|---|---|
| **End user on ARM64 Windows** (Surface Pro 11, Snapdragon X Elite) | Currently sees yellow tray + degraded mode | Full TCP + UDP + QUIC interception, identical to amd64 |
| **End user on amd64 Windows** | Currently sees WinDivert-based interception | Identical functional surface; driver swap is transparent unless they read Process Explorer |
| **Org admin** | Currently picks a "compatible" rollout strategy based on which fleet machines are ARM64 | Single MSI ships to entire fleet; no per-arch packaging |
| **Compliance officer / auditor** | Currently sees gaps in audit trail for ARM64 endpoints | Audit completeness restored — every redirected connect emits a record on every supported arch |
| **SRE / oncall** | Currently triage tickets blaming "macOS-style" passthrough on ARM64 endpoints | Failure surface narrows to driver-load/signing problems instead of "the platform doesn't support our agent" |

---

## 6. Constraints and assumptions

### Constraints

- **C-1.** WFP callout drivers cannot be unloaded if any flow still
  has a pending decision IRP. Driver unload (NexusWFPUnload) must
  cancel all outstanding IRPs at PASSIVE_LEVEL before
  `FwpsCalloutUnregisterByKey0` returns success.
- **C-2.** Driver attestation by Microsoft can take 1-3 business hours
  per submission, sometimes more under embargo (Patch Tuesday). The
  release pipeline must tolerate a 24-hour CAT-return window without
  blocking other work.
- **C-3.** The agent service depends on the WFP service
  (`ServiceDependency Id="NexusWFP"`). SCM enforces dependency
  ordering on `StartService` calls but does NOT enforce reverse
  ordering on `StopService` — uninstall MUST stop NexusAgent before
  NexusWFP, otherwise the user-mode service holds open IRPs and the
  driver-stop call hangs.
- **C-4.** Reboot-not-required is a goal (the existing WinDivert path
  doesn't require a reboot to install or uninstall). The pnputil
  `/install` flag triggers a class installer that may schedule a
  reboot if a competing filter driver is present; the install script
  MUST detect ERROR_SUCCESS_REBOOT_REQUIRED and surface it to the
  MSI for user prompt.

### Assumptions

- **A-1.** Customer Windows kernels are 24H2 or later. WFP
  ALE_CONNECT_REDIRECT_V4 was introduced in Windows 8.1; the
  semantics relied on here have been stable since 22H2. We do not
  attempt to support Windows 10 21H2 or earlier.
- **A-2.** No customer environment uses competing connect-redirect
  callouts (e.g. PaloAlto GlobalProtect, Cisco AnyConnect). If two
  drivers both redirect, the result is undefined (WFP serialises
  callouts by `weight` field; whoever has higher weight wins last).
  Empirical compatibility testing with the top three enterprise VPN
  clients is in E59-S6.
- **A-3.** The agent's proxy listener can bind both a TCP and a UDP
  socket to the same numeric port on `127.0.0.1` — confirmed in
  E59-S2 by code inspection of the existing listener.

---

## 7. Glossary

| Term | Meaning |
|---|---|
| **WFP** | Windows Filtering Platform — Microsoft's kernel network-policy framework. |
| **Callout** | A kernel function registered with WFP that runs at a specified layer when matching traffic appears. |
| **ALE** | Application Layer Enforcement — the WFP sub-layer family that fires at socket-state transitions (connect, accept, recv). |
| **Redirect handle** | Opaque handle returned by `FwpsRedirectHandleCreate0`, used to commit a connect-target modification. |
| **Inverted call** | Driver design pattern where user-mode posts N OVERLAPPED IRPs and the driver completes one per event — avoids polling. |
| **FlowTable** | In-driver hash map keyed by source port, value = original destination, used by GET_ORIG_DST lookup. |
| **pnputil** | Microsoft CLI for installing/uninstalling driver packages by INF. |
| **Attestation signing** | Microsoft Hardware Dev Center workflow that issues a Microsoft signature for a driver after automated validation. |
| **Boot-start driver** | Driver loaded by `winload.exe` before SCM comes up — earliest possible load order. |
| **`Start="auto"` driver** | Driver loaded by SCM during boot, after winload — slightly later than boot-start. Default for our design. |

---

## 8. MoSCoW

### Must — required for the epic to ship

- FR-1, FR-2, FR-3, FR-4, FR-5, FR-6, FR-7, FR-8, FR-9, FR-10
- NFR-1, NFR-2, NFR-3, NFR-5, NFR-6

### Should — strong preference, defer if time-constrained

- NFR-4 (ETW observability) — without this, prod debugging is
  noticeably harder, but the driver is operable.
- NFR-7 (per-IOCTL audit log) — info-level logging in agent;
  default-on but cheap to back out.

### Could — nice-to-have, may slip past GA

- Per-process throughput accounting (the policy table already keys
  by PID; we could surface bytes-in/bytes-out per PID without
  significant extra design).
- Connection-resume across driver reload (if the driver is upgraded
  without rebooting, the FlowTable is gone and in-flight flows
  break; recovering them would require persisting FlowTable state).

### Won't — explicitly out of scope

- Inbound flow interception (listener-side).
- L2/L3 packet capture for diagnostics — use pktmon.
- 32-bit x86 builds — Windows 11 has no 32-bit edition.
- Pre-22H2 Windows 10 support.
- Replacing the macOS / Linux interception paths — separate epics.

---

## 9. Acceptance criteria (epic-level — see stories for fine-grained)

The epic is complete when all of the following hold:

1. **Install + run on amd64.** `msiexec /i NexusAgent-<v>.msi /qb` on
   a clean Windows 11 24H2 amd64 machine results in:
   - NexusWFP driver installed in `%SystemRoot%\System32\drivers\`
     and registered as a kernel-mode SCM service.
   - NexusAgent user-mode service running, healthy, talking to Hub.
   - A `curl` to a non-trivial external HTTPS endpoint
     (e.g. `https://example.com`) is intercepted and the request
     emits a `traffic_event` row with `source=compliance-proxy` and
     `agent_pid != 0`.
2. **Install + run on arm64.** Same as (1), on a Surface Pro 11
   running Windows 11 24H2 ARM64.
3. **Uninstall cleanliness.** `msiexec /x NexusAgent-<v>.msi /qb`
   removes both services, both files, no orphans, no reboot
   required.
4. **Driver Verifier — clean.** A 30-minute Driver Verifier run with
   Standard + DDI Compliance Checking + IRP Logging enabled on the
   NexusWFP driver records zero violations.
5. **Performance baseline.** E59-S6 perf test reports redirect
   callout p99 < 200 µs at 1000 connect/s on Surface Pro 11.
6. **Audit completeness.** E59-S6 functional test reports zero
   missing audit events under a 10-minute mixed-traffic workload
   (TCP + UDP + QUIC + IPv6).
7. **Doc lockstep.** This file, the architecture doc, all six story
   SDDs, and the WiX wxs files are in lockstep: no story SDD
   describes behaviour absent from this epic file; no field appears
   in `Common.h` without a row in the architecture doc IOCTL table.

---

## 10. Story breakdown (see individual SDDs)

| Story | Subject | Estimate | Blocking |
|---|---|---|---|
| E59-S1 | Driver C skeleton + callout registration | 2 weeks | WDK environment |
| E59-S2 | User-mode Go integration (replace windivert_windows.go) | 2 weeks | E59-S1 IOCTL contract frozen |
| E59-S3 | MSI changes (wfp.wxi, pnputil CAs, delete windivert.wxi) | 1 week | E59-S1 driver compiles |
| E59-S4 | Cross-arch build (ARM64 .sys output, INF NT$ARCH$ sections) | 1 week | E59-S1 builds on amd64 |
| E59-S5 | Signing pipeline (Hardware Dev Center attestation) | 1 week + Microsoft turnaround | EV cert procured, Dev Center registered |
| E59-S6 | Testing — Driver Verifier + functional matrix + perf baseline | 2-3 weeks | E59-S5 signed builds available |

Stories are intentionally orderable, not strictly sequential — S2 can
begin once S1 has a frozen IOCTL contract even if the driver isn't
fully implemented; S4 can begin once S1 has any compilable output.

---

## 11. Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Attestation rejected by Microsoft for code-quality reasons (Driver Verifier or HLK signal) | Medium | High — blocks GA | E59-S6 runs Driver Verifier and HLK Filter Driver tests before submission |
| Attestation turnaround exceeds 24 hours on a release candidate | Medium | Medium — slips RC by a day | Schedule signing 48h before GA; never block GA on the same-day submission |
| Competing connect-redirect drivers in customer environments (enterprise VPN) | Low to Medium | Medium — silent misrouting | E59-S6 compatibility matrix; documentation + telemetry to detect the case |
| Kernel API ABI break on a future Windows feature update | Low | High — every release-tag retest | NFR-2 Driver Verifier on every release; monitoring KB articles for WFP ABI notes |
| EV cert key compromise | Low | Critical — Microsoft revokes all signed CATs | E59-S5 MUST require the EV cert private key live on a FIPS-140-2 HSM (YubiKey 5 FIPS or equivalent) with PIN-and-touch acknowledgement on every sign; signing only from a designated build workstation in the build room |

---

## 12. References

- `docs/developers/architecture/agent-windows-wfp-driver.md` —
  authoritative design.
- `packages/agent/platform/windows/installer/NexusAgent.wxs:226-259`
  — incumbent WinDivert architecture rationale (E42 Phase B), still
  the source of the "first-packet gap" requirement E59-S1 must
  preserve.
- Microsoft, [WFP Sample Drivers](https://github.com/microsoft/Windows-driver-samples/tree/main/network/trans).
- Microsoft, [Connect Redirect via WFP](https://learn.microsoft.com/en-us/windows/win32/fwp/proxying).
- WinDivert upstream issues #236, #379 — context for why we can't
  rely on WinDivert long term.

---

## 13. Memory anchors

- `[[project_e59_wfp_migration]]` — Epic tracking
- `[[feedback_msi_arch_x64_flag]]` — Lesson from the E42 MSI build
  (the `-arch x64` flag must be set explicitly; carried forward into
  E59-S3 wxs)
- `[[reference_microsoft_hardware_dev_center]]` — Attestation flow
  pointers
