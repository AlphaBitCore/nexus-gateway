#!/usr/bin/env pwsh
# E40 Phase 3 D2: Build the Windows agent + tray + dashboard artifacts.
#
# Produces in $repoRoot\dist\windows\staging\:
#   nexus-agent.exe          — Go daemon (cmd/agent, windows/amd64)
#   nexus-agent-tray.exe     — fyne.io/systray-based tray (cmd/agent-tray)
#   nexus-dashboard.exe      — Wails dashboard (packages/agent/ui)
#
# The output layout is what packages/agent/platform/windows/installer/
# (D3b — WiX) consumes to assemble the MSI.
#
# Requires (on the build host — must be Windows; Wails uses WebView2 which
# is Windows-only at build time):
#   - Go toolchain for windows/amd64
#   - Wails v2.10+ CLI:   go install github.com/wailsapp/wails/v2/cmd/wails@latest
#   - npm + node (resolves the @nexus-gateway/* workspace)
#   - Microsoft WebView2 SDK (bundled with the OS on Win10 1809+; Wails
#     does the runtime check at app start.)
#
# Cross-compiling the Wails dashboard from Linux/macOS is impractical
# (WebView2 host headers + CGO), so this script is intended to run on
# the Windows runner in the Phase 3/D4 CI matrix.
#
# Usage:  pwsh -NoProfile -File packages/agent/platform/windows/scripts/build.ps1
# Env:    VERSION = "1.0.0" (default)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$version  = if ($env:VERSION) { $env:VERSION } else { '1.0.0' }
$repoRoot = git rev-parse --show-toplevel
$dist     = Join-Path $repoRoot 'dist/windows'
$staging  = Join-Path $dist     'staging'

Write-Host "==> E40 Windows build starting (version=$version)"

if (Test-Path $dist) { Remove-Item -Recurse -Force $dist }
New-Item -ItemType Directory -Force -Path $staging | Out-Null

# ─── 0. NexusWFP driver staging (E59) ───────────────────────────────
# The driver project lives at packages/agent/platform/windows/nexus-wfp-driver/.
# It produces two .sys files (one per arch) via `build.bat` and one INF +
# CAT shared across arches. The CAT is signed in sign-driver.ps1 (E59-S5).
#
# Expected layout in the driver tree after `build.bat`:
#   bin/x64/Release/nexus-wfp.sys
#   bin/ARM64/Release/nexus-wfp.sys
#   nexus-wfp.inf
#   nexus-wfp.cat   (only present after sign-driver.ps1; pre-sign builds
#                    can stage an empty/test CAT — package.ps1 picks up
#                    whatever's here)
Write-Host '==> Staging NexusWFP driver artifacts'
$wfpStaging = Join-Path $staging 'wfp-driver'
$wfpAmd64Dir = Join-Path $wfpStaging 'amd64'
$wfpArm64Dir = Join-Path $wfpStaging 'arm64'
New-Item -ItemType Directory -Force -Path $wfpAmd64Dir | Out-Null
New-Item -ItemType Directory -Force -Path $wfpArm64Dir | Out-Null

$wfpDriverRoot = Join-Path $repoRoot 'packages/agent/platform/windows/nexus-wfp-driver'
$wfpAmd64Sys = Join-Path $wfpDriverRoot 'bin/x64/Release/nexus-wfp.sys'
$wfpArm64Sys = Join-Path $wfpDriverRoot 'bin/ARM64/Release/nexus-wfp.sys'
$wfpInf      = Join-Path $wfpDriverRoot 'nexus-wfp.inf'
$wfpCat      = Join-Path $wfpDriverRoot 'nexus-wfp.cat'

foreach ($pair in @(
    @($wfpAmd64Sys, (Join-Path $wfpAmd64Dir 'nexus-wfp.sys'), 'amd64 driver'),
    @($wfpArm64Sys, (Join-Path $wfpArm64Dir 'nexus-wfp.sys'), 'arm64 driver'),
    @($wfpInf,      (Join-Path $wfpStaging 'nexus-wfp.inf'),  'INF'),
    @($wfpCat,      (Join-Path $wfpStaging 'nexus-wfp.cat'),  'CAT')
)) {
    $src = $pair[0]; $dst = $pair[1]; $label = $pair[2]
    if (-not (Test-Path $src)) {
        throw "Missing $label at $src. Run nexus-wfp-driver\build.bat first (E59-S1)."
    }
    Copy-Item $src $dst
}
Write-Host "==> NexusWFP driver staged at $wfpStaging"

