# E44 — External Identity Provider Federation

**Epic:** E44
**Status:** Draft
**Date:** 2026-05-13

---

## Background

Nexus Gateway is a **Service Provider (SP)** in OIDC/SAML terminology. It does not host its own identity store as the primary source of truth for organisations; instead it federates with **external Identity Providers (IdPs)** such as Okta, Azure AD, Google Workspace, JumpCloud, OneLogin. End users authenticate at their company IdP and are then provisioned (manually or via SCIM) into Nexus.

Pre-E44 the platform supports federation, but only as a single-blob singleton: `SystemMetadata["sso.config"]` holds at most one OIDC and one SAML config; runtime code calls `IdPStore.GetOIDC()` which executes `SELECT ... LIMIT 1`. The `IdentityProvider` table exists but only ever holds a seeded `local` row used as a parent for `UserFederatedIdentity` rows of password-based admins. Multi-IdP is not expressible, and the admin UI `/system/identity-provider` renders SCIM/group-mapping sections under the dormant local row, which is confusing.

E44 makes external-IdP federation a first-class, multi-instance feature.

---

## Glossary

- **SP (Service Provider)** — Nexus Gateway itself. We never refer to Nexus as an IdP.
- **IdP (Identity Provider)** — External authentication source (Okta, Azure AD, Google, …). May support OIDC, SAML, or both.
- **Federation** — The act of an SP delegating end-user authentication to an IdP.
- **OIDC** — OpenID Connect, an OAuth 2.0 superset for SSO.
- **SAML 2.0** — XML-based SSO assertion protocol; orthogonal to OIDC.
- **SCIM 2.0** — Provisioning protocol. The IdP pushes Users/Groups into Nexus. Nexus exposes `/scim/v2/*` (SCIM target).
- **Nexus Local** — Built-in password authn that activates as the implicit fallback for admin sign-in when no external IdP is enabled. **Not modeled as a peer IdP row in the UI list.** May also serve as the authentication source for **device enrollment** when the operator picks the `local-login` device-auth mode (see FR-13 below) — this is a distinct mode of operation, not a promotion of Nexus Local to peer-IdP status.
- **Device-auth mode** — One of `mtls-only`, `enterprise-login`, or `local-login`; stored at `system_metadata.device.auth.mode` and managed from the admin **Settings → Device Authentication** page. Selects how desktop agents enroll.
- **Enterprise Login** — Device-auth mode in which agents launch a browser SSO flow that authenticates the user against an **external IdP** (OIDC or SAML). Requires at least one enabled non-local IdP.
- **Local Login** — Device-auth mode in which agents launch the same browser SSO flow but authenticate the user against the **Nexus Local** password store. Suitable for small / single-tenant / demo deployments that do not run an external IdP. Gated by the new IAM action `admin:device-enrollment.enroll`.

---

## User Roles

- **Org admin (security)** — Configures IdPs, generates SCIM tokens, maps IdP groups to Nexus IAM groups. Needs `admin:identity-provider.*`.
- **End user** — Signs into Nexus via "Sign in with Okta" (or company password if no IdP is enabled). Does not see this page.
- **Auditor** — Reads IdP list + SCIM tokens + group mappings. Needs `admin:identity-provider.read`.

---

## Functional Requirements

### FR-1 — List external IdPs
The system shall list every `IdentityProvider` row whose row type designates an external IdP (OIDC or SAML), ordered by `createdAt`. Local-fallback rows are not surfaced in this listing.

### FR-2 — Create an IdP via wizard
The system shall accept `POST /api/admin/identity-providers` with one of two protocol payloads: OIDC (issuer, jwks_uri, client_id, client_secret, audience, redirect_uri, authorize_url, token_url, email_claim, group_claim, scopes) or SAML (entity_id, sso_url, certificate_pem, signature_algorithm). The handler shall persist the row and return the created entity (with `client_secret` and `certificate_pem` masked).

### FR-3 — Update an IdP
`PUT /api/admin/identity-providers/:id` shall accept the same payload shape as create, atomically replacing the row. Secrets sent as `********` shall be ignored (unchanged from current).

### FR-4 — Delete an IdP
`DELETE /api/admin/identity-providers/:id` shall remove the IdP row, cascade-delete its `IdpGroupMapping` rows, revoke every `ScimToken` scoped to it, and queue a session revocation for every `NexusUser` linked via `UserFederatedIdentity.idpId`.

