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

The Prisma `Credential` table (`tools/db-migrate/schema/providers.prisma`) holds encrypted material as four hex-encoded `text` columns:

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

### 2.1 Class-separated derivation + row-identity AAD

The configured master is never used directly as the AEAD key. Both the Control Plane `Vault` and the AI Gateway `Decryptor` HKDF-derive a **provider-credential class sub-key** from it via the shared `packages/shared/core/keyderive` helper (`keyderive.DeriveKey32(master, ClassProviderCredential)`) at construction. The Hub alert-channel cipher (`packages/nexus-hub/internal/alerts/engine/secret.go`) derives a **different** sub-key (`ClassAlertChannelSecret`) from the same master. So although one env value seals two secret classes, the AEAD keys are distinct — a usage scoped to one class never yields the master that unlocks the other. The derivation is a cross-service [MUST MATCH] contract; centralizing it in `keyderive` (with a golden-vector test) makes "CP and ai-gw agree" a compile-time guarantee.

Every provider-credential seal/open also binds **Additional Authenticated Data** = `keyderive.ProviderCredentialAAD(credentialID, providerID)`. The AAD is the row's own immutable identity, so a ciphertext copied from credential A's row into credential B's row fails GCM authentication on open (instead of silently decrypting to A's, possibly higher-privilege, upstream key). To make the id available at *seal* time, the create paths generate the credential id (and, for the create-provider-with-inline-credential path, the provider id) **app-side** before encrypting, rather than relying on the DB `gen_random_uuid()` default. The key-rotation worker re-seals each row under the same AAD, so the binding survives rotation.

There is no legacy decryption fallback: ciphertext must be sealed by the current derived-key + AAD scheme. Fresh deployments are unaffected; a deployment carrying credentials sealed under an older scheme re-enters its provider keys (and alert-channel secrets) — the operational step lives in the `prod-deploy` skill.

## 3. Lifecycle

Admin CRUD lives under `POST/GET/PUT/DELETE /api/admin/credentials` plus four operational endpoints, all gated by IAM middleware on the `ResourceCredential` namespace (see [`iam-identity-architecture.md`](../../services/control-plane/iam-identity-architecture.md) for the verb model):

| Endpoint | IAM action | Role |
|---|---|---|
| `POST /credentials` | `admin:credential.create` | Encrypt plaintext key with current vault, insert four encrypted columns + metadata. |
| `GET /credentials` / `GET /credentials/:id` | `admin:credential.read` | Metadata-only response — encrypted columns are not in the API shape. |
| `PUT /credentials/:id` | `admin:credential.update` | Optional re-encrypt path when `apiKey` is supplied; otherwise metadata-only update. |
| `DELETE /credentials/:id` | `admin:credential.delete` | Hard delete. |
| `POST /credentials/:id/probe` | `admin:credential.probe` | Synchronous reliability probe against a stored credential. |
| `POST /credentials/:id/circuit-reset` | `admin:credential.update` | Force-closes the per-credential circuit breaker. Clears **both** the durable DB columns and the live Redis hash (see §6). |
| `POST /credentials/rotate-key` | `admin:credential.rotate` | Initiates the background key-rotation worker (multi-key vault only). |
| `GET /credentials/key-rotation-status` | `admin:credential.read` | Returns the current rotation state and pending count for the active rotation worker. |
| `GET /credentials/rotation-status` | `admin:credential.read` | Lists per-credential `rotationState` column values for the rotation Settings page. |
| `PUT /credentials/:id/reliability-overrides` | `admin:credential.update` | Adjusts per-credential reliability thresholds. |

Every create / update / delete invokes `hub.InvalidateConfigE("ai-gateway", "credentials")` so the Hub re-pushes the updated encrypted snapshot to every AI Gateway Thing. These are security-sensitive writes: the CP DB commits first (source of truth), then the push runs, and **a push failure returns HTTP 502 with a `propagation_error` envelope** (`code: HUB_PROPAGATION_FAILED`) — the handler does **not** report success while the fleet still serves a stale (e.g. just-deleted) credential. The success audit row is written only after the push succeeds, so the audit reflects the true outcome. The background master-key re-encryption worker (§3.1) is the one exception: it re-encrypts ciphertext without changing the decrypted key, has no HTTP caller to surface a 502 to, and stays fire-and-forget. Audit entries capture actor, verb, resource ID, and outcome — never the plaintext key, never the encrypted columns.

### 3.1 Key rotation

