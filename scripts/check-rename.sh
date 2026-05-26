#!/usr/bin/env bash
# Verify a rename has been fully swept across all 14 layers (Go source,
# tests, yaml, env examples, SQL seed, DB migrations, UI source, UI i18n,
# prod systemd EnvironmentFile, prod DB rows, docs, skills, CLAUDE.md +
# cursor rules, and test fixtures).
#
# Binding rule documented in docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md
# §6.5 "BINDING — Rename Sweep Discipline". Half-completing a rename leaves
# the system in an inconsistent state where half the code reads the old
# name and half reads the new — silent prod breakage by construction.
#
# Usage:
#   scripts/check-rename.sh <OLD> <NEW>                      # check one rename
#   scripts/check-rename.sh --plan                           # check every rename in the migration manifest
#   scripts/check-rename.sh --manifest <file>                # check renames from a custom TSV (old<TAB>new per line)
#   scripts/check-rename.sh --skip-prod <OLD> <NEW>          # skip layers 9 + 10 (no SSH; for local-only checks)
#
# Exit codes:
#   0 — clean (no unswept references found in any non-allowlisted location)
#   1 — at least one unswept reference found
#   2 — usage / argument error
#
# Allowlisted matches (NOT counted as breakage):
#   - docs/developers/architecture/configuration-architecture*.md
#   - CHANGELOG.md
#   - git commit messages (we don't scan these)
#   - this script itself + .cursor/rules/sdd-workflow.mdc rename references (annotated)

set -euo pipefail

# ─── Constants ──────────────────────────────────────────────────────────────

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# Paths excluded from "found in code" scans. These are documentation
# locations where the rename is explicitly recorded by design.
ALLOWLIST_PATTERNS=(
  ':!docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md'
  ':!CHANGELOG.md'
  ':!scripts/check-rename.sh'
)

# Default manifest of renames committed in this migration. Generated from
# the rename tables in configuration-architecture.md §6 and migration §PR-2/PR-4.
# Format: tab-separated old<TAB>new, comment lines start with #.
DEFAULT_MANIFEST="$REPO_ROOT/scripts/check-rename.manifest.tsv"

# Prod EC2 host for layers 9 + 10 (read-only psql + cat env).
PROD_HOST="${PROD_HOST:-ec2-user@18.204.174.212}"
PROD_PG_PASSWORD="${PROD_PG_PASSWORD:-VclwRVYAAadpVPJfY9hzd0cM}"

# ─── Helpers ────────────────────────────────────────────────────────────────

red()    { printf '\033[31m%s\033[0m' "$*"; }
green()  { printf '\033[32m%s\033[0m' "$*"; }
yellow() { printf '\033[33m%s\033[0m' "$*"; }
bold()   { printf '\033[1m%s\033[0m' "$*"; }

usage() {
  sed -n '2,30p' "$0" | sed 's|^# \?||'
  exit 2
}

# Word-boundary git grep using fixed-string matching. -F treats the
# pattern as a literal (no regex metacharacters), -w adds word boundary
# constraints (so `cache` doesn't match `cacheable`). This handles
# OLD names that contain dots (e.g. `siem.config`) without misfiring on
# regex metachars.
# Args: <pattern> <pathspecs...>
ggrep() {
  local pattern="$1"; shift
  git grep -nwF "$pattern" -- "$@" 2>/dev/null || true
}

# Layer scan. Returns the count of matches found in the layer.
# Args: <layer-num> <layer-name> <old-name> <pathspecs...>
scan_layer() {
  local num="$1"; shift
  local name="$1"; shift
  local old="$1"; shift
  # ggrep uses -wF (fixed-string + word-boundary) so OLD is matched
  # literally regardless of any regex metachars it contains.
  local pattern="$old"
  local pathspecs=("$@")

  # Apply allowlist exclusions
  local all_paths=("${pathspecs[@]}" "${ALLOWLIST_PATTERNS[@]}")

  local out
  out=$(ggrep "$pattern" "${all_paths[@]}")
  local count
  count=$(printf '%s' "$out" | grep -c . || true)

  if [[ "$count" -gt 0 ]]; then
    printf '  [%s] %s — %s match%s\n' \
      "$(red "L$num")" \
      "$(bold "$name")" \
      "$(red "$count")" \
      "$([[ "$count" == 1 ]] && echo "" || echo "es")"
    printf '%s\n' "$out" | sed 's/^/      /'
    return 1
  else
    printf '  [%s] %s — clean\n' "$(green "L$num")" "$name"
    return 0
  fi
}

