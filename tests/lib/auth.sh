#!/usr/bin/env bash
# tests/lib/auth.sh — Control Plane API auth via the real OAuth + PKCE flow,
# driven with a seeded admin account (admin@nexus.ai / admin123 by default).
#
# Why the OAuth flow rather than x-admin-key:
#   - The login flow itself is part of what we're testing; bypassing it would
#     hide regressions in /oauth/authorize, /authserver/password, /oauth/token.
#   - Tokens issued here behave exactly like the SPA's session tokens, so any
#     middleware that runs only on user sessions still gets exercised.
#
# Flow (cached after the first successful call):
#   1. GET /oauth/authorize?response_type=code&client_id=cp-ui&...
#      → 302 to /login?authctx=...
#   2. POST /authserver/password { authctx, email, password }
#      → 200 { redirectUri: "<redirect>?code=<code>&state=<state>" }
#   3. POST /oauth/token (grant_type=authorization_code + PKCE verifier)
#      → 200 { access_token, refresh_token, expires_in }
#
# Public helpers:
#   cp_login            — populate cache. Idempotent.
#   cp_token            — print a valid access_token (refreshes if missing).
#   cp_curl PATH ...    — GET PATH (or any method via -X) using the token.
#   cp_curl_code PATH   — same, returns just the HTTP status code.
#   cp_curl_full PATH   — same, returns body + "\n---HTTP_STATUS---\n<code>".

set -u

# Required env contract — populated by tests/lib/loadenv.sh from
# tests/.env.<target>. Removed the legacy localhost fallback defaults so a
# missing loadenv source fails fast (prevents prod-* skills from silently
# falling back to localhost). Token cache is target-keyed by default so
# local/dev/prod tokens never cross-pollute.
: "${NEXUS_TEST_TARGET:?source tests/lib/loadenv.sh first to set NEXUS_TEST_TARGET}"
: "${NEXUS_CP_URL:?source tests/lib/loadenv.sh first (or set NEXUS_CP_URL)}"
: "${NEXUS_ADMIN_EMAIL:?set NEXUS_ADMIN_EMAIL in tests/.env.${NEXUS_TEST_TARGET}}"
: "${NEXUS_ADMIN_PASSWORD:?set NEXUS_ADMIN_PASSWORD in tests/.env.${NEXUS_TEST_TARGET}}"
: "${NEXUS_OAUTH_CLIENT_ID:=cp-ui}"
: "${NEXUS_OAUTH_REDIRECT_URI:?source tests/lib/loadenv.sh first (or set NEXUS_OAUTH_REDIRECT_URI)}"
: "${NEXUS_TOKEN_CACHE:=/tmp/nexus_token_${NEXUS_TEST_TARGET}}"

# Internal: PKCE base64url encoder using only openssl.
_pkce_b64url() {
  openssl base64 -A | tr -d '=' | tr '+/' '-_'
}

