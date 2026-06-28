#!/usr/bin/env bash
# ============================================================================
# Nexus — Response-hook STREAMING TTFT cost (client-side, curl-timed)
#
# Measures, with NO resync, interleaved:
#   ② gateway response-hook OFF   vs   ③ gateway response-hook ON
#   ① direct OpenAI baseline (if OPENAI_API_KEY is set on this box)
# and reports the hold-back TTFT cost (③−②) plus server-side upstreamTotal
# attribution. Timing is done by curl (time_starttransfer / time_total), so the
# numbers are not distorted by any client read loop.
#
# HOW TO RUN (nothing to think about):
#     ./run_response_hooks_latency.sh
#
# OPENAI_API_KEY should already be exported in this box's environment (it enables
# the direct-OpenAI baseline arm). If it isn't, the gateway ON/OFF comparison
# still runs and is the headline result.
#
# Full python output is printed AND saved to ./logs/response_hooks_latency_<UTC>.log
# Overridable via env if ever needed: NEXUS_CP_URL, NEXUS_AI_GW_URL,
# NEXUS_ADMIN_EMAIL, NEXUS_ADMIN_PASSWORD, MODEL, ROUNDS, MAX_TOKENS.
# ============================================================================
set -uo pipefail
cd "$(dirname "$0")"

: "${NEXUS_CP_URL:=https://44.212.215.114}"
: "${NEXUS_ADMIN_EMAIL:=admin@nexus.ai}"
: "${NEXUS_ADMIN_PASSWORD:=PDdSQIt0y1QDkLPRD0wJ}"
: "${MODEL:=gpt-4o-mini}"
: "${ROUNDS:=3}"
: "${MAX_TOKENS:=128}"
export NEXUS_CP_URL NEXUS_ADMIN_EMAIL NEXUS_ADMIN_PASSWORD

GW_ARG=()
[ -n "${NEXUS_AI_GW_URL:-}" ] && GW_ARG=(--gw-url "${NEXUS_AI_GW_URL}")

mkdir -p logs
TS="$(date -u +%Y%m%dT%H%M%SZ)"
LOG="logs/response_hooks_latency_${TS}.log"

{
  echo "=================================================================="
  echo " Nexus — Response-hook streaming TTFT cost (client-side, curl-timed)"
  echo " Target CP : ${NEXUS_CP_URL}    model=${MODEL} rounds=${ROUNDS} max_tokens=${MAX_TOKENS}"
  if [ -n "${OPENAI_API_KEY:-}" ]; then
    echo " Direct-OpenAI baseline arm: ON  (OPENAI_API_KEY present)"
  else
    echo " Direct-OpenAI baseline arm: OFF (export OPENAI_API_KEY to enable)"
  fi
  echo " Started   : $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo " Log file  : ${LOG}"
  echo "=================================================================="
} | tee "$LOG"

python3 -u verify_response_hooks_latency.py \
  --model "${MODEL}" --rounds "${ROUNDS}" --max-tokens "${MAX_TOKENS}" "${GW_ARG[@]}" 2>&1 | tee -a "$LOG"
rc=${PIPESTATUS[0]}

{
  echo
  echo "------------------------------------------------------------------"
  echo "Exit code: ${rc}   (0 = hold-back TTFT cost confirmed)"
  echo "Full output saved to: ${LOG}"
} | tee -a "$LOG"
exit "$rc"
