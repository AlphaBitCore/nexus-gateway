# API Authentication

Nexus Gateway uses three independent authentication mechanisms, each scoped to a different API surface. Admin UI and CLI callers use OAuth 2.0 + PKCE to acquire a short-lived RS256 bearer token. AI traffic clients authenticate with a virtual key, a long-lived bearer secret prefixed `nvk_`. Internal services authenticate peer-to-peer using a shared `INTERNAL_SERVICE_TOKEN` (Hub API) or a per-service bearer token (Things API). No surface accepts more than one of these credential types. Sending the wrong credential type to a surface returns `401`.

---

## Admin OAuth+PKCE flow

The Control Plane runs a local OAuth 2.0 authorization server. The browser SPA and CLI helpers both use PKCE (`code_challenge_method=S256`) — there is no client secret. Bearer token is the only supported admin-API credential; cookie-based auth is not available.

### Endpoints

| Endpoint | Purpose |
|---|---|
| `GET /oauth/authorize` | Start a new auth flow; mints an opaque `authctx`, redirects to the login page |
| `POST /oauth/token` | Exchange auth code → access token (grant: `authorization_code`) or rotate a refresh token (grant: `refresh_token`) |
| `POST /oauth/revoke` | RFC 7009 token revocation |
| `POST /oauth/introspect` | RFC 7662 token introspection |
| `GET /.well-known/jwks.json` | RS256 public keys for signature verification |
| `GET /.well-known/openid-configuration` | OIDC discovery document |

Login-page helpers (consumed by the SPA, not by callers directly):

| Endpoint | Purpose |
|---|---|
| `GET /authserver/idps` | List enabled identity providers for the active `authctx` |
| `POST /authserver/password` | Authenticate against the local (Nexus) IdP |
| `GET /authserver/oidc/begin` | Initiate an external IdP (Okta / Azure AD) federation flow |
| `GET /authserver/oidc/callback` | Receive the external IdP's authorization-code callback |

### Flow — browser SPA

```mermaid
sequenceDiagram
    participant Browser
    participant SPA as Control Plane UI
    participant AS as Authserver (CP :3001)
    participant IdP as External IdP (optional)

    Browser->>AS: GET /oauth/authorize?code_challenge=...
    AS-->>Browser: 302 → /login?authctx=<id>
    Browser->>SPA: render login page
    SPA->>AS: GET /authserver/idps?authctx=<id>
    AS-->>SPA: [local, okta, ...]
    SPA->>AS: POST /authserver/password (local) OR navigate to IdP
    AS-->>SPA: { redirectUri: "?code=...&state=..." }
    SPA->>AS: POST /oauth/token (code + code_verifier)
    AS-->>SPA: { access_token, refresh_token, expires_in: 3600 }
    SPA->>SPA: cache token; use in Authorization header
```

### Flow — CLI / scripts

`cp_login` (in `tests/lib/auth.sh`) automates the PKCE exchange against `tests/.env.local` credentials and caches the token at `/tmp/nexus_test_token`:

```bash
source tests/lib/auth.sh
cp_login               # drives PKCE; caches token
cp_curl /api/admin/routing-rules
```

### Token shape

Access tokens are RS256-signed JWTs with these claims:

```
{
  "iss": "https://nexus.<tenant>/",
  "sub": "<user-id>",
  "aud": "<client-id>",
  "scope": "admin",
  "iat", "exp",        // default 1-hour expiry
  "jti",
  "session_id": "<sid>"
}
```

The Control Plane verifies the signature against its own JWKS. Every admin API request passes through `Verifier.Verify` which checks algorithm (RS256 only), signature, time window, issuer, audience, non-empty `sub`, and revocation status.

### Refresh token rotation

Refresh tokens are opaque (not JWT) and rotate on each use. The token server detects refresh-token reuse (same opaque value submitted twice) as a potential credential leak: it revokes the entire session chain and publishes a revocation event so all outstanding access tokens from that session are invalidated. Refresh tokens expire after 24 hours by default.

### Revocation

`POST /oauth/revoke` persists a revocation record in Postgres and publishes it on the NATS revocation topic. The `MQRevocationChecker` inside the verifier consumes these events into an in-memory set that is consulted on every verify call. Redis is not in the revocation path.

---

## Virtual key auth — AI traffic

A virtual key (`nvk_...`) is the credential for the AI Gateway's `/v1/*` endpoints. It is a long-lived bearer secret created by an admin in the Control Plane UI. The gateway resolves the virtual key by hashing the secret and looking up the `hashed_secret` column — the plaintext is never stored.

### Sending a virtual key

Two forms are accepted (the `x-nexus-virtual-key` header is checked first):

```http
x-nexus-virtual-key: nvk_abc123...
```

or

```http
Authorization: Bearer nvk_abc123...
```

### What a virtual key encodes

After successful resolution, the request context carries:

- `OrganizationID`, `ProjectID` — scope for quota and audit
- `allowed_models`, `allowed_providers` — optional allowlists; requests outside the allowlist receive `403`
- `quota_policy_id` — budget limit, rate limit, enforcement mode

### Error codes

| Error | Meaning |
|---|---|
| `VIRTUAL_KEY_MISSING` | No VK in either header |
| `VIRTUAL_KEY_INVALID` | VK not found in the database |
| `VIRTUAL_KEY_DISABLED` | VK has been revoked |
| `VIRTUAL_KEY_EXPIRED` | VK past its `expires_at` date |

---

## Internal-service token auth — Hub API

The Hub API (`/api/hub/*`) uses a shared symmetric secret: `INTERNAL_SERVICE_TOKEN`. Both the Control Plane and Nexus Hub load this token from their environment files. The token is sent as a bearer token in the `Authorization` header. It is never stored in YAML and must match exactly between the two services (`[MUST MATCH]` in `.env.example`).

The Things API (`/api/internal/things/*`) uses per-service bearer tokens. Server services (AI Gateway, Compliance Proxy) use a service token provisioned during initial registration. Agent devices use a device token provisioned during the agent enrollment ceremony (mTLS + enrollment token).

Neither of these token types flows through the OAuth endpoints.

---

## Auth per surface — summary

| Surface | Credential type | Where to obtain |
|---|---|---|
| `POST /v1/chat/completions` (and all `/v1/*` AI traffic) | Virtual key (`nvk_...`) | Admin UI → AI Gateway → Virtual Keys |
| `GET /api/admin/*` | OAuth+PKCE bearer (RS256 JWT) | `/oauth/authorize` flow |
| `POST /api/hub/*` | `INTERNAL_SERVICE_TOKEN` | Set via environment variable on CP and Hub |
| `POST /api/internal/things/*` | Per-service bearer token | Provisioned during service registration / agent enrollment |
| `GET /v1/models` | None | Public endpoint, no auth required |

---

## Canonical docs

- [`oauth-pkce-admin-auth-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/oauth-pkce-admin-auth-architecture.md) — PKCE flow mechanics, token shape, revocation, key rotation
- [`jwt-verifier-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/jwt-verifier-architecture.md) — RS256 verification, JWKS cache, multi-issuer federation
- [`credentials-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/credentials-architecture.md) — virtual key model, resolution logic, rotation policy
- [`authserver-login.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/auth/authserver-login.yaml) — SPA-facing interactive login endpoints

**Adjacent wiki pages**: [API-Overview](API-Overview) · [API-AI-Gateway](API-AI-Gateway) · [API-Admin](API-Admin) · [API-Hub](API-Hub) · [Control-Plane-Authentication](Control-Plane-Authentication) · [AI-Gateway-Virtual-Keys-Quotas](AI-Gateway-Virtual-Keys-Quotas)
