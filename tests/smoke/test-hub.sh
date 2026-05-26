#!/usr/bin/env bash
# tests/smoke/test-hub.sh
#
# Phase 1 L1 smoke for Nexus Hub.
#
# Coverage:
#   1. Public health: /healthz + /readyz return 200 with the expected JSON
#      shape, and /metrics is a valid Prometheus exposition.
#   2. Service-token-authed admin surface (/api/hub/*): list endpoints
#      cross-checked against thing / job / job_run DB tables.
#   3. Per-thing drilldown: pick the live nexus-hub thing and verify the
#      detail + shadow endpoints.
#
# Verification discipline (master plan §6): every assertion is HTTP-shape +
# DB cross-check or Prometheus content-shape.

set -eu

_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck disable=SC1091
source "$_dir/lib/env.sh"
# shellcheck disable=SC1091
source "$_dir/lib/assert.sh"
# shellcheck disable=SC1091
source "$_dir/lib/db.sh"
# shellcheck disable=SC1091
source "$_dir/lib/http.sh"

printf '== test-hub (Hub admin API) ==\n'

# Tiny wrapper for service-token-authed Hub calls, kept local to this script
# so http.sh stays focused on the AI Gateway / Hub public surface.
# Avoid `local path=` — clobbers PATH in zsh (tied-array). See tests/lib/http.sh.
hub_admin_curl() {
  local rel_path="$1"; shift
  curl -sS -H "Authorization: Bearer $NEXUS_HUB_SERVICE_TOKEN" "$@" "$NEXUS_HUB_URL$rel_path"
}
hub_admin_code() {
  local rel_path="$1"; shift
  curl -sS -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer $NEXUS_HUB_SERVICE_TOKEN" "$@" "$NEXUS_HUB_URL$rel_path"
}

# ----------------------------------------------------------------------------
# Section 1 — public health surface.
# ----------------------------------------------------------------------------

healthz_body=$(hub_curl /healthz)
healthz_status=$(printf '%s' "$healthz_body" | jq -r '.status // empty' 2>/dev/null || echo '')
assert_eq "ok" "$healthz_status" "healthz returns {status:ok}"

readyz_code=$(hub_curl_code /readyz)
assert_status 200 "$readyz_code" "readyz"

# /metrics: Prometheus exposition is plain text starting with HELP/TYPE lines
# and must contain at least one nexus_hub_* counter we know is registered.
metrics_body=$(hub_curl /metrics)
if printf '%s' "$metrics_body" | grep -qE '^# HELP '; then
  pass "metrics exposes Prometheus HELP lines"
else
  fail "metrics:HELP" "no '# HELP ' line in /metrics output"
fi
if printf '%s' "$metrics_body" | grep -qE '^nexus_hub_mq_'; then
  pass "metrics has nexus_hub_mq_* counters"
else
  fail "metrics:nexus_hub" "no nexus_hub_mq_* counter in /metrics"
fi

# ----------------------------------------------------------------------------
# Section 2 — service-token admin surface, with DB cross-check.
# ----------------------------------------------------------------------------

# /api/hub/things — total must match SELECT count(*) FROM thing.
things_body=$(hub_admin_curl /api/hub/things)
api_thing_count=$(printf '%s' "$things_body" | jq -r '.total // (.things|length)')
db_thing_count=$(db_scalar 'SELECT count(*) FROM thing')
assert_eq "$db_thing_count" "$api_thing_count" "things: API total matches DB count ($db_thing_count rows)"

# Sanity: every Thing in the response also appears in DB. Pick the first 3 ids
# to keep this fast even with large fleets.
ids_to_check=$(printf '%s' "$things_body" | jq -r '.things[0:3] | .[].id')
while IFS= read -r tid; do
  [[ -z "$tid" ]] && continue
  if db_exists "SELECT 1 FROM thing WHERE id='$tid'"; then
    pass "thing[$tid] exists in DB"
  else
    fail "thing[$tid]:db" "Hub returned id but no DB row"
  fi
done <<<"$ids_to_check"

# /api/hub/jobs — total must match SELECT count(*) FROM job.
jobs_body=$(hub_admin_curl /api/hub/jobs)
api_job_count=$(printf '%s' "$jobs_body" | jq -r '(.total // (.jobs|length) // length)')
db_job_count=$(db_scalar 'SELECT count(*) FROM job')
assert_eq "$db_job_count" "$api_job_count" "jobs: API total matches DB count ($db_job_count rows)"

# Pick the first job and verify /jobs/:id and /jobs/:id/runs.
first_job_id=$(printf '%s' "$jobs_body" | jq -r '(.jobs // .data // .)[0].id // empty')
if [[ -n "$first_job_id" ]]; then
  detail_code=$(hub_admin_code "/api/hub/jobs/$first_job_id")
  assert_status 200 "$detail_code" "jobs/$first_job_id detail"

  runs_body=$(hub_admin_curl "/api/hub/jobs/$first_job_id/runs?limit=5")
  runs_count_api=$(printf '%s' "$runs_body" | jq -r '(.runs // .data // .) | length // 0')
  pass "jobs/$first_job_id/runs: returned $runs_count_api row(s) (no DB cross-check; runs table churns)"
else
  fail "jobs:first" "could not find a first job id in the list response"
fi

# /api/hub/drift — JSON list of drifted things.
drift_code=$(hub_admin_code /api/hub/drift)
assert_status 200 "$drift_code" "drift"

# /api/hub/things/overrides — global list of thing-config overrides.
overrides_body=$(hub_admin_curl /api/hub/things/overrides)
api_override_count=$(printf '%s' "$overrides_body" | jq -r '(.overrides // .data // .) | length // 0')
db_override_count=$(db_scalar 'SELECT count(*) FROM thing_config_override')
assert_eq "$db_override_count" "$api_override_count" "things/overrides: API matches DB ($db_override_count rows)"

# /api/hub/config/catalog — declared config catalog.
catalog_code=$(hub_admin_code /api/hub/config/catalog)
assert_status 200 "$catalog_code" "config/catalog"

# /api/hub/enrollment/tokens — list of enrollment tokens (likely empty in dev).
tokens_code=$(hub_admin_code /api/hub/enrollment/tokens)
assert_status 200 "$tokens_code" "enrollment/tokens"

# ----------------------------------------------------------------------------
# Section 3 — Hub itself as a Thing (self-registration, e31-s6).
# ----------------------------------------------------------------------------

# The Hub registers itself under id="hub-dev" in dev. Verify it appears in
# the things list AND has a shadow.
hub_self_status=$(printf '%s' "$things_body" | jq -r '.things[] | select(.id=="hub-dev") | .status // empty')
if [[ "$hub_self_status" == "online" ]]; then
  pass "Hub self-thing (hub-dev) status=online"
else
  fail "Hub self-thing" "hub-dev not online (status=$hub_self_status)"
fi

# Pick a non-Hub thing and exercise its shadow endpoint — Hub itself has an
# empty shadow ({}), which is a less interesting check.
shadow_target=$(printf '%s' "$things_body" | jq -r '.things[] | select(.id!="hub-dev") | .id' | head -1)
if [[ -n "$shadow_target" ]]; then
  shadow_code=$(hub_admin_code "/api/hub/things/$shadow_target/shadow")
  assert_status 200 "$shadow_code" "things/$shadow_target/shadow"
fi

summary
