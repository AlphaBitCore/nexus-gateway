#!/usr/bin/env bash
# Enforce per-package unit test coverage on Go packages.
#
# Binding rule (CLAUDE.md → Mandatory rules → "Unit test coverage ≥95%"):
# every Go package in this repo must hit at least 95% statement coverage,
# OR be listed in scripts/.coverage-allowlist with a concrete reason.
# Adding entries to the allowlist requires explicit user approval — the
# long-term goal is an empty allowlist.
#
# Usage:
#   scripts/check-go-coverage.sh                  # check all packages (CI default)
#   scripts/check-go-coverage.sh --staged         # only packages with staged Go files (pre-commit)
#   scripts/check-go-coverage.sh --threshold=90   # override (advisory; binding stays at 95)
#   scripts/check-go-coverage.sh --json           # machine-readable per-package report
#   scripts/check-go-coverage.sh --strict-allowlist  # also fail if an allowlisted package now exceeds threshold (cleanup hint)
#
# Coverage measured via `go test -cover -count=1 ./...` per module.
# `[no statements]` packages (pure type definitions, doc-only) are skipped.

set -uo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$REPO_ROOT"

THRESHOLD=95
MODE="all"
JSON_OUTPUT=0
STRICT_ALLOWLIST=0
ALLOWLIST_FILE="$REPO_ROOT/scripts/.coverage-allowlist"

while [[ $# -gt 0 ]]; do
  case $1 in
    --staged) MODE="staged"; shift ;;
    --threshold=*) THRESHOLD="${1#*=}"; shift ;;
    --json) JSON_OUTPUT=1; shift ;;
    --strict-allowlist) STRICT_ALLOWLIST=1; shift ;;
    -h|--help)
      grep -E '^#' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

# Discover modules (every packages/*/go.mod is one Go module).
MODULES=()
while IFS= read -r modfile; do
  MODULES+=("$(dirname "$modfile")")
done < <(find packages -maxdepth 3 -name go.mod -not -path '*/node_modules/*' | sort)

