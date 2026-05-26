#!/usr/bin/env bash
# validate.sh — Lint an A1-spec.md for SDD completeness.
#
# Usage:
#   ./validate.sh .plans/AUTH-42/A1-spec.md
#   ./validate.sh .plans/AUTH-42/          (auto-finds A1-spec.md)
#
# Exits 0 if no errors, 1 if errors found (warnings do not affect exit code).

set -euo pipefail

# ── Args ──────────────────────────────────────────────────────────────────────
if [[ $# -lt 1 ]]; then
  echo "Usage: $0 <spec-file-or-plan-dir>" >&2
  exit 1
fi

TARGET="$1"
if [[ -d "$TARGET" ]]; then
  TARGET="$TARGET/A1-spec.md"
fi

if [[ ! -f "$TARGET" ]]; then
  echo "ERROR: File not found: $TARGET" >&2
  exit 1
fi

ERRORS=0
WARNINGS=0

pass()    { echo "  [PASS] $1"; }
warn()    { echo "  [WARN] $1"; ((WARNINGS++)) || true; }
fail()    { echo "  [FAIL] $1"; ((ERRORS++)) || true; }

echo ""
echo "Validating: $TARGET"
echo "────────────────────────────────────────────────"

# ── Required sections ─────────────────────────────────────────────────────────
echo ""
echo "Required sections:"

REQUIRED_SECTIONS=(
  "## Feature"
  "## Problem"
  "## User Scenarios"
  "## Functional Requirements"
  "## Non-Functional Requirements"
  "## Constraints"
  "## Acceptance Criteria"
  "## Out of Scope"
)

for section in "${REQUIRED_SECTIONS[@]}"; do
  if grep -qF "$section" "$TARGET"; then
    pass "$section"
  else
    fail "Missing section: $section"
  fi
done

# ── Status field ──────────────────────────────────────────────────────────────
echo ""
echo "Status:"

STATUS=$(grep -m1 '^\*\*Status:\*\*' "$TARGET" | sed 's/\*\*Status:\*\* *//' || echo "")
if [[ -z "$STATUS" ]]; then
  fail "No **Status:** field found"
elif [[ "$STATUS" == *"Draft"* ]]; then
  warn "Status is still Draft — confirm this is intentional"
elif [[ "$STATUS" == *"Approved"* ]]; then
  pass "Status: Approved"
else
  pass "Status: $STATUS"
fi

# ── Functional requirements ───────────────────────────────────────────────────
echo ""
echo "Functional Requirements:"

FR_COUNT=$(grep -cE '^- FR-[0-9]+\.' "$TARGET" || true)
if [[ "$FR_COUNT" -eq 0 ]]; then
  fail "No functional requirements found (expected: - FR-N. ...)"
elif [[ "$FR_COUNT" -lt 2 ]]; then
  warn "Only $FR_COUNT FR found — consider whether more are needed"
else
  pass "$FR_COUNT functional requirements"
fi

# Check for vague verbs in FRs
VAGUE_VERBS="support|handle|optimize|minimize|manage|improve|enhance|ensure|facilitate"
VAGUE_FRS=$(grep -E '^- FR-[0-9]+\.' "$TARGET" | grep -iE "$VAGUE_VERBS" || true)
if [[ -n "$VAGUE_FRS" ]]; then
  warn "FRs may contain vague verbs (support/handle/optimize/manage):"
  echo "$VAGUE_FRS" | sed 's/^/       /'
else
  pass "No vague verbs detected in FRs"
fi

# Check FR uses "shall"
FRS_WITHOUT_SHALL=$(grep -E '^- FR-[0-9]+\.' "$TARGET" | grep -v 'shall' || true)
if [[ -n "$FRS_WITHOUT_SHALL" ]]; then
  warn "Some FRs do not use 'shall' (preferred: 'The system shall ...'):"
  echo "$FRS_WITHOUT_SHALL" | sed 's/^/       /'
fi

# ── Acceptance criteria ───────────────────────────────────────────────────────
echo ""
echo "Acceptance Criteria:"

AC_COUNT=$(grep -cE '^- AC-[0-9]+\.' "$TARGET" || true)
if [[ "$AC_COUNT" -eq 0 ]]; then
  fail "No acceptance criteria found (expected: - AC-N. ...)"
elif [[ "$AC_COUNT" -lt "$FR_COUNT" ]]; then
  warn "$AC_COUNT ACs for $FR_COUNT FRs — every FR should map to at least one AC"
else
  pass "$AC_COUNT acceptance criteria"
fi

# ── Non-functional requirements ───────────────────────────────────────────────
echo ""
echo "Non-Functional Requirements:"

NFR_COUNT=$(grep -cE '^- NFR-[0-9]+\.' "$TARGET" || true)
if [[ "$NFR_COUNT" -eq 0 ]]; then
  warn "No NFRs found — consider performance, security, observability"
else
  pass "$NFR_COUNT non-functional requirements"
fi

# ── Out of scope ──────────────────────────────────────────────────────────────
echo ""
echo "Out of Scope:"

OOS_SECTION=$(awk '/^## Out of Scope/,/^## /' "$TARGET" | grep -vE '^## ' || true)
OOS_ITEMS=$(echo "$OOS_SECTION" | grep -c '^- ' || true)
if [[ "$OOS_ITEMS" -eq 0 ]]; then
  fail "Out of Scope section has no items — silence implies permission for AI agents"
else
  pass "$OOS_ITEMS out-of-scope items"
fi

# ── Open questions ────────────────────────────────────────────────────────────
echo ""
echo "Open Questions:"

UNRESOLVED=$(grep -cE '^\- \[ \]' "$TARGET" || true)
if [[ "$UNRESOLVED" -gt 0 ]]; then
  warn "$UNRESOLVED unresolved open question(s) — resolve before advancing to Phase 2"
else
  pass "No unresolved open questions"
fi

# ── Placeholder text check ────────────────────────────────────────────────────
echo ""
echo "Placeholder text:"

PLACEHOLDERS=$(grep -c '\[Actor\]\|\[YYYY-MM-DD\]\|\[ID or slug\]\|\[name or agent\]\|\[verb\] \[object\]' "$TARGET" || true)
if [[ "$PLACEHOLDERS" -gt 0 ]]; then
  warn "$PLACEHOLDERS placeholder(s) still in spec — replace before approving"
else
  pass "No unfilled placeholders detected"
fi

# ── Vague language ────────────────────────────────────────────────────────────
echo ""
echo "Language quality:"

VAGUE_WORDS="fast\|user.friendly\|robust\|seamless\|intelligent\|appropriate\|scalable\|flexible\|simple\|easy"
VAGUE_LINES=$(grep -inE "$VAGUE_WORDS" "$TARGET" | grep -v '^\s*<!--' || true)
if [[ -n "$VAGUE_LINES" ]]; then
  warn "Vague language detected — replace with measurable terms:"
  echo "$VAGUE_LINES" | head -5 | sed 's/^/       /'
else
  pass "No vague language detected"
fi

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "────────────────────────────────────────────────"
echo "Result: $ERRORS error(s), $WARNINGS warning(s)"
echo ""

if [[ "$ERRORS" -gt 0 ]]; then
  echo "SPEC NOT READY — fix errors before advancing to Phase 2."
  exit 1
elif [[ "$WARNINGS" -gt 0 ]]; then
  echo "Spec has warnings. Review before approving."
  exit 0
else
  echo "Spec looks good. Ready for approval."
  exit 0
fi