# cp_login — drive the OAuth + PKCE flow once and cache the access token.
# Returns 0 on success, 1 on any step failure.
cp_login() {
  local verifier challenge state authctx pwd_resp redirect_uri code token_resp access_token expires_in location

  verifier=$(openssl rand -base64 33 | _pkce_b64url)
  challenge=$(printf '%s' "$verifier" | openssl dgst -sha256 -binary | _pkce_b64url)
  state="cp-login-$(date +%s%N)-$$"

  # Step 1 — /oauth/authorize → 302 with authctx in Location header.
  location=$(curl -sS -o /dev/null -w '%{redirect_url}' \
    "$NEXUS_CP_URL/oauth/authorize?response_type=code&client_id=$NEXUS_OAUTH_CLIENT_ID&redirect_uri=$NEXUS_OAUTH_REDIRECT_URI&code_challenge=$challenge&code_challenge_method=S256&state=$state&scope=openid")
  authctx=$(printf '%s' "$location" | sed -nE 's/.*[?&]authctx=([^&]+).*/\1/p')
  if [[ -z "$authctx" ]]; then
    printf 'cp_login: /oauth/authorize did not return an authctx (got Location=%s)\n' "$location" >&2
    return 1
  fi

  # Step 2 — /authserver/password.
  pwd_resp=$(curl -sS -X POST "$NEXUS_CP_URL/authserver/password" \
    -H 'Content-Type: application/json' \
    -d "$(jq -nc --arg ctx "$authctx" --arg email "$NEXUS_ADMIN_EMAIL" --arg pwd "$NEXUS_ADMIN_PASSWORD" \
        '{authctx: $ctx, email: $email, password: $pwd}')")
  redirect_uri=$(printf '%s' "$pwd_resp" | jq -r '.redirectUri // empty')
  if [[ -z "$redirect_uri" ]]; then
    printf 'cp_login: /authserver/password failed: %s\n' "$pwd_resp" >&2
    return 1
  fi
  code=$(printf '%s' "$redirect_uri" | sed -nE 's/.*[?&]code=([^&]+).*/\1/p')
  if [[ -z "$code" ]]; then
    printf 'cp_login: missing code in redirectUri %s\n' "$redirect_uri" >&2
    return 1
  fi

  # Step 3 — /oauth/token.
  token_resp=$(curl -sS -X POST "$NEXUS_CP_URL/oauth/token" \
    -H 'Content-Type: application/x-www-form-urlencoded' \
    --data-urlencode "grant_type=authorization_code" \
    --data-urlencode "code=$code" \
    --data-urlencode "redirect_uri=$NEXUS_OAUTH_REDIRECT_URI" \
    --data-urlencode "client_id=$NEXUS_OAUTH_CLIENT_ID" \
    --data-urlencode "code_verifier=$verifier")
  access_token=$(printf '%s' "$token_resp" | jq -r '.access_token // empty')
  expires_in=$(printf '%s' "$token_resp" | jq -r '.expires_in // 3600')
  if [[ -z "$access_token" ]]; then
    printf 'cp_login: /oauth/token failed: %s\n' "$token_resp" >&2
    return 1
  fi

  # Cache as: <epoch_expiry>\n<token>. Refresh if epoch_expiry already past.
  local now expiry
  now=$(date +%s)
  # Subtract 60s safety margin so we refresh before the server rejects.
  expiry=$(( now + expires_in - 60 ))
  printf '%s\n%s\n' "$expiry" "$access_token" >"$NEXUS_TOKEN_CACHE"
}

# cp_token — print a valid access_token, refreshing the cache if needed.
cp_token() {
  if [[ -f "$NEXUS_TOKEN_CACHE" ]]; then
    local expiry token now
    expiry=$(sed -n '1p' "$NEXUS_TOKEN_CACHE" 2>/dev/null || echo 0)
    token=$(sed -n '2p' "$NEXUS_TOKEN_CACHE" 2>/dev/null || echo '')
    now=$(date +%s)
    if [[ -n "$token" && "$expiry" -gt "$now" ]]; then
      printf '%s' "$token"
      return 0
    fi
  fi
  cp_login || return 1
  sed -n '2p' "$NEXUS_TOKEN_CACHE"
}

# NOTE: do NOT use `local path=...` here. In zsh (macOS default shell), `path`
# is a tied-array parameter linked to $PATH; `local path="$1"` clobbers PATH
# inside the function and every subprocess (curl/date/openssl) errors with
# "command not found". Use a different variable name (rel_path).

# cp_curl <relative-path> [extra curl args...]
cp_curl() {
  local rel_path="$1"; shift
  local token
  token=$(cp_token) || return 1
  curl -sS -H "Authorization: Bearer $token" "$@" "$NEXUS_CP_URL$rel_path"
}

# cp_curl_code <relative-path> [extra curl args...]
cp_curl_code() {
  local rel_path="$1"; shift
  local token
  token=$(cp_token) || return 1
  curl -sS -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer $token" "$@" "$NEXUS_CP_URL$rel_path"
}

# cp_curl_full <relative-path> [extra curl args...]
cp_curl_full() {
  local rel_path="$1"; shift
  local token
  token=$(cp_token) || return 1
  curl -sS -H "Authorization: Bearer $token" \
    -w '\n---HTTP_STATUS---\n%{http_code}' "$@" "$NEXUS_CP_URL$rel_path"
}

# cp_login_check — round-trip a known endpoint to verify the login produced a
# token that actually authenticates. Returns 0 on success, 1 on failure.
cp_login_check() {
  local code
  code=$(cp_curl_code /api/admin/providers)
  [[ "$code" == "200" ]]
}
