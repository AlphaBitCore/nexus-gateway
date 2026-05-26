---
doc: agent-enrollment-architecture
area: service
service: agent
tier: 1
updated: 2026-05-21
---

# Agent Enrollment + Hub CA + Cert Mint Architecture

> **Tier 1 architecture doc.** Read this before touching `packages/nexus-hub/internal/identity/agentca/`, `packages/nexus-hub/internal/identity/enrollment/`, agent bootstrap, or the device-cert lifecycle. The Thing model that an enrolled agent joins lives in `thing-model.md`; the mTLS traffic-path it uses to talk to Hub afterwards lives in `agent-forwarder-architecture.md`.

The enrollment flow turns a one-time admin-issued token (or an SSO-issued enrollment JWT) into a device-bound mTLS identity plus an Ed25519 attestation identity. The Hub runs a self-issued ECDSA P-256 CA; agents present **two** CSRs in the same request (a P-256 mTLS CSR and an Ed25519 attestation CSR); Hub signs both via the same CA; agents thereafter authenticate to Hub by mTLS and sign outbound-traffic attestations with the Ed25519 key.

---

## 1. Hub CA + dual-cert split

The Hub generates a CA keypair on first startup if one doesn't exist on disk. Properties:

- **Algorithm** — ECDSA P-256 (small certs, fast signing).
- **CA validity** — long (10 years, `caValidityYears` in `packages/nexus-hub/internal/identity/agentca/ca.go`); rotation is a runbook, not an automatic event.
- **Storage** — CA private key in a Hub-config-only file with restrictive perms (`0600`) by default; KMS integration is supported for high-security deployments.
- **Trust** — agents trust the Hub CA via the device cert chain returned at enrollment; the certs are short-lived and signed by this CA.

The same Hub CA signs **two** end-entity certs per enrolled agent:

1. **Device mTLS cert** — P-256 leaf, used for `agent ↔ Hub` mTLS.
2. **Attestation cert** — Ed25519 leaf, used only to sign the `X-Nexus-Attestation` header on outbound traffic so the compliance-proxy can transparently passthrough an attested flow. No `ClientAuth` EKU, no mTLS role — see `agent-attestation-architecture.md` §0.1 for the dual-cert rationale (NIST SP 800-57 key separation + no ECDSA RNG nonce footgun + small blast radius in `agentca`).

Both certs live independently on disk inside `certDir`. The agent's Ed25519 key never touches the TLS handshake; the agent's P-256 key never signs attestation headers.

The Hub CA is **not** the same as the compliance-proxy's local CA for TLS bump. The Hub CA secures `agent ↔ Hub` mTLS and stamps attestation certs. The compliance-proxy CA secures TLS-bumped flows from intercepted apps. They are independent CAs and serve different purposes.

## 2. Enrollment token

```
enrollment_token {
  id, secret_hash,                       // hashed; plaintext returned to admin once
  issued_by_user_id,
  org_id, default_role,                  // landing zone hints for JIT-bound users
  os_constraint,                         // optional: "mac", "linux", "windows"
  device_id_hint,                        // optional: pre-assigned device id
  expires_at,                            // typically 24h after issue
  status: unused | redeemed | expired | revoked
}
```

The admin issues a token from the CP "Devices → Enrollment" surface. The plaintext is returned **once**; subsequent reads see only the metadata. The token is single-use: redemption flips `status` to `redeemed`.

## 3. CSR flow

On first boot, the agent:

1. Generates a fresh ECDSA P-256 keypair locally (`ecdsa.GenerateKey(elliptic.P256(), rand.Reader)`).
2. Constructs the mTLS CSR with `CommonName: "device-<hostname>"` (from `os.Hostname()`). No SAN, no embedded thing-id — Hub is the authoritative assigner of `thing_id`.
3. Generates a parallel Ed25519 keypair and a CSR with `CommonName: "device-<hostname>-attestation"`. This step is fail-open: any crypto error returns empty strings, and the enrollment still proceeds for the mTLS cert alone (the agent simply runs without attestation).
4. POSTs `HubEnrollRequest{ version, csrPem, attestationCsrPem, hostname, os, osVersion, deviceFingerprint }` to Hub `POST /api/internal/things/enroll`, authenticated by one of:
   - `X-Enrollment-Token: <admin-issued-token>` (mtls-only mode), or
   - `Authorization: Bearer <enrollment-JWT>` (SSO mode, E39).
