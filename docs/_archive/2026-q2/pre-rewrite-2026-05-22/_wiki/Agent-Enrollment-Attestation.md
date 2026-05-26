# Agent Enrollment Attestation

*Audience: operators provisioning devices; security reviewers auditing the agent identity model.*

Agent enrollment turns a one-time admin-issued token (or an SSO-produced enrollment JWT) into a device-bound mTLS identity plus an Ed25519 attestation identity. The Nexus Hub runs a self-issued ECDSA P-256 CA; each agent presents two certificate signing requests in the same enrollment call — one for mTLS transport authentication and one for request attestation. Attestation allows the Compliance Proxy to recognize agent-inspected traffic and skip redundant compliance work on those flows.

---

## Enrollment ceremony

On first boot, the agent generates its identity cryptographic material and registers with Hub:

1. The agent generates a fresh ECDSA P-256 keypair and constructs an mTLS CSR with `CommonName: "device-<hostname>"`. No SAN is embedded; Hub is the authoritative assigner of the `thing_id`.
2. In parallel, the agent generates an Ed25519 keypair and a second CSR (`CommonName: "device-<hostname>-attestation"`). This step is fail-open: a crypto error returns empty strings and enrollment proceeds for the mTLS cert alone.
3. The agent POSTs `HubEnrollRequest` to `POST /api/internal/things/enroll`, authenticated by either:
   - `X-Enrollment-Token: <admin-issued-token>` — standard token flow.
   - `Authorization: Bearer <enrollment-JWT>` — SSO flow (browser-driven PKCE, one-shot).

Hub validates the auth, signs both CSRs via the Hub CA, inserts the `thing` row (type `Agent`), mints the runtime `deviceToken`, and returns both signed certs plus the Hub CA chain. The agent writes all artifacts atomically into its `certDir`:

- `device-key.pem` + `device.pem` — P-256 mTLS identity
- `attestation-key.pem` + `attestation.pem` — Ed25519 attestation identity
- `gateway-ca.pem`, `device-id`, `thing-id`, `device-token`, `trust-level`

The agent then establishes a mTLS WebSocket to Hub using the new cert. If an enrollment token was used, Hub marks it `redeemed` at this point.

A device fingerprint (128-bit hardware-stable hash from `opsmetrics.ComputeDeviceFingerprint`) allows Hub to reuse the existing `thing_id` when the same physical host re-enrolls, rather than creating a duplicate entry.

## Token vs SSO enrollment

Two authentication paths reach the same `POST /api/internal/things/enroll` endpoint:

| Path | Trigger | Auth header |
|---|---|---|
| Token | Admin issues a token in CP UI → Devices → Device Auth; shares plaintext once via out-of-band channel | `X-Enrollment-Token` |
| SSO | End-user opens agent UI → clicks "Sign in" → browser PKCE flow against CP → one-shot enrollment JWT | `Authorization: Bearer <enrollment-JWT>` |

Enrollment tokens are single-use (status flips to `redeemed` after one successful enrollment), expire after a configured duration (typically 24 hours), and can carry optional constraints:

| Field | Purpose |
|---|---|
| `os_constraint` | Restrict to `mac`, `linux`, or `windows` |
| `device_id_hint` | Pre-assign a device ID |
| `default_role` | Landing-zone role for JIT-provisioned users |

For SSO enrollment, the agent generates a PKCE verifier/challenge, starts an ephemeral HTTP callback server on an OS-assigned port (`127.0.0.1:0`), and opens the system browser. The browser completes the OAuth+PKCE flow against CP, which redirects back to the agent's callback with an auth code. The agent exchanges the code for an enrollment JWT — which is consumed exactly once to mint the device cert. After enrollment completes, the agent holds no OAuth tokens; all further Hub communication uses the device cert over mTLS.

The signed-in email is persisted locally for menu-bar display only. It carries no API authority.

## Dual-cert design rationale

The enrollment produces two independent certs because the two keys serve different cryptographic roles:

- The **P-256 mTLS cert** authenticates the `agent ↔ Hub` transport handshake.
- The **Ed25519 attestation cert** signs the `x-nexus-attestation` header on outbound HTTP requests so the Compliance Proxy can verify the agent already inspected the payload.

Combining these roles into one key would create a NIST SP 800-57 key-separation violation and would introduce ECDSA per-signature nonce risk (ECDSA nonce reuse leaks the key; Ed25519 is deterministic and not vulnerable). Using two keys keeps the mTLS chain unchanged and limits the blast radius of any attestation key compromise.

