#!/usr/bin/env bash
# tests/smoke/test-control-plane.sh
#
# Phase 1 L1 smoke for the Control Plane admin API.
#
# Coverage strategy:
#   1. List endpoints — hit each list endpoint, assert HTTP 200 + JSON parses,
#      and where the resource has a clean DB table backing, cross-check that
#      the API's `total` matches `SELECT count(*) FROM "<table>"`. This pins
#      the DB → API path: a regression that drops rows on the way through
#      the admin handler shows up immediately.
#   2. CRUD round-trip — POST → GET /:id → PUT → DELETE on Organization,
#      with DB verification at every step. Organization is the simplest
#      tenant resource (no FK fan-out), so it's safe to mutate in a smoke run.
#
# Verification discipline (master plan §6): every assertion is HTTP-shape +
# DB cross-check. No "did the binary respond" tests.

set -eu

_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck disable=SC1091
source "$_dir/lib/env.sh"
# shellcheck disable=SC1091
source "$_dir/lib/assert.sh"
# shellcheck disable=SC1091
source "$_dir/lib/db.sh"
# shellcheck disable=SC1091
source "$_dir/lib/auth.sh"

printf '== test-control-plane (admin API) ==\n'

# Force a fresh login so a regression in /authserver/password fails this
# script directly rather than silently relying on a stale cached token.
rm -f "$NEXUS_TOKEN_CACHE"
if ! cp_login >/dev/null; then
  die "cp_login" "OAuth flow failed — see message above"
fi
pass "cp_login (admin@${NEXUS_ADMIN_EMAIL#*@}) issued bearer token"

# ----------------------------------------------------------------------------
# Section 1 — list endpoints with DB cross-check.
#
# Schema notes:
#   - Most endpoints return {data: [...], total: N}
#   - rule-packs returns a plain array (length is the count)
#   - alerts/rules returns {rules: [...]}, alerts/channels {channels: [...]}
#   - organizations returns {data: [...]} with no `total` (counted locally)
# ----------------------------------------------------------------------------

# NOTE: avoid `local path=` — `path` is a tied-array linked to $PATH in zsh
# and overwrites it inside the function. Use rel_path instead.

# assert_list_total <path> <table> <name>
# Compares API .total to DB count(*) on a quoted table name.
assert_list_total() {
  local rel_path="$1" table="$2" name="$3"
  local body api_total db_total
  body=$(cp_curl "$rel_path")
  api_total=$(printf '%s' "$body" | jq -r '.total // (.data|length) // length')
  db_total=$(db_scalar "SELECT count(*) FROM $table")
  if [[ "$api_total" == "$db_total" ]]; then
    pass "$name: API total=$api_total matches DB"
  else
    fail "$name" "API total=$api_total but DB count=$db_total"
  fi
}

# assert_list_field_count <path> <jq_field> <table> <name>
# Like assert_list_total but pulls the array under a custom jq path
# (e.g. .rules, .channels) and compares its length to DB.
assert_list_field_count() {
  local rel_path="$1" field="$2" table="$3" name="$4"
  local body api_count db_total
  body=$(cp_curl "$rel_path")
  api_count=$(printf '%s' "$body" | jq -r "$field | length")
  db_total=$(db_scalar "SELECT count(*) FROM $table")
  if [[ "$api_count" == "$db_total" ]]; then
    pass "$name: list size=$api_count matches DB"
  else
    fail "$name" "list size=$api_count but DB count=$db_total"
  fi
}

# assert_list_status <path> <name>
# Endpoints with no clean DB-backed count get a softer check: 200 + JSON parses.
assert_list_status() {
  local rel_path="$1" name="$2"
  local body code
  body=$(cp_curl "$rel_path")
  code=$(cp_curl_code "$rel_path")
  if [[ "$code" != "200" ]]; then
    fail "$name" "HTTP $code"
    return
  fi
  if printf '%s' "$body" | jq -e . >/dev/null 2>&1; then
    pass "$name: 200 + JSON"
  else
    fail "$name" "200 but body is not valid JSON: ${body:0:120}"
  fi
}

# Catalog & vault.
assert_list_total /api/admin/providers       '"Provider"'    "providers"
assert_list_total /api/admin/credentials     '"Credential"'  "credentials"
assert_list_total /api/admin/virtual-keys    '"VirtualKey"'  "virtual-keys"

# Routing & hooks.
assert_list_total /api/admin/routing-rules   '"RoutingRule"' "routing-rules"
assert_list_total /api/admin/hooks           '"HookConfig"'  "hooks"
assert_list_field_count /api/admin/rule-packs '.' 'rule_pack'  "rule-packs"

