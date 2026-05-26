---
doc: credentials-architecture
area: cross-cutting
service: safety
tier: 1
updated: 2026-05-20
---

# Credentials Architecture — Virtual Keys + Provider Credentials + Credential Pool

> **Tier 1 architecture doc.** Read this before touching `packages/ai-gateway/internal/credentials/{pool,stats,manager,decrypt}/`, `packages/ai-gateway/internal/auth/vkauth/`, `packages/shared/schemas/credstate/`, `packages/control-plane/internal/platform/crypto/`, or any virtual-key / credential CRUD path.

Three concerns share this doc because they're inseparable in practice:

1. **Virtual Keys (VK)** — what applications use to call Nexus.
2. **Provider Credentials** — what Nexus uses to call providers.
3. **Credential Pool** — how Nexus picks one of many healthy provider credentials.

---

## 1. Virtual Keys

### Model

Per `tools/db-migrate/schema.prisma` `model VirtualKey`:

```
VirtualKey {
  id, name, keyHash, keyPrefix,                     // HMAC-SHA256 of the real key + first 12 chars for display
  vkType,                                           // "personal" | "application"
  projectId, ownerId,                               // org scope is reached via Project→Org (application) or NexusUser→Org (personal)
  enabled,                                          // soft on/off
  vkStatus,                                         // "pending" | "active" | "expired" | "rejected" | "revoked"
  allowedModels,                                    // JSON array of { providerId, modelId } (modelId supports globs)
  rateLimitRpm,
  dryRunRateLimitRpm, compareEndpointRateLimitRpm,  // separate RPM caps for dry-run + /v1/estimate compare
  budgetLimitUsd, expiresAt,
  createdAt, updatedAt, lastUsedAt
}
```

A VK belongs to exactly one project (application VK) or one personal owner (personal VK); org scope is derived via `Project→Org` or `NexusUser→Org`. It is a **bearer credential** — the application sends `Authorization: Bearer nvk_...` to the AI Gateway, the gateway resolves the VK, and the request inherits the VK's org / project scope (cross-ref `tenancy-architecture.md`).

There is no `allowed_providers` column — provider scope is reached transitively via `allowedModels`'s `providerId` entries.

### Auth

`vkauth` (in `packages/ai-gateway/internal/auth/vkauth/vkauth.go`) does the resolution:

1. Parse `Authorization: Bearer ...`.
2. Hash the secret (HMAC-SHA256) and look up via `GetVirtualKeyByHash(keyHash)`.
3. Reject if the VK is not in an accepting state (`vkStatus` / `enabled` / `expiresAt`).
4. Hydrate the request metadata with `VirtualKeyID`, `OrganizationID` (resolved via the join chain), `ProjectID`, and the allowed-model list.

The hashed key (`keyHash`) is never logged; the audit row records only `VirtualKeyID`.

### Rotation

VKs are rotated by **issuing a new VK and revoking the old**. There is no in-place rotation (the secret is the identity). Apps switch their bearer; the admin revokes the old VK.

The schema does not carry a dedicated `expiring` status; admins typically issue the replacement VK in advance and revoke the old one once apps have migrated.

## 2. Provider Credentials

### Model

The canonical schema is `model Credential` in `tools/db-migrate/schema.prisma:391`. Key columns (grouped by concern):

```
Credential {
  // Identity + provider binding
  id, name, providerId,                         // FK → Provider (org-scoped via Provider)
  enabled,                                      // soft on/off

  // AES-256-GCM encryption components (NOT a single "encrypted_blob")
  encryptedKey, encryptionIv, encryptionTag,    // ciphertext + IV + auth tag
  encryptionKeyId,                              // key version label (default "v1") — see §6

  // Rotation lifecycle
  rotationState,                                // none | pending_rotation | validating | rotated | completed
  lastRotatedAt, rotationStartedAt,

  // Pool selection (L1-L3 weighted picking)
  selectionWeight,                              // 0 = excluded; higher = more traffic
  status,                                       // active | retiring | retired
  retireAt,                                     // auto-delete after this date

  // Circuit breaker (durable copy of Redis state, flushed on transitions)
  circuitState,                                 // closed | open | half_open
  circuitReason,                                // auth_fail | rate_limit | null
  circuitOpenedAt, circuitNextProbeAt,

  // Multi-window health classification (E41 v2)
  healthStatus,                                 // healthy | degraded | unavailable | unknown | collecting
  healthSuccessRate5m, healthSuccessRate1h,
  healthSamplesObserved, healthDominantError,
  healthTrend,                                  // improving | stable | degrading
  healthStatusChangedAt, healthCheckedAt,

  // Per-credential threshold overrides (JSONB; falls back to Hub shadow → credstate defaults)
  reliabilityOverrides,

  // Usage + timestamps
  lastUsedAt, lastSuccessAt, lastFailureAt, lastFailureReason,
  totalUsageCount, expiresAt,
  createdAt, updatedAt,
}
```

