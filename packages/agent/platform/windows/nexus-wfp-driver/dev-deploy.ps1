#!/usr/bin/env pwsh
# Host-driven deploy of the NexusWFP driver to the NexusWFP-Debug kernel-debug VM.
# Run ELEVATED on the Hyper-V HOST (not in the guest). Crash-safe by design: the
# driver loads only inside the disposable VM, `sc start` runs under a timeout, and
# a bugcheck is recovered by reverting the clean-debug-ready checkpoint.
#
# Initial VM provisioning (the one-time setup that makes this work — Secure Boot
# off, Smart App Control off, KDNET, the checkpoint) is documented in
# docs/developers/workflow/windows-kernel-debug-vm.md.
#
# Flow:
#   [build.ps1] -> revert clean-debug-ready -> Start-VM -> wait PSDirect ->
#   copy nexus-wfp.sys to guest C:\nexus\ -> trust the test-signing cert ->
#   ensure the kernel service -> `sc start` under a 45s Start-Job timeout ->
#   verify the driver is Running and NexusConnectRedirectV4/V6 are registered.
#
# Usage (elevated host):
#   pwsh -NoProfile -File dev-deploy.ps1               # build + revert + deploy + verify
#   pwsh -NoProfile -File dev-deploy.ps1 -SkipBuild    # deploy the existing bin\x64\Release\.sys
#   pwsh -NoProfile -File dev-deploy.ps1 -NoRevert     # deploy onto current VM state (risks FWP_E_ALREADY_EXISTS)
#   pwsh -NoProfile -File dev-deploy.ps1 -Stop         # sc stop in the guest (no revert)

[CmdletBinding()]
param(
    [switch]$SkipBuild,
    [switch]$NoRevert,
    [switch]$Stop,
    [string]$VMName = 'NexusWFP-Debug',
    [string]$Checkpoint = 'clean-debug-ready',
    [string]$GuestUser = 'dev2',
    [string]$GuestPass = 'abc123',
    [int]$StartTimeoutSec = 45
)

$ErrorActionPreference = 'Stop'
$driverDir = $PSScriptRoot
$sys = Join-Path $driverDir 'bin\x64\Release\nexus-wfp.sys'

function New-GuestCred {
    New-Object System.Management.Automation.PSCredential(
        $GuestUser, (ConvertTo-SecureString $GuestPass -AsPlainText -Force))
}

function Wait-PSDirect {
    param([int]$TimeoutSec = 150)
    $cred = New-GuestCred
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        try {
            if (Invoke-Command -VMName $VMName -Credential $cred -ErrorAction Stop -ScriptBlock { $env:COMPUTERNAME }) {
                return $cred
            }
        }
        catch { Start-Sleep -Seconds 3 }
    }
    throw "PowerShell Direct to $VMName not ready after ${TimeoutSec}s (is the VM booted? is dev2/abc123 valid?)."
}

$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
    [Security.Principal.WindowsBuiltinRole]::Administrator)
if (-not $isAdmin) { throw "Run elevated (Administrator) on the Hyper-V host." }

Get-VM -Name $VMName -ErrorAction Stop | Out-Null

if ($Stop) {
    $cred = Wait-PSDirect
    Write-Host "==> Stopping NexusWFP in guest" -ForegroundColor Cyan
    Invoke-Command -VMName $VMName -Credential $cred -ScriptBlock { sc.exe stop NexusWFP }
    return
}

# 1. Build (unless told to reuse the existing .sys).
if (-not $SkipBuild) {
    Write-Host "==> Building driver (build.ps1)" -ForegroundColor Cyan
    & (Join-Path $driverDir 'build.ps1')
    if ($LASTEXITCODE -ne 0) { throw "build.ps1 failed (exit $LASTEXITCODE)" }
}
if (-not (Test-Path $sys)) { throw "Driver not built: $sys (run without -SkipBuild)." }

# 2. Revert to the clean baseline (service demand-start + STOPPED, cert trusted,
#    0 WFP objects) so each deploy starts from a known-clean kernel.
if (-not $NoRevert) {
    Write-Host "==> Reverting $VMName to checkpoint '$Checkpoint'" -ForegroundColor Cyan
    Restore-VMCheckpoint -VMName $VMName -Name $Checkpoint -Confirm:$false
}

# 3. Ensure the VM is running, then wait for PowerShell Direct.
if ((Get-VM -Name $VMName).State -ne 'Running') {
    Write-Host "==> Starting $VMName" -ForegroundColor Cyan
    Start-VM -Name $VMName
}
$cred = Wait-PSDirect

