# Credentials architecture

Provider API keys are the most sensitive secrets the Nexus Gateway holds. They are encrypted at rest with AES-256-GCM, persisted only in the Control Plane database, distributed to AI Gateway in encrypted form via the Hub push channel, decrypted just-in-time on the request path, and never logged. The encryption keys themselves come from environment variables — there is no KMS or cloud-secret-store dependency, so a fresh deployment needs only `CREDENTIAL_ENCRYPTION_KEY` (or a passphrase+salt pair) set in `bootenv`.

Anchor packages:

- `packages/control-plane/internal/platform/crypto/aes_gcm.go` — `Vault` (single-key) and `MultiVault` (key-rotation) wrappers around Go's `crypto/cipher` AES-GCM. The authoritative format definition.
- `packages/control-plane/internal/ai/providers/credstore/credential.go` — DB row model (`Credential` + `CredentialEncrypted`) and CRUD methods.
- `packages/control-plane/internal/ai/providers/handler/credentials.go` — admin HTTP CRUD wired to the credstore + vault; route table + IAM middleware.
- `packages/control-plane/internal/ai/providers/handler/key_rotation.go` — background re-encryption worker for multi-key rotation.
- `packages/control-plane/internal/ai/providers/handler/provider_test_conn.go` — draft + saved provider probe flows (plaintext lives only in-memory).
- `packages/ai-gateway/internal/credentials/decrypt/decrypt.go` — gateway-side `Decryptor` and `MultiDecryptor`.
- `packages/ai-gateway/internal/credentials/manager/manager.go` — JIT decrypt + plaintext cache used by the request path.
- `packages/ai-gateway/internal/platform/store/credential.go` — gateway-side DB read for encrypted columns when the Hub-pushed cache misses.

## 1. Storage model

The Prisma `Credential` table (`tools/db-migrate/schema.prisma`) holds encrypted material as four hex-encoded `text` columns:

| Column | Holds |
|---|---|
| `encryptedKey` | hex-encoded ciphertext bytes |
| `encryptionIv` | hex-encoded 96-bit AES-GCM nonce |
| `encryptionTag` | hex-encoded 128-bit AES-GCM authentication tag |
| `encryption_key_id` | encryption-key version identifier for this row (defaults to `"v1"` on insert) |

Metadata (`name`, `providerId`, `enabled`, `rotationState`, `lastRotatedAt`, `lastUsedAt`, lifetime + circuit-breaker + reliability columns, `selectionWeight`, `expiresAt`) lives in the same row as plain typed columns. The encryption columns are split per field deliberately — the decryptor receives three separate hex strings; there is no JSON wrapper, version byte, or framed envelope. Adding a new encrypted field means three new `text` columns + one `encryption_key_id` reference, not one packed BLOB.

The HTTP response shape for `GET /credentials` (defined as `credstore.Credential`) **omits** the encrypted columns entirely — they cannot be exfiltrated by listing credentials through the admin API.

## 2. Encryption

The cipher is AES-256-GCM as exposed by Go's `crypto/cipher.NewGCMWithTagSize(block, 16)`. Key length is fixed at 32 bytes (256-bit); nonce is 12 bytes drawn fresh per encryption from `crypto/rand.Reader`; tag is 16 bytes returned by `aead.Seal`. The Decryptor enforces those exact lengths and rejects shorter / longer inputs before calling `aead.Open`, so a corrupted row fails closed instead of producing implausible plaintext.

Key material is supplied through environment variables, validated once at Control Plane boot in `packages/control-plane/cmd/control-plane/wiring/crypto.go: InitCrypto`:

- **Single-key mode (default)** — `CREDENTIAL_ENCRYPTION_KEY` set to a 64-character hex string (32 raw bytes) selects `Vault`. As a convenience for dev environments, `CREDENTIAL_ENCRYPTION_PASSPHRASE` + `CREDENTIAL_ENCRYPTION_SALT` derive the 32-byte key via HKDF-SHA256. Production deploys (controlled by the `Production` flag in `VaultConfig`) reject configurations that leave both unset.
- **Multi-key mode (rotation)** — `CREDENTIAL_KEY_MAP` set to `id1:hex1,id2:hex2,…` selects `MultiVault`. The last entry in the map is the *current* key used for new encryptions; older entries remain available for decrypting rows whose `encryption_key_id` still points at them. This is the seam that key rotation runs through.

The same env contract is read independently by the AI Gateway side (`packages/ai-gateway/cmd/ai-gateway/wiring/thingclient.go: InitCredManager` constructs the gateway-side `Decryptor` / `MultiDecryptor` and credential `Manager` from the same env variables) so both services derive an identical decryption key. There is no inter-service key transport — both sides bootstrap from the same `bootenv` block.

