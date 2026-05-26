# PKI and Certificate Management

## Overview

The Nexus Gateway compliance proxy performs TLS interception (bump/MITM) to inspect AI-bound HTTPS traffic. It dynamically generates leaf certificates signed by an enterprise sub-CA. For clients to trust these certificates, the sub-CA must be deployed to every endpoint's trust store.

---

## PKI Trust Chain

```
Enterprise Root CA (optional, if using intermediate)
     |
     v
Nexus Proxy Sub-CA (ECDSA P-256)
  - Loaded by compliance proxy at startup
  - Signs leaf certs on the fly (24h validity)
     |
     v
Dynamic Leaf Certificate (per hostname)
  - Generated per SNI hostname
  - Cached in LRU (in-memory) and Redis
  - ECDSA P-256 key per leaf
```

---

## CA Generation

### Generate the Private Key

```bash
openssl ecparam -name prime256v1 -genkey -noout -out nexus-proxy-ca.key
```

### Generate the Self-Signed CA Certificate

```bash
openssl req -new -x509 \
  -key nexus-proxy-ca.key \
  -out nexus-proxy-ca.crt \
  -days 1095 \
  -subj "/C=US/O=Your Corp/OU=IT Security/CN=Nexus Gateway Proxy CA" \
  -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -addext "subjectKeyIdentifier=hash"
```

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| Algorithm | ECDSA P-256 | Fast signing for per-host cert generation |
| Validity | 3 years (1095 days) | Balance rotation frequency vs. operational overhead |
| Path length | 0 | Signs only end-entity certs |
| Key usage | keyCertSign, cRLSign | Minimum required for CA |

### Protect the Private Key

```bash
chmod 600 nexus-proxy-ca.key
```

- **Production**: Use a secrets manager (Vault, AWS Secrets Manager) or mount as a Kubernetes secret.
- **Development**: Local file with restricted permissions.
- **Never commit the private key to version control.**

---

## Compliance Proxy CA Configuration

```yaml
ca:
  certPath: "/etc/compliance-proxy/ca.crt"
  keyPath: "/etc/compliance-proxy/ca.key"
  kms:
    provider: ""           # "noop" (default), or "command" for KMS unwrap
    command: []            # External KMS decrypt command
    timeoutSec: 30
    signingMode: "local"   # "local" (key in memory) or "remote" (key never leaves KMS)
    signCommand: []        # External signing command for remote mode
```

### KMS Integration

The compliance proxy supports wrapping the CA private key with an external KMS so the raw key never resides on disk in cleartext:

- **`provider: "noop"`** (default): Key file contains raw PEM.
- **`provider: "command"`**: Key file contains ciphertext. The configured command is called at startup with `{file}` replaced by the ciphertext path. Stdout must be the plaintext PEM.

Example with AWS KMS:
```yaml
ca:
  keyPath: "/etc/compliance-proxy/ca.key.enc"
  kms:
    provider: "command"
    command: ["aws", "kms", "decrypt", "--ciphertext-blob", "fileb://{file}", "--output", "text", "--query", "Plaintext"]
    timeoutSec: 30
```

### Remote Signing Mode

When `signingMode: "remote"`, the private key never enters process memory. Certificate signing is delegated to an external command. This provides the strongest key protection at the cost of higher signing latency.

---

## Certificate Cache

Leaf certificates are cached in two layers to minimize signing operations:

### Layer 1: In-Memory LRU Cache

- Per-process in-memory cache
- Fastest path for hot hostnames
- Metric: `cert_cache.hits_total{layer="l1"}`

### Layer 2: Redis Cache

- Shared across all compliance proxy instances
- Private keys are AES-256-GCM encrypted before storage
- Key prefix: `nexus:proxy:cert:<hostname>`
- Metric: `cert_cache.hits_total{layer="l2"}`

### Layer 3: On-Demand Signing

- Cache miss triggers certificate signing
- Metric: `cert_cache.misses_total`
- Signing duration: `cert_sign_ms`

> Metric names use the canonical dotted opsmetrics convention; Prometheus
> translates `.` to `_` on the wire, so scrapes show
> `cert_cache_hits_total{layer="l1"}` (see
> `packages/compliance-proxy/internal/metrics/prometheus.go:11`).

Redis is optional. If unavailable, the proxy operates with LRU-only caching (higher CPU usage for cold hostnames).

---

## Certificate Pinning and Passthrough

### Configured Exemptions

Some applications use certificate pinning and will fail if TLS is intercepted. Configure static exemptions:

```yaml
audit:
  pinning:
    exemptions:
      - host: "pinned-app.example.com"
        reason: "Known certificate-pinned application"
```

### Automatic Exemption

The proxy can automatically exempt hosts after repeated TLS handshake failures:

```yaml
audit:
  pinning:
    autoExempt:
      enabled: true
      failureThreshold: 3        # Consecutive failures before exemption
      windowSeconds: 3600         # Time window for counting failures (1h)
      exemptionDurationSeconds: 86400  # Duration of exemption (24h)
```

### Passthrough Metrics

Monitor pinning passthrough events:

| Metric | Label Values |
|--------|-------------|
| `pinning.passthrough_total` | `BUMP_FAILED_PASSTHROUGH` (handshake failed, fell back to passthrough) |
| | `BUMP_EXEMPT_PINNED` (auto-exempted after repeated failures) |
| | `BUMP_EXEMPT_CONFIGURED` (admin-configured exemption) |

---

## Agent CA (Hub-Managed)

The Agent CA is a separate ECDSA P-256 certificate authority managed by
Nexus Hub. It signs agent CSRs during enrollment to provide proof-of-possession.
After enrollment, agents authenticate with device tokens (not ongoing mTLS).

