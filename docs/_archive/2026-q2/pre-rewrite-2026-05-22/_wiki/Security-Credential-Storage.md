# Security Credential Storage

*Audience: security reviewers and operators managing provider credentials and virtual keys.*

Nexus Gateway stores two classes of sensitive credentials: provider API keys (used to call upstream AI providers) and virtual key secrets (used by applications to authenticate to the AI Gateway). Each class has a distinct protection scheme — provider keys are encrypted at rest with AES-256-GCM; virtual key secrets are HMAC-SHA256 hashed and never stored in recoverable form. This page covers the storage model, key sourcing, rotation propagation, and audit invariants for both classes.

---

## Provider credential encryption

Provider API keys are stored in the `Credential` table (`tools/db-migrate/schema.prisma`). The plaintext is never written to the database. Instead, three columns hold the components of an AES-256-GCM encryption:

| Column | Content |
|---|---|
| `encryptedKey` | Ciphertext of the provider API key |
| `encryptionIv` | 96-bit initialisation vector (random per write) |
| `encryptionTag` | 128-bit GCM authentication tag |
| `encryptionKeyId` | Master key version label (e.g. `"v1"`) for multi-key rotation |

AES-256-GCM provides both confidentiality and authenticated integrity — any tampering with the ciphertext causes decryption to fail, not silently return garbage.

### Master key sourcing

The master key is sourced exclusively from environment variables. Three env vars govern key material, all documented in [`.env.example`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/.env.example):

| Env var | Scope | Purpose |
|---|---|---|
| `CREDENTIAL_ENCRYPTION_KEY` | **[MUST MATCH]** AI Gateway + Control Plane | AES-256 master key, 64 hex chars (32 bytes). Generate with `openssl rand -hex 32`. |
| `CREDENTIAL_KEY_MAP` | **[MUST MATCH]** AI Gateway + Control Plane | Optional multi-key map for rotation: `"v1:<hex64>,v2:<hex64>"`. Takes precedence over `CREDENTIAL_ENCRYPTION_KEY` when present. |
| `CREDENTIAL_ENCRYPTION_PASSPHRASE` + `CREDENTIAL_ENCRYPTION_SALT` | Control Plane only | Alternative passphrase mode; master key derived via scrypt. Leave empty if not used. |

`[MUST MATCH]` means the value must be identical across all services that share it. Drift between Control Plane and AI Gateway is the most common source of decrypt failures. No secret field may appear in any committed YAML file — this is a hard binding that applies to all services.

### Decryption scope

The plaintext is decrypted into memory only for the lifetime of an individual request. The executor calls `credstate.Acquire(ref)` at dispatch time, keeping the plaintext narrowly scoped to request-local memory. The routing engine holds a `CredentialRef` (credential ID + version + provider), not the plaintext.

### Rotation propagation

When a provider credential is updated in the Control Plane:

1. CP encrypts the new value (new IV + tag) and persists the row.
2. CP forwards to Hub; Hub updates the shadow blob.
3. Hub sends a WebSocket change-signal to all AI Gateway instances.
4. Each AI Gateway invalidates the affected entry in its in-memory decrypted-credential cache.
5. The next request fetching that credential decrypts the fresh row.

No service restart is required. The rotation is end-to-end in seconds. The old plaintext is overwritten in the database — credential history is not kept.

### Key rotation with the multi-key map

To rotate the master encryption key without downtime:

1. Deploy Control Plane and AI Gateway with `CREDENTIAL_KEY_MAP="v1:<old_key>,v2:<new_key>"`.
2. Configure new credential writes to use `v2` as the active key (`encryptionKeyId="v2"`).
3. Re-encrypt existing rows in batches, bumping their `encryptionKeyId` to `v2`.
4. Once every row is on `v2`, drop the `v1:` entry on the next deploy.

During the window, both keys are available so there are no decrypt failures on rows not yet migrated.

If `CREDENTIAL_ENCRYPTION_KEY` is lost, all encrypted credentials become unrecoverable. There is no escrow. Recovery requires re-entering all provider credentials via the admin UI.

## Virtual key secret hashing

Virtual key secrets are bearer credentials used by applications to call the AI Gateway. The raw secret (`vk-...`) is presented exactly once — at creation time in the admin UI. The system immediately hashes it and discards the plaintext:

- `vkauth` (`packages/ai-gateway/internal/auth/vkauth/`) hashes incoming secrets at constant time and looks up by `hashed_secret` in the `virtual_key` table.
- The algorithm is HMAC-SHA256, keyed by `ADMIN_KEY_HMAC_SECRET` (`[MUST MATCH]` between Control Plane and AI Gateway).
- The hashed secret is never logged. Audit rows record only the `VirtualKeyID`.

VK rotation is performed by issuing a new virtual key and revoking the old. There is no in-place rotation — the secret is the identity.

## Compliance Proxy CA private key

The Compliance Proxy dynamically mints leaf certificates from a local sub-CA. The CA private key can be loaded from a KMS-wrapped ciphertext via an external command (`signingMode: "remote"`), keeping the raw key off disk. In remote signing mode, certificate signing is delegated to an external KMS command so the key never enters process memory. See [`docs/operators/ops/pki-and-certs.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/pki-and-certs.md) for the full PKI trust chain and key-protection options.

## Audit invariants

Every credential-related operation emits an audit record:

- `admin:credential.create` / `admin:credential.update` / `admin:credential.revoke` — records credential ID, provider, and masked metadata. The plaintext is never included.
- `admin:vk.create` / `admin:vk.revoke` — records virtual key ID and project scope.
- Per request: `traffic_event` stamps `credential_id` (not the plaintext), so investigators can correlate "which credential was used" with health metrics.

---

## Canonical docs

- [`docs/developers/architecture/cross-cutting/safety/credentials-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/credentials-architecture.md) — Virtual Key + Provider Credential + Credential Pool lifecycle
- [`.env.example`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/.env.example) — credential-encryption env vars (lines 42-57)
- [`docs/operators/ops/pki-and-certs.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/pki-and-certs.md) — Compliance Proxy PKI trust chain and key-protection options

**Adjacent wiki pages**: [Security Secrets Handling](Security-Secrets-Handling) · [Security Threat Model](Security-Threat-Model) · [Security Audit Forensics](Security-Audit-Forensics) · [Operations Credential Rotation](Operations-Credential-Rotation) · [Trust Boundaries](Trust-Boundaries)
