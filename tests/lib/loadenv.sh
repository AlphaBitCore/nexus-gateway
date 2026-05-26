#!/usr/bin/env bash
# tests/lib/loadenv.sh — target-aware env-file loader for test scripts and
# skills.
#
# Usage:
#   source tests/lib/loadenv.sh          # uses $NEXUS_TEST_TARGET, defaults to "local"
#   source tests/lib/loadenv.sh prod     # forces target=prod
#
# Selection rules (first match wins):
#   1. Caller passed a positional argument.
#   2. $NEXUS_TEST_TARGET already in the environment.
#   3. Default to "local" — but only for TTY runs. Non-TTY (CI / piped
#      pipelines) MUST set NEXUS_TEST_TARGET explicitly; this enforces the
#      scenarios-isolation binding (no silent target defaults in automation).
#
# After this returns:
#   - $NEXUS_TEST_TARGET is one of local | dev | prod.
#   - $NEXUS_TEST_ROOT points at the tests/ directory.
#   - Every key from tests/.env.<target>.example is loaded as a default.
#   - tests/.env.<target> (gitignored, operator-customised) values override
#     the .example defaults.
#   - Process env vars set BEFORE loadenv.sh ran win over both files (same
#     non-overload semantics as godotenv) — so `NEXUS_VK=x ./script.sh` and
#     systemd EnvironmentFile injection both keep working.
#
# Safety guards:
#   - target=local: every NEXUS_*_URL must reference localhost / 127.0.0.1.
#     A misconfigured .env.local pointing at prod fails fast here, not
#     halfway through a destructive scenario.
#   - target=prod: NEXUS_CP_URL must NOT be loopback. Catches the inverse
#     mistake — a freshly-copied .env.prod left with localhost defaults.

set -u

# ─── Capture pre-existing env so we can preserve override semantics ─────────
# Snapshot every NEXUS_* var that was set BEFORE we touched anything; values
# from .env files will only fill in the ones not in this list. macOS ships
# bash 3.2 which has no associative arrays, so use a space-padded sentinel
# string for membership checks. `env` works in both bash and zsh (macOS
# default), avoiding the bash-only `compgen` builtin.
_lo_preexisting=" "
while IFS= read -r _lo_name; do
  [[ -z "$_lo_name" ]] && continue
  _lo_preexisting="${_lo_preexisting}${_lo_name} "
done < <(env | awk -F= '/^NEXUS_/{print $1}')

# ─── Target selection ──────────────────────────────────────────────────────

_lo_arg_target="${1:-}"
if [[ -n "$_lo_arg_target" ]]; then
  NEXUS_TEST_TARGET="$_lo_arg_target"
elif [[ -z "${NEXUS_TEST_TARGET:-}" ]]; then
  if [[ -t 1 ]]; then
    NEXUS_TEST_TARGET="local"
  else
    echo "loadenv.sh: refusing to default to 'local' for non-TTY run." >&2
    echo "  Set NEXUS_TEST_TARGET=local|dev|prod explicitly, or pass it as the first arg." >&2
    return 1 2>/dev/null || exit 1
  fi
fi
export NEXUS_TEST_TARGET

case "$NEXUS_TEST_TARGET" in
  local|dev|prod) ;;
  *)
    echo "loadenv.sh: unknown target '$NEXUS_TEST_TARGET' (allowed: local|dev|prod)" >&2
    return 1 2>/dev/null || exit 1
    ;;
esac

# ─── Locate repo / tests dir ───────────────────────────────────────────────
# BASH_SOURCE is bash-only; zsh (macOS default shell) uses %x. Detect and
# branch so `source tests/lib/loadenv.sh` works from either shell.

if [[ -n "${ZSH_VERSION:-}" ]]; then
  # In zsh, ${(%):-%x} expands to the current sourced file path.
  _lo_script_path="${(%):-%x}"
elif [[ -n "${BASH_VERSION:-}" ]]; then
  _lo_script_path="${BASH_SOURCE[0]}"
else
  echo "loadenv.sh: unsupported shell (need bash or zsh)" >&2
  return 1 2>/dev/null || exit 1
fi
_lo_script_dir="$(cd "$(dirname "$_lo_script_path")" && pwd)"
NEXUS_TEST_ROOT="$(cd "$_lo_script_dir/.." && pwd)"
export NEXUS_TEST_ROOT

