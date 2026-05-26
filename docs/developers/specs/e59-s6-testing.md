# E59-S6 — Testing Matrix

**Status:** Design — pending implementation
**Date:** 2026-05-24
**Epic:** [E59](e59-windows-wfp-migration.md)
**Architecture:** [agent-windows-wfp-driver.md](../architecture/agent-windows-wfp-driver.md)
**Depends on:** E59-S5 (signed builds available for stock-Windows install testing)

---

## 1. User story

> **As** the release engineer
> **I want** a defined functional + performance + security test
> matrix that the WFP driver must pass before any release tag is
> cut
> **so that** customer-facing failures (BSODs, missed audit events,
> performance regressions, install/uninstall leftovers) are caught
> in CI or in a one-day pre-release lab cycle, not in production.

## 2. Goal

Define and automate four test pillars:

1. **Static — pre-compile checks.** Driver Verifier static analysis,
   /W4 warnings, WDK Static Driver Verifier (SDV).
2. **Dynamic — load-and-stress.** Driver Verifier runtime checks
   under a 30-minute synthetic workload.
3. **Functional — feature matrix.** TCP / UDP / QUIC / IPv6 / IPv4
   × allow / block / redirect × amd64 / arm64.
4. **Performance — baseline + regression.** Per-connect callout
   p50/p99 latency, sustained throughput at 1k connect/s, CPU
   overhead.

## 3. Test environments

| Env | OS | Arch | Purpose |
|---|---|---|---|
| **amd64-stock** | Windows 11 24H2 amd64 | x64 | Customer parity install/uninstall test |
| **arm64-stock** | Windows 11 24H2 arm64 (Surface Pro 11) | ARM64 | Cross-arch parity |
| **amd64-verifier** | Windows 11 24H2 amd64 | x64 | Driver Verifier runtime checks |
| **amd64-perf** | Lenovo ThinkPad X1 Carbon Gen 12 | x64 | NFR-1 baseline rig |
| **arm64-perf** | Surface Pro 11 (Snapdragon X Elite) | ARM64 | NFR-1 baseline rig |
| **amd64-VPN-compat** | Windows 11 + GlobalProtect / AnyConnect / Zscaler | x64 | Concurrent WFP filter interaction |
| **WSL2-host** | Windows 11 with WSL2-Ubuntu | x64 | Q2 from arch §13: WSL2 visibility |

All "stock" envs run WITHOUT `bcdedit /set testsigning on` —
production-equivalent posture.

## 4. Test plans

### TP1 — Static (CI on every PR)

- `MSBuild /p:RunWdkSdv=true` — WDK Static Driver Verifier.
- `MSBuild /p:WarningLevel=Level4 /p:TreatWarningsAsErrors=true`.
- `MSBuild /p:RunCodeAnalysis=true` — VS Code Analysis.
- `git grep '#ifdef.*_M_ARM64' packages/agent/platform/windows/nexus-wfp-driver/`
  must return zero outside WDK-provided headers (architecture D3).

### TP2 — Dynamic Driver Verifier (per release tag)

`verifier /standard /driver nexus-wfp.sys` plus:

- `/flags 0x09BB` (Standard + DDI Compliance + IRP Logging +
  Concurrency Stress)
- Reboot.
- Run TP3 (functional matrix) end-to-end for 30 minutes.
- `verifier /query` MUST report zero violations.
- `verifier /reset` to clean up.

### TP3 — Functional matrix (per release tag)

For each (env in {amd64-stock, arm64-stock}) × (protocol in {TCP-v4,
TCP-v6, UDP-v4, UDP-v6, QUIC-v4, QUIC-v6}) × (decision in {permit,
block, redirect}):

- Issue traffic matching the decision policy.
- Assert observed result matches expectation:
  - **permit:** Wireshark capture shows traffic to original dst.
  - **block:** `WSAGetLastError` returns `WSAEACCES` to caller; no
    packet to original dst in Wireshark.
  - **redirect:** Wireshark shows traffic to `127.0.0.1:proxyPort`;
    proxy logs the original dst.
- Audit event emitted for each (decision, srcPort) within 100 ms.

Tooling: `tools/wfp-functional-test/` (Go test program using
`crypto/tls` + `net/http` + raw UDP socket calls). 18 cases × 2
arches = 36 test cells; runtime ~5 minutes per cell, ~3 hours total.

### TP4 — Performance baseline (per release tag, both perf rigs)

Workload: `tools/wfp-perf-driver/` issues 1000 TCP connect/s to
random destinations for 60 seconds, measuring:

- **Per-connect callout latency** — measured via ETW high-resolution
  timestamps at callout entry/exit.
  - p50 < 50 µs
  - p99 < 200 µs
- **Sustained throughput** — 1000 connect/s for 60 s without
  callout latency degrading more than 25 % between minute 1 and
  minute 60.
- **CPU overhead** — measured via `typeperf` on
  `\Processor(_Total)\% Processor Time`; agent + driver combined
  must stay below 0.5 % of one core averaged over the run.

Regression check: compare against baseline JSON committed under
`tools/wfp-perf-driver/baselines/<rig>-<arch>.json`. >10 %
regression on p50 or p99 = release blocker until investigated.

### TP5 — Install/uninstall cleanliness (per release tag)

