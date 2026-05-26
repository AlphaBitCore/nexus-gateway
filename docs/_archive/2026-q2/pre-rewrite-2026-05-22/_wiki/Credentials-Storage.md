# Credentials Storage

*Audience: security reviewers and contributors working with provider credentials, virtual keys, or the credential encryption pipeline.*

Nexus Gateway stores two kinds of credentials: **virtual keys** (bearer tokens that applications use to call the AI Gateway) and **provider credentials** (API keys that Nexus uses to call AI providers). Virtual keys are stored as HMAC-SHA256 hashes — the plaintext is never stored. Provider credentials are stored encrypted at rest with AES-256-GCM using a master key sourced exclusively from environment variables. No secret appears in any committed YAML file; every secret lives in `.env.example` and is injected at runtime.

---

## Virtual keys

A virtual key (VK) is a bearer credential scoped to one project (and transitively one organization). Applications send `Authorization: Bearer vk-...` to the AI Gateway ingress.

The VK auth path (`packages/ai-gateway/internal/auth/vkauth/`):

1. Parse the `Authorization: Bearer ...` header.
2. Hash the secret with constant-time HMAC-SHA256 and look up by `hashed_secret` in the DB.
3. Reject if `status != active` or `expires_at < now`.
4. Hydrate `RequestContext` with `VirtualKeyID`, `OrganizationID`, `ProjectID`, allowed models, allowed providers, and `quota_policy_id`.
5. Stamp `last_used_at` (rate-limited to once per minute per VK to avoid write storms).

The plaintext secret is never stored or logged. Audit rows record `VirtualKeyID` only. VK rotation works by issuing a new VK and revoking the old — there is no in-place rotation because the secret IS the identity.

VKs resolve organization scope through two join chains: application VKs via `Project → Org` and personal VKs via `NexusUser → Org`. The `vkauth` query covers both chains to prevent personal VKs from silently resolving to NULL org.

## Provider credentials — AES-256-GCM

Provider API keys and service-account JSON are stored encrypted. The `Credential` schema in `tools/db-migrate/schema.prisma` stores three components per credential:

| Column | Contents |
|---|---|
| `encryptedKey` | AES-256-GCM ciphertext |
| `encryptionIv` | 96-bit IV (unique per encryption operation) |
| `encryptionTag` | 128-bit GCM authentication tag |
| `encryptionKeyId` | Key version label (default `"v1"`) — selects which master key decrypts this row |

The plaintext is decrypted only in memory, only for the request lifetime. `packages/ai-gateway/internal/credentials/decrypt/` holds an in-memory decrypted-credential cache scoped narrowly to the request — entries are invalidated via Hub change-signal when a credential is updated.

When Control Plane updates a credential: CP encrypts with a new IV + tag → persists → notifies Hub → Hub signals AI Gateway via WS → AI Gateway invalidates the affected cache entry → next request fetches fresh and decrypts. No service restart required, no stale-credential window.

## Encryption key management

Three env variables carry the master-key material. All are documented in `.env.example` (lines 42-57). None appear in any committed YAML — this is a hard architectural binding.

| Env var | Marked | Purpose |
|---|---|---|
| `CREDENTIAL_ENCRYPTION_KEY` | `[MUST MATCH]` AI GW + CP | AES-256 master key, 64 hex chars (32 bytes). Generate: `openssl rand -hex 32` |
| `CREDENTIAL_KEY_MAP` | `[MUST MATCH]` AI GW + CP | Optional multi-key rotation map. Format: `"v1:<hex64>,v2:<hex64>"`. Takes precedence when present. |
| `CREDENTIAL_ENCRYPTION_PASSPHRASE` + `CREDENTIAL_ENCRYPTION_SALT` | CP only | Passphrase mode — master key derived via scrypt. Leave empty if not using passphrase mode. |

`[MUST MATCH]` means AI Gateway and Control Plane must have identical values — drift produces decryption failures on the data plane.

### Key rotation

The `CREDENTIAL_KEY_MAP` supports zero-downtime rotation:

1. Deploy CP + AI Gateway with `CREDENTIAL_KEY_MAP="v1:<old_key>,v2:<new_key>"`.
2. New credential writes use `v2`; existing rows still decrypt via `v1` (the `encryptionKeyId` column selects the entry).
3. Re-encrypt existing rows in batches, bumping `encryptionKeyId` to `v2`.
4. Once every row is on `v2`, drop the `v1:` entry on the next deploy.

No decrypt failures during the migration window. If `CREDENTIAL_ENCRYPTION_KEY` is lost without a backup, all credentials are unrecoverable — re-create them via the admin UI.

## Credential pool

AI Gateway resolves provider credentials through a pool (`packages/ai-gateway/internal/credentials/pool/`):

1. Filter to credentials for the required (org, provider) combination.
2. Drop credentials with `status != active` or an open circuit breaker.
3. Pick one — weighted round-robin with stickiness on (org, model) for cache locality.

Per-credential health is tracked in `credstats` (5-minute rolling windows, 429 counts, consecutive failure counts). When the failure threshold is exceeded, the credential's circuit opens and it is excluded from the pool. Only when all credentials for a (provider, model) are open does the pool-level circuit open.

The `CredentialRef` carrier on `ResolvedRequest` holds only `{CredID, Version, Provider}` — the plaintext never reaches routing-engine memory. The executor calls `credstate.Acquire(ref)` at dispatch time to get the plaintext, narrowly scoped.

## Audit invariants

Every credential operation emits an audit row — creation, update, revoke, rotation — with the credential ID and masked metadata, never the plaintext. Traffic events stamp `credential_id` (not the secret) so investigators can correlate "which credential was used" with health metrics.

Admin-audit event types:
- `admin:credential.create` / `update` / `revoke` — includes credential ID, provider, masked metadata.
- `admin:vk.create` / `revoke` — includes VK ID, project, allowed-model restrictions.

## Operational concerns

| Concern | Behaviour |
|---|---|
| Test connection on credential create | CP runs a cheap upstream call (e.g., list models) before accepting the credential. Result surfaced to admin. |
| Repeated auth failures | Mark credential `status=degraded`; after N consecutive, auto-disable + alert. |
| Credential expiry (provider-side) | Not proactively tracked; detection is via repeated `auth` failures. |
| Lost `CREDENTIAL_ENCRYPTION_KEY` | All credentials are unrecoverable. Re-create via admin UI. |
| Manual decrypt during incident | Tooling in `tools/db-migrate/manual-scripts/` (gated by env-var presence of the key). Ciphertext exists in DB; plaintext never does. |

The `credstate` shared constants package (`packages/shared/schemas/credstate/`) is the single source of truth for Redis key prefixes (`cred:stats:<id>`, `cred:circuit:<id>`), circuit-state values (`closed | open | half_open`), open-reason values (`auth_fail | rate_limit`), and default reliability thresholds. AI Gateway, Control Plane, and Nexus Hub all import these symbols — they never each define their own.

---

## Canonical docs

- [`credentials-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/credentials-architecture.md) — VK auth, provider credential schema, pool selection, key management, rotation flow
- [`packages/shared/schemas/credstate/`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/packages/shared/schemas/credstate/) — shared Redis key prefixes, circuit-state enums, health-classification values

**Adjacent wiki pages**: [Trust Boundaries](Trust-Boundaries) · [Configuration Architecture](Configuration-Architecture) · [Storage Cache MQ Stack](Storage-Cache-MQ-Stack) · [AI Gateway Virtual Keys Quotas](AI-Gateway-Virtual-Keys-Quotas) · [Operations Credential Rotation](Operations-Credential-Rotation)
