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

# ── runtime-snapshot verification: POLL until converged ──────────────────────
# Hub→gateway config propagation takes a few seconds. A single read immediately
# after the PUT races the push and sees the OLD state — that false negative is
# exactly the "governance ON doesn't take effect" ghost from the Jul-10 arena
# session (tests/scripts/verify_hooks_sync.py proved the toggle works when you
# poll to convergence instead). So: poll GET /nodes/{id}/runtime every 0.5s
# (up to ~30s) until meta.desired_ver == meta.reported_ver AND the loaded hook
# set matches the target state. Caught-up-but-wrong-hooks = REAL bug (exit 1);
# never-caught-up = propagation problem (exit 1, with last-seen state);
# node not found = unverifiable (exit 3) so callers can't mistake it for OK.
echo "→ verifying propagation via runtime snapshot (poll until converged) …"
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
  echo "  ERROR: AI-gateway node not found — set NEXUS_GW_NODE_ID in .env.local." >&2
  echo "  Hooks were toggled but propagation is UNVERIFIED — do not benchmark on this state." >&2
  exit 3
fi

# expected hook names for the ON arm (from the snapshot we just restored)
EXPECT_ON=""
if [[ "$TARGET" == "on" && -f "$SNAPSHOT_FILE" ]]; then
  EXPECT_ON=$(paste -sd, "$SNAPSHOT_FILE")
fi

POLL_MAX="${NEXUS_HOOKS_POLL_MAX:-60}"   # 60 × 0.5s = 30s ceiling
CONVERGED=0
LAST_STATE=""
for _i in $(seq 1 "$POLL_MAX"); do
  RUNTIME=$(curl -sS "$CP_URL/api/admin/nodes/$GW_NODE_ID/runtime" -H "Authorization: Bearer $TOKEN")
  LAST_STATE=$(printf '%s' "$RUNTIME" | TGT="$TARGET" EXPECT_ON="$EXPECT_ON" python3 -c "
import sys,json,os
target=os.environ.get('TGT','off'); expect_on=[s for s in os.environ.get('EXPECT_ON','').split(',') if s]
try:
    d=json.load(sys.stdin)
    meta=d.get('meta',{}) or {}
    dv,rv=meta.get('desired_ver'),meta.get('reported_ver')
    hooks=d.get('snapshot',{}).get('sources',{}).get('config.hooks',{}).get('value',[]) or []
    names={h.get('name','') for h in hooks}
    resp=[h for h in hooks if h.get('stage')=='response']
    caught_up=(dv is not None and dv==rv)
    if target=='off':
        ok=caught_up and len(resp)==0
    else:
        ok=caught_up and all(n in names for n in expect_on)
    print(('OK' if ok else 'WAIT') + f'|dv={dv} rv={rv} hooks={len(hooks)} response={len(resp)}')
except Exception as e:
    print(f'WAIT|parse-error: {e}')
" 2>/dev/null || echo "WAIT|curl/parse failure")
  if [[ "$LAST_STATE" == OK\|* ]]; then CONVERGED=1; break; fi
  sleep 0.5
done

STATE_DETAIL="${LAST_STATE#*|}"
if [[ "$CONVERGED" == "1" ]]; then
  echo "  converged ✓  ($STATE_DETAIL)"
  if [[ "$TARGET" == "off" ]]; then
    echo "  response-stage hooks: none ✓  (SSE hold-back is OFF — clean A/B arm)"
  else
    echo "  restored hook set live on the gateway ✓"
  fi
else
  echo "  ERROR: did not converge within $((POLL_MAX / 2))s — last state: $STATE_DETAIL" >&2
  if [[ "$STATE_DETAIL" == *"dv="*"rv="* ]]; then
    echo "  If desired_ver == reported_ver above but the hook set is wrong → REAL bug (file it)." >&2
    echo "  If reported_ver is still behind → propagation stalled; check the gateway's Hub connection." >&2
  fi
  echo "  Do not benchmark on this state." >&2
  exit 1
fi

echo ""
echo "hooks are now: $TARGET (verified converged)"
