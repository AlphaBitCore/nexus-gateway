#!/usr/bin/env bash
# Every outbound *http.Client must come from packages/httpclient.New.
#
# The check itself is an AST test beside the factory it guards
# (packages/httpclient/bare_client_ast_test.go); this wrapper exists
# so `check:all` and CI reach it. Without it the rule ran only when someone
# happened to test packages/httpclient — and CI's go-test job is scoped to the
# modules a change touches, so a bare client added in ai-gateway or
# control-plane would have merged green. That is the same "the gate cannot
# reach the change" shape the rule is meant to prevent.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

if ! command -v go >/dev/null 2>&1; then
  # exit 3 is this repo's "did NOT run" contract: a gate that could not run must
  # not report the same success as one that ran and passed.
  echo "  [bare-http-client] go not installed — gate DID NOT RUN"
  exit 3
fi

out="$(cd packages/httpclient && go test ./ \
        -run TestNoBareHTTPClientConstruction -count=1 2>&1)"
rc=$?
if [ $rc -ne 0 ]; then
  printf '%s\n' "$out" | sed 's/^/  /'
  exit 1
fi
echo "[bare-http-client] OK — no bare http.Client construction outside the allowlist."
