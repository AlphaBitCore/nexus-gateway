# SDD — Authserver Interactive Login (SPA-Owned)

## Context

Interactive login is the user-facing end of the Nexus OAuth + PKCE flow.
Before this story the Control Plane served a server-rendered HTML form at
`GET /login` (template in `internal/authserver/login/templates/login.html`)
and handled the password form post at `POST /login/password` with an HTML
re-render on failure. The SPA's `/login` route collided with the backend
path and therefore only worked via client-side navigation — a hard reload
served the raw HTML form instead of the branded page.

This story moves login UI ownership to the SPA. The backend keeps the
OAuth state machine plus the IdP + password authentication logic, but now
exposes both as JSON endpoints.

## User Story

**As an** operator signing in to the Control Plane,
**I want** the login page to match the rest of the product (brand, i18n,
theme) and present each enabled IdP as a distinct button,
**so that** I can choose my login method (local password or an external
SSO provider) without ever landing on an unstyled page.

## Tasks

1. **Backend — JSON endpoints.** Add `GET /authserver/idps?authctx=` and
   `POST /authserver/password`. Delete `GET /login` and the HTML template.
   `authctx` is still minted by `/oauth/authorize` and required on both
   endpoints.

2. **Backend — error taxonomy.** Password handler returns typed errors:
   `invalid_credentials` (401), `user_disabled` (401), `authctx_expired`
   (400), `rate_limited` (429). No HTML re-render.

3. **SPA — method picker.** `LoginPage.tsx` reads `authctx` from the URL.
   Missing `authctx` → auto-redirect to `/oauth/authorize` via the
   existing `login()` helper. With `authctx` it calls the new
   `authApi.listIdps` and renders one button per IdP.

4. **SPA — local IdP flow.** Clicking the local-IdP button expands an
   inline email/password form. Submission is an AJAX POST to
   `/authserver/password`; on 200 the SPA navigates to `redirectUri`, on
   401/400/429 it renders the typed error inline and leaves the form
   open for retry.

5. **SPA — external IdP flow.** Clicking an external button navigates
   `window.location` to `/idp/{id}/start?authctx=...` — unchanged from the
   legacy HTML template's behaviour.

6. **Dev proxy.** `vite.config.ts` stops proxying `/login` (the SPA owns
   it) and adds `/authserver` → backend.

## Acceptance Criteria

- A hard reload on `/login?authctx=…` renders the SPA login page — not a
  server-rendered HTML form.
- With only a local IdP enabled, the SPA shows a single button for it;
  clicking it expands the email/password form inline. On invalid
  credentials the error appears inline (no navigation, no page reload).
  On success the browser lands on the configured redirect URI with a
  valid authorization code and state.
- With an external IdP enabled alongside local, both appear as separate
  buttons; clicking the external button redirects to
  `/idp/{id}/start?authctx=…`.
- `GET /login` is no longer served by the backend (404 from authserver);
  the SPA bundle is what answers this path in dev and prod.
- Rate-limit trips surface as an inline "too many attempts" message (429)
  without re-rendering an HTML page.
- `go test -race ./internal/authserver/...` and the SPA vitest suite are
  green.

## Non-Goals

- No change to `/oauth/authorize`, `/oauth/token`, `/oauth/revoke`,
  `/oauth/introspect`, `/idp/{id}/start`, or callback endpoints.
- No self-service password reset (still routed through a super admin per
  the Forgot Password page copy).
- No MFA. The method picker's shape leaves room for one later but this
  story does not add any.

## Operational Notes

- **Prod routing.** Nginx fronting the Control Plane needs SPA fallback
  (`try_files $uri /index.html`) for `/login` and `/auth/callback`. The
  same file must pass `/api`, `/oauth`, `/idp`, `/.well-known`, and the
  new `/authserver` prefix to the backend.
- **authctx lifetime.** Five-minute in-memory TTL is unchanged; if a
  user sits on the method picker past the TTL they see `authctx_expired`
  on submit and must restart from `/login` (which will re-mint a fresh
  `authctx`).
