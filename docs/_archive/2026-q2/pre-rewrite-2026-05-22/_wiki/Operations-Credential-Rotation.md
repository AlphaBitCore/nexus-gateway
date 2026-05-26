# Operations Credential Rotation

*Audience: operators managing a running Nexus Gateway deployment.*

Nexus Gateway manages four distinct credential types, each with its own rotation procedure. Provider API keys rotate through the admin UI with zero downtime and no service restart. The `CREDENTIAL_ENCRYPTION_KEY` master key requires re-encrypting every credential row before the old key is removed. The `INTERNAL_SERVICE_TOKEN` is a shared secret that must be updated atomically across all four services. Desktop Agent mTLS certificates rotate via re-enrollment. Each type is documented separately below.

---

## Provider API keys

Provider API keys (OpenAI, Anthropic, Google, etc.) are encrypted at rest with AES-256-GCM. The ciphertext, IV, and authentication tag live in separate columns of the `Credential` table (`encryptedKey`, `encryptionIv`, `encryptionTag`). The plaintext is decrypted only in memory, only for the request lifetime — it never appears in logs, audit rows, or API responses.

### Rotation flow

Rotation is an atomic ciphertext swap: the admin submits a new plaintext, the Control Plane validates it against the upstream provider, encrypts it with the current master key, and persists the new ciphertext. The AI Gateway picks up the change within seconds via the Hub shadow change-signal.

1. Obtain a new API key from the provider (OpenAI, Anthropic, etc.).
2. Open the Control Plane UI at **AI Gateway → Credentials → [select credential] → Update Key**.
3. Paste the new plaintext and save.
4. The Control Plane runs a test connection (`crypto.TestConnection`) — a cheap upstream call to validate the key. If the test fails, the save is rejected and the old key remains active.
5. On success, the new value is encrypted and persisted. The previous ciphertext is overwritten.
6. Hub signals the AI Gateway via WebSocket shadow change-signal.
7. The next request that uses this credential fetches the new blob from Hub and decrypts it in memory. No service restart required.

The `admin:credential.update` audit event is recorded; the plaintext is never logged.

### Verification

```bash
# Source auth helper (or use cp_curl equivalent for prod)
source tests/lib/loadenv.sh && source tests/lib/auth.sh && cp_login

# Check the credential version incremented
cp_curl "/api/admin/credentials/<credId>"
# Look for "version": N+1

# Issue a request that uses this credential and confirm success
curl -H "Authorization: Bearer <virtual-key>" \
     http://127.0.0.1:3050/v1/chat/completions \
     -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"ping"}]}'

# Confirm the audit event
cp_curl "/api/admin/audit?eventType=admin%3Acredential.update&limit=1"
```

### Failure modes

- **Test connection fails**: the provider's error is surfaced verbatim; the old key remains active. Verify the new key is valid on the provider's dashboard before retrying.
- **Key is from the wrong org**: the provider returns an auth-class error on the first real request. The per-credential circuit opens and an alert fires (`credential.auth_failure`).
- **Rollback needed**: there is no credential history. Input the previous (still-valid) key via the same Update Key flow.

---

## CREDENTIAL_ENCRYPTION_KEY (master encryption key)

`CREDENTIAL_ENCRYPTION_KEY` is the AES-256 master key that encrypts every provider credential row. It is stored only in the environment (systemd `EnvironmentFile`, `.env` for local dev) and must be identical across the AI Gateway and Control Plane (`[MUST MATCH]` in `.env.example`).

Loss of this key makes every stored credential unrecoverable — the admin must re-enter all provider API keys.

### Multi-key rotation (non-disruptive)

Use `CREDENTIAL_KEY_MAP` to rotate without a decryption gap:

1. Deploy the Control Plane and AI Gateway with both the old and new keys in `CREDENTIAL_KEY_MAP`:

```bash
CREDENTIAL_KEY_MAP="v1:<old-hex64>,v2:<new-hex64>"
```

