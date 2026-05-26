---
doc: idp-sso-architecture
area: service
service: control-plane
tier: 1
---

# IdP / SSO Architecture

> **Tier 1 architecture doc.** Read this before touching `packages/control-plane/internal/identity/authserver/**`, `packages/control-plane/internal/identity/sso/**`, `packages/control-plane/internal/identity/jwt/**`, the JIT user provisioning path, or the IdP CRUD UI. The IAM model that picks up after a successful login lives in `iam-identity-architecture.md`.

---

## 1. Positioning (binding terminology)

Nexus is always the **Service Provider** (SP). The acronym "IdP" in Nexus code, docs, and UI refers **only** to **external identity providers** — Okta, Azure AD, Google Workspace, generic OIDC, generic SAML.

Nexus Local accounts (super-admin bootstrap, break-glass admins) are the **implicit fallback**, never a peer IdP. Do not refer to Nexus Local as "the local IdP" in code or docs.

This rule is binding (memory `feedback_sp_idp_positioning`).

## 2. Authentication surfaces

Nexus exposes two authentication surfaces:

| Surface | Used by | Auth |
|---|---|---|
| Admin UI / admin API | Operators, developers | OAuth 2.0 + PKCE bearer (with optional external IdP federation in front). |
| `/v1/*` AI traffic | Applications | Bearer Virtual Key. |

This doc covers the first surface. Virtual Keys are documented in `credentials-architecture.md`.

All admin auth goes through PKCE bearer tokens. The local-password login surface lives in `packages/control-plane/internal/identity/authserver/login/password.go` (`POST /authserver/password`).

## 3. OAuth 2.0 + PKCE (local auth server)

The Control Plane runs a local authorisation server (`packages/control-plane/internal/identity/authserver`). Endpoints are mounted in `mount.go` (the `/oauth/*` block starts around line 137; the `/authserver/*` block — password, OIDC begin/callback — runs roughly line 232-266). PKCE flow:

1. Client (admin UI / CLI / `tests/lib/auth.sh`) generates `code_verifier` + `code_challenge`.
2. Client redirects user to `/oauth/authorize` with `code_challenge` + scopes.
3. User authenticates — either against a Nexus Local account or, when an external IdP is configured, the local auth server delegates to the IdP (see §4).
4. Auth server issues an authorisation code bound to `code_challenge`.
5. Client exchanges the code at `/oauth/token` with the original `code_verifier`.
6. Server returns a bearer access token (JWT) + opaque refresh token.

The `cp_login` helper in `tests/lib/auth.sh` is the canonical CLI entry point; it caches the token at `/tmp/nexus_test_token`. The `cp_curl` helper uses that token for subsequent admin API calls.

## 4. External IdP federation

When an IdP is configured for a tenant, the local auth server delegates step 3 to the IdP using OIDC (today) or SAML (planned).

### OIDC flow (shipped)

1. Local auth server redirects the browser to the IdP's `authorization_endpoint` with `response_type=code` + a Nexus-side `state` token + PKCE.
2. User authenticates at the IdP and consents.
3. IdP redirects back to Nexus `/authserver/oidc/callback` with code + state.
4. Nexus exchanges the code at the IdP's `token_endpoint`, receives ID token + access token.
5. Nexus validates the ID token via the JWT verifier (§5).
6. Nexus maps claims → Nexus user (JIT if needed, §6).
7. Nexus issues its own PKCE bearer to the admin UI client.

Handler: `packages/control-plane/internal/identity/authserver/login/oidc.go`. Begin handler is mounted at `/authserver/oidc/begin`.

### SAML flow (planned)

**Planned**: SAML SSO. The `IdentityProvider.type` enum supports `local | oidc | saml`, but only the OIDC handler is shipped (`login/oidc.go`). The SAML runtime handler — AuthnRequest emitter, callback at `/authserver/saml/callback`, signed-assertion validation, claim normalisation — is **not yet implemented**. Track progress in `docs/developers/roadmap.md`.

## 5. JWT verifier

