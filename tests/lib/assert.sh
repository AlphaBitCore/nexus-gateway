#!/usr/bin/env bash
# tests/lib/assert.sh — pass/fail tracking with deferred summary.
#
# Source this file from a test script. It defines:
#   pass <name>            — record a success and print ✓
#   fail <name> <reason>   — record a failure and print ✗ (does not exit)
#   die  <name> <reason>   — record a failure and exit 1 immediately
#   summary                — print summary table; exit 0 if all green, 1 otherwise
#
# State lives in shell-globals so a single test script can run many assertions.

set -u

NEXUS_TEST_PASS_COUNT=${NEXUS_TEST_PASS_COUNT:-0}
NEXUS_TEST_FAIL_COUNT=${NEXUS_TEST_FAIL_COUNT:-0}
NEXUS_TEST_FAIL_LIST=${NEXUS_TEST_FAIL_LIST:-}

# ANSI colors only when stdout is a TTY; otherwise plain text so the report is
# readable when redirected to a file.
if [[ -t 1 ]]; then
  _C_GREEN=$'\033[32m'
  _C_RED=$'\033[31m'
  _C_DIM=$'\033[2m'
  _C_RESET=$'\033[0m'
else
  _C_GREEN=""; _C_RED=""; _C_DIM=""; _C_RESET=""
fi

pass() {
  local name="$1"
  NEXUS_TEST_PASS_COUNT=$((NEXUS_TEST_PASS_COUNT + 1))
  printf '%s✓%s %s\n' "$_C_GREEN" "$_C_RESET" "$name"
}

fail() {
  local name="$1"
  local reason="${2:-}"
  NEXUS_TEST_FAIL_COUNT=$((NEXUS_TEST_FAIL_COUNT + 1))
  NEXUS_TEST_FAIL_LIST+="${name}|${reason}"$'\n'
  printf '%s✗%s %s\n' "$_C_RED" "$_C_RESET" "$name"
  if [[ -n "$reason" ]]; then
    printf '  %s%s%s\n' "$_C_DIM" "$reason" "$_C_RESET"
  fi
}

die() {
  fail "$1" "${2:-}"
  summary
  exit 1
}

summary() {
  local total=$((NEXUS_TEST_PASS_COUNT + NEXUS_TEST_FAIL_COUNT))
  printf '\n--- Summary ---\n'
  printf 'Total: %d  Pass: %s%d%s  Fail: %s%d%s\n' \
    "$total" \
    "$_C_GREEN" "$NEXUS_TEST_PASS_COUNT" "$_C_RESET" \
    "$_C_RED"   "$NEXUS_TEST_FAIL_COUNT" "$_C_RESET"
  if [[ "$NEXUS_TEST_FAIL_COUNT" -gt 0 ]]; then
    printf '\nFailures:\n'
    while IFS='|' read -r name reason; do
      [[ -z "$name" ]] && continue
      printf '  - %s\n' "$name"
      [[ -n "$reason" ]] && printf '    %s\n' "$reason"
    done <<<"$NEXUS_TEST_FAIL_LIST"
    return 1
  fi
  return 0
}

# assert_eq <expected> <actual> <name>
# Records a pass if string-equal, otherwise a fail with both values.
assert_eq() {
  local expected="$1"
  local actual="$2"
  local name="$3"
  if [[ "$expected" == "$actual" ]]; then
    pass "$name"
  else
    fail "$name" "expected=[$expected] got=[$actual]"
  fi
}

# assert_contains <haystack> <needle> <name>
assert_contains() {
  local haystack="$1"
  local needle="$2"
  local name="$3"
  if [[ "$haystack" == *"$needle"* ]]; then
    pass "$name"
  else
    fail "$name" "needle=[$needle] not found in haystack (truncated): [${haystack:0:200}]"
  fi
}

# assert_status <expected_code> <actual_code> <name>
# Convenience wrapper for HTTP status assertions.
assert_status() {
  local expected="$1"
  local actual="$2"
  local name="$3"
  if [[ "$expected" == "$actual" ]]; then
    pass "$name (HTTP $actual)"
  else
    fail "$name" "expected HTTP $expected, got HTTP $actual"
  fi
}
