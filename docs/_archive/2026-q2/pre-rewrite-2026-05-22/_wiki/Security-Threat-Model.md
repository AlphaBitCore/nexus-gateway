# Security Threat Model

*Audience: security reviewers evaluating Nexus Gateway for enterprise deployment.*

Nexus Gateway sits in the outbound path of AI traffic between applications and upstream LLM providers. The threat model maps the assets the system protects, the adversaries considered, the trust boundaries enforced at each service interface, and the controls that defend each boundary. This page is a summary; the canonical boundary table lives in the architecture overview.

---

## Assets and adversaries

The assets Nexus Gateway protects fall into three groups:

| Asset | Why it matters |
|---|---|
| Provider API keys (credentials) | Raw provider keys give unlimited access to upstream AI services and accrue cost against the operator's account. |
| Virtual key secrets | VK secrets grant AI Gateway access under the policies bound to that key's project and organization. |
| Traffic content (request/response bodies) | May contain proprietary prompts, PII, business logic, or confidential data being sent to AI providers. |
| Admin session tokens and OAuth credentials | Control over IAM, hooks, routing rules, kill-switch, and credential management. |
| Audit and traffic-event records | Tampered records undermine compliance evidence; deleted records create gaps in the audit chain. |
| Agent device certificates | Used for mTLS enrollment; compromise enables impersonation of endpoint devices. |

The adversaries considered:

- **External attacker** — unauthenticated actor trying to exfiltrate credentials, bypass enforcement, or inject traffic.
- **Compromised application** — application holding a valid virtual key attempting to escape its policy scope.
- **Malicious insider** — privileged admin attempting to disable compliance enforcement or exfiltrate data without a trace.
- **Compromised agent binary** — attacker delivering a tampered agent update to endpoint devices.

## Trust boundaries

Each service-to-service channel has a distinct auth mechanism. The table below is the operational summary; the canonical treatment is in [`docs/developers/architecture/overview.md` §10](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/overview.md).

| Channel | Auth mechanism |
|---|---|
| Admin UI ↔ Control Plane | OAuth 2.0 + PKCE bearer token. No cookie-based login path. |
| External IdP ↔ Control Plane | SAML / OIDC. JIT user provisioning on first successful federation. Nexus is the SP; Nexus Local is the implicit fallback, not a peer IdP. |
| Application ↔ AI Gateway | Bearer Virtual Key (`Authorization: Bearer vk-…`). VKs are project-scoped; the HMAC-SHA256 hash is stored, never the plaintext. |
| Agent ↔ Hub | mTLS. Hub's self-issued ECDSA P-256 CA issues device certificates during enrollment (CSR-based). Post-enrollment auth uses the issued device token. |
| Service ↔ Hub | WebSocket primary, HTTP fallback. Service Things authenticate with bootstrap tokens on first registration, then with their issued certificate. |
| Provider credentials | AES-256-GCM at rest. Decrypted in memory only for the request lifetime. See [Security Credential Storage](Security-Credential-Storage). |
| Compliance Proxy TLS intercept | Dynamically minted leaf certs (ECDSA P-256, 24 h validity) from a local sub-CA deployed to endpoint trust stores. |

## Controls by threat category

### Credential theft

Provider API keys are encrypted at rest with AES-256-GCM in the `Credential` table (`encryptedKey`, `encryptionIv`, `encryptionTag` as separate columns). The master key is sourced from `CREDENTIAL_ENCRYPTION_KEY` — an environment variable, never a committed YAML field. Rotation propagates without a service restart via Hub change-signal and dirty-set tracking. For full detail see [Security Credential Storage](Security-Credential-Storage).

Virtual key secrets are HMAC-SHA256 hashed before storage. The raw secret is never persisted; `vkauth` resolves keys via constant-time lookup by `hashed_secret`. The hashed value is never logged; audit records reference only the `VirtualKeyID`.

### Enforcement bypass

The kill switch and emergency passthrough provide operator-controlled bypass, but the audit trail is non-optional. Every bypassed request still emits a `traffic_event` with `passthrough=true` and a mandatory `bypass_reason`. Emergency passthrough rows carry an `expiresAt` bounded at 8 hours; Hub reconciles every 60 seconds and auto-reverts expired rows. See [Kill Switch](Kill-Switch) and [Emergency Passthrough](Emergency-Passthrough).

IAM gates all admin API endpoints. The `admin:passthrough.emergency-enable` action is separate from `admin:passthrough.write`, so an admin can pre-configure bypass parameters without holding the broader activation lever.

### Audit tampering

The `AdminAuditLog` table is hash-chained (`previousHash`, `integrityHash`, `hashInput`). Deleting or modifying a row breaks the chain, which is detectable. Retention defaults to 365 days; the floor is motivated by SOC 2 and ISO 27001 compliance requirements. SIEM forwarding provides an off-system copy. See [Security Audit Forensics](Security-Audit-Forensics).

### Tampered agent binaries

The agent autoupdater verifies SHA-256 and Ed25519 signature against a caller-supplied public key before applying any update. A bundle whose signature does not verify is rejected and the update attempt is aborted. See [Security Supply Chain](Security-Supply-Chain).

### macOS network safety

The macOS `NETransparentProxyProvider` sits in the host's entire outbound packet path. A misbehaving extension kills the machine's network. Five fail-open invariants govern the implementation to prevent this. See [Security Network Safety](Security-Network-Safety) and [Fail Open Posture](Fail-Open-Posture) for the full invariant set.

### Secrets in committed files

All secrets are environment variables only — no secret field may appear in any YAML committed to the repository. Cross-service shared secrets are tagged `[MUST MATCH]` in `.env.example`. See [Security Secrets Handling](Security-Secrets-Handling).

---

## Canonical docs

- [`docs/developers/architecture/overview.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/overview.md) — §10 trust-boundary table and §11 deployment topology
- [`docs/developers/architecture/cross-cutting/safety/credentials-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/credentials-architecture.md) — Virtual Key + Provider Credential lifecycle
- [`docs/developers/architecture/cross-cutting/safety/emergency-passthrough-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/emergency-passthrough-architecture.md) — emergency passthrough tiers and audit invariants

**Adjacent wiki pages**: [Security Reporting A Vulnerability](Security-Reporting-A-Vulnerability) · [Security Credential Storage](Security-Credential-Storage) · [Security Network Safety](Security-Network-Safety) · [Trust Boundaries](Trust-Boundaries) · [Fail Open Posture](Fail-Open-Posture)