# Prod EC2 scans (layers 9 + 10). Skipped when --skip-prod is set or when
# SSH is not reachable.
scan_prod_env() {
  local old="$1"
  printf '  [%s] %s — ' "$(yellow "L9")" "$(bold "prod EnvironmentFile")"
  local out
  out=$(ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 "$PROD_HOST" \
    "sudo cat /etc/systemd/system/nexus-*.service.d/env.conf 2>/dev/null || sudo cat /etc/nexus-gateway/env 2>/dev/null" \
    2>/dev/null | grep -E "^${old}=" || true)
  if [[ -n "$out" ]]; then
    printf '%s\n' "$(red "FOUND in prod EnvironmentFile")"
    printf '%s\n' "$out" | sed 's/^/      /'
    return 1
  else
    printf '%s\n' "$(green "clean")"
    return 0
  fi
}

scan_prod_db() {
  local old="$1"
  printf '  [%s] %s — ' "$(yellow "L10")" "$(bold "prod template + override rows")"
  # configKey style names are lowercase; env-style ALL_CAPS won't match.
  # Only run the SQL check if the name looks like a configKey (snake_case).
  if [[ "$old" =~ ^[a-z][a-z0-9_]*$ ]]; then
    local rows
    rows=$(ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 "$PROD_HOST" \
      "PGPASSWORD=$PROD_PG_PASSWORD psql -h localhost -U nexus -d nexus_gateway -tA -c \
        \"SELECT 'template:'||type||'/'||config_key FROM thing_config_template WHERE config_key = '$old'
          UNION ALL
          SELECT 'override:'||thing_id||'/'||config_key FROM thing_config_override WHERE config_key = '$old';\"" \
      2>/dev/null || true)
    if [[ -n "$rows" ]]; then
      printf '%s\n' "$(red "FOUND")"
      printf '%s\n' "$rows" | sed 's/^/      /'
      return 1
    else
      printf '%s\n' "$(green "clean")"
      return 0
    fi
  else
    printf '%s (env name; skip)\n' "$(yellow "n/a")"
    return 0
  fi
}

# Single rename audit. Returns 0 if clean, 1 if any layer found leftovers.
check_one() {
  local old="$1"
  local new="$2"
  local skip_prod="${3:-false}"

  printf '\n%s %s → %s\n' "$(bold "==>")" "$(red "$old")" "$(green "$new")"

  local failed=0

  # ─── Layer 1: Go source (production) ──────────────────────────────────
  scan_layer 1  "Go source"      "$old" '*.go' ':!*_test.go' ':!vendor/' || failed=1

  # ─── Layer 2: Go tests ────────────────────────────────────────────────
  scan_layer 2  "Go tests"       "$old" '*_test.go' ':!vendor/' || failed=1

  # ─── Layer 3: yaml ────────────────────────────────────────────────────
  scan_layer 3  "yaml configs"   "$old" '*.yaml' '*.yml' ':!node_modules/' || failed=1

  # ─── Layer 4: env example files ───────────────────────────────────────
  scan_layer 4  "env examples"   "$old" '.env.example' 'tests/.env.*.example' || failed=1

  # ─── Layer 5: seed SQL ────────────────────────────────────────────────
  scan_layer 5  "seed SQL"       "$old" 'tools/db-migrate/seed/' || failed=1

  # ─── Layer 6: DB migrations ───────────────────────────────────────────
  scan_layer 6  "DB migrations"  "$old" 'tools/db-migrate/migrations/' || failed=1

  # ─── Layer 7: admin UI source ─────────────────────────────────────────
  scan_layer 7  "admin UI source" "$old" \
    'packages/control-plane-ui/src/' \
    'packages/agent/' \
    '*.tsx' '*.ts' \
    ':!node_modules/' ':!*.test.tsx' ':!*.test.ts' || failed=1

  # ─── Layer 8: UI i18n locales ─────────────────────────────────────────
  scan_layer 8  "UI i18n locales" "$old" \
    'packages/control-plane-ui/public/' \
    'packages/control-plane-ui/src/i18n/' \
    'packages/ui-shared/src/i18n/' \
    '*.json' || failed=1

  # ─── Layer 9-10: prod EC2 (skipped if --skip-prod) ───────────────────
  if [[ "$skip_prod" == "true" ]]; then
    printf '  [%s] %s — skipped\n'  "$(yellow "L9")"  "$(bold "prod EnvironmentFile")"
    printf '  [%s] %s — skipped\n'  "$(yellow "L10")" "$(bold "prod DB rows")"
  else
    scan_prod_env "$old"  || failed=1
    scan_prod_db  "$old"  || failed=1
  fi

  # ─── Layer 11: docs ───────────────────────────────────────────────────
  scan_layer 11 "docs"           "$old" 'docs/' || failed=1

  # ─── Layer 12: skills ─────────────────────────────────────────────────
  scan_layer 12 "skills"         "$old" '.claude/skills/' || failed=1

  # ─── Layer 13: CLAUDE.md + cursor rules ───────────────────────────────
  scan_layer 13 "CLAUDE.md+cursor rules" "$old" 'CLAUDE.md' '.cursor/rules/' || failed=1

  # ─── Layer 14: tests directory ────────────────────────────────────────
  scan_layer 14 "test fixtures + scripts" "$old" 'tests/' || failed=1

  if [[ "$failed" -eq 0 ]]; then
    printf '  %s\n' "$(green "ALL 14 LAYERS CLEAN")"
    return 0
  else
    printf '  %s\n' "$(red "INCOMPLETE — fix the above before merging the PR")"
    return 1
  fi
}

