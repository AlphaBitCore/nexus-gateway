# Trust Boundaries

*Audience: security reviewers mapping authentication surfaces, and contributors adding new inter-service interactions.*

Nexus Gateway has five distinct trust boundaries, each with its own authentication mechanism and termination point. Internal service-to-hub calls use a pre-shared `INTERNAL_SERVICE_TOKEN` that all four server services must match. Application-to-gateway calls use Virtual Key bearer tokens. Admin UI calls use OAuth+PKCE bearer tokens. Agent-to-Hub calls use mTLS device certificates issued by Hub's self-hosted CA. External IdP federation uses SAML/OIDC with Nexus acting as the SP. Provider credentials are encrypted at rest with AES-256-GCM and decrypted only in-memory for the request lifetime. This page maps each boundary to its auth mechanism, its termination point, and the secrets that cross it.

---

## Service-to-Hub: internal service token

All four server-side services — Control Plane, AI Gateway, Compliance Proxy, and Hub itself for self-calls — authenticate calls to Hub using `INTERNAL_SERVICE_TOKEN`. This is a pre-shared bearer secret read from the environment variable of the same name.

The `[MUST MATCH]` tag in `.env.example` identifies this as a cross-service shared secret. All four services and Hub must be configured with the same value. Drift between consumers is the most common source of inter-service 403 errors in this codebase.

**Scope of use:**
- Control Plane → Hub: shadow writes (`POST /api/hub/shadow/...`), Thing management, audit queries.
- AI Gateway → Hub: Thing registration on boot, heartbeat, shadow pulls for Cat B keys.
- Compliance Proxy → Hub: same as AI Gateway.

**Where it lives.** The token is env-only — it never appears in any committed YAML (binding: CLAUDE.md "secrets are env-only — never yaml"). In development, it is set in the repo-root `.env` file loaded via `bootenv`. In production, it is set via `systemd EnvironmentFile=` or equivalent.

**Termination point.** The `INTERNAL_SERVICE_TOKEN` boundary terminates at Hub. Services authenticate to Hub; they do not use this token for calls to each other.

---

## Agent-to-Hub: mTLS device certificates

Desktop Agents authenticate to Hub using **mTLS**. Hub runs a self-hosted ECDSA P-256 certificate authority. The enrollment ceremony:

1. An admin generates a one-time enrollment token in the CP UI.
2. The agent receives the token and uses it for the enrollment request.
3. The agent generates a local key pair (device-side; private key never leaves the device).
4. The agent submits a CSR to Hub.
5. Hub validates the enrollment token (single-use enforcement — a token used once cannot be replayed), signs the CSR, and returns a device certificate.
6. The agent stores the cert in the platform keystore (`platform.DefaultPaths()` — never hardcoded `/Library/` or `/var/` paths).
7. Subsequent connections to Hub use this device cert for mutual TLS.

The Hub's CA private key and all device certs live on the server side of the Hub process. The CA is self-hosted — it is not a public CA and its root is not in browser trust stores.

**Revocation.** Revoking a device in the CP UI sets the Thing's `status = revoked` in the `thing` table. Hub rejects further mTLS handshakes from revoked Things.

**Termination point.** The mTLS boundary terminates at Hub. Agents do not communicate directly with the Control Plane, AI Gateway, or Compliance Proxy at runtime.

**Credential storage.** Device private keys are stored in the platform keystore, accessed only via `platform.DefaultPaths()`. They are never stored in hardcoded paths and never transmitted off the device.

---

## Application-to-AI-Gateway: Virtual Key bearer

Applications call the AI Gateway's `/v1/*` surface using a **Virtual Key (VK)** bearer token in the `Authorization: Bearer vk-...` header. Virtual Keys replace raw provider API keys — the application never holds an Anthropic or OpenAI key.

**VK model:**
- Each VK belongs to exactly one project (transitively one org).
- VKs may restrict accessible models and providers via `allowed_models` and `allowed_providers`.
- An optional `quota_policy_id` binding enforces per-VK rate/cost limits.
- VKs have a status (`active`, `revoked`, `expired`) and an optional `expires_at`.

