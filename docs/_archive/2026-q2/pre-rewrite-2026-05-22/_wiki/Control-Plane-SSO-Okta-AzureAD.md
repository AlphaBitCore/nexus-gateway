# Control Plane SSO Okta AzureAD

*Audience: operators configuring external identity providers.*

Nexus Gateway is always the **Service Provider (SP)**. External IdPs such as Okta, Azure AD, Google Workspace, or any generic OIDC provider are configured in the Control Plane and handle user authentication on behalf of Nexus. Nexus Local accounts remain available as a break-glass fallback — they are not a peer IdP.

---

## What SSO enables

When an external IdP is configured:

- Users authenticate at the external IdP rather than entering Nexus Local credentials.
- First-time federated users are provisioned automatically (JIT — just-in-time provisioning).
- IAM group memberships can be mapped from IdP claims (e.g., IdP group `nexus-admins` → IAM group `NexusAdminFullAccess`).
- Multiple IdPs can be active simultaneously; users pick from the login screen.

Without an IdP configured, all admin logins use Nexus Local accounts.

## OIDC federation flow

The shipped federation path uses OIDC (OpenID Connect).

```mermaid
sequenceDiagram
    participant User as Admin browser
    participant CP as Control Plane
    participant IdP as External IdP

    User->>CP: GET /oauth/authorize (click "Sign in with Okta")
    CP->>IdP: redirect to authorization_endpoint + PKCE + state
    User->>IdP: authenticate + consent
    IdP->>CP: GET /authserver/oidc/callback?code=...&state=...
    CP->>IdP: POST token_endpoint (exchange code for ID token)
    CP->>CP: validate ID token (JWKS, iss, aud, sub, exp)
    CP->>CP: map claims → Nexus user; JIT if new
    CP->>User: redirect with Nexus bearer token
```

The begin endpoint is `GET /authserver/oidc/begin`; the callback endpoint is `GET /authserver/oidc/callback`.

## Claim mapping and JIT provisioning

On each federated login, the Control Plane maps IdP claims to Nexus fields:

- `email` claim → Nexus user email.
- `name` / `given_name` / `family_name` → display name.
- Configured group/role claim → initial IAM group memberships.

If the user does not yet exist in Nexus, `FederatedStore.JITProvisionUser` creates a new `NexusUser` row with `source='oidc'` and places the user in the configured default organization. Subsequent logins update the user's display name and group memberships.

Federation can operate in two modes:

- **Open JIT** — any user that authenticates at the IdP gets a Nexus account.
- **Allowlist-only** — only pre-registered users can log in; others receive `user_not_provisioned`.

## Configuring an IdP

From the admin UI: IAM → Identity Providers → New → OIDC → fill in:

- Issuer URL (e.g., `https://acme.okta.com`)
- Client ID and client secret (stored encrypted at rest)
- Claim mapping (group/role claim name → IAM group)
- Default organization and default IAM group for JIT-provisioned users
- Allowlist mode on/off

After saving, use the "Test" button to verify the OIDC discovery document and JWKS resolve correctly. The IdP configuration is stored in the `IdentityProvider` table; the login screen lists active IdPs for users to pick from.

## Multiple IdPs per tenant

A tenant can have multiple IdPs configured — one per acquired company, for example. Users see all active IdPs on the login screen and choose which one to use. Nexus does not perform automatic IdP discovery from the email domain; users select explicitly.

## SAML (planned)

The `IdentityProvider.type` enum supports `local | oidc | saml`, but only the OIDC handler is shipped. The SAML AuthnRequest emitter, callback handler at `/authserver/saml/callback`, signed-assertion validation, and claim normalization are not yet implemented. Track progress in `docs/developers/roadmap.md`.

## Break-glass with Nexus Local

Nexus Local accounts remain usable even when an IdP is misconfigured or unreachable. A super-admin can always log in with a Local account to fix IdP settings. Nexus Local is the **implicit fallback** — never refer to it as a peer IdP in config or documentation.

## JWT validation for IdP tokens

The Control Plane validates OIDC ID tokens using the same JWT verifier as for locally issued tokens (`packages/control-plane/internal/identity/jwt/`). Per-verifier instance is single-issuer; multi-IdP federation uses multiple verifier instances. Validation checks: JWKS-fetched RS256 signature, `iss` whitelist, `aud` match, time-window (`iat`/`nbf`/`exp`), and non-empty `sub` (ghost-principal defence).

## Failure modes

| Failure | Behaviour |
|---|---|
| `state` mismatch | CSRF protection rejects; user retries |
| JWT signature invalid | JWKS stale-while-revalidate cache + kid-miss refresh handle short rotations; longer failures surface to admin |
| Empty `sub` | Verifier rejects as malformed (ghost-principal defence) |
| Claim mapping missing | User lands in default org with default IAM group; admin adjusts via Users page |
| IdP unreachable | New sign-ins fail; existing tokens valid until expiry; break-glass via Local account |
| Allowlist-mode block | User receives `user_not_provisioned`; admin adds them |

---

## Canonical docs

- [`idp-sso-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/idp-sso-architecture.md) — full federation flow, JIT provisioning, multi-IdP, sign-out
- [`idp-federation.md` (flow)](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/flows/idp-federation.md) — step-by-step OIDC sequence with failure modes

**Adjacent wiki pages**: [Control Plane Authentication](Control-Plane-Authentication) · [Control Plane IAM Model](Control-Plane-IAM-Model) · [Control Plane Multi Tenancy](Control-Plane-Multi-Tenancy) · [Feature IAM And SSO](Feature-IAM-And-SSO)
