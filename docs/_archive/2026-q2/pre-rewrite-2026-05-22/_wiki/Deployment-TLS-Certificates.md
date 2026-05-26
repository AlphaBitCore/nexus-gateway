# Deployment TLS Certificates

Nexus Gateway uses three separate certificate authorities for different purposes: a Compliance Proxy sub-CA for TLS interception, a Hub-managed Agent CA for enrollment, and Ed25519 signatures for agent auto-update bundles. Public TLS (HTTPS for the admin UI and AI Gateway API) is terminated at the AWS ALB and handled outside the application. This page covers CA generation, KMS integration for key protection, trust distribution to client devices, and CA rotation.

---

## Certificate authorities overview

| CA | Purpose | Algorithm | Managed by |
|---|---|---|---|
| Compliance Proxy CA | Signs dynamic leaf certificates for TLS interception; every intercepted hostname gets a 24-hour leaf cert | ECDSA P-256 | Operator — loaded by the Compliance Proxy at startup |
| Agent CA | Signs agent CSRs during enrollment; device tokens replace the cert for all subsequent auth | ECDSA P-256 | Nexus Hub — auto-generated on first start |
| Update signatures | Verifies agent auto-update bundles before installation | Ed25519 | Build pipeline — separate from CA infrastructure |

Public TLS (ALB certificate, nginx, etc.) is managed entirely outside Nexus — via ACM, Let's Encrypt, or corporate PKI. The application itself runs on plain HTTP behind the ALB.

---

## Compliance Proxy CA

The Compliance Proxy performs TLS bump (MITM) on HTTPS traffic from configured clients. It signs a dynamic leaf certificate for each intercepted hostname. For clients to trust these leaf certs, the Proxy CA must be in their OS trust store.

### Algorithm requirement

The CA key **must be ECDSA P-256**. The proxy reads the CA key at startup and rejects RSA keys with `no EC PRIVATE KEY PEM block found`. This is a hard constraint: a misconfigured CA key prevents the proxy from starting at all.

### Generating the CA

```bash
# Step 1: generate the private key
openssl ecparam -name prime256v1 -genkey -noout -out nexus-proxy-ca.key

# Step 2: generate the self-signed CA certificate
openssl req -new -x509 \
  -key nexus-proxy-ca.key \
  -out nexus-proxy-ca.crt \
  -days 1095 \
  -subj "/C=US/O=YourOrg/OU=IT Security/CN=Nexus Gateway Proxy CA" \
  -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
  -addext "keyUsage=critical,keyCertSign,cRLSign"

# Step 3: restrict key permissions
chmod 600 nexus-proxy-ca.key
```

Deploy to `/var/lib/nexus/proxy-ca/ca.{crt,key}` (owned by the `nexus` user, mode `640` for the key).

### Proxy CA configuration

```yaml
ca:
  certPath: "/var/lib/nexus/proxy-ca/ca.crt"
  keyPath:  "/var/lib/nexus/proxy-ca/ca.key"   # must be EC P-256
```

### KMS integration

For environments where the raw CA key must not reside on disk in cleartext, the proxy supports two KMS modes:

**Wrapped key** (`provider: "command"`): the key file contains ciphertext; an external command decrypts it at startup.

```yaml
ca:
  keyPath: "/var/lib/nexus/proxy-ca/ca.key.enc"
  kms:
    provider: "command"
    command: ["aws", "kms", "decrypt", "--ciphertext-blob", "fileb://{file}",
              "--output", "text", "--query", "Plaintext"]
    timeoutSec: 30
```

**Remote signing** (`signingMode: "remote"`): the private key never enters process memory; certificate signing is delegated to an external command. Provides the strongest key isolation at the cost of higher per-signing latency.

### Leaf certificate cache

Leaf certificates are cached in two layers to minimize signing operations:

- Layer 1: in-memory LRU (per-process, fastest path for hot hostnames)
- Layer 2: Valkey/Redis (shared across instances, AES-256-GCM encrypted at `nexus:proxy:cert:<hostname>`)
- Layer 3: on-demand signing (cache miss — triggers ECDSA P-256 signing)

Redis is optional. If unavailable, the proxy operates with LRU-only caching.

### Certificate pinning passthrough

Applications that use certificate pinning will fail with TLS MITM. The proxy handles this in two ways:

1. Static exemptions: configure per-host passthrough in the compliance proxy YAML.
2. Auto-exemption: after a configurable threshold of consecutive TLS handshake failures for a hostname, the proxy automatically passes that host through without interception.