**Auth path** (`packages/ai-gateway/internal/auth/vkauth/`):
1. Parse `Authorization: Bearer ...`.
2. Hash the secret (constant-time HMAC) and look up by `hashed_secret`.
3. Reject if `status != active` or `expires_at < now`.
4. Hydrate the `RequestContext` with `VirtualKeyID`, `OrganizationID`, `ProjectID`, `allowed_models`, `allowed_providers`, `quota_policy_id`.
5. Stamp `last_used_at` (rate-limited write — once per minute per VK to avoid write amplification).

The hashed secret is never logged; the audit row records only `VirtualKeyID`. The plaintext VK is shown once on creation and never stored — it cannot be recovered after that.

**Rotation.** VK rotation is done by issuing a new VK and revoking the old. There is no in-place rotation (the secret is the identity). An optional grace period keeps the old VK accepting traffic while marked `status=expiring`, so dashboards highlight the migration window.

**Org join chains.** A VK's org is resolved through two join chains: application VKs go through Project → Org, personal VKs go through NexusUser → Org. Both chains must be covered when adding a new VK-derived column; missing either chain produces silent NULLs for the uncovered type (the 2026-05-16 prod hotfix `da073580`).

**Termination point.** VK authentication terminates at the AI Gateway. Hub and the Control Plane are not involved on the data path; VK resolution is entirely within the AI Gateway.

---

## Admin UI-to-Control-Plane: OAuth+PKCE

Admin users authenticate to the Control Plane using **OAuth+PKCE**. The Control Plane acts as an OAuth Authorization Server for local admin accounts, and as a Service Provider when federated with an external IdP.

**Nexus-Local authentication.** Admin accounts stored in the `NexusUser` table authenticate via username/password (hashed with bcrypt). The Control Plane issues a short-lived bearer token on successful authentication. Sessions are tracked in Valkey with configurable TTL.

**External IdP federation (SAML/OIDC).** Nexus is always the **SP** (Service Provider). External IdPs — Okta, Azure AD, other OIDC/SAML providers — are the identity authorities. Nexus Local is the implicit fallback; it is not a peer IdP in the federation. JIT user provisioning creates a `NexusUser` row from IdP assertion claims on first successful federation.

**Cookie and session scope.** Admin sessions are bearer-token sessions (not cookie-based). Session cookies are scoped to the admin UI origin. They do not travel to the AI Gateway or Compliance Proxy endpoints, which are stateless request handlers that authenticate each request independently.

**IAM gating.** Every admin API endpoint is gated by `iamMW`, which evaluates the authenticated user's org-scoped IAM policy against the endpoint's declared resource/action. Drift between UI `allowedActions` and handler `iamMW(...)` declarations produces silent 403 errors; the IAM impact review is required for every endpoint add/move/rename.

**Termination point.** OAuth+PKCE terminates at the Control Plane. The admin UI holds a bearer token scoped to the Control Plane; it has no credentials to call Hub or the data-plane services directly.

---

## Provider credentials: AES-256-GCM at rest

Provider credentials (OpenAI API keys, Anthropic API keys, Google service-account JSON, etc.) are encrypted at rest using AES-256-GCM. Three separate columns on the `Credential` row hold the encrypted components:

- `encryptedKey` — the ciphertext.
- `encryptionIv` — the 96-bit initialization vector.
- `encryptionTag` — the 128-bit GCM authentication tag.

The `encryptionKeyId` column (default `"v1"`) identifies which master key encrypted the row, enabling multi-key rotation without downtime.

**Encryption key management.** The master key comes from environment variables (never YAML):

| Env var | Scope | Purpose |
|---|---|---|
| `CREDENTIAL_ENCRYPTION_KEY` | `[MUST MATCH]` AI Gateway + Control Plane | AES-256 master key, 64 hex chars. Default when no key map is set. |
| `CREDENTIAL_KEY_MAP` | `[MUST MATCH]` AI Gateway + Control Plane | Multi-key rotation map: `"v1:<hex64>,v2:<hex64>"`. Takes precedence when present. |

