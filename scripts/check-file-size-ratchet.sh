#!/usr/bin/env bash
# Enforce the file-size ratchet on production source files.
#
# Binding rule (docs/developers/workflow/conventions.md → Cross-cutting
# bindings → "File-size ratchet"): a production source file may not grow
# past its recorded baseline without an explicit waiver.
#
#   Rule 1 (no silent growth): a file present in scripts/.file-size-baseline
#           is capped at max(baseline, 800) + 10% lines.
#   Rule 2 (no new giants):    a file NOT in the baseline is capped at 800 lines.
#   Rule 3 (ratchet down):     when a file shrinks below its baseline,
#           --update-baseline rewrites the entry downward (the ratchet only
#           ever tightens; shrinking is never penalized).
#
# Production source = .go .ts .tsx .swift .py under packages/ and tools/,
# excluding *_test.go, *.test.ts / *.test.tsx, test directories, node_modules,
# dist, and generated files ("Code generated" / "DO NOT EDIT" in the first
# 5 lines). Test-file size is governed by the split-on-touch policy instead.
#
# Waivers: scripts/.file-size-waivers (<path> <cap> <one-line reason>);
# additions require explicit user approval — same governance as the
# coverage allowlist. The long-term goal is an empty waiver file.
#
# Usage:
#   scripts/check-file-size-ratchet.sh                    # full sweep (CI default)
#   scripts/check-file-size-ratchet.sh --staged           # staged production files only (pre-commit)
#   scripts/check-file-size-ratchet.sh --json             # machine-readable report
#   scripts/check-file-size-ratchet.sh --update-baseline  # Rule 3: ratchet shrunk entries down
#   scripts/check-file-size-ratchet.sh --regen-baseline   # rewrite the whole baseline (phase close-out)

set -uo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$REPO_ROOT"

BASELINE_FILE="$REPO_ROOT/scripts/.file-size-baseline"
WAIVER_FILE="$REPO_ROOT/scripts/.file-size-waivers"
NEW_FILE_CAP=800
GROWTH_FLOOR=800

MODE="all"
JSON_OUTPUT=0

while [[ $# -gt 0 ]]; do
  case $1 in
    --staged) MODE="staged"; shift ;;
    --update-baseline) MODE="update-baseline"; shift ;;
    --regen-baseline) MODE="regen-baseline"; shift ;;
    --json) JSON_OUTPUT=1; shift ;;
    -h|--help)
      grep -E '^#' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

# ---------------------------------------------------------------------------
# File discovery + measurement
# ---------------------------------------------------------------------------

# Tracked production source paths (one per line). Paths in this repo contain
# no whitespace; the line-oriented pipeline below relies on that.
list_production_files() {
  git ls-files -- packages tools \
    | grep -E '\.(go|ts|tsx|swift|py)$' \
    | grep -vE '(_test\.go|\.test\.tsx?)$' \
    | grep -vE '(^|/)tests?/|/node_modules/|/dist/'
}

# stdin: newline-separated paths (working-tree content).
# stdout: "<path>\t<lines>" for every non-generated, non-empty, readable file.
# Single awk process; getline returns <0 on unreadable paths (e.g. a tracked
# file deleted in the working tree by an in-flight refactor) instead of
# aborting the sweep.
measure_working_tree() {
  awk '
    {
      path = $0; n = 0; g = 0
      while ((getline line < path) > 0) {
        n++
        if (n <= 5 && (line ~ /Code generated/ || line ~ /DO NOT EDIT/)) g = 1
      }
      close(path)
      if (n > 0 && !g) printf "%s\t%d\n", path, n
    }
  '
}

# stdin: newline-separated paths. Measures the STAGED (index) blob so a
# partially staged file is judged on what would actually be committed.
measure_staged() {
  local p
  while IFS= read -r p; do
    git show ":$p" 2>/dev/null | awk -v path="$p" '
      { n++ }
      NR <= 5 && (/Code generated/ || /DO NOT EDIT/) { g = 1 }
      END { if (!g && NR > 0) printf "%s\t%d\n", path, n }
    '
  done
}

