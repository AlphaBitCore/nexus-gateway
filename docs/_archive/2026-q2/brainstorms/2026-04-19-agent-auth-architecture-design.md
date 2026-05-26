# Unified Agent Authentication Architecture — Design

**Date:** 2026-04-19
**Status:** Proposed
**Scope:** Spec A (core auth server + agent + RS integration + CP UI unification)
**Follow-up:** Spec B (ops extraction of auth server to its own process — pure deployment, no code changes)

---

## 1. Goal

Design and ship a unified authentication architecture for the Nexus Gateway platform that:

1. Provides a single **auth server** (hosted inside Control Plane today, extractable tomorrow) that mints JWTs for both agents and the Control Plane UI (SPA).
2. Supports **agent-side auth modes** — `os-only`, `sso-preferred`, `sso-strict` — configurable per fleet via Hub shadow.
3. Treats the **Nexus User table as a first-class local IdP** and unifies it with OIDC / SAML IdPs behind a common IdP abstraction, with JIT provisioning and role mapping.
4. Issues **device-bound, signed JWTs** verifiable by Hub / AI Gateway / Compliance Proxy through a shared verifier using JWKS.
5. Delivers **fast revocation** via RFC 7009 + MQ fan-out + refresh token rotation with replay detection.
6. Unifies **CP UI login** with the same OAuth / PKCE flow used by the agent.
7. Honours the project's **clean-sweep development policy** — no backward compatibility shims, no deferred cleanup, no parallel legacy paths. Old endpoints, tables, and code are deleted in the same PR that replaces them.

## 2. Locked Architectural Decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | Loopback redirect + PKCE (RFC 8252) for agent | Standard for native apps; avoids custom URL scheme hijack |
| 2 | Device-bound JWT via `device_id` claim (not RFC 8705 mTLS-bound) | Works for SPA + agent with one verifier; avoids per-request client cert overhead |
| 3 | Three configurable auth modes: `os-only`, `sso-preferred`, `sso-strict` | Fleet-level policy, switched via Hub shadow without agent restart |
| 4 | Access token 1h, refresh token 24h (rotating) | Balances security vs. friction; acceptable for enterprise fleet |
| 5 | RS256 signatures, JWKS distribution | Public keys to RS; private key stays in auth server |
| 6 | ±5 min clock skew tolerance, server-time offset from `Date` header | No NTP dependency; cross-timezone safe; drift-level alerts |
| 7 | RFC 7009 revocation + MQ push via `nexus.auth.revocation` | p99 < 1 s propagation; bloom-filter hot path on RS |
| 8 | Refresh rotation with replay detection | If stolen refresh token is reused, whole session is revoked |
| 9 | Platform-native secret storage on agent (macOS Keychain / Windows DPAPI / Linux libsecret) | Refresh token never lands on plain filesystem |
| 10 | Hosted login page — agent and SPA never render login forms themselves | Agent / SPA are IdP-agnostic; IdP changes are transparent |
| 11 | Nexus User is source of truth; all IdPs (local / OIDC / SAML) map to `nexus_user.id` | One user identity; multiple federated logins |
| 12 | Auth server is a self-contained package inside CP (`packages/control-plane/internal/authserver/`) with no coupling to admin API / IAM / analytics | Spec B extraction is a deployment change, not a refactor |

## 3. System Components

```
┌────────────────────────────────────────────────────────────────┐
│                        Auth Server (in CP)                     │
│  OAuth/OIDC endpoints · Hosted login page · JWKS · Revocation  │
│  IdP adapters: local · oidc · saml                             │
│                           │                                     │
│          ┌────────────────┼──────────────────┐                 │
│          │                │                  │                 │
│  nexus_user table   identity_provider   oauth_client           │
│  user_federated_identity · refresh_token · revoked_token       │
└──────────┬──────────────────────────────────────────────┬──────┘
           │                                              │
   (1) OAuth flows                          (2) nexus.auth.revocation
           │                                              │
  ┌────────▼─────────┐                         ┌──────────▼────────┐
  │  Agent (desktop) │                         │  Resource Servers │
  │  loopback + PKCE │                         │  Hub / AG / Proxy │
  │  auth mode sm    │                         │  shared/jwtverif. │
  │  platform keychain│                        │  bloom + MQ revoc │
  └──────────────────┘                         └───────────────────┘
  ┌──────────────────┐
  │  CP UI (SPA)     │
  │  silent refresh  │
  │  HttpOnly cookie │
  └──────────────────┘
```

### Service boundaries

- **Auth server** (in CP process) — issues tokens, hosts login page, manages IdPs, publishes revocation events.
- **Agent** — opens system browser to auth server, listens on loopback for callback, stores tokens in OS keychain, manages auth state machine.
- **CP UI (SPA)** — triggers same OAuth flow via redirect; stores access token in memory, refresh token in HttpOnly cookie.
- **Resource servers (Hub / AI Gateway / Compliance Proxy)** — embed `shared/jwtverifier`, verify signature / claims / revocation, attach claims to request context.

