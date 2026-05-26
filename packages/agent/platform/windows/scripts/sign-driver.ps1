#!/usr/bin/env pwsh
# E59-S5: Sign the NexusWFP kernel driver bundle with the EV
# code-signing cert and produce a CAT ready for submission to
# Microsoft Hardware Dev Center.
#
# Inputs (parameters or env):
#   -DriverRoot <path>   Default: packages/agent/platform/windows/nexus-wfp-driver
#   -CertSha1 <thumb>    EV cert thumbprint (40 hex). Env: WINDOWS_CERT_THUMB.
#   -TimestampUrl <url>  Default: http://timestamp.digicert.com
#   -InfFile <path>      Default: $DriverRoot/nexus-wfp.inf
#
# Outputs:
#   $DriverRoot/nexus-wfp.cat   (EV-signed, ready for Microsoft attestation)
#   $DriverRoot/bin/x64/Release/nexus-wfp.sys    (EV-signed)
#   $DriverRoot/bin/ARM64/Release/nexus-wfp.sys  (EV-signed)
#
# Workflow:
#   1. inf2cat /driver:$DriverRoot /os:10_X64,10_ARM64 — generates an
#      unsigned CAT covering both .sys files referenced by the INF.
#   2. signtool sign each .sys + the CAT with the EV cert via
#      hardware-token PIN/touch.
#   3. signtool verify /pa /v — sanity check.
#
# After this script, run submit-driver.ps1 to round-trip the CAT
# through Microsoft Hardware Dev Center for the attestation signature.

[CmdletBinding()]
param(
    [string]$DriverRoot,
    [string]$CertSha1,
    [string]$TimestampUrl,
    [string]$InfFile
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if (-not $DriverRoot) {
    $repoRoot = git rev-parse --show-toplevel
    $DriverRoot = Join-Path $repoRoot 'packages/agent/platform/windows/nexus-wfp-driver'
}
if (-not $CertSha1) {
    if ($env:WINDOWS_CERT_THUMB) { $CertSha1 = $env:WINDOWS_CERT_THUMB }
    else { throw 'Missing -CertSha1 or WINDOWS_CERT_THUMB env var.' }
}
if (-not $TimestampUrl) {
    $TimestampUrl = if ($env:TIMESTAMP_URL) { $env:TIMESTAMP_URL } else { 'http://timestamp.digicert.com' }
}
if (-not $InfFile) { $InfFile = Join-Path $DriverRoot 'nexus-wfp.inf' }

if (-not (Test-Path $InfFile)) {
    throw "INF file not found: $InfFile"
}

$amd64Sys = Join-Path $DriverRoot 'bin/x64/Release/nexus-wfp.sys'
$arm64Sys = Join-Path $DriverRoot 'bin/ARM64/Release/nexus-wfp.sys'

foreach ($sys in @($amd64Sys, $arm64Sys)) {
    if (-not (Test-Path $sys)) {
        throw "Driver binary not found: $sys. Run build.bat first (E59-S1)."
    }
}

# Locate inf2cat (ships with WDK).
$inf2cat = Get-Command inf2cat.exe -ErrorAction SilentlyContinue
if (-not $inf2cat) {
    $wdkCandidates = @(
        'C:\Program Files (x86)\Windows Kits\10\bin\10.0.26100.0\x64\inf2cat.exe',
        'C:\Program Files (x86)\Windows Kits\10\bin\10.0.22621.0\x64\inf2cat.exe',
        'C:\Program Files (x86)\Windows Kits\10\bin\x86\inf2cat.exe'
    )
    foreach ($c in $wdkCandidates) {
        if (Test-Path $c) { $inf2cat = Get-Command $c; break }
    }
}
if (-not $inf2cat) {
    throw 'inf2cat.exe not on PATH. Install the Windows Driver Kit and re-run.'
}

# Locate signtool (ships with Windows SDK).
$signtool = Get-Command signtool.exe -ErrorAction SilentlyContinue
if (-not $signtool) {
    $sdkCandidates = @(
        'C:\Program Files (x86)\Windows Kits\10\bin\10.0.26100.0\x64\signtool.exe',
        'C:\Program Files (x86)\Windows Kits\10\bin\10.0.22621.0\x64\signtool.exe',
        'C:\Program Files (x86)\Windows Kits\10\bin\x86\signtool.exe'
    )
    foreach ($c in $sdkCandidates) {
        if (Test-Path $c) { $signtool = Get-Command $c; break }
    }
}
if (-not $signtool) {
    throw 'signtool.exe not on PATH. Install Windows SDK signing tools.'
}

# ─── 1. Generate unsigned CAT ───────────────────────────────────────
Write-Host '==> Running inf2cat'
& $inf2cat.Source /driver:$DriverRoot /os:10_X64,10_ARM64 /verbose
if ($LASTEXITCODE -ne 0) {
    throw "inf2cat failed (exit $LASTEXITCODE)"
}

$catFile = Join-Path $DriverRoot 'nexus-wfp.cat'
if (-not (Test-Path $catFile)) {
    throw "Expected $catFile not produced by inf2cat"
}
Write-Host "==> CAT generated: $catFile"

# ─── 2. Sign each .sys + the CAT ────────────────────────────────────
$toSign = @($amd64Sys, $arm64Sys, $catFile)

foreach ($file in $toSign) {
    Write-Host "==> EV-signing $file"
    & $signtool.Source sign `
        /sha1 $CertSha1 `
        /fd sha256 `
        /tr $TimestampUrl `
        /td sha256 `
        /v `
        $file
    if ($LASTEXITCODE -ne 0) {
        throw "signtool sign failed for $file (exit $LASTEXITCODE)"
    }
}

# ─── 3. Verify ──────────────────────────────────────────────────────
foreach ($file in $toSign) {
    Write-Host "==> Verifying $file"
    & $signtool.Source verify /pa /v $file
    if ($LASTEXITCODE -ne 0) {
        Write-Warning "signtool verify failed for $file (exit $LASTEXITCODE). The CAT is still EV-signed but will need fixup before Microsoft submission."
    }
}

Write-Host "==> EV-signing complete. Next step: submit-driver.ps1 to obtain Microsoft attestation."
Get-Item $toSign | Select-Object Name, Length, LastWriteTime | Format-Table -AutoSize
