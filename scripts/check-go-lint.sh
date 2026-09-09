#!/usr/bin/env bash
# check-go-lint.sh — run CI's linter on the Go modules a commit touches.
#
# golangci-lint is the only gate in this repo that sees an unexported function
# lose its last caller. `go build`, `go vet`, `go test -race`, the pre-commit
# hooks and every npm check are silent on it. Until this existed the linter ran
# ONLY in CI, so a branch learned about its violations one round-trip after the
# fact — and dead code accumulated in between. This release shipped exactly that:
# an envelope change removed a helper's last caller and left the helper behind.
#
# Pinned to the version .github/workflows/go-ci.yml uses. A local linter on a
# different version disagrees with CI, which is worse than no local linter: it
# teaches people to distrust the gate. The workflow already carries a note about
# a stale cache replaying SA5011 false positives, so the failure mode is real.
#
# Absent binary is a SKIP, not a block: a missing developer tool must not stop a
# commit, and CI still enforces it. A skip exits 3, NOT 0 — see below.
#
# Usage: scripts/check-go-lint.sh --staged   (modules with staged .go files)
#        scripts/check-go-lint.sh            (every workspace module)

set -uo pipefail

PINNED="v2.12.2"   # keep in lockstep with .github/workflows/go-ci.yml

# Exit 3 means "did not run", and it exists because exit 0 was a lie every
# consumer believed. The pre-commit hook captures this script's output and
# discards it on success, so a skip printed nothing at all and rendered as
# `go lint (CI parity) ✓`; check:all counted the same skip toward "all 46 gates
# passed". A branch then reached CI carrying fourteen violations that both
# local gates had reported clean. A gate that could not run and a gate that ran
# and passed must not print the same thing.
SKIP=3

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# Scope the cache to this checkout.
#
# golangci-lint's default cache is one shared directory per user, and its keys
# do not separate one checkout from another. Run this from a worktree after
# running it from a sibling and it replays the SIBLING's findings, complete
# with that tree's absolute paths — measured here as 159 findings for
# packages/shared, all naming files in another worktree, and zero after a
# `cache clean`. The header above already warned that a stale cache replays
# results; this is the same hazard with a different symptom, and the symptom
# is worse because the output looks like a real finding list.
#
# A per-checkout directory costs ~17 MB and removes the class. It is under the
# repo root, which .gitignore already covers.
export GOLANGCI_LINT_CACHE="${GOLANGCI_LINT_CACHE:-$repo_root/.golangci-cache}"

# Look where the instruction below installs it, not only on PATH. `go install`
# writes to GOBIN or GOPATH/bin, and neither is on the PATH a git hook inherits
# on a default macOS setup — so this gate printed "not installed" against a
# present binary and every commit skipped the only lint the repo runs locally.
bin="$(command -v golangci-lint || true)"
if [[ -z "$bin" ]]; then
  for d in "$(go env GOBIN 2>/dev/null)" "$(go env GOPATH 2>/dev/null)/bin"; do
    if [[ -n "$d" && -x "$d/golangci-lint" ]]; then bin="$d/golangci-lint"; break; fi
  done
fi
if [[ -z "$bin" ]]; then
  printf '  [go-lint] golangci-lint not installed — skipping (CI still enforces it).\n'
  printf '  [go-lint] install the pinned version with:\n'
  printf '  [go-lint]   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@%s\n' "$PINNED"
  exit $SKIP
fi

have="$("$bin" --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
if [[ -n "$have" && "v$have" != "$PINNED" ]]; then
  printf '  [go-lint] local golangci-lint is v%s but CI pins %s — a disagreement here is\n' "$have" "$PINNED"
  printf '  [go-lint] worse than no local check, so skipping. Install the pinned version.\n'
  exit $SKIP
fi

# macOS ships bash 3.2, which has no `mapfile` — the same portability trap the
# preflight hit with ${VAR^^}. Newline-delimited strings work in both.
modules=""
if [[ "${1:-}" == "--staged" ]]; then
  files="$(git diff --cached --name-only --diff-filter=ACMR -- '*.go')"
  [[ -z "$files" ]] && exit 0
  # A module is the nearest ancestor directory holding a go.mod.
  while IFS= read -r f; do
    [[ -z "$f" ]] && continue
    d="$(dirname "$f")"
    while [[ "$d" != "." && "$d" != "/" ]]; do
      if [[ -f "$d/go.mod" ]]; then modules="${modules}${d}"$'\n'; break; fi
      d="$(dirname "$d")"
    done
  done <<< "$files"
  modules="$(printf '%s' "$modules" | sort -u)"
else
  # Every module in the repo, not just the ones go.work lists. Starting from
  # go.work made this list identical to the workspace by construction, so a
  # module outside it — tests/scenarios, tests/agent/gap_closure — was never
  # swept, and the GOWORK=off branch below could not be reached.
  #
  # The openapigen testdata fixtures are excluded: one is named `broken` and is
  # deliberately unbuildable, so linting it is a permanent false failure.
  modules="$(git ls-files '*/go.mod' \
    | sed 's|/go.mod$||' \
    | grep -v '/openapigen/testdata/' \
    | sort -u)"
fi

[[ -z "$modules" ]] && exit 0

# Modules the workspace knows about. Anything else has its own go.mod and is
# outside go.work on purpose (tests/scenarios), so it has to be linted with
# GOWORK=off — the same mode the repo requires to build it. Left in workspace
# mode it fails with a typechecking error that reads like a broken gate and
# blocks every commit touching the module.
workspace_modules="$(go work edit -json 2>/dev/null | sed -n 's/.*"DiskPath": "\(.*\)".*/\1/p')"

in_workspace() {
  local want="${1#./}"
  while IFS= read -r w; do
    [[ -z "$w" ]] && continue
    [[ "${w#./}" == "$want" ]] && return 0
  done <<< "$workspace_modules"
  return 1
}

rc=0
while IFS= read -r m; do
  [[ -z "$m" || ! -d "$m" ]] && continue
  gowork=''
  if ! in_workspace "$m"; then gowork='off'; fi
  if ! out="$(cd "$m" && GOWORK="$gowork" "$bin" run ./... 2>&1)"; then
    printf '  [go-lint] %s\n' "$m"
    printf '%s\n' "$out" | sed 's/^/    /'
    rc=1
  fi
done <<< "$modules"
exit $rc