## 4. Data Model

All changes land in a single migration that also drops the obsolete session / SSO-config tables.

### 4.1 `nexus_user` — extended

```sql
ALTER TABLE nexus_user
  ADD COLUMN password_updated_at timestamptz,
  ADD COLUMN break_glass boolean NOT NULL DEFAULT false,
  ADD COLUMN disabled_reason text,
  ADD COLUMN disabled_at timestamptz;
-- existing: id, email, password_hash, roles[], created_at, updated_at
```

- `break_glass = true` marks a local admin account that must remain usable even if all SSO IdPs are disabled or broken.
- `disabled_at IS NOT NULL` means the user is blocked; all tokens for them are implicitly revoked.

### 4.2 `identity_provider`

```sql
CREATE TABLE identity_provider (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  type            text NOT NULL CHECK (type IN ('local','oidc','saml')),
  name            text NOT NULL,               -- shown on hosted login page
  enabled         boolean NOT NULL DEFAULT true,
  config          jsonb NOT NULL DEFAULT '{}', -- type-specific (issuer, client_id, client_secret, metadata_url, ...)
  role_mapping    jsonb NOT NULL DEFAULT '[]', -- [{match:"group=admins", role:"platform_admin"}, ...]
  default_role    text NOT NULL DEFAULT 'developer',
  jit_enabled     boolean NOT NULL DEFAULT true,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
```

- Exactly one `type='local'` row is seeded at migration time with `name='Nexus Local'`. Local IdP can be disabled but never deleted.
- `config` JSON schema is validated per-type in the adapter layer.

### 4.3 `user_federated_identity`

```sql
CREATE TABLE user_federated_identity (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id           uuid NOT NULL REFERENCES nexus_user(id) ON DELETE CASCADE,
  idp_id            uuid NOT NULL REFERENCES identity_provider(id) ON DELETE RESTRICT,
  external_subject  text NOT NULL,              -- IdP's immutable sub (for local IdP: nexus_user.id as string)
  external_email    text,
  raw_claims        jsonb,                      -- last seen claims (debug / re-mapping)
  linked_at         timestamptz NOT NULL DEFAULT now(),
  last_login_at     timestamptz,
  UNIQUE (idp_id, external_subject)
);
CREATE INDEX ON user_federated_identity (user_id);
```

- A single `nexus_user` can bind multiple `user_federated_identity` rows (local + Okta + future IdPs).
- JIT provisioning: on first OIDC / SAML login, auth server creates `nexus_user` + `user_federated_identity` atomically.

### 4.4 `oauth_client`

```sql
CREATE TABLE oauth_client (
  id                  text PRIMARY KEY,           -- 'agent-desktop', 'cp-ui'
  name                text NOT NULL,
  type                text NOT NULL CHECK (type IN ('public','confidential')),
  redirect_uris       text[] NOT NULL,            -- loopback patterns for agent; SPA callback for cp-ui
  allowed_scopes      text[] NOT NULL,
  require_pkce        boolean NOT NULL DEFAULT true,
  access_ttl_seconds  int NOT NULL DEFAULT 3600,
  refresh_ttl_seconds int NOT NULL DEFAULT 86400,
  client_secret_hash  text,                       -- confidential clients only
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now()
);
```

- Seeded with `agent-desktop` (public, PKCE-required, loopback) and `cp-ui` (public, PKCE-required, fixed redirect).

### 4.5 `refresh_token`

A `session_id` represents a single login; a refresh-token chain (rotations) all share the same `session_id`. This replaces the otherwise-redundant "family" concept.

```sql
CREATE TABLE refresh_token (
  jti            uuid PRIMARY KEY,
  session_id     uuid NOT NULL,                   -- shared by all rotations of one login
  parent_jti     uuid REFERENCES refresh_token(jti),
  user_id        uuid NOT NULL REFERENCES nexus_user(id) ON DELETE CASCADE,
  client_id      text NOT NULL REFERENCES oauth_client(id),
  device_id      text,                            -- agent only
  token_hash     bytea NOT NULL,                  -- SHA-256 of the opaque refresh token
  used_at        timestamptz,                     -- non-null when rotated; replay check
  expires_at     timestamptz NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON refresh_token (session_id);
CREATE INDEX ON refresh_token (user_id);
CREATE INDEX ON refresh_token (expires_at);
```

### 4.6 `revoked_token`