Rotation is one-shot: an admin calls `POST /credentials/rotate-key`, an atomic guard prevents concurrent rotations, and a background worker re-encrypts every credential whose `encryption_key_id` does not match the current key. Each row is decrypted with its existing key, re-encrypted with the current key, and the four encrypted columns are overwritten in place. There is no overlapping read-old/write-new window — once a row's `encryption_key_id` advances, the old key is no longer used for that row. When the worker finishes, it triggers a Hub push so AI Gateways pick up the new ciphertexts.

A row that cannot be re-encrypted — most often one whose `encryption_key_id` is no longer present in `CREDENTIAL_KEY_MAP`, which `MultiVault.Decrypt` rejects with "unknown encryption key ID" — is recorded as **stuck**, logged, and added to an in-memory exclusion set so the next batch query (`encryption_key_id != current AND id <> ALL(excluded)`) never re-selects it. This is the worker's liveness contract: a stuck row is queried at most once, so each batch strictly shrinks the candidate set and the run always terminates and always releases the concurrency guard — one stale-key row can never spin the worker forever or wedge every future rotation behind a permanent `409 ROTATION_IN_PROGRESS`. Because stuck rows are excluded rather than retried in-loop, healthy rows queued behind them still migrate in the same run. The completion log reports the rotated and stuck counts; the stuck rows remain on their old key and still show in the `key-rotation-status` pending count, so an operator restores the missing key (or deletes the orphan rows) and re-runs `rotate-key` to retry.

The old key must remain in `CREDENTIAL_KEY_MAP` until every row has migrated; removing it before then leaves orphan rows that fail decryption at request time and surface as 5xx on the data path. The `GET /credentials/key-rotation-status` endpoint reports the pending count so operators can confirm zero before retiring an old key.

## 4. Distribution to AI Gateway

Credentials never leave the Control Plane in plaintext. The flow is:

1. CP encrypts the plaintext via the vault and writes the four encrypted columns.
2. CP calls `hub.InvalidateConfigE("ai-gateway", "credentials")` after every mutation; a push failure becomes an HTTP 502 (`propagation_error`) so the admin retries rather than believing a revoked credential already stopped working.
3. The Hub config-push channel pushes the encrypted credential snapshot to every connected AI Gateway Thing.
4. AI Gateway holds the encrypted columns in memory; plaintext is not pre-materialised.
5. On a request the credential manager (`packages/ai-gateway/internal/credentials/manager/manager.go`) calls `Decryptor.Decrypt` JIT, caches the plaintext in a per-credential entry, and serves the upstream HTTP call. The cache TTL is short and the cache invalidates whenever a new Hub push arrives, so admin updates take effect on the next request without restart. Concurrent decrypt requests for the same credential are collapsed via singleflight so a popular credential's rotation never produces a stampede of `aead.Open` calls.

When the Hub push hasn't reached the gateway yet (cold start, transient WebSocket break) the gateway falls back to a direct DB read against the same encrypted columns via `packages/ai-gateway/internal/platform/store/credential.go`. The DB row carries the same `encryption_key_id` field, so the `MultiDecryptor` route works identically against either source.

## 5. Probe paths (test-connection)

Two distinct probe surfaces exist; both delegate the actual upstream HTTP call to AI Gateway so the connectivity check uses the real adapter code path.

**Saved-credential probe** — `POST /credentials/:id/probe` (handler `ProbeCredential` in `credential_reliability.go`) is a thin proxy: it gates the request with IAM + audit, then forwards the body verbatim to `AI Gateway POST /internal/v1/credentials/:id/probe`. The gateway side reads the encrypted row, decrypts via its credential manager, and runs the adapter's `Probe()` against the upstream. Plaintext never enters Control Plane memory on this path; the gateway returns `{ok, latencyMs, detail, error}` and CP proxies it to the admin UI. Audit records the outcome only.

**Saved-provider probe (legacy 'test stored provider' button)** — `POST /providers/:id/test` (handler `ProviderTest` in `provider_test_conn.go`) decrypts on the Control Plane side via `decryptCredentialByID` and forwards `{name, adapterType, baseUrl, apiKey}` to `AI Gateway /internal/provider-test`. Plaintext lives only in request-scoped memory for the duration of the call.

**Draft-credential probe (create wizard)** — `POST /providers/test-connection` (handler `ProviderTestConnection`) accepts `{name, adapterType, baseUrl, apiKey}` directly from the admin UI form, validates the adapter type against `IsValidAdapterType`, and forwards the same payload to `AI Gateway /internal/provider-test`. Plaintext lives only in: the operator's browser memory, the HTTPS request body, the CP handler's request-scoped variables, and the inter-service POST body to the gateway. It is never persisted, never logged, and never written to the DB. If the operator abandons the wizard, the plaintext is garbage-collected with the request scope.

