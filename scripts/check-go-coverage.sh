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
# Coverage measured via `go test -cover -count=1` per module — the full
# `./...` tree in all/CI mode, only the staged packages in --staged mode
# (that is the documented pre-commit contract: "staged Go packages", and
# per-package coverage is a per-package number, so testing the touched
# packages answers the pre-commit question exactly; the full-tree sweep
# stays with CI). `[no statements]` packages (pure type definitions,
# doc-only) are skipped.

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
# --no-renames + ACMD: a staged rename decomposes into its delete+add halves
# so BOTH affected packages are checked, and deleting a test file counts —
# it lowers its package's coverage exactly like editing one.
if [[ "$MODE" == "staged" ]]; then
  STAGED="$(git diff --cached --name-only --no-renames --diff-filter=ACMD 2>/dev/null | grep -E '\.go$' || true)"
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
# Initialise with `=()` rather than a bare `declare -a`: under `set -u`,
# bash 5.x treats a declared-but-never-assigned array as unbound, so
# `${#FAILED_PKGS[@]}` would throw "unbound variable" on a clean tree with
# zero failures (the common case) — a false gate failure. The `=()` form is
# bound-and-empty on both bash 3.2 and 5.x.
declare -a FAILED_PKGS=()
declare -a UNNECESSARY_ALLOWLIST=()
declare -a OK_PKGS=()

run_module() {
  # Subshell cd rather than pushd: each module runs in its own background
  # process, and a shared directory stack would race between them.
  local module="$1"
  shift
  ( cd "$module" && go test -cover -count=1 "$@" 2>&1 )
}

# nearest_module_root walks up from a dir to the closest ancestor holding a
# go.mod. Used to keep a staged file out of an OUTER module's target list
# when it really belongs to a nested module (packages/agent/ui inside
# packages/agent, go.mod-bearing testdata fixtures) — go test errors on a
# directory from a different module, and that error carries no parseable
# per-package FAIL line.
nearest_module_root() {
  local d="$1"
  while [[ -n "$d" && "$d" != "." && "$d" != "/" ]]; do
    [[ -f "$d/go.mod" ]] && { echo "$d"; return 0; }
    d="$(dirname "$d")"
  done
  return 1
}

