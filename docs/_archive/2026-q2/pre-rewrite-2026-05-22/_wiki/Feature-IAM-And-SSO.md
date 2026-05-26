# Feature IAM And SSO

Nexus Gateway uses an AWS-IAM-shaped policy engine for admin access control: identities, policies with Allow/Deny effects, resource NRNs (Nexus Resource Names), and action patterns. Policy evaluation is deny-overrides — any explicit Deny wins over any Allow. Enterprise tenants federate admin logins through Okta, Azure AD, or any OIDC-compatible identity provider; on first successful login a Nexus user is provisioned just-in-time and mapped to roles via IdP assertion claims.

---

## IAM model

Three independent dimensions compose the IAM model:

### Actions

Actions follow the format `admin:<resource>.<verb>`. The resource is a kebab-case catalog name; the verb is from a closed set (create, read, update, delete, plus resource-specific verbs like `toggle`, `export`, `simulate`, `approve`, `rotate`).

Examples:
- `admin:provider.read`, `admin:provider.create`
- `admin:routing-rule.update`, `admin:routing-rule.simulate`
- `admin:virtual-key.create`, `admin:virtual-key.approve`
- `admin:kill-switch.toggle`
- `admin:audit-log.export`
- `admin:credential.probe`, `admin:credential.rotate`

### Resource NRNs

Resources are addressed by NRN (Nexus Resource Name):

```
nrn:nexus:<service>:<scope>:<resourceType>/<resourceID>
```

- `<service>` — `gateway | iam | compliance | agent | platform`
- `<scope>` — `*` for global, `<org-id>` for org-scoped, `<org-id>/<project-id>` for project-scoped. Scope matching is hierarchical: a pattern for `acme` matches both `acme` and `acme/marketing`.
- `<resourceType>` — kebab-case catalog name (e.g. `provider`, `routing-rule`, `virtual-key`, `iam-policy`)
- `<resourceID>` — concrete ID or `*` wildcard

Examples: `nrn:nexus:gateway:*:provider/openai`, `nrn:nexus:iam:*:iam-policy/*`

### Policies

Policy documents combine effect, actions, and resources:

```json
{
  "effect": "Allow",
  "actions": ["admin:routing-rule.read", "admin:routing-rule.simulate"],
  "resources": ["nrn:nexus:gateway:*:routing-rule/*"]
}
```

Policies attach to users directly or via groups (displayed as "roles" in the UI). Group membership is the recommended path for operator-team management. Policy evaluation loads all applicable policies (direct + group-inherited) and caches them with a 60-second TTL.

## Admin authentication

Admin UI and API access uses OAuth 2.0 with PKCE. The Control Plane runs a local authorisation server that issues bearer access tokens (JWTs) after the user authenticates. All admin auth goes through PKCE bearer tokens — there is no cookie-based login.

Flow:
1. Admin UI generates `code_verifier` + `code_challenge`.
2. Redirects to `/oauth/authorize` with the challenge.
3. User authenticates — either against a Nexus Local account or via an external IdP (when configured).
4. Auth server issues an authorisation code.
5. UI exchanges the code at `/oauth/token` with the original verifier.
6. Server returns a bearer access token (JWT) + opaque refresh token.

## External IdP federation (SSO)

Nexus is the Service Provider. "IdP" in Nexus always refers to an **external** identity provider — Okta, Azure AD, Google Workspace, or any generic OIDC provider. Nexus Local accounts (super-admin, break-glass) are the implicit fallback and are never a peer IdP.

OIDC federation flow:
1. Admin UI redirects to the configured IdP's authorization endpoint.
2. User authenticates at the IdP.
3. IdP redirects back to Nexus `/authserver/oidc/callback` with an authorization code.
4. Nexus exchanges the code, validates the ID token via the JWT verifier.
5. Nexus maps IdP claims to Nexus fields (email, display name, groups).
6. If the user does not exist in Nexus, a Nexus account is provisioned just-in-time with roles derived from IdP assertion claims (e.g., IdP group `nexus-admins` → Nexus group `Admins`).
7. Nexus issues its own PKCE bearer token to the admin UI.

SAML federation is planned but not yet shipped. The `IdentityProvider.type` enum supports `local | oidc | saml`; only the OIDC path is active.

## Where it sits

- IAM engine: `packages/control-plane/internal/identity/iam/` and `packages/shared/identity/iam/`
- Action + NRN catalog: `packages/shared/identity/iam/catalog.go` and `catalog_data.go`
- Local auth server (OAuth/PKCE): `packages/control-plane/internal/identity/authserver/`
- OIDC federation handler: `packages/control-plane/internal/identity/authserver/login/oidc.go`
- JWT verifier: `packages/control-plane/internal/identity/jwt/`

Every admin API handler is wrapped with `iamMW(...)` that derives the resource NRN via `iam.BuildRequestNRNForAction(action)` and evaluates the calling principal's policies. Drift between a handler's `iamMW(...)` call and the UI's `allowedActions` array produces silent 403s — the IAM impact review binding in the development workflow exists to catch this drift at PR time.

## How to enable and configure

**Local IAM (always on):** Manage users, groups, and policies from **IAM** in the Control Plane UI. Assign policies to groups, assign users to groups. The seeded super-admin account has unrestricted access and serves as the break-glass credential.

**External IdP federation:**
1. Navigate to **Settings → Identity Providers** and select **New Provider**.
2. Choose **OIDC** and fill in the IdP's discovery URL (or manual endpoint fields), client ID, and client secret.
3. Configure claim-to-role mappings (e.g., `groups` claim value `nexus-admins` → Nexus group `Admins`).
4. Save. The next admin login will redirect through the configured IdP.

Local accounts remain available for break-glass access. An IdP outage does not lock out the super-admin.

---

## Canonical docs

- [`iam-identity-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/iam-identity-architecture.md) — NRN format, action taxonomy (full catalog), policy document shape, identity attach, IAM impact review binding
- [`idp-sso-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/idp-sso-architecture.md) — SP/IdP positioning, OIDC flow, JIT provisioning, SAML status, JWT verifier

**Adjacent wiki pages**: [Feature Audit And SIEM](Feature-Audit-And-SIEM) · [Control Plane IAM Model](Control-Plane-IAM-Model) · [Control-Plane-SSO-Okta-AzureAD](Control-Plane-SSO-Okta-AzureAD) · [Control Plane Authentication](Control-Plane-Authentication) · [Features Index](Features-Index)