The plaintext (provider API key, service-account JSON, etc.) is encrypted at rest with AES-256-GCM; the three components `encryptedKey` (ciphertext), `encryptionIv` (96-bit IV), and `encryptionTag` (128-bit authentication tag) live in separate columns. `encryption_key_id` labels which master key encrypted this row — used by the multi-key map (§6) to pick the right key during decrypt. The plaintext is decrypted only in memory and only for the request lifetime.

Credentials FK to `Provider`; `Provider` is org-scoped, so credentials inherit org scope transitively. Per the binding `tenancy-architecture.md` invariant: cross-org credential sharing is forbidden by design.

### `credstate` (shared constants) vs the decrypt cache

Two distinct packages, easy to confuse:

- `packages/shared/schemas/credstate/` is the **shared single source of truth for constants and enums** — Redis key prefixes (`cred:stats:<id>`, `cred:circuit:<id>`, dirty-set + in-flight-set names), circuit-state values (`closed | open | half_open`), open-reason values (`auth_fail | rate_limit`), health-classification values, and default reliability thresholds used when neither Hub-shadow globals nor a per-credential override applies. It is import-only; no behaviour. AI Gateway, Control Plane, and Nexus Hub all import these symbols so the runtime contract stays in lockstep.
- `packages/ai-gateway/internal/credentials/decrypt/` is the **in-memory decrypted-credential cache** used by the data plane. It holds AES-decrypted plaintexts narrowly scoped to the request lifetime and consults Hub change-signals to invalidate stale entries.

When the Control Plane updates a credential:

1. CP encrypts the new value (new IV + tag) and persists the row.
2. CP forwards to Hub; Hub updates the shadow.
3. Hub signals AI Gateway via WS change-signal.
4. AI Gateway invalidates the affected entry in `internal/credentials/decrypt/`.
5. The next request that needs this credential fetches the fresh row + decrypts.
6. Plaintext cached again, scoped to the request.

No service restart, no per-request decryption when unchanged, no stale-credential window.

### Rotation flow

Admin → CP API → encrypt → persist (version++) → Hub shadow → AI GW dirty-set → next request uses new key. End-to-end seconds, no restart.

The old credential value is **overwritten** in the DB (we do not keep credential history). Rollback requires a new credential update.

## 3. Credential Pool

A pool exposes "the set of healthy credentials for (provider, model) right now". The router (`routing-architecture.md`) consults the pool when resolving the credential for a `ResolvedRequest`:

1. Filter credentials to (org, provider) that the routed `(provider, model)` requires.
2. Drop credentials with `status != active` or `health.circuit_state == open`.
3. Pick one — typically weighted round-robin, with stickiness on (org, model) for cache locality.

### Health rollup

`credstats` (in `packages/ai-gateway/internal/credentials/stats/`) tracks per-credential per-window stats:

- Success / failure counts per `ErrorClass`.
- Rolling 429 count (5 min window).
- Last-success-at, last-failure-at.

When the failure burst threshold is exceeded, the credential's circuit opens (cross-ref `error-taxonomy-architecture.md` §5). Open credentials are excluded from the pool until cool-down expires and a probe succeeds.

Per-credential circuit state is **separate** from per-(provider, model) circuit state. A single bad API key shouldn't open the breaker for an entire model; the pool just excludes that credential. Only when **all** credentials for a (provider, model) are open does the (provider, model) breaker itself open.

### Pool & fallback chain interaction

The routing engine's fallback chain (`routing-architecture.md` §7) runs **after** the pool decision. The pool says "use credential X for (openai, gpt-4o)"; if the request still fails, the fallback chain tries `(anthropic, claude-3-5-sonnet)` and the pool repeats for that pair with the org's anthropic credentials.

## 4. The `CredentialRef` carrier

`ResolvedRequest.Credential` is a `CredentialRef`, not the plaintext:

```go
type CredentialRef struct {
    CredID    string
    Version   int
    Provider  string
}
```

The executor calls `credstate.Acquire(ref)` at dispatch time. This keeps the plaintext narrowly scoped (request-local). The credential never lives in routing-engine-side memory.

## 5. Provider/model health & alerting

`credstats` emits per-credential health rollups that feed:

