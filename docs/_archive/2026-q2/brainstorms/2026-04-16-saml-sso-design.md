# SAML SSO + Unified SSO Configuration Design

**Date:** 2026-04-16
**Status:** Approved
**Scope:** Add SAML 2.0 SP support alongside existing OIDC; unify SSO configuration model; add configurable default role

## Goals

1. Support SAML 2.0 HTTP-POST binding so enterprises using SAML IdPs (Azure AD, Okta, ADFS, OneLogin) can authenticate
2. Allow OIDC and SAML to be enabled simultaneously — login page renders buttons dynamically
3. Add a configurable `defaultRole` per protocol (fallback when no group mapping matches; `null` = reject login)
4. Expose SP metadata endpoint + UI display for easy IdP onboarding
5. All production code paths fully implemented — no stubs, TODOs, or placeholder logic

## Non-Goals

- SAML HTTP-Redirect binding (can be added later)
- Encrypted assertions (`wantAssertionEncrypted` field reserved but not implemented)
- SAML Single Logout (SLO)
- Multiple IdPs of the same protocol type (only one OIDC + one SAML)
- Agent-side SAML (agents continue using OIDC only)

---

## 1. Data Model — Unified `sso.config`

Stored in `system_metadata` under key `sso.config`. Replaces the current `oidc.config` key.

```jsonc
{
  "oidc": {
    "enabled": true,
    "displayName": "Azure AD (OIDC)",
    "issuer": "https://login.microsoftonline.com/{tenant}/v2.0",
    "jwksUri": "https://login.microsoftonline.com/{tenant}/discovery/v2.0/keys",
    "clientId": "...",
    "clientSecret": "...",
    "redirectUri": "https://nexus.example.com/api/admin/auth/sso/oidc/callback",
    "authorizeUrl": "",
    "tokenUrl": "",
    "audience": "...",
    "emailClaim": "email",
    "groupClaim": "groups",
    "groupRoleMap": { "admins": "super_admin", "compliance": "compliance_admin" },
    "defaultRole": "viewer"
  },
  "saml": {
    "enabled": false,
    "displayName": "Okta (SAML)",
    "idpMetadataUrl": "",
    "idpEntityId": "https://sts.windows.net/{tenant}/",
    "idpSsoUrl": "https://login.microsoftonline.com/{tenant}/saml2",
    "idpCert": "-----BEGIN CERTIFICATE-----\n...",
    "spEntityId": "https://nexus.example.com/saml",
    "emailAttribute": "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress",
    "groupAttribute": "http://schemas.microsoft.com/ws/2008/06/identity/claims/groups",
    "groupRoleMap": { "sre-team": "super_admin" },
    "defaultRole": "viewer",
    "signAuthnRequest": false,
    "wantAssertionEncrypted": false
  }
}
```

### defaultRole Semantics

| Value | Behavior |
|---|---|
| `"viewer"`, `"provider_admin"`, `"compliance_admin"`, `"super_admin"` | Fallback role when no group matches |
| `null` or `"none"` | Reject login with 403 `NO_ROLE_MAPPING` |

### Migration Strategy

- Backend reads `sso.config` first; if absent, falls back to reading `oidc.config` and wraps it as `{ oidc: <old>, saml: { enabled: false } }`
- First save via the new PUT endpoint writes `sso.config`; old `oidc.config` key is preserved (not deleted) for rollback safety
- No database schema migration required

---

## 2. API Endpoints

### Configuration Management (requires `admin:UpdateSettings`)

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/admin/settings/sso` | Returns full `sso.config`; sensitive fields (`clientSecret`, `idpCert`) masked as `"***"` |
| `PUT` | `/api/admin/settings/sso` | Updates full `sso.config`; `"***"` values mean "keep existing" |
| `POST` | `/api/admin/settings/sso/test` | Test OIDC JWT token validation (new backend handler for existing frontend call) |
| `POST` | `/api/admin/settings/sso/saml/fetch-metadata` | Fetch IdP metadata XML from URL, parse and return `{ idpEntityId, idpSsoUrl, idpCert }` |

### OIDC Authentication Flow (path migration)

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/admin/auth/sso/oidc/authorize` | Initiate OIDC authorization code flow |
| `GET` | `/api/admin/auth/sso/oidc/callback` | OIDC callback |
| `GET` | `/api/admin/auth/sso/authorize` | 301 redirect to new path (backwards compat) |
| `GET` | `/api/admin/auth/sso/callback` | 301 redirect to new path (backwards compat) |

### SAML Authentication Flow (new)

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/admin/auth/saml/metadata` | SP metadata XML (public, no auth required) |
| `GET` | `/api/admin/auth/saml/login` | Generate AuthnRequest, redirect to IdP SSO URL |
| `POST` | `/api/admin/auth/saml/acs` | Assertion Consumer Service — HTTP-POST binding callback |

### SSO Provider Discovery (public)

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/admin/auth/sso/providers` | Returns enabled SSO providers for login page rendering |