```sql
CREATE TABLE revoked_token (
  id          bigserial PRIMARY KEY,
  scope       text NOT NULL CHECK (scope IN ('jti','user','device','session')),
  target_jti        uuid,
  target_user_id    uuid,
  target_device_id  text,
  target_session_id uuid,
  revoked_at  timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz NOT NULL,               -- natural expiry of affected tokens; cleanup key
  reason      text NOT NULL,
  actor       text                                -- 'user_logout' | 'admin:<user_id>' | 'replay_detected' | 'unenroll' | 'role_change'
);
CREATE INDEX ON revoked_token (expires_at);
CREATE INDEX ON revoked_token (target_user_id)    WHERE target_user_id    IS NOT NULL;
CREATE INDEX ON revoked_token (target_device_id)  WHERE target_device_id  IS NOT NULL;
CREATE INDEX ON revoked_token (target_session_id) WHERE target_session_id IS NOT NULL;
```

### 4.7 Obsolete structures removed in the same migration

- Any pre-existing `admin_session` / `user_session` table and CP's `/api/admin/auth/login` associated state.
- Any legacy SSO-config table split across `saml_config` / `oidc_config` (merged into `identity_provider`).

## 5. API Surface

### 5.1 OAuth / OIDC protocol endpoints (auth server)

| Endpoint | Method | Purpose |
|---|---|---|
| `/oauth/authorize` | GET | Start auth code flow (expects PKCE `code_challenge` + `state` + `nonce`; agent adds `binding_id`) |
| `/oauth/device-binding` | POST | mTLS pre-flight: bind a `binding_id` to the caller's `device_id` (agent only) |
| `/oauth/token` | POST | Exchange code → JWT; refresh → JWT (rotating) |
| `/oauth/revoke` | POST | RFC 7009 revocation (access or refresh token) |
| `/oauth/introspect` | POST | RFC 7662 introspection (RS fallback when MQ is down) |
| `/.well-known/jwks.json` | GET | Public key set for JWT verification |
| `/.well-known/openid-configuration` | GET | OIDC discovery |

### 5.2 Hosted login page endpoints

| Endpoint | Method | Purpose |
|---|---|---|
| `/login` | GET | Render login page for a given `authorize` request; lists enabled IdPs |
| `/login/password` | POST | Local IdP submission (email + password) |
| `/idp/{idp_id}/start` | GET | Kick off OIDC / SAML external flow |
| `/idp/{idp_id}/callback` | GET | Receive OIDC / SAML response; JIT provision; redirect back to client |

The hosted page is served under the auth server origin. Agent and SPA never render these forms themselves.

### 5.3 Admin management endpoints (CP admin API)

| Endpoint | Method | Purpose |
|---|---|---|
| `/api/admin/idp` | GET, POST | List / create IdPs |
| `/api/admin/idp/{id}` | GET, PATCH, DELETE | Manage an IdP (local IdP is un-deletable) |
| `/api/admin/oauth-clients` | GET, POST | Manage OAuth clients |
| `/api/admin/oauth-clients/{id}` | GET, PATCH, DELETE | Manage a client |
| `/api/admin/federated-identities?user_id=` | GET | View a user's linked external identities |
| `/api/admin/federated-identities/{id}` | DELETE | Unlink an external identity |
| `/api/admin/auth/sessions` | GET | List active sessions (from `refresh_token`) |
| `/api/admin/auth/sessions` | DELETE | Force-logout a user / device / session |

### 5.4 Self-service endpoints

| Endpoint | Method | Purpose |
|---|---|---|
| `/api/me` | GET | Current user + linked identities |
| `/api/me/password` | POST | Change local password |
| `/api/me/sessions` | GET | Own active sessions |
| `/api/me/sessions/{id}` | DELETE | Revoke own session |

### 5.5 Agent end-to-end login flow (`sso-strict` mode)

The browser does not carry the device certificate, so the flow uses a **device binding pre-flight**: the agent first registers its `device_id` against a one-time `binding_id` over its existing mTLS channel to the auth server, then embeds the `binding_id` in the browser-side authorize URL.

1. Agent receives `auth.*` config via Hub shadow update after enrollment.
2. Agent's auth state machine enters `Authenticating`, generates `code_verifier`, `code_challenge` (S256), `state`, `nonce`, and a random `binding_id`.
3. Agent starts a loopback HTTP server on an ephemeral port (`http://127.0.0.1:<port>/callback`).
4. **Device binding pre-flight (mTLS, agent → auth server):**
   `POST {authServerURL}/oauth/device-binding` with body `{binding_id, state, code_challenge}`. The auth server authenticates the caller via the device certificate, resolves `device_id`, and stores an entry `(binding_id → {device_id, state, code_challenge, expires_at: now+5min})`. Returns `204 No Content`.
5. Agent opens the system browser to
   `GET {authServerURL}/oauth/authorize?response_type=code&client_id=agent-desktop&redirect_uri=http://127.0.0.1:{port}/callback&code_challenge=...&code_challenge_method=S256&state=...&nonce=...&scope=traffic:write+shadow:read&binding_id=...`.
