#!/usr/bin/env bash
# smoke-compose.sh — bring the quickstart stack up and prove it works.
#
# This is the release gate for the compose form factor. It is deliberately
# end-to-end: unit checks on individual images cannot catch a wrong service
# hostname, a missing env var, or a migrator that exits non-zero and wedges
# every `service_completed_successfully` dependent.
set -euo pipefail

REGISTRY="${REGISTRY:-ghcr.io/alphabitcore}"
VERSION="${VERSION:-latest}"

UI_PORT=""
GATEWAY_PORT=""
PROXY_PORT=""

while [ $# -gt 0 ]; do
  case "$1" in
    --registry)     REGISTRY="$2"; shift 2 ;;
    --version)      VERSION="$2"; shift 2 ;;
    --ui-port)      UI_PORT="$2"; shift 2 ;;
    --gateway-port) GATEWAY_PORT="$2"; shift 2 ;;
    --proxy-port)   PROXY_PORT="$2"; shift 2 ;;
    *) echo "ERROR: unknown flag $1" >&2; exit 1 ;;
  esac
done

# Host ports. The gate should exercise the documented defaults when it can, but
# it must never fight whatever else is on this machine for them: a developer box
# routinely has something on 8080, and taking it away from that process to run a
# release check is not this script's business. So each port falls back to a free
# one and says which it used. deploy/docker-compose.yml derives every published
# port AND every external address it advertises from these same variables, so
# moving one moves the console origin, the OAuth callback the migrator registers,
# and the issuer with it — which is the only reason this is safe to move at all.
port_free() {
  # A listener is what matters, not a socket in TIME_WAIT.
  ! lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
}
pick_port() { # preferred label
  local preferred="$1" label="$2" p
  if port_free "$preferred"; then printf '%s' "$preferred"; return; fi
  for p in $(seq $((preferred + 10000)) $((preferred + 10099))); do
    if port_free "$p"; then
      echo "    ${label} port ${preferred} is in use; using ${p}" >&2
      printf '%s' "$p"
      return
    fi
  done
  echo "ERROR: could not find a free ${label} port near ${preferred}." >&2
  exit 1
}
# Chosen at the point of use, not here. A port picked at startup and published
# minutes later is a real race on a developer box, not a theoretical one: this
# gate spends several minutes building images first, and a dev stack started in
# that window took 3128 out from under a run that had already decided to use it.

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT/deploy"

# Scratch space for the logs this script captures. A fixed /tmp path would be
# both world-readable and predictable on a shared build host: the migrator log
# captured below carries the stack's own rotation output, and a symlink planted
# at a known path in advance redirects a root-run gate wherever the planter
# chooses. mktemp -d gives an unguessable name; 0700 keeps its contents to this
# user.
SMOKE_TMP="$(mktemp -d "${TMPDIR:-/tmp}/nexus-smoke.XXXXXXXX")"
chmod 700 "$SMOKE_TMP"
MIGRATOR_RERUN_LOG="$SMOKE_TMP/second-migrator-run.log"

cleanup() {
  echo "==> [smoke] tearing down"
  docker compose logs --no-color --tail 100 > "$SMOKE_TMP/compose-logs.txt" 2>&1 || true
  # --profile compliance is load-bearing here: `down` leaves a profile-gated
  # service alone, and --remove-orphans does not catch it either (it is a
  # declared service, not an orphan). Without it the compliance-proxy this
  # script starts survives the run under `restart: unless-stopped`, holds host
  # port 3128, and the NEXT run's `up -d compliance-proxy` finds an identical
  # config hash and reuses it — so the arm would assert "running" about a
  # container that never started against this run's database, and would scan a
  # log carrying the previous run's errors.
  docker compose --profile compliance down -v --remove-orphans >/dev/null 2>&1 || true
  echo "    captured logs: $SMOKE_TMP"
}
trap cleanup EXIT

rm -f .env
# A script variable, not just a prefix on the call below: three checks read it
# back (the rotated password authenticates, the token exchange, the admin API
# call). A prefix assignment is scoped to that one command, so anything reading
# $ADMIN_PASSWORD afterwards would silently send an empty password — and the
# checks that matter most here would be asserting nothing.
ADMIN_PASSWORD=smoke-password-not-a-secret
export ADMIN_PASSWORD
./init-secrets.sh >/dev/null
# Point the stack at the registry and version under test. The compose file
# reads both from .env, so rewriting them here is what makes --registry local
# actually run the locally-built images.
sed -i.bak "s#^NEXUS_VERSION=.*#NEXUS_VERSION=${VERSION}#" .env
sed -i.bak "s#^NEXUS_REGISTRY=.*#NEXUS_REGISTRY=${REGISTRY}#" .env
rm -f .env.bak

# Teardown first, not only on exit. The trap below runs `down -v`, but a
# previous run that was SIGKILLed never reached it, and a surviving
# migrator-deps volume would make this run skip the dependency install — the
# one path that touches the npm registry and the one every operator hits on a
# fresh deployment. Silently testing the warm path instead is worse than
# testing nothing, because the gate still reports PASS.
docker compose --profile compliance down -v --remove-orphans >/dev/null 2>&1 || true

[ -n "$UI_PORT" ]      || UI_PORT="$(pick_port 8080 UI)"
[ -n "$GATEWAY_PORT" ] || GATEWAY_PORT="$(pick_port 3050 gateway)"
# Exported rather than written into .env: a shell variable outranks .env in
# Compose's resolution, and init-secrets.sh owns that file.
export NEXUS_UI_PORT="$UI_PORT" NEXUS_GATEWAY_PORT="$GATEWAY_PORT"
UI="http://localhost:${UI_PORT}"
GW="http://localhost:${GATEWAY_PORT}"

echo "==> [smoke] starting stack ($REGISTRY, $VERSION) — ui ${UI_PORT}, gateway ${GATEWAY_PORT}"
docker compose up -d --quiet-pull

echo "==> [smoke] waiting for the control plane to report healthy..."
ok=false
for _ in $(seq 1 90); do
  if curl -fsS ${UI}/healthz >/dev/null 2>&1; then ok=true; break; fi
  sleep 2
done
if [ "$ok" != true ]; then
  echo "ERROR: control plane never became healthy through the UI proxy" >&2
  docker compose ps
  exit 1
fi
echo "    /healthz OK"

echo "==> [smoke] checking the migrator completed cleanly..."
migrator_exit="$(docker compose ps -a --format json db-migrator | sed -n 's/.*"ExitCode":\([0-9-]*\).*/\1/p' | head -1)"
if [ "${migrator_exit:-1}" != "0" ]; then
  echo "ERROR: db-migrator exited with ${migrator_exit:-unknown}; dependent services would never start" >&2
  docker compose logs db-migrator
  exit 1