# ─── Parse .env.<target>.example then .env.<target> on top ─────────────────
# Manual parse (not `source`) so we can honor non-overload semantics:
# pre-existing process env values are preserved; .env values only fill gaps.

_lo_load_file() {
  local file="$1"
  [[ -f "$file" ]] || return 1
  local line key value
  while IFS= read -r line || [[ -n "$line" ]]; do
    # Strip leading whitespace; skip blank lines and comments.
    line="${line#"${line%%[![:space:]]*}"}"
    [[ -z "$line" || "${line:0:1}" == "#" ]] && continue
    # Tolerate a leading "export " just in case.
    [[ "$line" == export\ * ]] && line="${line#export }"
    # Split on the first =.
    key="${line%%=*}"
    value="${line#*=}"
    key="${key%"${key##*[![:space:]]}"}"   # rtrim
    # Strip surrounding " or ' on the value.
    if [[ "${value:0:1}" == '"' && "${value: -1}" == '"' ]]; then
      value="${value:1:${#value}-2}"
    elif [[ "${value:0:1}" == "'" && "${value: -1}" == "'" ]]; then
      value="${value:1:${#value}-2}"
    fi
    # Non-overload: skip keys already in the pre-existing process env.
    if [[ "$_lo_preexisting" == *" $key "* ]]; then
      continue
    fi
    # Assign + export. `printf -v` is bash-only; use shell-detect so zsh
    # (macOS default) works too. Escape backslashes + double-quotes in value
    # before eval to keep raw .env content intact.
    if [[ -n "${ZSH_VERSION:-}" ]]; then
      typeset -g "$key=$value"
    else
      local _esc_value="${value//\\/\\\\}"; _esc_value="${_esc_value//\"/\\\"}"
      eval "$key=\"$_esc_value\""
    fi
    export "$key"
  done < "$file"
  return 0
}

_lo_example="$NEXUS_TEST_ROOT/.env.${NEXUS_TEST_TARGET}.example"
_lo_user="$NEXUS_TEST_ROOT/.env.${NEXUS_TEST_TARGET}"

if [[ ! -f "$_lo_example" && ! -f "$_lo_user" ]]; then
  echo "loadenv.sh: neither $_lo_example nor $_lo_user exists." >&2
  echo "  Copy .env.${NEXUS_TEST_TARGET}.example to .env.${NEXUS_TEST_TARGET} and fill in values." >&2
  return 1 2>/dev/null || exit 1
fi

# Example first (defaults), then user file (overrides) — non-overload still
# applies, so process env beats both.
_lo_load_file "$_lo_example" || true
_lo_load_file "$_lo_user"    || true

mkdir -p "${NEXUS_TEST_LOG_DIR:-/tmp/nexus-test}"

# ─── Safety guards ─────────────────────────────────────────────────────────

# 1. target=local: every URL must be loopback. A pasted-prod-into-local
#    .env.local file fails here before any destructive scenario runs.
if [[ "$NEXUS_TEST_TARGET" == "local" ]]; then
  for var in NEXUS_HUB_URL NEXUS_CP_URL NEXUS_AI_GW_URL NEXUS_PROXY_URL NEXUS_UI_URL; do
    # ${!var} is bash indirect expansion; eval works in both bash and zsh.
    eval "val=\${${var}:-}"
    if [[ -n "$val" && "$val" != *localhost* && "$val" != *127.0.0.1* ]]; then
      echo "loadenv.sh: target=local but $var=$val does not reference localhost." >&2
      echo "  Fix .env.local or set NEXUS_TEST_TARGET to the correct target." >&2
      return 1 2>/dev/null || exit 1
    fi
  done
fi

# 2. target=prod: NEXUS_CP_URL must NOT be loopback. Catches the inverse
#    mistake — fresh .env.prod copy still has placeholder localhost values.
if [[ "$NEXUS_TEST_TARGET" == "prod" ]]; then
  if [[ -z "${NEXUS_CP_URL:-}" || "$NEXUS_CP_URL" == *localhost* || "$NEXUS_CP_URL" == *127.0.0.1* ]]; then
    echo "loadenv.sh: target=prod but NEXUS_CP_URL=${NEXUS_CP_URL:-<unset>} is loopback." >&2
    echo "  Fix tests/.env.prod to point at the real production hostname." >&2
    return 1 2>/dev/null || exit 1
  fi
fi

unset _lo_arg_target _lo_script_dir _lo_example _lo_user _lo_preexisting _lo_name
unset -f _lo_load_file