## 3. Lifecycle

Admin CRUD lives under `POST/GET/PUT/DELETE /api/admin/credentials` plus four operational endpoints, all gated by IAM middleware on the `ResourceCredential` namespace (see [`iam-identity-architecture.md`](../../services/control-plane/iam-identity-architecture.md) for the verb model):

| Endpoint | IAM action | Role |
|---|---|---|
| `POST /credentials` | `admin:credential.create` | Encrypt plaintext key with current vault, insert four encrypted columns + metadata. |
| `GET /credentials` / `GET /credentials/:id` | `admin:credential.read` | Metadata-only response — encrypted columns are not in the API shape. |
| `PUT /credentials/:id` | `admin:credential.update` | Optional re-encrypt path when `apiKey` is supplied; otherwise metadata-only update. |
| `DELETE /credentials/:id` | `admin:credential.delete` | Hard delete. |
| `POST /credentials/:id/probe` | `admin:credential.probe` | Synchronous reliability probe against a stored credential. |
| `POST /credentials/:id/circuit-reset` | `admin:credential.update` | Clears the per-credential failure circuit-breaker state. |
| `POST /credentials/rotate-key` | `admin:credential.rotate` | Initiates the background key-rotation worker (multi-key vault only). |
| `GET /credentials/key-rotation-status` | `admin:credential.read` | Returns the current rotation state and pending count for the active rotation worker. |
| `GET /credentials/rotation-status` | `admin:credential.read` | Lists per-credential `rotationState` column values for the rotation Settings page. |
| `PUT /credentials/:id/reliability-overrides` | `admin:credential.update` | Adjusts per-credential reliability thresholds. |

Every create / update / delete / rotation invokes `hub.InvalidateConfig("ai-gateway", "credentials")` so the Hub re-pushes the updated encrypted snapshot to every AI Gateway Thing. Audit entries capture actor, verb, resource ID, and outcome — never the plaintext key, never the encrypted columns.

### 3.1 Key rotation

Rotation is one-shot: an admin calls `POST /credentials/rotate-key`, an atomic guard prevents concurrent rotations, and a background worker re-encrypts every credential whose `encryption_key_id` does not match the current key. Each row is decrypted with its existing key, re-encrypted with the current key, and the four encrypted columns are overwritten in place. There is no overlapping read-old/write-new window — once a row's `encryption_key_id` advances, the old key is no longer used for that row. Rows that fail to re-encrypt are skipped with a logged error and counted in the pending set; an operator can re-run `rotate-key` to retry. When the worker finishes, it triggers a Hub push so AI Gateways pick up the new ciphertexts.

The old key must remain in `CREDENTIAL_KEY_MAP` until every row has migrated; removing it before then leaves orphan rows that fail decryption at request time and surface as 5xx on the data path. The `GET /credentials/key-rotation-status` endpoint reports the pending count so operators can confirm zero before retiring an old key.

## 4. Distribution to AI Gateway

Credentials never leave the Control Plane in plaintext. The flow is:

1. CP encrypts the plaintext via the vault and writes the four encrypted columns.
2. CP calls `hub.InvalidateConfig("ai-gateway", "credentials")` after every mutation.
3. The Hub config-push channel pushes the encrypted credential snapshot to every connected AI Gateway Thing.
4. AI Gateway holds the encrypted columns in memory; plaintext is not pre-materialised.
5. On a request the credential manager (`packages/ai-gateway/internal/credentials/manager/manager.go`) calls `Decryptor.Decrypt` JIT, caches the plaintext in a per-credential entry, and serves the upstream HTTP call. The cache TTL is short and the cache invalidates whenever a new Hub push arrives, so admin updates take effect on the next request without restart. Concurrent decrypt requests for the same credential are collapsed via singleflight so a popular credential's rotation never produces a stampede of `aead.Open` calls.

When the Hub push hasn't reached the gateway yet (cold start, transient WebSocket break) the gateway falls back to a direct DB read against the same encrypted columns via `packages/ai-gateway/internal/platform/store/credential.go`. The DB row carries the same `encryption_key_id` field, so the `MultiDecryptor` route works identically against either source.

## 5. Probe paths (test-connection)

Two distinct probe surfaces exist; both delegate the actual upstream HTTP call to AI Gateway so the connectivity check uses the real adapter code path.

**Saved-credential probe** — `POST /credentials/:id/probe` (handler `ProbeCredential` in `credential_reliability.go`) is a thin proxy: it gates the request with IAM + audit, then forwards the body verbatim to `AI Gateway POST /internal/v1/credentials/:id/probe`. The gateway side reads the encrypted row, decrypts via its credential manager, and runs the adapter's `Probe()` against the upstream. Plaintext never enters Control Plane memory on this path; the gateway returns `{ok, latencyMs, detail, error}` and CP proxies it to the admin UI. Audit records the outcome only.