For (env in {amd64-stock, arm64-stock}):

1. Fresh VM snapshot.
2. `msiexec /i NexusAgent-<v>.msi /qb` → expect success.
3. `sc qc NexusWFP` shows STATE: 4 RUNNING.
4. `sc qc NexusAgent` shows STATE: 4 RUNNING.
5. Issue traffic; assert audit events flow.
6. `msiexec /x NexusAgent-<v>.msi /qb` → expect success.
7. Post-uninstall assertions:
   - `Get-Service NexusWFP` returns nothing.
   - `Get-Service NexusAgent` returns nothing.
   - `Get-ChildItem 'C:\Windows\System32\drivers\nexus-wfp*'` empty.
   - `Get-ChildItem 'C:\Windows\System32\DriverStore\FileRepository\nexus-wfp*'` empty.
   - `Get-ChildItem 'C:\Program Files\Nexus Agent'` empty or
     missing.
   - Registry: `HKLM:\SYSTEM\CurrentControlSet\Services\NexusWFP`
     does not exist.
   - `pnputil /enum-drivers | findstr nexus-wfp` empty.
8. `shutdown /r /t 0` → after reboot, system is functional (proves
   no orphan boot-start dependency).

### TP6 — VPN-compatibility (per release tag, amd64-VPN-compat env)

For each VPN client (PaloAlto GlobalProtect, Cisco AnyConnect,
Zscaler):

1. VPN client running and connected to a test gateway.
2. Install our MSI.
3. Issue traffic matching:
   - Both products want to redirect: who wins? Document.
   - We want to redirect, VPN doesn't: our redirect succeeds.
   - VPN wants to redirect, we don't: VPN redirect succeeds.
4. Assert: no system instability, no NULL pointer in either driver.

The "who wins" result is published as an interop matrix in the
release notes; we don't yet aim to BE the higher-priority filter.

### TP7 — WSL2 visibility (per release tag, WSL2-host env)

Resolves architecture §13 Q2.

1. WSL2-Ubuntu running.
2. Inside WSL2: `curl https://example.com`.
3. Assert: an audit event was emitted on the Windows side for the
   WSL2 traffic. If not, document that WSL2 traffic bypasses our
   layer (architectural call-out, not a defect).

### TP8 — Long-running soak (pre-GA only)

48-hour soak on each perf rig:

- 100 connect/s sustained.
- Memory growth < 1 MB over 48 hours (no leak).
- No driver-side error events in the Windows Event Log.
- Driver Verifier counters at end-of-soak: zero violations.

## 5. CI integration

- **Per-PR:** TP1 (static checks) on a Windows-amd64 runner.
- **Per-merge-to-main:** TP1 + TP3 (functional) + TP5 (install) on
  amd64 only. Arm64 is opt-in due to runner scarcity.
- **Per-release-tag:** Full matrix TP1-TP7 on both arches. TP8 soak
  before GA.

Test results published to `tests/reports/e59-<tag>/` with one
markdown file per test plan, including:
- Test ID, env, expected, observed, pass/fail, evidence
  (Wireshark capture, ETW trace, screenshot).

## 6. Acceptance criteria

This story is done when:

1. All test tooling listed in §4 lives under `tools/wfp-*/` and is
   runnable via documented `make` / PowerShell targets.
2. CI configuration runs TP1 + TP3 + TP5 on amd64 for every PR
   touching `packages/agent/platform/windows/nexus-wfp-driver/`,
   `packages/agent/internal/platform/windows/wfp_*.go`, or
   `packages/agent/platform/windows/installer/wfp.wxi`.
3. Release-tag CI runs the full matrix and publishes a unified
   markdown report.
4. The TP4 baseline JSON files are committed; subsequent regression
   check is wired.
5. At least one full TP1-TP7 dry-run has been completed
   successfully on a feature-branch tag (pre-GA shakedown).
6. Architecture doc §13 open questions Q1-Q4 are resolved
   (closed or moved into the appropriate test plan as ongoing
   monitoring) with evidence.

## 7. Risks

- **R-S6.1:** Arm64 runner scarcity — GitHub Actions doesn't offer
  hosted Windows-arm64 runners as of 2026-05. Self-hosted runners
  on a Surface Pro 11 cost a physical device and a dedicated
  network drop. Mitigation: shared runner with macos-arm64 lab,
  Windows-arm64 image supplied; documented in
  `docs/operators/ops/runbooks/arm64-runner.md`.
- **R-S6.2:** VPN-compat results are unowned-defect-prone — a
  failure with one VPN vendor might be that vendor's bug, but we
  end up triaging it because we shipped the MSI. Mitigation:
  every VPN-compat failure is filed with the VPN vendor with a
  minimal repro within 5 business days.
- **R-S6.3:** Driver Verifier false positives — DDI Compliance
  Checking has known false positives in current WDK builds.
  Mitigation: maintain a per-rule suppression file
  (`tests/wfp-driver-verifier-suppress.json`) with Microsoft KB
  references for each suppression.

## 8. Out of scope

- Driver implementation (E59-S1).
- User-mode integration tests in isolation (those live with E59-S2
  unit tests).
- Functional tests of the Hub/CP/AI Gateway pipelines — unchanged
  by E59.
