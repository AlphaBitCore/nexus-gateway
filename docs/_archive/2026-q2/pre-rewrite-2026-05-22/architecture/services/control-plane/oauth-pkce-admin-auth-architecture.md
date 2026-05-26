---
doc: oauth-pkce-admin-auth-architecture
area: service
service: control-plane
tier: 1
---

# OAuth+PKCE Admin Auth Architecture

> **Tier 2 architecture doc.** Sister doc to `idp-sso-architecture.md`. Read when touching `packages/control-plane/internal/identity/authserver/`, `packages/shared/identity/pkce/`, or the bearer-token issuance flow. The external-IdP-federated path is in `idp-sso-architecture.md` §4; this doc is the **local authorization server** mechanics.

Nexus Control Plane runs a local OAuth 2.0 authorization server with PKCE. It issues short-lived bearer tokens to the admin UI (and to CLI helpers like `cp_login`). Bearer is the only admin-API auth.

---

## 1. The local AS surfaces

Mounted by `packages/control-plane/internal/identity/authserver/mount.go` (the `/oauth/*` block sits around lines 137-229; the `/authserver/*` block follows). No `/api/admin/` prefix; the OAuth + OIDC discovery endpoints live at the well-known top-level paths required by the specs:

| Endpoint | Method | Purpose |
|---|---|---|
| `/oauth/authorize` | GET | Start an auth flow; redirect to login UI or IdP. |
| `/oauth/token` | POST | Exchange auth code OR refresh token for a fresh access token (`grant_type=authorization_code` or `grant_type=refresh_token`). |
| `/oauth/introspect` | POST | RFC 7662 token introspection (used by sibling services + the MQ revocation checker). |
| `/oauth/revoke` | POST | RFC 7009 token revocation. |
| `/oauth/device-binding` | POST | Bind a device cert to a pending OAuth flow (agent enrollment path; requires mTLS). |
| `/.well-known/jwks.json` | GET | JWKS for bearer-token signature verification (RS256). |
| `/.well-known/openid-configuration` | GET | OIDC discovery doc. |
| `/authserver/oidc/begin` | GET | Begin OIDC SSO flow against a configured external IdP. |
| `/authserver/oidc/callback` | GET | Receive the IdP's authorization-code callback. |
| `/authserver/password` | POST | Nexus Local username+password authentication (depends on the seeded local IdP). |
| `/authserver/idps` | GET | List configured IdPs for the login screen. |

The AS lives in `packages/control-plane/internal/identity/authserver/`. There is **no separate `/oauth/refresh` endpoint** — refresh-token exchange goes through `/oauth/token` with `grant_type=refresh_token` per RFC 6749 §6.

## 2. PKCE flow (S256)

1. Client (browser / CLI) generates `code_verifier` (random 43-128 chars) + `code_challenge = base64url(sha256(code_verifier))`.
2. Client redirects user to `/oauth/authorize?response_type=code&code_challenge=...&code_challenge_method=S256&scope=admin&state=...`.
3. User authenticates — Nexus Local credentials at `/authserver/password` OR external IdP via `/authserver/oidc/begin` (cross-ref `idp-sso-architecture.md`).
4. AS issues an authorisation code (one-shot, **5-minute** TTL — `authCodeTTL = 5 * time.Minute` in `login/password.go:16`), bound to `code_challenge`.
5. Client POSTs to `/oauth/token` with `grant_type=authorization_code` + `code` + `code_verifier` + `client_id`.
6. AS verifies `sha256(code_verifier) == code_challenge`, marks code used, returns:
   - `access_token` (RS256 JWT, default **1 hour** expiry — `defaultAccessTTL = time.Hour`, `oauth/token.go:29`).
   - `refresh_token` (opaque, default **24 hours** expiry — `defaultRefreshTTL = 24 * time.Hour`, `oauth/token.go:30`).
   - `token_type: "Bearer"`.
   - `expires_in: 3600`.

Per-client overrides can be persisted via `ClientStore.AccessTTLSeconds` / `RefreshTTLSeconds`, but most clients run on defaults.

PKCE primitives live in `packages/shared/identity/pkce/`.

## 3. Why PKCE (no client secret)

The admin UI is a public client (browser SPA) — it has no safe place to store a client secret. PKCE proves the token-exchange caller is the same party that started the flow, **without** a pre-shared secret. CLI helpers (`cp_login`) use the same flow; they're public clients too.

## 4. Bearer token shape

