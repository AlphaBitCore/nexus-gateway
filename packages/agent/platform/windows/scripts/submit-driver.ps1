#!/usr/bin/env pwsh
# E59-S5: Submit the EV-signed NexusWFP CAT to Microsoft Hardware
# Dev Center for attestation signing, then download the Microsoft-
# signed CAT back into the driver tree.
#
# Inputs (parameters or env):
#   -DriverRoot <path>      Default: packages/agent/platform/windows/nexus-wfp-driver
#   -AzureTenantId <guid>   Env: HWDEVCENTER_TENANT_ID.
#   -AzureClientId <guid>   Env: HWDEVCENTER_CLIENT_ID.
#   -AzureClientSecret <s>  Env: HWDEVCENTER_CLIENT_SECRET (or use a
#                            cert-based auth flow in CI).
#   -ProductName <string>   Default: "Nexus WFP Driver".
#   -PollIntervalSec <int>  Default: 60.
#   -MaxWaitMinutes <int>   Default: 1440 (24 hours, per epic §6 C-2).
#
# Outputs:
#   Replaces $DriverRoot/nexus-wfp.cat with the Microsoft-attestation-
#   signed CAT. The EV-signed .sys files (carrying their EV signature
#   only) are untouched; Microsoft signs the CAT, not the .sys.
#
# Reference:
#   https://learn.microsoft.com/en-us/windows-hardware/drivers/dashboard/hardware-api-reference

