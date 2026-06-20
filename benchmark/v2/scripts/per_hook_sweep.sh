#!/usr/bin/env bash
# per_hook_sweep.sh — isolate the latency cost of EACH compliance hook.
#
# Enables exactly one hook at a time, runs S-02, saves the result tagged with the
# hook name, disables, repeats for all four. Produces the data to confirm/refute
# the hypothesis that response-quality-signals (SSE hold-back) owns most of the
# compliance cost.
#
# Run on the Nexus AMI from benchmark/v2/:
#   ./scripts/per_hook_sweep.sh            # real sweep
#   ./scripts/per_hook_sweep.sh --dry-run  # print the plan, no API calls, no run
#
# Requires .env.local (same contract as hooks_toggle.sh):
#   NEXUS_ADMIN_EMAIL, NEXUS_ADMIN_PASSWORD, NEXUS_OAUTH_REDIRECT_URI
# Optional: NEXUS_CP_URL (default http://localhost:3001), NEXUS_OAUTH_CLIENT_ID,
#   NEXUS_GW_NODE_ID, BENCH_VUS (default 6 → 3 effective on S-02),
#   BENCH_DURATION (default 300), BENCH_WARMUP (default 30), PYTHON (default python).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
V2_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
ENV_FILE="$V2_DIR/.env.local"

HOOKS=(pii-scanner keyword-blocker response-quality-signals noop-baseline)
OUTPUT_DIR="results/per_hook"
PYTHON="${PYTHON:-python}"
VUS="${BENCH_VUS:-6}"
DURATION="${BENCH_DURATION:-300}"
WARMUP="${BENCH_WARMUP:-30}"

DRY_RUN=false
[[ "${1:-}" == "--dry-run" ]] && DRY_RUN=true

# ── dry-run: describe the plan and exit before any auth / API / benchmark ──────
if [[ "$DRY_RUN" == true ]]; then
  echo "DRY RUN — per_hook_sweep.sh would do the following (no API calls, no runs):"
  echo "  env file:    $ENV_FILE"
  echo "  output dir:  $V2_DIR/$OUTPUT_DIR"
  echo "  S-02 knobs:  BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=$VUS BENCH_DURATION=$DURATION BENCH_WARMUP=$WARMUP"
  echo "  hooks to sweep (one at a time): ${HOOKS[*]}"
  echo ""
  for h in "${HOOKS[@]}"; do
    echo "  • disable all ${#HOOKS[@]} hooks → enable only '$h' → assert 1 loaded →"
    echo "    BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=$VUS BENCH_DURATION=$DURATION BENCH_WARMUP=$WARMUP \\"
    echo "      $PYTHON cli.py run --scenario s02 --gateway nexus --mode cache-disabled --output $OUTPUT_DIR"
    echo "    → rename results_<id>.{json,csv} to results_<id>_hook_${h}.{json,csv} → disable all → sleep 5"
  done
  echo ""
  echo "Then disable all hooks and print a summary of the result files."
  exit 0
fi

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

# ── PKCE S256 auth (same exchange as hooks_toggle.sh) ─────────────────────────
_b64url() { openssl base64 -A | tr -d '=' | tr '+/' '-_'; }
_urlencode() { python3 -c "import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1],safe=''))" "$1"; }
_json_field() { python3 -c "import sys,json;v=json.load(sys.stdin).get('$1','');print(str(v).lower() if isinstance(v,bool) else str(v))"; }

authenticate() {
  local verifier challenge state location authctx pwd_resp code token_resp
  verifier=$(openssl rand -base64 33 | _b64url)
  challenge=$(printf '%s' "$verifier" | openssl dgst -sha256 -binary | _b64url)
  state="per-hook-sweep-$$"
  location=$(curl -sS -o /dev/null -w '%{redirect_url}' \
    "$CP_URL/oauth/authorize?response_type=code&client_id=$CLIENT_ID&redirect_uri=$(_urlencode "$NEXUS_OAUTH_REDIRECT_URI")&code_challenge=$challenge&code_challenge_method=S256&state=$state&scope=openid")
  authctx=$(printf '%s' "$location" | sed -nE 's/.*[?&]authctx=([^&]+).*/\1/p')
  [[ -z "$authctx" ]] && { echo "error: /oauth/authorize returned no authctx; Location=$location" >&2; exit 1; }
  pwd_resp=$(curl -sS -X POST "$CP_URL/authserver/password" -H 'Content-Type: application/json' \
    -d "{\"authctx\":\"$authctx\",\"email\":\"$NEXUS_ADMIN_EMAIL\",\"password\":\"$NEXUS_ADMIN_PASSWORD\"}")
  code=$(printf '%s' "$(printf '%s' "$pwd_resp" | _json_field redirectUri)" | sed -nE 's/.*[?&]code=([^&]+).*/\1/p')
  [[ -z "$code" ]] && { echo "error: /authserver/password returned no code; resp=$pwd_resp" >&2; exit 1; }
  token_resp=$(curl -sS -X POST "$CP_URL/oauth/token" -H 'Content-Type: application/x-www-form-urlencoded' \
    --data-urlencode "grant_type=authorization_code" --data-urlencode "code=$code" \
    --data-urlencode "redirect_uri=$NEXUS_OAUTH_REDIRECT_URI" --data-urlencode "client_id=$CLIENT_ID" \
    --data-urlencode "code_verifier=$verifier")
  TOKEN=$(printf '%s' "$token_resp" | _json_field access_token)
  [[ -z "$TOKEN" ]] && { echo "error: /oauth/token returned no access_token; resp=$token_resp" >&2; exit 1; }
}

