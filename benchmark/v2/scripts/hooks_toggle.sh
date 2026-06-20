#!/usr/bin/env bash
# benchmark/v2/scripts/hooks_toggle.sh — enable/disable Nexus compliance hooks
# for the hooks A/B benchmark, with snapshot-and-restore so the gateway returns
# to its EXACT prior state.
#
# Usage (run on the Nexus EC2 instance, from benchmark/v2/):
#   ./scripts/hooks_toggle.sh off   # before the hooks-OFF arm
#   ./scripts/hooks_toggle.sh on    # after the hooks-OFF arm (restore baseline)
#
# WHY THIS EXISTS / WHAT v1.5 GOT WRONG
# -------------------------------------
# The hooks A/B measures the latency cost of Nexus's compliance pipeline. In
# v1.5 only pii-scanner + keyword-blocker (request stage) were toggled. The
# RESPONSE-stage hook `response-quality-signals` stayed ON, and it holds back
# the SSE stream until ~400 chars of response are buffered. That hold-back was
# present in BOTH arms, so the measured delta collapsed to ~0. To measure the
# real overhead, EVERY response-stage hook must be off in the OFF arm.
#
# CORRECTNESS: response-content-safety and pii-outbound-scanner ship DISABLED in
# the seed. A naive `on` that sets enabled=true on a fixed list would turn those
# ON — leaving the gateway MORE restrictive than baseline. So `off` snapshots the
# set of currently-enabled hooks to a state file, and `on` restores exactly that
# set (and forces everything else off). Result: a clean round-trip.
#
# Requires in .env.local (benchmark/v2/.env.local):
#   NEXUS_ADMIN_EMAIL, NEXUS_ADMIN_PASSWORD, NEXUS_OAUTH_REDIRECT_URI
# Optional:
#   NEXUS_CP_URL (default http://localhost:3001), NEXUS_OAUTH_CLIENT_ID (cp-ui),
#   NEXUS_GW_NODE_ID (skip AI-gateway node auto-discovery)
#
# NOTE: this script re-authenticates on every invocation, so OAuth token expiry
# (1h) across a long A/B run is a non-issue for the toggle itself. Any *custom*
# admin calls in your orchestration must fetch their own fresh token.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="$SCRIPT_DIR/../.env.local"
SNAPSHOT_FILE="${NEXUS_HOOKS_SNAPSHOT:-/tmp/nexus_hooks_enabled_snapshot.txt}"

# Request-stage compliance hooks always disabled in the OFF arm.
REQUEST_COMPLIANCE_HOOKS=("pii-scanner" "keyword-blocker")

