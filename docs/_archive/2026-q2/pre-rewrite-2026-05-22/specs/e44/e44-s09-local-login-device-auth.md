# E44 / S09 — Local-login device-auth mode + uniform enroll RBAC

**Epic:** E44 — External Identity Provider Federation
**Story:** S09
**Requirements:** [e44-external-idp-federation.md](../../../../docs/developers/specs/e44/e44-external-idp-federation.md) (FR-13, FR-14, AC-11/12/13)
**OpenAPI:** [e44-s01-identity-providers.yaml](../../../../docs/users/api/openapi/auth/e44-s01-identity-providers.yaml) (extended)
**Status:** Approved (auto-approved per user directive 2026-05-13)
**Date:** 2026-05-13

---

## User story

As an **operator of a small / demo / single-tenant Nexus deployment** who does not run an external IdP, I want desktop agents to enroll via the same browser-based SSO flow as Enterprise Login, but using **Nexus Local accounts** (e.g. `admin@nexus.ai`) as the identity source — so my devices can self-enroll without setting up Okta/Azure AD.

And:

As a **security admin**, I want the device-enrollment endpoint to require a dedicated IAM permission so that low-privilege CP users (e.g. `viewer`) cannot enroll arbitrary devices into the fleet.

---

## Architecture summary

The story adds a **third device-auth mode** `local-login` stored at `system_metadata.device.auth.mode` alongside the existing `mtls-only` and `enterprise-login`. The mode is purely an admin policy bit; the agent's runtime behaviour and the Hub's enrollment-JWT verification are **unchanged**.

Three reasons it's cheap:

1. The Hub's enrollment-JWT verification (`packages/nexus-hub/internal/handler/enrollment_handler.go:91-150`) is already IdP-agnostic — it only checks the CP issuer, the RSA signature, audience, purpose, and JTI replay. The JWT carries no IdP type, and Hub doesn't care which login form the user used on the CP login page.
2. The CP login page (`/oauth/authorize` → `/login`) already supports both `/authserver/password` (local) and `/authserver/oidc/begin` (external) routes; the choice is made at the moment the user clicks a button on the CP login page, not at agent boot.
3. The agent's onboarding panel branches on the bootstrap-returned `deviceAuthMode` value. By **normalising `local-login` → `enterprise-login`** in Hub's `/api/public/agent-bootstrap` response, the agent's existing two-branch UI (`mtls-only` token panel vs `enterprise-login` SSO button) handles both modes through a single code path. No agent rebuild needed.

The story also introduces a **carved-out IAM resource** `device-enrollment` with a single verb `Enroll`. `POST /api/agent/sso-enroll` resolves the user behind the consumed OAuth auth code and calls the existing `iam.Authorizer` to check `admin:device-enrollment.enroll`. The gate applies **uniformly** to both `enterprise-login` and `local-login` paths, fixing a pre-existing security gap where any authenticated CP user (including `viewer`) could enroll a device.

The SP/IdP positioning rule (memory: "Nexus is the SP; Nexus Local is the implicit fallback, not a peer IdP") is preserved — Nexus Local is **not** promoted to a peer IdP row in the admin IdP page. The new mode is described in the UI as a distinct mode of operation, with an advisory note that production deployments should prefer `enterprise-login` with an external IdP.

---

## Tasks

### T09.1 — IAM catalog: add `device-enrollment` resource

In `packages/shared/security/iam/catalog_data.go`:

- Add a new verb constant `VerbEnroll = "enroll"` if not already present in the verb constants list.
- Add a new resource entry under `ServiceIAM`:
  ```go
  {Name: "device-enrollment", Service: ServiceIAM, Verbs: []string{VerbEnroll}},
  ```
- Add the convenience var `ResourceDeviceEnrollment = MustFind("device-enrollment")` alongside the other `Resource*` vars.

Touch `packages/control-plane/internal/iam/managed.go` to grant `admin:device-enrollment.enroll` in **`NexusSuperAdmin`** and **`NexusProviderAdmin`** managed-policy fixtures. Keep it absent from `NexusComplianceAdmin` and `NexusViewer`. Update the managed-policies test fixture so existing IAM tests stay coherent.

### T09.2 — Admin handler: accept `local-login` mode

In `packages/control-plane/internal/handler/admin_settings.go`:

- Extend the mode validation tuple in `UpdateDeviceAuthSettings` to accept all three values: `mtls-only`, `enterprise-login`, `local-login`.
- Replace the single `if body.Mode == "enterprise-login" && ...` guard with a `switch` over the mode value:
  - `enterprise-login` — existing guard (≥1 enabled non-local IdP); returns 400 `no_sso_provider` if missing.
  - `local-login` — require the seeded local IdP to exist with `enabled=true` (defensive only); returns 400 `local_idp_unavailable` if missing.
- Extend `GetDeviceAuthSettings` to populate a new boolean response field `localLoginAvailable` reflecting the local-IdP presence check.

### T09.3 — Enrollment endpoint: IAM gate

In `packages/control-plane/internal/handler/agent_sso_enroll.go`:

- After the existing user-lookup block (around line 175-186), invoke the existing `iam.Authorizer` to check whether `entry.UserID` holds `admin:device-enrollment.enroll`.
- The endpoint is unauthenticated at the HTTP level (it consumes an OAuth auth code), so the check is performed in-handler rather than via `iamMW` middleware. Resolve the user via the same lookup the handler already performs.
- On deny, log `slog.Warn("sso-enroll: user lacks enroll permission", "user_id", entry.UserID, "email", entry.Email)` and return HTTP 403 with body `{"error":"iam_denied"}`. Apply uniformly regardless of which IdP the user authenticated through.

### T09.4 — Hub bootstrap: normalise `local-login`

In `packages/nexus-hub/internal/handler/agent_bootstrap.go` (`body` method, around line 62-101):

- After reading the raw mode from `system_metadata`, before stamping the response: if `mode == "local-login"`, set `mode = "enterprise-login"` with a short comment explaining that the agent UI is mode-agnostic from `enterprise-login` downward.

Hub's runtime configuration is unchanged; `cpURL` / `cpJWKSURL` / `cpIssuer` already cover the JWT verification path for both `enterprise-login` and `local-login`.

### T09.5 — Seed: grant new IAM action

In `tools/db-migrate/seed/seed.ts`:

- Grant `admin:device-enrollment.enroll` to the `super-admins` and `provider-admins` group-policy entries (following the existing pattern for other `iam.*` resources).
- Re-run `cd tools/db-migrate && npx prisma db seed` so the local DB reflects the new permission.

### T09.6 — UI: third radio + i18n + advisory

In `packages/control-plane-ui/src/pages/settings/DeviceAuthSettingsPage.tsx`:

- Add a third radio option `local-login` between `mtls-only` and `enterprise-login`.
- Add a small advisory note under the `local-login` radio: "Recommended only for small / demo deployments. Production fleets should configure an external IdP and use Enterprise Login." (i18n key `authModeLocalAdvisory`).
- Compute a defensive `localDisabled = mode === 'local-login' && !localLoginAvailable`; bind it to the Save button alongside `enterpriseDisabled`.
- Reword `ssoNotConfiguredWarning` so it does **not** conflate the two modes; the new copy must hint at Local Login as an alternative when the operator has no external IdP.

In `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/pages.json` (then mirrored to `public/locales/`):

- Add: `fleet.authModeLocal` (label "Local Login"), `fleet.authModeLocalDesc` (description), `fleet.authModeLocalAdvisory` (advisory note), `fleet.localLoginUnavailable` (defensive warning when local IdP disabled).
- Update: `fleet.ssoNotConfiguredWarning` text to point at both options. Verify key counts match across all three locales using the `i18n-gap-check` skill.

### T09.7 — OpenAPI spec

In `docs/users/api/openapi/auth/e44-s01-identity-providers.yaml`:

- Extend the `mode` enum on `PUT /api/admin/settings/device-auth` request body and `GET /api/admin/settings/device-auth` response to include `local-login`.
- Add `localLoginAvailable: boolean` to the GET response schema.
- Add a `403 iam_denied` response on `POST /api/agent/sso-enroll`.

### T09.8 — Unit tests

- **Go**: `packages/control-plane/internal/handler/admin_settings_test.go` — table-driven test covering all three modes × local-only / external-only / both / neither IdP states.
- **Go**: `packages/nexus-hub/internal/handler/agent_bootstrap_test.go` — verify `local-login` → `enterprise-login` normalisation; verify the response cache key still works.
- **Go**: `packages/control-plane/internal/handler/agent_sso_enroll_test.go` — RBAC accept (super_admin), deny (viewer), 403 body shape; ensure neither path bypasses the gate.
- **Vitest**: `packages/control-plane-ui/src/pages/settings/DeviceAuthSettingsPage.test.tsx` — three-radio render, conditional save disabling, advisory copy present, error toast on 400/403.

