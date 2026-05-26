# Control Plane Authentication

*Audience: contributors and operators who need to understand or implement admin authentication.*

The Control Plane runs a local OAuth 2.0 authorization server with PKCE. It issues short-lived bearer tokens to the admin UI and to CLI helpers such as `cp_login`. Bearer token authentication is the only admin-API auth path — no cookie-based sessions.

---

## OAuth+PKCE flow

PKCE (Proof Key for Code Exchange, S256) is required because the admin UI is a public client (browser SPA) with no safe place to store a client secret. CLI helpers like `cp_login` are also public clients.

The flow:

1. Client (browser or CLI) generates `code_verifier` (random 43-128 chars) and `code_challenge = base64url(sha256(code_verifier))`.
2. Client redirects user to `GET /oauth/authorize?response_type=code&code_challenge=...&code_challenge_method=S256&scope=admin&state=...`.
3. User authenticates — either against a Nexus Local account at `POST /authserver/password`, or via an external IdP through `GET /authserver/oidc/begin` (see [Control Plane SSO Okta AzureAD](Control-Plane-SSO-Okta-AzureAD)).
4. Authorization server issues an authorization code (one-shot, 5-minute TTL), bound to `code_challenge`.
5. Client POSTs to `/oauth/token` with `grant_type=authorization_code` + `code` + `code_verifier` + `client_id`.
6. Server verifies `sha256(code_verifier) == code_challenge`, marks code used, returns access token + refresh token.

Token lifetimes (default):

- **Access token** (RS256 JWT): 1 hour (`defaultAccessTTL = time.Hour`).
- **Refresh token** (opaque): 24 hours (`defaultRefreshTTL = 24 * time.Hour`).

## Authorization server endpoints

Mounted by `packages/control-plane/internal/identity/authserver/mount.go`:

| Endpoint | Method | Purpose |
|---|---|---|
| `/oauth/authorize` | GET | Start an auth flow |
| `/oauth/token` | POST | Exchange authorization code or refresh token for an access token |
| `/oauth/introspect` | POST | RFC 7662 token introspection |
| `/oauth/revoke` | POST | RFC 7009 token revocation |
| `/.well-known/jwks.json` | GET | Public JWKS for RS256 verification |
| `/.well-known/openid-configuration` | GET | OIDC discovery document |
| `/authserver/oidc/begin` | GET | Begin OIDC federation with an external IdP |
| `/authserver/oidc/callback` | GET | Receive the IdP's authorization-code callback |
| `/authserver/password` | POST | Nexus Local username+password authentication |
| `/authserver/idps` | GET | List configured IdPs for the login screen |

There is no `/oauth/refresh` endpoint — refresh-token exchange goes through `/oauth/token` with `grant_type=refresh_token` per RFC 6749 §6.

## Bearer token shape

```
header:  { alg: "RS256", kid: "<key-id>" }
payload: {
  iss: "https://nexus.<tenant>/",
  sub: "<user-id>",
  aud: "<client-id>",
  scope: "admin",
  iat, exp, jti,
  session_id: "<sid>"
}
```

The Control Plane verifies tokens via `packages/control-plane/internal/identity/jwt/`. The same verifier handles both locally issued tokens and tokens derived from external IdPs.

## Refresh tokens and rotation

Refresh tokens are opaque (not JWT) and stored in Postgres. The `RefreshHelper` rotates them on each use:

- `Rotate(ctx, raw, ttl)` issues a new token and invalidates the parent.
- If the same opaque value is submitted twice (token reuse), `Rotate` returns `ErrReplay`, which triggers deletion of the entire session chain and publishes a session-scoped revocation event.
- Outstanding access tokens are invalidated via the `MQRevocationChecker` in `identity/jwt/mqrevocation.go` — this is not a Redis lookup; it consumes revocation events from the MQ into an in-memory set.

## Logout and revocation

- **UI sign-out** — client clears its local token cache.
- **Server-side revocation** — `POST /oauth/revoke` (RFC 7009) persists a revocation event and publishes on the MQ revocation topic. The JWT verifier picks up these events and rejects the token on the next request.
- **Refresh chain revocation** — revoking one token in a chain (`RefreshStore.DeleteBySessionID`) invalidates all descendants.

## CLI helper

The canonical CLI entry point:

```bash
source tests/lib/auth.sh
cp_login                     # idempotent; caches token at /tmp/nexus_test_token
cp_curl /api/admin/...       # uses cached bearer
```

`cp_login` performs a localhost-loopback PKCE flow using the admin credentials in `tests/.env.local`.

## Nexus Local fallback

Nexus Local accounts (super-admin bootstrap, break-glass admins) remain usable even when an external IdP is misconfigured or unreachable. Local accounts authenticate through `POST /authserver/password`; passwords are bcrypt-hashed and never logged.

---

## Canonical docs

- [`oauth-pkce-admin-auth-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/oauth-pkce-admin-auth-architecture.md) — PKCE flow, token shapes, refresh rotation, revocation
- [`idp-sso-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/idp-sso-architecture.md) — external IdP federation (the alternative to step 3)

**Adjacent wiki pages**: [Control Plane Overview](Control-Plane-Overview) · [Control Plane IAM Model](Control-Plane-IAM-Model) · [Control Plane SSO Okta AzureAD](Control-Plane-SSO-Okta-AzureAD) · [First Admin Login](First-Admin-Login) · [Trust Boundaries](Trust-Boundaries)