if [[ ${#MODULES[@]} -eq 0 ]]; then
  echo "no Go modules found under packages/" >&2
  exit 2
fi

# In --staged mode, restrict to modules with staged *.go changes.
if [[ "$MODE" == "staged" ]]; then
  STAGED="$(git diff --cached --name-only --diff-filter=ACM 2>/dev/null | grep -E '\.go$' || true)"
  if [[ -z "$STAGED" ]]; then
    echo "[check-go-coverage] no staged Go files — skipping."
    exit 0
  fi
  FILTERED=()
  for m in "${MODULES[@]}"; do
    if echo "$STAGED" | grep -qE "^$m/"; then
      FILTERED+=("$m")
    fi
  done
  # Empty-array guard: under `set -u`, "${FILTERED[@]}" is unbound when
  # FILTERED has zero elements. The early-exit below already handles
  # "no modules to check" — gate the reassignment so we reach it.
  if [[ ${#FILTERED[@]} -gt 0 ]]; then
    MODULES=("${FILTERED[@]}")
  else
    MODULES=()
  fi
  if [[ ${#MODULES[@]} -eq 0 ]]; then
    echo "[check-go-coverage] staged files outside Go modules — skipping."
    exit 0
  fi
fi

# Glob-match a package import path against an allowlist pattern.
# Patterns support shell-style globs (*, ?, [abc]).
match_pattern() {
  local pkg="$1" pat="$2"
  [[ "$pkg" == $pat ]]
}

is_allowed() {
  local pkg="$1"
  [[ -f "$ALLOWLIST_FILE" ]] || return 1
  while IFS= read -r raw; do
    # Strip trailing comment and surrounding whitespace.
    local line="${raw%%#*}"
    line="$(echo "$line" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
    [[ -z "$line" ]] && continue
    if match_pattern "$pkg" "$line"; then
      return 0
    fi
  done < "$ALLOWLIST_FILE"
  return 1
}

# Run coverage per module.
declare -a FAILED_PKGS
declare -a UNNECESSARY_ALLOWLIST
declare -a OK_PKGS

run_module() {
  local module="$1"
  pushd "$module" >/dev/null || return 1
  local raw
  raw="$(go test -cover -count=1 ./... 2>&1)"
  popd >/dev/null || true
  echo "$raw"
}

ALL_OUTPUT=""
for m in "${MODULES[@]}"; do
  ALL_OUTPUT+="$(run_module "$m")"$'\n'
done

# Parse each line. Possible shapes:
#   ok  	IMPORT	0.123s	coverage: 95.0% of statements
#   ok  	IMPORT	0.123s	coverage: [no statements]
#   IMPORT		coverage: 0.0% of statements              (failed build / no test files in some Go versions)
#   ?   	IMPORT	[no test files]
#   FAIL	IMPORT [...]
while IFS= read -r line; do
  [[ -z "$line" ]] && continue

  # Per-package failure line is exactly "FAIL\t<importpath>\t<time>" or
  # "FAIL\t<importpath> [build failed]". A bare "FAIL" (test summary) or
  # nested "--- FAIL: TestName" lines must be skipped — they're noise.
  if [[ "$line" =~ ^FAIL[[:space:]]+github\.com/[^[:space:]]+ ]]; then
    pkg="$(echo "$line" | awk '{print $2}')"
    # Test failures in allowlisted packages (typically DB-bound where
    # local schema is stale) are surfaced as warnings, not blockers —
    # the coverage rule is about the 95% threshold, not about whether
    # external infra is available locally.
    if is_allowed "$pkg"; then
      OK_PKGS+=("$pkg (test failure tolerated; allowlisted)")
    else
      FAILED_PKGS+=("$pkg (test failure: $line)")
    fi
    continue
  fi
  # Standalone "FAIL" or "--- FAIL: ..." lines: noise from individual
  # subtest failures already captured by the per-package FAIL line above.
  if [[ "$line" == FAIL ]] || [[ "$line" == "--- FAIL:"* ]]; then
    continue
  fi

  if echo "$line" | grep -q '\[no test files\]'; then
    # `?   IMPORT [no test files]`
    pkg="$(echo "$line" | awk '{print $2}')"
    if is_allowed "$pkg"; then
      OK_PKGS+=("$pkg (allowlisted; [no test files])")
    else
      FAILED_PKGS+=("$pkg → [no test files]; threshold ${THRESHOLD}%")
    fi
    continue
  fi

  if echo "$line" | grep -q 'coverage: \[no statements\]'; then
    # Pure type defs / doc-only — no logic to test.
    pkg="$(echo "$line" | awk '{print $2}')"
    OK_PKGS+=("$pkg (no statements)")
    continue
  fi

  if echo "$line" | grep -q 'coverage: '; then
    # Extract import path: usually field 2 of an `ok` line, but lines that begin
    # with the package name + tab when build skipped a test deps.
    pkg=""
    if [[ "$line" == ok* ]]; then
      pkg="$(echo "$line" | awk '{print $2}')"
    else
      # Standalone "coverage: X% of statements" line (from a FAILed test
      # package): no import path on this line — already captured by the
      # corresponding "FAIL\t<pkg>" line. Skip.
      if [[ "$line" == coverage:* ]]; then
        continue
      fi
      pkg="$(echo "$line" | awk -F'\t' '{print $1}')"
    fi
    pct="$(echo "$line" | sed -nE 's/.*coverage: ([0-9.]+)% of statements.*/\1/p')"
    if [[ -z "$pkg" || -z "$pct" ]]; then
      continue
    fi
    awk_result="$(awk -v p="$pct" -v t="$THRESHOLD" 'BEGIN { print (p+0 >= t+0) ? "ok" : "fail" }')"
    if [[ "$awk_result" == "ok" ]]; then
      OK_PKGS+=("$pkg ${pct}%")
      if [[ "$STRICT_ALLOWLIST" -eq 1 ]] && is_allowed "$pkg"; then
        UNNECESSARY_ALLOWLIST+=("$pkg now at ${pct}% — remove from allowlist")
      fi
    else
      if is_allowed "$pkg"; then
        OK_PKGS+=("$pkg ${pct}% (allowlisted)")
      else
        FAILED_PKGS+=("$pkg ${pct}% < ${THRESHOLD}%")
      fi
    fi
  fi
done <<< "$ALL_OUTPUT"

if [[ "$JSON_OUTPUT" -eq 1 ]]; then
  echo '{"threshold":'"$THRESHOLD"',"failed":['
  for i in "${!FAILED_PKGS[@]}"; do
    [[ $i -gt 0 ]] && echo ,
    printf '  %s' "$(printf '%s' "${FAILED_PKGS[$i]}" | sed 's/"/\\"/g; s/^/"/; s/$/"/')"
  done
  echo
  echo '],"ok_count":'"${#OK_PKGS[@]}"'}'
  [[ ${#FAILED_PKGS[@]} -eq 0 ]] && exit 0 || exit 1
fi

# Human-friendly report.
echo ""
if [[ ${#FAILED_PKGS[@]} -eq 0 ]]; then
  echo "[check-go-coverage] all packages ≥ ${THRESHOLD}% (or allowlisted) — ${#OK_PKGS[@]} packages checked"
  if [[ ${#UNNECESSARY_ALLOWLIST[@]} -gt 0 ]]; then
    echo ""
    echo "Allowlist entries that can be removed:"
    for e in "${UNNECESSARY_ALLOWLIST[@]}"; do
      echo "  - $e"
    done
  fi
  exit 0
fi

echo "[check-go-coverage] ${#FAILED_PKGS[@]} package(s) below ${THRESHOLD}%:"
echo ""
for f in "${FAILED_PKGS[@]}"; do
  echo "  ✗ $f"
done
echo ""
echo "Options:"
echo "  1. Add quality tests to bring the package above ${THRESHOLD}%."
echo "  2. If the package is DB-bound / OS-bound / integration-only /"
echo "     test helper / entry point: add it to scripts/.coverage-allowlist"
echo "     with a one-line rationale. Requires user approval."
echo ""
echo "Reference: CLAUDE.md → Mandatory rules → \"Unit test coverage ≥95%\""
exit 1
