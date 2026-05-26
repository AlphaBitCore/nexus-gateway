# NexusWFP Kernel Driver

In-house Windows Filtering Platform (WFP) callout driver that
replaces WinDivert as the Nexus Agent's traffic interception layer.

**Status:** Skeleton (E59-S1). Loadable, registers four callouts,
exposes the IOCTL contract, but does not yet redirect traffic — the
implementation pass lands after architecture review.

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
| `Filter.c` | FwpmEngine session + sublayer + filters (filter wiring stubbed) |
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

```cmd
cd packages\agent\platform\windows\nexus-wfp-driver
build.bat
```

Outputs:

```
bin\x64\Release\nexus-wfp.sys
bin\ARM64\Release\nexus-wfp.sys
```

Both unsigned. Signing happens in `packages\agent\platform\windows\
scripts\sign-driver.ps1` (E59-S5).

## Loading for development

Production builds need Microsoft Hardware Dev Center attestation
(E59-S5). For dev iteration:

```cmd
REM Enable test-signed loading. Reboot required.
bcdedit /set testsigning on
shutdown /r /t 0

REM After reboot:
cd %~dp0
signtool sign /a /v /fd sha256 /tr http://timestamp.digicert.com bin\x64\Release\nexus-wfp.sys
pnputil /add-driver nexus-wfp.inf /install
sc start NexusWFP
netsh wfp show state > wfp-state.txt
findstr /i NexusConnectRedirectV4 wfp-state.txt
```

The last command should print a single match — confirmation that
the redirect-v4 callout is registered.

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
