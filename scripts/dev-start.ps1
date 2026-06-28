#!/usr/bin/env pwsh
#Requires -Version 7.0

# ─── Nexus Gateway — Local Development Bootstrap (Windows / PowerShell) ──────────
# Native PowerShell equivalent of scripts/dev-start.sh: checks prerequisites,
# starts the Docker infra (PostgreSQL + Valkey + NATS), bootstraps the repo-root
# .env, runs Prisma migrations + seed, then optionally starts the Control Plane
# UI. The four Go services run as host processes (see the startup guide this
# prints) — only the stateful infra is containerized.
#
# Usage:
#   ./scripts/dev-start.ps1                  # bootstrap + start Control Plane UI (PRESERVES existing DB data)
#   ./scripts/dev-start.ps1 -NoDev           # bootstrap only; start services manually
#   ./scripts/dev-start.ps1 -ForceReset      # DESTRUCTIVE: wipe DB + docker volumes, then bootstrap + start UI
#   ./scripts/dev-start.ps1 -ForceReset -NoDev
#
# IMPORTANT: -ForceReset wipes ALL local data including traffic_event, audit log,
# virtual keys, etc. Use it only when you genuinely want a clean slate. The
# default (no flag) runs `prisma db push` WITHOUT --force-reset, so additive
# schema changes apply but existing rows survive.
#
# Windows notes:
#   - Uses 127.0.0.1 everywhere (the .env DATABASE_URL/REDIS_ADDRS already do):
#     `localhost` resolves to IPv6 ::1 first on Windows and Docker-published
#     ports answer on IPv4 only, so Node/Prisma/curl hang on `localhost`.
#   - Dev secrets are generated with the .NET RNG — no openssl dependency (unlike
#     the bash script). openssl is still used (if present) only for the optional
#     Compliance Proxy dev CA; missing openssl just warn-skips that one step.

[CmdletBinding()]
param(
    [switch]$ForceReset,
    [switch]$NoDev,
    [switch]$Reset
)

$ErrorActionPreference = 'Stop'

$RepoDir = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path

# ─── Output helpers ─────────────────────────────────────────────────────────
function Write-Log  { param([string]$m) Write-Host "[nexus] $m" -ForegroundColor Cyan }
function Write-Ok   { param([string]$m) Write-Host "  ✔ $m" -ForegroundColor Green }
function Write-Warn { param([string]$m) Write-Host "  ⚠  $m" -ForegroundColor Yellow }
function Write-Err  { param([string]$m) Write-Host "  ✖ $m" -ForegroundColor Red; exit 1 }