5. Hub:
   - Validates auth (token lookup + status check, or SSO JWT verification).
   - Validates both CSRs (well-formed, correct algorithm).
   - Signs the device cert via the Hub CA (P-256, short validity).
   - If `attestationCsrPem` is non-empty, signs the attestation cert via `agentca.SignAttestationCSR` (Ed25519-only, no `ClientAuth` EKU) and stashes the public-key bytes in `thing_agent.sysinfo` so the compliance-proxy can look them up at verify time. Empty `attestationCsrPem` (pre-E60 agent build) is tolerated.
   - Inserts the `thing` row (type `Agent`) and mints the runtime `deviceToken`.
   - Returns `HubEnrollResponse{ id, deviceToken, certPem, caCertPem, certSerial, certExpiresAt, trustLevel, attestationCertPem }`.
6. Agent writes the artifacts atomically into `certDir` (via `enrollment.Manager.persistHubEnrollment`):
   - `device-key.pem` (P-256 private key) — written first so a crash never leaves a cert without its key.
   - `device.pem` (signed mTLS cert).
   - `gateway-ca.pem` (Hub CA chain).
   - `device-id`, `thing-id`, `device-token`, `trust-level`.
   - `attestation-key.pem` + `attestation.pem` when the attestation cert was issued.
7. Agent establishes mTLS WS to Hub using the new cert.
8. Token (when used) transitions to `redeemed`.

The device-fingerprint field (`deviceFingerprint`) is a hardware-stable 128-bit hash computed by `opsmetrics.ComputeDeviceFingerprint`. Hub uses it to dedupe re-enrollments from the same physical host: a prior agent's `thing_id` is reused when the fingerprint matches; empty fingerprint (e.g., sandboxed runtime that can't read ioreg / machine-id) falls back to "always create a fresh thing_id".

The agent's device-id is opaque externally — admins see it but operate on the human-readable `display_name` for the device.

## 4. Device token auth (HTTP fallback)

When the WS link can't be established (network conditions), the agent falls back to HTTPS calls to Hub. The HTTPS calls use mTLS for the underlying transport and additionally carry `Authorization: Bearer <device-token>` for authenticated endpoints; the `deviceToken` is the runtime credential Hub mints during enrollment and stores at `certDir/device-token`. The mTLS layer pins the agent identity; the bearer token authorises the call. Hub heartbeats, audit drain, shadow fetch, spillstore mint, and `update-check` all run on this combination.

For initial OAuth+PKCE SSO of the **user** sitting at the workstation (so the Nexus desktop UI can show "logged in as Alice"), the flow is separate — see `idp-sso-architecture.md`. SSO produces a one-shot enrollment JWT that the agent uses to call the same `POST /api/internal/things/enroll` endpoint; it is not cached after enrollment completes.

## 5. mTLS handshake

Agent → Hub:

1. Agent presents device cert + intermediate.
2. Hub verifies signature chains up to its own CA.
3. Hub extracts `thing-id` from SAN; looks up the `thing` row.
4. If `thing.status != revoked`, accept the connection; populate request context with `thing_id`.

Hub → agent verification is symmetric: agents trust Hub via the `hub_ca_fingerprint` returned at enrollment. Agents pin Hub's cert chain.

## 6. Renewal

Device certs have a short validity (90 days, `certValidityDays` in `packages/nexus-hub/internal/identity/agentca/ca.go`). Renewal is a separate `/renew-cert` endpoint.

1. Caller (e.g. an operator-driven rotation or future scheduler) invokes `enrollment.Manager.Renew(ctx)` on the agent.
2. Agent generates a fresh ECDSA P-256 keypair + CSR (same hostname-bound CN as the original).
3. Agent calls `POST /api/internal/things/renew-cert` over the existing mTLS connection (current cert + `device-token` authorise the call).
4. Hub validates, signs the new cert via the same CA. The dual-cert attestation key is not refreshed by this endpoint — re-issue requires the enrollment endpoint with `attestationCsrPem`.
5. Agent atomically swaps `device.pem` + `device-key.pem` inside `certDir`.