- The credential pool's exclusion logic.
- The CP admin UI ("Credential Health" card per credential).
- Alerting (cross-ref `alerting-architecture.md`): a rule fires when a credential burns through its 429 budget or has consecutive failures.

The `provider.unavailable` rule (from `error-taxonomy-architecture.md`) considers the **entire** (provider, model) — it fires when all credentials in the pool are open.

## 6. Encryption key management

Three env vars carry the master-key material — all live in `.env.example` (lines 42-57). Per the binding "secrets are env-only — never yaml" rule (`CLAUDE.md`), none of these appear in any committed yaml.

| Env var | Scope | Purpose |
|---|---|---|
| `CREDENTIAL_ENCRYPTION_KEY` | [MUST MATCH] AI Gateway + Control Plane | AES-256 master key, 64 hex chars (32 bytes). Default key when no rotation map is set. Generate via `openssl rand -hex 32`. |
| `CREDENTIAL_KEY_MAP` | [MUST MATCH] AI Gateway + Control Plane | Optional multi-key map for rotation. Format: `"v1:<hex64>,v2:<hex64>"`. Takes precedence over `CREDENTIAL_ENCRYPTION_KEY` when present. The row's `encryption_key_id` column (default `"v1"`) selects which entry decrypts that row. |
| `CREDENTIAL_ENCRYPTION_PASSPHRASE` + `CREDENTIAL_ENCRYPTION_SALT` | Control Plane only | Alternative passphrase mode; the master key is derived via scrypt. Leave both empty if not used. |

**Rotation flow** with the key map: deploy CP + AI Gateway with `CREDENTIAL_KEY_MAP="v1:<old>,v2:<new>"`, set `v2` as the active key for new writes, then re-encrypt existing rows in batches and bump their `encryption_key_id` to `v2`. Once every row is on `v2`, drop the `v1:` entry on the next deploy. This is non-disruptive — no decrypt failures during the window.

Per-tenant key derivation is a future enhancement (not in scope for individual feature PRs). When it lands, the key derivation function will be HKDF over a master key + tenant id; the rotation policy will track.

## 7. Audit invariants

Every credential-related operation emits audit:

- `admin:credential.create` / `update` / `revoke` — admin-audit row, includes credential id + provider + masked metadata, **never** the plaintext.
- `admin:vk.create` / `revoke` — admin-audit row.
- Per request: traffic_event stamps `credential_id` (no secret), so investigators can correlate "which credential was used" with health metrics.

## 8. Operational concerns

| Concern | Behaviour |
|---|---|
| Test connection on credential create | CP `crypto.TestConnection(plaintext, provider)` — runs a cheap upstream call (e.g., list models) before accepting. Surface the result to admin. |
| Failure with `auth` class | Mark credential `status=degraded`; if N consecutive, auto-disable + alert. |
| Credential expiry (provider-side) | We don't proactively track expiry (providers vary); detection is via repeated `auth` failures. |
| Manual decrypt during incident | Tooling lives in `tools/db-migrate/manual-scripts/` (gated by env-var presence of the key). Never grep the DB for plaintexts; they don't exist there. |
| Lost `CREDENTIAL_ENCRYPTION_KEY` | All credentials are unrecoverable. Re-create via admin. |

## 9. Sources

- `packages/ai-gateway/internal/auth/vkauth/` — VK auth + RequestContext hydration.
- `packages/ai-gateway/internal/credentials/manager/` — provider credential CRUD entry points.
- `packages/ai-gateway/internal/credentials/pool/` — pool + selection (L1-L3 weighted).
- `packages/ai-gateway/internal/credentials/stats/` — health rollup.
- `packages/ai-gateway/internal/credentials/decrypt/` — in-memory decrypted-credential cache.
- `packages/shared/schemas/credstate/` — shared Redis key/enum/threshold constants (no behaviour).
- `packages/control-plane/internal/platform/crypto/` — encryption / decryption helpers, test connection.
- `tools/db-migrate/schema.prisma:391` — `Credential` model (and `VirtualKey` model nearby).
- `.env.example:42-57` — credential-encryption env vars.

## 10. Cross-references

- `routing-architecture.md` — fallback chain interacts with pool health.
- `error-taxonomy-architecture.md` — `ErrorClass` drives per-credential circuit.
- `tenancy-architecture.md` — credentials are strictly org-scoped.
- `alerting-architecture.md` — credential-health alert rules.
- `audit-pipeline-architecture.md` — credential-related audit events.
- `idp-sso-architecture.md` — Nexus is the SP; not the same kind of credential.
