#!/usr/bin/env bash
# tests/smoke/run-all.sh — placeholder. Phase 1 will land real smoke scripts
# (test-control-plane.sh, test-hub.sh) here. Until then this exits 0 so the
# top-level runner is wired up but doesn't block other phase development.

set -eu

_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Discover smoke scripts. Each must be self-contained and exit 0 on pass.
shopt -s nullglob
scripts=("$_dir"/test-*.sh)
shopt -u nullglob

if [[ ${#scripts[@]} -eq 0 ]]; then
  printf 'No smoke scripts present yet (Phase 1 not landed). Treating as PASS.\n'
  exit 0
fi

failed=0
for s in "${scripts[@]}"; do
  printf '\n--- %s ---\n' "$(basename "$s")"
  if bash "$s"; then
    printf '✓ %s\n' "$(basename "$s")"
  else
    printf '✗ %s\n' "$(basename "$s")"
    failed=$((failed + 1))
  fi
done

exit "$failed"