```
header:    { alg: "RS256", kid: "<key-id>" }
payload:   {
  iss: "https://nexus.<tenant>/",
  sub: "<user-id>",
  aud: "<client-id>",
  scope: "admin",
  iat, exp, jti,
  session_id: "<sid>",
  ...
}
signature: RS256(header.payload, private_key)
```

The Control Plane verifies its own bearer tokens via `packages/control-plane/internal/identity/jwt/` (same code path that verifies external-IdP tokens; see `jwt-verifier-architecture.md`).

## 5. Refresh tokens

Refresh tokens are **opaque** (not JWT) — stored in Postgres with rotation on each use. The helper is `token.RefreshHelper` (`identity/authserver/token/refresh.go`):

- `NewChain(ctx, userID, clientID, deviceID, ttl)` mints a new chain on initial token exchange.
- `Rotate(ctx, raw, ttl)` rotates a refresh token, issuing a new one and invalidating the parent.
- Refresh token **reuse** (same opaque value submitted twice) signals leak: `Rotate` returns `ErrReplay`; `Mount`'s replay hook then deletes the whole session chain (`refresh.DeleteBySessionID`) and publishes a session-scoped revocation event so the JWT verifier's `MQRevocationChecker` invalidates outstanding access tokens.
- Refresh tokens are scoped to the same user + client + (optional) device id.

This is the OAuth "refresh token rotation" pattern with rotated-token-reuse detection.

## 6. CLI helper integration (`tests/lib/auth.sh`)

The canonical CLI entry point for testing / scripting:

```bash
source tests/lib/auth.sh
cp_login                                    # idempotent; caches at /tmp/nexus_test_token
cp_curl /api/admin/...                      # uses cached bearer
```

`cp_login` performs a localhost-loopback PKCE flow with the admin credentials in `tests/.env.local`. The token is cached on disk so subsequent `cp_curl` calls reuse it until expiry.

## 7. Logout / revocation

- **UI sign-out**: client clears local token cache.
- **Server-side revocation**: `POST /oauth/revoke` (RFC 7009) records a revocation event via `revocation.Service.Revoke(...)` in `identity/authserver/revocation/service.go`, persisting to Postgres and publishing on the MQ revocation topic.
- **Verifier-side enforcement**: `MQRevocationChecker` in `identity/jwt/mqrevocation.go` consumes those events into an in-memory set that `Verifier.Verify` consults on every request. Cold checks can fall back to `/oauth/introspect`. **This is not a Redis SISMEMBER lookup** — Redis is not in the revocation path.
- **Refresh chain revocation**: a single chain compromise revokes all descendants via `RefreshStore.DeleteBySessionID` plus a session-scoped revocation event.

## 8. Token rotation under key compromise

If the AS's signing keypair is compromised:

1. Generate a new keypair.
2. Add the new public key to JWKS with a new `kid`.
3. Continue signing new tokens with the new key.
4. Keep the old public key in JWKS until all in-flight tokens expire (~1h).
5. After expiry window, remove the old key from JWKS.

`packages/shared/identity/rstokenauth/` (RS256 token issue/verify) supports multi-kid JWKS rotation.

<!-- 💡 harvest: nothing new — PKCE flow is contained; no new rule/skill candidate. Existing skill `prod-login` exercises the production version of this flow. -->

## 9. Sources

- `packages/control-plane/internal/identity/authserver/` — local AS implementation.
- `packages/control-plane/internal/identity/authserver/mount.go` — endpoint mount authority.
- `packages/control-plane/internal/identity/authserver/oauth/token.go` — token-grant handler + TTL defaults.
- `packages/control-plane/internal/identity/authserver/token/` — access-token signer + refresh-token rotation helper.
- `packages/control-plane/internal/identity/authserver/revocation/` — revocation publisher + service + store.
- `packages/shared/identity/pkce/` — code-verifier / challenge primitives.
- `packages/shared/identity/rstokenauth/` — RS256 token issue/verify.
- `packages/control-plane/internal/identity/jwt/` — verifier for issued + IdP-federated tokens.
- `tests/lib/auth.sh` — CLI helper.
- `.claude/skills/prod-login/` — production OAuth+PKCE flow runbook.

## 10. Cross-references

- `idp-sso-architecture.md` — external IdP federation path (replaces step 3 above).
- `jwt-verifier-architecture.md` — bearer + ID-token verification mechanics.
- `iam-identity-architecture.md` — what bearer authorises once authenticated.
- `audit-pipeline-architecture.md` — login / token-rotation events emit admin-audit.
