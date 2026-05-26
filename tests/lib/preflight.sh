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

# 4. Hub admin API with service token.
hub_admin_status=$(curl -sS -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer $NEXUS_HUB_SERVICE_TOKEN" "$NEXUS_HUB_URL/api/hub/things")
assert_status 200 "$hub_admin_status" "hub:/api/hub/things (service token)"

# 4. AI Gateway /v1/models with a real VK (only if NEXUS_TEST_VK is set —
#    Phase 1 doesn't need it, Phase 4/5 do).
if [[ -n "${NEXUS_TEST_VK:-}" && "$NEXUS_TEST_VK" != "nvk_REPLACE_ME" ]]; then
  aigw_status=$(aigw_curl_code "$NEXUS_TEST_VK" /v1/models)
  assert_status 200 "$aigw_status" "ai-gateway:/v1/models (VK auth)"
else
  printf '  (skipping ai-gateway VK check: NEXUS_TEST_VK not set)\n'
fi

# 5. Compliance Proxy listening.
proxy_code=$(curl -sS -o /dev/null -w '%{http_code}' "$NEXUS_PROXY_URL/" || echo "000")
# Compliance proxy returns 400/404/etc. on root — anything other than 000
# (connection refused) means it's listening.
if [[ "$proxy_code" != "000" ]]; then
  pass "compliance-proxy:listening (HTTP $proxy_code)"
else
  fail "compliance-proxy:listening" "no TCP connection to $NEXUS_PROXY_URL"
fi

summary
