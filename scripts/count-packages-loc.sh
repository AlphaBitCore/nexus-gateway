#!/usr/bin/env bash
# TODO(doc-audit-2026-05-21): one-off LOC measurement helper. Zero non-self
# references across docs, Makefile, CI, package.json. Low-harm utility —
# keep available for ad-hoc measurements; revisit if still unused next cycle.
#
# Delegates to count-packages-code-loc.py (cloc-based: code lines exclude
# comments and blanks per language rules in cloc).
#
# Usage: ./scripts/count-packages-loc.sh
#        ./scripts/count-packages-loc.sh -- --all-languages --show-comments
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec python3 "$ROOT/scripts/count-packages-code-loc.py" "$@"
