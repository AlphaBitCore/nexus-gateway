#!/usr/bin/env bash
# Fail if the pure-forward benchmark switch is activated in any tracked file.
# NEXUS_PERF_PURE_FORWARD=1 disables the audit trail and must never ship — it is
# a benchmark-only, host-set env var. Commented references (e.g. in .env.example)
# are allowed; only an ACTIVE assignment is a violation.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# Match active assignments, ignore comment lines (leading # after optional space).
# No "\b" — macOS git grep -E (POSIX ERE) does not support it and silently matches
# nothing. The trailing "[:=]" anchors this to a real assignment, not prose.
hits="$(git grep -nE '^[^#]*NEXUS_PERF_PURE_FORWARD[[:space:]]*[:=][[:space:]]*(1|true|"1"|"true")' -- . ':!scripts/check-no-benchmark-env.sh' || true)"

if [[ -n "$hits" ]]; then
  echo "ERROR: NEXUS_PERF_PURE_FORWARD is activated in a committed file (audit-disabling benchmark switch must never ship):" >&2
  echo "$hits" >&2
  exit 1
fi
echo "OK: no committed file activates NEXUS_PERF_PURE_FORWARD"