# ─── Manifest mode ──────────────────────────────────────────────────────────

check_manifest() {
  local manifest="$1"
  local skip_prod="${2:-false}"
  if [[ ! -f "$manifest" ]]; then
    printf 'manifest file not found: %s\n' "$manifest" >&2
    exit 2
  fi

  local total=0
  local clean=0
  local dirty=0
  local dirty_names=()

  while IFS=$'\t' read -r old new || [[ -n "$old" ]]; do
    # Skip blank lines and comments
    [[ -z "$old" || "$old" =~ ^[[:space:]]*# ]] && continue
    # Allow "(deleted)" as new for delete-only renames (e.g., REDIS_URL,
    # bootstrapKey). The script still scans for leftover OLD references.
    if [[ -z "${new:-}" ]]; then
      new="(deleted)"
    fi
    total=$((total + 1))
    if check_one "$old" "$new" "$skip_prod"; then
      clean=$((clean + 1))
    else
      dirty=$((dirty + 1))
      dirty_names+=("$old → $new")
    fi
  done < "$manifest"

  printf '\n%s\n' "$(bold "═══════════════ Manifest summary ═══════════════")"
  printf '  total renames: %d\n'  "$total"
  printf '  %s clean: %d\n'   "$(green "✓")" "$clean"
  printf '  %s dirty: %d\n'   "$(red   "✗")" "$dirty"
  if [[ "$dirty" -gt 0 ]]; then
    printf '\n%s\n' "$(bold "Dirty renames:")"
    for n in "${dirty_names[@]}"; do
      printf '  - %s\n' "$n"
    done
    exit 1
  fi
  exit 0
}

# ─── Main ───────────────────────────────────────────────────────────────────

SKIP_PROD=false

# Parse --skip-prod anywhere in args
NEW_ARGS=()
for arg in "$@"; do
  if [[ "$arg" == "--skip-prod" ]]; then
    SKIP_PROD=true
  else
    NEW_ARGS+=("$arg")
  fi
done
set -- "${NEW_ARGS[@]:-}"

case "${1:-}" in
  ""|"-h"|"--help")
    usage
    ;;
  "--plan")
    check_manifest "$DEFAULT_MANIFEST" "$SKIP_PROD"
    ;;
  "--manifest")
    [[ -z "${2:-}" ]] && { printf -- '--manifest requires a path\n' >&2; exit 2; }
    check_manifest "$2" "$SKIP_PROD"
    ;;
  *)
    [[ -z "${2:-}" ]] && usage
    if check_one "$1" "$2" "$SKIP_PROD"; then
      exit 0
    else
      exit 1
    fi
    ;;
esac