### T09.9 — Architecture doc update

Already merged inline: `docs/users/product/architecture.md` E44 section now lists the three device-auth modes, the bootstrap-normalisation behaviour, and the carved-out `device-enrollment` IAM resource.

---

## Acceptance

- **AC-S09.1** — `PUT /api/admin/settings/device-auth` with `mode='local-login'` returns 200 when the seeded local IdP exists with `enabled=true`; returns 400 `local_idp_unavailable` otherwise.
- **AC-S09.2** — `GET /api/admin/settings/device-auth` includes the new `localLoginAvailable` field, with value `true` after seed, `false` if the local IdP is disabled.
- **AC-S09.3** — `GET /api/public/agent-bootstrap` returns `deviceAuthMode='enterprise-login'` whenever the raw stored mode is either `enterprise-login` or `local-login`; returns `mtls-only` only when the raw mode is `mtls-only`.
- **AC-S09.4** — `POST /api/agent/sso-enroll` with an auth code minted by `super_admin` succeeds (200 + enrollment JWT); same endpoint with an auth code minted by `viewer` returns 403 `{"error":"iam_denied"}`. The check fires for both `enterprise-login` and `local-login` modes.
- **AC-S09.5** — UI Settings → Device Authentication shows three radios; saving `local-login` succeeds; saving `enterprise-login` with no external IdP remains disabled.
- **AC-S09.6** — End-to-end: with `local-login` set, a fresh agent install (post warm-bootstrap fix) advances past "Contacting the gateway", shows the SSO sign-in button, opens the browser, user signs in as `admin@nexus.ai`, agent completes enrollment, dashboard renders Overview / Activity / Identity / Diagnostics / Settings.
- **AC-S09.7** — `go test -race -count=1 ./packages/control-plane/... ./packages/nexus-hub/... ./packages/shared/security/iam/...` is green.
- **AC-S09.8** — `npm test` (Vitest) in `control-plane-ui` is green; `/i18n-gap-check` reports zero gaps on the new keys.

---

## IAM impact review (per CLAUDE.md binding rule)

- **Action added**: `admin:device-enrollment.enroll`.
- **Resource carved out**: `device-enrollment` (new). Granting "enroll devices" does not imply `settings.update`, `device-defaults.update`, or any IdP CRUD.
- **UI / route**: `Settings → Device Authentication` page already exists with `allowedActions: ['admin:settings.update']`; that's for *managing the mode*, unchanged. The enrollment endpoint lives on the agent surface (`/api/agent/sso-enroll`), not in `shellRouteConfig.tsx`. No nav change.
- **Backend handler**: gated via in-handler `iam.Authorizer.UserHasAction` call (the endpoint is consumed by the agent over an OAuth auth code, not by an admin session, so `iamMW` middleware doesn't fit).
- **Managed-policy fixtures**: `NexusSuperAdmin` and `NexusProviderAdmin` gain `device-enrollment.enroll`. `NexusComplianceAdmin` and `NexusViewer` do not. The viewer test fixture in `packages/control-plane/internal/iam/managed.go` is updated so the assertion suite stays green.
- **Seed**: `super-admins` + `provider-admins` group policies grant the action. Existing seeded users keep their roles; only the new action joins.
- **Recorded in commit message**: "carved out `device-enrollment` resource so granting enroll doesn't imply settings.update or device-defaults.update".

---

## Out of scope (explicit non-goals)

- Per-user device-enrollment quota.
- A separate "device-management" resource (delete / re-enroll APIs). Today those still flow through other surfaces; we don't touch them in this story.
- Auto-suggestion to switch to `local-login` when no external IdP is configured. The UI surfaces the option; the operator chooses.
- Prod guardrail hiding `local-login` based on `crypto.production=true`. Per user decision, none.
- Agent binary rebuild (Hub-side normalisation makes the agent UI mode-agnostic).
- Multi-tenant scoping of device enrollment (every device today belongs to the singleton platform; multi-tenancy is a separate epic).