# Normalized "B<TAB>path<TAB>lines" baseline records (comments/blanks dropped).
baseline_records() {
  [[ -f "$BASELINE_FILE" ]] || return 0
  awk '!/^[[:space:]]*#/ && NF >= 2 { printf "B\t%s\t%s\n", $1, $2 }' "$BASELINE_FILE"
}

# Normalized "W<TAB>path<TAB>cap" waiver records.
waiver_records() {
  [[ -f "$WAIVER_FILE" ]] || return 0
  awk '!/^[[:space:]]*#/ && NF >= 2 { printf "W\t%s\t%s\n", $1, $2 }' "$WAIVER_FILE"
}

# ---------------------------------------------------------------------------
# Baseline maintenance modes
# ---------------------------------------------------------------------------

write_baseline_header() {
  cat > "$1" <<'EOF'
# scripts/.file-size-baseline — file-size ratchet baseline ("<path> <lines>" rows).
#
# Maintained ONLY by scripts/check-file-size-ratchet.sh:
#   --regen-baseline  rewrites this table from the current tree;
#   --update-baseline ratchets entries downward when files shrink (Rule 3).
# Growth past max(baseline, 800) + 10% fails the check (Rule 1); files not
# listed here are capped at 800 lines (Rule 2). Never hand-edit a size
# upward — growth needs a waiver in scripts/.file-size-waivers (user approval).
EOF
}

if [[ "$MODE" == "regen-baseline" ]]; then
  TMP="$(mktemp)"
  write_baseline_header "$TMP"
  list_production_files | measure_working_tree \
    | awk -F'\t' '{ printf "%s %d\n", $1, $2 }' | sort >> "$TMP"
  mv "$TMP" "$BASELINE_FILE"
  COUNT=$(grep -cv '^#' "$BASELINE_FILE" || true)
  echo "[check-file-size-ratchet] baseline regenerated: $COUNT files → $BASELINE_FILE"
  exit 0
fi

if [[ "$MODE" == "update-baseline" ]]; then
  if [[ ! -f "$BASELINE_FILE" ]]; then
    echo "[check-file-size-ratchet] no baseline at $BASELINE_FILE — run --regen-baseline first." >&2
    exit 2
  fi
  CURRENT="$(mktemp)"
  TMP="$(mktemp)"
  trap 'rm -f "$CURRENT" "$TMP"' EXIT
  list_production_files | measure_working_tree > "$CURRENT"
  if [[ ! -s "$CURRENT" ]]; then
    echo "[check-file-size-ratchet] measured zero production files — refusing to rewrite the baseline." >&2
    exit 2
  fi
  # Default FS (whitespace) parses both the tab-separated measurement rows
  # and the space-separated baseline rows; paths contain no whitespace.
  # The rewritten baseline goes to TMP; the ratchet log is captured on stdout.
  RATCHET_LOG="$(awk -v out="$TMP" '
    NR == FNR { cur[$1] = $2; next }
    /^[[:space:]]*#/ || NF < 2 { print > out; next }
    {
      if (($1 in cur) && cur[$1] + 0 < $2 + 0) {
        printf "%s %d\n", $1, cur[$1] > out
        printf "  ratchet-down %s: %d -> %d\n", $1, $2, cur[$1]
      } else {
        print > out
      }
    }
  ' "$CURRENT" "$BASELINE_FILE")"
  mv "$TMP" "$BASELINE_FILE"
  trap - EXIT
  rm -f "$CURRENT"
  if [[ -n "$RATCHET_LOG" ]]; then
    echo "$RATCHET_LOG"
    N="$(printf '%s\n' "$RATCHET_LOG" | grep -c 'ratchet-down')"
  else
    N=0
  fi
  echo "[check-file-size-ratchet] baseline ratcheted down for $N file(s)."
  exit 0
fi

# ---------------------------------------------------------------------------
# Check modes (full sweep / --staged)
# ---------------------------------------------------------------------------

if [[ ! -f "$BASELINE_FILE" ]]; then
  echo "[check-file-size-ratchet] missing baseline $BASELINE_FILE — run --regen-baseline first." >&2
  exit 2
fi

CURRENT="$(mktemp)"
trap 'rm -f "$CURRENT"' EXIT

