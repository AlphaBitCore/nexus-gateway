#!/usr/bin/env pwsh
# E23W-S4: User-runnable uninstaller for the Nexus Agent.
#
# Stops the service, removes the MSI-installed product, and optionally
# clears per-user data. Self-elevates to Administrator if not already.
#
# Usage:
#   pwsh -NoProfile -File packages/agent/platform/windows/scripts/uninstall.ps1
#   pwsh -NoProfile -File packages/agent/platform/windows/scripts/uninstall.ps1 -KeepData

param(
    [switch]$KeepData
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# Self-elevate if not running as administrator
$identity  = [System.Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object System.Security.Principal.WindowsPrincipal($identity)
if (-not $principal.IsInRole([System.Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host '==> Re-launching as Administrator...'
    $argList = @('-NoProfile', '-File', $PSCommandPath)
    if ($KeepData) { $argList += '-KeepData' }
    Start-Process pwsh -Verb RunAs -ArgumentList $argList
    exit 0
}

Write-Host '==> Nexus Agent uninstaller'

# 1. Stop and disable the service (ignore failures — service may already be gone)
$svc = Get-Service -Name 'NexusAgent' -ErrorAction SilentlyContinue
if ($svc) {
    if ($svc.Status -ne 'Stopped') {
        Write-Host '==> Stopping NexusAgent service'
        Stop-Service -Name 'NexusAgent' -Force -ErrorAction SilentlyContinue
    }
} else {
    Write-Host '==> NexusAgent service not registered; skipping stop step'
}

# 2. Find the installed product code via the registry
#    (Get-WmiObject Win32_Product is slow and triggers MSI repair on every row;
#    walking the Uninstall key is faster and side-effect-free.)
$uninstallKeys = @(
    'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall',
    'HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall'
)

$product = $null
foreach ($root in $uninstallKeys) {
    if (-not (Test-Path $root)) { continue }
    $product = Get-ChildItem $root | ForEach-Object {
        $props = Get-ItemProperty $_.PSPath -ErrorAction SilentlyContinue
        if ($props -and $props.DisplayName -eq 'Nexus Agent') { $props }
    } | Select-Object -First 1
    if ($product) { break }
}

if ($product -and $product.PSChildName -match '^\{[0-9A-Fa-f-]+\}$') {
    $productCode = $product.PSChildName
    Write-Host "==> Uninstalling Nexus Agent ($productCode)"
    & msiexec.exe /x $productCode /quiet /norestart
    if ($LASTEXITCODE -ne 0 -and $LASTEXITCODE -ne 3010) {
        Write-Warning "msiexec exited with code $LASTEXITCODE (3010 = reboot required is OK)"
    }
} else {
    Write-Warning '==> Nexus Agent MSI registration not found; nothing to uninstall.'
}

# 3. Optionally remove per-user data
if (-not $KeepData) {
    $programData = Join-Path $env:ProgramData 'NexusAgent'
    if (Test-Path $programData) {
        Write-Host "==> Removing $programData"
        Remove-Item -Recurse -Force $programData -ErrorAction SilentlyContinue
    }
} else {
    Write-Host '==> -KeepData specified; leaving %ProgramData%\NexusAgent in place.'
}

Write-Host '==> Uninstall complete.'
