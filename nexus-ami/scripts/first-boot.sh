#!/bin/bash
# first-boot.sh — orchestrator. Runs ONCE per launched instance.
# Triggered by nexus-first-boot.service BEFORE any Nexus service starts.
#
# Idempotent: presence of /etc/nexus/.initialized short-circuits everything.
# Architecture: docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md

set -euo pipefail

MARKER=/etc/nexus/.initialized
SCRIPT_DIR=/usr/local/sbin

if [ -f "$MARKER" ]; then
  echo "[nexus-first-boot] already initialized (marker $MARKER present); skipping."
  exit 0
fi

echo "[nexus-first-boot] starting initialization..."

mkdir -p /etc/nexus /etc/compliance-proxy /var/lib/nexus /var/log/nexus
chown root:nexus /etc/nexus /etc/compliance-proxy
chmod 0750 /etc/nexus /etc/compliance-proxy
chown nexus:nexus /var/lib/nexus /var/log/nexus
chmod 0750 /var/lib/nexus /var/log/nexus

"$SCRIPT_DIR/nexus-first-boot-secrets"
"$SCRIPT_DIR/nexus-first-boot-ca"

# Stamp publicURL into each service's yaml — the four config validators
# refuse to start if this top-level field is empty. Detect the instance's
# reachable IP (EC2 IMDSv2 → public-ipv4 first, local-ipv4 fallback for
# private-subnet deployments; on non-EC2 use hostname -I). Each service
# gets the right URL shape for the port/scheme it exposes externally.
# Idempotent — re-running won't double-write because we grep before sed.
# Hit on 2026-05-28 first-launch test of build #8 after the upstream merge
# added publicURL as a required validator field.
echo "[nexus-first-boot] resolving instance publicURL..."
IP=""
TOKEN=$(curl -fsS -X PUT "http://169.254.169.254/latest/api/token" \
  -H "X-aws-ec2-metadata-token-ttl-seconds: 21600" -m 3 2>/dev/null || true)
