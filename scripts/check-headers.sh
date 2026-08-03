#!/usr/bin/env bash
# Enforce the Nexus header naming contract from
# docs/developers/architecture/cross-cutting/foundation/nexus-headers.md §1:
#
#   1. All X-Nexus-* string literals in Go code use canonical mixed-case
#      ("X-Nexus-Via", never "x-nexus-via"). Wire behaviour is identical
#      either way (Go canonicalises), so nothing but a gate keeps the
#      codebase from drifting back to mixed spellings.
#   2. No per-service prefix (X-Nexus-Aigw-* / -Cp- / -Agent-). One
#      X-Nexus-Hook, not three prefixed variants.
#   3. Every member of the AcceptHeaders / ExposeHeaders registries has a
#      use site outside the registry file — a member nothing reads or sets
#      is dead-allowlist noise (two such ghosts have shipped before).
#
# Legitimate exceptions are named in the allowlists below WITH their
# reason; add to them only with a reason that survives review.
#
# Usage: scripts/check-headers.sh   (repo-wide; fast — pure git grep)

set -uo pipefail
REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$REPO_ROOT"

FAIL=0

# --- Check 1: lowercase x-nexus-* literals in production Go ----------------
#
# Test files are exempt: deliberate case-insensitivity probes (a lowercase
# needle against a canonicalising Header.Get / a ToLower'd haystack) are
# legitimate there, and distinguishing them from carelessness needs a
# human. Production code has no such excuse.
#
# Allowlisted patterns (regex against the full grep line):
#  - forwardheader denylist prefix: compared against ToLower(name), MUST be
#    lowercase ("x-nexus-").
#  - OpenAPI vendor extensions (x-nexus-tier / x-nexus-iam-action /
#    x-nexus-unresolved-*): spec fields, not HTTP headers; OpenAPI
#    convention is lowercase.
# Allowed tokens are STRIPPED from each candidate line and the remainder is
# re-tested, rather than excluding whole lines — a line-level exclude would
# let a canonical or allowlisted literal mask a mis-cased one sharing its
# line (`w.Header().Set("X-Nexus-Via", r.Header.Get("x-nexus-via"))` must
# still fail).
strip_allowed_check1() {
  sed -E '
    s/"X-Nexus(-[A-Z][a-zA-Z0-9]*)+"//g
    s/"x-nexus-"//g
    s/x-nexus-tier//g
    s/x-nexus-iam-action//g
    s/x-nexus-unresolved[a-z0-9-]*//g
  '
}
raw="$(git grep --untracked -inE '"x-nexus-[a-z0-9-]+"' -- 'packages/**/*.go' ':!packages/**/*_test.go' 2>/dev/null || true)"
hits=""
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  if printf '%s' "$line" | strip_allowed_check1 | grep -qiE '"x-nexus-[a-z0-9-]+"'; then
    hits+="${line}"$'\n'
  fi
done <<< "$raw"
hits="${hits%$'\n'}"
if [[ -n "$hits" ]]; then
  echo "[check-headers] FAIL — lowercase x-nexus-* literals (contract: mixed-case X-Nexus-*):"
  echo "$hits" | sed 's/^/  /'
  FAIL=1
fi

# --- Check 2: per-service prefixes ------------------------------------------
#
# X-Nexus-Aigw-No-Cache is the one sanctioned survivor: a deprecated
# dual-read alias, retired together with its read site. Same strip-then-
# retest shape as check 1, so the sanctioned alias cannot mask a NEW
# prefixed header sharing its line.
strip_allowed_check2() {
  sed -E '
    s/"[Xx]-[Nn]exus-[Aa]igw-[Nn]o-[Cc]ache"//g
    s/"[Xx]-[Nn]exus-[Aa]igw-[Vv]ia"//g
  '
}
raw="$(git grep --untracked -niE '"x-nexus-(aigw|cp|agent)-[a-z0-9-]+"' -- 'packages/**/*.go' 2>/dev/null || true)"
hits=""
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  if printf '%s' "$line" | strip_allowed_check2 | grep -qiE '"x-nexus-(aigw|cp|agent)-[a-z0-9-]+"'; then
    hits+="${line}"$'\n'
  fi
done <<< "$raw"
hits="${hits%$'\n'}"
if [[ -n "$hits" ]]; then
  echo "[check-headers] FAIL — per-service header prefix (contract: unified X-Nexus-* names):"
  echo "$hits" | sed 's/^/  /'
  FAIL=1
fi

# --- Check 3: registry members must have a use site -------------------------
#
# Server-Timing is set via a literal; every X-Nexus member is set/read via a
# literal or named constant carrying the same string. A member whose string
# appears nowhere outside markers.go (and its tests) is a ghost.
registry_members() {
  # Pull quoted names out of the two registry slices.
  awk '/^var (AcceptHeaders|ExposeHeaders) = \[\]string\{/,/^\}/' \
    packages/shared/traffic/markers.go | grep -oE '"[A-Za-z0-9-]+"' | tr -d '"'
}
while IFS= read -r name; do
  case "$name" in
    Content-Type|Authorization) continue ;; # universal, read everywhere
  esac
  if ! git grep --untracked -lF "\"$name\"" -- 'packages/**/*.go' 2>/dev/null \
      | grep -v 'packages/shared/traffic/markers' | grep -q .; then
    echo "[check-headers] FAIL — registry member \"$name\" has no use site outside markers.go (ghost)"
    FAIL=1
  fi
done < <(registry_members)

if [[ "$FAIL" -ne 0 ]]; then
  echo
  echo "Contract: docs/developers/architecture/cross-cutting/foundation/nexus-headers.md §1."
  echo "Legitimate exceptions go in this script's allowlists, with a reason."
  exit 1
fi
echo "[check-headers] OK — header naming contract holds."