Passthrough events are tracked via Prometheus metrics: `nexus_compliance_proxy_pinning_passthrough_total`.

---

## Trust distribution to client devices

Every device that routes HTTPS traffic through the Compliance Proxy must have the Proxy CA in its OS trust store. MDM distribution is the standard approach for managed fleets.

### macOS

Manual:
```bash
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain nexus-proxy-ca.crt
```

MDM (Jamf, Kandji, Mosyle): upload as a Certificate payload targeting System keychain, scope to target devices.

### Windows

Manual:
```cmd
certutil -addstore Root nexus-proxy-ca.crt
```

Group Policy: Computer Configuration → Policies → Windows Settings → Security Settings → Public Key Policies → Trusted Root Certification Authorities.

### Linux

Debian/Ubuntu:
```bash
sudo cp nexus-proxy-ca.crt /usr/local/share/ca-certificates/nexus-proxy-ca.crt
sudo update-ca-certificates
```

RHEL/CentOS:
```bash
sudo cp nexus-proxy-ca.crt /etc/pki/ca-trust/source/anchors/nexus-proxy-ca.crt
sudo update-ca-trust
```

### Tools that bypass the OS trust store

| Tool | Configuration |
|---|---|
| Node.js | `NODE_EXTRA_CA_CERTS=/path/to/nexus-proxy-ca.crt` |
| Python (requests) | `REQUESTS_CA_BUNDLE=/path/to/nexus-proxy-ca.crt` |
| Java | `keytool -importcert -alias nexus-proxy-ca -file nexus-proxy-ca.crt -keystore $JAVA_HOME/lib/security/cacerts` |
| Firefox | `policies.json` with `ImportEnterpriseRoots: true` |
| curl | `--cacert nexus-proxy-ca.crt` override or system trust (default) |

---

## Agent CA (Hub-managed)

The Agent CA is a separate ECDSA P-256 CA managed by Nexus Hub. It is used only during the enrollment ceremony: the agent presents a CSR with an enrollment token; Hub validates the token, signs the CSR, and returns the signed certificate plus a device token. All subsequent agent authentication uses the device token, not the certificate.

Hub auto-generates the Agent CA on first start if no cert/key files exist at the configured path (`data/agentca/` by default, overridden by `AGENT_CA_DIR`).

```yaml
agentCA:
  certFile: "data/agentca/agent-ca.crt"
  keyFile:  "data/agentca/agent-ca.key"
  dir:      "data/agentca"
```

Apply the same key-protection rules as the Proxy CA: restrict file permissions (`chmod 600`), use a secrets manager in production.

---

## CA rotation

### Compliance Proxy CA rotation

1. Generate new CA certificate and key.
2. Deploy new CA trust to all endpoints (while keeping old CA trusted during transition).
3. Update compliance proxy config to use the new CA cert/key paths.
4. Restart compliance proxy instances.
5. Clear Valkey/Redis cert cache: `DEL nexus:proxy:cert:*` (forces re-signing with the new CA).
6. After transition period, remove old CA trust from all endpoints.

### Emergency CA compromise

1. Generate new CA immediately.
2. Clear Valkey/Redis cert cache.
3. Restart compliance proxy with the new CA.
4. Push new CA trust via MDM emergency deployment.
5. Remove old CA trust from all endpoints.
6. Review audit logs for anomalous traffic during the compromise window.

---

## Verifying trust

```bash
# Test TLS interception through the proxy
openssl s_client -connect api.openai.com:443 \
  -proxy proxy.example.com:3128 \
  -CAfile nexus-proxy-ca.crt </dev/null 2>&1 | grep "Verify return code"
# Expected: Verify return code: 0 (ok)

# Check CA expiry
openssl x509 -in nexus-proxy-ca.crt -checkend 604800
# Warns if expiring within 7 days
```

---

## Canonical docs

- [`pki-and-certs.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/pki-and-certs.md) — full CA generation, KMS integration, PAC file template, and CA rotation procedures
- [`ec2-single-node.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/ec2-single-node.md) — deployment checklist including certificate path and ownership requirements

**Adjacent wiki pages**: [Deployment-Single-Node-Production](Deployment-Single-Node-Production) · [Deployment-Environment-Variables](Deployment-Environment-Variables) · [Compliance-Proxy-TLS-Interception](Compliance-Proxy-TLS-Interception) · [Agent-Enrollment-Attestation](Agent-Enrollment-Attestation) · [Security-Credential-Storage](Security-Credential-Storage)
