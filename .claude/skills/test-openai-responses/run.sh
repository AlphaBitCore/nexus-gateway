#!/usr/bin/env bash
# /test-openai-responses runner.
#
# Drives 5 hand-rolled requests against POST /v1/responses (E56 ingress)
# on the local AI Gateway, then cross-checks the resulting traffic_event
# DB rows + Prometheus counters. Optional --cross-format runs the S6
# guard rejections against a non-OpenAI routing rule.
#
# Usage:
#   .claude/skills/test-openai-responses/run.sh [--vk <vk>] [--model <id>]
#                                               [--gw-url <url>] [--cross-format]
#                                               [--cp-url <url>]
#                                               [--cp-user <email>] [--cp-pass <pass>]
#                                               [--report <path>]
#
# Honors env vars: NEXUS_VK, NEXUS_GW_URL, NEXUS_CP_URL, NEXUS_CP_USER, NEXUS_CP_PASS.
#
# Output: Markdown report at /tmp/test-openai-responses-<UTC-timestamp>.md.

set -euo pipefail

GW_URL="${NEXUS_GW_URL:-http://localhost:3050}"
CP_URL="${NEXUS_CP_URL:-http://localhost:3001}"
CP_USER="${NEXUS_CP_USER:-admin@nexus.ai}"
CP_PASS="${NEXUS_CP_PASS:-admin123}"
VK="${NEXUS_VK:-}"
MODEL="gpt-5.2"
CROSS_FORMAT=0
REPORT="/tmp/test-openai-responses-$(date -u +%Y%m%dT%H%M%SZ).md"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --vk) VK="$2"; shift 2 ;;
    --model) MODEL="$2"; shift 2 ;;
    --gw-url) GW_URL="$2"; shift 2 ;;
    --cp-url) CP_URL="$2"; shift 2 ;;
    --cp-user) CP_USER="$2"; shift 2 ;;
    --cp-pass) CP_PASS="$2"; shift 2 ;;
    --cross-format) CROSS_FORMAT=1; shift ;;
    --report) REPORT="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

if [[ -z "$VK" ]]; then
  # Attempt to read from tests/.env.local (standing project local VK).
  if [[ -f tests/.env.local ]]; then
    set -a
    # shellcheck disable=SC1091
    . tests/.env.local
    set +a
    VK="${NEXUS_VK:-${LOCAL_TEST_VK:-}}"
  fi
fi
if [[ -z "$VK" ]]; then
  echo "ERROR: --vk required (or set NEXUS_VK in tests/.env.local)" >&2
  exit 2
fi

PY="$(command -v python3)"
exec "$PY" "$(dirname "$0")/run.py" \
  --vk "$VK" \
  --model "$MODEL" \
  --gw-url "$GW_URL" \
  --cp-url "$CP_URL" \
  --cp-user "$CP_USER" \
  --cp-pass "$CP_PASS" \
  --report "$REPORT" \
  $([[ "$CROSS_FORMAT" -eq 1 ]] && echo --cross-format)