Response:
```json
{
  "providers": [
    { "type": "oidc", "label": "Azure AD (OIDC)", "authorizeUrl": "/api/admin/auth/sso/oidc/authorize" },
    { "type": "saml", "label": "Okta (SAML)", "loginUrl": "/api/admin/auth/saml/login" }
  ]
}
```

---

## 3. SAML Authentication Flow (Backend)

### Login Initiation

```
GET /api/admin/auth/saml/login
  → Load SAML config from sso.config
  → Validate SAML is enabled
  → Build crewjam/saml.ServiceProvider
  → Generate AuthnRequest with unique ID
  → Store request ID in session store (key: "saml_request:<id>", TTL: 5 min) for InResponseTo validation
  → Generate CSRF state, store alongside
  → Redirect to IdP SSO URL with SAMLRequest query parameter
```

### ACS (Assertion Consumer Service)

```
POST /api/admin/auth/saml/acs
  → Receive SAMLResponse (base64-encoded form field)
  → Load SAML config from sso.config
  → Build crewjam/saml.ServiceProvider
  → Call sp.ParseResponse(samlResponse, allowedRequestIDs)
    - Validates Response-level OR Assertion-level signature (at least one)
    - Validates time conditions (NotBefore / NotOnOrAfter)
    - Validates Audience matches SP Entity ID
    - Validates Destination matches ACS URL
    - Validates InResponseTo matches stored request ID (anti-replay)
  → Delete used request ID from session store
  → Extract NameID as subject
  → Extract email from configured emailAttribute
  → Extract groups from configured groupAttribute
  → Call completeSSO() shared logic
```

### SP Metadata

```
GET /api/admin/auth/saml/metadata
  → Load SAML config
  → Build crewjam/saml.ServiceProvider (with SP cert + key)
  → Return sp.Metadata() as application/xml
```

### SP Key Pair

- Auto-generated self-signed RSA 2048 X.509 certificate on first SAML enable
- Stored in `system_metadata["saml.sp_keypair"]` as `{ cert: "PEM...", key: "PEM..." }`
- Used for: metadata endpoint SP cert, optional AuthnRequest signing
- Admin can upload custom cert/key pair via the settings UI (future enhancement, not first version)

---

## 4. Shared SSO Logic — `sso_common.go`

Extract common post-authentication logic used by both OIDC callback and SAML ACS:

```go
type SSOIdentity struct {
    Subject     string
    Provider    string
    Email       string
    DisplayName string
    Groups      []string
    GroupRoleMap map[string]string
    DefaultRole *string // nil = reject
}

func (h *AuthHandler) completeSSO(c echo.Context, id SSOIdentity) error {
    // 1. Resolve role: iterate id.Groups against id.GroupRoleMap
    //    - First match wins
    //    - No match: use id.DefaultRole if non-nil, else reject with 403
    // 2. FindOrCreateSSOUser(subject, provider, email, displayName)
    // 3. Create session record with resolved role
    // 4. Generate refresh token
    // 5. Set session + CSRF cookies
    // 6. Redirect to /
}
```

OIDC `SSOCallback` refactored to use `completeSSO()`. SAML `SAMLAcs` also calls `completeSSO()`.

---

## 5. Frontend Changes

### Login Page (`LoginPage.tsx`)

- On mount: `GET /api/admin/auth/sso/providers`
- Render one button per enabled provider with `label` as button text
- Click triggers `window.location.href = authorizeUrl` (OIDC) or `loginUrl` (SAML)
- No providers enabled: SSO section hidden

### Settings SSO Tab (`SettingsSsoTab.tsx`)

Restructured into collapsible sections:

1. **OIDC Configuration** — existing fields + new: `displayName`, `defaultRole` selector
2. **SAML Configuration** — new section:
   - Enable toggle
   - Display Name
   - IdP Metadata URL + "Fetch Metadata" button (calls `/sso/saml/fetch-metadata`, auto-fills IdP Entity ID / SSO URL / Cert)
   - IdP Entity ID, IdP SSO URL, IdP Certificate (textarea, PEM)
   - SP Entity ID (editable, default derived from app URL)
   - Sign AuthnRequest (checkbox)
   - Email Attribute, Group Attribute
   - Default Role selector (role dropdown + "Reject unmapped users" option)
   - Group → Role Mapping (same pattern as OIDC)
3. **SP Information** (read-only card):
   - Entity ID, ACS URL, Metadata URL displayed as copyable text
   - "Download Metadata XML" button (fetches from `/api/admin/auth/saml/metadata`)
4. **Test Token** (OIDC) — existing, now with backend handler

### API Service (`system.ts`)