6. Auth server looks up `binding_id`, verifies `state` and `code_challenge` match the pre-flight record, redirects to `/login`. The hosted page reads `identity_provider`, renders available IdPs (password form for local, buttons for OIDC / SAML).
7. User authenticates (local password or SSO). Auth server validates, finds or JIT-creates `nexus_user`, updates `user_federated_identity.last_login_at`, mints an authorization code tied to the `device_id` retrieved via `binding_id`.
8. Auth server 302s the browser back to the loopback URL with `code` + `state`.
9. Agent validates `state`, exchanges code: `POST /oauth/token` with `grant_type=authorization_code`, `code_verifier`, `code`, `client_id` (call is over mTLS; auth server re-verifies that the connection's `device_id` matches the code's bound `device_id`).
10. Auth server returns `access_token` (JWT, 1h, carrying `device_id` claim), `refresh_token` (opaque, 24h, rotating), `token_type=Bearer`, `expires_in`.
11. Agent stores refresh token in OS keychain, keeps access token in memory, transitions to `Authenticated`, starts refresh loop.

The `binding_id` record expires after 5 minutes and is deleted on first successful code mint; replay is impossible.

### 5.6 CP UI (SPA) end-to-end login flow

1. User opens `https://cp.nexus.ai/`. React app mounts; `<AuthBootstrap>` runs before routes render.
2. No access token in memory → attempts silent refresh: `POST /oauth/token` with `grant_type=refresh_token`; browser includes the HttpOnly refresh cookie.
3. On success → new access token is held in memory; router navigates to intended path.
4. On failure / no cookie → `window.location = {authServerURL}/oauth/authorize?client_id=cp-ui&redirect_uri=https://cp.nexus.ai/auth/callback&code_challenge=...&state=...&nonce=...`.
5. Same hosted login page as agent; IdP selection is identical.
6. After auth, auth server 302s back to `/auth/callback?code=...&state=...`.
7. SPA exchanges code at `/oauth/token`; auth server returns access JWT in response body and sets `Set-Cookie: nexus_refresh=...; HttpOnly; Secure; SameSite=Strict; Path=/oauth`.
8. SPA stores access token in memory only; router replaces URL.

### 5.7 Agent vs SPA — one flow, different carriers

| Aspect | Agent | SPA |
|---|---|---|
| Redirect target | `http://127.0.0.1:{port}/callback` | `https://cp.nexus.ai/auth/callback` |
| Access token storage | Memory (process lifetime) | Memory (JS heap, tab lifetime) |
| Refresh token storage | OS keychain | HttpOnly+Secure+SameSite=Strict cookie on `/oauth` |
| `device_id` claim | Yes (from mTLS cert) | No |
| PKCE | Required (S256) | Required (S256) |
| CSRF defense | `state` param | `state` param + SameSite=Strict cookie |
| Multi-tab sync | N/A | BroadcastChannel for logout fan-out |

## 6. JWT Structure & Verification

### 6.1 Access token claims

```json
{
  "iss": "https://auth.nexus.ai",
  "sub": "usr_...",
  "aud": ["hub","ai-gateway","proxy"],
  "exp": 1713544800,
  "iat": 1713541200,
  "nbf": 1713541200,
  "jti": "tok_...",
  "client_id": "agent-desktop",
  "scope": "traffic:write shadow:read",
  "device_id": "dev_...",
  "session_id": "sess_...",
  "email": "alice@corp.com",
  "roles": ["developer","team:data"],
  "idp": "okta",
  "auth_mode": "sso-strict",
  "amr": ["pwd"]
}
```

Header: `{ "alg": "RS256", "typ": "JWT", "kid": "key-2026-01" }`.

### 6.2 JWKS distribution

- Auth server exposes `GET /.well-known/jwks.json` with all currently valid signing keys.
- Each signing key has a 90-day lifetime with 14 days of overlap during rotation.
- Resource servers cache JWKS for 15 min with stale-while-revalidate semantics.

### 6.3 `shared/jwtverifier` package

New package at `packages/shared/jwtverifier/`.