2. Set the new key as active for new writes (update the service config to prefer `v2`).
3. Re-encrypt existing rows in batches using the re-encryption utility in `tools/db-migrate/manual-scripts/`. Each row's `encryptionKeyId` column is updated from `v1` to `v2` as it is re-encrypted.
4. Verify all rows have `encryptionKeyId = 'v2'`:

```sql
SELECT encryption_key_id, COUNT(*) FROM "Credential" GROUP BY encryption_key_id;
-- Expected after full rotation: only v2 rows remain
```

5. Remove the `v1` entry from `CREDENTIAL_KEY_MAP` on the next deploy:

```bash
CREDENTIAL_KEY_MAP="v2:<new-hex64>"
```

This is non-disruptive: both keys are valid during the window, so no decrypt failures occur. Generate a new AES-256 key with `openssl rand -hex 32`.

---

## INTERNAL_SERVICE_TOKEN (inter-service auth)

`INTERNAL_SERVICE_TOKEN` is a shared bearer token used for service-to-service calls (Hub ↔ AI Gateway ↔ Control Plane ↔ Compliance Proxy). It is marked `[MUST MATCH]` in `.env.example` — all services must present and accept the same value.

Rotation requires a short maintenance window because there is no rolling-update grace period:

1. Generate a new token: `openssl rand -hex 32`.
2. Update `INTERNAL_SERVICE_TOKEN` in every service's environment file simultaneously.
3. Restart services in dependency order: Hub → Control Plane → AI Gateway → Compliance Proxy.
4. Verify all services registered with Hub:

```sql
SELECT id, type, status FROM thing ORDER BY type;
-- Expected: status = 'online' for each service
```

If a service fails to start with an auth error, verify the token value is byte-for-byte identical across all environment files.

Similarly, `COMPLIANCE_PROXY_API_TOKEN` and `AI_GATEWAY_API_TOKEN` are service-specific tokens for the compliance proxy and AI gateway respectively — both are `[MUST MATCH]` with Hub's expected values and rotate by the same procedure.

---

## Desktop Agent mTLS certificates (per-agent rotation)

Each Desktop Agent has a per-device mTLS certificate signed by the Agent CA. Certificate rotation happens through re-enrollment, not in-place replacement.

### Single-device rotation

1. Revoke the device's current enrollment from the Control Plane UI at **Agents → [select device] → Revoke**.
2. Generate a new enrollment token at **Agents → Enroll**.
3. Run the enrollment command on the device:

```bash
# macOS
sudo nexus-agent enroll --token <enrollment-token> --hub-url https://<hub-host>
```

4. Confirm the device appears online in the Control Plane UI.

### Bulk rotation after CA compromise

If the Agent CA key is compromised, all per-device certificates must be rotated:

1. Generate a new CA (Hub generates one automatically if the CA files are absent on restart — or replace the CA files manually).
2. Restart Hub with the new CA.
3. Revoke all existing enrollment tokens and issue new ones.
4. Push re-enrollment instructions to all devices (out-of-band, as agents cannot authenticate with the new CA until re-enrolled).

---

## Virtual key rotation

Virtual keys are rotated by issuing a new key and revoking the old — there is no in-place rotation because the secret is the key's identity.

1. Create a new virtual key in the Control Plane UI at **AI Gateway → Virtual Keys → Create**.
2. Update applications or SDKs to use the new key.
3. Revoke the old key once traffic has migrated.

A grace period (admin-configurable) keeps the old key accepting traffic while marked `status=expiring`, giving dashboards time to highlight the migration window.

---

## Canonical docs

- [`credentials-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/credentials-architecture.md) — AES-256-GCM encryption, `credstate` dirty-set, multi-key map rotation, pool behavior
- [`credential-rotation.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/flows/credential-rotation.md) — provider API key rotation flow diagram and verification steps
- [`.env.example`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/.env.example) — `[MUST MATCH]` cross-service secret catalog

**Adjacent wiki pages**: [Operations Backup Restore](Operations-Backup-Restore) · [Operations FAQ](Operations-FAQ) · [Credentials Storage](Credentials-Storage) · [Deployment Environment Variables](Deployment-Environment-Variables) · [AI Gateway Virtual Keys Quotas](AI-Gateway-Virtual-Keys-Quotas)
