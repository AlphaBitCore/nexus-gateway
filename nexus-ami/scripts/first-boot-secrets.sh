#!/bin/bash
# first-boot-secrets.sh — generate 5 per-instance [MUST MATCH] secrets and
# write them into per-service env files under /etc/nexus/.
#
# Five secrets per .env.example contract:
#   INTERNAL_SERVICE_TOKEN       — all 4 services share (Hub bearer + X-RS-Token)
#   ADMIN_KEY_HMAC_SECRET        — control-plane + ai-gateway share (VK/admin key hashing)
#   CREDENTIAL_ENCRYPTION_KEY    — control-plane + ai-gateway share (AES-256, 64 hex)
#   COMPLIANCE_PROXY_API_TOKEN   — control-plane + compliance-proxy share
#   AI_GATEWAY_API_TOKEN         — ai-gateway only (its /runtime/* admin surface)
#
# DATABASE_URL is added later by first-boot-db.sh after the DB is created.
# Architecture: docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md

set -euo pipefail

CONFIG_DIR=/etc/nexus

# Idempotent — if env files already exist, secrets are already minted. Re-
# running would invalidate previously-encrypted DB rows (CREDENTIAL_ENCRYPTION_KEY
# rotation), break already-issued admin keys (ADMIN_KEY_HMAC_SECRET rotation),
# and break inter-service auth (INTERNAL_SERVICE_TOKEN rotation). Hit on
# 2026-05-28 when an operator manually re-invoked nexus-first-boot to recover
# from a Bug-1 deadlock and silently rotated secrets while the DB still held
# rows encrypted with the prior set.
if [ -f "$CONFIG_DIR/nexus-hub.env" ]; then
  echo "[first-boot-secrets] env files already present in $CONFIG_DIR/; skipping (idempotent)."
  exit 0
fi

echo "[first-boot-secrets] generating 5 per-instance secrets..."

INTERNAL_SERVICE_TOKEN=$(openssl rand -hex 32)
ADMIN_KEY_HMAC_SECRET=$(openssl rand -hex 32)
CREDENTIAL_ENCRYPTION_KEY=$(openssl rand -hex 32)
COMPLIANCE_PROXY_API_TOKEN=$(openssl rand -hex 32)
AI_GATEWAY_API_TOKEN=$(openssl rand -hex 32)

cat > "$CONFIG_DIR/nexus-hub.env" <<EOF
INTERNAL_SERVICE_TOKEN=$INTERNAL_SERVICE_TOKEN
NEXUS_HUB_URL=http://127.0.0.1:3060
EOF

cat > "$CONFIG_DIR/control-plane.env" <<EOF
INTERNAL_SERVICE_TOKEN=$INTERNAL_SERVICE_TOKEN
ADMIN_KEY_HMAC_SECRET=$ADMIN_KEY_HMAC_SECRET
CREDENTIAL_ENCRYPTION_KEY=$CREDENTIAL_ENCRYPTION_KEY
COMPLIANCE_PROXY_API_TOKEN=$COMPLIANCE_PROXY_API_TOKEN
NEXUS_HUB_URL=http://127.0.0.1:3060
AI_GATEWAY_URL=http://127.0.0.1:3050
COMPLIANCE_PROXY_URL=http://127.0.0.1:3040
COMPLIANCE_PROXY_RUNTIME_URL=http://127.0.0.1:3040
EOF

cat > "$CONFIG_DIR/ai-gateway.env" <<EOF
INTERNAL_SERVICE_TOKEN=$INTERNAL_SERVICE_TOKEN
ADMIN_KEY_HMAC_SECRET=$ADMIN_KEY_HMAC_SECRET
CREDENTIAL_ENCRYPTION_KEY=$CREDENTIAL_ENCRYPTION_KEY
AI_GATEWAY_API_TOKEN=$AI_GATEWAY_API_TOKEN
NEXUS_HUB_URL=http://127.0.0.1:3060
EOF

cat > "$CONFIG_DIR/compliance-proxy.env" <<EOF
INTERNAL_SERVICE_TOKEN=$INTERNAL_SERVICE_TOKEN
COMPLIANCE_PROXY_API_TOKEN=$COMPLIANCE_PROXY_API_TOKEN
NEXUS_HUB_URL=http://127.0.0.1:3060
EOF

chmod 0640 "$CONFIG_DIR"/*.env
chown root:nexus "$CONFIG_DIR"/*.env

echo "[first-boot-secrets] wrote 4 env files under $CONFIG_DIR/ (mode 0640, root:nexus)."