$session = New-PSSession -VMName $VMName -Credential $cred
try {
    # 4. Copy the freshly-built .sys into the guest.
    Write-Host "==> Copying nexus-wfp.sys -> guest C:\nexus\" -ForegroundColor Cyan
    Invoke-Command -Session $session -ScriptBlock { New-Item -ItemType Directory -Force 'C:\nexus' | Out-Null }
    Copy-Item -ToSession $session -Path $sys -Destination 'C:\nexus\nexus-wfp.sys' -Force

    # 5. Trust the test-signing cert in the guest (Root + TrustedPublisher).
    #    Idempotent — the checkpoint already trusts the stable WDK test cert, but
    #    a re-signed cert needs re-adding, so always push the current signer.
    $sig = Get-AuthenticodeSignature $sys
    if ($sig.SignerCertificate) {
        $cerLocal = Join-Path $env:TEMP 'nexus-wfp-signer.cer'
        [System.IO.File]::WriteAllBytes($cerLocal, $sig.SignerCertificate.Export('Cert'))
        Copy-Item -ToSession $session -Path $cerLocal -Destination 'C:\nexus\nexus-wfp-signer.cer' -Force
        Invoke-Command -Session $session -ScriptBlock {
            certutil -addstore -f Root 'C:\nexus\nexus-wfp-signer.cer' | Out-Null
            certutil -addstore -f TrustedPublisher 'C:\nexus\nexus-wfp-signer.cer' | Out-Null
        }
    }

    # 6. Ensure the kernel service exists (the checkpoint already has it demand-start).
    Invoke-Command -Session $session -ScriptBlock {
        if ((sc.exe query NexusWFP 2>&1) -match 'FAILED 1060') {
            sc.exe create NexusWFP type= kernel start= demand binPath= C:\nexus\nexus-wfp.sys | Out-Null
        }
    }

    # 7. `sc start` under a timeout — a bugcheck freezes the guest, so never block
    #    forever. The job re-opens its own PSDirect connection in a fresh runspace.
    Write-Host "==> Starting NexusWFP (timeout ${StartTimeoutSec}s; a timeout means a BUGCHECK froze the guest)" -ForegroundColor Cyan
    $job = Start-Job -ScriptBlock {
        param($vm, $u, $p)
        $c = New-Object System.Management.Automation.PSCredential($u, (ConvertTo-SecureString $p -AsPlainText -Force))
        Invoke-Command -VMName $vm -Credential $c -ScriptBlock { sc.exe start NexusWFP 2>&1 }
    } -ArgumentList $VMName, $GuestUser, $GuestPass

    if (Wait-Job $job -Timeout $StartTimeoutSec) {
        Write-Host (Receive-Job $job | Out-String)
        Remove-Job $job
    }
    else {
        Stop-Job $job; Remove-Job $job -Force
        Write-Warning "sc start did not return in ${StartTimeoutSec}s -- the driver likely BUGCHECKED the guest."
        Write-Warning "Diagnose: attach kd over net:port=50000 (key in D:\vm-iso\$VMName-kdnet.txt), run !analyze -v."
        Write-Warning "Recover:  Restore-VMCheckpoint -VMName $VMName -Name $Checkpoint -Confirm:`$false"
        return
    }

    # 8. Verify the driver is up and the redirect callouts are registered.
    Write-Host "==> Verifying driver + WFP callouts" -ForegroundColor Cyan
    $verify = Invoke-Command -Session $session -ScriptBlock {
        $drv = Get-CimInstance Win32_SystemDriver -Filter "Name='NexusWFP'"
        $stateFile = Join-Path $env:TEMP 'nexus-wfp-state.xml'
        netsh wfp show state file=$stateFile | Out-Null
        $txt = Get-Content $stateFile -Raw
        [PSCustomObject]@{
            DriverState = $drv.State
            V4 = [bool]($txt -match 'NexusConnectRedirectV4')
            V6 = [bool]($txt -match 'NexusConnectRedirectV6')
        }
    }
    Write-Host ("    Driver={0}  V4-callout={1}  V6-callout={2}" -f $verify.DriverState, $verify.V4, $verify.V6) -ForegroundColor Green
    if ($verify.DriverState -eq 'Running' -and $verify.V4 -and $verify.V6) {
        Write-Host "==> NexusWFP loaded + both redirect callouts registered." -ForegroundColor Green
    }
    else {
        Write-Warning "Driver did not fully come up (state/callouts above). Inspect the guest WFP state."
    }
}
finally {
    Remove-PSSession $session
}
