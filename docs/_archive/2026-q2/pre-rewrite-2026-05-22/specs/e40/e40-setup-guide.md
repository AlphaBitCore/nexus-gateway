# E40 — Setup Guide

## Overview

Nexus Gateway offers three deployment modes: AI Gateway (API proxy), Compliance Proxy (transparent TLS interception), and Agent (desktop endpoint interception). Without an integrated setup guide, customers cannot self-serve deployment — they hit opaque TLS errors, cannot locate CA certificates, and require professional services support for initial configuration.

E40 delivers an interactive Setup section in the Control Plane UI and the backend infrastructure that powers it, enabling IT admins and developers to deploy all three modes without external documentation.

---

## Functional Requirements

### FR-1: Compliance Proxy Onboarding Intercept

**FR-1.1** When `onboarding.enabled = true`, the compliance proxy MUST respond to CONNECT requests targeting monitored AI-provider domains with `407 Proxy Authentication Required` before sending `200 Connection Established`.

**FR-1.2** The `407` response body MUST be valid HTML containing a link to the CP-UI Setup Guide page (`/setup/proxy`).

**FR-1.3** The `407` response MUST include `Content-Type: text/html; charset=utf-8` and a human-readable `Proxy-Authenticate` header value.

**FR-1.4** Onboarding mode MUST be togglable via the Hub Shadow desired state; the proxy picks up the change via the existing WebSocket config-sync path without restart.

**FR-1.5** When `onboarding.enabled = false` (default), proxy behavior MUST be identical to pre-E40 (no behavior change on the CONNECT path).

**FR-1.6** CONNECT requests targeting domains NOT in the monitored domain list MUST NOT be affected by onboarding mode.

### FR-2: Proxy CA Certificate Management Endpoint

**FR-2.1** The compliance proxy management HTTP server MUST expose `GET /management/ca-cert` returning the proxy Sub-CA certificate in PEM format (`Content-Type: application/x-pem-file`).

**FR-2.2** The CA private key MUST NOT be accessible via any HTTP endpoint.

**FR-2.3** The compliance proxy MUST report its management server base URL as `managementURL` in the Thing shadow reported state on startup.

**FR-2.4** If the proxy CA is not yet loaded (startup race), the endpoint MUST return `503 Service Unavailable`.

### FR-3: Control Plane Setup Relay APIs

**FR-3.1** `GET /api/admin/setup/proxy/{thingId}/ca-cert` — reads `managementURL` from the Hub thing shadow, calls `{managementURL}/management/ca-cert`, and streams the PEM response to the caller. MUST return `404` if the thing is not found or has no `managementURL`.

**FR-3.2** `GET /api/admin/setup/proxy/{thingId}/mdm-profile` — fetches the CA cert via FR-3.1, base64-encodes it, renders the mobileconfig template with the cert and a caller-supplied `organization` query parameter, and returns `application/x-apple-aspen-config`.

**FR-3.3** `GET /api/admin/setup/proxy/{thingId}/pac-file?proxyHost=<host>&proxyPort=<port>` — returns a PAC file (`application/x-ns-proxy-autoconfig`) routing all monitored AI-provider domains through the specified proxy. Uses the canonical provider domain list maintained in `interception_domain` rows with `type = 'ai_provider'`.

**FR-3.4** `PATCH /api/admin/setup/proxy/{thingId}/onboarding` — accepts `{"enabled": true|false}` and pushes the value to the proxy's Shadow desired state via the Hub API. Requires `admin:WriteSettings` IAM permission.

**FR-3.5** All four endpoints MUST emit admin audit log events.

**FR-3.6** All four endpoints MUST be protected by IAM; minimum required permission is `admin:ReadSettings` for GET endpoints and `admin:WriteSettings` for PATCH.

### FR-4: Agent Platform CA Auto-Trust

**FR-4.1** After the agent TLS Engine initializes (and generates or loads the Device CA), the agent MUST call `platform.InstallCACert(pem, logger)` to install the CA into the OS trust store.

**FR-4.2** Installation MUST be idempotent: if the CA is already trusted, the function returns without error and without re-installing.

**FR-4.3** Platform implementations:
- macOS: `security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain`
- Windows: `certutil -addstore -f Root`
- Linux (Debian/Ubuntu): copy to `/usr/local/share/ca-certificates/nexus-agent-ca.crt` + `update-ca-certificates`
- Linux (RHEL/CentOS): copy to `/etc/pki/ca-trust/source/anchors/nexus-agent-ca.crt` + `update-ca-trust`

**FR-4.4** If the installation fails (insufficient privileges, OS error), the agent MUST log a warning and continue running — CA installation failure is non-fatal.

**FR-4.5** The agent runs as root (macOS LaunchDaemon, Windows LocalSystem service) so installation should succeed in normal deployment.

### FR-5: mobileconfig Certificate Payload

**FR-5.1** The mobileconfig template (`nexus-agent.mobileconfig.template`) MUST include a `com.apple.security.root` Certificate payload with a `{{PROXY_CA_CERT_BASE64}}` placeholder.

**FR-5.2** The Control Plane `mdm-profile` endpoint (FR-3.2) MUST substitute `{{PROXY_CA_CERT_BASE64}}` with the actual base64-encoded CA cert DER bytes and `{{ORGANIZATION}}` with the caller-supplied organization name before returning.

### FR-6: CP-UI Setup Section