fi
echo "    migrator exit 0"

# Half of the install-once assertion; the other half is the reuse check after
# the second pass below. Checked separately because together they are the only
# thing that distinguishes "installed, then reused" — the property shipping a
# thin image depends on — from "never had to install at all", which is what a
# leftover warm volume produces and which passes the reuse check on its own.
echo "==> [smoke] checking the first migrator pass installed its dependencies..."
# Captured first, then matched with a here-string. `cmd | grep -q` is unsafe
# under `set -o pipefail`: grep -q exits the moment it matches, the producer
# takes SIGPIPE, and the pipeline reports 141 — so the check fails precisely
# when it should pass.
migrator_first_log="$(docker compose logs db-migrator 2>&1 || true)"
if ! grep -q "installing dependencies (first run for this lockfile" <<<"$migrator_first_log"; then
  echo "ERROR: the first migrator pass never installed dependencies into the migrator-deps volume." >&2
  echo "       Either the teardown above left a warm volume behind (so this run proves nothing about" >&2
  echo "       the fresh-install path an operator hits), or the image is no longer shipping thin." >&2
  docker compose logs db-migrator >&2
  exit 1
fi
echo "    first pass installed into the dependency volume"

echo "==> [smoke] checking the ai-gateway audit spool is writable..."
# The spool is the disk half of the audit pipeline's no-loss promise. Its
# directory lives on a named volume, and a volume mounted at a path the image
# does not already own is created root-owned — which a distroless :nonroot
# service cannot write. That failure is a startup WARN and nothing else: the
# gateway keeps serving, having silently downgraded from `spillblock` to
# `block`, so only an assertion here can catch it.
gw_log="$(docker compose logs ai-gateway 2>&1 || true)"
if grep -q "create spool directory" <<<"$gw_log"; then
  echo "ERROR: ai-gateway could not create its audit spool directory — the audit spill is disabled." >&2
  grep -m 2 "create spool directory" <<<"$gw_log" >&2
  exit 1
fi
# Positive half: the directory is actually there, read from the volume itself
# (the service image is distroless, so there is no shell to look with).
gw_spool="$(docker run --rm -v nexus-gateway_gateway-audit-spool:/v alpine:3 ls /v 2>/dev/null || true)"
if [ -z "$gw_spool" ]; then
  echo "ERROR: the ai-gateway audit-spool volume is empty — the service never created its spool directory." >&2
  exit 1
fi
echo "    ai-gateway audit spool present (${gw_spool})"

echo "==> [smoke] checking the SPA is served..."
# Same capture-then-match shape as the migrator check below: under `set -o
# pipefail`, `curl | grep -q` can report the pipeline as failed because grep
# exited first and curl took SIGPIPE, which turns a passing assertion into a
# release-blocking flake that only shows up on a slow response.
spa_html="$(curl -fsS ${UI}/ || true)"
if ! grep -q '<div id="root"' <<<"$spa_html"; then
  echo "ERROR: the UI did not serve the SPA shell" >&2
  exit 1
fi
echo "    SPA OK"

echo "==> [smoke] starting a PKCE authorization-code flow to get a valid authctx..."
# POST /authserver/password is not a bare credential check — it is the second
# leg of the control plane's OAuth authorization-code + PKCE flow, and it
# 400s with authctx_expired unless the request body carries an authctx minted
# by a prior GET /oauth/authorize for a registered client (see
# packages/control-plane/internal/identity/authserver/login/password.go and
# .../oauth/authorize.go). "cp-ui" plus one of its seeded localhost redirect
# URIs (tools/db-migrate/seed/fixtures/OAuthClient.json) is the only client
# this script can drive without a browser. PKCE is mandatory for every client
# (S256-only, enforced unconditionally by AuthorizeHandler), so a
# verifier/challenge pair is generated even though nothing here ever
# exchanges the resulting authorization code at /oauth/token.
CLIENT_ID="cp-ui"
# MUST be the origin this stack actually serves the console on, because that is
# what a browser sends: the SPA builds redirect_uri from window.location.origin.
# It must be derived from the published port, never written as a literal. The
# seed fixture registers the `npm run dev` origin (localhost:3000), so a
# hardcoded value there would authenticate over a URI no real user of this
# deployment can produce — and pass while the published console cannot be logged
# into at all, since /oauth/authorize answers 400 invalid_request for an
# unregistered redirect_uri.
CONSOLE_ORIGIN="${UI}"
REDIRECT_URI="${CONSOLE_ORIGIN}/auth/callback"
CODE_VERIFIER="$(openssl rand -hex 32)"
CODE_CHALLENGE="$(printf '%s' "$CODE_VERIFIER" | openssl dgst -sha256 -binary | openssl base64 | tr '+/' '-_' | tr -d '=')"
STATE="$(openssl rand -hex 16)"

authorize_headers="$(curl -sS -D - -o /dev/null -G "${UI}/oauth/authorize" \
  --data-urlencode "response_type=code" \
  --data-urlencode "client_id=${CLIENT_ID}" \
  --data-urlencode "redirect_uri=${REDIRECT_URI}" \
  --data-urlencode "code_challenge=${CODE_CHALLENGE}" \
  --data-urlencode "code_challenge_method=S256" \
  --data-urlencode "state=${STATE}")"

authctx="$(printf '%s' "$authorize_headers" | grep -i '^location:' | grep -oE 'authctx=[^&[:space:]]+' | head -1 | cut -d= -f2 || true)"
if [ -z "$authctx" ]; then
  echo "ERROR: /oauth/authorize did not hand back an authctx; response headers were:" >&2
  printf '%s\n' "$authorize_headers" >&2
  exit 1
fi
echo "    authctx acquired"

echo "==> [smoke] checking an unregistered console origin is still refused..."
# Guards the fix for the above from degrading into "register everything". The
# console callback is registered by the migrator for THIS deployment's origin
# only; an arbitrary origin must still be refused, or an attacker-chosen
# redirect_uri could carry an authorization code off-site.
rogue_authorize_headers="$(curl -sS -D - -o /dev/null -G "${CONSOLE_ORIGIN}/oauth/authorize" \
  --data-urlencode "response_type=code" \
  --data-urlencode "client_id=${CLIENT_ID}" \
  --data-urlencode "redirect_uri=http://nexus-smoke-rogue.invalid/auth/callback" \
  --data-urlencode "code_challenge=${CODE_CHALLENGE}" \
  --data-urlencode "code_challenge_method=S256" \
  --data-urlencode "state=${STATE}")"
