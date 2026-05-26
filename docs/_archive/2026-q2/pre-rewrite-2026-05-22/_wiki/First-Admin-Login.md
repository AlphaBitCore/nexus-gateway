# First Admin Login

The Control Plane UI at `http://localhost:3000` is the admin dashboard for Nexus Gateway. It uses an OAuth 2.0 + PKCE authorization flow — no cookies, no legacy session endpoints. The local dev seed pre-creates five users with different roles so the first login requires no account setup. This page covers the login flow, the seeded accounts, a brief UI tour, and how to change the seed password.

---

## Logging in

1. Open `http://localhost:3000` in a browser. The login screen appears immediately.

2. Enter the seeded super-admin credentials:

   ```
   Email:    admin@nexus.ai
   Password: admin123
   ```

3. Click **Sign in**. The browser stays on `localhost:3000` — the OAuth + PKCE exchange happens in the background between the UI (running as a public client) and the Control Plane authorization server at `:3001`. No redirect to an external IdP occurs in the default local-dev configuration.

4. After a successful exchange the UI issues a bearer token (RS256 JWT, 1-hour expiry) and stores it in memory. The dashboard home page loads.

The UI handles token refresh automatically via the `grant_type=refresh_token` path (24-hour refresh token). Idle sessions past 24 hours require a re-login.

## Seeded accounts

The seed creates five accounts, each with a different role:

| Email | Role | Password |
|---|---|---|
| `admin@nexus.ai` | `super_admin` | `admin123` |
| `alice@nexus.ai` | `admin` | `admin123` |
| `bob@nexus.ai` | `provider_manager` | `provider123` |
| `carol@nexus.ai` | `compliance` | `compliance123` |
| `diana@nexus.ai` | `viewer` | `viewer123` |

These accounts are defined in [`tools/db-migrate/seed/data/seed-baseline.sql`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/tools/db-migrate/seed/data/seed-baseline.sql). The password hashes in the seed are the argon2id hashes of the plaintext passwords listed above.

## UI tour — top-level sections

After login, the sidebar shows the main sections. The exact nav items visible depend on the logged-in user's role; `admin@nexus.ai` (super_admin) sees all sections.

**AI Gateway** — virtual key management, provider credential setup, routing rules, model catalog, and the traffic timeline. This is the first stop when configuring an AI traffic path through the gateway.

**Compliance Proxy** — interception domain allowlist, hook configuration for the proxy path, and the compliance-proxy traffic timeline.

**Hooks** — the shared hooks framework. Hook types (keyword filter, PII scanner, content safety, etc.) apply across both the AI Gateway and the Compliance Proxy path depending on how routing rules assign them.

**Traffic** — the unified traffic timeline. Every `traffic_event` row — regardless of whether it came in via the AI Gateway, Compliance Proxy, or Desktop Agent — appears here. Click any row to open the full request drawer: routing trace, hook decisions, token counts, latency breakdown, and the audit body.

**Devices** — the enrolled Desktop Agent fleet. Shows each agent node's status, version, last-seen, and config-sync health. Issue enrollment tokens here for new macOS installations.

**Settings → Providers** — manage provider credentials (OpenAI, Anthropic, Google, Azure, etc.). Each provider has a base URL and one or more encrypted Credential rows. A real provider API key must be added here before the AI Gateway can forward requests.

**Settings → IAM** — role definitions, policies, and user management. The IAM model is NRN-based (Nexus Resource Name) with RBAC + ABAC.

**Infrastructure** — Nodes (all registered service nodes), Config Sync (desired vs applied config for each node), Jobs (scheduled background jobs), and Kill Switch (3-tier emergency stop).

## Admin API from the shell

For scripted calls without a browser, the repo ships a helper that wraps the OAuth + PKCE flow and caches the token:

```bash
cp tests/.env.local.example tests/.env.local   # gitignored; edit if needed
source tests/lib/loadenv.sh local               # reads tests/.env.local
source tests/lib/auth.sh

cp_login                                        # idempotent; caches token at /tmp/nexus_token_local
cp_curl /api/admin/me                           # confirm session: returns the logged-in user
cp_curl /api/admin/analytics/cost?groupBy=model # example analytics query
```

## OAuth + PKCE under the hood

The Control Plane runs a local OAuth 2.0 authorization server with PKCE (S256 method) at `:3001`. The authorization endpoints are at `/oauth/authorize` (starts the flow), `/oauth/token` (exchanges code for tokens), and `/oauth/revoke`. There is no cookie-based session — every admin API call after login carries an RS256 bearer JWT in the `Authorization: Bearer` header.

The PKCE flow used by the browser UI:

1. The UI generates a `code_verifier` (random 43–128 chars) and computes `code_challenge = base64url(sha256(code_verifier))`.
2. The UI redirects internally to `/oauth/authorize?code_challenge=...&code_challenge_method=S256`.
3. The login form POSTs credentials to `/authserver/password` which validates against the `NexusUser` row's argon2id hash and issues a one-time authorization code (5-minute TTL).
4. The UI exchanges the code + `code_verifier` at `/oauth/token`, receiving an access token (1-hour expiry) and a refresh token (24-hour expiry, opaque, stored in Postgres with rotation on use).

The same PKCE flow is available to CLI scripts via `tests/lib/auth.sh`'s `cp_login` function — it drives the full exchange and caches the token at `/tmp/nexus_token_local`.

## Changing the seed password

To change `admin@nexus.ai`'s password after first login:

1. Navigate to the user menu (top-right) → **Profile**.
2. Click **Change password**, enter the current password (`admin123`), and set a new one.

For bulk password resets or CI environments where the seed password must differ from the default, edit the password hash in the seed SQL before running `npx prisma db seed`. The hash format is argon2id. The seed does NOT re-hash on every run — it applies the seed-baseline snapshot, which contains the pre-computed hashes.

## Status and setup surfaces

After first login, two pages are useful for confirming the stack is healthy before proceeding to the first AI request:

**Infrastructure → Nodes** shows all registered service nodes and their online/offline status. After a clean bring-up, four nodes should be online: `ai-gateway`, `compliance-proxy`, `control-plane`, `nexus-hub`.

**Infrastructure → Config Sync** shows the desired vs applied config for each node. In a fresh stack with no admin changes made yet, all nodes show "in sync" — no drift. If any node shows "out of sync" it means a config change was pushed but the service has not yet pulled and applied it.

If a node is missing from the Nodes page, the most common cause is `INTERNAL_SERVICE_TOKEN` mismatch between `.env` and the service config. Check the service log for `INTERNAL_SERVICE_TOKEN is not set`.

---

## Canonical docs

- [`oauth-pkce-admin-auth-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/oauth-pkce-admin-auth-architecture.md) — PKCE flow, token shape, refresh mechanics, endpoint list
- [`seed-baseline.sql`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/tools/db-migrate/seed/data/seed-baseline.sql) — the seeded user rows

**Adjacent wiki pages**: [Quickstart](Quickstart) · [Your First AI Request](Your-First-AI-Request) · [Control Plane Authentication](Control-Plane-Authentication) · [Control Plane Admin UI Tour](Control-Plane-Admin-UI-Tour)