# Generate 32 random bytes as 64 lowercase hex chars (openssl rand -hex 32 equivalent).
function New-DevHexSecret {
    $bytes = [byte[]]::new(32)
    [System.Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
    return ([System.Convert]::ToHexString($bytes)).ToLowerInvariant()
}

# Write text as UTF-8 WITHOUT a BOM — a BOM on the first line of .env makes Go's
# godotenv parse the first key with a leading ﻿, silently breaking it.
function Write-FileNoBom {
    param([string]$Path, [string]$Content)
    $utf8NoBom = [System.Text.UTF8Encoding]::new($false)
    [System.IO.File]::WriteAllText($Path, $Content, $utf8NoBom)
}

# Poll a probe scriptblock until it exits 0. The probe is a native command whose
# $LASTEXITCODE we inspect (output suppressed).
function Wait-Until {
    param(
        [scriptblock]$Probe,
        [int]$Retries,
        [string]$What,
        [switch]$WarnOnly
    )
    for ($i = 0; $i -lt $Retries; $i++) {
        & $Probe *> $null
        if ($LASTEXITCODE -eq 0) { return $true }
        Start-Sleep -Seconds 1
    }
    if ($WarnOnly) { Write-Warn "$What failed to become ready"; return $false }
    Write-Err "$What failed to become ready"
}

if ($Reset) {
    Write-Err "-Reset has been renamed to -ForceReset (the old name didn't make the destructive intent obvious). Re-run with -ForceReset if you really want to wipe the local DB + docker volumes."
}
if ($ForceReset) {
    Write-Log "Force-reset mode: WILL WIPE the local Postgres / Valkey / NATS volumes + the entire nexus_gateway database before re-applying schema. All traffic_event, audit log, virtual keys, etc. will be lost."
}
if ($NoDev) {
    Write-Log "Bootstrap only (-NoDev): will not start dev servers automatically"
}

# ─── 1. Check prerequisites ─────────────────────────────────────────────────

Write-Log "Checking prerequisites..."

foreach ($tool in @(
    @{ Name = 'docker'; Msg = 'Docker is not installed. Install Docker Desktop from https://docker.com' },
    @{ Name = 'node';   Msg = 'Node.js is not installed. Install v20+ from https://nodejs.org' },
    @{ Name = 'npm';    Msg = 'npm is not installed.' },
    @{ Name = 'go';     Msg = 'Go is not installed. Install Go 1.25+ from https://go.dev/dl/' }
)) {
    if (-not (Get-Command $tool.Name -ErrorAction SilentlyContinue)) { Write-Err $tool.Msg }
}

$nodeMajor = [int](((node -v) -replace '^v', '') -split '\.')[0]
if ($nodeMajor -lt 20) { Write-Err "Node.js v20+ required (found $(node -v))" }

$goVer = ((go version) -split '\s+')[2] -replace '^go', ''
$goParts = $goVer -split '\.'
$goMajor = [int]$goParts[0]
$goMinor = [int]$goParts[1]
if ($goMajor -lt 1 -or ($goMajor -eq 1 -and $goMinor -lt 25)) {
    Write-Warn "Go 1.25+ recommended for this repo (found go$goVer)"
}

$hasOpenssl = [bool](Get-Command openssl -ErrorAction SilentlyContinue)
if (-not $hasOpenssl) {
    Write-Warn "openssl not found; the Compliance Proxy dev CA step will be skipped (not needed to bring the agent / Hub / CP online)"
}

Write-Ok "Node.js $(node -v) | npm $(npm -v) | Go $((go version) -split '\s+' | Select-Object -Index 2) | Docker $((docker --version) -split '\s+' | Select-Object -Index 2)"

# ─── 1b. Bootstrap repo-root .env (service boot secrets) ────────────────────
# packages/shared/core/bootenv loads <repo-root>/.env into each Go binary at
# process start. Without it, Control Plane errors at boot ("INTERNAL_SERVICE_TOKEN
# is not set") and ai-gateway can't decrypt Credential rows. We copy .env.example
# and substitute the CHANGE_ME_* placeholders with safe dev defaults. Secret-
# valued vars (HMAC + credential-encryption key) get real per-developer random
# values; the fixed shared tokens match the values used elsewhere in the repo.

Set-Location $RepoDir
Write-Log "Bootstrapping repo-root .env (service boot secrets)..."

$rootEnvPath = Join-Path $RepoDir '.env'
if (Test-Path $rootEnvPath) {
    Write-Ok ".env exists (no changes — edit by hand if you need to rotate secrets)"
}
else {
    $exPath = Join-Path $RepoDir '.env.example'
    if (-not (Test-Path $exPath)) { Write-Err ".env is missing and .env.example was not found at repo root" }

    $devEncryptionKey = New-DevHexSecret  # AES-256 master key (64 hex chars)
    # SEC-M9-01: the HMAC secret must be a real per-developer random value — the CP
    # fails closed on an empty secret and ships NO committed fallback.
    $devHmacSecret = New-DevHexSecret

    $envText = [System.IO.File]::ReadAllText($exPath)
    $subs = @(
        @{ Pattern = '(?m)^INTERNAL_SERVICE_TOKEN=.*$';     Value = 'INTERNAL_SERVICE_TOKEN=dev-service-token' },
        @{ Pattern = '(?m)^HUB_CONFIG_TOKEN=.*$';           Value = 'HUB_CONFIG_TOKEN=dev-hub-config-token' },
        @{ Pattern = '(?m)^ADMIN_KEY_HMAC_SECRET=.*$';      Value = "ADMIN_KEY_HMAC_SECRET=$devHmacSecret" },
        @{ Pattern = '(?m)^CREDENTIAL_ENCRYPTION_KEY=.*$';  Value = "CREDENTIAL_ENCRYPTION_KEY=$devEncryptionKey" },
        @{ Pattern = '(?m)^COMPLIANCE_PROXY_API_TOKEN=.*$'; Value = 'COMPLIANCE_PROXY_API_TOKEN=dev-compliance-proxy-token' },
        # Wire the web assistant ("Chat with Nexus") to the seeded bootstrap
        # system-assistant virtual key so /assistant/chat works out of the box.
        @{ Pattern = '(?m)^NEXUS_ASSISTANT_SYSTEM_VK=.*$';  Value = 'NEXUS_ASSISTANT_SYSTEM_VK=nvk_local_b0075000' }
    )
    foreach ($s in $subs) {
        $envText = [System.Text.RegularExpressions.Regex]::Replace($envText, $s.Pattern, [System.Text.RegularExpressions.MatchEvaluator] { param($m) $s.Value })
    }
    Write-FileNoBom -Path $rootEnvPath -Content $envText
    Write-Ok "Created repo-root .env from .env.example with dev-default secrets"

    # Hard-fail if any CHANGE_ME_ placeholder survived on a real KEY=VALUE line.
    if (Select-String -Path $rootEnvPath -Pattern '^[A-Z][A-Z0-9_]*=CHANGE_ME_' -Quiet) {
        Write-Err ".env still contains CHANGE_ME_* placeholders after substitution — update scripts/dev-start.ps1 to cover the new variable"
    }
}

# ─── 2. Start Docker services ───────────────────────────────────────────────

Write-Log "Starting Docker services (PostgreSQL + Valkey + NATS)..."
Set-Location $RepoDir

if ($ForceReset) {
    docker compose down -v 2>$null
}

docker compose up -d
if ($LASTEXITCODE -ne 0) { Write-Err "docker compose up -d failed" }

Write-Log "Waiting for PostgreSQL..."
Wait-Until -Retries 30 -What 'PostgreSQL' -Probe { docker compose exec -T postgres pg_isready -U postgres } | Out-Null
Write-Ok "PostgreSQL ready (127.0.0.1:55532)"

# Valkey is Redis-wire-compatible (E61-S3 swap). The container CLI is valkey-cli.
Write-Log "Waiting for Valkey (Redis-wire-compatible)..."
Wait-Until -Retries 15 -What 'Valkey' -Probe { docker compose exec -T valkey valkey-cli ping } | Out-Null
Write-Ok "Valkey ready (127.0.0.1:6437; speaks Redis protocol)"

Write-Log "Waiting for NATS..."
$natsOk = Wait-Until -Retries 15 -What 'NATS' -WarnOnly -Probe { docker compose exec -T nats wget -q --spider http://localhost:8222/healthz }
if ($natsOk) { Write-Ok "NATS JetStream ready (127.0.0.1:4222)" }
else { Write-Warn "NATS not ready — Hub consumers will not function without it" }

# ─── 3. Install npm dependencies ────────────────────────────────────────────

Write-Log "Installing npm dependencies..."
Set-Location $RepoDir
npm install --silent
if ($LASTEXITCODE -ne 0) { Write-Err "npm install failed" }
Write-Ok "npm dependencies installed"

# ─── 4. Run Prisma migrations ───────────────────────────────────────────────

Write-Log "Running database migrations (tools/db-migrate)..."
$dbMigrateDir = Join-Path $RepoDir 'tools/db-migrate'
Set-Location $dbMigrateDir

# Prisma reads DATABASE_URL from tools/db-migrate/.env via dotenv. Bootstrap it
# from .env.example on first run.
$migrateEnvPath = Join-Path $dbMigrateDir '.env'
if (-not (Test-Path $migrateEnvPath)) {
    $migrateExample = Join-Path $dbMigrateDir '.env.example'
    if (Test-Path $migrateExample) {
        Copy-Item $migrateExample $migrateEnvPath
        Write-Ok "Created tools/db-migrate/.env from .env.example (override locally if needed)"
    }
    else {
        Write-Err "tools/db-migrate/.env is missing and .env.example was not found"
    }
}

# Mirror the seed's required secrets from the repo-root .env into this dir's .env.
# seed.ts runs from this dir via tsx and only reads tools/db-migrate/.env, so the
# repo-root .env is not visible without this step; both MUST match the running
# services' values or the demo tier aborts.
if (Test-Path $rootEnvPath) {
    $rootLines = [System.IO.File]::ReadAllLines($rootEnvPath)
    foreach ($keyName in @('CREDENTIAL_ENCRYPTION_KEY', 'ADMIN_KEY_HMAC_SECRET')) {
        $rootLine = $rootLines | Where-Object { $_ -match "^$keyName=" } | Select-Object -First 1
        if ($rootLine) {
            $rootVal = ($rootLine -split '=', 2)[1]
            $migrateText = [System.IO.File]::ReadAllText($migrateEnvPath)
            if ($migrateText -match "(?m)^$keyName=") {
                $migrateText = [System.Text.RegularExpressions.Regex]::Replace($migrateText, "(?m)^$keyName=.*$", [System.Text.RegularExpressions.MatchEvaluator] { param($m) "$keyName=$rootVal" })
            }
            else {
                if ($migrateText.Length -gt 0 -and -not $migrateText.EndsWith("`n")) { $migrateText += "`n" }
                $migrateText += "$keyName=$rootVal`n"
            }
            Write-FileNoBom -Path $migrateEnvPath -Content $migrateText
            Write-Ok "Propagated $keyName from repo-root .env into tools/db-migrate/.env"
        }
    }
}

if ($ForceReset) {
    npx prisma db push --force-reset
    if ($LASTEXITCODE -ne 0) { Write-Err "prisma db push --force-reset failed" }
    Write-Ok "Database wiped + schema re-applied (-ForceReset)"
}
else {
    npx prisma db push
    if ($LASTEXITCODE -ne 0) { Write-Err "prisma db push failed" }
    Write-Ok "Database schema pushed (data preserved — use -ForceReset to wipe)"
}

# ─── 4a-bis. Apply post-push schema extras `prisma db push` cannot express ────
# PostgreSQL-native RANGE partitioning has no Prisma representation and lives in
# hand-written SQL. Without re-applying it, metric_ops_raw stays a plain table and
# the Hub `ops-raw-partition` job fails every cycle (SQLSTATE 42P17). Re-runnable.
$extrasSql = Join-Path $dbMigrateDir 'schema-extras.sql'
if (Test-Path $extrasSql) {
    # PowerShell has no `<` input redirection — pipe the file content into psql.
    Get-Content -Raw $extrasSql | docker exec -i nexus-postgres psql -U postgres -d nexus_gateway -q -v ON_ERROR_STOP=1 *> $null
    if ($LASTEXITCODE -eq 0) {
        Write-Ok "Applied schema-extras.sql (metric_ops_raw → RANGE-partitioned)"
    }
    else {
        Write-Warn "Could not apply schema-extras.sql — Hub ops-raw-partition job will error until fixed"
    }
}
else {
    Write-Warn "schema-extras.sql not found at $extrasSql — Hub ops-raw-partition job may error"
}

# ─── 4b. Seed database ─────────────────────────────────────────────────────

Write-Log "Seeding database..."
npx prisma db seed
if ($LASTEXITCODE -ne 0) { Write-Err "prisma db seed failed" }
Write-Ok "Database seeded"

# ─── 4c. Compliance Proxy dev CA (TLS-bump cert issuer) ─────────────────────
# compliance-proxy.dev.yaml points the cert issuer at ./dev-certs/ca.{crt,key}.
# Without this CA the proxy aborts at boot. Skipped (with a warning) when openssl
# is absent — the proxy is not required to bring the agent / Hub / CP online.
$cpDir = Join-Path $RepoDir 'packages/compliance-proxy'
Set-Location $cpDir
$caCrt = Join-Path $cpDir 'dev-certs/ca.crt'
$caKey = Join-Path $cpDir 'dev-certs/ca.key'
if ((Test-Path $caCrt) -and (Test-Path $caKey)) {
    Write-Ok "Compliance Proxy dev CA already present (packages/compliance-proxy/dev-certs/)"
}
elseif ($hasOpenssl) {
    New-Item -ItemType Directory -Force (Join-Path $cpDir 'dev-certs') | Out-Null
    openssl ecparam -name prime256v1 -genkey -noout -out dev-certs/ca.key 2>$null
    # pathlen:0 — the proxy CA only ever signs leaf certs; the constraint stops a
    # stolen CA key from minting an intermediate CA that devices would trust.
    openssl req -new -x509 -key dev-certs/ca.key -out dev-certs/ca.crt -days 365 `
        -subj "/O=Nexus Dev/CN=Nexus Compliance Proxy Dev CA" `
        -addext "basicConstraints=critical,CA:TRUE,pathlen:0" 2>$null
    if ($LASTEXITCODE -eq 0) {
        Write-Ok "Generated Compliance Proxy dev CA (packages/compliance-proxy/dev-certs/{ca.crt,ca.key})"
    }
    else {
        Write-Warn "openssl failed to generate the Compliance Proxy dev CA — run 'make dev-certs' in packages/compliance-proxy/ before starting the proxy."
    }
}
else {
    Write-Warn "openssl missing — skipping Compliance Proxy dev CA. Run 'make dev-certs' in packages/compliance-proxy/ before starting the proxy."
}

# ─── 5. Startup guide ───────────────────────────────────────────────────────

Set-Location $RepoDir
Write-Host ""
Write-Host "═══════════════════════════════════════════════════════════════════" -ForegroundColor Green
Write-Host "  Bootstrap complete — start each service in a separate terminal:" -ForegroundColor Green
Write-Host "═══════════════════════════════════════════════════════════════════" -ForegroundColor Green
Write-Host ""
Write-Host "  Nexus Hub (port 3060):" -ForegroundColor Cyan
Write-Host "    cd packages/nexus-hub; go run ./cmd/nexus-hub/ -config nexus-hub.dev.yaml"
Write-Host ""
Write-Host "  Control Plane (port 3001):" -ForegroundColor Cyan
Write-Host "    cd packages/control-plane; go run ./cmd/control-plane/ -config control-plane.dev.yaml"
Write-Host ""
Write-Host "  Control Plane UI (port 3000):" -ForegroundColor Cyan
Write-Host "    npm run dev:control-plane-ui"
Write-Host ""
Write-Host "  AI Gateway (port 3050):" -ForegroundColor Cyan
Write-Host "    cd packages/ai-gateway; go run ./cmd/ai-gateway/ -config ai-gateway.dev.yaml"
Write-Host ""
Write-Host "  Compliance Proxy (proxy :3128, runtime API :3040):" -ForegroundColor Cyan
Write-Host "    cd packages/compliance-proxy; go run ./cmd/compliance-proxy/ -config compliance-proxy.dev.yaml"
Write-Host ""
Write-Host "  Tip: the Go services need CGO on Windows (audit DB = go-sqlcipher)." -ForegroundColor Yellow
Write-Host "       Set `$env:CGO_ENABLED='1' and put mingw-w64 gcc on PATH, or the" -ForegroundColor Yellow
Write-Host "       agent build dies at runtime. Without -config <svc>.dev.yaml the" -ForegroundColor Yellow
Write-Host "       binary fails fast on required dev fields." -ForegroundColor Yellow
Write-Host ""
Write-Host "═══════════════════════════════════════════════════════════════════" -ForegroundColor Green
Write-Host "  Once the services are up — try it (seeded demo):" -ForegroundColor Green
Write-Host "═══════════════════════════════════════════════════════════════════" -ForegroundColor Green
Write-Host "  Console: http://localhost:3000  —  log in: admin@nexus.ai / nexus-demo" -ForegroundColor Cyan
Write-Host "  Add a provider API key (Settings → Providers) before any request will succeed." -ForegroundColor Yellow
Write-Host ""
Write-Host "  Stop Docker: docker compose down" -ForegroundColor Yellow
Write-Host ""

if ($NoDev) {
    Write-Ok "Bootstrap complete. Start services manually using the commands above."
    exit 0
}

# ─── 6. Start Control Plane UI ──────────────────────────────────────────────

Write-Log "Starting Control Plane UI (Ctrl+C to stop)..."
npm run dev:control-plane-ui