The `[MUST MATCH]` tag means both the AI Gateway (which decrypts at request time) and the Control Plane (which encrypts on credential create/update) must use the same key. Drift between them makes existing credentials unreadable to the gateway.

**Decryption is request-scoped.** The plaintext is decrypted only in memory and only for the request lifetime via a `CredentialRef` → `credstate.Acquire(ref)` call at dispatch time. The plaintext is never written to a log, never returned in an API response, and never stored in Valkey.

**Rotation propagation.** When a credential is updated via the CP UI: CP encrypts with the current master key → persists → forwards to Hub → Hub signals AI Gateway via WS change-signal → AI Gateway invalidates its in-memory decrypt cache → next request fetches fresh ciphertext and decrypts. No service restart required; no stale-credential window after the signal arrives.

## Service-level isolation

Each trust boundary terminates at a specific service. No service holds credentials for another service's downstream consumers:

| Boundary | Terminates at | What crosses |
|---|---|---|
| Admin UI → Control Plane | Control Plane | OAuth+PKCE bearer token (short-lived) |
| Control Plane → Hub | Hub | `INTERNAL_SERVICE_TOKEN` (long-lived, pre-shared) |
| AI Gateway → Hub | Hub | `INTERNAL_SERVICE_TOKEN` (same pre-shared secret) |
| Compliance Proxy → Hub | Hub | `INTERNAL_SERVICE_TOKEN` (same pre-shared secret) |
| Application → AI Gateway | AI Gateway | Virtual Key bearer (per-project, per-user) |
| Desktop Agent → Hub | Hub | mTLS device certificate (per-device, Hub-issued) |
| External IdP → Control Plane | Control Plane | SAML assertion / OIDC ID token (short-lived, single-use) |
| Upstream provider calls | Provider API | Provider API key (decrypted in-memory, request-lifetime only) |

The AI Gateway and Compliance Proxy hold no admin credentials and cannot call the Control Plane admin API directly. The Desktop Agent holds no Virtual Keys and cannot call the AI Gateway directly. Each service is scoped to exactly the credentials it needs for its role.

## Secrets posture

All secrets are env-only (binding: CLAUDE.md "secrets are env-only — never yaml"). The main shared secrets and their consumers:

| Env var | Consumers | `[MUST MATCH]` |
|---|---|---|
| `INTERNAL_SERVICE_TOKEN` | Hub, Control Plane, AI Gateway, Compliance Proxy | ✅ — all four must match |
| `CREDENTIAL_ENCRYPTION_KEY` | AI Gateway (decrypts), Control Plane (encrypts) | ✅ — both must match |
| `CREDENTIAL_KEY_MAP` | AI Gateway, Control Plane | ✅ — both must match (overrides single key) |
| `SESSION_SECRET` | Control Plane | ❌ — single consumer |
| `DB_URL` | Hub, Control Plane | ❌ — both connect to same DB |

The `[MUST MATCH]` tag in `.env.example` identifies secrets that must be identical across multiple services. Drift between `INTERNAL_SERVICE_TOKEN` values is the most common source of inter-service 403 errors after a deployment. Drift between `CREDENTIAL_ENCRYPTION_KEY` values makes all existing credentials unreadable to the AI Gateway.

The `.env.example` file at the repo root is the canonical catalog of all env vars with their descriptions and `[MUST MATCH]` annotations.

## Session lifecycle (admin sessions)

Admin sessions are managed by the Control Plane:

1. Admin navigates to the CP UI and is redirected to the OAuth+PKCE authorization endpoint.
2. For local accounts: credentials are submitted and verified against the `NexusUser` table (bcrypt).
3. For federated accounts: the Control Plane redirects to the external IdP; on successful assertion, JIT-provisioning creates or updates the `NexusUser` row.
4. The Control Plane issues a short-lived bearer token and stores the session in Valkey.
5. Subsequent admin API calls include the bearer token in `Authorization: Bearer ...`; the Control Plane validates and resolves the session from Valkey.
6. Session TTL is configurable; idle sessions expire automatically.

The `cp_login` / `cp_curl` helpers in `tests/lib/auth.sh` replicate this flow for local development and integration tests:

```bash
# Obtain a bearer token for the seeded super-admin account
cp_login
# Call any admin API endpoint with the cached token
cp_curl GET /admin/virtual-keys
```

These helpers are the canonical entry point for local admin API calls and automation tests.

## IAM gating on admin endpoints

Every admin API endpoint in the Control Plane is decorated with `iamMW(resource, action)`. This middleware:

1. Extracts the bearer token from the request.
2. Resolves the session from Valkey.
3. Loads the user's org-scoped IAM policy.
4. Evaluates `(org, resource, action)` against the policy.
5. Returns HTTP 403 if the policy does not permit the action.

Drift between the UI's `allowedActions` check (which hides buttons for unauthorized actions) and the handler's `iamMW` declaration produces silent 403 errors: the UI shows a button, the user clicks it, the API returns 403 with no visible error in some UI paths. The IAM impact review (CLAUDE.md binding) is required for every endpoint add/move/rename.

---

## Credential pool security model

Provider credentials are not held as a 1:1 mapping between a routing rule and an API key. They are managed through a credential pool with circuit-breaker protection:

- Multiple credentials for the same (org, provider) can be registered. The pool picks one using weighted round-robin with stickiness on (org, model) for cache locality.
- When a credential repeatedly fails authentication (`auth` error class), its circuit opens and the credential is excluded from the pool.
- When **all** credentials for a (provider, model) pair have open circuits, the (provider, model) circuit itself opens and routing fails for that pair until a probe succeeds.
- Per-credential circuit state is stored in Valkey (short-lived) and in the `Credential` row in PostgreSQL (durable copy flushed on transitions).

This design prevents a single bad API key from blocking all traffic to a provider: the pool excludes that credential, and remaining healthy credentials continue to serve requests.

**The `CredentialRef` carrier.** The routing engine resolves a `CredentialRef` (credential ID + version), not the plaintext. The executor calls `credstate.Acquire(ref)` at dispatch time to retrieve the decrypted key. This keeps the plaintext narrowly scoped — it exists only in the executor's stack frame during the upstream call and is never held in routing-engine-side memory.

## Summary: boundary properties

| Boundary | Credential type | Rotation model | Recovery on loss |
|---|---|---|---|
| Service-to-Hub | Pre-shared env token | Manual re-configure all services | Re-deploy with new matching token |
| Agent-to-Hub | mTLS device cert | Re-enrollment (revoke old device, issue new) | Admin revokes device; agent re-enrolls |
| Application-to-AI-Gateway | Virtual Key bearer | Issue new VK; revoke old | Issue new VK; apps switch keys |
| Admin UI | OAuth+PKCE session | Session expiry / re-login | Re-login |
| Provider credentials | AES-256-GCM ciphertext | Re-encrypt with new master key | All credentials unrecoverable if key lost |

The "provider credentials unrecoverable if key lost" property is intentional — the AES-GCM master key is the security boundary. Operators must treat `CREDENTIAL_ENCRYPTION_KEY` as a root secret: back it up securely, never commit it to version control, and set it as an `EnvironmentFile=` or K8s Secret in production.

## Token field stamping: 5 sites rule

When a new usage-derived field is added to the `traffic_event` schema (e.g., a new token type from a provider), it must be stamped at all five sites in `proxy.go` + `proxy_cache.go`. Missing even one site results in NULL for that field on all traffic served by the missed code path.

The five sites correspond to five combinations of streaming × cache status:
1. `handleNonStream` — non-streaming, cache miss (new upstream call).
2. `handleStreamWithSubscription` — streaming, cache miss.
3. `handleNonStreamHit` — non-streaming, cache hit (replaying stored response).
4. `handleStreamHit` — streaming, cache hit.
5. `handleNonStreamWithSubscription` → `handleNonStreamCacheSave` — non-streaming, writing to cache.

This is not a trust-boundary concern per se, but it is documented here because missing stamp sites affect the accuracy of the cost and token attribution data that feeds into per-VK billing and analytics — directly impacting the financial accuracy of the trust boundary between the gateway and the organization billing the AI spend.

