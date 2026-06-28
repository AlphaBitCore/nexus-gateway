# Nexus Hub enrollment architecture

The Hub is the enrollment authority for the fleet. It runs the agent
Certificate Authority, issues the credentials an agent needs to talk to the
platform (an mTLS client certificate, a device token, and an optional
attestation certificate), verifies enrollment JWTs minted by the Control Plane
in enterprise-login mode, and serves the unauthenticated bootstrap endpoint
agents call before they have any credential. This doc covers the Hub-side
enrollment authority; the agent-side enrollment client and platform keystore are
documented under the agent service.

## Identity subsystem

`cmd/nexus-hub/wiring/identity.go` (`InitIdentity`) wires three pieces:

- **JWKS cache** — created only when `authServer.jwksURL` is configured. Without
  it, Bearer (enrollment-JWT) enrollment is rejected with 503 and the Hub logs a
  startup WARN; token-based enrollment still works.
- **Agent CA** — loaded from explicit `agentCA.certFile` / `agentCA.keyFile` when
  both are set; otherwise loaded-or-generated in `agentCA.dir` (default
  `.agent-ca`). The file-pair path never auto-generates.
- **Enrollment service** — the enrollment-token store wrapper.

## Agent certificate authority

`internal/identity/agentca` manages a self-signed **ECDSA P-256** CA. The CA
certificate is valid for ten years; issued client certificates are valid for
ninety days. The CA private key is written to disk with `0600` permissions. The
CA exposes two issuance paths:

- **`SignCSR`** — signs an agent-submitted PKCS#10 CSR for the mTLS identity. The
  issued certificate carries `KeyUsage=DigitalSignature` and
  `ExtKeyUsage=ClientAuth`. The agent keeps its private key; only the CSR and
  public key reach the Hub.
- **`SignAttestationCSR`** — signs the optional traffic-attestation CSR. It
  enforces two invariants: the CSR public key **must be Ed25519** (ECDSA / RSA
  are rejected), and the issued certificate has `KeyUsage=DigitalSignature` with
  **no `ExtKeyUsage`** — in particular no `ClientAuth`. This is the
  key-separation rule: a compromised attestation key cannot be used as an mTLS
  client certificate to impersonate the agent's identity.

## Credentials issued at enrollment

A successful enrollment hands the agent three credentials:

| Credential | Purpose | Storage |
|---|---|---|
| Device token | Bearer credential for ongoing `/api/internal/things/*` calls | 32 random bytes; plaintext hex returned once, SHA-256 hash persisted on the Thing; bounded lifetime (`agentca.DeviceTokenTTL`, 30 days) stamped on `thing.device_token_expires_at` |
| mTLS client certificate | Transport identity (P-256, `ClientAuth`) | Agent holds the private key; Hub stores the cert serial + expiry on `thing_agent` |
| Attestation certificate (optional) | Per-request traffic attestation | Ed25519, 90-day validity; the public-key bytes **and** the cert `NotAfter` (`certExpiresAt`) are stamped into `thing_agent.sysinfo` and served via `GET /api/internal/things/:id/attestation-pubkey` for the Compliance Proxy to verify signed attestation headers |

**Attestation cert expiry is enforced at verify time.**
The `GET /api/internal/things/:id/attestation-pubkey` response carries
`certExpiresAt` alongside `publicKey`; the Compliance Proxy's
`AttestationVerifier` rejects a key once `now > certExpiresAt` (outcome
`expired` → MITM fallback). Without this the 90-day cert was advisory — a
compromised or exfiltrated attestation key would have bypassed compliance
inspection forever. The expiry rides through the CP key cache as
`AttestationKey.CertExpiresAt`; a legacy stamp with no expiry on record is
treated as non-expiring (fail-open).

**Revocation.** The `GET /attestation-pubkey` query joins `thing` and
excludes `status = 'revoked'`, so a revoked (unenrolled) device's attestation key
is not served — the row is filtered out → `404` → CP `unknown_agent` → MITM
fallback. This makes `UnenrollDevice` (which sets `thing.status = 'revoked'`) the
single revocation lever: unenrolling a decommissioned or known-compromised device
immediately stops its attested traffic from bypassing inspection. Because the
trust decision is enforced at the serving query (not by mutating
`sysinfo.attestation`), it is reversible and catches every revocation path that
sets the status, not just the CP admin route. Propagation is bounded by the CP
key cache's ≤60 s positive TTL (after which it re-fetches and gets the `404`); an
immediate Hub→CP cache `Invalidate(agentID)` push to close that ≤60 s window is an
available optimization (`AttestationKeyCache.Invalidate` exists) but not required
for the time-bounded-revocability invariant.

Device tokens (`agentca.GenerateDeviceToken` / `HashDeviceToken`) are compared by
SHA-256 hash, so the plaintext is never stored. The same hashing gates the
device-token auth middleware below.