**Saved-provider probe (legacy 'test stored provider' button)** — `POST /providers/:id/test` (handler `ProviderTest` in `provider_test_conn.go`) decrypts on the Control Plane side via `decryptCredentialByID` and forwards `{name, adapterType, baseUrl, apiKey}` to `AI Gateway /internal/provider-test`. Plaintext lives only in request-scoped memory for the duration of the call.

**Draft-credential probe (create wizard)** — `POST /providers/test-connection` (handler `ProviderTestConnection`) accepts `{name, adapterType, baseUrl, apiKey}` directly from the admin UI form, validates the adapter type against `IsValidAdapterType`, and forwards the same payload to `AI Gateway /internal/provider-test`. Plaintext lives only in: the operator's browser memory, the HTTPS request body, the CP handler's request-scoped variables, and the inter-service POST body to the gateway. It is never persisted, never logged, and never written to the DB. If the operator abandons the wizard, the plaintext is garbage-collected with the request scope.

## 6. Redaction principles

Plaintext key material is kept out of logs and out of observable response bodies by construction, not by post-hoc redaction:

- The credstore's API response shape (`credstore.Credential`) does not include the encrypted columns — they are read into a separate `CredentialEncrypted` struct only when the request-path explicitly needs them.
- Log statements in the credential lifecycle and probe handlers identify rows by ID and report error class, never by ciphertext content or plaintext. The decryptor's error branches log the key ID, not the failed bytes.
- Audit events for credential operations populate `audit.Entry` (`packages/control-plane/internal/platform/audit/writer.go`) with `ActorID`, `Action`, `EntityType = "credential"`, `EntityID`, and outcome flags in `BeforeState` / `AfterState`. The state slots are `any`-typed but the credential handlers populate them with metadata-only payloads — `{id, name, providerId}` on create/update, `{ok, error}` for probe, `{cleared: true}` for circuit-reset, threshold values for reliability overrides — never plaintext or ciphertext.

## 7. Deferred concerns

Tracked for when an actual product driver appears, not built ahead of demand:

- **Per-credential-type schema** — every credential today is a single opaque `apiKey` string encrypted into one row. Provider-specific shapes that need a structured secret (OAuth refresh tokens, AWS access-key + secret-key pairs, Vertex service-account JSON) currently ride as opaque strings that the adapter splits at use time. A future per-type schema with multiple encrypted columns per credential is feasible — the storage model already separates ciphertext / IV / tag per field — but no in-tree consumer requires it yet.
- **External KMS integration** — keys come from env vars today. AWS KMS / GCP KMS / HashiCorp Vault wrappers can substitute the `Vault` / `MultiVault` interface without changing callers, but the env-only baseline is intentionally the OSS default so a fresh deploy is operable with no cloud-secret-manager dependency.

## References

- `tools/db-migrate/schema.prisma` — `Credential` model (encrypted column layout).
- `packages/control-plane/internal/platform/crypto/aes_gcm.go` — `Vault` + `MultiVault` AES-256-GCM wrappers.
- `packages/control-plane/cmd/control-plane/wiring/crypto.go` — CP-side vault init from env.
- `packages/control-plane/internal/ai/providers/credstore/credential.go` — `Credential` / `CredentialEncrypted` row models + DB CRUD.
- `packages/control-plane/internal/ai/providers/handler/credentials.go` — admin CRUD handler + Hub invalidation.
- `packages/control-plane/internal/ai/providers/handler/key_rotation.go` — background rotation worker.
- `packages/control-plane/internal/ai/providers/handler/credential_reliability.go` — saved-credential probe proxy.
- `packages/control-plane/internal/ai/providers/handler/provider_test_conn.go` — draft + saved-provider probe paths.
- `packages/control-plane/internal/platform/audit/writer.go` — `audit.Entry` shape.
- `packages/ai-gateway/cmd/ai-gateway/wiring/thingclient.go` — gateway-side `InitCredManager`.
- `packages/ai-gateway/cmd/ai-gateway/configdispatch/configdispatch.go` — Hub `credentials` push handler that calls `CredManager.ClearCache()`.
- `packages/ai-gateway/internal/credentials/decrypt/decrypt.go` — gateway `Decryptor` + `MultiDecryptor`.
- `packages/ai-gateway/internal/credentials/manager/manager.go` — JIT decrypt + plaintext cache + singleflight.
- `packages/ai-gateway/internal/platform/store/credential.go` — gateway-side DB fallback read.
- `packages/shared/identity/iam/catalog_data.go` — `ResourceCredential` IAM verb catalogue.