### Lifecycle

1. **Auto-generation**: Hub generates a self-signed CA on first start if no cert/key
   files exist at the configured path (default `data/agentca/`).
2. **Enrollment**: Agent presents an enrollment token and a CSR to
   `POST /api/internal/things/enroll`. Hub validates the token, signs the CSR,
   issues a device token (32-byte random hex, SHA-256 hashed in DB), registers
   the thing, and returns the signed certificate + device token.
3. **Post-enrollment auth**: Agent uses the device token as a Bearer token for all
   subsequent HTTP and WebSocket connections to Hub.

### Configuration

```yaml
agentCA:
  certFile: "data/agentca/agent-ca.crt"
  keyFile:  "data/agentca/agent-ca.key"
  dir:      "data/agentca"    # auto-generate CA here if cert/key don't exist
```

Environment variable override: `AGENT_CA_DIR`.

### Key Protection

Same recommendations as the Compliance Proxy CA: restrict file permissions
(`chmod 600`), use a secrets manager in production, never commit keys.

---

## Compliance Proxy CA Trust Deployment

### macOS

#### Manual

```bash
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain \
  nexus-proxy-ca.crt
```

#### MDM (Jamf, Kandji, Mosyle)

1. Upload `nexus-proxy-ca.crt` as a Certificate payload.
2. Install in System keychain.
3. Scope to target devices.

### Windows

#### Manual

```cmd
certutil -addstore Root nexus-proxy-ca.crt
```

#### Group Policy

1. Computer Configuration > Policies > Windows Settings > Security Settings > Public Key Policies > Trusted Root Certification Authorities.
2. Import `nexus-proxy-ca.crt`.
3. Link GPO to target OU.

### Linux

#### Debian/Ubuntu

```bash
sudo cp nexus-proxy-ca.crt /usr/local/share/ca-certificates/nexus-proxy-ca.crt
sudo update-ca-certificates
```

#### RHEL/CentOS

```bash
sudo cp nexus-proxy-ca.crt /etc/pki/ca-trust/source/anchors/nexus-proxy-ca.crt
sudo update-ca-trust
```

---

## Per-Tool Trust

Some tools do not use the OS trust store:

| Tool | Configuration |
|------|--------------|
| Firefox | `policies.json` with `ImportEnterpriseRoots: true` or manual import in certificate manager |
| Node.js | `NODE_EXTRA_CA_CERTS=/path/to/nexus-proxy-ca.crt` |
| Python (requests) | `REQUESTS_CA_BUNDLE=/path/to/nexus-proxy-ca.crt` |
| Go | `SSL_CERT_FILE=/path/to/combined-ca.crt` (or relies on system trust) |
| Java | `keytool -importcert -alias nexus-proxy-ca -file nexus-proxy-ca.crt -keystore $JAVA_HOME/lib/security/cacerts` |
| curl | Uses system CA by default; override with `--cacert` |

---

## PAC File Configuration

A PAC (Proxy Auto-Configuration) file routes AI provider traffic through the compliance proxy while sending other traffic direct.

### Basic Template

```javascript
function FindProxyForURL(url, host) {
    if (dnsDomainIs(host, "api.openai.com") ||
        dnsDomainIs(host, "api.anthropic.com") ||
        dnsDomainIs(host, "generativelanguage.googleapis.com") ||
        dnsDomainIs(host, "api.deepseek.com") ||
        dnsDomainIs(host, "api.x.ai") ||
        dnsDomainIs(host, "api.moonshot.cn") ||
        dnsDomainIs(host, "open.bigmodel.cn") ||
        dnsDomainIs(host, "api.minimax.chat") ||
        dnsDomainIs(host, "copilot-proxy.githubusercontent.com") ||
        false) {
        return "PROXY proxy.example.com:3128; DIRECT";
    }
    return "DIRECT";
}
```

### Failover Patterns

- **Fail-open** (general workforce): `PROXY proxy1:3128; PROXY proxy2:3128; DIRECT`
- **Fail-closed** (high-sensitivity): `PROXY proxy1:3128; PROXY proxy2:3128` (no DIRECT fallback)

### Distribution

- **WPAD**: DHCP option 252 or DNS `wpad.<domain>` record
- **MDM**: macOS configuration profile, Windows GPO, Linux NetworkManager
- **Manual**: Browser/OS proxy settings with PAC URL

---

## CA Rotation

1. Generate new CA certificate and key.
2. Deploy new CA trust to all endpoints (while keeping old CA trusted).
3. Update compliance proxy config to use new CA cert/key paths.
4. Restart compliance proxy instances (graceful, rolling).
5. Clear Redis cert cache (`DEL nexus:proxy:cert:*`) to force re-signing with new CA.
6. After transition period, remove old CA trust from all endpoints.

### Emergency CA Compromise

1. Generate new CA immediately.
2. Clear Redis cert cache.
3. Restart compliance proxy with new CA.
4. Push new CA trust via MDM emergency deployment.
5. Remove old CA trust from all endpoints.
6. Review audit logs for anomalous traffic during compromise window.

---

## Verifying Trust

```bash
# Test through the proxy
openssl s_client -connect api.openai.com:443 \
  -proxy proxy.example.com:3128 \
  -CAfile nexus-proxy-ca.crt \
  </dev/null 2>&1 | head -20
# Look for: "Verify return code: 0 (ok)"

# Verify CA details
openssl x509 -in nexus-proxy-ca.crt -text -noout | grep -A2 "Validity"
openssl x509 -in nexus-proxy-ca.crt -checkend 604800  # Warn if expiring within 7 days
```
