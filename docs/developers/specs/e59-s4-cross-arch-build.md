# E59-S4 — Cross-Arch Build (amd64 + arm64)

**Status:** Design — pending implementation
**Date:** 2026-05-24
**Epic:** [E59](e59-windows-wfp-migration.md)
**Architecture:** [agent-windows-wfp-driver.md](../architecture/agent-windows-wfp-driver.md) §8
**Depends on:** E59-S1 (amd64 build works), E59-S3 (MSI staging script in place)

---

## 1. User story

> **As** an ARM64 Windows customer
> **I want** to install the same MSI my amd64 colleagues install and
> get the same interception fidelity
> **so that** my IT department doesn't have to maintain two MSIs, and
> I don't see a "degraded mode" yellow tray icon on my Surface
> Pro 11.

## 2. Goal

Two outcomes:

1. **`nexus-wfp.sys` builds for arm64** from the same source tree as
   amd64, with no `#ifdef` branching in callout or IOCTL logic
   (architecture D3 binding).
2. **A single MSI ships both `.sys` outputs** alongside one
   `nexus-wfp.inf` with `NT$ARCH$` sections; pnputil resolves the
   matching `.sys` based on the OS at install time.

## 3. Tasks

### T1 — vcxproj cross-arch (~0.5 day)

In `packages/agent/platform/windows/nexus-wfp-driver/nexus-wfp.vcxproj`:

- Add the ARM64 platform alongside x64:
  ```xml
  <ItemGroup Label="ProjectConfigurations">
    <ProjectConfiguration Include="Release|x64">
      <Configuration>Release</Configuration>
      <Platform>x64</Platform>
    </ProjectConfiguration>
    <ProjectConfiguration Include="Release|ARM64">
      <Configuration>Release</Configuration>
      <Platform>ARM64</Platform>
    </ProjectConfiguration>
  </ItemGroup>
  ```
- Set `<PlatformToolset>WindowsKernelModeDriver10.0</PlatformToolset>`
  for both — the WDK toolset is arch-aware.
- Ensure output paths differ per arch: `bin\$(Platform)\Release\`.

### T2 — `build.bat` two-pass (~0.5 day)

```bat
@echo off
setlocal
set MSBUILD="C:\Program Files\Microsoft Visual Studio\2022\BuildTools\MSBuild\Current\Bin\MSBuild.exe"

%MSBUILD% nexus-wfp.sln /p:Configuration=Release /p:Platform=x64
if errorlevel 1 exit /b 1

%MSBUILD% nexus-wfp.sln /p:Configuration=Release /p:Platform=ARM64
if errorlevel 1 exit /b 1

echo Driver build complete:
dir bin\x64\Release\nexus-wfp.sys
dir bin\ARM64\Release\nexus-wfp.sys
```

### T3 — INF NT$ARCH$ sections (~0.25 day)

Already shaped by E59-S3 §4; this story actually wires the two
sections to two different `CopyFiles` directives that reference
distinct source files in the staging area:

```ini
[NexusWfpInstall.NTamd64]
CopyFiles = NexusWfpFiles.amd64
AddReg    = NexusWfpRegSection

[NexusWfpInstall.NTarm64]
CopyFiles = NexusWfpFiles.arm64
AddReg    = NexusWfpRegSection

[NexusWfpFiles.amd64]
nexus-wfp.sys, , , 0x00000040

[NexusWfpFiles.arm64]
nexus-wfp.sys, , , 0x00000040

[SourceDisksFiles.amd64]
nexus-wfp.sys = 1,amd64

[SourceDisksFiles.arm64]
nexus-wfp.sys = 1,arm64
```

The two `.sys` files have the same filename — pnputil copies the
correct arch's file based on which `SourceDisksFiles.<arch>` section
applies for the current OS.

### T4 — Staging layout in `build.ps1` (~0.5 day)

Update `packages/agent/platform/windows/scripts/build.ps1` (the
Go-build + dashboard-build script) to also bring in BOTH driver
outputs:

```
dist/windows/staging/wfp-driver/
├── nexus-wfp.inf
├── nexus-wfp.cat            (single CAT, signed in E59-S5)
├── amd64/
│   └── nexus-wfp.sys
└── arm64/
    └── nexus-wfp.sys
```

Build.ps1 looks for both `bin\x64\Release\nexus-wfp.sys` and
`bin\ARM64\Release\nexus-wfp.sys` in the driver source tree; if
either is missing, fail with "Run nexus-wfp\build.bat first".

### T5 — wxs reference (~0.25 day)

The `wfp.wxi` from E59-S3 needs the two-arch File source paths:

```xml
<Component Id="WfpSysFileAmd64" Guid="*" Bitness="always64">
    <File Id="nexus_wfp.sys.amd64"
          Source="$(var.StagingDir)\wfp-driver\amd64\nexus-wfp.sys"
          Name="nexus-wfp.sys"
          KeyPath="yes" />
    <Condition><![CDATA[MsiAMD64]]></Condition>
