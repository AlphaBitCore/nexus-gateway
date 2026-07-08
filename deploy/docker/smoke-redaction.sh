#!/usr/bin/env bash
# smoke-redaction.sh — self-contained container smoke for docker-compose.full.yml.
#
# Verifies, WITHOUT any provider credential or external network, the things the
# container packaging is responsible for:
#   1. db-init completes (schema push + prod seed).
#   2. All four service containers report healthy.
#   3. The Vectorscan content-scanning engine actually works INSIDE the shipped
#      ai-gateway + compliance-proxy runtime images (hs-selfcheck: hs_compile +
#      hs_alloc_scratch + hs_scan → scanRC=0, matches>=1). This is the check
#      that catches a FAT_RUNTIME=ON libhs that links but silently never scans.
#   4. The ai-gateway authenticates the seeded virtual key and runs the request
#      pipeline (a request reaches the upstream-forward stage rather than
#      bouncing at auth).
#
# What it deliberately does NOT do: enable a compliance hook and assert an
# actual redaction end-to-end. Hook enablement flows Admin-UI → Control Plane →
# Hub shadow → gateway (config-sync), and a real redaction path needs a
# reachable upstream provider. That full E2E belongs to tests/scripts/
# smoke-gateway.py against a credentialled deployment, not to this
# credential-free packaging gate.
#
# Usage:  [NEXUS_IMAGE_TAG=ci] deploy/docker/smoke-redaction.sh
# Leaves the stack DOWN on exit; set SMOKE_KEEP_UP=1 to keep it.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"
ENV_FILE=deploy/docker/.env.compose
COMPOSE=(docker compose -f docker-compose.full.yml --env-file "$ENV_FILE")

[ -f "$ENV_FILE" ] || ./scripts/compose-init.sh
export NEXUS_IMAGE_TAG="${NEXUS_IMAGE_TAG:-latest}"
REGISTRY="${NEXUS_IMAGE_REGISTRY:-alphabitcore}"

cleanup() {
  if [ "${SMOKE_KEEP_UP:-0}" != "1" ]; then
    "${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

fail() { echo "[smoke] FAIL: $*" >&2; "${COMPOSE[@]}" ps; exit 1; }

# ── Gate: Vectorscan engine works inside the shipped runtime images ──────────
echo "[smoke] verifying Vectorscan engine in the runtime images..."
for svc in ai-gateway compliance-proxy; do
  out=$(docker run --rm --entrypoint hs-selfcheck "$REGISTRY/nexus-$svc:$NEXUS_IMAGE_TAG" 2>&1) \
    || fail "hs-selfcheck exited non-zero for $svc: $out"
  echo "$out" | grep -q 'scanRC=0' && echo "$out" | grep -qE 'matches=[1-9]' \
    || fail "hs-selfcheck did not confirm a working scan for $svc: $out"
  echo "[smoke]   $svc: $out"
done

# ── Bring the stack up ───────────────────────────────────────────────────────
echo "[smoke] bringing the stack up (tag: $NEXUS_IMAGE_TAG)..."
"${COMPOSE[@]}" up -d

echo "[smoke] waiting for services to report healthy..."
for i in $(seq 1 120); do
  unhealthy=0
  for c in nexus-full-hub nexus-full-ai-gateway nexus-full-compliance-proxy nexus-full-console; do
    st=$(docker inspect "$c" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' 2>/dev/null || echo missing)
    [ "$st" = healthy ] || unhealthy=1
  done
  [ "$unhealthy" -eq 0 ] && break
  [ "$i" -eq 120 ] && { "${COMPOSE[@]}" logs --tail 30; fail "services not all healthy after 10m"; }
  sleep 5
done
echo "[smoke] all four services healthy."

# ── Gate: db-init seeded successfully ────────────────────────────────────────
[ "$(docker inspect nexus-full-db-init --format '{{.State.ExitCode}}')" = 0 ] \
  || fail "db-init did not exit 0"
echo "[smoke] db-init completed (schema + seed)."

# ── Gate: gateway authenticates the seeded VK and runs the pipeline ──────────
# The bootstrap seed mints a system-assistant VK; read its plaintext from the
# db-init log (local default). A request with it must pass auth and reach the
# pipeline — proven by anything other than 401/403 (a 502 "no upstream" is the
# expected result with no provider credential configured).
VK=$(docker logs nexus-full-db-init 2>&1 | sed -n 's/.*VK plaintext (local default): \(nvk_[A-Za-z0-9_]*\).*/\1/p' | head -1)
[ -n "$VK" ] || fail "could not read seeded VK from db-init log"
code=$(curl -sk -o /dev/null -w '%{http_code}' -X POST http://localhost:3050/v1/chat/completions \
  -H "Authorization: Bearer $VK" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"ping"}]}')
case "$code" in
  401|403) fail "gateway rejected the seeded VK (HTTP $code) — auth/pipeline broken" ;;
  *) echo "[smoke] gateway authenticated VK and ran the pipeline (HTTP $code; 502 = no upstream configured, expected)." ;;
esac

echo "[smoke] all gates green."