### FR-5 — Test an IdP before save
`POST /api/admin/identity-providers/test` shall accept a candidate (unsaved) IdP payload and verify connectivity (OIDC: discovery doc + JWKS reachability; SAML: metadata + cert parse). Returns `{ok, detail | error}`. Does not persist.

### FR-6 — Test a saved IdP
`POST /api/admin/identity-providers/:id/test` shall run the same probe against a saved row. Optionally accepts a body `{token}` to validate a JWT against the configured JWKS (replaces today's `POST /api/admin/settings/sso/test`).

### FR-7 — Multi-IdP-aware login
The login flow shall route through the chosen IdP id. Both the begin handler (`/authserver/oidc/begin`) and the callback (`/authserver/oidc/callback`) shall persist and read `IdPID` from `PendingAuthzEntry` so callbacks correctly attribute the token to its issuing IdP.

### FR-8 — Local fallback when no IdP is enabled
If `IdentityProvider` has zero rows with `enabled=true`, the `/login` page shall render the local password form as the only option. Otherwise it shall render "Sign in with `<idp.name>`" buttons per enabled IdP plus an optional "Use local admin password" link gated by an admin-only flag.

### FR-9 — SCIM provisioning attribution
`POST /scim/v2/Users` shall stamp `UserFederatedIdentity` linking the new user to the SCIM token's `identityProviderId`, set `NexusUser.source = 'scim'`, and use the IdP's `defaultRole`. `POST /scim/v2/Groups` (and `PATCH /Groups/:id` for membership adds) shall consult `IdpGroupMapping (idpId, externalGroupId)` to add the user to the mapped `IamGroup` instead of creating a new local IamGroup per external group.

### FR-10 — UI: redesigned IdP page
The `/system/identity-provider` page shall be redesigned with: intro "how it works" block (collapsible, expanded on first visit, localStorage-pinned thereafter); empty-state CTA when no IdPs configured; per-IdP card with collapsed-by-default SCIM section and Group Mapping section; the seed "Nexus Local" row is not rendered (or rendered as a minimal informational note). An "Add Identity Provider" wizard shall accept name + protocol + protocol-specific config + test-before-save.

### FR-11 — Nav reorganisation
The IdP page shall move from `/system/identity-provider` to `/iam/identity-providers` (under the IAM nav section). The `/settings` page's `sso` tab shall be removed; the per-IdP wizard is the canonical replacement. Existing SSO config (if any) shall be deleted (greenfield per CLAUDE.md).

### FR-12 — IAM action catalog
A new `iam.ResourceIdentityProvider` shall be added under `ServiceIAM` with verbs `Create/Read/Update/Delete/Probe`. Managed policies `NexusSecurityAdminAccess` shall gain `identity-provider.read`; `NexusViewer` fixture shall gain `identity-provider.read`; `NexusAdminFullAccess` / `NexusIAMAdmin` auto-cover via wildcards.

### FR-13 — Local-login device-auth mode
The system shall expose a third **device-auth mode** value `local-login` (alongside `mtls-only` and `enterprise-login`) at `PUT /api/admin/settings/device-auth`. When active, desktop agents that have not yet enrolled launch the same browser SSO flow as `enterprise-login`, but the CP login page authenticates the user against the Nexus Local password store (no external IdP required). The validator shall accept `local-login` only when the seeded local IdP row exists with `enabled=true`; the response shape of `GET /api/admin/settings/device-auth` shall include a new boolean `localLoginAvailable` reflecting this. Hub's `/api/public/agent-bootstrap` shall normalise `local-login` to `enterprise-login` in its public response so existing agent UIs handle both modes through a single SSO branch.

### FR-14 — Enrollment endpoint IAM gate
`POST /api/agent/sso-enroll` shall enforce that the user behind the consumed auth code holds the new IAM action `admin:device-enrollment.enroll`. The action lives on a carved-out `device-enrollment` resource under `ServiceIAM` with a single verb `Enroll`, so the privilege can be granted in isolation without implying any other admin capability. The gate applies uniformly to **both** `enterprise-login` and `local-login` paths — fixing the prior gap where any authenticated CP user (including `viewer`) could enroll devices. Managed policies `NexusSuperAdmin` and `NexusProviderAdmin` shall grant the action; `NexusComplianceAdmin` and `NexusViewer` shall not. Denied requests return HTTP 403 with `{"error":"iam_denied"}`.

---

## Non-Functional Requirements

- **NFR-1 (security):** Client secrets and SAML private keys are encrypted at rest (AES-256-GCM via the same key the credential store uses). Returned to the UI masked. Visible in plaintext only to the binary owner of the keystore.
- **NFR-2 (audit):** Every CRUD + Probe + Test endpoint emits an `AdminAuditLog` entry with `entityType='identity-provider'`, `entityId=idpID`, before/after snapshots (Test/Probe carry no diff).
- **NFR-3 (consistency):** When an IdP transitions from enabled to disabled OR is deleted, every active session (RefreshToken + access JWT) tied to its users is revoked within 5 seconds.
- **NFR-4 (greenfield):** Per CLAUDE.md, no backwards-compatibility shim. `SystemMetadata["sso.config"]` and `SystemMetadata["oidc.config"]` are deleted. The `local` IdP seed remains for the fallback path only.

---

## Constraints

- **English-only artifacts.** All code, comments, OpenAPI descriptions, and `docs/` content is English. i18n locale files (en/zh/es) carry user-visible translations.
- **No data-migration code.** Per CLAUDE.md, dev DB is re-seeded; production deploys are not yet a concern.
- **No defer/mocks in production code.** Tests may mock IdP HTTP responses, but the production probe path uses a real `http.Client` against real discovery / metadata URLs.

---

## Out of Scope

- Multi-tenant `IdentityProvider.organizationId` scoping. Today every IdP is platform-global; making them org-scoped is its own future epic (Phase 2 of multi-tenancy).
- MFA / step-up enforcement at the SP layer. Today MFA is delegated entirely to the IdP.
- SAML SP-initiated SLO (Single Logout). Out of scope for first ship.
- Bulk SCIM operations (`/scim/v2/Bulk`). RFC 7644 optional, no IdP we target requires it.
- Inbound SAML JIT (auto-create users on first SAML login). OIDC has it via `jitEnabled`; SAML keeps current behavior (admin must SCIM-provision first or the login is rejected).

---

## Acceptance Criteria

- **AC-1** — Listing the IdP page with no IdPs configured shows the empty state + "Add Identity Provider" CTA; the local password login form remains usable.
- **AC-2** — Adding an OIDC IdP via the wizard, with valid issuer/client_id/secret, persists a row visible in the list; clicking "Sign in with `<name>`" on `/login` initiates the OIDC redirect against the new IdP.
- **AC-3** — Adding a second OIDC IdP for a different vendor concurrently does not break either flow; each IdP's `pending.IdPID` round-trips correctly.
- **AC-4** — Generating a SCIM token under an IdP, POSTing a SCIM User against `/scim/v2/Users`, produces a `NexusUser` with `source='scim'` and a `UserFederatedIdentity` row keyed to that IdP.
- **AC-5** — Adding an `IdpGroupMapping (idpId, externalGroupId=okta-eng, iamGroupId=group-XYZ)`, then SCIM-pushing a Group with that ID and members, results in those `NexusUser` rows landing in IAM group `group-XYZ`.
- **AC-6** — Disabling an enabled IdP revokes every active session for its linked users (verified via 401 on a previously-valid access token within 5s).
- **AC-7** — Deleting an IdP with linked `UserFederatedIdentity` rows refuses unless `?force=true` is passed; with `force` it cascades and revokes.
- **AC-8** — `usePermission('idp:read')` returns true for a user holding `admin:identity-provider.read`; the IAM Policy Editor's Catalog Picker lists the new resource under `ServiceIAM`.
- **AC-9** — `go test -race -count=1 ./packages/control-plane/...` is green; `npx tsc --noEmit` in `control-plane-ui` is green; `npx vitest run` is green.
- **AC-10** — UI page renders correctly across en/zh/es (locale switch works; key counts match across all three).
- **AC-11** — `Settings → Device Authentication` page surfaces three radio options: `mTLS Only`, `Local Login`, `Enterprise Login`. Selecting `Local Login` is savable when the seed local IdP exists and is enabled; selecting `Enterprise Login` remains disabled until ≥1 non-local IdP is enabled.
- **AC-12** — With `system_metadata.device.auth.mode='local-login'`, `GET /api/public/agent-bootstrap` returns `deviceAuthMode='enterprise-login'` (normalised). A fresh agent install advances past the "Contacting the gateway" screen and renders the SSO sign-in button without rebuilding the agent binary.
- **AC-13** — Signing in to the agent's browser tab as `admin@nexus.ai` (super_admin) completes enrollment end-to-end; signing in as `diana@nexus.ai` (viewer) yields HTTP 403 `iam_denied` from `/api/agent/sso-enroll` and an enrollment-failed surface in the agent UI.