[CmdletBinding()]
param(
    [string]$DriverRoot,
    [string]$AzureTenantId,
    [string]$AzureClientId,
    [string]$AzureClientSecret,
    [string]$ProductName = 'Nexus WFP Driver',
    [int]$PollIntervalSec = 60,
    [int]$MaxWaitMinutes = 1440
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# ─── Setup + credential resolution ──────────────────────────────────
if (-not $DriverRoot) {
    $repoRoot = git rev-parse --show-toplevel
    $DriverRoot = Join-Path $repoRoot 'packages/agent/platform/windows/nexus-wfp-driver'
}
if (-not $AzureTenantId)     { $AzureTenantId     = $env:HWDEVCENTER_TENANT_ID }
if (-not $AzureClientId)     { $AzureClientId     = $env:HWDEVCENTER_CLIENT_ID }
if (-not $AzureClientSecret) { $AzureClientSecret = $env:HWDEVCENTER_CLIENT_SECRET }

if (-not $AzureTenantId -or -not $AzureClientId -or -not $AzureClientSecret) {
    throw 'Missing Azure AD credentials. Set HWDEVCENTER_TENANT_ID / HWDEVCENTER_CLIENT_ID / HWDEVCENTER_CLIENT_SECRET, or pass -AzureTenantId / -AzureClientId / -AzureClientSecret.'
}

$catPath = Join-Path $DriverRoot 'nexus-wfp.cat'
if (-not (Test-Path $catPath)) {
    throw "EV-signed CAT not found at $catPath. Run sign-driver.ps1 first."
}

$amd64Sys = Join-Path $DriverRoot 'bin/x64/Release/nexus-wfp.sys'
$arm64Sys = Join-Path $DriverRoot 'bin/ARM64/Release/nexus-wfp.sys'
$infPath  = Join-Path $DriverRoot 'nexus-wfp.inf'

# ─── 1. Acquire Azure AD access token ───────────────────────────────
Write-Host '==> Acquiring Azure AD token'
$tokenBody = @{
    grant_type    = 'client_credentials'
    client_id     = $AzureClientId
    client_secret = $AzureClientSecret
    resource      = 'https://manage.devcenter.microsoft.com'
}
$tokenResp = Invoke-RestMethod `
    -Uri "https://login.microsoftonline.com/$AzureTenantId/oauth2/token" `
    -Method POST `
    -Body $tokenBody `
    -ContentType 'application/x-www-form-urlencoded'
$accessToken = $tokenResp.access_token

$authHeaders = @{
    'Authorization' = "Bearer $accessToken"
    'Content-Type'  = 'application/json'
}

# ─── 2. Build the submission archive ────────────────────────────────
# Microsoft expects a zip containing: nexus-wfp.cat + nexus-wfp.inf
# + each architecture's .sys, packaged with the same structure as
# the inf2cat output.
Write-Host '==> Building submission archive'
$workDir = Join-Path $env:TEMP "nexus-wfp-submit-$(Get-Random)"
New-Item -ItemType Directory -Force -Path $workDir | Out-Null
$amd64Dir = Join-Path $workDir 'x64'
$arm64Dir = Join-Path $workDir 'arm64'
New-Item -ItemType Directory -Force -Path $amd64Dir | Out-Null
New-Item -ItemType Directory -Force -Path $arm64Dir | Out-Null
Copy-Item $catPath  (Join-Path $workDir 'nexus-wfp.cat')
Copy-Item $infPath  (Join-Path $workDir 'nexus-wfp.inf')
Copy-Item $amd64Sys (Join-Path $amd64Dir 'nexus-wfp.sys')
Copy-Item $arm64Sys (Join-Path $arm64Dir 'nexus-wfp.sys')

$archive = Join-Path $env:TEMP "nexus-wfp-submit-$(Get-Random).zip"
Compress-Archive -Path "$workDir\*" -DestinationPath $archive -Force
Write-Host "==> Archive built: $archive"

# ─── 3. Find / create the Product entry ─────────────────────────────
# Hardware Dev Center has Products (a long-lived driver identity) and
# Submissions (per-build attestation requests).
$apiRoot = 'https://manage.devcenter.microsoft.com/v2.0/my/hardware'

Write-Host '==> Looking up Product'
$products = Invoke-RestMethod -Headers $authHeaders -Uri "$apiRoot/products"
$product = $products.value | Where-Object { $_.productName -eq $ProductName } | Select-Object -First 1
if (-not $product) {
    Write-Host "==> Creating Product '$ProductName'"
    $createBody = @{
        productName       = $ProductName
        testHarness       = 'Attestation'
        requestedSignatures = @('WINDOWS_v100_X64', 'WINDOWS_v100_ARM64')
        deviceMetadataIds = @()
        firmwareVersion   = '0'
        deviceType        = 'internalExternal'
    } | ConvertTo-Json -Depth 5
    $product = Invoke-RestMethod -Headers $authHeaders -Method POST -Uri "$apiRoot/products" -Body $createBody
}
$productId = $product.id
Write-Host "==> Product id: $productId"

# ─── 4. Create a Submission and get the SAS upload URI ──────────────
Write-Host '==> Creating Submission'
$submBody = @{
    name = "nexus-wfp $(Get-Date -Format 'yyyy-MM-dd_HH-mm-ss')"
    type = 'initial'
} | ConvertTo-Json -Depth 3
$subm = Invoke-RestMethod -Headers $authHeaders -Method POST -Uri "$apiRoot/products/$productId/submissions" -Body $submBody
$submId = $subm.id
$uploadUri = $subm.downloads.items[0].url

Write-Host "==> Submission id: $submId  upload to: $($uploadUri.Substring(0, [Math]::Min(80, $uploadUri.Length)))..."

# ─── 5. Upload the archive to Azure Blob via SAS URI ────────────────
Write-Host '==> Uploading archive'
Invoke-WebRequest -Method PUT -Uri $uploadUri `
    -Headers @{ 'x-ms-blob-type' = 'BlockBlob' } `
    -InFile $archive `
    -ContentType 'application/zip' | Out-Null
Write-Host '==> Upload complete.'

# ─── 6. Commit the submission ───────────────────────────────────────
Invoke-RestMethod -Headers $authHeaders -Method POST `
    -Uri "$apiRoot/products/$productId/submissions/$submId/commit" `
    -Body '{}' | Out-Null
Write-Host '==> Submission committed; Microsoft is now signing.'

# ─── 7. Poll for completion ─────────────────────────────────────────
$deadline = (Get-Date).AddMinutes($MaxWaitMinutes)
while ((Get-Date) -lt $deadline) {
    Start-Sleep -Seconds $PollIntervalSec
    $cur = Invoke-RestMethod -Headers $authHeaders -Uri "$apiRoot/products/$productId/submissions/$submId"
    $state = $cur.workflowStatus.currentStep
    Write-Host "[$(Get-Date -Format 'HH:mm:ss')] Submission state: $state"
    if ($state -eq 'finalizeIngestion' -or $state -eq 'completed' -or $state -eq 'finalizeAttestationSigning') {
        # Final state — signed CAT is downloadable.
        $downloadUri = $cur.downloads.items | Where-Object { $_.type -eq 'signedPackage' } | Select-Object -ExpandProperty url -First 1
        if ($downloadUri) {
            Write-Host '==> Downloading Microsoft-signed CAT'
            $signedZip = Join-Path $env:TEMP "nexus-wfp-signed-$(Get-Random).zip"
            Invoke-WebRequest -Uri $downloadUri -OutFile $signedZip -UseBasicParsing
            $extractDir = Join-Path $env:TEMP "nexus-wfp-signed-extract-$(Get-Random)"
            Expand-Archive -Path $signedZip -DestinationPath $extractDir
            $signedCat = Get-ChildItem -Recurse -Filter 'nexus-wfp.cat' -Path $extractDir | Select-Object -First 1
            if (-not $signedCat) {
                throw "Microsoft-signed CAT not found in extracted archive at $extractDir"
            }
            Copy-Item $signedCat.FullName $catPath -Force
            Write-Host "==> Microsoft-signed CAT installed at $catPath"
            Get-Item $catPath | Select-Object Name, Length, LastWriteTime | Format-Table
            exit 0
        }
    }
    if ($state -eq 'failed') {
        throw "Submission $submId failed. Inspect at https://partner.microsoft.com/dashboard/hardware/driver/$productId/$submId"
    }
}

throw "Submission $submId did not complete within $MaxWaitMinutes minutes. Inspect manually at https://partner.microsoft.com/dashboard/hardware/."
