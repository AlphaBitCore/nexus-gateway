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
# commit, and CI still enforces it.
#
# Usage: scripts/check-go-lint.sh --staged   (modules with staged .go files)
#        scripts/check-go-lint.sh            (every workspace module)

set -uo pipefail

PINNED="v2.12.2"   # keep in lockstep with .github/workflows/go-ci.yml

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

bin="$(command -v golangci-lint || true)"
if [[ -z "$bin" ]]; then
  printf '  [go-lint] golangci-lint not installed — skipping (CI still enforces it).\n'
  printf '  [go-lint] install the pinned version with:\n'
  printf '  [go-lint]   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@%s\n' "$PINNED"
  exit 0
fi

have="$("$bin" --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
if [[ -n "$have" && "v$have" != "$PINNED" ]]; then
  printf '  [go-lint] local golangci-lint is v%s but CI pins %s — a disagreement here is\n' "$have" "$PINNED"
  printf '  [go-lint] worse than no local check, so skipping. Install the pinned version.\n'
  exit 0
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
  modules="$(go work edit -json | sed -n 's/.*"DiskPath": "\(.*\)".*/\1/p')"
fi

[[ -z "$modules" ]] && exit 0

rc=0
while IFS= read -r m; do
  [[ -z "$m" || ! -d "$m" ]] && continue
  if ! out="$(cd "$m" && "$bin" run ./... 2>&1)"; then
    printf '  [go-lint] %s\n' "$m"
    printf '%s\n' "$out" | sed 's/^/    /'
    rc=1
  fi
done <<< "$modules"
exit $rc
