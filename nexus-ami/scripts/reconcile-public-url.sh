#!/bin/bash
# reconcile-public-url.sh — keep the appliance's IP-derived config in sync with
# the instance's CURRENT public address. Installed as /usr/local/sbin/nexus-reconcile-url
# and run on EVERY boot by nexus-reconcile-url.service (NOT gated by the
# first-boot marker), and runnable by hand after an address change.
#
# Why this exists: first-boot.sh / first-boot-ca.sh capture the public IP
# exactly ONCE (gated by /etc/nexus/.initialized) and bake it into FOUR places —
#   1. publicURL: in the four service yamls,
#   2. AUTH_SERVER_ISSUER= in control-plane.env,
#   3. the cp-ui OAuthClient.redirectUris row in Postgres,
#   4. the nginx TLS cert SAN (/etc/nexus/tls.crt).
# EC2 hands out a NEW public IPv4 on stop/start (and when an Elastic IP is
# later attached). After such a change the baked values are stale and admin UI
# login breaks in two stages: first /oauth/authorize rejects the new redirect
# with {"error":"invalid_request","error_description":"redirect_uri not
# registered"} (1+3); then, once that is fixed, the password submit appears to
# succeed but the SPA bounces back to /login because the control-plane's JWKS
# fetch over the new publicURL fails cert verification (x509: valid for <old>,
# not <new>) so /api/admin/me returns 401 (2+4). This script reconciles all
# four to the current IP and restarts the affected services.
#
# Design notes:
#   - Fail-safe: if the current address cannot be resolved, or resolves only to
#     loopback while a real address is already baked, we DO NOTHING (never
#     downgrade a working config to 127.0.0.1).
#   - No-op fast path: if the baked IP already equals the current IP, exit 0
#     without touching anything (the common every-boot case → no restart blip).
#   - Standalone by design: first-boot.sh (high blast radius, many hard-won
#     fixes) is intentionally left untouched; this script mirrors its IP
#     resolution but uses replace-if-changed semantics instead of append-once.
#
# Architecture: docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md

set -euo pipefail

MARKER=/etc/nexus/.initialized
CONFIG_DIR=/etc/nexus
CP_YAML="$CONFIG_DIR/control-plane.config.yaml"
CP_ENV="$CONFIG_DIR/control-plane.env"
DB_NAME=nexus_gateway

FORCE=false
STATUS_ONLY=false
case "${1:-}" in
  --force)  FORCE=true ;;
  --status) STATUS_ONLY=true ;;
  --help|-h)
    echo "usage: nexus-reconcile-url [--force|--status]"
    echo "  (no args)  reconcile IP-derived config to the current public IP if it changed"
    echo "  --force    re-stamp even if the baked IP already matches"
    echo "  --status   print baked vs current IP and exit (no changes)"
    exit 0 ;;
esac

log() { echo "[nexus-reconcile-url] $*"; }

# Nothing to reconcile until first-boot has stamped the initial config.
if [ ! -f "$MARKER" ]; then
  log "first-boot not complete (no $MARKER); nothing to reconcile."
  exit 0
fi

# ─── Resolve the instance's CURRENT reachable IP (mirror of first-boot.sh) ───
# EC2 IMDSv2 public-ipv4 first, local-ipv4 fallback for private-subnet
# deployments, hostname -I for non-EC2.
NEW=""
TOKEN=$(curl -fsS -X PUT "http://169.254.169.254/latest/api/token" \
  -H "X-aws-ec2-metadata-token-ttl-seconds: 21600" -m 3 2>/dev/null || true)
