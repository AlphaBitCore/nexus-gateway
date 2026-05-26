# E59-S3 — MSI Changes for WFP

**Status:** Design — pending implementation
**Date:** 2026-05-24
**Epic:** [E59](e59-windows-wfp-migration.md)
**Architecture:** [agent-windows-wfp-driver.md](../architecture/agent-windows-wfp-driver.md) §10 (install sequencing)
**Depends on:** E59-S1 (driver compiles), E59-S2 (user-mode swap landed in source tree)

---

## 1. User story

> **As** the agent release pipeline
> **I want** the MSI installer to deploy the NexusWFP driver via
> `pnputil /add-driver` and start it before the user-mode service,
> AND remove WinDivert in the same release
> **so that** a customer running `msiexec /i NexusAgent-<v>.msi`
> sees a clean install that uses the in-house WFP path and a clean
> uninstall that leaves no orphan driver or WinDivert remnants.

## 2. Goal

Three coupled changes to the WiX source tree:

1. **Add** `packages/agent/platform/windows/installer/wfp.wxi` —
   declares the WFP driver components and the four sc.exe
   CustomActions (create / start / stop / delete).
2. **Delete** `packages/agent/platform/windows/installer/windivert.wxi`
   and the WinDivert SHA256 lock + download logic in
   `build.ps1`. No parallel-WinDivert path remains.
3. **Update** `packages/agent/platform/windows/installer/NexusAgent.wxs`:
   replace `<?include windivert.wxi ?>` with `<?include wfp.wxi ?>`
   and update the `<ServiceDependency Id="WinDivert" />` reference
   on the NexusAgent ServiceInstall to `Id="NexusWFP"`.

CLAUDE.md "development-phase policy: no backward compatibility" —
no `windivert-fallback` MSI option, no `legacy=true` flag.

## 3. New `wfp.wxi`

Structure mirrors the existing `windivert.wxi` after E42 + the
`fix(agent/installer): make Windows MSI actually buildable` patch
(commit `ee8154cb8`), with these differences:

- Driver file is `nexus-wfp.sys` (single binary at install time;
  cross-arch story is in E59-S4).