# Quotas.
assert_list_total /api/admin/quota-policies  '"QuotaPolicy"'  "quota-policies"
assert_list_total /api/admin/quota-overrides '"QuotaOverride"' "quota-overrides"

# Tenancy.
# organizations: response has {data: [...]} but no .total — count locally.
assert_list_field_count /api/admin/organizations '.data' '"Organization"' "organizations"
assert_list_total       /api/admin/projects      '"Project"'     "projects"
assert_list_total       /api/admin/users         '"NexusUser"'   "users"

# Alerts.
assert_list_field_count /api/admin/alerts/rules    '.rules'    '"AlertRule"'    "alerts/rules"
assert_list_field_count /api/admin/alerts/channels '.channels' '"AlertChannel"' "alerts/channels"

# Compliance.
assert_list_status /api/admin/compliance/exemption-grants "compliance/exemption-grants"

# Fleet.
assert_list_total /api/admin/device-groups '"DeviceGroup"' "device-groups"

# Audit / analytics — count cross-check is brittle here (rows churn quickly),
# so we only assert 200 + JSON shape.
assert_list_status /api/admin/traffic            "traffic (live audit)"
assert_list_status /api/admin/admin-audit-logs   "admin-audit-logs"
assert_list_status /api/admin/analytics/summary  "analytics/summary"
# /api/admin/proxy/audit removed in 337fbd015 (2026-05-11 "dead proxy/audit
# removal"); replaced by /api/admin/proxy/compliance/* surfaces.

# Settings.
assert_list_status /api/admin/settings           "settings"
assert_list_status /api/admin/setup-state        "setup-state"

# ----------------------------------------------------------------------------
# Section 2 — Organization full CRUD round-trip with DB verification.
#
# We pick a unique slug per run so the script is idempotent and parallel-safe.
# ----------------------------------------------------------------------------

code="SMOKE_$(date +%s)_$$"
display_name="Smoke Test Org $code"

# CREATE.
create_full=$(cp_curl_full /api/admin/organizations -X POST \
  -H 'Content-Type: application/json' \
  -d "$(jq -nc --arg code "$code" --arg name "$display_name" \
        '{name: $name, code: $code, description: "created by tests/smoke/test-control-plane.sh"}')")
create_status=$(printf '%s' "$create_full" | awk '/^---HTTP_STATUS---$/{getline; print; exit}')
create_body=$(printf '%s' "$create_full" | sed '/^---HTTP_STATUS---$/,$d')
if [[ "$create_status" != "201" && "$create_status" != "200" ]]; then
  die "org-create" "POST returned $create_status: $create_body"
fi
org_id=$(printf '%s' "$create_body" | jq -r '.id // .data.id // empty')
if [[ -z "$org_id" ]]; then
  die "org-create:id" "no id in create response: $create_body"
fi
pass "org-create: HTTP $create_status, id=$org_id"

if db_exists "SELECT 1 FROM \"Organization\" WHERE id='$org_id'"; then
  pass "org-create:db row present"
else
  fail "org-create:db" "DB has no Organization row with id=$org_id"
fi

# READ.
get_body=$(cp_curl "/api/admin/organizations/$org_id")
got_code=$(printf '%s' "$get_body" | jq -r '.code // .data.code // empty')
assert_eq "$code" "$got_code" "org-get: returned code matches"

# UPDATE.
new_desc="updated by smoke at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
update_status=$(cp_curl_code "/api/admin/organizations/$org_id" -X PUT \
  -H 'Content-Type: application/json' \
  -d "$(jq -nc --arg desc "$new_desc" '{description: $desc}')")
assert_status 200 "$update_status" "org-update"

db_desc=$(db_scalar "SELECT description FROM \"Organization\" WHERE id='$org_id'")
assert_eq "$new_desc" "$db_desc" "org-update:db description reflects PUT"

# DELETE.
delete_status=$(cp_curl_code "/api/admin/organizations/$org_id" -X DELETE)
if [[ "$delete_status" != "200" && "$delete_status" != "204" ]]; then
  fail "org-delete" "expected 200 or 204, got $delete_status"
else
  pass "org-delete: HTTP $delete_status"
fi

if db_exists "SELECT 1 FROM \"Organization\" WHERE id='$org_id'"; then
  fail "org-delete:db" "Organization row $org_id still present after DELETE"
else
  pass "org-delete:db row gone"
fi

summary