```go
package jwtverifier

type Config struct {
    Issuer    string
    JWKSURL   string
    Audience  string            // "hub" | "ai-gateway" | "proxy" | "cp-ui"
    ClockSkew time.Duration     // default 5 min
    RevCheck  RevocationChecker
    Logger    *slog.Logger
    Metrics   MetricsCollector
}

type Verifier struct { /* ... */ }

type Claims struct {
    Issuer    string   `json:"iss"`
    Subject   string   `json:"sub"`
    Audience  []string `json:"aud"`
    ExpiresAt int64    `json:"exp"`
    IssuedAt  int64    `json:"iat"`
    NotBefore int64    `json:"nbf"`
    JTI       string   `json:"jti"`

    ClientID  string   `json:"client_id"`
    Scope     string   `json:"scope"`
    DeviceID  string   `json:"device_id,omitempty"`
    SessionID string   `json:"session_id,omitempty"`
    Email     string   `json:"email"`
    Roles     []string `json:"roles"`
    IDP       string   `json:"idp"`
    AuthMode  string   `json:"auth_mode,omitempty"`
    AMR       []string `json:"amr"`
}

// Verify performs signature + iss/aud/exp/nbf checks and then consults
// the revocation checker.
func (v *Verifier) Verify(ctx context.Context, token string) (*Claims, error)

// Middleware returns an Echo middleware that attaches Claims on success.
func (v *Verifier) Middleware() echo.MiddlewareFunc

type RevocationChecker interface {
    IsRevoked(ctx context.Context, claims *Claims) (bool, error)
}
```

### 6.4 Error sentinels

```go
var (
    ErrInvalidSignature = errors.New("invalid signature")
    ErrExpired          = errors.New("token expired")
    ErrNotYetValid      = errors.New("token not yet valid")
    ErrWrongAudience    = errors.New("wrong audience")
    ErrWrongIssuer      = errors.New("wrong issuer")
    ErrRevoked          = errors.New("token revoked")
    ErrMalformed        = errors.New("malformed token")
    ErrJWKSUnavailable  = errors.New("jwks unavailable")
)
```

### 6.5 Fail-closed policy

- If JWKS fetch fails and no cached JWKS is available, all new token verifications return `ErrJWKSUnavailable`.
- Cached JWKS continues to be used for up to 15 min after it becomes stale, bounding the outage's impact.

## 7. Agent Integration

### 7.1 Auth config source

Agent never hardcodes auth server URLs. It receives them through Hub shadow's Category A desired config:

```yaml
auth:
  authServerURL: https://cp.nexus.ai
  jwksURL:       https://cp.nexus.ai/.well-known/jwks.json
  clientID:      agent-desktop
  mode:          sso-strict        # os-only | sso-preferred | sso-strict
  osOnlyTTL:     8h
```

### 7.2 Auth mode behaviour

| Mode | Trigger | Fallback |
|---|---|---|
| `os-only` | Never triggers SSO; authenticates via mTLS device cert + OS user info | None |
| `sso-preferred` | Attempts SSO on startup / token expiry; **falls back to `os-only`** on failure | Local session TTL = `osOnlyTTL` |
| `sso-strict` | SSO required on startup / token expiry; **blocks AI traffic** on failure | None |

### 7.3 Agent auth state machine

```
NotEnrolled
    │ enroll()
    ▼
Enrolled (no JWT) ──────── Unenroll
    │
    ├── mode=os-only ─────────────────────┐
    │                                      ▼
    │                                  OSAuthed
    │
    └── mode=sso-*
        │ user login
        ▼
    Authenticating (browser open)
        │ callback: code → token
        ▼
    Authenticated ──── heartbeat / refresh loops
        │
        │ access expiring (5 min pre-exp)
        ▼
    Refreshing
        │ success → Authenticated
        │ refresh 401 / revoked push
        ▼
    Revoked
        │ mode=sso-strict  → Authenticating (force re-login)
        │ mode=sso-preferred → OSAuthed (degraded)
        │ mode=os-only      → OSAuthed
```

State transitions are logged locally and reported to Hub via heartbeat.

### 7.4 Token storage

```go
// packages/agent/core/security/secretstore/store.go
type Store interface {
    Set(key string, value []byte) error
    Get(key string) ([]byte, error)
    Delete(key string) error
}

type darwinKeychain  struct{} // macOS Security framework
type windowsDPAPI    struct{} // CredWrite / CredRead
type linuxSecret     struct{} // libsecret via D-Bus
```

Stored items:

| Key | Value | Why not a plain file |
|---|---|---|
| `nexus.refresh_token` | JWT refresh token | Would allow replay until revocation propagates |
| `nexus.session_id` | Auth server session id | Single-logout hook |
| `nexus.server_time_offset` | Signed integer, seconds | Clock skew correction |

Access token lives only in `atomic.Value`; agent restart always triggers refresh.

**Fallback for headless Linux without D-Bus:** encrypted file at `~/.config/nexus/agent-secrets.enc` using HKDF-SHA256 key derived from the device private key. Device private key is already file-system-protected (0600); the encryption prevents accidental `cat` disclosure but does not expand the attack surface.

### 7.5 Clock skew handling

- Auth server responses carry the standard `Date:` header.
- Agent records `serverOffset = serverTime - localTime` in `secretstore`.
- All `exp` / `iat` comparisons use `time.Now().Add(serverOffset)`.
- Offset magnitude reported via heartbeat metric.