rogue_authctx="$(printf '%s' "$rogue_authorize_headers" | grep -i '^location:' | grep -oE 'authctx=[^&[:space:]]+' | head -1 | cut -d= -f2 || true)"
if [ -n "$rogue_authctx" ]; then
  echo "ERROR: /oauth/authorize minted an authctx for an UNREGISTERED redirect_uri — redirect_uri validation is not enforcing the registered set." >&2
  echo "       This deployment must not be published: an attacker-chosen redirect_uri can carry an authorization code off-site." >&2
  printf '%s\n' "$rogue_authorize_headers" >&2
  exit 1
fi
echo "    unregistered origin refused"

echo "==> [smoke] checking the public seed password was rotated out..."
# The single most important check in this script. The seed ships admin@nexus.ai
# with the password "nexus-demo", public in the OSS repository. If the migrator's
# rotation silently failed, every published deployment would accept it.
stale_code="$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST ${UI}/authserver/password \
  -H 'Content-Type: application/json' \
  -d "{\"authctx\":\"${authctx}\",\"email\":\"admin@nexus.ai\",\"password\":\"nexus-demo\"}")"
# Assert exactly 401 (PasswordHandler's invalid_credentials response — see
# packages/control-plane/internal/identity/authserver/login/password.go),
# not merely "not 200". A 429 (rate-limited) or 500 (internal error) is not
# proof of rejection and must not be relabelled as one.
if [ "$stale_code" != "401" ]; then
  echo "ERROR: the public seed password 'nexus-demo' was not rejected with 401 (got $stale_code) — credential rotation did not take effect, or the check itself is broken." >&2
  echo "       This deployment must not be published." >&2
  exit 1
fi
echo "    public seed password rejected ($stale_code)"

echo "==> [smoke] checking the generated admin password works..."
# PasswordHandler authenticates the email/password pair BEFORE it ever looks
# at authctx, so the rejected attempt above never reached (and therefore
# never consumed) the pending authorize entry — the same authctx is still
# valid for this second, successful call.
fresh_code="$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST ${UI}/authserver/password \
  -H 'Content-Type: application/json' \
  -d "{\"authctx\":\"${authctx}\",\"email\":\"admin@nexus.ai\",\"password\":\"${ADMIN_PASSWORD}\"}")"
if [ "$fresh_code" != "200" ]; then
  echo "ERROR: the generated admin password does not authenticate ($fresh_code) — the deployment would be unusable." >&2
  exit 1
fi
echo "    generated admin password accepted"

echo "==> [smoke] checking the issued token is ACCEPTED by the admin API..."
# The password check above proves a credential, not a usable console. Between
# them sit two steps a browser takes and this script did not: exchanging the
# authorization code for an access token, and calling the admin API with it.
#
# Everything in that gap can fail while every assertion above still passes. The
# control plane verifies admin tokens against a JWKS it FETCHES, and the
# address it fetches from defaults to the issuer — the origin a browser uses,
# which in a container deployment need not resolve to the control plane at all.
# When it does not, tokens are minted correctly and rejected on arrival: sign-in
# succeeds, /api/admin/me answers 401, the SPA restarts the flow, and the
# console cannot be signed into by anyone. Asserting only the password step
# reports PASS on exactly that deployment.
#
# So finish the flow the way the SPA does and call one authenticated endpoint.
#
# On a FRESH authorize, not the authctx used above: unlike the rejected
# attempt, the successful password login consumed that pending entry, and
# reusing it answers authctx_expired — which would fail this check for a
# reason that has nothing to do with what it is meant to prove. The code
# verifier must be the one belonging to this authorize too, since /oauth/token
# checks the PKCE challenge minted with it.
flow_verifier="$(openssl rand -hex 32)"
flow_challenge="$(printf '%s' "$flow_verifier" | openssl dgst -sha256 -binary | openssl base64 | tr '+/' '-_' | tr -d '=')"
flow_headers="$(curl -sS -D - -o /dev/null -G "${UI}/oauth/authorize" \
  --data-urlencode "response_type=code" \
  --data-urlencode "client_id=${CLIENT_ID}" \
  --data-urlencode "redirect_uri=${REDIRECT_URI}" \
  --data-urlencode "code_challenge=${flow_challenge}" \
  --data-urlencode "code_challenge_method=S256" \
  --data-urlencode "state=$(openssl rand -hex 16)")"
flow_authctx="$(printf '%s' "$flow_headers" | grep -i '^location:' | grep -oE 'authctx=[^&[:space:]]+' | head -1 | cut -d= -f2 || true)"
if [ -z "$flow_authctx" ]; then
  echo "ERROR: /oauth/authorize did not hand back an authctx for the token-acceptance check." >&2
  exit 1
fi
login_body="$(curl -sS -X POST ${UI}/authserver/password \
  -H 'Content-Type: application/json' \
  -d "{\"authctx\":\"${flow_authctx}\",\"email\":\"admin@nexus.ai\",\"password\":\"${ADMIN_PASSWORD}\"}")"
# The code arrives in a JSON redirectUri, not a Location header — the SPA
# performs the redirect itself. Match the code's own character set rather than
# "everything up to the next &": the JSON escapes that separator as \u0026,
# whose own characters are all alphanumeric, so a negated-& class runs straight
# past it and carries "\u0026state=..." into the code. That corruption does not
# surface here — it surfaces one step later as an invalid_grant from
# /oauth/token, which reads like a broken authorization server.
auth_code="$(printf '%s' "$login_body" | sed -n 's/.*[?&]code=\([A-Za-z0-9_-]*\).*/\1/p')"
if [ -z "$auth_code" ]; then
  echo "ERROR: no authorization code in the password response; body was: $login_body" >&2
  exit 1
fi
token_body="$(curl -sS -X POST ${UI}/oauth/token \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode "grant_type=authorization_code" \
  --data-urlencode "code=${auth_code}" \
  --data-urlencode "redirect_uri=${REDIRECT_URI}" \
  --data-urlencode "client_id=${CLIENT_ID}" \
  --data-urlencode "code_verifier=${flow_verifier}")"
access_token="$(printf '%s' "$token_body" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')"
if [ -z "$access_token" ]; then
  echo "ERROR: /oauth/token issued no access_token; body was: $token_body" >&2
  echo "       The code it was given: '${auth_code}' (an invalid_grant here usually" >&2
  echo "       means that value is not exactly what the password response returned)." >&2
  exit 1
fi
me_code="$(curl -s -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer ${access_token}" ${UI}/api/admin/me)"
if [ "$me_code" != "200" ]; then
  echo "ERROR: /api/admin/me answered ${me_code} for a token this deployment just issued." >&2
  echo "       Sign-in works and the console is still unusable: the SPA reads this 401 as" >&2
  echo "       'not signed in' and returns to the login form. Check that the control plane" >&2
  echo "       can reach its own JWKS address (AUTH_SERVER_JWKS_URL) from inside its" >&2
  echo "       container — the issuer-derived default is the browser's origin." >&2
  exit 1