Today the agent runtime does not schedule renewal automatically — there is no goroutine that watches `not_after` in `packages/agent`. An expired cert means the next mTLS handshake fails and the agent must re-enroll with a fresh token or SSO flow.

A revoked agent's renewal request is rejected — the agent must re-enroll with a new token or SSO flow.

## 7. Revocation

Admin revokes via CP UI:

1. CP API: `POST /agent-devices/:id/unenroll` (`packages/control-plane/internal/fleet/handler/agent/devices.go`).
2. CP flips `thing.status = 'revoked'` for the device row.
3. The next mTLS-authenticated request from that device is refused because `agentmtls` middleware checks `device.Status == "revoked"` (`packages/control-plane/internal/platform/middleware/agentmtls.go`).
4. Agent's HTTPS / WS calls thereafter fail at app-layer auth.
5. Agent enters minimal-functionality mode: no traffic interception (where policy requires Hub), local UI shows "Device revoked, contact admin".
6. Re-enrollment requires a new token.

There is no CRL or OCSP responder today — revocation is enforced purely by the `thing.status` check at request time, plus the short cert validity. A future iteration may add a CRL endpoint for offline verifiers.

## 8. Compliance-proxy cert mint (distinct)

The compliance-proxy has its **own** CA used to mint short-lived leaf certs for TLS-bumping intercepted flows. That CA, those certs, and that signing flow are **independent** of the Hub CA flow above.

Cross-ref `compliance-pipeline-architecture.md` §4 for the cert-mint mechanics there. Don't conflate them — they share the word "CA" and "cert" and absolutely nothing else.

## 9. Failure modes

| Failure | Behavior |
|---|---|
| Token expired | Redeem returns 410; admin re-issues. |
| Token already redeemed | Redeem returns 409. |
| CSR invalid | Redeem returns 400. |
| Hub CA missing | Hub generates on first start; existing agents need re-enroll if the CA file is deleted (no CA = no validation). |
| Cert expired without renewal (e.g., agent offline) | Agent re-enrolls with admin re-issued token. |
| Lost device private key (keystore wipe) | Same as above: re-enroll. |
| Stolen device cert | Admin revokes via UI; agent on the stolen device enters revoked mode. |

## 10. Sources

- `packages/nexus-hub/internal/identity/agentca/ca.go` — Hub CA generation + `SignCSR` (mTLS) + `SignAttestationCSR` (Ed25519, no `ClientAuth` EKU).
- `packages/nexus-hub/internal/identity/enrollment/` — token model + SSO JWT validation + redemption.
- `packages/agent/internal/identity/enrollment/enroll.go` — agent-side CSR generation (dual: P-256 mTLS + Ed25519 attestation), atomic persist into `certDir`, `Manager.Renew`.
- `packages/agent/internal/identity/enrollment/hub_enroll.go` — `HubEnrollRequest` / `HubEnrollResponse` wire types, `POST /api/internal/things/enroll` client (token + JWT variants), `Deregister`.
- `packages/agent/internal/identity/enrollment/sso_flow.go` + `sso_pkce.go` + `sso_server.go` — one-shot SSO enrollment JWT acquisition (browser → PKCE → callback server → JWT → enroll).
- `packages/agent/internal/identity/keystore/` — platform secret store (audit DB key only; cert + key files live in `certDir`).
- `packages/agent/internal/network/relay/` and `packages/shared/transport/http` — mTLS HTTP client plumbing the enrollment + heartbeat paths share.
- `tools/db-migrate/schema.prisma` — `enrollment_token`, `thing`, `thing_agent`.

## 11. Cross-references

- `thing-model.md` — the agent joins the Thing registry on enrollment.
- `agent-forwarder-architecture.md` — what the enrolled agent does.
- `agent-ne-fail-open-architecture.md` — macOS NE provider safety after enrollment.
- `agent-attestation-architecture.md` — dual-cert rationale + how the Ed25519 cert is used at request time.
- `idp-sso-architecture.md` — SSO enrollment JWT path.
- `audit-pipeline-architecture.md` — enrollment events emit admin-audit.
- `compliance-pipeline-architecture.md` — distinct cert-mint flow.