| Drift | Action |
|---|---|
| < 5 min | Normal (±5 min skew tolerance) |
| 5–15 min | `INFO` log + metric |
| 15 min – 1 h | `WARN` + admin UI badge "clock anomaly" on device |
| > 1 h | `ERROR` + optional forced re-login (prevents 1970 exploit) |

The agent never changes the system clock and does not depend on NTP.

### 7.6 Refresh strategy

Triggers:

1. Proactive: 5 min before access `exp`.
2. Reactive: any 401 from RS.
3. Startup: access token is memory-only, so boot always triggers refresh.

Rotation: every refresh issues a new refresh token with a new `jti` in the same `session_id` chain.

Retry backoff: `1 s, 5 s, 30 s, 2 min, 10 min`. No infinite-loop on clock-skew-induced failures.

### 7.7 Mode × refresh-failure matrix

| Mode | `invalid_grant` | Network failure |
|---|---|---|
| `os-only` | N/A | N/A |
| `sso-preferred` | Degrade to `OSAuthed`; background retry SSO | Use current access until expiry, then degrade |
| `sso-strict` | Block AI traffic; notify user to re-login | Use current access until expiry, then block |

Blocking is scoped to **proxied AI traffic**; agent heartbeat and shadow sync keep running so operators can still see state.

### 7.8 New / changed agent packages

```
packages/agent/core/
├── auth/              # NEW
│   ├── authenticator.go     # OAuth client, PKCE, loopback server
│   ├── tokenmanager.go      # Refresh loop, state machine
│   ├── clockoffset.go       # Date header parsing + drift metrics
│   └── modes.go             # os-only / sso-preferred / sso-strict behaviour
├── secretstore/       # NEW
│   ├── darwin.go
│   ├── windows.go
│   ├── linux.go
│   └── fallback.go          # Encrypted file fallback
└── enrollment/        # EXISTING, touched
    └── enroll.go            # On enrollment success, bootstrap auth from shadow
```

### 7.9 Removed from agent in the same PR

`packages/agent/core/gateway/client.go` is deleted wholesale. Its functions are replaced as follows:

| Old client call | Replacement |
|---|---|
| `Enroll` | Already migrated to Hub `/api/internal/things/enroll` |
| `Unenroll` | `thingclient` + Hub unenroll endpoint |
| `SendHeartbeat` | `thingclient` (already) |
| `PullConfig` | `thingclient` shadow (already) |
| `UploadAudit` | Hub `/api/internal/things/audit` |
| `UploadExemption` | Hub endpoint |
| `CheckUpdate` | Hub endpoint |
| `RenewCert` | Hub endpoint |
| `GetAuthConfig` | **Deleted** — config comes from Hub shadow |
| `Authenticate` | **Deleted** — agent talks to auth server directly |
| `GetAuthStatus` | **Deleted** — local state machine |

The `gateway` package itself is removed.

## 8. Revocation

### 8.1 Layering

```
RFC 7009 Token Revocation  →  Auth server writes revoked_token row
                                       │
                                       ▼
MQ publish nexus.auth.revocation  →  RS update in-memory bloom / maps
                                       │
                                       ▼
Refresh rotation + replay detection  (session-wide revocation on replay)
```

### 8.2 Trigger scenarios

| Scenario | Revocation scope | Trigger |
|---|---|---|
| User logout (SPA / agent) | Current session | Client → `/oauth/revoke` |
| Admin disables user | All sessions for user | Admin API |
| Admin force-logout device | All sessions for `device_id` | Admin API |
| Refresh replay detected | Entire `session_id` chain | Auth server internal |
| Device unenroll | All sessions for `device_id` + cert revoke | Agent → Hub → auth server |
| Role / IdP change | All sessions for affected user | Admin API |

### 8.3 MQ event shape

Topic: `nexus.auth.revocation` on NATS JetStream via `packages/shared/transport/mq`. Retained 24 h (covering max access token lifetime).

```json
{
  "event_id": "evt_...",
  "revoked_at": "2026-04-19T10:30:00Z",
  "type": "jti | user | device | session",
  "target": {
    "jti": "tok_...",
    "user_id": "usr_...",
    "device_id": "dev_...",
    "session_id": "sess_..."
  },
  "reason": "user_logout | admin_disable | replay_detected | unenroll | role_change",
  "expires_at": "2026-04-19T11:30:00Z"
}
```

### 8.4 `MQRevocationChecker` on RS

```go
type MQRevocationChecker struct {
    filter    *bloom.Filter           // ~1 MB, ~100k jti
    byUser    map[string]time.Time    // user_id   → revokedAt cutoff
    byDevice  map[string]time.Time    // device_id → revokedAt cutoff
    bySession map[string]time.Time    // session_id → revokedAt cutoff
    mu        sync.RWMutex
}

func (c *MQRevocationChecker) IsRevoked(ctx context.Context, claims *Claims) (bool, error) {
    // 1. jti bloom check (false-positive falls back to introspect)
    // 2. byUser[claims.sub]:            revokedAt > claims.iat → revoked
    // 3. byDevice[claims.device_id]:    same rule
    // 4. bySession[claims.session_id]:  same rule
}
```

