#!/usr/bin/env bash
# tests/smoke/run-all.sh — runs every test-*.sh in this directory and reports
# three outcomes, not two. A suite that exits 77 AND created the marker this
# runner allocated for it declared itself SKIPPED; anything else that exits 77
# is a failure, because a skip has to be a statement rather than an exit code
# any tool could happen to produce.

set -eu

_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Discover smoke scripts. Each must be self-contained and exit 0 on pass.
shopt -s nullglob
scripts=("$_dir"/test-*.sh)
shopt -u nullglob

if [[ ${#scripts[@]} -eq 0 ]]; then
  # Not a pass. Zero suites verifies exactly as much as every suite skipping,
  # which the tally below refuses — the two must not disagree.
  printf 'FAIL: no smoke suites found — nothing could be verified.\n'
  exit 1
fi

# Three outcomes, not two. A suite that exits 77 declared itself SKIPPED
# (GNU convention) — it is reported as such and does not fail the run, but it
# is also never printed as a pass: "did not run" and "ran and passed" have to
# stay distinguishable from the outside.
failed=0
skipped=0
passed=0
for s in "${scripts[@]}"; do
  name="$(basename "$s")"
  printf '\n--- %s ---\n' "$name"
  # The marker is allocated here and removed before the run, so only the suite
  # itself can create it. A tool that happens to exit 77 — curl does, on a CA
  # cert it cannot read — leaves no marker and is counted as a FAILURE.
  marker="$(mktemp -u "${TMPDIR:-/tmp}/nexus-skip.XXXXXX")"
  rm -f "$marker"
  set +e
  NEXUS_SKIP_MARKER="$marker" bash "$s"
  rc=$?
  set -e
  if [[ "$rc" -eq 0 ]]; then
    printf '✓ %s\n' "$name"; passed=$((passed + 1))
  elif [[ "$rc" -eq 77 && -f "$marker" ]]; then
    printf '⊘ %s (skipped)\n' "$name"; skipped=$((skipped + 1))
  else
    if [[ "$rc" -eq 77 ]]; then
      printf '✗ %s (exit 77 with no skip marker — a tool exited 77, this is NOT a skip)\n' "$name"
    else
      printf '✗ %s\n' "$name"
    fi
    failed=$((failed + 1))
  fi
  rm -f "$marker"
done

printf '\n--- smoke suites: %d passed, %d skipped, %d failed ---\n' \
  "$passed" "$skipped" "$failed"

# A run in which EVERYTHING skipped is not a pass: it means no suite could
# reach its target, which from the outside looks exactly like a clean run.
if [[ "$passed" -eq 0 && "$skipped" -gt 0 && "$failed" -eq 0 ]]; then
  printf 'FAIL: every smoke suite skipped — nothing was actually verified.\n'
  exit 1
fi

# Carry the skip up. The caller (tests/run-all.sh) classifies a phase by its
# exit code alone, so a tally that lives only in this log would report the
# phase as a clean PASS while a suite never ran — the exact distinction this
# protocol exists to preserve, lost one level up.
if [[ "$failed" -eq 0 && "$skipped" -gt 0 ]]; then
  exit 77
fi

# Clamp: 77 is the skip signal, so a failure count that lands on it exactly
# must not be mistaken for one.
if [[ "$failed" -eq 77 ]]; then
  exit 76
fi

exit "$failed"
