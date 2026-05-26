#!/usr/bin/env bash
# new.sh — Scaffold a new SDD plan directory with blank templates.
#
# Usage:
#   ./new.sh TICKET-ID        e.g. ./new.sh AUTH-42
#   ./new.sh user-export      (slug if no ticket system)

set -euo pipefail

SKILL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMPLATES_DIR="$SKILL_DIR/templates"
PLANS_DIR="$(pwd)/.plans"

# ── Args ──────────────────────────────────────────────────────────────────────
if [[ $# -lt 1 ]]; then
  echo "Usage: $0 <ticket-id-or-slug>" >&2
  exit 1
fi

TICKET="$1"
TARGET="$PLANS_DIR/$TICKET"

# ── Guard: already exists ─────────────────────────────────────────────────────
if [[ -d "$TARGET" ]]; then
  echo "Plan directory already exists: $TARGET"
  echo "Existing files:"
  ls "$TARGET"
  exit 0
fi

# ── Create directory ──────────────────────────────────────────────────────────
mkdir -p "$TARGET"

# ── Copy templates ────────────────────────────────────────────────────────────
TODAY="$(date +%Y-%m-%d)"

for tmpl in A1-spec A2-plan A3-tasks; do
  src="$TEMPLATES_DIR/${tmpl}.md"
  dst="$TARGET/${tmpl}.md"
  if [[ ! -f "$src" ]]; then
    echo "Warning: template not found: $src" >&2
    continue
  fi
  # Substitute today's date placeholder
  sed "s/\[YYYY-MM-DD\]/$TODAY/g" "$src" > "$dst"
done

# ── Ensure .plans is in .gitignore ────────────────────────────────────────────
GITIGNORE="$(pwd)/.gitignore"
if [[ -f "$GITIGNORE" ]]; then
  if ! grep -qxF '.plans/' "$GITIGNORE" && ! grep -qxF '.plans' "$GITIGNORE"; then
    echo '.plans/' >> "$GITIGNORE"
    echo "Added .plans/ to .gitignore"
  fi
else
  echo '.plans/' > "$GITIGNORE"
  echo "Created .gitignore with .plans/"
fi

# ── Done ──────────────────────────────────────────────────────────────────────
echo ""
echo "Created plan: $TARGET"
echo ""
ls "$TARGET"
echo ""
echo "Next: fill in $TARGET/A1-spec.md, then run validate.sh to check it."