if [ -n "$TOKEN" ]; then
  IP=$(curl -fsS -H "X-aws-ec2-metadata-token: $TOKEN" \
    http://169.254.169.254/latest/meta-data/public-ipv4 -m 3 2>/dev/null || true)
  [ -z "$IP" ] && IP=$(curl -fsS -H "X-aws-ec2-metadata-token: $TOKEN" \
    http://169.254.169.254/latest/meta-data/local-ipv4 -m 3 2>/dev/null || true)
fi
[ -z "$IP" ] && IP=$(hostname -I 2>/dev/null | awk '{print $1}')
[ -z "$IP" ] && IP=127.0.0.1
echo "[nexus-first-boot] publicURL host = $IP"

stamp_public_url() {
  local yaml="$1"; local url="$2"
  if grep -q '^publicURL:' "$yaml"; then
    echo "[nexus-first-boot] $yaml already has publicURL; skipping."
  else
    sed -i "1i publicURL: \"$url\"" "$yaml"
    echo "[nexus-first-boot] $yaml <- publicURL=$url"
  fi
}
stamp_public_url /etc/nexus/nexus-hub.config.yaml        "http://${IP}:3060"
stamp_public_url /etc/nexus/control-plane.config.yaml    "https://${IP}/"
stamp_public_url /etc/nexus/ai-gateway.config.yaml       "https://${IP}/v1"
stamp_public_url /etc/nexus/compliance-proxy.config.yaml "http://${IP}:3128"

# Stamp AUTH_SERVER_ISSUER into control-plane.env (env override fills the
# yaml's empty authServer.issuer placeholder). Must match the publicURL the
# CP advertises so JWT iss-claim validation + JWKS fetch line up. Idempotent
# replace, not append.
if ! grep -q '^AUTH_SERVER_ISSUER=' /etc/nexus/control-plane.env; then
  echo "AUTH_SERVER_ISSUER=https://${IP}/" >> /etc/nexus/control-plane.env
  echo "[nexus-first-boot] /etc/nexus/control-plane.env <- AUTH_SERVER_ISSUER=https://${IP}/"
fi

# Hub needs the SAME issuer: it pins jwt.WithIssuer(cfg.AuthServer.Issuer)
# when verifying CP-minted enrollment JWTs (enrollment_handler.go). Without
# this override the Hub keeps the yaml's loopback issuer while CP mints
# tokens with iss=https://<IP>/ — every remote agent enrollment through
# :443 fails with an issuer-mismatch 401.
if ! grep -q '^AUTH_SERVER_ISSUER=' /etc/nexus/nexus-hub.env; then
  echo "AUTH_SERVER_ISSUER=https://${IP}/" >> /etc/nexus/nexus-hub.env
  echo "[nexus-first-boot] /etc/nexus/nexus-hub.env <- AUTH_SERVER_ISSUER=https://${IP}/"
fi

"$SCRIPT_DIR/nexus-first-boot-db"

# Register this instance's redirect URI on the cp-ui OAuth client. The seed
# ships with localhost / cp.nexus.ai defaults; without this update an admin
# launching the appliance and clicking "Login" gets a 400 invalid_request
# from /oauth/authorize because the per-instance redirect_uri is not in the
# OAuthClient.redirectUris array. Idempotent — array_append fires only if
# missing. Runs as the postgres OS user (peer auth in pg_hba.conf). Hit on
# 2026-05-29 first-user-test of build #10.
echo "[nexus-first-boot] registering cp-ui redirect_uri for this instance..."
sudo -u postgres psql -d nexus_gateway -v ON_ERROR_STOP=1 <<SQL
UPDATE "OAuthClient"
SET "redirectUris" = array_append("redirectUris", 'https://${IP}/auth/callback'),
    "updatedAt" = NOW()
WHERE "id" = 'cp-ui'
  AND NOT ('https://${IP}/auth/callback' = ANY("redirectUris"));
SQL

touch "$MARKER"
chmod 0644 "$MARKER"

# At first boot the four nexus-* services tried to start in parallel with
# postgresql.service / nats.service / valkey.service, but postgresql's
# ExecStartPre `postgresql-check-db-dir` aborted (no PG_VERSION until
# first-boot-db.sh runs initdb). That marks postgresql as `failed`, which
# cascades a sticky "Dependency failed" into nexus-hub / cp / gateway /
# proxy. systemd does not auto-retry sticky dependency failures, so the
# services stay inactive until a manual restart or reboot. Hit on
# 2026-05-29 first-launch test of build #9. Reset + start them here so the
# instance reaches a fully healthy state on first boot. --no-block because
# we are a Type=oneshot unit and must not wait on these long-running
# dependents.
# ─── High-concurrency runtime tuning (RAM-relative; computed here, not at
# build time, because the build instance RAM differs from the runtime one) ──
MEM_MB=$(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo)

# GOMEMLIMIT: a Go soft memory cap so the GC defends a ceiling before the
# kernel OOM-kills a service under a burst of large request/response bodies.
# Total Go budget ~40% of RAM (Postgres shared_buffers ~25%, Valkey ~15%),
# split by working-set size — the AI Gateway (largest bodies + caches) gets
# the biggest share. Idempotent: only stamped if not already present.
GO_BUDGET=$(( MEM_MB * 40 / 100 ))
set_gomemlimit() {  # $1 = env file, $2 = share percent of the Go budget
  local f=$1 pct=$2 mib
  [ -f "$f" ] || { echo "[nexus-first-boot] WARN: $f missing, skipping GOMEMLIMIT"; return; }
  mib=$(( GO_BUDGET * pct / 100 )); [ "$mib" -lt 256 ] && mib=256
  if ! grep -q '^GOMEMLIMIT=' "$f"; then
    echo "GOMEMLIMIT=${mib}MiB" >> "$f"
    echo "[nexus-first-boot] $f <- GOMEMLIMIT=${mib}MiB"
  fi
}
set_gomemlimit /etc/nexus/ai-gateway.env       35
set_gomemlimit /etc/nexus/nexus-hub.env        30
set_gomemlimit /etc/nexus/control-plane.env    20
set_gomemlimit /etc/nexus/compliance-proxy.env 15

# NEXUS_EVENTS storage tier: pin DURABLE FILE storage on the appliance. The code
# default is in-memory (the perf rig's disk-bandwidth lever — it frees the data
# disk for throughput), but on this single-box, single-broker compliance appliance
# a NATS restart with an in-memory stream would drop published-but-undrained audit
# events, violating the no-loss audit promise. Durability outranks the disk-write
# saving here. Idempotent: only stamped if not already present. The Hub owns the
# stream config (it calls EnsureStreams), so this lives in nexus-hub.env.
if [ -f /etc/nexus/nexus-hub.env ] && ! grep -q '^NEXUS_EVENTS_STORAGE=' /etc/nexus/nexus-hub.env; then
  echo "NEXUS_EVENTS_STORAGE=file" >> /etc/nexus/nexus-hub.env
  echo "[nexus-first-boot] /etc/nexus/nexus-hub.env <- NEXUS_EVENTS_STORAGE=file"
fi

# Valkey maxmemory: cap the cache so response-cache growth cannot OOM the
# co-located box. ~15% of RAM; the allkeys-lru policy is already in
# valkey.conf. Stamped idempotently, then restart so it takes effect before
# the Go services begin filling the cache.
VK_MB=$(( MEM_MB * 15 / 100 )); [ "$VK_MB" -lt 256 ] && VK_MB=256
if [ -f /etc/valkey/valkey.conf ] && ! grep -q '^maxmemory ' /etc/valkey/valkey.conf; then
  echo "maxmemory ${VK_MB}mb" >> /etc/valkey/valkey.conf
  echo "[nexus-first-boot] /etc/valkey/valkey.conf <- maxmemory ${VK_MB}mb"
  systemctl restart valkey 2>/dev/null || true
fi

echo "[nexus-first-boot] kicking downstream services (cleared sticky Dependency failed from boot race)..."
# nginx is kicked too — it tries to start before first-boot-ca.sh has written
# /etc/nexus/tls.crt and fails with "cannot load certificate ... No such file".
# Once the cert exists (it does now), a reset-failed + start brings nginx up.
# Hit on 2026-05-29 first-launch test of build #11.
systemctl reset-failed nexus-hub nexus-control-plane nexus-gateway nexus-proxy nginx 2>/dev/null || true
systemctl start --no-block nexus-hub nexus-control-plane nexus-gateway nexus-proxy nginx

echo "[nexus-first-boot] initialization complete."