# ─── 1. nexus-agent.exe (daemon) ────────────────────────────────────
Write-Host '==> Building nexus-agent.exe'
Push-Location (Join-Path $repoRoot 'packages/agent')
try {
    $env:GOOS   = 'windows'
    $env:GOARCH = 'amd64'
    $ldflags    = "-s -w -X main.version=$version"
    & go build -trimpath -ldflags="$ldflags" -o (Join-Path $staging 'nexus-agent.exe') ./cmd/agent
    if ($LASTEXITCODE -ne 0) { throw "go build (agent) failed (exit $LASTEXITCODE)" }
} finally {
    Pop-Location
}

# ─── 2. nexus-agent-tray.exe (system tray) ──────────────────────────
# Pre-step: stamp the Win32 manifest + ICO into a .syso so the tray
# binary advertises per-monitor v2 DPI awareness and uses our icon
# in Task Manager / the tray.
Write-Host '==> Generating tray .syso (rsrc)'
$rsrc = Get-Command rsrc -ErrorAction SilentlyContinue
if ($null -eq $rsrc) {
    throw "rsrc CLI not on PATH. Install: go install github.com/akavel/rsrc@latest"
}
$manifest = Join-Path $repoRoot 'packages/agent/platform/windows/NexusAgent/app.manifest'
$icoPath  = Join-Path $repoRoot 'packages/agent/cmd/agent-tray/icons/active.ico'
$sysoPath = Join-Path $repoRoot 'packages/agent/cmd/agent-tray/agent-tray_windows.syso'
$rsrcArgs = @('-manifest', $manifest, '-o', $sysoPath, '-arch', 'amd64')
if (Test-Path $icoPath) { $rsrcArgs += @('-ico', $icoPath) }
& rsrc @rsrcArgs
if ($LASTEXITCODE -ne 0) { throw "rsrc failed (exit $LASTEXITCODE)" }

Write-Host '==> Building nexus-agent-tray.exe'
Push-Location (Join-Path $repoRoot 'packages/agent')
try {
    # -H windowsgui suppresses the console window that would otherwise
    # flash up alongside the tray on launch.
    $ldflags = "-s -w -H=windowsgui -X main.version=$version"
    & go build -trimpath -ldflags="$ldflags" -o (Join-Path $staging 'nexus-agent-tray.exe') ./cmd/agent-tray
    if ($LASTEXITCODE -ne 0) { throw "go build (agent-tray) failed (exit $LASTEXITCODE)" }
} finally {
    # Clean up the syso so it doesn't get accidentally committed. The
    # build is reproducible from rsrc.
    Remove-Item $sysoPath -ErrorAction SilentlyContinue
    Pop-Location
    Remove-Item Env:GOOS -ErrorAction SilentlyContinue
    Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
}

# ─── 3. nexus-dashboard.exe (Wails) ────────────────────────────────
Write-Host '==> Building nexus-dashboard.exe (wails)'
$wails = Get-Command wails -ErrorAction SilentlyContinue
if ($null -eq $wails) {
    throw "wails CLI not on PATH. Install: go install github.com/wailsapp/wails/v2/cmd/wails@latest"
}
Push-Location (Join-Path $repoRoot 'packages/agent/ui')
try {
    & wails build `
        -platform windows/amd64 `
        -clean `
        -trimpath `
        -ldflags "-s -w -X main.version=$version" `
        -webview2 embed
    if ($LASTEXITCODE -ne 0) { throw "wails build failed (exit $LASTEXITCODE)" }
} finally {
    Pop-Location
}

$dashBin = Join-Path $repoRoot 'packages/agent/ui/build/bin/nexus-dashboard.exe'
if (-not (Test-Path $dashBin)) {
    throw "wails build produced no $dashBin"
}
Copy-Item $dashBin (Join-Path $staging 'nexus-dashboard.exe')

# ─── 4. App manifest (referenced by WiX, optional) ──────────────────
$appManifest = Join-Path $repoRoot 'packages/agent/platform/windows/NexusAgent/app.manifest'
if (Test-Path $appManifest) {
    Copy-Item $appManifest $staging
}

# ─── 5. Done ────────────────────────────────────────────────────────
Write-Host "==> Build complete: $staging"
Get-ChildItem $staging | Select-Object Name, Length | Format-Table