if [[ "$MODE" == "staged" ]]; then
  STAGED="$(git diff --cached --name-only --diff-filter=ACM 2>/dev/null \
    | grep -E '\.(go|ts|tsx|swift|py)$' \
    | grep -E '^(packages|tools)/' \
    | grep -vE '(_test\.go|\.test\.tsx?)$' \
    | grep -vE '(^|/)tests?/|/node_modules/|/dist/' || true)"
  if [[ -z "$STAGED" ]]; then
    echo "[check-file-size-ratchet] no staged production source files — skipping."
    exit 0
  fi
  printf '%s\n' "$STAGED" | measure_staged > "$CURRENT"
else
  list_production_files | measure_working_tree > "$CURRENT"
fi

CHECKED=$(wc -l < "$CURRENT" | tr -d ' ')

# Tagged stream: B=baseline entry, W=waiver, C=current measurement.
RESULT="$({ baseline_records; waiver_records; awk -F'\t' '{ printf "C\t%s\t%s\n", $1, $2 }' "$CURRENT"; } \
  | awk -F'\t' -v new_cap="$NEW_FILE_CAP" -v floor="$GROWTH_FLOOR" '
      $1 == "B" { base[$2] = $3 + 0; next }
      $1 == "W" { waiv[$2] = $3 + 0; next }
      $1 == "C" {
        path = $2; lines = $3 + 0
        if (path in waiv) {
          cap = waiv[path]
          why = sprintf("waiver cap %d (scripts/.file-size-waivers)", cap)
        } else if (path in base) {
          b = base[path]
          m = (b > floor ? b : floor)
          cap = int(m * 1.1)
          why = sprintf("cap %d = max(baseline %d, %d) + 10%%", cap, b, floor)
        } else {
          cap = new_cap
          why = sprintf("new file cap %d (not in baseline)", cap)
        }
        if (lines > cap) printf "%s\t%d\t%s\n", path, lines, why
      }
    ')"

declare -a VIOLATIONS=()
if [[ -n "$RESULT" ]]; then
  while IFS= read -r line; do
    VIOLATIONS+=("$line")
  done <<< "$RESULT"
fi

if [[ "$JSON_OUTPUT" -eq 1 ]]; then
  echo '{"checked":'"$CHECKED"',"failed":['
  for i in "${!VIOLATIONS[@]}"; do
    [[ $i -gt 0 ]] && echo ,
    v="$(printf '%s' "${VIOLATIONS[$i]}" | tr '\t' ' ' | sed 's/"/\\"/g')"
    printf '  "%s"' "$v"
  done
  echo
  echo ']}'
  [[ ${#VIOLATIONS[@]} -eq 0 ]] && exit 0 || exit 1
fi

echo ""
if [[ ${#VIOLATIONS[@]} -eq 0 ]]; then
  echo "[check-file-size-ratchet] all $CHECKED production source files within their caps."
  exit 0
fi

echo "[check-file-size-ratchet] ${#VIOLATIONS[@]} file(s) over the size cap:"
echo ""
for v in "${VIOLATIONS[@]}"; do
  path="$(printf '%s' "$v" | cut -f1)"
  lines="$(printf '%s' "$v" | cut -f2)"
  why="$(printf '%s' "$v" | cut -f3)"
  echo "  ✗ $path: $lines lines > $why"
done
echo ""
echo "Options (try in order):"
echo "  1. Trim comments FIRST — over-long comment blocks are the usual cause."
echo "     Tighten prose, drop redundant restatements, keep the load-bearing"
echo "     rationale. Comments count toward the line total, so this is the"
echo "     cheapest way back under the cap and it keeps the code intact."
echo "  2. Decompose the file — split it along its responsibility seams"
echo "     (see docs/developers/workflow/conventions.md → File-size ratchet)."
echo "  3. If the size is genuinely irreducible (declaration table, protocol"
echo "     matrix): add '<path> <cap> <reason>' to scripts/.file-size-waivers."
echo "     Requires explicit user approval."
echo ""
echo "Shrinking is always allowed; after a shrink, --update-baseline tightens"
echo "the recorded baseline automatically."
exit 1