- `user` / `device` / `session` revocations are stored as cutoff timestamps — a single map entry kills all past tokens for that subject.
- On MQ disconnect > 30 s, verifier enters **strict mode**: every verify calls `/oauth/introspect`. On reconnect, it replays `GET /admin/revocations?since=<last_event_id>`.

### 8.5 Refresh replay detection

```
refresh_token columns used:
    jti, session_id, parent_jti, used_at

POST /oauth/token grant_type=refresh_token:
  1. Look up the incoming refresh token by hash.
  2. If used_at IS NOT NULL:
       → REPLAY. Revoke the whole session_id (type=session event).
       → 401 invalid_grant.
  3. Else:
       → Stamp used_at = NOW() on the current row.
       → Insert a new refresh_token (parent_jti=current.jti, session_id carried over).
       → Return new access + refresh.
```

A stolen refresh token can fire exactly once; whoever refreshes second triggers the session revocation.

### 8.6 Logout flows

**User logout:**

1. Client `POST /oauth/revoke` with `token=<refresh_token>&token_type_hint=refresh_token`.
2. Auth server writes `revoked_token (scope=session, target_session_id=...)`, deletes `refresh_token` rows in that session, publishes MQ event.
3. Client clears local storage (agent: secretstore; SPA: memory + cookie expiry request).

**Admin force-logout a user:**

1. `DELETE /api/admin/auth/sessions?user_id=X`.
2. Auth server writes `revoked_token (scope=user, target_user_id=X, revoked_at=NOW())`.
3. `DELETE FROM refresh_token WHERE user_id = X`.
4. MQ event published.
5. RS rejects any token for X on the next request.

### 8.7 Cleanup

Hub's scheduled-job runner executes hourly:

```sql
DELETE FROM revoked_token
WHERE expires_at < NOW() - INTERVAL '1 day';

DELETE FROM refresh_token
WHERE expires_at < NOW();
```

### 8.8 Performance budget

| Operation | Budget |
|---|---|
| `IsRevoked` hot path (bloom) | p99 < 50 μs |
| MQ event → RS memory update | p99 < 500 ms |
| `/oauth/revoke` | p99 < 50 ms |
| Bloom false-positive → introspect | p99 < 20 ms |
| End-to-end propagation (click "Logout" → AG rejects) | p99 < 1 s |

## 9. Auth Server Package Layout

The auth server lives inside CP today as an internally isolated package. It owns its five new tables (plus `nexus_user` read + write of `password_hash` / `last_login_at`) and the `shared/mq` interface. It does **not** import CP's admin API, IAM service, or analytics modules.

```
packages/control-plane/internal/authserver/
├── oauth/
│   ├── authorize.go       # /oauth/authorize
│   ├── token.go           # /oauth/token
│   ├── revoke.go          # /oauth/revoke (RFC 7009)
│   ├── introspect.go      # /oauth/introspect (RFC 7662)
│   ├── jwks.go            # /.well-known/jwks.json
│   └── pkce.go            # PKCE S256
├── login/
│   ├── page.go            # GET /login
│   ├── password.go        # POST /login/password
│   └── callback.go        # /idp/{idp_id}/callback
├── idp/
│   ├── local.go
│   ├── oidc.go
│   ├── saml.go
│   └── registry.go
├── token/
│   ├── signer.go          # RS256 signing + kid rotation
│   ├── refresh.go         # Rotation + replay detection
│   └── keystore.go        # Phase 1: PEM; Phase 2: KMS interface
├── revocation/
│   ├── store.go
│   └── publisher.go
└── mount.go               # Registers routes on the CP Echo instance
```

Spec B's extraction — moving this package to `packages/nexus-auth-server/` and deploying it as a standalone process — is a deployment change. No code changes are required.

## 10. CP UI (SPA) Admin Changes

All CP UI admin screens relating to auth are consolidated in this spec. No legacy "SSO config" page survives.

New / restructured pages under `Settings → Identity`:

| Page | Function |
|---|---|
| **Identity Providers** | CRUD for `identity_provider` rows; shows type (local / oidc / saml), enabled toggle, default role, role mapping editor, JIT switch |
| **OAuth Clients** | Manage `oauth_client` rows (agent-desktop, cp-ui, future) |
| **Federated Identities** (read-only) | Lookup by user; shows linked external identities, last login, unlink action |
| **Sessions** | List active sessions; filter by user / device; force-logout |

Local IdP is present by default, cannot be deleted, and can be disabled only when at least one `break_glass=true` user exists (guard against self-lockout).

SPA bootstrap logic is unaware of which IdPs are enabled — it always redirects to the auth server and lets the hosted login page render whatever is available.

