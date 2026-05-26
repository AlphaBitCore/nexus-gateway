#!/usr/bin/env pwsh
# E23W-S4: Build the MSI installer with WiX v4.
#
# Requires:
#   - WiX Toolset v4 (`dotnet tool install --global wix`)
#   - The staging directory produced by build.ps1
#
# Optionally signs the MSI itself if a code-signing cert is configured
# (delegates to sign.ps1 logic via the same env vars).
#
# Env:
#   VERSION = "1.0.0" (default; should match build.ps1)
#
# Usage:
#   pwsh -NoProfile -File packages/agent/platform/windows/scripts/package.ps1

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$version  = if ($env:VERSION) { $env:VERSION } else { '1.0.0' }
$repoRoot = git rev-parse --show-toplevel
$dist     = Join-Path $repoRoot 'dist/windows'
$staging  = Join-Path $dist     'staging'
$installer = Join-Path $repoRoot 'packages/agent/platform/windows/installer/NexusAgent.wxs'
$msiPath  = Join-Path $dist     "NexusAgent-$version.msi"

Write-Host "==> E23W MSI packaging starting (version=$version)"

if (-not (Test-Path $staging)) {
    throw "Staging directory not found: $staging. Run build.ps1 first."
}
if (-not (Test-Path $installer)) {
    throw "WiX source not found: $installer"
}

$wix = Get-Command wix -ErrorAction SilentlyContinue
if ($null -eq $wix) {
    throw 'wix CLI not found. Install with: dotnet tool install --global wix'
}

Write-Host "==> Running wix build"
# -arch x64 is critical: without it WiX defaults to x86, and ProgramFiles64Folder
# / System64Folder get WoW64-redirected on 64-bit Windows so files land in
# `C:\Program Files (x86)\` and the WinDivert binPath ends up under SysWOW64
# (a 32-bit DLL dir that has no `drivers` subdir) — services then fail to start
# with Error 1920. Bitness="always64" on the components only takes effect once
# the MSI itself is authored as 64-bit.
& wix build $installer `
    -arch x64 `
    -d "Version=$version" `
    -d "StagingDir=$staging" `
    -o $msiPath
if ($LASTEXITCODE -ne 0) { throw "wix build failed (exit $LASTEXITCODE)" }

if (-not (Test-Path $msiPath)) {
    throw "wix build reported success but $msiPath does not exist"
}

# Sign the MSI itself when a cert is configured. We reuse sign.ps1's
# signtool discovery + cert logic by invoking it inline; since sign.ps1
# operates on the staging directory, we sign the MSI directly here.
if (-not [string]::IsNullOrWhiteSpace($env:WINDOWS_CERT_PATH) -and (Test-Path $env:WINDOWS_CERT_PATH)) {
    Write-Host '==> Signing MSI'
    $signtool = Get-Command signtool.exe -ErrorAction SilentlyContinue
    if ($signtool) {
        $timestamp = if ($env:TIMESTAMP_URL) { $env:TIMESTAMP_URL } else { 'http://timestamp.digicert.com' }
        $signArgs = @(
            'sign',
            '/f', $env:WINDOWS_CERT_PATH,
            '/tr', $timestamp,
            '/td', 'sha256',
            '/fd', 'sha256'
        )
        if (-not [string]::IsNullOrEmpty($env:WINDOWS_CERT_PASSWORD)) {
            $signArgs += @('/p', $env:WINDOWS_CERT_PASSWORD)
        }
        $signArgs += $msiPath
        & $signtool.Source @signArgs
        if ($LASTEXITCODE -ne 0) { throw "signtool sign failed for MSI (exit $LASTEXITCODE)" }
    } else {
        Write-Warning '==> signtool.exe not on PATH; MSI will ship unsigned.'
    }
}

Write-Host "==> MSI ready: $msiPath"
Get-Item $msiPath | Select-Object Name, Length, LastWriteTime | Format-Table