fi
echo "    admin API accepted the issued token (${me_code})"

echo "==> [smoke] checking the gateway answers on its published port..."
code="$(curl -s -o /dev/null -w '%{http_code}' ${GW}/v1/models)"
# requireVK (packages/ai-gateway/internal/ingress/models/models.go) has no
# code path that returns 200 without a valid virtual key: vkAuth.Authenticate
# always fails on a missing key and the handler unconditionally writes 401.
# 403 is also accepted because it is reachable via a connection-stage hook
# reject. 200 here would mean VK admission enforcement regressed and the full
# model catalog is exposed to any anonymous caller — a hard failure, not a
# "maybe". A connection failure or 5xx is also not proof the gateway is up.
case "$code" in
  200) echo "ERROR: gateway returned 200 with no virtual key — VK admission enforcement is disabled, exposing the full model catalog to anyone." >&2; exit 1 ;;
  401|403) echo "    gateway responded $code" ;;
  *) echo "ERROR: gateway returned $code (expected 401/403)" >&2; exit 1 ;;
esac

echo "==> [smoke] checking the system-assistant virtual key was rotated..."
# The db-migrator rotates TWO seeded credentials, not just the admin
# password: the system-assistant Virtual Key. The bootstrap seed ships it
# with a deterministic plaintext, public in the OSS repository —
# assistantVkKey(id) = "nvk_local_" + id.slice(0, 8), for
# ASSISTANT_VK_ID = "b0075000-0000-4000-8000-0000000000a2"
# (tools/db-migrate/seed/bootstrap/index.ts) — and entrypoint.sh's
# `UPDATE "VirtualKey" ...` is supposed to overwrite that row's hash with
# NEXUS_ASSISTANT_SYSTEM_VK. Nothing above this point actually proves that
# UPDATE took effect: a wrong id, a renamed column, or a future schema
# change could make it silently affect zero rows, and this script would
# still reach here and print PASS while publishing a key any GitHub reader
# can derive.
#
# Both halves of this pair are required together, exactly like the password
# pair above: the REJECT check alone would pass just as well against a dead
# /v1/models route (nothing is ever accepted, rotated or not), and the
# ACCEPT check alone would pass even if the old public key were also still
# valid (rotation silently never happened). Only the pair proves rotation
# actually occurred.
ASSISTANT_VK_ID="b0075000-0000-4000-8000-0000000000a2"
STALE_ASSISTANT_VK="nvk_local_${ASSISTANT_VK_ID:0:8}"
# Split the grep from the cut rather than piping them: under `set -euo
# pipefail`, "grep ... | cut ..." aborts the whole script the instant grep
# finds no match (pipefail propagates grep's exit 1 out of the pipeline,
# and set -e then kills the script inside the command substitution) — before
# the "not found in .env" guard below ever gets a chance to print its
# friendlier diagnostic. `|| true` on the grep alone neutralizes that so a
# non-match falls through to the guard instead of a bare pipefail abort.
ASSISTANT_VK_LINE="$(grep '^NEXUS_ASSISTANT_SYSTEM_VK=' .env || true)"
ASSISTANT_VK="${ASSISTANT_VK_LINE#*=}"
if [ -z "$ASSISTANT_VK" ]; then
  echo "ERROR: NEXUS_ASSISTANT_SYSTEM_VK was not found in .env — cannot check virtual-key rotation." >&2
  exit 1
fi

stale_vk_code="$(curl -s -o /dev/null -w '%{http_code}' \
  ${GW}/v1/models \
  -H "Authorization: Bearer ${STALE_ASSISTANT_VK}")"
if [ "$stale_vk_code" != "401" ]; then
  echo "ERROR: the public seed system-assistant key still authenticates ($stale_vk_code) — virtual-key rotation did not take effect." >&2
  echo "       This deployment must not be published." >&2
  exit 1
fi
echo "    public seed assistant key rejected ($stale_vk_code)"

fresh_vk_code="$(curl -s -o /dev/null -w '%{http_code}' \
  ${GW}/v1/models \
  -H "Authorization: Bearer ${ASSISTANT_VK}")"
if [ "$fresh_vk_code" != "200" ]; then
  echo "ERROR: the generated system-assistant key does not authenticate ($fresh_vk_code) — the system assistant would be unusable." >&2
  exit 1
fi
echo "    generated assistant key accepted ($fresh_vk_code)"

echo "==> [smoke] checking demo-tier secrets were rotated (users, virtual keys, admin API keys)..."
# docker/db-migrator/rotate-demo-secrets.mjs re-stamps every demo NexusUser /
# VirtualKey / AdminApiKey row with a plaintext derived from
# (ADMIN_KEY_HMAC_SECRET, row id) instead of the seed's own public formula.
# Everything above this point only proves the TWO bootstrap credentials
# (admin password, system-assistant VK) rotated — this is the check that
# would have caught the original defect (12 working virtual keys + a
# super-admin API key left public on a fresh quickstart).
#
# Recompute the rotated value by running the SAME script the migrator ran,
# against the SAME already-built image, rather than re-implementing the HMAC
# derivation a second time in bash — any drift between this check and the
# migrator's own math would make the check meaningless.
DEMO_SECRET="$(grep '^ADMIN_KEY_HMAC_SECRET=' .env | cut -d= -f2-)"
demo_rotate() { # kind id
  # --entrypoint overrides the image's ENTRYPOINT (entrypoint.sh, which
  # requires DATABASE_URL and would otherwise treat "node ..." as arguments
  # appended to itself rather than a replacement command).
  docker run --rm \
    -e ADMIN_KEY_HMAC_SECRET="$DEMO_SECRET" -e ROTATE_KIND="$1" -e ROTATE_ID="$2" \
    --entrypoint node \
    "${REGISTRY}/db-migrator:${VERSION}" \
    /opt/nexus-scripts/rotate-demo-secret.js
}

# Stale (public) values, per tools/db-migrate/seed/demo/index.ts:
#   demoAdminKey(id) = "nak_demo_" + id.slice(0, 8)
#   demoVkKey(id)    = "nvk_demo_" + id.slice(0, 8)
# for AdminApiKey "super-admin" (id a5e8d4b2-d8fd-487f-a830-6a3437c715d0, a
# tools/db-migrate/seed/fixtures/demo/IamGroupMembership.json member of the
# super-admins IAM group) and VirtualKey "demo01" (id
# 0c101489-c080-4384-9b00-90ecedf6adc7, tools/db-migrate/seed/fixtures/demo/VirtualKey.json).
DEMO_SUPER_ADMIN_KEY_ID="a5e8d4b2-d8fd-487f-a830-6a3437c715d0"
STALE_DEMO_ADMIN_KEY="nak_demo_a5e8d4b2"
STALE_DEMO_VK="nvk_demo_0c101489"