Device tokens expire. Each token is issued (at enrollment) or rotated (see
[Device-token rotation](#device-token-rotation)) with a fixed TTL
(`agentca.DeviceTokenTTL`, 30 days); the absolute expiry is stamped on the
first-class `thing.device_token_expires_at` column. `ValidateDeviceToken` filters
on `device_token_expires_at > NOW()`, so an expired (or never-issued) token is
rejected fail-closed by the lookup itself — there is no separate Go-side expiry
check that a future edit could skip. The agent rotates its token well before the
TTL lapses, so a healthy device never lets its token expire; a stolen plaintext
token (read from the agent's on-disk credential file) is replayable for at most
one rotation period instead of forever.

## Enrollment tokens

For the `mtls-only` device-auth mode, enrollment is gated by an admin-minted
enrollment token. `internal/identity/enrollment` (backed by
`internal/identity/store/enrollstore`) mints an opaque `enroll-<hex>` token via
the admin endpoint `POST /api/hub/enrollment/token`; the raw string is returned
once at creation and the table stores only its SHA-256 hash in `token_hash`. A
token carries a `thingType`, a label, and a status (`pending` / `used` /
`revoked` / `expired`) and defaults to a 24-hour expiry. `ValidateToken` accepts
a token only while it is `pending` and unexpired; `MarkUsed` flips it to `used`
and binds it to the enrolled `thingId`.

## The enrollment endpoint

`POST /api/internal/things/enroll` (`internal/identity/handler/enroll`) is the
handshake. A CSR is always required. The handler picks the credential path from
the request headers:

1. `Authorization: Bearer <enrollment-jwt>` → SSO enrollment (enterprise-login
   mode).
2. `X-Enrollment-Token: <opaque-token>` → token enrollment (mtls-only mode).

**The minted Thing's *type* is authoritative per path, never caller-controlled.**
The enrollment-token path pins `thingType` from the
operator-issued token row — a token cut for an `agent` cannot enroll an
`ai-gateway`. The SSO/JWT path is a *device-enrollment* grant (authorized by the
device verb `admin:device-enrollment.enroll`, which a normal user holds to enroll
their own desktop agent), so it is hard-pinned to `agent`: a request asking for a
service-type Thing (`ai-gateway` / `compliance-proxy` / `control-plane`) is
refused `403 ENROLL_TYPE_FORBIDDEN` before any Thing is minted. This matters
because a service-type Thing inherits that tier's entire desired state on
registration (providers / credentials / virtual_keys / routing_rules / …) and
would receive every future service-config push. Service Things therefore enroll
**only** via the operator-issued enrollment-token path, never via a user-obtained
SSO/JWT credential.

Both paths converge on `doEnroll`, which runs in this order:

1. Sign the CSR with `SignCSR` (subject `device-<thingId>`).
2. Mint a device token (`GenerateDeviceToken`).
3. Register the Thing (`RegisterThing`), stamping `physical_id` from the
   device fingerprint when present.
4. Store the device-token hash on the Thing together with its expiry
   (`NOW() + agentca.DeviceTokenTTL` on `device_token_expires_at`).
5. Upsert `thing_agent` with hostname / OS / cert serial / cert expiry.
6. When an Ed25519 attestation CSR rides along, sign it with
   `SignAttestationCSR` and store the attestation public key. Attestation
   failures are non-fatal — they are logged and skipped so an Ed25519 issuance
   error never breaks the mTLS enrollment the agent depends on.

The response carries the device token and its expiry (`deviceTokenExpiresAt`),
the signed certificate, the CA certificate, the cert serial and expiry, the
heartbeat interval, the computed trust level, and the Thing's initial desired
config (so the agent has its starting configuration without a separate pull). The desired/reported config
contract is owned by
[thing-config-sync-architecture.md](../../cross-cutting/foundation/thing-config-sync-architecture.md).

### Trust level and device assignment

The token path creates no device-to-user binding, so the Thing stays at the
base trust level. The SSO/JWT path upserts a `DeviceAssignment` (source `sso`)
binding the device to the authenticated user, which raises the trust level and
emits a tamper-evident `device-assignment.update` audit row through the shared
hash chain — see
[admin-audit-log-coverage.md](../../cross-cutting/observability/admin-audit-log-coverage.md).
When the request carries a hardware-stable device fingerprint, the SSO path
reuses the existing Thing row keyed by `physical_id` instead of minting a new one
on every reinstall or second SSO account from the same host.

## Enrollment-JWT verification

`verifyEnrollmentJWT` validates the Bearer enrollment JWT against an
RSA-signing-method check and the following pinned claims:

- `aud` must equal `nexus-hub-enrollment`.
- `iss` must equal the configured `authServer.issuer` (`CpIssuer`) — this stops
  a third-party signer that happens to publish keys on the same JWKS URL from
  impersonating the Control Plane.
- `purpose` must equal `enrollment`.
- `exp` is required, and `jti` must be present.

The `jti` feeds a two-layer replay guard. **L1** is an in-process map:
each JTI is recorded with its `exp`, rejected on reuse, and swept after it
expires. **L2** is the shared Redis SETNX dedup (`nexus:enroll:jti:<jti>` with
TTL = `exp − now`, the same primitive the spill-upload flow uses), wired from
`cfg.SpillDedup`. Because L2 is checked on every redemption, a captured JWT can no
longer be replayed across a **Hub restart** (which clears L1 but not Redis) or on
**another Hub** (multi-Hub HA): the SETNX miss is detected as a replay. A Redis
error degrades to the L1 single-Hub guarantee rather than blocking enrollment, and
when no Redis dedup is wired the guard is L1-only (legacy single-Hub behaviour).
Verification maps failures to three error codes: `JWT_INVALID` (bad signature,
wrong claim, expired, missing `jti`), `JWT_REPLAYED` (JTI already seen), and
`JWKS_UNAVAILABLE` (no keys cached).

### JWKS cache

`internal/jwks` fetches the Control Plane's RSA public keys from
`authServer.jwksURL`, indexes them by `kid`, and refreshes every five minutes.
On a fetch failure the last successfully fetched keys remain valid for a
ten-minute grace window; past that the cache is cleared and `Get` returns `ErrCacheEmpty`,
which the enrollment handler surfaces as a 503. An empty `kid` returns the sole
cached key as a single-key-set convenience.

## Device-token authentication

After enrollment, every agent call to `/api/internal/things/*` is gated by the
`DeviceOrServiceAuth` middleware, which accepts either credential:

- The **internal service token** (used by the Control Plane and other services),
  compared in constant time.
- A **device token** plus an `X-Thing-Id` header (or `id` query parameter). The
  token is SHA-256-hashed and validated against the stored hash for that Thing;
  on success the resolved Thing is attached to the request context.

The same group also fronts the agent-facing register / heartbeat / config-pull /
audit-upload routes, so a revoked device token immediately locks an agent out of
all of them.

## Device-token rotation

`POST /api/internal/things/renew-token` is the device-token counterpart of the
cert-renewal endpoint. It takes no body and trusts no caller-supplied identity:
it runs inside the `DeviceOrServiceAuth` group, so the caller's *current*
still-valid device token authenticates the call and the resolved Thing in the
request context is exactly the identity whose token is rotated. The handler mints
a fresh token, overwrites the stored hash, and re-stamps
`device_token_expires_at` with a new TTL — overwriting the hash invalidates the
previous token immediately, so a rotation bounds any stolen token's lifetime.

Two consequences fall out of authenticating with the current token:

- An expired token cannot rotate itself — `ValidateDeviceToken` fails closed
  before the handler runs. Rotation must therefore happen while the token is
  still valid (refresh-while-valid); the agent's multi-day renewal window
  guarantees it.
- The internal service token has no device identity in context, so a
  service-token caller is rejected — only a device rotates its own token.

## Bootstrap and device-auth modes

`GET /api/public/agent-bootstrap` is unauthenticated by design — a
pre-enrollment agent has no client certificate yet. It returns two fields:

- `controlPlaneURL` — the operator-set Control Plane URL (`authServer.url`), so a
  fresh install only needs the Hub URL the installer already configured.
- `deviceAuthMode` — read live from the `device.auth.mode` key in
  `system_metadata`, defaulting to `mtls-only`. The raw value `local-login` is
  normalised to `enterprise-login` on this agent-facing surface so a single
  agent build drives both SSO modes through one browser-SSO branch; the raw
  value stays in `system_metadata` for admin and audit visibility.

The response is cached for sixty seconds to bound DB load under boot-storm
conditions. The two modes select the enrollment credential path:
`mtls-only` uses an enrollment token; `enterprise-login` uses an enrollment JWT
obtained through the agent's browser-SSO flow.

## References

- `packages/nexus-hub/cmd/nexus-hub/wiring/identity.go` — identity subsystem wiring
- `packages/nexus-hub/internal/identity/agentca/` — agent CA, CSR signing, device-token generation
- `packages/nexus-hub/internal/identity/enrollment/` — enrollment-token service
- `packages/nexus-hub/internal/identity/store/enrollstore/` — enrollment-token + device-assignment storage
- `packages/nexus-hub/internal/identity/handler/enroll/` — enrollment endpoint, JWT verification, device-token auth middleware
- `packages/nexus-hub/internal/identity/handler/bootstrap/` — public bootstrap endpoint
- `packages/nexus-hub/internal/jwks/` — Control Plane JWKS cache
