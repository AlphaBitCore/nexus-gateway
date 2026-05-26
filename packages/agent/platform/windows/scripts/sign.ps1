#!/usr/bin/env pwsh
# E23W-S4: Authenticode-sign the Windows agent binaries.
#
# Skips silently when no code-signing cert is configured. This lets the same
# CI workflow run on every push (unsigned) and produce a signed MSI only when
# the cert env vars are populated (typically on tagged releases).
#
# Env:
#   WINDOWS_CERT_PATH     = path to .pfx file (required to sign)
#   WINDOWS_CERT_PASSWORD = .pfx password (optional; signtool will prompt if missing)
#   TIMESTAMP_URL         = RFC 3161 timestamp URL (default: digicert)
#
# Usage:
#   pwsh -NoProfile -File packages/agent/platform/windows/scripts/sign.ps1

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if ([string]::IsNullOrWhiteSpace($env:WINDOWS_CERT_PATH)) {
    Write-Warning '==> WINDOWS_CERT_PATH not set; skipping Authenticode signing.'
    Write-Warning '    Binaries will ship unsigned. Set WINDOWS_CERT_PATH and WINDOWS_CERT_PASSWORD to enable signing.'
    exit 0
}

if (-not (Test-Path $env:WINDOWS_CERT_PATH)) {
    throw "WINDOWS_CERT_PATH points to a missing file: $($env:WINDOWS_CERT_PATH)"
}

$repoRoot  = git rev-parse --show-toplevel
$staging   = Join-Path $repoRoot 'dist/windows/staging'
$timestamp = if ($env:TIMESTAMP_URL) { $env:TIMESTAMP_URL } else { 'http://timestamp.digicert.com' }

if (-not (Test-Path $staging)) {
    throw "Staging directory not found: $staging. Run build.ps1 first."
}

# Locate signtool.exe — search standard Windows SDK install locations
function Find-SignTool {
    $cmd = Get-Command signtool.exe -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }

    $kitRoots = @(
        "${env:ProgramFiles(x86)}\Windows Kits\10\bin",
        "${env:ProgramFiles}\Windows Kits\10\bin"
    ) | Where-Object { $_ -and (Test-Path $_) }

    foreach ($root in $kitRoots) {
        $candidate = Get-ChildItem -Path $root -Recurse -Filter signtool.exe -ErrorAction SilentlyContinue |
            Where-Object { $_.FullName -match '\\x64\\signtool\.exe$' } |
            Sort-Object FullName -Descending |
            Select-Object -First 1
        if ($candidate) { return $candidate.FullName }
    }
    return $null
}

$signtool = Find-SignTool
if (-not $signtool) {
    throw 'signtool.exe not found. Install the Windows 10 SDK or add signtool to PATH.'
}
Write-Host "==> Using signtool: $signtool"

# Collect signable artifacts (PE binaries only)
$targets = Get-ChildItem -Path $staging -Recurse -Include '*.exe', '*.dll', '*.sys' -File

if ($targets.Count -eq 0) {
    Write-Warning '==> No .exe/.dll found under staging; nothing to sign.'
    exit 0
}

foreach ($file in $targets) {
    Write-Host "==> Signing $($file.FullName)"
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
    $signArgs += $file.FullName

    & $signtool @signArgs
    if ($LASTEXITCODE -ne 0) { throw "signtool sign failed for $($file.FullName) (exit $LASTEXITCODE)" }

    & $signtool verify /pa $file.FullName
    if ($LASTEXITCODE -ne 0) { throw "signtool verify failed for $($file.FullName) (exit $LASTEXITCODE)" }
}

Write-Host "==> Signed $($targets.Count) file(s) successfully."