stale_demo_admin_code="$(curl -s -o /dev/null -w '%{http_code}' \
  ${UI}/api/admin/me -H "X-Nexus-Admin-Key: ${STALE_DEMO_ADMIN_KEY}")"
if [ "$stale_demo_admin_code" != "401" ]; then
  echo "ERROR: the public demo super-admin key '${STALE_DEMO_ADMIN_KEY}' was not rejected with 401 (got $stale_demo_admin_code) — demo AdminApiKey rotation did not take effect." >&2
  echo "       This deployment must not be published." >&2
  exit 1
fi
echo "    public demo admin key rejected ($stale_demo_admin_code)"

stale_demo_vk_code="$(curl -s -o /dev/null -w '%{http_code}' \
  ${GW}/v1/models -H "Authorization: Bearer ${STALE_DEMO_VK}")"
if [ "$stale_demo_vk_code" != "401" ]; then
  echo "ERROR: the public demo virtual key '${STALE_DEMO_VK}' was not rejected with 401 (got $stale_demo_vk_code) — demo VirtualKey rotation did not take effect." >&2
  echo "       This deployment must not be published." >&2
  exit 1
fi
echo "    public demo virtual key rejected ($stale_demo_vk_code)"

echo "==> [smoke] checking the public demo login (alice@nexus.ai / nexus-demo) was rejected..."
stale_demo_login_code="$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST ${UI}/authserver/password \
  -H 'Content-Type: application/json' \
  -d "{\"authctx\":\"${authctx}\",\"email\":\"alice@nexus.ai\",\"password\":\"nexus-demo\"}")"
if [ "$stale_demo_login_code" != "401" ]; then
  echo "ERROR: the public demo password 'nexus-demo' for alice@nexus.ai was not rejected with 401 (got $stale_demo_login_code) — demo NexusUser rotation did not take effect." >&2
  echo "       This deployment must not be published." >&2
  exit 1
fi
echo "    public demo login rejected ($stale_demo_login_code)"

echo "==> [smoke] checking a rotated demo user password actually logs in..."
# The reject check above (alice@nexus.ai / nexus-demo) only proves the
# PUBLIC password stopped working — it would pass just as well if rotation
# had broken the account entirely (e.g. a bad UPDATE that zeroed the hash).
# Only an ACCEPT with the freshly rotated password proves the demo tier's
# NexusUser rows are still usable, mirroring the admin-key / virtual-key
# pairing below. Recompute the SAME rotated password the migrator wrote by
# rerunning the per-row derivation script against the SAME secret, rather
# than reimplementing the HMAC math a second time here.
if ! rotated_user_line="$(demo_rotate password nexus-user-admin)"; then
  echo "ERROR: the rotate-demo-secret helper failed for the demo user password." >&2
  exit 1
fi
ROTATED_USER_PASSWORD="$(printf '%s' "$rotated_user_line" | cut -f1)"
if [ -z "$ROTATED_USER_PASSWORD" ]; then
  echo "ERROR: could not re-derive the rotated demo user password; the helper produced no output." >&2
  exit 1
fi

# The authctx acquired above was already consumed by the "generated admin
# password works" check (PasswordHandler's d.Pending.Take only succeeds once
# per authctx, on its one successful call) — mint a fresh one for this
# second login attempt.
user_authorize_headers="$(curl -sS -D - -o /dev/null -G "${UI}/oauth/authorize" \
  --data-urlencode "response_type=code" \
  --data-urlencode "client_id=${CLIENT_ID}" \
  --data-urlencode "redirect_uri=${REDIRECT_URI}" \
  --data-urlencode "code_challenge=${CODE_CHALLENGE}" \
  --data-urlencode "code_challenge_method=S256" \
  --data-urlencode "state=${STATE}")"
user_authctx="$(printf '%s' "$user_authorize_headers" | grep -i '^location:' | grep -oE 'authctx=[^&[:space:]]+' | head -1 | cut -d= -f2 || true)"
if [ -z "$user_authctx" ]; then
  echo "ERROR: could not mint a fresh authctx for the rotated demo user login check; response headers were:" >&2
  printf '%s\n' "$user_authorize_headers" >&2
  exit 1
fi

fresh_demo_user_code="$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST ${UI}/authserver/password \
  -H 'Content-Type: application/json' \
  -d "{\"authctx\":\"${user_authctx}\",\"email\":\"alice@nexus.ai\",\"password\":\"${ROTATED_USER_PASSWORD}\"}")"
if [ "$fresh_demo_user_code" != "200" ]; then
  echo "ERROR: the rotated demo user password does not authenticate ($fresh_demo_user_code) — demo NexusUser rotation would leave the demo tier unusable." >&2
  exit 1
fi
echo "    rotated demo user password accepted ($fresh_demo_user_code)"

echo "==> [smoke] checking a rotated demo admin key + virtual key actually authenticate..."
# The reject checks above alone would pass just as well against a dead route
# (nothing is ever accepted, rotated or not) — only pairing them with an
# ACCEPT proves the rotation produced WORKING credentials, not merely broken
# ones. This is also the only place that proves the demo tier stayed usable,
# per the maintainer's explicit choice to keep it rather than delete it.
# Captured before the `read`, because `read <<<"$(cmd)"` reports READ's status,
# not the command substitution's: a `docker run` that failed would leave the
# variables empty and be reported below as a misleading 401.
# Both arms are needed: a bare `line="$(cmd)"` under `set -e` aborts on a
# non-zero helper before any check runs, and a helper that exits 0 with no
# output would otherwise leave the variables empty and be reported below as a
# misleading 401.
if ! rotated_admin_line="$(demo_rotate admin-key a5e8d4b2-d8fd-487f-a830-6a3437c715d0)"; then
  echo "ERROR: the rotate-demo-secret helper failed for the demo super-admin key." >&2
  exit 1
fi
if [ -z "$rotated_admin_line" ]; then
  echo "ERROR: could not re-derive the rotated demo super-admin key; the helper produced no output." >&2
  exit 1
fi
IFS=$'\t' read -r ROTATED_ADMIN_KEY ROTATED_ADMIN_HASH ROTATED_ADMIN_PREFIX <<<"$rotated_admin_line"
fresh_demo_admin_code="$(curl -s -o /dev/null -w '%{http_code}' \
  ${UI}/api/admin/me -H "X-Nexus-Admin-Key: ${ROTATED_ADMIN_KEY}")"
if [ "$fresh_demo_admin_code" != "200" ]; then
  echo "ERROR: the rotated demo super-admin key does not authenticate ($fresh_demo_admin_code) — the demo tier would be unusable." >&2
  exit 1