This path is gated on `provider:create` (the provider-config-write tier), **not** `provider:read`: it dials a caller-supplied base URL and relays the upstream status + error detail, which is a blind-SSRF / internal-endpoint fingerprinting oracle. Gating it on the write tier means only a caller who could already configure a provider (and thus set the base URL anyway) can run it, preserving the diagnostic detail for legitimate admins while closing the oracle for read-only viewers. The egress itself is guarded at the AI Gateway: the shared provider-probe client installs an SSRF dial guard (`shared/transport/http` `AdminEgressAllowPrivate`) that refuses the cloud-metadata / link-local range (169.254.169.254 et al.) at dial time while still permitting on-prem RFC-1918 / loopback provider endpoints (self-hosted vLLM/Ollama). The guard runs on the resolved address, defeating DNS-rebinding.

## 6. Circuit breaker state & recovery

The per-credential circuit breaker has its state in **two stores that must stay consistent**:

- **Live — Redis hash `cred:circuit:{id}`** (keys in `packages/shared/schemas/credstate`). This is what the AI Gateway reads during pool selection (`credpool.BulkCircuitStates` / `CircuitReader` in `packages/ai-gateway/internal/credentials/pool/pool.go`). A missing hash means *closed*.
- **Durable — `Credential.circuitState / circuitReason / circuitOpenedAt / circuitNextProbeAt`**. This is what the admin UI badge shows, and what the Hub rehydrates Redis from on restart.

**Open.** The gateway's `RecordAttempt` (`packages/ai-gateway/internal/credentials/stats/buffer.go`) opens the circuit on upstream failures: a single `429` → `open` with reason `rate_limit` and a cooldown `next_probe_at`; three consecutive `401/403` → `open` with reason `auth_fail`. Each transition `SADD`s the credential to `cred:circuit:dirty`.

**Persist.** The Hub `credential-circuit-flush` job (`packages/nexus-hub/internal/jobs/defs/retention/credential_circuit_flush.go`, ~30s) drains `cred:circuit:dirty` into the durable columns (at-least-once via a per-Hub in-flight working set). On first run after a restart it **rehydrates** Redis from the durable columns so a wiped Redis cannot silently re-arm — but it deliberately skips a `rate_limit` row whose cooldown has already elapsed.

**Recover.** A `rate_limit` circuit auto-promotes `open → half_open` once `next_probe_at` passes (on the next Redis read), and a subsequent `2xx` closes it (DEL + dirty). An `auth_fail` circuit has no time-based recovery — a bad key will not fix itself, so it requires a manual reset.

**Manual reset (`POST /credentials/:id/circuit-reset`).** Reconciles **both** stores: it `UPDATE`s the durable columns to closed (`credstore.ClearCircuit`) so the UI clears instantly and a later Hub restart cannot re-arm from a stale row, and it `DEL`s the live Redis hash (and `SREM`s any pending dirty marker) so the gateway sees the credential as eligible immediately. Reconciling both stores is load-bearing: clearing Redis without the DB `UPDATE` would leave the durable row "open" forever — nothing would mark it dirty, so the flush job would never reconcile it, and a restart would re-arm the live circuit from the stale row.

**Auto-clear on key replacement.** Replacing a credential's upstream key (`PUT /credentials/:id` with a new `apiKey`) runs the same both-stores reconcile (`Handler.clearCredentialCircuit`, shared with manual reset) once the new ciphertext is persisted. Without this, an `auth_fail` circuit — which has no time-based recovery — would strand a just-fixed key "open" until someone also remembered the separate `circuit-reset` call. The clear is best-effort relative to the update: the key swap already committed, and a `rate_limit` circuit self-heals on its own cooldown regardless, so a failed clear is logged rather than failing the request.

**Self-heal.** The flush job periodically (every `circuitReconcileInterval`, default 5m) reconciles **orphans** — durable rows that are non-closed but whose live Redis hash is absent — by force-closing them. This covers any path that can leave the stores divergent (a Redis eviction, or a cooldown-elapsed `rate_limit` that rehydrate did not re-arm). It only acts when Redis is genuinely absent (the live state is already closed), so it can never close a circuit that is legitimately open, and it skips in-flight members so it never races the flush writer.

**Single-credential caveat.** Pool selection only consults the circuit when a provider has **more than one** credential (`resolveCredential` in `packages/ai-gateway/internal/providers/target/resolver.go`). A single-credential provider is never excluded — failing closed on the only key would guarantee an outage rather than fail over — so for such providers the circuit is advisory state surfaced in the UI and recovered via `RecordAttempt`, not an enforcement gate.

## 7. Redaction principles

Plaintext key material is kept out of logs and out of observable response bodies by construction, not by post-hoc redaction:

