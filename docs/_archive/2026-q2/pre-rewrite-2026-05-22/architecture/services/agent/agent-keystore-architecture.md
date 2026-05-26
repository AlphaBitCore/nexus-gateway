---
doc: agent-keystore-architecture
area: service
service: agent
tier: 1
updated: 2026-05-21
---

# Agent Keystore Architecture

> **Tier 2 architecture doc.** Read when touching `packages/agent/internal/identity/keystore/`. The keystore wraps the platform's native secret store for the one secret the agent persists outside the filesystem: the SQLCipher audit DB key.

The agent's other long-lived material (device mTLS key + cert, Ed25519 attestation key + cert, device-token, thing-id) is written as ordinary files inside `certDir` by the enrollment manager — not via the keystore. The keystore exists to give the audit DB key a platform-grade hiding place (Keychain / DPAPI) where casual file copy isn't enough.

---

## 1. What's stored in the keystore

A single key:

| Key name | Bytes | Why protected |
|---|---|---|
| `nexus-agent-audit-db-key` | 32 random bytes (AES-256) | Encrypts the local SQLCipher audit / queue / staging DB |

Everything else lives on disk:

- **Device mTLS material** — `device.pem` (cert), `device-key.pem` (ECDSA P-256 private key), `gateway-ca.pem`, `device-id`, `thing-id`, `device-token`, `trust-level`, `sso-email`. Written atomically by `enrollment.Manager.persistHubEnrollment` into `certDir` at file mode `0600`.
- **Ed25519 attestation key** — `attestation-key.pem` written by the same enrollment flow when Hub returns an attestation cert.

There is no long-lived "user SSO bearer" or "user SSO refresh" cache. SSO enrollment is one-shot: the user signs in, the IdP returns an enrollment JWT, the agent uses it immediately to call `POST /api/internal/things/enroll`, then discards it. The persistent identity is the device mTLS cert + `device-token`; sign-out is a re-enrollment, not a token refresh.

## 2. Per-platform backing

### macOS (`keystore_darwin.go`)

`Security.framework` Keychain via the `keybase/go-keychain` library (avoids the `/usr/bin/security` CLI subprocess, which would leak the key on argv to any `ps` reader). Items are written as Generic Passwords under service `com.nexus-gateway.agent`, with `SynchronizableNo` + `AccessibleWhenUnlockedThisDeviceOnly` so the secret never leaves the host and never participates in iCloud Keychain sync.

### Windows (`keystore_windows.go`)

DPAPI (`CryptProtectData` / `CryptUnprotectData` via `crypt32.dll`) wraps the raw key bytes; the wrapped ciphertext is then base64-encoded and persisted at `~/.nexus/secrets/<key>.dpapi` with file mode `0600`. The DPAPI envelope is bound to the user account, so even local filesystem inspection by a different OS user cannot recover the plaintext.

### Linux (`keystore_linux.go`)

`FileStore`: writes base64-encoded bytes to `~/.nexus/secrets/<key>.key` at file mode `0600`. Protection is filesystem ACL only — there is no libsecret integration in this package (libsecret support, if needed, lives in the sibling `secretstore` package and is not wired into the audit-DB-key path). No boot_id KDF, no derived encryption: the on-disk file is the raw key after base64. The package source file carries an explicit `LIMITATION:` comment flagging this and recommending TPM2 or a secrets manager for production hardening.

## 3. The Go interface

```go
package keystore

// Store provides platform-specific secret storage.
type Store interface {
    Get(key string) ([]byte, error)   // returns nil if not found
    Set(key string, value []byte) error
    Delete(key string) error
}

// NewPlatformStore returns the platform-native Store implementation.
// Each *_<goos>.go file ships its own constructor.
func NewPlatformStore() Store
```

The `Store` interface is `Get / Set / Delete` only — no `Put`, no `List`. Construction is platform-specific (each `*_<goos>.go` builds its own concrete type) and reflective discovery isn't needed because the package only ever holds a single key.

## 4. The audit DB key lifecycle

The helper `GetOrCreateDBKey(store Store) ([]byte, error)` is the only consumer:

1. `store.Get("nexus-agent-audit-db-key")` returns the cached key on subsequent boots.
2. On the first boot the key is missing → 32 bytes from `crypto/rand` → `store.Set(...)` → return the new key.
3. SQLCipher opens the local audit DB with the key.

If the keystore returns an error (Keychain locked, DPAPI service down, secrets dir unwritable), `GetOrCreateDBKey` propagates the error — the caller in `cmd/agent` treats this as a fatal boot condition because an unreadable audit DB key means SQLCipher cannot open the queue. There is no automatic wipe-and-fresh-start path.

## 5. Sign-out / unenroll

Sign-out reuses the enrollment lifecycle: `auth.ClearEnrollment` deletes the `device-token` and `thing-id` files inside `certDir`, intentionally leaving `device.pem` + `device-key.pem` so a subsequent re-enrollment with the same machine identity is fast. The audit DB key in the keystore is **not** touched — it belongs to the local SQLCipher database, which survives sign-out.

## 6. Forensic considerations

The keystore + SQLCipher together form a defence-in-depth layer:

- Casual local-user inspection (file copy of the audit DB) yields nothing — the DB is encrypted at rest with the keystore-held key.
- A motivated attacker with the matching OS user (or root on Linux) can extract the audit DB key — at which point they already own the host. The keystore is **not** a defence against rooted attackers; it defends against casual disclosure (forgotten laptop, IT inspection without proper authority).

## 7. Sources

- `packages/agent/internal/identity/keystore/keystore.go` — `Store` interface + `GetOrCreateDBKey`.
- `packages/agent/internal/identity/keystore/keystore_darwin.go` — macOS Keychain backing.
- `packages/agent/internal/identity/keystore/keystore_windows.go` — DPAPI backing.
- `packages/agent/internal/identity/keystore/keystore_linux.go` — `FileStore` (0600 file, no libsecret).
- `packages/agent/internal/identity/secretstore/` — separate package (file-based fallback for an unrelated secret, not used for the audit DB key).
- `packages/agent/internal/observability/audit/queue/queue.go` — SQLCipher DB opened with the keystore-held key.
- `packages/agent/internal/identity/enrollment/enroll.go` — writes device + attestation cert/key files into `certDir`.

## 8. Cross-references

- `agent-paths-abstraction-architecture.md` — `StateDir` holds the audit DB and the enrollment cert files.
- `agent-enrollment-architecture.md` — device + attestation cert/key file lifecycle.
- `audit-pipeline-architecture.md` — SQLCipher staging.