fi
echo "    rotated demo admin key accepted ($fresh_demo_admin_code)"

if ! rotated_vk_line="$(demo_rotate virtual-key 0c101489-c080-4384-9b00-90ecedf6adc7)"; then
  echo "ERROR: the rotate-demo-secret helper failed for the demo virtual key." >&2
  exit 1
fi
if [ -z "$rotated_vk_line" ]; then
  echo "ERROR: could not re-derive the rotated demo virtual key; the helper produced no output." >&2
  exit 1
fi
IFS=$'\t' read -r ROTATED_VK ROTATED_VK_HASH ROTATED_VK_PREFIX <<<"$rotated_vk_line"
fresh_demo_vk_code="$(curl -s -o /dev/null -w '%{http_code}' \
  ${GW}/v1/models -H "Authorization: Bearer ${ROTATED_VK}")"
if [ "$fresh_demo_vk_code" != "200" ]; then
  echo "ERROR: the rotated demo virtual key does not authenticate ($fresh_demo_vk_code) — the demo tier would be unusable." >&2
  exit 1
fi
echo "    rotated demo virtual key accepted ($fresh_demo_vk_code)"

echo "==> [smoke] checking a second migrator run leaves an operator-created credential untouched..."
# Regression test for the defect this branch's PART 1 fixes. An earlier,
# EXCLUSION-based version of rotate-demo-secrets.mjs ("every source='local'
# NexusUser except the bootstrap admin", "every VirtualKey except the
# assistant VK", "every AdminApiKey") equals "only the demo rows" ONLY on a
# first boot against an empty database — db-migrator has no "already ran"
# gate and runs on EVERY `docker compose up` (the documented upgrade path is
# `docker compose pull && docker compose up -d`), and admin-API-created
# users default to source='local' just like the demo rows. Under the
# exclusion rule, a SECOND rotation pass would silently overwrite the
# passwordHash of any user an operator created after the first boot.
#
# Simulate that: insert an operator-style NexusUser directly against the
# running database (bypassing the admin API, but with a passwordHash
# computed the SAME scrypt scheme the running services verify one under),
# rerun db-migrator a second time exactly as an upgrade would, and assert
# the credential still authenticates. A regression back to exclusion-based
# selection would silently overwrite this row's passwordHash and this check
# would start failing with 401.
OPERATOR_ORG_ID="b0075000-0000-4000-8000-0000000000a0" # bootstrap's Default Organization — always present, independent of SEED_DEMO
OPERATOR_EMAIL="smoke-operator@nexus.invalid"
OPERATOR_PASSWORD="operator-password-$(openssl rand -hex 8)"

# Compute the passwordHash the running services would independently verify
# against, using the SAME scrypt parameters tools/db-migrate/seed/lib.ts
# hashPassword() / packages/control-plane/internal/identity/authn/password.go
# use — reusing the image's own Node runtime rather than requiring one on
# the host, exactly like demo_rotate above.
OPERATOR_PASSWORD_HASH="$(docker run --rm \
  -e OPERATOR_PASSWORD="$OPERATOR_PASSWORD" \
  --entrypoint node \
  "${REGISTRY}/db-migrator:${VERSION}" \
  -e '
    const { scryptSync, randomBytes } = require("crypto");
    const SCRYPT_OPTIONS = { N: 1 << 17, r: 8, p: 1, maxmem: 256 * 1024 * 1024 };
    const salt = randomBytes(32);
    const hash = scryptSync(process.env.OPERATOR_PASSWORD, salt, 64, SCRYPT_OPTIONS);
    process.stdout.write(salt.toString("hex") + ":" + hash.toString("hex"));
  ')"

POSTGRES_USER_ENV="$(grep '^POSTGRES_USER=' .env | cut -d= -f2-)"
POSTGRES_DB_ENV="$(grep '^POSTGRES_DB=' .env | cut -d= -f2-)"