The production incident that established this rule: E53-S4 added `CacheCreationTokens` to the usage struct. Four of the five cache-path stamp sites were missed. All cache traffic showed NULL cost for cache-creation tokens until the hotfix was applied.

## Virtual Key rotation procedure

Rotating a Virtual Key (when a key is suspected compromised or when periodic rotation policy requires it) follows a well-defined procedure that avoids a hard cutover:

1. Admin creates a new VK in the CP UI (Settings → Virtual Keys → New).
2. The new VK is distributed to the application team. The old VK is marked `status=expiring` (a grace state that allows traffic but highlights in dashboards).
3. Application team updates SDK configuration to use the new VK.
4. Admin verifies in the traffic analytics that the old VK's `last_used_at` has passed the cutover timestamp with zero new requests.
5. Admin revokes the old VK (`status=revoked`). Any subsequent request with the old VK receives HTTP 401.

The `status=expiring` grace period is optional. If the security incident requires immediate revocation, the admin can skip to step 5; affected applications will break immediately and need to be updated with the new VK.

The new VK can have different quota limits, model restrictions, or routing policy bindings than the old one. This is a common rotation pattern: the new VK tightens the policy; the old one stays active in `expiring` state during the migration window so the application team can move at their own pace.

## IAM model overview

The Control Plane's IAM model uses resource NRNs (Nexus Resource Names) and action strings. Every admin API endpoint is associated with a `(resource_nrn, action)` pair, enforced by `iamMW`. The IAM policy is org-scoped: a user's policy is evaluated in the context of the org they are accessing.

Built-in IAM roles and their typical associated actions:
- **super-admin** — all actions on all resources within the org.
- **admin** — most actions; cannot create/delete other admins or manage IAM policies.
- **operator** — read-only on config and analytics; can activate kill-switch.
- **viewer** — read-only on traffic and analytics only.

Custom roles can be defined in the `IamPolicy` table with explicit `(resource_nrn_pattern, action, allow/deny)` rows. The evaluation order: explicit deny > explicit allow > role-based default.

**IAM impact of endpoint moves.** When an admin endpoint is moved to a new URL or renamed, the `iamMW` resource/action pair must be updated to match. If the UI's `allowedActions` check is not updated simultaneously, the UI will either show a button the user cannot click (silent 403) or hide a button the user has access to (unnecessary restriction). The IAM impact review is a binding pre-merge step for any endpoint change (CLAUDE.md: "API / menu / route changes require IAM impact review").

## Federation posture: Nexus is always the SP

When external IdP federation is configured, the terminology boundary is strict:

- **Nexus is the SP** (Service Provider). It validates assertions from external IdPs and provisions users.
- **External IdPs** (Okta, Azure AD, Google Workspace OIDC, etc.) are the identity authority for federated accounts.
- **Nexus Local** is the implicit fallback — the set of admin accounts stored in the `NexusUser` table. It is not a peer IdP in federation; it is the baseline authentication path.

This matters operationally: if an external IdP is misconfigured or unreachable, Nexus Local accounts still work. Federated admins cannot log in until the IdP is healthy. The CP UI login page always shows the Local login option alongside any configured IdP SSO buttons. An org that wants to enforce IdP-only login must disable Nexus Local accounts explicitly — the platform does not auto-disable them on IdP registration.

**JIT provisioning.** On the first successful federation assertion from a new user, the Control Plane creates a `NexusUser` row with the IdP-supplied `email`, `display_name`, and a default org role. Subsequent logins update `display_name` and `last_sso_at` but do not change the org role — role changes require an admin action to avoid IdP-controlled privilege escalation.

**SCIM provisioning.** SCIM user and group sync is a future capability. Today, deprovisioning must be done by revoking the `NexusUser` row manually in the CP UI. JIT-provisioned users that leave the IdP tenant continue to have access until their Nexus account is revoked.

## Security invariants contributors must preserve

Every PR that touches any of the trust boundaries listed above must preserve these invariants. Violating them typically surfaces as a 403 in the best case and a silent security regression in the worst.

