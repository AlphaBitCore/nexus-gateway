#!/usr/bin/env bash
# status.sh — Show the phase status of all active SDD plans.
#
# Usage:
#   ./status.sh               (scans .plans/ in current directory)
#   ./status.sh /path/to/dir  (scans a specific plans root)

set -euo pipefail

PLANS_ROOT="${1:-$(pwd)/.plans}"

if [[ ! -d "$PLANS_ROOT" ]]; then
  echo "No .plans/ directory found at: $PLANS_ROOT"
  echo "Run 'new.sh <ticket>' to create your first plan."
  exit 0
fi

# ── Helpers ───────────────────────────────────────────────────────────────────

phase_badge() {
  local dir="$1"
  local a1 a2 a3
  a1=$([[ -f "$dir/A1-spec.md" ]]   && echo "1" || echo "0")
  a2=$([[ -f "$dir/A2-plan.md" ]]   && echo "1" || echo "0")
  a3=$([[ -f "$dir/A3-tasks.md" ]]  && echo "1" || echo "0")

  if [[ "$a3" == "1" ]]; then
    # Count tasks and completed tasks
    local total done
    total=$(grep -cE '^\- \[[ x]\]' "$dir/A3-tasks.md" 2>/dev/null || echo 0)
    done=$(grep -cE '^\- \[x\]' "$dir/A3-tasks.md" 2>/dev/null || echo 0)
    if [[ "$total" -gt 0 && "$done" -ge "$total" ]]; then
      echo "Phase 4 — Implementation complete ($done/$total tasks done)"
    elif [[ "$total" -gt 0 ]]; then
      echo "Phase 4 — Implementing ($done/$total tasks done)"
    else
      echo "Phase 3 — Tasks written (A3 exists, no tasks found)"
    fi
  elif [[ "$a2" == "1" ]]; then
    echo "Phase 3 — Plan approved, awaiting task breakdown"
  elif [[ "$a1" == "1" ]]; then
    echo "Phase 2 — Spec written, awaiting plan"
  else
    echo "Phase 0 — Directory empty"
  fi
}

spec_status() {
  local dir="$1"
  local spec="$dir/A1-spec.md"
  if [[ ! -f "$spec" ]]; then echo "—"; return; fi
  grep -m1 '^\*\*Status:\*\*' "$spec" | sed 's/\*\*Status:\*\* *//' || echo "Unknown"
}

open_questions() {
  local dir="$1"
  local spec="$dir/A1-spec.md"
  if [[ ! -f "$spec" ]]; then echo ""; return; fi
  local count
  count=$(grep -cE '^\- \[ \]' "$spec" 2>/dev/null || echo 0)
  if [[ "$count" -gt 0 ]]; then
    echo " [$count open question(s)]"
  fi
}

# ── Scan ──────────────────────────────────────────────────────────────────────
echo ""
echo "SDD Plans — $PLANS_ROOT"
echo "════════════════════════════════════════════════════"

FOUND=0
while IFS= read -r -d '' dir; do
  ticket=$(basename "$dir")
  phase=$(phase_badge "$dir")
  status=$(spec_status "$dir")
  questions=$(open_questions "$dir")

  printf "\n  %-20s  %s\n" "$ticket" "$phase"
  printf "  %-20s  Spec status: %s%s\n" "" "$status" "$questions"
  FOUND=$((FOUND + 1))
done < <(find "$PLANS_ROOT" -mindepth 1 -maxdepth 1 -type d -print0 | sort -z)

if [[ "$FOUND" -eq 0 ]]; then
  echo "  No plans found."
  echo ""
  echo "  Run: .claude/skills/spec-writing/scripts/new.sh <ticket-id>"
else
  echo ""
  echo "────────────────────────────────────────────────────"
  echo "  $FOUND plan(s) total"
fi

echo ""