## 11. Phasing (work order; no compatibility layers)

Development is broken into seven phases. **Each phase's PR both adds the new structure and removes the code it replaces** — no deprecated paths left alive.

### Phase 1 — Auth server core

- Create 5 new tables + extend `nexus_user` in one migration; drop any pre-existing session / SSO-config tables in the same migration.
- Seed default local IdP and the two OAuth clients (`agent-desktop`, `cp-ui`).
- Implement `oauth/*` endpoints, hosted login page, local IdP adapter.
- Keystore starts with PEM on disk (rotation CLI in Phase 7).
- Delete CP's old `/api/admin/auth/login` + related session middleware.

### Phase 2 — `shared/jwtverifier` + RS integration

- Ship the verifier package with JWKS cache + fail-closed.
- Hub mounts JWT middleware on user-scoped routes.
- AG mounts JWT middleware on `/v1/*` (API-key path remains for programmatic clients; unchanged).
- Proxy mounts JWT middleware on its admin / control surfaces.
- Remove `device-token` bearer verification on any user-scoped RS route.

### Phase 3 — Agent auth rewrite

- Add `agent/internal/auth/` + `agent/internal/secretstore/`.
- Delete `packages/agent/core/gateway/client.go` and the entire `gateway` package.
- Wire enrollment → shadow bootstrap → auth bootstrap.
- Implement the full state machine, clock offset, refresh loop, mode behaviours.
- End-to-end tests with loopback server and a mock auth server.

### Phase 4 — Revocation

- Implement `/oauth/revoke`, MQ publisher, `MQRevocationChecker`.
- Implement refresh-rotation replay detection.
- Implement admin force-logout endpoints.
- Wire scheduled cleanup job in Hub.

### Phase 5 — IdP expansion

- OIDC adapter (generic; verified against `dexidp/dex`).
- SAML adapter.
- JIT provisioning + role mapping engine (regex + exact match).
- Account linking API (link additional IdP identity to an existing user).

### Phase 6 — CP UI migration

- SPA: OAuth / PKCE flow, silent refresh on boot, HttpOnly refresh cookie, BroadcastChannel for multi-tab logout.
- New admin pages: Identity Providers, OAuth Clients, Federated Identities, Sessions.
- Delete the old SSO config pages.

### Phase 7 — Ops & observability

- Metrics: JWT verify rate, revocation lag, refresh rate, mode distribution, clock drift.
- Audit log (auth-server events): login, logout, revocation, admin force-logout, IdP changes.
- Key rotation CLI: `nexus-hub auth rotate-key` (signs next kid, flips active kid after overlap window).
- Documentation: `docs/ops/auth-server.md`, `docs/dev/jwt-verifier-integration.md`, updated `docs/dev/architecture.md`.

## 12. Testing Strategy

- **Unit:** sign / verify, PKCE S256, refresh rotation state machine, replay detection, clock-offset computation, IdP adapters (mock IdP).
- **Integration:** CP + mock Okta (`dexidp/dex`) + mock agent (loopback); all three modes × (happy path / access expiry / refresh failure / revocation).
- **End-to-end:** real agent binary + real Hub + real CP + fake IdP container; runs in CI via docker-compose.
- **Chaos:** random MQ disconnection, auth server restart, JWKS unavailable; verify fail-closed / strict-mode fallback.
- **Clock tests:** agent container with `date -s` offsets of ±10 min, ±1 h, 1970, future; verify behaviour matches Section 7.5 matrix.

## 13. Documentation Deliverables

- `docs/requirements/e{N}-unified-agent-auth.md` — requirements doc.
- `docs/sdd/e{N}-s{1..7}-*.md` — one SDD per phase.
- `docs/openapi/e{N}-s1-auth-server.yaml` — OAuth / OIDC surface.
- `docs/dev/jwt-verifier-integration.md` — guide for future RS.
- `docs/ops/auth-server.md` — runbook: key rotation, emergency mode switch (`sso-strict` → `os-only` via shadow push), log locations.
- Updates to `docs/dev/architecture.md` reflecting the new auth boundary.

## 14. Open Questions (Deferred to Plan)

These are implementation details rather than design choices; they will be resolved during planning.

- Exact SAML library choice (`crewjam/saml` vs `russellhaering/gosaml2`).
- Hosted login page rendering technology (server-rendered HTML vs a small embedded React bundle served by auth server).
- SCIM provisioning for enterprise IdPs — likely a separate future spec.
- MFA / step-up authentication — out of scope for Spec A; covered by the `amr` claim so a later spec can layer it in without data model changes.

## 15. Non-Goals

- SCIM / directory sync.
- MFA policy engine.
- Per-request API-key rotation for AI Gateway (the `/v1/*` API-key path is out of scope).
- Any standalone auth server deployment work (Spec B).