# staged_pkgs_for_module prints the module-relative package dirs ("./x/y")
# holding staged .go changes (from $STAGED, which includes deletions and
# both halves of renames). Dirs the Go tool cannot test are filtered out,
# because their go-test error output has no parseable per-package line:
#   - testdata trees and _/. prefixed components (invisible to the Go tool);
#   - dirs that no longer exist or hold no .go files after a deletion;
#   - dirs whose nearest go.mod is NOT this module (nested modules).
staged_pkgs_for_module() {
  local module="$1"
  echo "$STAGED" \
    | grep -E "^$module/" \
    | while IFS= read -r f; do dirname "$f"; done \
    | sort -u \
    | while IFS= read -r dir; do
        case "/$dir/" in
          */testdata/*|*/_*|*/.*) continue ;;
        esac
        [[ -d "$dir" ]] || continue
        has_go=0
        for g in "$dir"/*.go; do
          [[ -e "$g" ]] && { has_go=1; break; }
        done
        [[ "$has_go" -eq 1 ]] || continue
        [[ "$(nearest_module_root "$dir")" == "$module" ]] || continue
        echo "${dir/#$module/.}"
      done
}

# Modules run concurrently. `go test ./...` parallelises packages within one
# module, but no single module saturates a developer box — measured serially,
# the modules cost the SUM of their runtimes while the machine sits partly
# idle. Each module writes to its own file so interleaved writes cannot
# corrupt another's output, and the results are concatenated in MODULES order
# afterwards so failures report deterministically.
#
# In staged mode each module tests only its staged packages, not its whole
# tree: per-package coverage is a per-package number, so the touched packages
# are the whole pre-commit question. The full sweep belongs to CI.
TMPDIR_COV="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_COV"' EXIT

# Concurrency is BOUNDED, not "every module at once". Each module's own
# `go test ./...` already parallelises across its packages, so launching all
# modules together oversubscribes the box by the product of the two. The cost
# is not slowness: timing-sensitive packages — the CLI's PTY/shell tests, the
# profiling package's signal handlers — lose their races under that load and
# the sweep reddens on a DIFFERENT innocent package each run. A gate that
# reddens at random is one people learn to re-run instead of read.
COV_JOBS="${COV_JOBS:-$(( $(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4) / 2 ))}"
[[ "$COV_JOBS" -lt 2 ]] && COV_JOBS=2

for i in "${!MODULES[@]}"; do
  m="${MODULES[$i]}"
  while [[ "$(jobs -rp | wc -l)" -ge "$COV_JOBS" ]]; do wait -n; done
  if [[ "$MODE" == "staged" ]]; then
    TARGETS=()
    while IFS= read -r d; do
      [[ -n "$d" ]] && TARGETS+=("$d")
    done < <(staged_pkgs_for_module "$m")
    [[ ${#TARGETS[@]} -eq 0 ]] && continue
    printf '%s\n' "${TARGETS[@]}" > "$TMPDIR_COV/$i.targets"
    { run_module "$m" "${TARGETS[@]}" > "$TMPDIR_COV/$i.out" 2>&1; echo $? > "$TMPDIR_COV/$i.rc"; } &
  else
    printf '%s\n' "./..." > "$TMPDIR_COV/$i.targets"
    { run_module "$m" ./... > "$TMPDIR_COV/$i.out" 2>&1; echo $? > "$TMPDIR_COV/$i.rc"; } &
  fi
done
wait

# Shared-build-cache eviction retry: on a multi-session box, another process
# wiping GOCACHE (`go clean -cache`) mid-compile makes imports fail with
# "could not import X (open .../go-build/...: no such file or directory)" and
# cascades [build failed] across dozens of healthy packages; the rerun simply
# rebuilds the evicted objects. Retry ONCE, per module, ONLY on that exact
# signature — real compile errors don't match it, and the retry re-runs the
# full test set so nothing is weakened.
for i in "${!MODULES[@]}"; do
  [[ -f "$TMPDIR_COV/$i.rc" ]] || continue
  rc="$(cat "$TMPDIR_COV/$i.rc")"
  [[ "$rc" != "0" ]] || continue
  if grep -qE 'could not import .*no such file or directory' "$TMPDIR_COV/$i.out"; then
    echo "[check-go-coverage] ${MODULES[$i]}: build-cache eviction detected (concurrent 'go clean -cache'?) — retrying once." >&2
    RETRY_TARGETS=()
    while IFS= read -r d; do
      [[ -n "$d" ]] && RETRY_TARGETS+=("$d")
    done < "$TMPDIR_COV/$i.targets"
    run_module "${MODULES[$i]}" "${RETRY_TARGETS[@]}" > "$TMPDIR_COV/$i.out" 2>&1
    echo $? > "$TMPDIR_COV/$i.rc"
  fi
done

ALL_OUTPUT=""
for i in "${!MODULES[@]}"; do
  [[ -f "$TMPDIR_COV/$i.out" ]] || continue
  ALL_OUTPUT+="$(cat "$TMPDIR_COV/$i.out")"$'\n'
  # Fail-closed guard: a module whose go test exited non-zero WITHOUT any
  # parseable per-package "FAIL github.com/..." line would otherwise vanish
  # from the report entirely (e.g. "no Go files in ...", toolchain errors,
  # a directory from a different module). Surface it as a failure with the
  # raw output head instead of silently passing.
  rc="$(cat "$TMPDIR_COV/$i.rc" 2>/dev/null || echo 0)"
  if [[ "$rc" != "0" ]] && ! grep -qE '^FAIL[[:space:]]+github\.com/' "$TMPDIR_COV/$i.out"; then
    FAILED_PKGS+=("module ${MODULES[$i]}: go test exited rc=$rc with no per-package FAIL line — $(head -c 300 "$TMPDIR_COV/$i.out" | tr '\n' ' ')")
  fi
done

# Packages whose every file sits behind a build tag. Without the tag the
# package contains no Go files at all, so `go test -cover` cannot build it and
# reports `[setup failed]` — which the FAIL branch below would otherwise call a
# test failure. There is nothing to measure and nothing to fix: it is the same
# fact as `coverage: [no statements]`, phrased differently by the toolchain.
#
# Go prints, on the lines preceding the FAIL:
#   # github.com/org/repo/pkg
#   package github.com/org/repo/pkg: build constraints exclude all Go files in /abs/path
#
# This is a property of the package, not an exemption for one of them, so it
# belongs in the gate rather than in the allowlist. A package with SOME files
# excluded still builds and is measured normally — only an entirely tag-gated
# package lands here.
#
# It is a property under the CURRENT GOOS and tag set, though, not an absolute
# one, and that is the catch: a `//go:build linux` misplaced onto every file of
# a package would exempt it silently on a macOS run. So each exemption is NAMED
# in the report rather than folded into the "N packages checked" count, where it
# would look exactly like a package at 100%. A gate that hides what it skipped
# is how a package stops being measured without anyone deciding that.
TAG_ONLY_EXEMPTED=()
TAG_ONLY_PKGS="$(grep -F 'build constraints exclude all Go files' <<< "$ALL_OUTPUT" \
  | grep -oE 'github\.com/[^[:space:]:]+' | sort -u)"

is_tag_only() {
  [[ -n "$TAG_ONLY_PKGS" ]] && grep -qxF "$1" <<< "$TAG_ONLY_PKGS"
}

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
    # Entirely tag-gated package: no untagged Go files exist, so the
    # `[setup failed]` is the toolchain saying "nothing here", not a broken
    # test. Run it with its tag to exercise it.
    if is_tag_only "$pkg"; then
      OK_PKGS+=("$pkg (all files behind build tags; nothing to measure untagged)")
      TAG_ONLY_EXEMPTED+=("$pkg")
      continue
    fi
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
    # `?   IMPORT [no test files]` — extract via the github.com token so a
    # leading-tab variant cannot slip past (see the coverage-line note below).
    pkg="$(echo "$line" | grep -oE 'github\.com/[^[:space:]]+' | head -1)"
    if is_allowed "$pkg"; then
      OK_PKGS+=("$pkg (allowlisted; [no test files])")
    else
      FAILED_PKGS+=("$pkg → [no test files]; threshold ${THRESHOLD}%")
    fi
    continue
  fi

  if echo "$line" | grep -q 'coverage: \[no statements\]'; then
    # Pure type defs / doc-only — no logic to test.
    pkg="$(echo "$line" | grep -oE 'github\.com/[^[:space:]]+' | head -1)"
    OK_PKGS+=("$pkg (no statements)")
    continue
  fi

  if echo "$line" | grep -q 'coverage: '; then
    # Extract the import path wherever it sits on the line. `go test -cover`
    # emits several shapes depending on whether the package has test functions:
    #   ok  \tIMPORT\tTIME\tcoverage: NN.N% of statements   (tested package)
    #   \tIMPORT\t\tcoverage: 0.0% of statements             (no test functions)
    # A bare "coverage: …" continuation line from a FAILed package carries no
    # import path. Matching the `github.com/` token handles every shape and
    # returns empty for the continuation line (skipped below). The previous
    # `awk -F'\t' '{print $1}'` read an empty field on the leading-tab no-test
    # shape, so every test-less package was silently dropped from the gate —
    # letting un-tested, non-allowlisted packages pass unnoticed.
    pkg="$(echo "$line" | grep -oE 'github\.com/[^[:space:]]+' | head -1)"
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
  if [[ ${#TAG_ONLY_EXEMPTED[@]} -gt 0 ]]; then
    echo ""
    echo "Not measured — every Go file is behind a build tag on this GOOS/tag set:"
    for e in "${TAG_ONLY_EXEMPTED[@]}"; do
      echo "  - $e"
    done
    echo "  Run these with their tag to exercise them. If one is here by accident"
    echo "  (a build constraint on files that should be portable), the package is"
    echo "  silently unmeasured and this line is the only sign of it."
  fi
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

# "[build failed]" tells you nothing without the compiler's own error lines
# ("# import/path" header + file:line diagnostics). Surface them so a
# transient failure (e.g. under heavy parallel load) is diagnosable from the
# report alone instead of vanishing with the temp files.
case "${FAILED_PKGS[*]}" in
  *"build failed"*)
    echo ""
    echo "Compiler diagnostics for the build failure(s) above:"
    # Herestring instead of `echo | grep`: head(1) closing the pipe early
    # would make the echo builtin report "write error: Broken pipe".
    grep -E '^#|^[^[:space:]]*\.go:[0-9]+|^[[:space:]]+.*\.go:[0-9]+' <<< "$ALL_OUTPUT" | head -40 | sed 's/^/    /'
    ;;
esac
echo ""
echo "Options:"
echo "  1. Add quality tests to bring the package above ${THRESHOLD}%."
echo "  2. If the package is DB-bound / OS-bound / integration-only /"
echo "     test helper / entry point: add it to scripts/.coverage-allowlist"
echo "     with a one-line rationale. Requires user approval."
echo ""
echo "Reference: CLAUDE.md → Mandatory rules → \"Unit test coverage ≥95%\""
exit 1
