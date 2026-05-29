#!/bin/bash
# first-boot-ca.sh — generate per-instance certificates.
#
# Two CAs / keypairs are produced:
#   1. /etc/compliance-proxy/{ca.crt,ca.key}  — Compliance Proxy MITM CA
#      used to mint leaf certs for upstream provider domains.
#   2. /etc/nexus/{tls.crt,tls.key}           — nginx HTTPS cert (self-signed,
#      CN=nexus-gateway). The operator is expected to replace this with a
#      real cert signed for their hostname in production.
#
# Architecture: docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md

set -euo pipefail

PROXY_CA_DIR=/etc/compliance-proxy
NEXUS_DIR=/etc/nexus

# Idempotent — re-issuing the MITM CA invalidates every agent's trust store
# entry (operators have to redistribute the new ca.crt). Re-issuing the nginx
# cert is harmless but pointless.
if [ -f "$PROXY_CA_DIR/ca.crt" ] && [ -f "$NEXUS_DIR/tls.crt" ]; then
  echo "[first-boot-ca] CAs already present; skipping (idempotent)."
  exit 0
fi

echo "[first-boot-ca] generating Compliance Proxy MITM CA..."

# ECDSA P-256 — small + fast leaf signing; matches dev CA shape used by
# packages/compliance-proxy/dev-certs/.
openssl ecparam -genkey -name prime256v1 -noout -out "$PROXY_CA_DIR/ca.key"
openssl req -x509 -new -nodes -key "$PROXY_CA_DIR/ca.key" -sha256 -days 3650 \
  -subj "/CN=Nexus Compliance Proxy CA/O=Nexus Gateway" \
  -out "$PROXY_CA_DIR/ca.crt"

chmod 0640 "$PROXY_CA_DIR/ca.crt" "$PROXY_CA_DIR/ca.key"
chown root:nexus "$PROXY_CA_DIR/ca.crt" "$PROXY_CA_DIR/ca.key"

echo "[first-boot-ca] generating nginx HTTPS self-signed cert..."

# Detect the instance's reachable IPs so the cert SAN covers everything Go's
# default TLS client will check against. Without IP SANs, Go's HTTPS client
# rejects `https://<ip>/.well-known/jwks.json` with x509: cannot validate
# certificate for <ip> because it doesn't contain any IP SANs — tokens are
# issued correctly at /oauth/token but cannot be verified at /api/admin/me,
# the SPA bounces back to /login on every login attempt. Hit on 2026-05-29.
TOKEN=$(curl -fsS -X PUT "http://169.254.169.254/latest/api/token" \
  -H "X-aws-ec2-metadata-token-ttl-seconds: 21600" -m 3 2>/dev/null || true)
PUBLIC_IP=""
LOCAL_IP=""
if [ -n "$TOKEN" ]; then
  PUBLIC_IP=$(curl -fsS -H "X-aws-ec2-metadata-token: $TOKEN" \
    http://169.254.169.254/latest/meta-data/public-ipv4 -m 3 2>/dev/null || true)
  LOCAL_IP=$(curl -fsS -H "X-aws-ec2-metadata-token: $TOKEN" \
    http://169.254.169.254/latest/meta-data/local-ipv4 -m 3 2>/dev/null || true)
fi
SAN="IP:127.0.0.1,DNS:nexus-gateway,DNS:localhost"
[ -n "$PUBLIC_IP" ] && SAN="IP:${PUBLIC_IP},${SAN}"
[ -n "$LOCAL_IP" ] && [ "$LOCAL_IP" != "$PUBLIC_IP" ] && SAN="${SAN},IP:${LOCAL_IP}"
echo "[first-boot-ca]   cert SAN: ${SAN}"

openssl req -x509 -nodes -newkey rsa:2048 -days 365 \
  -subj "/CN=nexus-gateway/O=Nexus Gateway" \
  -addext "subjectAltName=${SAN}" \
  -keyout "$NEXUS_DIR/tls.key" \
  -out    "$NEXUS_DIR/tls.crt" 2>/dev/null

chmod 0640 "$NEXUS_DIR/tls.crt" "$NEXUS_DIR/tls.key"
chown root:nexus "$NEXUS_DIR/tls.crt" "$NEXUS_DIR/tls.key"


# Install the nginx self-signed cert into the system CA trust store. Without
# this, Go's default HTTP client (used by the JWT verifier's JWKS fetcher in
# the control-plane) rejects the self-signed cert with x509 "unknown
# authority" — tokens are issued correctly at /oauth/token but cannot be
# verified at /api/admin/me, the SPA bounces back to /login on every login
# attempt. Hit on 2026-05-29 first-user-test of build #10. Acceptable: the
# anchor is per-instance and only ever signs this appliance's own hostname.
echo "[first-boot-ca] trusting self-signed nginx cert in the system CA bundle..."
install -o root -g root -m 0644 "$NEXUS_DIR/tls.crt" \
  /etc/pki/ca-trust/source/anchors/nexus-gateway.crt
update-ca-trust

echo "[first-boot-ca] complete (proxy CA + nginx self-signed cert + system CA anchor)."
