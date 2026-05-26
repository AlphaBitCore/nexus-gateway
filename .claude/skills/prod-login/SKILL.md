---
name: prod-login
description: >
  Log into the production Control Plane (https://nexus.taskforce10x.com) via the
  real OAuth + PKCE flow using the seeded super-admin account
  (admin@nexus.ai / admin123), cache the bearer token in an isolated cache file,
  and expose ready-to-call helpers (`cp_login`, `cp_curl`, `cp_curl_code`,
  `cp_curl_full`) for any subsequent prod admin API call. Use whenever you need
  to read or write prod CP state (virtual keys, providers, routing rules,
  analytics, IAM policies, etc.) from this workstation. Trigger keywords:
  prod login, login to prod, prod CP login, prod admin login, prod cp_curl,
  prod API call, /prod-login.
user-invocable: true
---

# Prod Login

Drives the real prod OAuth + PKCE login (no x-admin-key bypass) so middleware
that runs only on user sessions still gets exercised, then leaves you with
working `cp_curl` / `cp_curl_code` / `cp_curl_full` helpers.

The underlying script is `tests/lib/auth.sh` (the same one used for local
tests); this skill sources `tests/lib/loadenv.sh prod` first so every prod
session uses identical config from `tests/.env.prod`.

## When to use

- User says "login to prod", "prod cp_curl", "call prod CP API", `/prod-login`.
- Any step that needs to read prod admin state from this workstation: virtual
  keys, providers, credentials, routing rules, analytics summaries, IAM
  policies, jobs, kill switch, etc.
- As a prerequisite for `/smoke-gateway` against prod, prod deploy/debug
  workflows, or one-shot admin API calls.

## Prod env contract

All values live in `tests/.env.prod` (gitignored, operator-customised — copy
from `tests/.env.prod.example` and fill in the real prod admin password if it
ever rotates away from the seeded `admin123`). The loader (`tests/lib/loadenv.sh`)
fails closed if `NEXUS_CP_URL` is not a real hostname when target=prod, so a
half-configured `.env.prod` won't silently fall back to localhost.

Token cache lands at `/tmp/nexus_token_prod` — derived automatically from the
target so prod and local tokens never cross-pollute (no manual `NEXUS_TOKEN_CACHE`
override needed).

## How to log in (one-liner)

```bash
bash -c 'source tests/lib/loadenv.sh prod && source tests/lib/auth.sh && cp_login && cp_login_check && echo "prod login OK"'
```

`cp_login` is idempotent and caches at `/tmp/nexus_token_prod`; `cp_login_check`
round-trips `/api/admin/providers` and exits 0 only if the token actually
authenticates. Re-running the one-liner is safe — it auto-refreshes if the
token has expired.

## How to call any prod admin endpoint

After (or together with) login:

```bash
bash -c 'source tests/lib/loadenv.sh prod && source tests/lib/auth.sh && cp_login && cp_curl "/api/admin/<path>"'
```

For non-GET methods, pass `-X` and the body via `-d` after the path:

```bash
bash -c 'source tests/lib/loadenv.sh prod && source tests/lib/auth.sh && cp_login && \
  cp_curl /api/admin/routing-rules -X POST \
    -H "Content-Type: application/json" \
    -d "$(cat /tmp/my-rule.json)"'
```

## Common prod admin calls

After `source tests/lib/loadenv.sh prod && source tests/lib/auth.sh && cp_login`:

```bash
# Virtual keys (list)
cp_curl /api/admin/virtual-keys | jq '.[] | {id, name, status, used_usd, quota_usd}'

# Find a specific VK by name
cp_curl /api/admin/virtual-keys | jq '.[] | select(.name=="research all models")'

# Providers + their adapter type
cp_curl /api/admin/providers | jq '.[] | {id, name, adapter_type}'

# Routing rules
cp_curl /api/admin/routing-rules | jq '.[] | {id, name, enabled, priority}'

# IAM policies
cp_curl /api/admin/iam/policies | jq '.[] | {id, name}'

# Cost summary
cp_curl '/api/admin/analytics/cost?groupBy=device' | jq '.'

# Health probes (do NOT need login, but useful)
curl -sS "$NEXUS_CP_URL/healthz"
curl -sS "$NEXUS_CP_URL/ready"
```

## Common failure patterns

| Symptom | Fix |
|---|---|
| `loadenv.sh: target=prod but NEXUS_CP_URL=… is loopback` | `tests/.env.prod` is missing or has localhost placeholders. Copy from `tests/.env.prod.example` and fill in. |
| `cp_login: /oauth/authorize did not return an authctx` | Wrong `NEXUS_OAUTH_REDIRECT_URI` in `tests/.env.prod` — prod's `cp-ui` OAuth client only accepts `https://nexus.taskforce10x.com/auth/callback`. |
| `cp_login: /authserver/password failed` | Wrong creds (admin password rotated?), or the user is locked out. Check `User` table on prod DB. |
| `cp_curl` returns 401 on a known-good endpoint | Token expired; just rerun the one-liner — `cp_token` auto-refreshes. |
| `cp_curl` returns 403 | IAM denial — the super-admin policy is missing a resource grant. See `iam_resource_nrn_bug` memory + `packages/control-plane/internal/identity/iam/managed.go`. |
| `Could not resolve host: nexus.taskforce10x.com` | Network/DNS issue on the workstation; check VPN / Wi-Fi. |
| `gnutls/openssl handshake failed` | Likely an in-progress nginx restart on prod; retry in 5–10s. |

## Token cache hygiene

The cache file at `/tmp/nexus_token_prod` is a plain text file with epoch
expiry on line 1 and the access token on line 2. To force a fresh login:

```bash
rm -f /tmp/nexus_token_prod
```

Never commit this file, and never copy it onto another machine — it grants
super-admin access until expiry.

## Pairs well with

- `prod-debug` — once logged in, the cp_curl helpers complement DB and journalctl probes.
- `prod-deploy` — the deploy skill's Step 7c smoke check uses this same login.
- `smoke-gateway` — when running against prod, invoke with `--target prod`; the smoke script auto-loads `tests/.env.prod`.