HOOKS_JSON=""
refresh_hooks() { HOOKS_JSON=$(curl -sS "$CP_URL/api/admin/hooks" -H "Authorization: Bearer $TOKEN"); }
_hook_id() {
  printf '%s' "$HOOKS_JSON" | python3 -c "
import sys,json
d=json.load(sys.stdin); hooks=d.get('data',d) if isinstance(d,dict) else d
for h in (hooks if isinstance(hooks,list) else []):
    if h.get('name')==sys.argv[1]: print(h['id']); break
" "$1" 2>/dev/null
}
set_hook() {
  local name="$1" enabled="$2" uuid resp actual
  uuid=$(_hook_id "$name")
  [[ -z "$uuid" ]] && { echo "  - $name: not present (skipped)"; return 0; }
  resp=$(curl -sS -X PUT "$CP_URL/api/admin/hooks/$uuid" -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' -d "{\"enabled\": $enabled}")
  actual=$(printf '%s' "$resp" | _json_field enabled)
  [[ "$actual" != "$enabled" ]] && { echo "error: $name did not set enabled=$enabled; resp=$resp" >&2; return 1; }
}
disable_all() { for h in "${HOOKS[@]}"; do set_hook "$h" false; done; }

# Count enabled hooks (among our sweep set) via the live hooks list.
count_enabled() {
  refresh_hooks
  printf '%s' "$HOOKS_JSON" | python3 -c "
import sys,json
d=json.load(sys.stdin); hooks=d.get('data',d) if isinstance(d,dict) else d
want=set(sys.argv[1:])
print(sum(1 for h in (hooks if isinstance(hooks,list) else []) if h.get('name') in want and h.get('enabled')))
" "${HOOKS[@]}"
}

mkdir -p "$V2_DIR/$OUTPUT_DIR"
echo "→ authenticating …"; authenticate; refresh_hooks; echo "  token ok ✓"

declare -a PRODUCED=()
for hook in "${HOOKS[@]}"; do
  echo "═══ hook: $hook ═══"
  disable_all
  set_hook "$hook" true
  n=$(count_enabled)
  if [[ "$n" != "1" ]]; then
    echo "  warning: expected exactly 1 enabled hook, got $n — continuing but flag the result" >&2
  else
    echo "  exactly 1 hook enabled ($hook) ✓"
  fi
  echo "  running S-02 (nexus, BENCH_VUS=$VUS, ${DURATION}s) …"
  ( cd "$V2_DIR" && BENCH_UNIQUE_PROMPTS=1 BENCH_VUS="$VUS" BENCH_DURATION="$DURATION" BENCH_WARMUP="$WARMUP" \
      "$PYTHON" cli.py run --scenario s02 --gateway nexus --mode cache-disabled --output "$OUTPUT_DIR" )
  # Rename the newest results_*.json/.csv with the hook suffix (cli.py has no
  # --output-suffix; the harness names files results_<id>.{json,csv}).
  newest=$(ls -t "$V2_DIR/$OUTPUT_DIR"/results_*.json 2>/dev/null | head -1 || true)
  if [[ -n "$newest" && "$newest" != *_hook_* ]]; then
    base="${newest%.json}"
    mv "$base.json" "${base}_hook_${hook}.json"
    [[ -f "$base.csv" ]] && mv "$base.csv" "${base}_hook_${hook}.csv"
    PRODUCED+=("${base}_hook_${hook}.json")
    echo "  saved: $(basename "${base}_hook_${hook}.json")"
  else
    echo "  warning: no new results file found to tag for $hook" >&2
  fi
  disable_all
  sleep 5
done

echo ""
echo "═══ sweep complete — result files ═══"
for f in "${PRODUCED[@]}"; do echo "  $f"; done
echo "All hooks left DISABLED. Re-enable baseline with: ./scripts/hooks_toggle.sh on"
