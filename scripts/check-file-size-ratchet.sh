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
#   scripts/check-file-size-ratchet.sh --prune-baseline   # Rule 4 ALONE: drop retired rows
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
    --prune-baseline) MODE="prune-baseline"; shift ;;
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

# --prune-baseline does Rule 4 and NOTHING else.
#
# It exists because the refusal below has to name a command that fixes exactly
# what it refused. --update-baseline also ratchets every shrunk entry down: on
# this tree that is 204 files and a 408-line diff, so a developer who hit the
# refusal over ONE retired row would be handed a rewrite they did not ask for
# and cannot review for the thing it is actually doing.
if [[ "$MODE" == "prune-baseline" ]]; then
  if [[ ! -f "$BASELINE_FILE" ]]; then
    echo "[check-file-size-ratchet] no baseline at $BASELINE_FILE — run --regen-baseline first." >&2
    exit 2
  fi
  TRACKED="$(mktemp)"
  TMP="$(mktemp)"
  trap 'rm -f "$TRACKED" "$TMP"' EXIT
  list_production_files > "$TRACKED"
  if [[ ! -s "$TRACKED" ]]; then
    echo "[check-file-size-ratchet] listed zero production files — refusing to rewrite the baseline." >&2
    exit 2
  fi
  PRUNE_LOG="$(awk -v out="$TMP" '
    NR == FNR { tracked[$1] = 1; next }
    /^[[:space:]]*#/ || NF < 2 { print > out; next }
    {
      if ($1 in tracked) { print > out; next }
      printf "  prune %s\n", $1
    }
  ' "$TRACKED" "$BASELINE_FILE")"
  mv "$TMP" "$BASELINE_FILE"
  trap - EXIT
  rm -f "$TRACKED"
  PRUNED=0
  if [[ -n "$PRUNE_LOG" ]]; then
    echo "$PRUNE_LOG"
    PRUNED="$(printf '%s\n' "$PRUNE_LOG" | grep -c '  prune ' || true)"
  fi
  echo "[check-file-size-ratchet] pruned $PRUNED retired row(s); no cap was changed."
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
  # Rule 4 is TWO jobs, and only one of them was implemented: rows were
  # ratcheted down, and every row not in the measurement was retained
  # verbatim — so a retired path lived forever and the check's own advice
  # ("--update-baseline prunes it") could not clear the refusal it printed.
  #
  # Pruning keys off the TRACKED set, not the measured one. measure_working_tree
  # legitimately drops a file it cannot read — a tracked file deleted in the
  # working tree by an in-flight refactor — and pruning on measurability would
  # silently discard that file's cap mid-refactor. Tracked-ness is also the
  # exact predicate the refusal uses, so the remedy provably clears it.
  TRACKED="$(mktemp)"
  list_production_files > "$TRACKED"
  RATCHET_LOG="$(awk -v out="$TMP" '
    FILENAME == ARGV[1] { tracked[$1] = 1; next }
    FILENAME == ARGV[2] { cur[$1] = $2; next }
    /^[[:space:]]*#/ || NF < 2 { print > out; next }
    {
      if (!($1 in tracked)) {
        printf "  prune %s: no longer a tracked production file\n", $1
        next
      }
      if (($1 in cur) && cur[$1] + 0 < $2 + 0) {
        printf "%s %d\n", $1, cur[$1] > out
        printf "  ratchet-down %s: %d -> %d\n", $1, $2, cur[$1]
      } else {
        print > out
      }
    }
  ' "$TRACKED" "$CURRENT" "$BASELINE_FILE")"
  rm -f "$TRACKED"
  mv "$TMP" "$BASELINE_FILE"
  trap - EXIT
  rm -f "$CURRENT"
  N=0
  PRUNED=0
  if [[ -n "$RATCHET_LOG" ]]; then
    echo "$RATCHET_LOG"
    N="$(printf '%s\n' "$RATCHET_LOG" | grep -c 'ratchet-down' || true)"
    PRUNED="$(printf '%s\n' "$RATCHET_LOG" | grep -c '  prune ' || true)"
  fi
  echo "[check-file-size-ratchet] baseline ratcheted down for $N file(s); pruned $PRUNED retired row(s)."
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

# A baseline row naming a path that is no longer a tracked production file caps
# NOTHING, and it is worse than inert: it grandfathers the path. Anything later
# created at that name inherits max(baseline, 800) + 10% instead of Rule 2's
# flat 800.
#
# Rule 4 already knew this — --update-baseline prunes retired paths — but the
# pruning only happens if somebody remembers to run it, so between runs the rot
# is invisible: the sweep answers "all N files within their caps" and never
# names the row. Refuse BEFORE measuring anything, so the answer is never a
# reassuring count computed over a stale list.
STALE_ROWS="$(
  comm -23 \
    <(grep -v '^#' "$BASELINE_FILE" | awk 'NF { print $1 }' | sort -u) \
    <(list_production_files | sort -u)
)"
if [[ -n "$STALE_ROWS" ]]; then
  echo "[check-file-size-ratchet] $(printf '%s\n' "$STALE_ROWS" | wc -l | tr -d ' ') baseline row(s) name a path that is no longer a tracked production file:" >&2
  printf '  ✗ %s\n' $STALE_ROWS >&2
  echo "" >&2
  echo "  The file MOVED   → repoint the row, in the same commit as the move." >&2
  echo "  The file is GONE → scripts/check-file-size-ratchet.sh --prune-baseline drops it, and changes nothing else." >&2
  exit 2
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