if [ -n "$TOKEN" ]; then
  NEW=$(curl -fsS -H "X-aws-ec2-metadata-token: $TOKEN" \
    http://169.254.169.254/latest/meta-data/public-ipv4 -m 3 2>/dev/null || true)
  [ -z "$NEW" ] && NEW=$(curl -fsS -H "X-aws-ec2-metadata-token: $TOKEN" \
    http://169.254.169.254/latest/meta-data/local-ipv4 -m 3 2>/dev/null || true)
fi
[ -z "$NEW" ] && NEW=$(hostname -I 2>/dev/null | awk '{print $1}')

# Inner (private) IP. It belongs in the TLS SAN too, so callers that reach the
# box over the private address still pass cert verification. The private IP is
# normally stable across stop/start, but a new ENI / subnet move can change it,
# so we reconcile the cert against it independently of the public IP.
LOCAL_IP=""
[ -n "$TOKEN" ] && LOCAL_IP=$(curl -fsS -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/meta-data/local-ipv4 -m 3 2>/dev/null || true)
[ -z "$LOCAL_IP" ] && LOCAL_IP=$(hostname -I 2>/dev/null | awk '{print $1}')

# Current baked IP, read from the control-plane publicURL.
OLD=$(grep -hoE '^publicURL: "https?://[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' "$CP_YAML" 2>/dev/null \
        | grep -oE '[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' || true)

# Desired TLS SAN IP set (deduped, non-empty): public, inner, loopback.
DESIRED_IPS="127.0.0.1"
[ -n "$NEW" ] && DESIRED_IPS="$NEW $DESIRED_IPS"
[ -n "$LOCAL_IP" ] && [ "$LOCAL_IP" != "$NEW" ] && DESIRED_IPS="$DESIRED_IPS $LOCAL_IP"

# Cert coverage: does the live cert SAN already contain every desired IP? A
# missing entry (changed public OR inner IP, or any drift) means the cert is
# stale and must be re-issued — independently of the publicURL delta.
CERT_STALE=false
CERT_SAN=$(openssl x509 -in "$CONFIG_DIR/tls.crt" -noout -ext subjectAltName 2>/dev/null || true)
for ip in $DESIRED_IPS; do
  echo "$CERT_SAN" | grep -qE "IP Address:${ip}([,[:space:]]|\$)" || CERT_STALE=true
done

log "public IP = ${NEW:-<unresolved>} ; inner IP = ${LOCAL_IP:-<none>} ; baked publicURL IP = ${OLD:-<none>} ; cert covers all = $([ "$CERT_STALE" = true ] && echo no || echo yes)"
if [ "$STATUS_ONLY" = true ]; then
  exit 0
fi

# Fail-safe guards: never act on an unresolved or loopback-only address when a
# real address is already in place.
if [ -z "$NEW" ]; then
  log "could not resolve current IP; leaving config unchanged."
  exit 0
fi
if [ "$NEW" = "127.0.0.1" ] && [ -n "$OLD" ] && [ "$OLD" != "127.0.0.1" ]; then
  log "resolved only loopback while '$OLD' is baked; refusing to downgrade."
  exit 0
fi

# Two independent change signals: the public-facing config (publicURL / issuer /
# OAuth redirect) keys on the PUBLIC IP only; the TLS cert keys on SAN coverage
# (public OR inner IP).
PUBLIC_CHANGED=false
[ "$OLD" != "$NEW" ] && PUBLIC_CHANGED=true   # includes the OLD="" (never stamped) case

if [ "$PUBLIC_CHANGED" = false ] && [ "$CERT_STALE" = false ] && [ "$FORCE" != true ]; then
  log "publicURL IP current and cert SAN covers all addresses; nothing to do."
  exit 0
fi

log "reconciling (public_changed=$PUBLIC_CHANGED cert_stale=$CERT_STALE force=$FORCE): publicURL ${OLD:-<none>} -> $NEW"

# ─── 1+2. Public-facing config (only when the PUBLIC IP changed) ──────────────
# publicURL / AUTH_SERVER_ISSUER / cp-ui redirect are public-facing only — the
# inner IP is irrelevant to them, so they are reconciled solely on a public-IP
# change (or --force).
if [ "$PUBLIC_CHANGED" = true ] || [ "$FORCE" = true ]; then
  # 1. publicURL in the four service yamls + AUTH_SERVER_ISSUER in env. Replace
  # the old IP everywhere it appears; if OLD is unknown, stamp canonical shapes.
  if [ -n "$OLD" ]; then
    for f in "$CONFIG_DIR/nexus-hub.config.yaml" "$CP_YAML" \
             "$CONFIG_DIR/ai-gateway.config.yaml" "$CONFIG_DIR/compliance-proxy.config.yaml" \
             "$CP_ENV"; do
      [ -f "$f" ] && sed -i "s/${OLD}/${NEW}/g" "$f"
    done
  else
    sed -i "s#^publicURL:.*#publicURL: \"http://${NEW}:3060\"#"  "$CONFIG_DIR/nexus-hub.config.yaml" 2>/dev/null || true
    sed -i "s#^publicURL:.*#publicURL: \"https://${NEW}/\"#"     "$CP_YAML" 2>/dev/null || true
    sed -i "s#^publicURL:.*#publicURL: \"https://${NEW}/v1\"#"   "$CONFIG_DIR/ai-gateway.config.yaml" 2>/dev/null || true
    sed -i "s#^publicURL:.*#publicURL: \"http://${NEW}:3128\"#"  "$CONFIG_DIR/compliance-proxy.config.yaml" 2>/dev/null || true
    if grep -q '^AUTH_SERVER_ISSUER=' "$CP_ENV" 2>/dev/null; then
      sed -i "s#^AUTH_SERVER_ISSUER=.*#AUTH_SERVER_ISSUER=https://${NEW}/#" "$CP_ENV"
    else
      echo "AUTH_SERVER_ISSUER=https://${NEW}/" >> "$CP_ENV"
    fi
  fi
  log "updated publicURL + AUTH_SERVER_ISSUER"

  # 2. cp-ui OAuthClient.redirectUris. Swap the stale per-instance redirect for
  # the current one, then guarantee the current one is present. Wait briefly for
  # Postgres readiness so an early-boot run does not silently skip the update.
  for _ in $(seq 1 30); do
    sudo -u postgres pg_isready -q 2>/dev/null && break
    sleep 1
  done
  NEW_URI="https://${NEW}/auth/callback"
  OLD_URI="https://${OLD}/auth/callback"
  sudo -u postgres psql -d "$DB_NAME" -v ON_ERROR_STOP=1 >/dev/null <<SQL
UPDATE "OAuthClient"
SET "redirectUris" = array_replace("redirectUris", '${OLD_URI}', '${NEW_URI}'),
    "updatedAt" = NOW()
WHERE "id" = 'cp-ui' AND '${OLD_URI}' = ANY("redirectUris");
UPDATE "OAuthClient"
SET "redirectUris" = array_append("redirectUris", '${NEW_URI}'),
    "updatedAt" = NOW()
WHERE "id" = 'cp-ui' AND NOT ('${NEW_URI}' = ANY("redirectUris"));
SQL
  log "registered cp-ui redirect_uri $NEW_URI"
fi

# ─── 3. Regenerate the nginx TLS cert (when its SAN no longer covers a current IP) ─
# The control-plane's JWT verifier fetches JWKS over the publicURL with Go's
# default (cert-verifying) HTTP client. If the cert SAN does not name the IP the
# request targets, that fetch fails with `x509: certificate is valid for <old>,
# not <new>`, the token cannot be verified, /api/admin/me returns 401, and the
# SPA bounces back to /login after a seemingly-successful password submit. The
# SAN must therefore track BOTH the public and the inner IP — so cert regen is
# driven by SAN coverage (CERT_STALE), independent of the publicURL delta, and
# catches an inner-IP change even when the public IP is unchanged. Mirrors
# first-boot-ca.sh's cert shape; the per-instance MITM CA
# (/etc/compliance-proxy/ca.crt) is CN-based and intentionally left alone
# (re-issuing it would break every enrolled agent's trust store).
if [ "$CERT_STALE" = true ] || [ "$PUBLIC_CHANGED" = true ] || [ "$FORCE" = true ]; then
  SAN="IP:127.0.0.1,DNS:nexus-gateway,DNS:localhost"
  [ -n "$NEW" ] && SAN="IP:${NEW},${SAN}"
  [ -n "$LOCAL_IP" ] && [ "$LOCAL_IP" != "$NEW" ] && SAN="${SAN},IP:${LOCAL_IP}"
  log "regenerating nginx cert with SAN: ${SAN}"
  openssl req -x509 -nodes -newkey rsa:2048 -days 365 \
    -subj "/CN=nexus-gateway/O=Nexus Gateway" \
    -addext "subjectAltName=${SAN}" \
    -keyout "$CONFIG_DIR/tls.key" \
    -out    "$CONFIG_DIR/tls.crt" 2>/dev/null
  chmod 0640 "$CONFIG_DIR/tls.crt" "$CONFIG_DIR/tls.key"
  chown root:nexus "$CONFIG_DIR/tls.crt" "$CONFIG_DIR/tls.key"
  # Re-anchor in the system CA trust store so the Go JWKS client trusts it.
  install -o root -g root -m 0644 "$CONFIG_DIR/tls.crt" \
    /etc/pki/ca-trust/source/anchors/nexus-gateway.crt
  update-ca-trust
fi

# ─── 4. Reload nginx + restart services that read publicURL / verify tokens ───
log "reloading nginx + restarting nexus services..."
systemctl reload nginx || systemctl restart nginx
systemctl restart nexus-hub nexus-control-plane nexus-gateway nexus-proxy

log "reconcile complete: appliance now advertises https://${NEW}/"
