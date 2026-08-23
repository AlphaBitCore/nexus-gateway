#!/usr/bin/env bash
# tests/lib/test-preflight-checks.sh — self-tests for preflight.sh's verdicts.
#
# A preflight check that cannot fail is worse than no check: it prints a green
# tick over the evidence that something is wrong. These drive the check
# functions with a stubbed curl and assert the verdict, not the wording.
#
# Run: bash tests/lib/test-preflight-checks.sh

set -uo pipefail

_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_fail=0

_check() {
  if [[ "$2" == "$3" ]]; then printf '  ok   %s\n' "$1"
  else printf '  FAIL %s\n       want: %s\n       got:  %s\n' "$1" "$2" "$3"; _fail=1; fi
}

_stub_dir="$(mktemp -d)"
trap 'rm -rf "$_stub_dir"' EXIT

# A curl that behaves like the real one on a refused/failed connection: it
# prints 000 for %{http_code} AND exits non-zero.
_stub_curl() {
  cat > "$_stub_dir/curl" <<STUB
#!/usr/bin/env bash
printf '$1'
exit $2
STUB
  chmod +x "$_stub_dir/curl"
}

_verdict() {  # <what proxy_probe returns for the stubbed curl>
  ( export PATH="$_stub_dir:$PATH" NEXUS_PROXY_URL=https://unreachable.example:3128
    source "$_dir/preflight_checks.sh" >/dev/null 2>&1
    proxy_verdict )
}

_stub_curl "000" 35
_check "a failed TLS connect is not reported as listening" "unreachable" "$(_verdict)"

_stub_curl "000" 7
_check "a refused connection is not reported as listening" "unreachable" "$(_verdict)"

_stub_curl "400" 0
_check "a 400 on root means it IS listening" "listening:400" "$(_verdict)"

_stub_curl "404" 0
_check "a 404 on root means it IS listening" "listening:404" "$(_verdict)"

[[ $_fail -eq 0 ]] && printf 'preflight check self-test: PASS\n' || printf 'preflight check self-test: FAIL\n'
exit $_fail
