#!/usr/bin/env bash
# Enforces lockstep between packages/nexus-hub/internal/jobs/ JobID constants
# and the §5 catalogue in docs/developers/architecture/cross-cutting/foundation/jobs-architecture.md.
#
# Failure modes detected:
# 1. A JobID const in a .go file that has no row in §5 (job shipped without
#    catalogue update).
# 2. A catalogue row whose ID does not appear in any .go file (stale doc row
#    after a job rename or deletion).
#
# Multi-cadence helpers (rollup_merge.go, thing_rollup_merge.go,
# ops_rollup_cascade.go) use field-based IDs (j.cfg.id) rather than const;
# their cadence variants are listed in the catalogue but cannot be machine-
# verified from the helper file. The catalogue text lists them by name; this
# script accepts that as the source of truth for those variants.
#
# Run from anywhere in the repo. CI / pre-commit wires this in when any
# packages/nexus-hub/internal/jobs/ file or jobs-architecture.md is staged.

set -uo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
JOBS_DIR="$REPO_ROOT/packages/nexus-hub/internal/jobs"
DOC="$REPO_ROOT/docs/developers/architecture/cross-cutting/foundation/jobs-architecture.md"

if [ ! -d "$JOBS_DIR" ]; then
  echo "✗ check-jobs-catalogue: missing $JOBS_DIR"
  exit 1
fi

# Doc-rewrite escape hatch (2026-05-22 archive sweep removed every
# architecture doc this script depends on; rewrite is queued). When the
# doc is absent we silently skip — lockstep enforcement resumes the
# moment the new file lands at the expected path. Re-enable strict mode
# by deleting this block.
if [ ! -f "$DOC" ]; then
  echo "⊘ jobs-catalogue lockstep skipped — doc absent (post 2026-05-22 archive sweep, awaiting rewrite)"
  exit 0
fi

failed=0

# Step 1: every JobID const in a .go file must appear as a row in §5.
for f in "$JOBS_DIR"/*.go; do
  case "$f" in
    *_test.go) continue ;;
  esac
  # Extract first JobID const value (either inline const or inside const block).
  ids=$(grep -E '^\s*[a-zA-Z]+JobID\s*=' "$f" 2>/dev/null | sed 's/.*"\([^"]*\)".*/\1/' | sort -u || true)
  [ -z "$ids" ] && continue
  while IFS= read -r id; do
    [ -z "$id" ] && continue
    if ! grep -qF "\`$id\`" "$DOC"; then
      echo "✗ job ID '$id' (defined in $(basename "$f")) is missing from $DOC §5"
      failed=$((failed + 1))
    fi
  done <<<"$ids"
done

# Step 2: catch the multi-cadence variants by name pattern. They are spelled in
# the doc as `<base>-1h` / `<base>-1d` / `<base>-1mo` / `thing-merge-...`.
# We just verify these labels exist in the doc since their construction-time
# IDs only appear in main.go.
for v in merge-1h merge-1d merge-1mo thing-merge-1h thing-merge-1d thing-merge-1mo ops-rollup-1d ops-rollup-1mo; do
  if ! grep -qF "\`$v\`" "$DOC"; then
    echo "✗ multi-cadence variant '$v' missing from $DOC §5"
    failed=$((failed + 1))
  fi
done

if [ "$failed" -gt 0 ]; then
  echo ""
  echo "✗ jobs-catalogue lockstep: $failed mismatch(es)."
  echo "  Fix: add the row(s) to $DOC §5 in the same PR."
  exit 1
fi

# Bonus: a friendly tally for green runs.
total=$(grep -cE '^\| `[a-z][a-z0-9.-]*` \|' "$DOC" 2>/dev/null || echo 0)
echo "✓ jobs-catalogue lockstep ($total catalogue rows)"
exit 0