For OIDC ID tokens and any internally-signed JWTs (the Control Plane's own bearer access tokens), Nexus runs a JWT verifier in `packages/control-plane/internal/identity/jwt/`. Per-Verifier instance is **single-issuer**; multi-IdP federation uses multiple Verifier instances or a router upstream of the JWT verify call. See `jwt-verifier-architecture.md` for the full surface (algorithm pinning, JWKS cache, revocation, ghost-principal defence).

The verifier is also used by the Hub when validating Service Things' bootstrap tokens. (Hub-side service auth detail: see `service-call-framework.md`.)

## 6. JIT user provisioning

On first successful federated login for a user that does not yet exist in Nexus:

1. Map IdP claims to Nexus fields:
   - `email` (claim) → Nexus user email.
   - `name` / `given_name` / `family_name` (claims) → display name.
   - Configurable group/role claim → initial IAM group membership.
2. Create the Nexus user row via `FederatedStore.JITProvisionUser` (`packages/control-plane/internal/identity/authserver/store/federated_store.go`).
3. Assign the initial group memberships.
4. Land the user in a default organisation/project (configured per IdP).
5. Proceed with token issuance.

Subsequent logins for the same external identity (matched by `iss` + `sub`) update the user record (display name, group memberships) on each login.

Federation can be configured to **block** unmapped users (allowlist-only mode) or to allow JIT with a default role. Allowlist-blocked logins return `user_not_provisioned`.

**Note:** there is no dedicated `admin:user.jit_provisioned` admin-audit event today. JIT runs inside the OIDC callback handler and the user creation is observable via the `NexusUser` row plus its `source='oidc'` field; a dedicated audit event is in the queue (see `docs/developers/roadmap.md`).

## 7. Multi-IdP per tenant

A tenant can have multiple IdPs configured (one for each acquired company, for example). The user picks an IdP on the login screen ("Sign in with Okta", "Sign in with Azure AD"); the SPA renders enabled IdPs returned by `GET /authserver/idps`, which orders by IdP name (`packages/control-plane/internal/identity/authserver/store/idp_store.go:50-56`). Nexus does not perform automatic IdP discovery from the email domain. The federation layer instantiates a Verifier per IdP issuer (per §5).

## 8. Local fallback (break-glass)

Nexus Local accounts always remain usable, even when an IdP is misconfigured or down. This is the break-glass channel: a super-admin can log in with a Local account and fix the IdP config.

Local accounts are managed via the IAM API. Password hash uses bcrypt; passwords are never logged.

## 9. Sign-out

- **Nexus session sign-out** — invalidate the access token via `POST /oauth/revoke` (RFC 7009; mounted at `packages/control-plane/internal/identity/authserver/mount.go:203`); the SPA clears its client-side token cache.
- **Federated sign-out** — not wired today. A sign-out at the IdP does not propagate to Nexus, and Nexus does not emit `end_session_endpoint` to the IdP on local sign-out. Track in `docs/developers/roadmap.md` if a customer requires it.

## 10. Sources

- `packages/control-plane/internal/identity/authserver/` — OAuth+PKCE local server + login handlers (`login/password.go`, `login/oidc.go`, `login/idps.go`).
- `packages/control-plane/internal/identity/authserver/mount.go` — endpoint mount authority.
- `packages/control-plane/internal/identity/sso/` — SSO admin handlers.
- `packages/control-plane/internal/identity/jwt/` — JWT verification + JWKS polling.
- `packages/shared/identity/pkce/` — PKCE primitives.
- `packages/shared/identity/rstokenauth/` — RS256 token issue/verify.
- `tests/lib/auth.sh` — canonical CLI auth helper.
- `docs/developers/specs/e44/e44-external-idp-federation.md` — feature requirements.

## 11. Cross-references

- `iam-identity-architecture.md` — what IAM does once you're authenticated.
- `tenancy-architecture.md` — where a JIT user lands in the org tree.
- `audit-pipeline-architecture.md` — login + admin-action audit events.
- `service-call-framework.md` — service-to-service auth (Things) is separate; the JWT verifier is reused.
