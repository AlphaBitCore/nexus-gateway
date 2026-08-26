#!/bin/bash
# lib-verify_test.sh — behavioral test for the fail-closed supply-chain
# gate. Asserts the security-critical invariant: verify_sha256 / verify_git_commit
# return SUCCESS only for an exact match and FAIL (non-zero) for every tamper /
# drift / missing-input case. Run: bash nexus-ami/scripts/lib-verify_test.sh
#
# Self-contained: no network, no real artifacts — exercises the gate logic
# against locally-constructed fixtures.

set -uo pipefail

# Source under test (sibling file).
# shellcheck source=lib-verify.sh
source "$(dirname "$0")/lib-verify.sh"

fails=0
pass() { echo "  PASS: $1"; }
fail() { echo "  FAIL: $1" >&2; fails=$((fails + 1)); }

# expect_ok <desc> <cmd...>   — command must return 0
expect_ok() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d (expected success, got $?)"; fi; }
# expect_fail <desc> <cmd...> — command must return non-zero (fail-closed)
expect_fail() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then fail "$d (expected failure, got success)"; else pass "$d"; fi; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# ── verify_sha256 ────────────────────────────────────────────────────────────
printf 'nexus-appliance-artifact\n' > "$WORK/artifact.bin"
GOOD=$(nexus_sha256 "$WORK/artifact.bin")
BAD=0000000000000000000000000000000000000000000000000000000000000000

echo "verify_sha256:"
expect_ok   "matching digest passes"                 verify_sha256 "$GOOD" "$WORK/artifact.bin"
expect_fail "mismatched digest is refused"           verify_sha256 "$BAD"  "$WORK/artifact.bin"
expect_fail "empty expected digest is refused"       verify_sha256 ""      "$WORK/artifact.bin"
expect_fail "blank (spaces) expected digest refused" verify_sha256 "   "   "$WORK/artifact.bin"
expect_fail "missing file is refused"                verify_sha256 "$GOOD" "$WORK/does-not-exist.bin"

# A one-byte tamper must flip the verdict (collision-resistance smoke).
printf 'nexus-appliance-artifactX\n' > "$WORK/tampered.bin"
expect_fail "tampered content under old digest refused" verify_sha256 "$GOOD" "$WORK/tampered.bin"

# ── verify_git_commit ────────────────────────────────────────────────────────
REPO="$WORK/repo"
git init -q "$REPO"
git -C "$REPO" -c user.email=t@t -c user.name=t commit -q --allow-empty -m init
HEAD=$(git -C "$REPO" rev-parse HEAD)
WRONG=1111111111111111111111111111111111111111

echo "verify_git_commit:"
expect_ok   "HEAD equal to pinned commit passes"   verify_git_commit "$REPO" "$HEAD"
expect_fail "HEAD != pinned (moved tag) refused"   verify_git_commit "$REPO" "$WRONG"
expect_fail "empty pinned commit refused"          verify_git_commit "$REPO" ""
expect_fail "non-git directory refused"            verify_git_commit "$WORK" "$HEAD"

echo
if [ "$fails" -eq 0 ]; then
  echo "lib-verify_test: ALL PASS"
  exit 0
fi
echo "lib-verify_test: $fails FAILURE(S)" >&2
exit 1