**FR-6.1** The CP-UI sidebar navigation MUST include a "Setup" section with three sub-pages: AI Gateway, Compliance Proxy, Agent.

**FR-6.2 AI Gateway page:**
- Virtual Key selector (list existing VKs; link to create new one)
- Code snippet tabs: Python, Node.js, curl — with the selected VK and the gateway base URL pre-filled
- Copy-to-clipboard on each snippet

**FR-6.3 Compliance Proxy page:**
- Proxy instance selector (lists registered compliance-proxy Things)
- CA Certificate: download button (calls FR-3.1), displays SHA-256 fingerprint
- MDM Profile: download button (calls FR-3.2), organization name input
- PAC File generator: proxy host + port inputs, download button (calls FR-3.3)
- Per-OS CA installation instructions: macOS (manual + MDM), Windows (manual + GPO), Linux
- Onboarding mode toggle (calls FR-3.4) with status indicator
- Verification commands: `openssl s_client` and `curl` one-liners

**FR-6.4 Agent page:**
- Download links per platform: macOS `.pkg`, Windows `.msi`, Linux `.deb`/`.rpm`
- Enrollment token: inline "Generate Token" button (calls existing enrollment token API); displays token with copy button
- Installation instructions with token pre-filled
- Link to Nodes page for verifying enrollment

**FR-6.5** All user-visible strings in the Setup section MUST use `t('setup:...')` i18n keys with translations in all three locale files (en, zh, es).

---

## Non-Functional Requirements

**NFR-1 Security** — The CA private key MUST never traverse the Control Plane, Hub, or any network boundary. Only the public CA cert PEM travels.

**NFR-2 No persistent CA storage** — Control Plane MUST NOT store the CA cert in the PostgreSQL application database. It is fetched on-demand from the live proxy management endpoint.

**NFR-3 Onboarding mode safety** — When `onboarding.enabled = true`, only CONNECTs to monitored AI-provider domains are affected. All other proxy traffic (unlisted domains, passthrough) is unaffected.

**NFR-4 Idempotency** — All setup API calls (CA download, MDM profile, PAC file) MUST be idempotent and safe to retry.

**NFR-5 Latency** — The CA cert relay (FR-3.1) MUST complete within 5 seconds under normal conditions. The management endpoint call is internal and should be sub-100ms.

**NFR-6 Availability** — CA cert endpoint failures (proxy offline, timeout) MUST surface as clear error messages in CP-UI, not silent failures.

**NFR-7 Agent CA install non-blocking** — CA installation failure MUST NOT prevent the agent from starting or intercepting traffic.

---

## User Roles & Personas

| Persona | Role | Primary interaction |
|---------|------|---------------------|
| IT Admin | Deploys compliance proxy, manages CA distribution | Compliance Proxy setup page: CA download, MDM profile, PAC file, onboarding toggle |
| DevOps / SRE | Deploys proxy infrastructure, configures MDM | Same as IT Admin; also uses verification commands |
| Developer | Integrates applications with AI Gateway | AI Gateway setup page: code snippets with VK |
| End User | Installs agent on their machine | Agent setup page (IT Admin prepares, user follows instructions) |

---

## Constraints & Assumptions

- **CA cert on proxy filesystem**: The proxy Sub-CA cert and key are stored on the compliance proxy server filesystem. The management endpoint serves only the public cert; the key is never exposed.
- **managementURL must be reachable from Control Plane**: The proxy management port (default 3041) must be network-accessible from the Control Plane service. In Kubernetes this is a ClusterIP service; in EC2 it is a VPC-internal address.
- **Agent runs as root**: macOS LaunchDaemon and Windows LocalSystem both have the privileges needed for CA installation.
- **Linux detection**: The agent detects the Linux distro family at compile time via build tags or at runtime via `/etc/os-release` to choose the correct CA installation command.
- **mobileconfig template**: The template is used both as a static file (for manual MDM upload) and as a Go template rendered at request time by Control Plane.
- **Enrollment token API pre-existing**: The Agent setup page reuses the existing enrollment token creation API; E40 only adds the UI surface.

---

## Glossary

| Term | Definition |
|------|-----------|
| Sub-CA | The ECDSA P-256 certificate authority used by the compliance proxy to sign per-hostname leaf certificates during TLS interception |
| Device CA | The per-machine ECDSA P-256 CA generated by the agent on first run for local TLS interception |
| mobileconfig | Apple MDM configuration profile format (`.mobileconfig`); supports Certificate payloads for CA trust distribution |
| PAC file | Proxy Auto-Configuration JavaScript file; browsers evaluate it to route traffic to the proxy |
| Onboarding mode | Compliance proxy operational mode in which CONNECT requests return 407 + HTML guide instead of 200 |
| managementURL | The base URL of the compliance proxy's health/management HTTP server, reported in the Thing shadow |

---

## Priority (MoSCoW)

| Requirement | Priority |
|-------------|----------|
| FR-1: Onboarding intercept | Must |
| FR-2: CA management endpoint | Must |
| FR-3: CP setup relay APIs | Must |
| FR-4: Agent CA auto-trust | Must |
| FR-5: mobileconfig Certificate payload | Must |
| FR-6.1: Setup section navigation | Must |
| FR-6.2: AI Gateway page | Must |
| FR-6.3: Compliance Proxy page | Must |
| FR-6.4: Agent page | Must |
| FR-6.5: i18n | Must |
| NFR-1–7 | Must |