# ── argument ──────────────────────────────────────────────────────────────────
if [[ $# -ne 1 ]] || [[ "$1" != "on" && "$1" != "off" ]]; then
  echo "usage: $(basename "$0") on|off" >&2
  exit 1
fi
TARGET="$1"

# ── env ───────────────────────────────────────────────────────────────────────
if [[ ! -f "$ENV_FILE" ]]; then
  echo "error: .env.local not found at $ENV_FILE" >&2
  exit 1
fi
set -a; source "$ENV_FILE"; set +a

: "${NEXUS_ADMIN_EMAIL:?NEXUS_ADMIN_EMAIL not set in .env.local}"
: "${NEXUS_ADMIN_PASSWORD:?NEXUS_ADMIN_PASSWORD not set in .env.local}"
: "${NEXUS_OAUTH_REDIRECT_URI:?NEXUS_OAUTH_REDIRECT_URI not set in .env.local}"

CP_URL="${NEXUS_CP_URL:-http://localhost:3001}"
CLIENT_ID="${NEXUS_OAUTH_CLIENT_ID:-cp-ui}"

# ── helpers ─────────────────────────────────────────────────────────────────
_b64url() { openssl base64 -A | tr -d '=' | tr '+/' '-_'; }
_urlencode() { python3 -c "import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1],safe=''))" "$1"; }
_json_field() { python3 -c "import sys,json;v=json.load(sys.stdin).get('$1','');print(str(v).lower() if isinstance(v,bool) else str(v))"; }

# ── authenticate (PKCE S256) ──────────────────────────────────────────────────
echo "→ authenticating (PKCE S256) …"
VERIFIER=$(openssl rand -base64 33 | _b64url)
CHALLENGE=$(printf '%s' "$VERIFIER" | openssl dgst -sha256 -binary | _b64url)
STATE="hooks-toggle-$$"

LOCATION=$(curl -sS -o /dev/null -w '%{redirect_url}' \
  "$CP_URL/oauth/authorize?response_type=code&client_id=$CLIENT_ID&redirect_uri=$(_urlencode "$NEXUS_OAUTH_REDIRECT_URI")&code_challenge=$CHALLENGE&code_challenge_method=S256&state=$STATE&scope=openid")
AUTHCTX=$(printf '%s' "$LOCATION" | sed -nE 's/.*[?&]authctx=([^&]+).*/\1/p')
[[ -z "$AUTHCTX" ]] && { echo "error: /oauth/authorize returned no authctx; Location=$LOCATION" >&2; exit 1; }

PWD_RESP=$(curl -sS -X POST "$CP_URL/authserver/password" -H 'Content-Type: application/json' \
  -d "{\"authctx\":\"$AUTHCTX\",\"email\":\"$NEXUS_ADMIN_EMAIL\",\"password\":\"$NEXUS_ADMIN_PASSWORD\"}")
CODE=$(printf '%s' "$(printf '%s' "$PWD_RESP" | _json_field redirectUri)" | sed -nE 's/.*[?&]code=([^&]+).*/\1/p')
[[ -z "$CODE" ]] && { echo "error: /authserver/password returned no code; resp=$PWD_RESP" >&2; exit 1; }

TOKEN_RESP=$(curl -sS -X POST "$CP_URL/oauth/token" -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode "grant_type=authorization_code" --data-urlencode "code=$CODE" \
  --data-urlencode "redirect_uri=$NEXUS_OAUTH_REDIRECT_URI" --data-urlencode "client_id=$CLIENT_ID" \
  --data-urlencode "code_verifier=$VERIFIER")
TOKEN=$(printf '%s' "$TOKEN_RESP" | _json_field access_token)
[[ -z "$TOKEN" ]] && { echo "error: /oauth/token returned no access_token; resp=$TOKEN_RESP" >&2; exit 1; }
echo "  token: ${TOKEN:0:20}…  ✓"

# ── fetch all hooks (id, name, enabled, stage) ────────────────────────────────
HOOKS_JSON=$(curl -sS "$CP_URL/api/admin/hooks" -H "Authorization: Bearer $TOKEN")

# id for a hook name
_hook_id() {
  printf '%s' "$HOOKS_JSON" | python3 -c "
import sys,json
d=json.load(sys.stdin); hooks=d.get('data',d) if isinstance(d,dict) else d
for h in (hooks if isinstance(hooks,list) else []):
    if h.get('name')==sys.argv[1]: print(h['id']); break
" "$1" 2>/dev/null
}

# PUT enabled state for a hook by name; verifies the echo
_set_hook() {
  local name="$1" enabled="$2" uuid resp actual
  uuid=$(_hook_id "$name")
  if [[ -z "$uuid" ]]; then echo "  - $name: not present (skipped)"; return 0; fi
  resp=$(curl -sS -X PUT "$CP_URL/api/admin/hooks/$uuid" -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' -d "{\"enabled\": $enabled}")
  actual=$(printf '%s' "$resp" | _json_field enabled)
  [[ "$actual" != "$enabled" ]] && { echo "error: $name did not set enabled=$enabled; resp=$resp" >&2; return 1; }
  echo "  $name ($uuid): enabled=$actual ✓"
}

if [[ "$TARGET" == "off" ]]; then
  # 1) snapshot currently-enabled hook names so `on` restores EXACTLY this set.
  printf '%s' "$HOOKS_JSON" | python3 -c "
import sys,json
d=json.load(sys.stdin); hooks=d.get('data',d) if isinstance(d,dict) else d
for h in (hooks if isinstance(hooks,list) else []):
    if h.get('enabled'): print(h.get('name',''))
" > "$SNAPSHOT_FILE"
  echo "→ snapshotted $(wc -l < "$SNAPSHOT_FILE" | tr -d ' ') enabled hook(s) to $SNAPSHOT_FILE"

  # 2) disable: request-compliance hooks + EVERY response-stage hook (kills the
  #    SSE hold-back that contaminated the v1.5 A/B).
  mapfile -t RESPONSE_HOOKS < <(printf '%s' "$HOOKS_JSON" | python3 -c "
import sys,json
d=json.load(sys.stdin); hooks=d.get('data',d) if isinstance(d,dict) else d
for h in (hooks if isinstance(hooks,list) else []):
    if h.get('stage')=='response': print(h.get('name',''))
")
  echo "→ disabling request-compliance + all response-stage hooks …"
  for h in "${REQUEST_COMPLIANCE_HOOKS[@]}"; do _set_hook "$h" false; done
  for h in "${RESPONSE_HOOKS[@]}"; do [[ -n "$h" ]] && _set_hook "$h" false; done

elif [[ "$TARGET" == "on" ]]; then
  # restore EXACTLY the hooks that were enabled at snapshot time. Anything not in
  # the snapshot is forced off, so a prior buggy state can't leave extras on.
  if [[ ! -f "$SNAPSHOT_FILE" ]]; then
    echo "warning: no snapshot at $SNAPSHOT_FILE — falling back to known baseline" >&2
    printf '%s\n' "noop-baseline" "pii-scanner" "keyword-blocker" "response-quality-signals" > "$SNAPSHOT_FILE"
  fi
  mapfile -t WANT_ON < "$SNAPSHOT_FILE"
  echo "→ restoring ${#WANT_ON[@]} hook(s) from snapshot …"
  # Build the set of all hook names; enable if in snapshot, else disable.
  mapfile -t ALL_NAMES < <(printf '%s' "$HOOKS_JSON" | python3 -c "
import sys,json
d=json.load(sys.stdin); hooks=d.get('data',d) if isinstance(d,dict) else d
for h in (hooks if isinstance(hooks,list) else []): print(h.get('name',''))
")
  for name in "${ALL_NAMES[@]}"; do
    [[ -z "$name" ]] && continue
    if printf '%s\n' "${WANT_ON[@]}" | grep -qxF "$name"; then _set_hook "$name" true; else _set_hook "$name" false; fi
  done
fi

# ── runtime-snapshot verification (propagation to the AI-gateway node) ────────
echo "→ verifying propagation via runtime snapshot …"
GW_NODE_ID="${NEXUS_GW_NODE_ID:-}"
if [[ -z "$GW_NODE_ID" ]]; then
  GW_NODE_ID=$(curl -sS "$CP_URL/api/admin/nodes" -H "Authorization: Bearer $TOKEN" | python3 -c "
import sys,json
d=json.load(sys.stdin); nodes=d.get('data',d) if isinstance(d,dict) else d
for n in (nodes if isinstance(nodes,list) else []):
    if '-3050' in n.get('id',''): print(n['id']); break
" 2>/dev/null)
fi

if [[ -z "$GW_NODE_ID" ]]; then
  echo "  warning: AI-gateway node not found — set NEXUS_GW_NODE_ID in .env.local" >&2
else
  RUNTIME=$(curl -sS "$CP_URL/api/admin/nodes/$GW_NODE_ID/runtime" -H "Authorization: Bearer $TOKEN")
  echo "$RUNTIME" | python3 -c "
import sys,json
d=json.load(sys.stdin)
try:
    hooks=d['snapshot']['sources'].get('config.hooks',{}).get('value',[])
    resp=[h for h in hooks if h.get('stage')=='response']
    print(f\"  loaded hooks: {len(hooks)} total, {len(resp)} response-stage\")
    for h in resp: print(f\"    [response] {h.get('name','?')}\")
except (KeyError,TypeError) as e:
    print(f'  could not parse runtime snapshot: {e}', file=sys.stderr)
"
  if [[ "$TARGET" == "off" ]]; then
    RESP_COUNT=$(echo "$RUNTIME" | python3 -c "
import sys,json
d=json.load(sys.stdin)
try: print(len([h for h in d['snapshot']['sources'].get('config.hooks',{}).get('value',[]) if h.get('stage')=='response']))
except: print(-1)
" 2>/dev/null)
    if [[ "$RESP_COUNT" == "0" ]]; then
      echo "  response-stage hooks: none ✓  (SSE hold-back is OFF — clean A/B arm)"
    elif [[ "$RESP_COUNT" == "-1" ]]; then
      echo "  warning: could not parse runtime snapshot" >&2
    else
      echo "  warning: $RESP_COUNT response-stage hook(s) still loaded — hold-back STILL active" >&2
      echo "  the OFF arm will be contaminated; investigate before benchmarking." >&2
    fi
  fi
fi

echo ""
echo "hooks are now: $TARGET"