- Driver registration uses `pnputil /add-driver nexus-wfp.inf /install`
  via deferred CA, not direct file copy + sc create:
  - `pnputil` runs the class installer, which validates signatures
    BEFORE installation. A test-signed driver on a host without
    `testsigning on` will fail here cleanly (no half-installed
    state).
  - `pnputil /add-driver /install` registers BOTH the file (under
    `%SystemRoot%\System32\DriverStore\FileRepository\`) AND the
    SCM service (per the INF's `[NexusWfpService_AddReg]` section).
- INF file is also staged so `pnputil /delete-driver
  nexus-wfp.inf /uninstall` works on uninstall.

Skeleton:

```xml
<Fragment>
    <!-- Driver INF + .sys staged to a tools dir under INSTALLFOLDER
         where pnputil can read them. Not into System32 directly —
         pnputil owns the driver store layout. -->
    <DirectoryRef Id="INSTALLFOLDER">
        <Directory Id="WfpDriverStaging" Name="wfp-driver">
            <Component Id="WfpInfFile" Guid="*" Bitness="always64">
                <File Id="nexus_wfp.inf"
                      Source="$(var.StagingDir)\wfp-driver\nexus-wfp.inf"
                      KeyPath="yes" />
            </Component>
            <Component Id="WfpSysFile" Guid="*" Bitness="always64">
                <File Id="nexus_wfp.sys"
                      Source="$(var.StagingDir)\wfp-driver\nexus-wfp.sys"
                      KeyPath="yes" />
            </Component>
            <Component Id="WfpCatFile" Guid="*" Bitness="always64">
                <File Id="nexus_wfp.cat"
                      Source="$(var.StagingDir)\wfp-driver\nexus-wfp.cat"
                      KeyPath="yes" />
            </Component>
        </Directory>
    </DirectoryRef>

    <!-- pnputil install: registers the driver package + creates SCM
         entry per INF. Deferred no-impersonate so the kernel-driver
         install runs as LocalSystem. Return="check" because a
         signing failure here SHOULD fail the install. -->
    <CustomAction Id="WfpDriverInstall"
                  Directory="WfpDriverStaging"
                  ExeCommand='pnputil.exe /add-driver "[#nexus_wfp.inf]" /install'
                  Execute="deferred"
                  Impersonate="no"
                  Return="check" />

    <!-- sc.exe explicit start (the INF sets ServiceStart=2 = auto,
         honoured at next boot; we start it now so the agent service
         doesn't have to wait). -->
    <CustomAction Id="WfpServiceStart"
                  Directory="System64Folder"
                  ExeCommand="sc.exe start NexusWFP"
                  Execute="deferred"
                  Impersonate="no"
                  Return="ignore" />

    <CustomAction Id="WfpServiceStop"
                  Directory="System64Folder"
                  ExeCommand="sc.exe stop NexusWFP"
                  Execute="deferred"
                  Impersonate="no"
                  Return="ignore" />

    <CustomAction Id="WfpDriverUninstall"
                  Directory="WfpDriverStaging"
                  ExeCommand='pnputil.exe /delete-driver "[#nexus_wfp.inf]" /uninstall /force'
                  Execute="deferred"
                  Impersonate="no"
                  Return="ignore" />

    <InstallExecuteSequence>
        <Custom Action="WfpDriverInstall"
                After="InstallFiles"
                Condition="NOT Installed" />
        <Custom Action="WfpServiceStart"
                After="WfpDriverInstall"
                Condition="NOT Installed" />
        <Custom Action="WfpServiceStop"
                After="DeleteServices"
                Condition="REMOVE=&quot;ALL&quot;" />
        <Custom Action="WfpDriverUninstall"
                After="WfpServiceStop"
                Condition="REMOVE=&quot;ALL&quot;" />
    </InstallExecuteSequence>
</Fragment>
```

## 4. INF file shape (`nexus-wfp.inf`)

Owned by E59-S1 (driver project) but shaped by this story's pnputil
contract:

```ini
[Version]
Signature   = "$Windows NT$"
Class       = NetService
ClassGUID   = {4D36E974-E325-11CE-BFC1-08002BE10318}
Provider    = %ManufacturerName%
CatalogFile = nexus-wfp.cat
DriverVer   = ...  ; stamped at build time
PnpLockdown = 1

[Manufacturer]
%ManufacturerName% = Standard, NT$ARCH$

[Standard.NTamd64]
%DeviceDescription% = NexusWfpInstall, Root\NexusWFP

[Standard.NTarm64]
%DeviceDescription% = NexusWfpInstall, Root\NexusWFP

[NexusWfpInstall.NTamd64]
CopyFiles = NexusWfpFiles
AddReg    = NexusWfpRegSection

[NexusWfpInstall.NTarm64]
CopyFiles = NexusWfpFiles
AddReg    = NexusWfpRegSection

[NexusWfpFiles]
nexus-wfp.sys, , , 0x00000040

[NexusWfpInstall.NTamd64.Services]
AddService = NexusWFP, 2, NexusWfpService_Inst

[NexusWfpInstall.NTarm64.Services]
AddService = NexusWFP, 2, NexusWfpService_Inst

[NexusWfpService_Inst]
DisplayName    = %ServiceDescription%
ServiceType    = 1                                 ; SERVICE_KERNEL_DRIVER
StartType      = 2                                 ; SERVICE_AUTO_START
ErrorControl   = 1                                 ; SERVICE_ERROR_NORMAL
ServiceBinary  = %12%\nexus-wfp.sys                ; %12% = drivers folder

[Strings]
ManufacturerName   = "Nexus Gateway"
DeviceDescription  = "Nexus WFP Filter"
ServiceDescription = "Nexus WFP Packet Capture Driver"
```

NT$ARCH$ expands to NTamd64 or NTarm64 based on the host arch
when pnputil is invoked. E59-S4 enables cross-arch by including
the matching `.sys` file alongside the INF.

## 5. Tasks

### T1 — Write `wfp.wxi` (~0.5 day)

Per §3 skeleton. Mirror the `Bitness="always64"` + `Component`
patterns from the post-E42 `windivert.wxi` for consistency.

### T2 — Delete `windivert.wxi` + update `NexusAgent.wxs` (~0.5 day)

- `git rm packages/agent/platform/windows/installer/windivert.wxi`
- In `NexusAgent.wxs`:
  - Replace `<?include windivert.wxi ?>` with `<?include wfp.wxi ?>`
  - Update `<ServiceDependency Id="WinDivert" />` →
    `<ServiceDependency Id="NexusWFP" />`
  - Remove `<ComponentRef Id="WinDivertSysFile" />` and
    `<ComponentRef Id="WinDivertDllFile" />` from the AgentCore
    Feature; add `<ComponentRef Id="WfpInfFile" />`,
    `<ComponentRef Id="WfpSysFile" />`, `<ComponentRef Id="WfpCatFile" />`.

### T3 — `build.ps1` cleanup (~0.5 day)

- Remove the WinDivert SHA256 download + verify block (lines that
  reference `WinDivert-2.2.2-A.zip`, `WinDivert.sha256`,
  `wdStaging`, etc.).
- Add the new staging step:
  - Look for `nexus-wfp.sys`, `nexus-wfp.inf`, `nexus-wfp.cat`
    under `packages/agent/platform/windows/nexus-wfp-driver/bin/Release/`
    (path produced by E59-S1's vcxproj).
  - If any is missing, fail with the operator hint:
    "Run msbuild on nexus-wfp.sln first (see E59-S1)."
  - Copy them to `dist/windows/staging/wfp-driver/`.
- Update the `Get-ChildItem $staging | Format-Table` final summary
  to list `wfp-driver\nexus-wfp.{sys,inf,cat}` instead of
  `windivert\WinDivert{.dll,64.sys}`.

### T4 — `third_party/windivert/` removal (~0.5 day)

- `git rm -r third_party/windivert/` (the SHA256 lock + README).
- Update `docs/developers/workflow/conventions.md` if it references
  the WinDivert third-party check. (Skip if no reference.)

### T5 — Build + smoke test (~1 day)

- On the build host:
  ```
  pwsh -File packages/agent/platform/windows/scripts/build.ps1
  pwsh -File packages/agent/platform/windows/scripts/package.ps1
  ```
- Verify the produced MSI summary template is `x64;0` (regression
  guard for the bug we fixed in commit `ec57e3dce`).
- On a testsigning-enabled amd64 Windows 11 VM:
  ```
  msiexec /i dist\windows\NexusAgent-1.0.0.msi /qb /l*v install.log
  ```
- Assert `sc query NexusWFP` → `STATE: 4 RUNNING`.
- Assert `Get-ChildItem 'C:\Windows\System32\DriverStore\FileRepository\nexus-wfp.inf_*'`
  shows the driver package landed in the store.
- `msiexec /x dist\windows\NexusAgent-1.0.0.msi /qb /l*v uninstall.log`
- Assert: no `NexusWFP` service, no nexus-wfp.* under
  DriverStore\FileRepository, no nexus-wfp.* under System32\drivers.

## 6. Acceptance criteria

1. Fresh `packages/agent/platform/windows/scripts/build.ps1` + `package.ps1`
   on an amd64 build host produces `dist/windows/NexusAgent-<v>.msi`
   with summary template `x64;0`.
2. The MSI installs cleanly on Windows 11 24H2 amd64
   (testsigning enabled for the duration of E59 until S5 lands).
3. `sc qc NexusWFP` shows `TYPE: 1 KERNEL_DRIVER`, `START_TYPE: 2
   AUTO_START`, `BINARY_PATH_NAME: \??\C:\WINDOWS\system32\DriverStore\...`.
4. `sc qc NexusAgent` shows
   `DEPENDENCIES: NexusWFP` (no more `WinDivert`).
5. Uninstall leaves no `nexus-wfp.*` files, no `NexusWFP` service,
   no entries in `pnputil /enum-drivers | findstr nexus-wfp`.
6. `git grep -i 'windivert'` on the branch tip returns matches ONLY
   in:
   - `docs/_archive/**` (historical brainstorms; do not edit)
   - `docs/developers/specs/e59-*.md` (the migration narrative)
   - Commit messages.
   Production code, WiX, build scripts, and tests must be
   WinDivert-free.

## 7. Risks

- **R-S3.1:** pnputil reboot prompt — if a competing kernel filter
  driver is in the path, pnputil can return
  `ERROR_SUCCESS_REBOOT_REQUIRED` (3010). The CA must detect this
  and set the MSI's `REBOOT_REQUIRED` property so the installer UI
  prompts the user. Specific Win32 error code 3010, not 0 — easy
  to miss.
- **R-S3.2:** Driver package leak on failed install — pnputil
  registers the package BEFORE creating the service. If the service
  install step fails halfway, the driver store can be left with
  an orphan FileRepository entry. Mitigation: the deferred CA
  `WfpDriverInstall` runs `pnputil /add-driver /install` (the
  `/install` flag combines both steps atomically); the rollback CA
  in MSI handles the failed-install case by calling
  `pnputil /delete-driver /force /uninstall`.

## 8. Out of scope

- Driver implementation (E59-S1).
- User-mode Go integration (E59-S2).
- Cross-arch story (E59-S4 — adds arm64 .sys to the staging dir).
- Signing pipeline (E59-S5 — replaces test-sign with attestation-sign).
- Test matrix (E59-S6).
