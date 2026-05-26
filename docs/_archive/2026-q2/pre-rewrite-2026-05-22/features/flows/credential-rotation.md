# Flow — Provider credential rotation

## What this flow accomplishes

Rotate an upstream provider API key with **zero downtime** and **no service restart**.

## Actors

Admin · CP · Hub · AI Gateway · App.

## Sequence

1. **Admin obtains a new API key from the provider** (OpenAI / Anthropic / Gemini / …).
2. **Admin → CP UI → AI Gateway → Credentials → select credential → "Update Key"** → paste new plaintext → save.
3. **CP** runs `crypto.TestConnection(plaintext, provider)` (cheap upstream call; e.g., list models). If it fails: surface the error, do **not** persist.
4. **CP** encrypts the new value with `CREDENTIAL_ENCRYPTION_KEY` → forwards to Hub → Hub persists `credential` row (`encrypted_blob`, `version += 1`, `updated_at` refreshed) → previous plaintext is overwritten in DB.
5. **Hub** → change-signal AI Gateway shadow (`credentials/v=N+1`).
6. **AI Gateway** → `credstate.MarkDirty(credId)` on the change-signal.
7. **Next `/v1/*` request** that uses this credential:
   - `credstate.Acquire(credId)` sees dirty → fetches the new encrypted blob from Hub.
   - Decrypts in memory only.
   - Caches with the new version; clears the dirty flag.
   - Forwards to upstream with the new key.
8. **Audit row** `admin:credential.update` recorded; never logs the plaintext.

End-to-end: typically seconds. No service restart anywhere. No in-flight requests interrupted.

## Failure modes

- **Test connection fails** — surface the provider's error verbatim (sanitised); reject save.
- **Cred validates but is wrong-org** — provider returns auth-class error after rotation. Per-credential circuit opens; the pool excludes it; alert fires.
- **`credstate` race** — atomic flag; multiple in-flight requests may each fetch once, but only one actually hits Hub thanks to single-flight.
- **Hub down at rotation moment** — CP error surfaces; admin retries.
- **Rollback needed** — there is no credential history. Rollback requires inputting the previous (still-valid) key via the same Update Key flow.

## Verification

```bash
# 1) Note current credential version:
docker exec postgres psql -U postgres -d nexus_gateway \
  -c "SELECT id, version, updated_at FROM credential WHERE id='cred-...'"

# 2) Rotate via UI or:
cp_curl -X PUT /api/admin/credentials/cred-... -d '{"plaintext":"<new key>"}'

# 3) Confirm version bumped.

# 4) Issue a request that uses this credential; observe success.
curl ... /v1/chat/completions

# 5) Confirm audit row:
docker exec postgres psql -U postgres -d nexus_gateway \
  -c "SELECT event_type, emitted_at FROM admin_audit WHERE event_type='admin:credential.update' ORDER BY emitted_at DESC LIMIT 1"
```

## References

- `docs/developers/architecture/cross-cutting/safety/credentials-architecture.md` §2 — `credstate` dirty-set + version.
- `docs/developers/architecture/cross-cutting/foundation/multi-endpoint-coordination-architecture.md` §6 — flow diagram.
- `docs/users/features/cp-ui/ai-gateway.md` — admin surface.
- `docs/developers/architecture/cross-cutting/observability/admin-audit-log-coverage.md` — emitted audit shape.
