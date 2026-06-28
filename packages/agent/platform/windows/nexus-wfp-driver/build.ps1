#!/usr/bin/env pwsh
# Build the NexusWFP kernel driver (.sys) on a Windows host. Verified working
# 2026-06-16 with VS2022 + WDK 10.0.26100 (x64 Release).
#
# Why this exists (and build.bat alone does not work): the stock build.bat runs
# msbuild WITHOUT pinning the SDK version, so msbuild resolves the bundled
# Desktop SDK (e.g. 10.0.22621.0) which has NO km\ kernel headers and fails with
#   error C1083: Cannot open include file: 'ntddk.h'
# This script:
#   1. Locates MSBuild for any VS2022 edition.
#   2. Auto-selects the newest installed Windows SDK that actually has
#      km\ntddk.h (the kernel headers the WDK adds), and pins it via
#      WindowsTargetPlatformVersion.
#   3. Sets DriverVer + STAMPINF_VERSION (stampinf errors 87 on a bare -v).
#   4. Skips the inf2cat / InfVerif catalog packaging (this WDK install is
#      missing x86\InfVerif.dll; the .cat is release-only — E59-S5 / sign-driver.ps1).
#
# Output: bin\<platform>\<config>\nexus-wfp.sys (test-signed by the WDK test cert).
#
# Usage:
#   pwsh -NoProfile -File build.ps1                 # x64 Release
#   pwsh -NoProfile -File build.ps1 -Platform both  # x64 + ARM64
#   pwsh -NoProfile -File build.ps1 -Platform ARM64 -Configuration Debug

[CmdletBinding()]
param(
    [ValidateSet('x64', 'ARM64', 'both')]
    [string]$Platform = 'x64',
    [ValidateSet('Release', 'Debug')]
    [string]$Configuration = 'Release',
    [string]$DriverVer = '1.0.0.0'
)

$ErrorActionPreference = 'Stop'
$driverDir = $PSScriptRoot
$sln = Join-Path $driverDir 'nexus-wfp.sln'

# 1. Locate MSBuild (any VS2022 edition).
$msbuild = @('BuildTools', 'Community', 'Professional', 'Enterprise') |
    ForEach-Object { "C:\Program Files\Microsoft Visual Studio\2022\$_\MSBuild\Current\Bin\MSBuild.exe" } |
    Where-Object { Test-Path $_ } | Select-Object -First 1
if (-not $msbuild) {
    throw "MSBuild for Visual Studio 2022 not found. Install VS2022 Build Tools + the WDK (Component.Microsoft.Windows.DriverKit)."
}

# 2. Pick the newest Windows SDK that has km\ntddk.h (the kernel-mode headers).
$incRoot = 'C:\Program Files (x86)\Windows Kits\10\Include'
$sdk = Get-ChildItem $incRoot -Directory -ErrorAction Stop |
    Where-Object { Test-Path (Join-Path $_.FullName 'km\ntddk.h') } |
    Sort-Object { try { [version]$_.Name } catch { [version]'0.0' } } -Descending |
    Select-Object -First 1
if (-not $sdk) {
    throw "No Windows SDK with km\ntddk.h found under $incRoot. Install the WDK — it adds the km\ kernel headers."
}
$sdkVer = $sdk.Name

$platforms = if ($Platform -eq 'both') { @('x64', 'ARM64') } else { @($Platform) }
$env:STAMPINF_VERSION = $DriverVer

foreach ($plat in $platforms) {
    Write-Host "==> Building NexusWFP ($plat, $Configuration, SDK $sdkVer)" -ForegroundColor Cyan
    & $msbuild $sln /t:Rebuild `
        /p:Configuration=$Configuration /p:Platform=$plat `
        /p:WindowsTargetPlatformVersion=$sdkVer /p:DriverVer=$DriverVer `
        /p:SkipPackageVerification=true /p:EnableInf2cat=false `
        /m /v:minimal
    if ($LASTEXITCODE -ne 0) { throw "msbuild failed for $plat (exit $LASTEXITCODE)" }

    $sys = Join-Path $driverDir "bin\$plat\$Configuration\nexus-wfp.sys"
    if (-not (Test-Path $sys)) { throw "build reported success but $sys is missing" }
    $f = Get-Item $sys
    Write-Host "==> OK: $sys  ($($f.Length) bytes)" -ForegroundColor Green
}

Write-Host ""
Write-Host "Driver built + test-signed. To load on the kernel-debug VM, run dev-load.ps1 (elevated) inside the VM." -ForegroundColor Yellow