docker compose exec -T postgres psql -U "$POSTGRES_USER_ENV" -d "$POSTGRES_DB_ENV" -v ON_ERROR_STOP=1 -c "
  INSERT INTO \"NexusUser\"
    (id, \"organizationId\", \"displayName\", email, status, \"canAccessControlPlane\", source, \"passwordHash\", \"passwordUpdatedAt\", \"createdAt\", \"updatedAt\")
  VALUES
    (gen_random_uuid()::text, '${OPERATOR_ORG_ID}', 'Smoke Operator', '${OPERATOR_EMAIL}', 'active', true, 'local', '${OPERATOR_PASSWORD_HASH}', now(), now(), now());
" >/dev/null
echo "    inserted operator-style NexusUser (${OPERATOR_EMAIL})"

echo "==> [smoke] checking an operator's OWN 'nexus-demo' password (non-demo org) is not swept by rotation..."
# F-4.1 regression test. rotate-demo-secrets.mjs's NexusUser predicate used
# to be password-verification ALONE ("stored passwordHash verifies against
# the public plaintext DEMO_PASSWORD"). That check alone cannot distinguish
# a genuine demo row from an operator who deliberately set their OWN
# account's password to the literal string "nexus-demo" — nothing stops an
# operator from doing that, and the resulting hash verifies identically
# either way. The fix adds an org-membership condition: demo NexusUser rows
# only ever live in demo orgs (fixture ids "10000000-*" / "5fabaad6-...",
# derived at runtime from tools/db-migrate/seed/fixtures/demo/
# Organization.json — see rotate-demo-secrets.mjs loadDemoOrgIds()), so a row
# in the bootstrap org (never a demo org) must be left alone regardless of
# what its password verifies against.
#
# Insert a NexusUser in the BOOTSTRAP org (not a demo org) whose password is
# the literal collision string "nexus-demo", swept by the SAME second
# migrator run as the operator-survival check above, then assert after that
# run that "nexus-demo" still authenticates for this row — proving the org
# condition, not just the password condition, is what saved it.
OPERATOR_COLLISION_EMAIL="smoke-operator-collision@nexus.invalid"
OPERATOR_COLLISION_PASSWORD_HASH="$(docker run --rm \
  --entrypoint node \
  "${REGISTRY}/db-migrator:${VERSION}" \
  -e '
    const { scryptSync, randomBytes } = require("crypto");
    const SCRYPT_OPTIONS = { N: 1 << 17, r: 8, p: 1, maxmem: 256 * 1024 * 1024 };
    const salt = randomBytes(32);
    const hash = scryptSync("nexus-demo", salt, 64, SCRYPT_OPTIONS);
    process.stdout.write(salt.toString("hex") + ":" + hash.toString("hex"));
  ')"

docker compose exec -T postgres psql -U "$POSTGRES_USER_ENV" -d "$POSTGRES_DB_ENV" -v ON_ERROR_STOP=1 -c "
  INSERT INTO \"NexusUser\"
    (id, \"organizationId\", \"displayName\", email, status, \"canAccessControlPlane\", source, \"passwordHash\", \"passwordUpdatedAt\", \"createdAt\", \"updatedAt\")
  VALUES
    (gen_random_uuid()::text, '${OPERATOR_ORG_ID}', 'Smoke Operator (nexus-demo collision)', '${OPERATOR_COLLISION_EMAIL}', 'active', true, 'local', '${OPERATOR_COLLISION_PASSWORD_HASH}', now(), now(), now());
" >/dev/null
echo "    inserted operator-style NexusUser with password literally 'nexus-demo' in the bootstrap (non-demo) org (${OPERATOR_COLLISION_EMAIL})"

echo "==> [smoke] making the demo rows look operator-owned before the rerun..."
# The demo tier is sample data a deployment OWNS once it has it: an operator
# revokes a demo key, or pastes a real provider key into one of the pre-wired
# "*-prod" Credential rows. The seed used to send every non-id column as the
# update payload on every run, which reverted both — the revoked key came back
# enabled, and the real provider key was replaced by the fixture placeholder,
# unrecoverably. Stamp both states here and assert them after the rerun.
docker compose exec -T postgres psql -U "$POSTGRES_USER_ENV" -d "$POSTGRES_DB_ENV" -v ON_ERROR_STOP=1 -c "
  UPDATE \"Credential\" SET \"encryptedKey\" = 'operator-pasted-provider-key' WHERE name = 'openai-prod';
  UPDATE \"AdminApiKey\" SET enabled = false WHERE id = '${DEMO_SUPER_ADMIN_KEY_ID}';
" >/dev/null
echo "    openai-prod credential carries an operator value; demo super-admin key disabled"

echo "==> [smoke] rerunning db-migrator (second pass, simulating an upgrade's 'docker compose pull && docker compose up -d')..."
# `docker compose run` (not `up`) — the db-migrator SERVICE already exited
# after the first pass and `up -d db-migrator` alone would find nothing
# changed and skip re-running it; `run` always starts a fresh container from
# the same image/env, which is what an upgrade's `up -d` does once the image
# reference itself changes.
if ! docker compose run --rm db-migrator > "$MIGRATOR_RERUN_LOG" 2>&1; then
  echo "ERROR: db-migrator's second run failed; log:" >&2
  cat "$MIGRATOR_RERUN_LOG" >&2
  exit 1
fi
echo "    second migrator run exit 0"

# The migrator image ships thin and installs its dependencies on first run into
# the `migrator-deps` volume, keyed on the lockfile's hash. That "install once,
# reuse thereafter" property is the entire point of shipping thin, and losing it
# is SILENT — a regression just reinstalls the whole dependency tree on every
# `up`, which still exits 0 and still passes every other check here. So assert
# both halves directly: the first pass installed (checked above, right after the
# migrator exit code), and this second pass against the unchanged lockfile
# skipped.
echo "==> [smoke] checking the migrator reused its dependency volume instead of reinstalling..."
if ! grep -q "dependencies already present for this lockfile" "$MIGRATOR_RERUN_LOG"; then
  echo "ERROR: the second migrator run did not report reusing the migrator-deps volume." >&2
  echo "       Expected 'dependencies already present for this lockfile' — the lockfile did not change" >&2
  echo "       between the two runs, so the install must have been skipped. Either the volume is not" >&2
  echo "       mounted (check deploy/docker-compose.yml's db-migrator volumes:) or the lockfile-hash" >&2
  echo "       marker at node_modules/.deps-lock-hash is not being written. Second-run log:" >&2
  cat "$MIGRATOR_RERUN_LOG" >&2
  exit 1
fi
echo "    second run reused the dependency volume (no reinstall)"

echo "==> [smoke] checking the operator credential survived the second migrator run..."
operator_authorize_headers="$(curl -sS -D - -o /dev/null -G "${UI}/oauth/authorize" \
  --data-urlencode "response_type=code" \
  --data-urlencode "client_id=${CLIENT_ID}" \
  --data-urlencode "redirect_uri=${REDIRECT_URI}" \
  --data-urlencode "code_challenge=${CODE_CHALLENGE}" \
  --data-urlencode "code_challenge_method=S256" \
  --data-urlencode "state=${STATE}")"
operator_authctx="$(printf '%s' "$operator_authorize_headers" | grep -i '^location:' | grep -oE 'authctx=[^&[:space:]]+' | head -1 | cut -d= -f2 || true)"
if [ -z "$operator_authctx" ]; then
  echo "ERROR: could not mint a fresh authctx for the operator-credential-survival check; response headers were:" >&2
  printf '%s\n' "$operator_authorize_headers" >&2
  exit 1
fi

operator_login_code="$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST ${UI}/authserver/password \
  -H 'Content-Type: application/json' \
  -d "{\"authctx\":\"${operator_authctx}\",\"email\":\"${OPERATOR_EMAIL}\",\"password\":\"${OPERATOR_PASSWORD}\"}")"
if [ "$operator_login_code" != "200" ]; then
  echo "ERROR: the operator-created credential no longer authenticates after a second migrator run ($operator_login_code) — demo-secret rotation is unscoped and destroyed an operator-created row." >&2
  echo "       This deployment must not be published." >&2
  exit 1
fi
echo "    operator-created credential survived a second migrator run ($operator_login_code)"

echo "==> [smoke] checking the operator's own 'nexus-demo' password (non-demo org) survived the second migrator run..."
# The pair to the insert above: a REJECT-only check here would be
# meaningless (this row's plaintext password IS "nexus-demo" — rejecting it
# would just mean the row got destroyed, not preserved). Only an ACCEPT
# proves the org-scoping condition, not the password-verification condition
# alone, is what determined this row was left untouched.
collision_authorize_headers="$(curl -sS -D - -o /dev/null -G "${UI}/oauth/authorize" \
  --data-urlencode "response_type=code" \
  --data-urlencode "client_id=${CLIENT_ID}" \
  --data-urlencode "redirect_uri=${REDIRECT_URI}" \
  --data-urlencode "code_challenge=${CODE_CHALLENGE}" \
  --data-urlencode "code_challenge_method=S256" \
  --data-urlencode "state=${STATE}")"
collision_authctx="$(printf '%s' "$collision_authorize_headers" | grep -i '^location:' | grep -oE 'authctx=[^&[:space:]]+' | head -1 | cut -d= -f2 || true)"
if [ -z "$collision_authctx" ]; then
  echo "ERROR: could not mint a fresh authctx for the operator-nexus-demo-collision check; response headers were:" >&2
  printf '%s\n' "$collision_authorize_headers" >&2
  exit 1
fi

collision_login_code="$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST ${UI}/authserver/password \
  -H 'Content-Type: application/json' \
  -d "{\"authctx\":\"${collision_authctx}\",\"email\":\"${OPERATOR_COLLISION_EMAIL}\",\"password\":\"nexus-demo\"}")"
if [ "$collision_login_code" != "200" ]; then
  echo "ERROR: an operator's own account with password literally 'nexus-demo' in a NON-demo org no longer authenticates after a second migrator run ($collision_login_code) — NexusUser rotation is not org-scoped and rotated an operator's own credential out from under them." >&2
  echo "       This deployment must not be published." >&2
  exit 1
fi
echo "    operator's own 'nexus-demo' password (non-demo org) survived a second migrator run ($collision_login_code)"

echo "==> [smoke] checking the rerun left operator-owned demo rows alone..."
surviving_cred="$(docker compose exec -T postgres psql -U "$POSTGRES_USER_ENV" -d "$POSTGRES_DB_ENV" -tAc \
  "SELECT \"encryptedKey\" FROM \"Credential\" WHERE name = 'openai-prod'" | tr -d '\r')"
if [ "$surviving_cred" != "operator-pasted-provider-key" ]; then
  echo "ERROR: the second migrator run overwrote the openai-prod credential (now: ${surviving_cred:-<empty>})." >&2
  echo "       A real provider key an operator pasted into that row would be gone, with no copy anywhere." >&2
  exit 1
fi
echo "    operator-entered provider key survived the rerun"

surviving_enabled="$(docker compose exec -T postgres psql -U "$POSTGRES_USER_ENV" -d "$POSTGRES_DB_ENV" -tAc \
  "SELECT enabled FROM \"AdminApiKey\" WHERE id = '${DEMO_SUPER_ADMIN_KEY_ID}'" | tr -d '\r')"
