#!/usr/bin/env bash
# ============================================================================
# Nexus — Hook-config SYNC verification (refutes "Bug 6")
#
# Proves: toggling a hook's enabled flag reaches the AI gateway LIVE, with NO
# resync — confirmed at the execution level (traffic_event requestHooksPipeline
# + x-nexus-hook header) and on the independent /runtime snapshot.
#
# HOW TO RUN (nothing to think about):
#     ./run_hooks_sync.sh
#
# Full python output is printed AND saved to ./logs/hooks_sync_<UTC>.log
# Overridable via env if ever needed: NEXUS_CP_URL, NEXUS_ADMIN_EMAIL,
# NEXUS_ADMIN_PASSWORD.
# ============================================================================
set -uo pipefail
cd "$(dirname "$0")"

: "${NEXUS_CP_URL:=https://44.212.215.114}"
: "${NEXUS_ADMIN_EMAIL:=admin@nexus.ai}"
: "${NEXUS_ADMIN_PASSWORD:=PDdSQIt0y1QDkLPRD0wJ}"
export NEXUS_CP_URL NEXUS_ADMIN_EMAIL NEXUS_ADMIN_PASSWORD

mkdir -p logs
TS="$(date -u +%Y%m%dT%H%M%SZ)"
LOG="logs/hooks_sync_${TS}.log"

{
  echo "=================================================================="
  echo " Nexus — Hook-config SYNC verification (Bug 6 check)"
  echo " Target CP : ${NEXUS_CP_URL}"
  echo " Started   : $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo " Log file  : ${LOG}"
  echo "=================================================================="
} | tee "$LOG"

python3 -u verify_hooks_sync.py 2>&1 | tee -a "$LOG"
rc=${PIPESTATUS[0]}

{
  echo
  echo "------------------------------------------------------------------"
  echo "Exit code: ${rc}   (0 = hook sync works / Bug 6 NOT reproduced)"
  echo "Full output saved to: ${LOG}"
} | tee -a "$LOG"
exit "$rc"
