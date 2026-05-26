# E44 — External Identity Provider Federation (SDD)

**Epic:** E44
**Requirements:** [e44-external-idp-federation.md](../../../../docs/developers/specs/e44/e44-external-idp-federation.md)
**OpenAPI:** [e44-s01-identity-providers.yaml](../../../../docs/users/api/openapi/auth/e44-s01-identity-providers.yaml)
**Status:** Approved (auto-approved per user's "execute all" directive 2026-05-13)
**Date:** 2026-05-13

---

## Architecture summary

The `IdentityProvider` table becomes the canonical home for external IdP configurations. The pre-E44 single-blob `SystemMetadata["sso.config"]` is deleted. Each row represents one (vendor, protocol) pair: e.g., Okta-as-OIDC and Okta-as-SAML are two rows if both protocols are configured. The seed `local` row remains in the DB but is hidden from the UI and is used solely as the parent of `UserFederatedIdentity` rows for password-based local admins (and to power the local-password fallback when no external IdPs are enabled).

Login routing carries `IdPID` through `PendingAuthzEntry` so the callback handler attributes tokens to the correct IdP. The OIDC begin handler accepts `?idp_id=<uuid>`; if absent and exactly one OIDC IdP is enabled, it defaults to that single row (back-compat for single-IdP installs). SAML begin/ACS handlers are net-new (greenfield) and follow the same `IdPID`-aware pattern via SAML RelayState.

SCIM provisioning gains attribution: the SCIM token's `identityProviderId` is propagated onto the new `NexusUser` via a `UserFederatedIdentity` row, and SCIM Group creation/membership consults `IdpGroupMapping` to write `IamGroupMembership` against the mapped Nexus IAM group instead of inserting unrelated local IamGroup rows.

The admin UI page `/system/identity-provider` moves to `/iam/identity-providers`, redesigned with an explanatory intro block, an "Add IdP" wizard, and per-IdP cards whose SCIM and Group Mapping sub-sections collapse by default. The `/settings` page's `sso` tab is deleted.

---

## Story breakdown

### S01 — IdentityProvider CRUD + Test endpoints (backend)

**User story:** As an org admin, I can create, update, delete, and test external IdPs via a stable REST surface.

**Tasks:**

- **T01.1** — Add IAM resource. In `packages/shared/security/iam/catalog_data.go`, add `{Name: "identity-provider", Service: ServiceIAM, Verbs: [VerbCreate, VerbRead, VerbUpdate, VerbDelete, VerbProbe]}` and the convenience var `ResourceIdentityProvider = MustFind("identity-provider")`.
- **T01.2** — Store: add `packages/control-plane/internal/store/identity_provider_crud.go` with `Create`, `Update`, `Delete`, `GetByID`. Hand-written SQL. Secrets fields are encrypted before write via `internal/crypto/aes_gcm.go`.
- **T01.3** — Handler: extend `packages/control-plane/internal/handler/admin_identity_provider.go` with `Create`, `Update`, `Delete`, `TestSaved`, `TestCandidate`. Mask secrets in response payloads. Emit audit entries (`audit.EntryFor(c, iam.ResourceIdentityProvider, …)`).
- **T01.4** — Test probe implementation: a small `idptest` subpackage under `internal/authserver/idptest/` that, given an OIDC or SAML config, fetches discovery / metadata over `http.Client` (timeout 10s), validates JWKS reachability, parses any returned cert, returns a structured probe result.
- **T01.5** — Route registration. Update `RegisterIdentityProviderRoutes` to mount POST `/identity-providers`, PUT/DELETE `/identity-providers/:id`, POST `/identity-providers/test`, POST `/identity-providers/:id/test`. All gated by `iam.ResourceIdentityProvider.Action(VerbCreate|VerbUpdate|VerbDelete|VerbProbe)`.

**Acceptance:**

- AC-S01.1 — POST with a valid OIDC body creates a row; response masks `clientSecret` to `********`; row is visible via GET list.
- AC-S01.2 — PUT preserves unchanged secret when `clientSecret = "********"` is sent; replaces when a new value is sent.
- AC-S01.3 — DELETE on an IdP with linked `UserFederatedIdentity` returns 409 unless `?force=true`; with `force` it deletes, cascades `IdpGroupMapping`, revokes scoped `ScimToken`, fans out session revocations.
- AC-S01.4 — POST `/test` (candidate) and POST `/:id/test` (saved) both return `{ok, detail}` or `{ok:false, error}` within 10s; results are audited with `VerbProbe`.

---

### S02 — Multi-IdP login routing (OIDC)

**User story:** As an end user, when multiple external IdPs are configured, I can choose which one to authenticate against.

**Tasks:**

- **T02.1** — Add `IdPID string` to `PendingAuthzEntry` in `packages/control-plane/internal/authserver/store/pending.go`.
- **T02.2** — `OIDCBeginHandler` (`internal/authserver/login/oidc.go:31`) accepts and validates `idp_id` from the query. If absent and there is exactly one enabled OIDC IdP, defaults to that row's id; otherwise returns 400. Stamps `pending.IdPID = idpID`.
- **T02.3** — `OIDCCallbackHandler` (`oidc.go:67`) reads `pending.IdPID`, loads IdP via `IdPStore.GetByID(idpID)` (already exists, line 58), uses **that row's config** for JWKS validation + JIT provisioning. Replaces all calls to `IdPStore.GetOIDC()`.
- **T02.4** — `IdPStore.GetOIDC()` deleted (`packages/control-plane/internal/authserver/store/idp_store.go:75`). All callers refactored to use `GetByID`.
- **T02.5** — `JWKSCache` becomes a registry keyed by `idpID`. Per-IdP caches lazy-populated. Located at `internal/authserver/login/jwks_registry.go`.
- **T02.6** — `IdpsResponse` (`login/idps.go`) already returns multiple rows; no change. Frontend `/login` page renders one "Sign in with `<name>`" button per row + an optional local-fallback link.

**Acceptance:**

- AC-S02.1 — With 2 enabled OIDC IdPs, GET `/authserver/idps` returns both; clicking each produces a different upstream redirect.
- AC-S02.2 — A callback to `/authserver/oidc/callback?state=authctx-A` validates against IdP A's JWKS; a concurrent `state=authctx-B` validates against IdP B's. No cross-talk.
- AC-S02.3 — With exactly 1 enabled OIDC IdP, `idp_id` may be omitted (back-compat with the simplest deployments).

---

### S03 — Delete legacy SSO single-blob path

**User story:** As a maintainer, the SSO configuration is read exclusively from `IdentityProvider` rows; there is no single-blob fallback.

**Tasks:**

- **T03.1** — Delete `packages/control-plane/internal/handler/sso_config.go` entirely.
- **T03.2** — Delete `packages/control-plane/internal/authserver/store/sso_config_store.go` entirely.
- **T03.3** — Remove `/api/admin/settings/sso`, `/sso/test`, `/sso/saml/fetch-metadata` routes from `admin_settings.go`. The SAML metadata-parse helper moves into `idptest`.
- **T03.4** — Remove `ssoConfigKeyV2`, `ssoConfigKeyV1`, and `samlSPKeyPair` constants. Remove `SetSystemMetadata(…, ssoConfigKeyV2, …)` write paths.
- **T03.5** — Delete `LoadFederationConfig` / `LegacyOIDCMigration` if any remain.
- **T03.6** — Tests `sso_config_test.go`, `admin_revocation_triggers_test.go:481-559` rewritten against `IdentityProvider` table.

**Acceptance:**

- AC-S03.1 — `grep -rn "sso_config\|sso.config\|ssoConfigKey" packages/control-plane/` returns zero hits (excluding deleted-file mentions in commit messages).
- AC-S03.2 — `go build ./packages/control-plane/...` and `go test -race -count=1 ./...` are green.

---

### S04 — SCIM provisioning attribution

**User story:** As an IdP operator, SCIM-pushed users are attributable to the source IdP and SCIM-pushed group memberships land in the correct Nexus IAM group.

**Tasks:**

- **T04.1** — `scim.go CreateUser`: after `store.CreateNexusUser`, read the request's `c.Get("scimToken")` (set by `scimAuth`), call `Federated.LinkUser(userID, scimToken.IdentityProviderID, externalSubject=user.ExternalID)`. Set `source='scim'`.
- **T04.2** — Add `IamGroup.source` ('local' | 'scim') column + `IamGroup.identityProviderId` nullable FK. SCIM-managed groups are read-only in the admin UI (write attempts return 400).
- **T04.3** — `scim.go CreateGroup` and `PatchGroup`: when adding members, look up `IdpGroupMapping (scimToken.IdentityProviderID, request.externalId)`. If present, write `IamGroupMembership(group_id=mapping.iamGroupId, principalId=memberUserId, principalType='nexus_user')`. If absent, create a new `IamGroup(source='scim', identityProviderId=scimToken.IdentityProviderID, name=...)` and back-fill the mapping (auto-discovery convenience).
- **T04.4** — `IdpGroupMapping` admin handler emits audit on create/delete.

**Acceptance:**

- AC-S04.1 — POST `/scim/v2/Users` with a valid SCIM token produces a `NexusUser.source='scim'` + a `UserFederatedIdentity` row linking to the token's IdP.
- AC-S04.2 — Pre-existing `IdpGroupMapping(IdP=A, externalGroupId='okta-eng', iamGroupId='X')`. POST `/scim/v2/Groups` for `externalId='okta-eng'` with 3 members lands all 3 members in `IamGroupMembership(groupId='X')`.

---

### S05 — UI redesign + Add-IdP wizard

**User story:** As an admin, I can understand what the page is for, add a new IdP via a guided wizard, and manage its SCIM + group-mapping without nesting confusion.

**Tasks:**

- **T05.1** — Rewrite `packages/control-plane-ui/src/pages/settings/IdentityProviderPage.tsx`: top intro block (collapsible, localStorage `nexus.idp.intro.collapsed`); empty-state CTA; per-IdP card with name + protocol badge + enabled toggle + Test button + Edit + Delete buttons; collapsible "SCIM Provisioning" + "Group → IAM Mapping" sub-sections. The Nexus Local row is not rendered.
- **T05.2** — New `IdentityProviderWizard.tsx` modal: 4 steps — (1) Name + protocol picker, (2) protocol-specific fields, (3) Test connection, (4) Save + optional auto-generate first SCIM token.
- **T05.3** — API service `packages/control-plane-ui/src/api/services/identityProvider.ts`: extend with `create`, `update`, `delete`, `testCandidate`, `testSaved`. Existing list, SCIM-token, group-mapping endpoints stay.
- **T05.4** — Delete `SettingsSsoTab` and its lazy entry. Remove `sso` tab key from `SettingsPage.tsx`.
- **T05.5** — i18n: en/zh/es `pages.json` `identityProvider.*` block — replaced with the new copy. Add new keys for the wizard, the intro block, the empty state. Copy to `public/locales/`. Verify key counts match across all three.

**Acceptance:**

- AC-S05.1 — Navigate to `/iam/identity-providers` (new route). With zero IdPs, see the intro + the "Add Identity Provider" CTA, no other rows.
- AC-S05.2 — Wizard creates an OIDC IdP that immediately shows on the list.
- AC-S05.3 — Click "Test" on a saved row → modal renders `{ok, detail}` from the probe response.
- AC-S05.4 — Locale switcher cycles en/zh/es; no key reads `pages:identityProvider.<key>` as raw key.

---

### S06 — Nav reorganisation

**User story:** As an admin, IdP/SSO/SCIM features live under a clear "Identity & Authentication" home, not buried in a generic Settings page.

**Tasks:**

- **T06.1** — Add `/iam/identity-providers` route in `shellRouteConfig.tsx`; move IdP nav entry from `sectionKey:'system'` to `sectionKey:'iam'`. Delete `/system/identity-provider` (or redirect for one release — N/A here per greenfield rule).
- **T06.2** — Delete `sso` tab from `SettingsPage.tsx`. The remaining 8 tabs stay (out of scope for E44; tracked as E45 follow-up).
- **T06.3** — Update `nav.json` (en/zh/es) labels: rename `identityProvider` to a clearer key if needed.

**Acceptance:**

- AC-S06.1 — IAM nav section shows "Identity Providers" alongside Users / Orgs / Projects / Roles / Policies / Simulator.
- AC-S06.2 — `/settings` page no longer renders an SSO tab.

---

### S07 — IAM action catalog + managed policies

**User story:** As a security admin, the new IdP CRUD verbs are correctly assignable through the existing managed policies + Policy Editor.

**Tasks:**

- **T07.1** — Catalog addition (done in T01.1).
- **T07.2** — Update `tools/db-migrate/seed/seed.ts:1122-1129` `NexusSecurityAdminAccess.policyDocument` — append `admin:identity-provider.read` to the existing Statement.
- **T07.3** — Update `packages/control-plane/internal/iam/managed.go:65-78` `NexusViewer` fixture — append `admin:identity-provider.read` (the E43 P6 coverage test requires every catalog resource's read action).
- **T07.4** — Update `packages/control-plane-ui/src/hooks/usePermission.ts` ACTION_MAP — add `idp:read → admin:identity-provider.read`, `idp:write → admin:identity-provider.update`, `idp:create → admin:identity-provider.create`, `idp:delete → admin:identity-provider.delete`, `idp:probe → admin:identity-provider.probe`.
- **T07.5** — Reseed DB (dev).

**Acceptance:**

- AC-S07.1 — `go test ./packages/shared/security/iam/...` is green (catalog round-trip tests).
- AC-S07.2 — Catalog Picker lists "identity-provider" under `iam` service.

---

### S08 — End-to-end verify

**Tasks:**

- **T08.1** — Restart all 4 services with rebuilt binaries.
- **T08.2** — Smoke-test: `tests/scripts/smoke-gateway.py` (verify AI Gateway /v1/chat/completions still serves; auth flow unchanged for vk-auth path).
- **T08.3** — Manual: load `/iam/identity-providers`, verify empty state. Wizard-create an OIDC IdP with a known issuer (e.g., a local test OIDC stub or a public discovery URL). Verify list reflects the row.
- **T08.4** — Hit `/login` (CP UI) and verify "Sign in with `<idp>`" button appears.
- **T08.5** — POST a SCIM User against `/scim/v2/Users` with a generated token; verify the new user lands with `source='scim'` and a `UserFederatedIdentity` row.
- **T08.6** — Toggle an IdP off; verify within 5s an existing session's API call returns 401.
- **T08.7** — Cross-locale check: switch en → zh → es; verify no raw key strings.

**Acceptance:** All AC- entries in S01–S07 pass.

---

## Risk register

- **R1: Login flow regression for the single-IdP common case.** Mitigated by S02 default-to-single rule (idp_id may be omitted if exactly one OIDC enabled).
- **R2: SAML implementation completeness.** Today only metadata parse exists. Adding ACS handler is greenfield code; needs careful round-trip testing with a known-good IdP. Scoped as **stretch goal** — if SAML ACS doesn't land in this Epic, the schema still supports it and S02 covers OIDC fully.
- **R3: SCIM provisioning subtleties.** RFC 7644 PATCH operations are tricky; the existing handler already implements them, so the attribution add (T04.1) sits on a working foundation. T04.3 group-mapping logic is the riskier addition; covered by AC-S04.2.
- **R4: Cascade delete breaks audit trail.** When an IdP is force-deleted, the `UserFederatedIdentity` rows go with it; downstream audit queries that JOIN against `UserFederatedIdentity` will lose the linkage. Mitigation: audit log entry preserves a JSON snapshot of the IdP row at delete time so forensic queries can resolve the historical id.
