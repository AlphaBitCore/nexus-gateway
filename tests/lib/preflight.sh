#!/usr/bin/env bash
# tests/lib/preflight.sh — verify every dependency before a test run.
#
# Exit 0 if everything is reachable, exit 1 with a diagnostic if not.

set -eu

_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$_dir/env.sh"
# shellcheck disable=SC1091
source "$_dir/assert.sh"
# shellcheck disable=SC1091
source "$_dir/db.sh"
# shellcheck disable=SC1091
source "$_dir/http.sh"
# shellcheck disable=SC1091
source "$_dir/auth.sh"
# shellcheck disable=SC1091
source "$_dir/preflight_checks.sh"

printf '== Preflight ==\n'

# 1. Postgres reachable + has a Provider table (proxy for migrations applied).
if db_health; then
  pass "postgres:reachable"
else
  die "postgres:reachable" "container=$NEXUS_PG_CONTAINER not responding to pg_isready"
fi
if db_exists "SELECT 1 FROM \"Provider\" LIMIT 1"; then
  pass "postgres:Provider-seeded"
else
  fail "postgres:Provider-seeded" "no rows in Provider — run: cd tools/db-migrate && npx prisma db seed"
fi

# 2. Hub /healthz (note: Echo registers /healthz, not /health).
hub_status=$(hub_curl_code /healthz || echo "000")
assert_status 200 "$hub_status" "hub:/healthz"

# 3. Control Plane real OAuth login + token round-trip on a real admin endpoint.
# This drives /oauth/authorize → /authserver/password → /oauth/token end-to-end,
# so a regression in any of those endpoints surfaces here.
rm -f "$NEXUS_TOKEN_CACHE"  # force a fresh login for preflight
if cp_login; then
  pass "control-plane:OAuth login (admin@${NEXUS_ADMIN_EMAIL#*@})"
else
  die "control-plane:OAuth login" "OAuth flow failed — see message above"
fi
if cp_login_check; then
  pass "control-plane:bearer token works (GET /api/admin/providers)"
else
  fail "control-plane:bearer token" "GET /api/admin/providers did not return 200 with the issued token"
fi

# 4. Hub admin API. /api/hub is guarded by HUB_CONFIG_TOKEN — see the
# Group("/api/hub", ServiceAuth(cfg.HubConfigToken)) registration in
# packages/nexus-hub/internal/handler/routes.go — NOT by the internal service
# token, which guards /api/internal/*. A local .env sets both to the same dev
# string, so sending the wrong one passes locally and can only fail against a
# real deployment: precisely the release gate this preflight stands in front of.
#
# NEXUS_HUB_SERVICE_TOKEN is the local spelling; accept it too, the way
# smoke-gateway.py accepts either name. Under `set -u` an unset name is a hard
# abort, so a deployment that simply spells it differently took the whole
# preflight down after four checks had already passed.
hub_token="${HUB_CONFIG_TOKEN:-${NEXUS_HUB_SERVICE_TOKEN:-}}"
if [[ -n "$hub_token" ]]; then
  hub_admin_status=$(curl -sS -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer $hub_token" "$NEXUS_HUB_URL/api/hub/things")
  assert_status 200 "$hub_admin_status" "hub:/api/hub/things (service token)"
else
  printf '  (skipping hub service-token check: neither NEXUS_HUB_SERVICE_TOKEN nor INTERNAL_SERVICE_TOKEN is set)\n'
fi

# 4. AI Gateway /v1/models with a real VK (only if NEXUS_TEST_VK is set —
#    Phase 1 doesn't need it, Phase 4/5 do).
if [[ -n "${NEXUS_TEST_VK:-}" && "$(printf %s "${NEXUS_TEST_VK:-}" | tr a-z A-Z)" != *REPLACE* ]]; then
  aigw_status=$(aigw_curl_code "$NEXUS_TEST_VK" /v1/models)
  assert_status 200 "$aigw_status" "ai-gateway:/v1/models (VK auth)"
else
  printf '  (skipping ai-gateway VK check: NEXUS_TEST_VK unset or still a placeholder)\n'
fi

# 5. Compliance Proxy listening. It answers 400/404/etc. on root, so any HTTP
# code means it is up; only a failed connection means it is not.
#
# Unreachable is a FAIL locally and a SKIP anywhere else, and that asymmetry is
# deliberate: the proxy is a TLS CONNECT intercept point reached directly by
# org-managed devices, never published through nginx. A correct remote
# deployment is therefore unreachable from wherever this runs, and hard-failing
# on it would block every remote run — including the release gate.
proxy_state=$(proxy_verdict)
if [[ "$proxy_state" != "unreachable" ]]; then
  pass "compliance-proxy:listening (HTTP ${proxy_state#listening:})"
elif [[ "$NEXUS_TEST_TARGET" == "local" ]]; then
  fail "compliance-proxy:listening" "no TCP connection to $NEXUS_PROXY_URL"
else
  printf '  (skipping compliance-proxy check: target=%s, the proxy is not published through nginx)\n' "$NEXUS_TEST_TARGET"
fi

summary