## Attestation header

When `attestationEnabled` is set in the device's `agent_settings`, the agent signs every outbound HTTP request with its Ed25519 key:

```
x-nexus-attestation: v1;ts=1716100000;nonce=ab12cd34...;hash=sha256:abc123...;agent_id=550e8400-...;sig=base64url(Ed25519-sig)
```

The header binds the request timestamp, a random nonce (replay protection), the SHA-256 hash of the request body, and the agent's UUID to a single Ed25519 signature. The Compliance Proxy, when transparently in the network path, bumps TLS, reads the inner HTTP request, and verifies the signature. A valid signature triggers pure passthrough — no hook pipeline, no duplicate `traffic_event` row. The agent's audit row is the system-of-record for the flow.

Invalid, expired, or missing attestation headers fall back to the normal MITM compliance pipeline. Attestation is a performance optimization, not a security gate.

## Attestation: cryptographic design summary

Ed25519 is used for attestation (rather than HMAC or reusing the ECDSA mTLS key) for three reasons:

- **Per-agent isolation** — each agent has its own Ed25519 keypair. Compromising one device's attestation key does not allow forging attestations for other agents. An HMAC approach would require a shared secret across the fleet.
- **Deterministic signatures** — Ed25519 is deterministic; ECDSA requires a fresh random nonce per signature and leaks the private key if the nonce reuses. The deterministic property eliminates an entire class of crypto implementation bugs.
- **Reuses existing PKI** — the attestation cert is signed by the same Hub CA during enrollment, so no new key distribution infrastructure is needed.

The signing pre-image is a canonical newline-separated string: `v1\nts=<unix-seconds>\nnonce=<32-hex>\nhash=sha256:<hex>\nagent_id=<UUID>\n`. Adding a field in a future v2 header does not change v1 verification semantics.

The Compliance Proxy verifies the header before the compliance pipeline runs. A valid signature triggers pure passthrough — no hooks, no duplicate audit row. Invalid, expired, or missing headers fall back to the normal MITM pipeline. Attestation never blocks a request; it is a performance optimization, not a security gate.

## Failure modes

| Failure | Behavior |
|---|---|
| Token expired | Hub returns 410; admin re-issues token |
| Token already redeemed | Hub returns 409 |
| CSR invalid | Hub returns 400; agent regenerates and retries |
| Cert expired without renewal (agent was offline) | Admin re-issues token; agent re-enrolls |
| Lost device private key (keystore wipe) | Admin re-issues token; agent re-enrolls |
| Stolen device cert | Admin revokes via CP UI; agent enters revoked mode |
| macOS NE extension not approved | User sees system prompt; must allow in System Settings |
| Platform keystore unavailable | Agent surfaces in Health & Diagnostics; cannot persist key safely |

## Cert lifecycle: renewal and revocation

Device certs have a short validity (90 days by default). Renewal is automatic and uses the same enrollment endpoint; the current cert authorizes the renewal call, no additional token is needed:

1. ~14 days before expiry, the agent generates a fresh CSR and POSTs to `POST /api/internal/things/enroll` over the existing mTLS connection.
2. Hub signs the new cert and returns it.
3. The agent atomically swaps the cert + key files in `certDir`.

Admin-initiated revocation:

1. Admin revokes via CP UI → Devices → select device → Revoke.
2. Hub flips `thing.status = revoked`, adds the cert serial to the CRL, and closes the agent's WS session.
3. The agent's subsequent mTLS calls fail and the device enters minimal-functionality mode.
4. Re-enrollment requires a new token or SSO flow.

---

## Canonical docs

- [`agent-enrollment-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-enrollment-architecture.md) — Full cryptographic flow; Hub CA design; dual-cert rationale; renewal and revocation
- [`agent-sso-enrollment-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-sso-enrollment-architecture.md) — SSO-driven enrollment; browser PKCE flow; ephemeral callback port; no-token-refresh model
- [`agent-attestation-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-attestation-architecture.md) — Attestation header format; CP verification flow; dual-cert decision record; threat model
- [`agent-enrollment.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/flows/agent-enrollment.md) — Step-by-step operator enrollment flow from CP UI to first event

**Adjacent wiki pages**: [Agent Overview](Agent-Overview) · [Agent macOS NE Architecture](Agent-macOS-NE-Architecture) · [Agent Policy Evaluation](Agent-Policy-Evaluation) · [Installing The Desktop Agent](Installing-The-Desktop-Agent) · [Trust Boundaries](Trust-Boundaries)