**Token comparison must be constant-time.** All token validation in the gateway uses `subtle.ConstantTimeCompare` or equivalent. Time-based token comparison leaks information about partial matches. This applies to `INTERNAL_SERVICE_TOKEN`, Virtual Key hash comparison, and session token validation.

**VK plaintext is shown once and never stored.** At creation, the Control Plane generates a random secret, hashes it via HMAC-SHA256, stores the hash in `hashed_secret`, and returns the plaintext once. The Control Plane does not store the plaintext. After the creation response, the VK secret cannot be recovered — only rotated.

**Provider credentials never log or return plaintext.** Any code path that calls `credstate.Acquire(ref)` receives a plaintext key. This key must never be written to a log field, an error message, a trace annotation, or an API response body. Static analysis catches `log.Info` calls with credential structs; dynamic review catches serialization paths.

**Enrollment token is single-use.** Agent enrollment tokens are one-time-use: Hub marks the token consumed on the first successful CSR signing. A replay attack with a captured enrollment token returns HTTP 409 (token already used) without creating a new device certificate.

**CA private key is Hub-internal.** The Hub Agent CA private key is generated on Hub boot and stored in Hub's keystore at `platform.DefaultPaths().HubCAKey`. It is never exposed via API, never transmitted to the Control Plane or data-plane services, and never logged. Only Hub signs CSRs.

## Cross-boundary call inventory

The following table is the exhaustive inventory of every cross-boundary call at runtime. Any new inter-service call that is not in this table requires an architecture review and a PR that adds it here.

| Caller | Callee | Auth mechanism | Direction | Note |
|---|---|---|---|---|
| Admin UI | Control Plane | OAuth+PKCE bearer | Admin action | Every admin API call |
| Control Plane | Hub | `INTERNAL_SERVICE_TOKEN` | Config write, shadow read | Shadow CRUD, Thing management |
| Control Plane | Hub | `INTERNAL_SERVICE_TOKEN` | Audit query | Admin audit log queries |
| AI Gateway | Hub | `INTERNAL_SERVICE_TOKEN` | Thing registration | On boot |
| AI Gateway | Hub | `INTERNAL_SERVICE_TOKEN` | Shadow pull (Cat B) | On config change-signal |
| AI Gateway | Hub | `INTERNAL_SERVICE_TOKEN` | Heartbeat | Every 10s |
| Compliance Proxy | Hub | `INTERNAL_SERVICE_TOKEN` | Same as AI Gateway | Same |
| Desktop Agent | Hub | mTLS device cert | Thing registration, shadow pull, heartbeat | Boot + ongoing |
| Desktop Agent | Hub | mTLS device cert | Audit upload | Drains local SQLCipher queue |
| Application | AI Gateway | Virtual Key bearer | `/v1/*` AI request | Every AI request via Path A |
| External IdP | Control Plane | SAML / OIDC | Assertion POST | On federation login |
| AI Gateway | Provider API | Provider API key (plaintext, in-memory) | Upstream AI call | Per request |
| Compliance Proxy | Provider API | None (TLS relay) | CONNECT tunnel | Per intercepted flow |
| Desktop Agent | Provider API | None (TLS MITM) | OS-level intercept | Per intercepted flow |

The Desktop Agent and Compliance Proxy act as MITM proxies — they do not authenticate to the upstream provider; they relay the client's existing provider connection. Provider authentication happens at the application level (the application's own API key or the VK-resolved credential for Path A).

---

## Canonical docs

- [`credentials-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/credentials-architecture.md) — Virtual Key model, provider credential encryption columns, credential pool, rotation flow, encryption key env vars, audit invariants
- [`overview.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/overview.md) — §10 trust boundaries table
- [`agent-enrollment-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-enrollment-architecture.md) — mTLS enrollment ceremony, CSR flow, single-use token enforcement, revocation

**Adjacent wiki pages**: [Architecture Overview](Architecture-Overview) · [Fail Open Posture](Fail-Open-Posture) · [Control Plane Vs Data Plane](Control-Plane-Vs-Data-Plane) · [The Five Services](The-Five-Services) · [Credentials Storage](Credentials-Storage)