if [ "$surviving_enabled" != "f" ]; then
  echo "ERROR: the second migrator run re-enabled the demo super-admin API key (enabled=${surviving_enabled:-<empty>})." >&2
  echo "       A super-admin credential an operator revoked would authenticate again on :8080/api." >&2
  exit 1
fi
echo "    revoked demo super-admin key stayed revoked"

echo "==> [smoke] bringing up the compliance profile..."
# The profile ships as one of the six published images and gets latest/-avx2 and
# the Docker Hub mirror after this gate passes, but nothing here ever started it
# — so a compliance-proxy that cannot read its CA, or cannot find MQ_DRIVER,
# reached every operator with a green release. Starting it is the whole check:
# it exits at startup on a CA it cannot read, and `restart: unless-stopped`
# turns that into a loop rather than a visible failure.
[ -n "$PROXY_PORT" ] || PROXY_PORT="$(pick_port 3128 proxy)"
export NEXUS_PROXY_PORT="$PROXY_PORT"
PROXY="http://localhost:${PROXY_PORT}"
echo "    publishing the proxy on ${PROXY_PORT}"

# --force-recreate so this arm cannot pass on a container left over from an
# earlier run: Compose would otherwise reuse one whose config hash matches.
docker compose --profile compliance up -d --quiet-pull --force-recreate compliance-proxy
cp_up=false
for _ in $(seq 1 20); do
  cp_state="$(docker compose ps --format json compliance-proxy 2>/dev/null || true)"
  if grep -q '"State":"running"' <<<"$cp_state"; then cp_up=true; break; fi
  sleep 2
done
if [ "$cp_up" != true ]; then
  echo "ERROR: compliance-proxy never reached the running state under --profile compliance." >&2
  docker compose logs compliance-proxy >&2
  exit 1
fi
# Running once is not enough: a crash loop passes through "running" on every
# restart. Assert it is still running after a settle window, and that nothing in
# its log is a CA-read failure.
sleep 5
cp_state="$(docker compose ps --format json compliance-proxy 2>/dev/null || true)"
if ! grep -q '"State":"running"' <<<"$cp_state"; then
  echo "ERROR: compliance-proxy started and then stopped — it is crash-looping." >&2
  docker compose logs compliance-proxy >&2
  exit 1
fi
# Matched against the issuer's own error text
# (packages/compliance-proxy/internal/tls/issuer/issuer.go: "cert: read CA cert
# %s" / "cert: read CA key %s") rather than a generic "permission denied": the
# container legitimately logs an unrelated EACCES warning when the audit
# NDJSON fallback cannot create its spool directory, and a pattern loose enough
# to catch that is a gate that fails on a healthy stack.
cp_log="$(docker compose logs compliance-proxy 2>&1 || true)"
if grep -qE "cert: read CA (cert|key)" <<<"$cp_log"; then
  echo "ERROR: compliance-proxy could not read its CA — deploy/compliance-ca is not readable to the container uid (65532)." >&2
  docker compose logs compliance-proxy >&2
  exit 1
fi
# And it must actually answer on the port it publishes. 127.0.0.1:9 is the
# discard port: whatever the proxy answers (a 4xx/5xx envelope, or a refusal to
# connect upstream) proves it is listening and serving; curl exit 7 would mean
# nothing accepted the connection at all.
# `cmd; status=$?` never reaches the second line under `set -e` when the first
# one fails — which is every case this check exists for, so the diagnostic below
# was unreachable and a non-7 exit killed the gate with no message at all.
curl_status=0
curl_err="$(curl -sS -m 10 -o /dev/null -x ${PROXY} http://127.0.0.1:9/ 2>&1)" || curl_status=$?
# 7 (connection refused) is the only verdict meaning "nothing is serving". A
# timeout, an empty reply or a proxy-side error all prove something answered —
# but which one it was is worth printing rather than discarding — which is what
# pairing curl -S with 2>/dev/null would do.
if [ "$curl_status" = 7 ]; then
  echo "ERROR: nothing is listening on the compliance proxy port 3128 (curl exit 7)." >&2
  docker compose logs compliance-proxy >&2
  exit 1
fi
if grep -q "create spool directory" <<<"$cp_log"; then
  echo "ERROR: compliance-proxy could not create its NDJSON audit spool — its spillblock policy degrades to block, so a full channel now back-pressures intercepted TLS connections." >&2
  grep -m 2 "create spool directory" <<<"$cp_log" >&2
  exit 1
fi
cp_spool="$(docker run --rm -v nexus-gateway_proxy-audit-spool:/v alpine:3 ls /v 2>/dev/null || true)"
if [ -z "$cp_spool" ]; then
  echo "ERROR: the compliance-proxy audit-spool volume is empty — the service never created its spool directory." >&2
  exit 1
fi
echo "    compliance-proxy audit spool present (${cp_spool})"

echo "    compliance profile came up and answers on :3128 (curl exit ${curl_status}${curl_err:+: $curl_err})"

echo "==> [smoke] PASS"
