# Agent identity & enrollment

Before the agent can do anything useful it must become a known Thing: get a
device certificate signed by Nexus Hub, receive a device-id and a bearer token,
and (optionally) an attestation key. This document covers that lifecycle — the
two enrollment paths, the credentials that result, the attestation key issued
alongside them, and the hardened browser launch the SSO path relies on.

## The enrollment Manager

`enrollment.Manager` owns the on-disk identity artifacts in the agent's cert
directory: the mTLS device certificate (`device.pem`), its key
(`device-key.pem`), the gateway CA (`gateway-ca.pem`), the device-id, and the
bearer token. `IsEnrolled` is the boot gate — it reports enrolled only when the
cert, key, and token are all present; until then the daemon serves the status
IPC so the Dashboard can drive sign-in, and does not start interception. The
Manager also exposes `ThingID`, `CertPaths`, `TrustLevel`, and the SSO email.

Three collaborators are injected as options: a `HubEnroller` (the Hub HTTP call
that signs a CSR into a device cert), a `CertRenewer` (cert rotation), and a
`TokenRenewer` (device-token rotation — see
[Device-token renewal](#device-token-renewal)).

## Two enrollment paths

**Token enrollment** (`Enroll`) is the headless / mTLS path: the agent generates
a P-256 ECDSA key, builds a CSR, and exchanges an enrollment token with Hub for
the signed device certificate. This is the path the `ENROLL_TOKEN` IPC command
drives.

**SSO enrollment** drives a PKCE OAuth flow against the Control Plane:

1. The agent generates a PKCE verifier/challenge and starts a short-lived local
   HTTP server on loopback to catch the OAuth redirect.
2. It opens the operator's Control Plane authorize URL in the user's browser
   (through the hardened opener below).
3. The browser completes SSO; the redirect hits the local callback server, which
   captures the authorization code (matched against the PKCE state).
4. The agent exchanges the code for an enrollment JWT and presents it to Hub,
   which signs the device certificate.

The SSO path is what the Dashboard's sign-in flow uses; it is split across
`sso_flow.go` (orchestration), `sso_pkce.go` (the PKCE primitives), and
`sso_server.go` (the loopback callback server).

## Key material

Enrollment provisions two independent keypairs:

- **P-256 ECDSA** — the mTLS device identity. Its certificate (`device.pem`) is
  what every authenticated Hub call presents.
- **Ed25519** — the traffic-attestation key, generated in the same `Enroll` step.
  This is **fail-open**: a failure to generate or register the attestation key
  does not block enrollment — the device still comes up, just without
  attestation.

The bearer token returned by enrollment is stored in the cert directory and read
back by `auth.LoadDeviceToken`; it rides as `Authorization: Bearer` on Hub calls.
`auth.ClearEnrollment` wipes these artifacts — the sign-out / unenroll path.
SSO-flow refresh tokens and session ids are kept separately in `secretstore` (see
[agent-keystore-architecture.md](agent-keystore-architecture.md)).

## Device-token renewal

The device token has a bounded lifetime: Hub stamps an expiry on it at
enrollment, which the agent persists next to the token (`device-token-expires`,
read by `auth.LoadDeviceTokenExpiry`). A background scheduler checks
`Manager.DeviceTokenNeedsRenewal` once an hour and, when the token enters its
renewal window (`DeviceTokenRenewWindow` before expiry — comfortably ahead of the
lapse), calls `Manager.RenewDeviceToken`. That asks Hub to rotate the token
(authenticated by the current still-valid token), then atomically replaces the
on-disk `device-token` + `device-token-expires` files — the token file first, so
a crash never leaves a longer-lived token than Hub granted. Hub overwrites the
stored hash as part of the same call, so the previous token is invalidated the
moment Hub responds and a copy of it is useless thereafter. A missing or
unparseable expiry is treated as "renew now", so a legacy enrollment self-heals
onto a bounded token rather than running unbounded. The rotation goes through the
HTTP Hub client, which reads the token fresh from disk on every call, so all
subsequent authenticated calls transparently pick up the rotated token.

## Attestation

The Ed25519 key issued at enrollment backs `attestation.Signer`. The signer lazy-
loads the key from the platform keystore on its first `Sign`, caches it in
memory, and produces the `X-Nexus-Attestation` header value stamped on every
outbound CONNECT to the compliance proxy. `InjectInto` is the request hook the
agent's tlsbump `UpstreamTransport` calls per request.

**What attestation proves (v1).** The header proves that the signing agent holds
the Ed25519 private key that was registered with Hub at enrollment time. It is
**enrollment-key attestation** — not hardware-rooted device attestation. The key
is a software Ed25519 key held in the platform keystore (macOS
Keychain via Security.framework, Windows DPAPI, or on Linux a `0600` file under
`~/.nexus/secrets`), written there by enrollment and read back by the signer; it
is not a plaintext PEM on disk. There is no TPM or Secure Enclave binding
in v1, so on Linux the key is still only filesystem-ACL protected. The
header therefore proves "this request came from a process that enrolled and
received this key", not "this request came from a specific managed device". A
future v2 can bind the key to a hardware security module and include per-request
body binding.

**v1 body binding.** For bounded request bodies the injector hashes the full
body (`sha256(body)`) and includes it in the signed pre-image. For streaming or
oversized bodies the injector falls back to `sha256("")` (the empty-body hash),
because the full body cannot be consumed without breaking the downstream send.
The Compliance Proxy verifies the signature but does not enforce the hash field
in the default v1 mode — the hash is present in the audit trail for consistency
but is not a strict guard.

Two additional properties:

- **Live toggle.** The signer is constructed with an `enabledLookup` callback
  read on every `Sign`, sourced from the applied-config device defaults
  (`attestationEnabled`), so an admin flipping attestation takes effect on the
  next request without a restart.
- **Fail-open.** Every `Sign` error path causes the caller to omit the header;
  the request still flows to the compliance proxy through the normal bumped path.
  Attestation never blocks traffic.

The wire format the signer emits is defined in
`packages/shared/transport/tlsbump/attestation.go`, shared with the
compliance-proxy verifier that reads it.

## Hardened browser launch

The SSO path must open a URL in the user's browser, and the Dashboard's
`OPEN_BROWSER` IPC command does the same. Both go through `openbrowser.Opener`,
which invokes the platform open command with a **fixed argv** (e.g. `open <url>`
on macOS) — there is **no shell-interpreted command execution**: the URL is a
single argv element, never substituted into a shell string, so a compromised
WebView renderer cannot inject extra commands, metacharacters, or arguments.
`Open` also enforces a fixed policy before that command runs: the URL must parse
as absolute, its scheme must be `https`, and its host must be on an
operator-configured allowlist (`SetAllowedHosts`, populated from the configured
Control Plane URL). Anything else is rejected before `dispatch` runs the platform
open command. (The `OPEN_BROWSER` IPC entry point is covered in
[agent-tray-ipc-architecture.md](agent-tray-ipc-architecture.md).)

## References

- `packages/agent/internal/identity/enrollment/enroll.go` — the `Manager`, on-disk artifacts, `Enroll`, and the parallel Ed25519 generation
- `packages/agent/internal/identity/enrollment/hub_enroll.go` — the Hub CSR-signing call
- `packages/agent/internal/identity/enrollment/sso_flow.go` — PKCE SSO orchestration
- `packages/agent/internal/identity/enrollment/sso_pkce.go` — PKCE verifier/challenge
- `packages/agent/internal/identity/enrollment/sso_server.go` — the loopback OAuth callback server
- `packages/agent/internal/identity/auth/device_token.go` — `LoadDeviceToken` / `LoadDeviceTokenExpiry` / `ClearEnrollment`
- `packages/agent/internal/sync/hub/client.go` — `RenewDeviceToken` (the rotation HTTP call)
- `packages/agent/internal/identity/attestation/signer.go` — the Ed25519 attestation signer (`Sign` / `InjectInto`)
- `packages/agent/internal/host/openbrowser/openbrowser.go` — the https + host-allowlist browser opener
- `packages/shared/transport/tlsbump/attestation.go` — the attestation header wire format
