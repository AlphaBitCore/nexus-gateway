#!/usr/bin/env bash
# tests/lib/preflight_checks.sh — the preflight verdicts that are worth testing.
#
# preflight.sh reads as a script, which makes its decisions unreachable to a
# test. The ones whose failure branch is easy to strand live here as pure
# functions returning a verdict string, and tests/lib/test-preflight-checks.sh
# drives them with a stubbed curl.

# proxy_verdict — is the compliance proxy accepting connections?
# Echoes "listening:<code>" or "unreachable".
proxy_verdict() {
  local code rc
  # `|| echo "000"` is the trap: on a failed connection curl ALREADY prints 000
  # from -w and THEN exits non-zero, so the fallback appends a second one and
  # the result is 000000 — which is not equal to "000", so the unreachable
  # branch could never be taken. Read the exit status instead of guessing it
  # from the output.
  code=$(curl -sS -o /dev/null -w '%{http_code}' "$NEXUS_PROXY_URL/" 2>/dev/null)
  rc=$?
  if [[ $rc -ne 0 || "$code" == "000" || -z "$code" ]]; then
    echo "unreachable"
  else
    echo "listening:$code"
  fi
}
