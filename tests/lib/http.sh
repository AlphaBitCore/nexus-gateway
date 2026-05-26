#!/usr/bin/env bash
# tests/lib/http.sh — shared curl wrappers for AI Gateway and Hub.
#
# Use cp_curl from auth.sh for Control Plane (cookie auth). These helpers
# target endpoints that authenticate by VK header (AI Gateway) or no auth
# (Hub /health).

set -u

# Required env contract — populated by tests/lib/loadenv.sh from
# tests/.env.<target>. Removed silent localhost fallbacks so a missing
# source fails fast rather than silently targeting the wrong host.
: "${NEXUS_AI_GW_URL:?source tests/lib/loadenv.sh first (or set NEXUS_AI_GW_URL)}"
: "${NEXUS_HUB_URL:?source tests/lib/loadenv.sh first (or set NEXUS_HUB_URL)}"
: "${NEXUS_PROXY_URL:?source tests/lib/loadenv.sh first (or set NEXUS_PROXY_URL)}"

# NOTE: do NOT use `local path=...` — in zsh (macOS default shell), `path` is
# a tied-array linked to $PATH and overwrites it inside the function. Use
# rel_path. (Same fix as tests/lib/auth.sh.)

# aigw_curl <vk> <relative-path> [curl options...]
# Path is the second positional arg so the variadic options can be empty.
aigw_curl() {
  local vk="$1"; shift
  local rel_path="$1"; shift
  curl -sS -H "Authorization: Bearer $vk" "$@" "$NEXUS_AI_GW_URL$rel_path"
}

# aigw_curl_code <vk> <relative-path> [curl options...]
aigw_curl_code() {
  local vk="$1"; shift
  local rel_path="$1"; shift
  curl -sS -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer $vk" "$@" "$NEXUS_AI_GW_URL$rel_path"
}

# hub_curl <relative-path> [curl options...]
hub_curl() {
  local rel_path="$1"; shift
  curl -sS "$@" "$NEXUS_HUB_URL$rel_path"
}

hub_curl_code() {
  local rel_path="$1"; shift
  curl -sS -o /dev/null -w '%{http_code}' "$@" "$NEXUS_HUB_URL$rel_path"
}

# wait_for_url <url> <timeout_sec> — block until url returns HTTP 2xx or
# timeout expires.
wait_for_url() {
  local url="$1"
  local timeout="${2:-30}"
  local deadline=$(( $(date +%s) + timeout ))
  while [[ $(date +%s) -lt $deadline ]]; do
    local code
    code=$(curl -sS -o /dev/null -w '%{http_code}' "$url" 2>/dev/null || echo "000")
    if [[ "$code" =~ ^2 ]]; then
      return 0
    fi
    sleep 0.5
  done
  return 1
}