```typescript
// Unified SSO config type
export interface SsoConfig {
  oidc: OidcProviderConfig;
  saml: SamlProviderConfig;
}

export interface OidcProviderConfig {
  enabled: boolean;
  displayName: string;
  issuer: string;
  jwksUri: string;
  clientId: string;
  clientSecret: string;
  redirectUri: string;
  authorizeUrl: string;
  tokenUrl: string;
  audience: string;
  emailClaim: string;
  groupClaim: string;
  groupRoleMap: Record<string, AdminRole>;
  defaultRole: AdminRole | null;
}

export interface SamlProviderConfig {
  enabled: boolean;
  displayName: string;
  idpMetadataUrl: string;
  idpEntityId: string;
  idpSsoUrl: string;
  idpCert: string;
  spEntityId: string;
  emailAttribute: string;
  groupAttribute: string;
  groupRoleMap: Record<string, AdminRole>;
  defaultRole: AdminRole | null;
  signAuthnRequest: boolean;
}

// API methods
getSsoConfig: () => api.get<SsoConfig>('/api/admin/settings/sso'),
updateSsoConfig: (input: Partial<SsoConfig>) => api.put<SsoConfig>('/api/admin/settings/sso', input),
testSsoToken: (token: string) => api.post<SsoTestResponse>('/api/admin/settings/sso/test', { token }),
fetchSamlMetadata: (url: string) => api.post<{ idpEntityId: string; idpSsoUrl: string; idpCert: string }>('/api/admin/settings/sso/saml/fetch-metadata', { url }),
```

### i18n

All new UI text added to `en/zh/es` locale files under `pages.settingsSso.*` and `pages.login.*` namespaces.

---

## 6. File Change List

### New Files

| File | Description |
|---|---|
| `packages/control-plane/internal/handler/sso_common.go` | Shared logic: `completeSSO()`, `SSOIdentity`, role mapping with defaultRole |
| `packages/control-plane/internal/handler/saml.go` | SAML handlers: `SAMLLogin`, `SAMLAcs`, `SAMLMetadata` |
| `packages/control-plane/internal/handler/saml_test.go` | SAML handler unit tests |
| `packages/control-plane/internal/handler/sso_common_test.go` | completeSSO unit tests (defaultRole=null reject, group mapping, etc.) |

### Modified Files

| File | Changes |
|---|---|
| `packages/control-plane/go.mod` | Add `github.com/crewjam/saml` dependency |
| `packages/control-plane/internal/middleware/jwt.go` | `OidcConfig` add `DefaultRole *string`, `DisplayName string` |
| `packages/control-plane/internal/handler/sso.go` | Refactor OIDC handlers to use `completeSSO()`; update path to `/sso/oidc/*` |
| `packages/control-plane/internal/handler/auth_routes.go` | Register SAML routes + OIDC 301 redirects + `/auth/sso/providers` |
| `packages/control-plane/internal/handler/admin_settings.go` | Config key migration `oidc.config` → `sso.config`; new `FetchSAMLMetadata` handler; new `TestSSOToken` handler; mask sensitive fields on GET |
| `packages/control-plane-ui/src/api/services/system.ts` | `OidcConfig` → `SsoConfig` unified type; SAML API methods |
| `packages/control-plane-ui/src/pages/settings/SettingsSsoTab.tsx` | Restructure to OIDC + SAML dual sections + SP info + defaultRole |
| `packages/control-plane-ui/src/auth/LoginPage.tsx` | Call `/auth/sso/providers` for dynamic SSO button rendering |
| `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/pages.json` | SAML i18n keys |

### No Changes Required

- **Database schema** — no migration; reuses `system_metadata` + existing `NexusUser.ssoSubject/ssoProvider`
- **Agent API** — agent OIDC auth unchanged
- **Compliance Proxy / AI Gateway** — not affected

### Dependencies

| Dependency | Purpose |
|---|---|
| `github.com/crewjam/saml` (latest stable) | SAML SP core: XML signature validation, metadata generation, AuthnRequest construction |

Transitive: `github.com/beevik/etree` (XML), `github.com/russellhaering/goxmldsig` (XML DSig), `github.com/mattermost/xml-roundtrip-validator`.

---

## 7. Security Considerations

- **XML Signature Wrapping**: Delegated to `crewjam/saml` which has protections against known wrapping attacks
- **Replay Protection**: `InResponseTo` validation against stored request IDs; request IDs expire after 5 minutes
- **CSRF**: State parameter stored server-side for both OIDC and SAML flows
- **Sensitive Field Masking**: `clientSecret` and `idpCert` masked in GET responses; `"***"` on PUT means "keep existing"
- **SP Key Storage**: SP private key stored in `system_metadata` — same security boundary as other secrets in the system
- **Certificate Validation**: IdP certificate is parsed and validated on config save; invalid PEM rejected

---

## 8. Testing Strategy

- **Unit Tests**: `sso_common_test.go` — role mapping with defaultRole, reject on null, group priority; `saml_test.go` — metadata generation, ACS response parsing with mock assertions
- **Integration**: Manual testing with a real IdP (Okta developer account or Azure AD free tier) — document steps in a runbook
- **Regression**: Existing OIDC flow must continue working after refactor; old redirect paths return 301