- The credstore's API response shape (`credstore.Credential`) does not include the encrypted columns — they are read into a separate `CredentialEncrypted` struct only when the request-path explicitly needs them.
- Log statements in the credential lifecycle and probe handlers identify rows by ID and report error class, never by ciphertext content or plaintext. The decryptor's error branches log the key ID, not the failed bytes.
- Audit events for credential operations populate `audit.Entry` (`packages/control-plane/internal/platform/audit/writer.go`) with `ActorID`, `Action`, `EntityType = "credential"`, `EntityID`, and outcome flags in `BeforeState` / `AfterState`. The state slots are `any`-typed but the credential handlers populate them with metadata-only payloads — `{id, name, providerId}` on create/update, `{ok, error}` for probe, `{cleared: true}` for circuit-reset, threshold values for reliability overrides — never plaintext or ciphertext.

## 8. Known limitations and deferred concerns

Tracked for when an actual product driver appears, not built ahead of demand:

- **Per-credential-type schema** — every credential today is a single opaque `apiKey` string encrypted into one row. Provider-specific shapes that need a structured secret (OAuth refresh tokens, AWS access-key + secret-key pairs, Vertex service-account JSON) currently ride as opaque strings that the adapter splits at use time. A future per-type schema with multiple encrypted columns per credential is feasible — the storage model already separates ciphertext / IV / tag per field — but no in-tree consumer requires it yet.
- **No KMS/HSM integration (V2 roadmap item)** — The credential master key (`CREDENTIAL_ENCRYPTION_KEY`) lives only in process memory, sourced from a plain environment variable (or HKDF-derived from a passphrase+salt pair). There is no KMS-managed key-encryption key (KEK) and no HSM-backed key storage. Production operators must supply `CREDENTIAL_ENCRYPTION_KEY` from a secrets manager (e.g. AWS Secrets Manager, HashiCorp Vault) and inject it at deploy time via the `EnvironmentFile=` systemd directive or equivalent. Enterprise deployments that require auditable key access, HSM-backed isolation, or rotation-without-redeploy need envelope encryption with an external KMS-managed KEK. AWS KMS / GCP KMS / HashiCorp Vault wrappers can substitute behind the existing `Vault` / `MultiVault` interface without changing callers; the env-only baseline is intentionally the OSS default so a fresh deploy is operable with no cloud-secret-manager dependency. Envelope encryption with a KMS-managed KEK is planned for production hardening (see roadmap).

## References

- `tools/db-migrate/schema/providers.prisma` — `Credential` model (encrypted column layout).
- `packages/control-plane/internal/platform/crypto/aes_gcm.go` — `Vault` + `MultiVault` AES-256-GCM wrappers.
- `packages/control-plane/cmd/control-plane/wiring/crypto.go` — CP-side vault init from env.
- `packages/control-plane/internal/ai/providers/credstore/credential.go` — `Credential` / `CredentialEncrypted` row models + DB CRUD.
- `packages/control-plane/internal/ai/providers/handler/credentials.go` — admin CRUD handler + Hub invalidation.
- `packages/control-plane/internal/ai/providers/handler/key_rotation.go` — background rotation worker.
- `packages/control-plane/internal/ai/providers/handler/credential_reliability.go` — saved-credential probe proxy.
- `packages/control-plane/internal/ai/providers/handler/credentials.go` `CircuitReset` + `credstore.ClearCircuit` — manual circuit reset (DB + Redis reconcile, §6).
- `packages/nexus-hub/internal/jobs/defs/retention/credential_circuit_flush.go` — durability flush, restart rehydrate, and orphan self-heal reconcile (§6).
- `packages/ai-gateway/internal/credentials/pool/pool.go` + `internal/providers/target/resolver.go` — circuit-aware pool selection (multi-credential only, §6).
- `packages/control-plane/internal/ai/providers/handler/provider_test_conn.go` — draft + saved-provider probe paths.
- `packages/control-plane/internal/platform/audit/writer.go` — `audit.Entry` shape.
- `packages/ai-gateway/cmd/ai-gateway/wiring/thingclient.go` — gateway-side `InitCredManager`.
- `packages/ai-gateway/cmd/ai-gateway/configdispatch/configdispatch.go` — Hub `credentials` push handler that calls `CredManager.ClearCache()`.
- `packages/ai-gateway/internal/credentials/decrypt/decrypt.go` — gateway `Decryptor` + `MultiDecryptor`.
- `packages/ai-gateway/internal/credentials/manager/manager.go` — JIT decrypt + plaintext cache + singleflight.
- `packages/ai-gateway/internal/platform/store/credential.go` — gateway-side DB fallback read.
- `packages/shared/identity/iam/catalog_data.go` — `ResourceCredential` IAM verb catalogue.
