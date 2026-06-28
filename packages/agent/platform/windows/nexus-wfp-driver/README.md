# NexusWFP Kernel Driver

In-house Windows Filtering Platform (WFP) callout driver that
replaces WinDivert as the Nexus Agent's traffic interception layer.

**Status:** Implemented and working (E59-S1/S2). Loads, registers the
four callouts (V4/V6 connect-redirect + V4/V6 QUIC-block), exposes the
IOCTL contract, and transparently redirects outbound **TCP** connections
to the agent's local proxy (`127.0.0.1:<proxyPort>`); the agent recovers
the original destination (driver FlowTable / `GET_ORIG_DST`) and
MITM-inspects the flow, over both IPv4 and IPv6. UDP passes through
untouched **except** UDP/443 from processes on the admin-pushed
QUIC-fallback allowlist, which is blocked at `ALE_AUTH_CONNECT` so those
apps fall back to interceptable TCP/443 (macOS-parity
QUIC-force-TCP-fallback; protocol v2). Verified end-to-end on Windows 11
24H2 (client → kernel redirect → agent MITM → real upstream → audit).
Driver signing for stock (non-test-signed) Windows is E59-S5.

- **Architecture:** [`docs/developers/architecture/agent-windows-wfp-driver.md`](../../../../../docs/developers/architecture/agent-windows-wfp-driver.md)
- **Epic:** [`docs/developers/specs/e59-windows-wfp-migration.md`](../../../../../docs/developers/specs/e59-windows-wfp-migration.md)
- **Stories:** E59-S1 driver skeleton · E59-S2 user-mode Go ·
  E59-S3 MSI · E59-S4 cross-arch · E59-S5 signing · E59-S6 testing

## Source tree

| File | Purpose |
|---|---|
| `Common.h` | Shared definitions — IOCTL codes, wire structs, GUIDs |
| `Driver.c` | DriverEntry / EvtDriverUnload / device-object setup |
| `Callouts.c` | The four classify functions + callout registration |
| `Filter.c` | FwpmEngine session + sublayer + filters (binds each callout to its layer) |
| `Ioctl.c` | IOCTL dispatcher (HELLO / SET_PROXY_PORT / PUSH_POLICY / GET_ORIG_DST / AUDIT_PUMP) |
| `nexus-wfp.inf` | INF manifest, NT$ARCH$ for amd64 + arm64 |
| `nexus-wfp.vcxproj` / `.sln` | WDK KMDF project, both platforms |
| `build.bat` | Drives msbuild for x64 and ARM64 |

## Building

Prerequisites:

- Windows 11 amd64 build host (WDK arm64-on-arm64 builds are not yet
  battle-tested in our CI matrix).
- Visual Studio 2022 Build Tools (or full IDE) with the
  "Desktop development with C++" workload, including the v143 toolset
  for x64 AND ARM64.
- Windows Driver Kit (WDK) 11 24H2 or later, with both x64 and
  ARM64 build tools selected at install time.
- Spectre-mitigated libraries for both arches (extra component in
  the VS installer; required because the project sets
  `<SpectreMitigation>Spectre</SpectreMitigation>`).

```powershell
cd packages\agent\platform\windows\nexus-wfp-driver
pwsh -NoProfile -File build.ps1              # x64 Release; -Platform both for x64 + ARM64
```

`build.ps1` is the verified path. It auto-selects the newest installed Windows
SDK that actually has the `km\` kernel headers and pins it via
`WindowsTargetPlatformVersion` — without that pin msbuild resolves the bundled
Desktop SDK (no `km\`) and fails with `error C1083: Cannot open include file:
'ntddk.h'`. It also sets `DriverVer`/`STAMPINF_VERSION` (stampinf errors 87
otherwise) and skips the `.cat` catalog packaging (`SkipPackageVerification` +
`EnableInf2cat=false`) — the catalog is release-only (E59-S5) and needs WDK
tooling (`x86\InfVerif.dll`) absent from a plain WDK install. `build.bat` is the
older entry point and omits these flags; prefer `build.ps1`.

Outputs (test-signed by the WDK test cert):

```
bin\x64\Release\nexus-wfp.sys
bin\ARM64\Release\nexus-wfp.sys
```

Production signing (Microsoft attestation) happens in `packages\agent\platform\
windows\scripts\sign-driver.ps1` (E59-S5).

## Loading for development

Production builds need Microsoft Hardware Dev Center attestation
(E59-S5). For dev iteration the driver loads inside the crash-safe
**NexusWFP-Debug** kernel-debug VM (never your workstation — a driver bug
bugchecks the machine). Full VM provisioning + the dev loop are documented in
[`docs/developers/workflow/windows-kernel-debug-vm.md`](../../../../../docs/developers/workflow/windows-kernel-debug-vm.md).

Host-driven and hands-off, from the **elevated Hyper-V host** (`dev-deploy.ps1`):

```powershell
pwsh -NoProfile -File dev-deploy.ps1             # build + revert checkpoint + deploy + verify
pwsh -NoProfile -File dev-deploy.ps1 -SkipBuild  # deploy an already-built .sys
pwsh -NoProfile -File dev-deploy.ps1 -Stop       # sc stop in the guest
```

It reverts the `clean-debug-ready` checkpoint, copies the `.sys` into the guest
over PowerShell Direct, trusts the test cert, `sc start`s the driver under a 45s
timeout (timeout ⇒ bugcheck → attach `kd`, revert), and asserts the
`NexusConnectRedirectV4 / V6` callouts registered.

## Uninstalling for development

```cmd
sc stop NexusWFP
pnputil /delete-driver nexus-wfp.inf /uninstall /force
```

## CLAUDE.md doc lockstep

This directory is mapped in the doc-lockstep config to BOTH the
architecture doc AND `docs/developers/specs/e59-*.md`. Any change here
that affects an IOCTL field, callout GUID, INF section, or build flag
MUST update the matching doc rows in the same PR. The architecture
doc is the source of truth; this README is a thin entry-point only.