</Component>

<Component Id="WfpSysFileArm64" Guid="*" Bitness="always64">
    <File Id="nexus_wfp.sys.arm64"
          Source="$(var.StagingDir)\wfp-driver\arm64\nexus-wfp.sys"
          Name="nexus-wfp.sys"
          KeyPath="yes" />
    <Condition><![CDATA[MsiARM64]]></Condition>
</Component>
```

The two components install conditionally — only the matching arch's
`.sys` is copied. Filename is identical (`nexus-wfp.sys`) so the INF
needs no per-arch path manipulation.

### T6 — Go user-mode arm64 build (~0.5 day)

`packages/agent/internal/platform/windows/wfp_*.go` from E59-S2 must
compile under `GOOS=windows GOARCH=arm64`. Likely already does
(pure-Go DeviceIoControl), but explicitly add an arm64 GitHub Actions
matrix entry in `.github/workflows/agent-windows.yml` (or wherever
CI lives) to keep it green.

### T7 — MSI summary template (~0.25 day)

The MSI Summary `Template` field is `x64` today (E42 + the
`-arch x64` patch in commit `ec57e3dce`). For a multi-arch MSI we
want `Intel64;0` would be wrong — actually the correct value for a
multi-arch MSI is `x64;0` (the MSI executor on arm64 runs amd64
under emulation, and arm64 Windows accepts `x64;0` MSIs natively
since 24H2).

Test: install the same MSI on amd64 AND arm64 Windows 11 24H2; both
should succeed. If arm64 rejects `x64;0`, file a Microsoft bug and
fall back to either two MSIs OR a single-arch arm64 MSI bundled
alongside via Burn.

### T8 — Verify both .sys are signed by the same CAT (~0.5 day)

This is the bridge into E59-S5: the produced CAT file MUST cover
both `amd64/nexus-wfp.sys` and `arm64/nexus-wfp.sys` (referenced
from one INF). E59-S5 owns the signing submission; E59-S4 just
verifies the file set the CAT covers.

## 4. Acceptance criteria

1. `nexus-wfp\build.bat` produces both `bin\x64\Release\nexus-wfp.sys`
   and `bin\ARM64\Release\nexus-wfp.sys`, with zero compiler warnings
   at /W4 and zero WDK SDV warnings.
2. `pwsh -File packages\agent\platform\windows\scripts\build.ps1`
   stages the two `.sys` into the correct subdirs.
3. `pwsh -File packages\agent\platform\windows\scripts\package.ps1`
   produces a single MSI whose Summary `Template` is `x64;0` AND
   whose File table contains both `nexus-wfp.sys.amd64` and
   `nexus-wfp.sys.arm64` entries.
4. The MSI installs cleanly on amd64 Windows 11 24H2 — `sc qc
   NexusWFP` shows the driver up.
5. The same MSI installs cleanly on arm64 Windows 11 24H2 (Surface
   Pro 11) — `sc qc NexusWFP` shows the driver up.
6. `git grep '#ifdef.*_M_ARM64' packages/agent/platform/windows/nexus-wfp-driver/`
   returns NO matches inside callout or IOCTL logic — only inside
   WDK-provided headers (architecture D3).
7. `GOOS=windows GOARCH=arm64 go build ./packages/agent/...` green.

## 5. Risks

- **R-S4.1:** WDK build on arm64 host — WDK 11 supports building for
  ARM64 from an amd64 host. Building from an arm64 host is supported
  by WDK 11 24H2+ but less battle-tested. The CI/release pipeline
  MUST use an amd64 build host (E59-S5 binding).
- **R-S4.2:** MSI cross-arch installer compatibility — Microsoft's
  documented "amd64 MSI on arm64 Windows" support is real but has
  edge cases with custom actions running native exes. Our pnputil
  CA runs the OS-native pnputil (arm64 pnputil on arm64 Windows),
  not the amd64 one, so the driver-store call is native.
- **R-S4.3:** Per-arch `#ifdef` creep — once we have an arm64
  build, it's tempting to add `#ifdef _M_ARM64` for one-off
  workarounds. Architecture D3 is binding: any such addition
  requires explicit review and an architecture-doc update.

## 6. Out of scope

- Driver functional changes (E59-S1).
- Signing (E59-S5 — adds the attestation flow).
- arm64-specific testing (E59-S6 — runs the test matrix on both
  arches).
