# Air-Gapped Deployment Runbook

Nexus Gateway can be deployed in environments where the server-side services have no mandatory runtime outbound internet access. "Air-gapped" in this document means the four Go services (Hub, Control Plane, AI Gateway, Compliance Proxy) and their three backing services (PostgreSQL, Valkey, NATS JetStream) run entirely on internal infrastructure. No call originates from Nexus itself to the public internet during normal operation.

**Important limitation on provider traffic.** The AI Gateway forwards LLM calls to configured upstream endpoints. In a standard deployment those endpoints are `api.openai.com`, `api.anthropic.com`, etc. — which are public. In a true air-gapped environment you must replace those with one of two options:

- **Option A — Internal provider mirror:** An internal reverse proxy or API gateway that forwards to the public provider endpoints on behalf of the air-gapped network. The Nexus AI Gateway connects to the mirror; the mirror handles the egress.
- **Option B — Self-hosted LLM server:** A self-hosted model server (Ollama, vLLM, llama.cpp, or any OpenAI-compatible server) running entirely on-premises. Configure Nexus to point at the internal endpoint; no provider traffic leaves the network.

Until you have chosen and provisioned one of these two options, no AI requests will complete. Document that decision before starting this runbook.

This runbook covers: Hub, Control Plane, AI Gateway, Compliance Proxy, Control Plane UI, and the Desktop Agent. It adapts the EC2 single-node recipe (`docs/operators/ops/ec2-single-node.md`) and PKI guidance (`docs/operators/ops/pki-and-certs.md`) for environments where all package downloads must be pre-staged.

---

## Architecture compatibility

Nexus Gateway has no mandatory runtime outbound dependencies of its own. All four services communicate over the internal network exclusively:

- **PostgreSQL** — on-premises, any standard deployment.
- **Valkey** (Redis-wire-compatible, BSD-3-Clause) — on-premises. See `docs/operators/ops/redis-setup.md`.
- **NATS JetStream** — on-premises. Single binary, no package dependencies.
- **Inter-service communication** — Hub ↔ CP ↔ AI Gateway ↔ Compliance Proxy over loopback or internal LAN; no external calls.
- **Config flow** — Admin UI → CP admin API → Hub HTTP API → Hub shadow → WebSocket push → service callbacks. No Redis pub/sub, no external bus.

Optional outbound endpoints that an air-gapped deployment must explicitly address:

| Endpoint | Used for | Air-gapped disposition |
|---|---|---|
| LLM provider APIs (`api.openai.com`, etc.) | AI traffic forwarding | Replace with internal mirror (Option A) or self-hosted LLM (Option B) |
| macOS Agent auto-update check (`GET /api/internal/things/update-check`) | Agent binary self-update | Hub endpoint is on-premises; no internet access needed. The auto-updater is disabled by default (no Ed25519 key provisioned) — see Step 6 |
| Apple notarization check | macOS agent `.pkg` install-time trust check | One-time check at install time only; no runtime dependency. Handled at install time on the endpoint; not a Nexus server concern |
| OS package manager (`dnf`, `apt`, `yum`) | Go binary and infrastructure package install | Handled at build time on a connected build host; transferred to air-gapped host via approved channel |

**Architecture references:** `docs/developers/architecture/overview.md` (system topology), `docs/developers/architecture/cross-cutting/foundation/mq-architecture.md` (NATS internals), `docs/developers/architecture/cross-cutting/storage/spillstore-architecture.md` (spillstore modes).

---

## Pre-flight checklist

Complete all items before starting the deployment steps.

1. **Internal CA provisioned.** You need an internal ECDSA P-256 CA for the Compliance Proxy. If your organization already has an internal PKI, generate a sub-CA signed by your root. If not, generate a self-signed CA — commands are in Step 8.
2. **Build host ready.** A connected Linux host (or CI pipeline) with Go 1.25+, Node.js 20+, and Docker accessible. This host builds all artifacts; the air-gapped target never needs internet access.
3. **Artifact transfer channel approved.** A secure channel for copying binaries, Docker images, and migration tarballs to the air-gapped target: USB drive, secure file share, bastion-mediated SCP, or similar.
4. **Provider mode chosen.** Either an internal LLM mirror endpoint or a self-hosted model server (Ollama, vLLM, or llama.cpp) is provisioned and reachable from the air-gapped host before you start Step 5.
5. **Target host provisioned.** Linux host with at least 4 CPU cores, 8 GB RAM, 100 GB disk. OS packages (`openssl`, `postgresql16-server`, `nginx`) installed from an internal OS mirror or pre-staged RPM/DEB set before this runbook begins — those installs are OS-level air-gapped tooling outside Nexus's scope.
6. **Secrets pre-generated.** Generate the four shared secrets on the build host or a trusted workstation. Keep them for Step 3:

   ```bash
   openssl rand -hex 32   # → INTERNAL_SERVICE_TOKEN
   openssl rand -hex 32   # → ADMIN_KEY_HMAC_SECRET
   openssl rand -hex 32   # → CREDENTIAL_ENCRYPTION_KEY (64 hex chars = 32 bytes AES-256)
   openssl rand -hex 32   # → COMPLIANCE_PROXY_API_TOKEN
   ```

---

## Step 1 — Pre-bundle dependencies on the connected build host

All downloads happen here. Nothing is fetched on the air-gapped target.

### 1a. Build Go binaries

From the repository root on the connected build host:

```bash
VER="$(git describe --tags --match 'prod-*' --abbrev=0 2>/dev/null || echo dev)@$(git rev-parse --short HEAD)"
LDFLAGS="-X main.buildVersion=${VER}"

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$LDFLAGS" \
    -o dist/nexus-hub ./packages/nexus-hub/cmd/nexus-hub/
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$LDFLAGS" \
    -o dist/nexus-control-plane ./packages/control-plane/cmd/control-plane/
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$LDFLAGS" \
    -o dist/nexus-ai-gateway ./packages/ai-gateway/cmd/ai-gateway/
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$LDFLAGS" \
    -o dist/nexus-compliance-proxy ./packages/compliance-proxy/cmd/compliance-proxy/
```

All Go services compile to statically linked binaries (`CGO_ENABLED=0`). No shared libraries are needed on the target host.

### 1b. Vendor Go modules (for reproducible rebuilds, optional)

If you anticipate rebuilding from source on an internal build host without internet access:

```bash
go work vendor   # writes all module sources to vendor/ in each workspace root
tar czf dist/go-vendor.tar.gz vendor/
```

### 1c. Build the Control Plane UI

```bash
cd packages/control-plane-ui
npm ci
npm run build   # outputs to dist/
cd ../..
tar czf dist/nexus-ui.tar.gz -C packages/control-plane-ui/dist .
```

### 1d. Download NATS server binary

NATS is not in standard Linux package repositories. Download and include in the transfer bundle:

```bash
# Adjust version to match your target release
curl -LO https://github.com/nats-io/nats-server/releases/download/v2.10.24/nats-server-v2.10.24-linux-amd64.zip
unzip nats-server-v2.10.24-linux-amd64.zip -d dist/
chmod +x dist/nats-server-v2.10.24-linux-amd64/nats-server
```

### 1e. Bundle database migrations

The migration tarball lets you run `prisma migrate deploy` on the air-gapped target from a Node.js process that has the Prisma CLI. Prisma CLI must also be transferred.

```bash
tar czf dist/db-migrate.tar.gz tools/db-migrate/
```

Alternatively, if Node.js is not available on the air-gapped host, run migrations via SSH tunnel from the build host (see Step 4).

### 1f. Package for transfer

```bash
tar czf nexus-airgap-bundle.tar.gz dist/
# Transfer nexus-airgap-bundle.tar.gz to the air-gapped host via approved channel
```

---

## Step 2 — Bring up infrastructure inside the air gap

On the air-gapped target host. These steps adapt `docs/operators/ops/ec2-single-node.md` §Prerequisites.

### 2a. OS user and directories

```bash
sudo useradd -r -s /sbin/nologin nexus
sudo mkdir -p /etc/nexus /var/log/nexus \
    /var/lib/nexus/{authkeys,agent-ca,proxy-ca}
sudo chown -R nexus:nexus /var/log/nexus /var/lib/nexus
sudo chmod 750 /var/lib/nexus/authkeys /var/lib/nexus/proxy-ca
```

### 2b. Install NATS server

```bash
sudo mv /tmp/nexus-bundle/nats-server /usr/local/bin/
sudo chmod +x /usr/local/bin/nats-server
```

### 2c. PostgreSQL — create user and database

```bash
sudo postgresql-setup --initdb
sudo systemctl enable --now postgresql

sudo -u postgres psql -c "CREATE USER nexus WITH PASSWORD 'CHANGE_ME';"
sudo -u postgres psql -c "CREATE DATABASE nexus_gateway OWNER nexus;"
# Edit /var/lib/pgsql/data/pg_hba.conf:
#   host   nexus_gateway  nexus  127.0.0.1/32  scram-sha-256
sudo systemctl restart postgresql
```

### 2d. Valkey

Valkey 8 (`valkey/valkey-bundle`) is the recommended cache server. In an air-gapped environment, pre-pull the Docker image on the build host and export it:

```bash
# On connected build host:
docker pull valkey/valkey-bundle:8-trixie
docker save valkey/valkey-bundle:8-trixie | gzip > dist/valkey-bundle-8-trixie.tar.gz

# On air-gapped target:
docker load < /tmp/nexus-bundle/valkey-bundle-8-trixie.tar.gz
docker run -d \
    --name nexus-valkey \
    -p 6379:6379 \
    -v /var/lib/nexus/valkey:/data \
    --restart unless-stopped \
    valkey/valkey-bundle:8-trixie
```

If Docker is not available, Valkey can also be compiled from source or installed from an internal package mirror. The Nexus `REDIS_MODE`, `REDIS_ADDRS` environment variables point each service at the correct host and port. See `docs/operators/ops/redis-setup.md` for the full configuration schema.

Verify Valkey health:

```bash
docker exec nexus-valkey valkey-cli ping   # PONG
```

### 2e. NATS JetStream

Create `/etc/nats/nats.conf`:

```
port: 4222
http_port: 8222
jetstream {
    store_dir: /var/lib/nexus/nats
    max_memory_store: 1GB
    max_file_store: 10GB
}
```

```bash
sudo mkdir -p /var/lib/nexus/nats
sudo chown nexus:nexus /var/lib/nexus/nats
# Create /etc/systemd/system/nats.service — see systemd section below
sudo systemctl enable --now nats
```

Verify NATS health:

```bash
curl -s http://127.0.0.1:8222/healthz   # {"status":"ok"}
```

---

## Step 3 — Deploy Nexus services

Install binaries and write configuration files. Start order must be followed: Hub before all other services.

### 3a. Install binaries

```bash
sudo mv /tmp/nexus-bundle/nexus-{hub,control-plane,ai-gateway,compliance-proxy} /usr/local/bin/
sudo chmod +x /usr/local/bin/nexus-*
```

### 3b. Write shared secrets to the environment file

Create `/etc/nexus/nexus-gateway.env` (mode `0640`, owner `root:nexus`):

```bash
# /etc/nexus/nexus-gateway.env
# [MUST MATCH all 4 services]
INTERNAL_SERVICE_TOKEN=<generated-in-preflight>

# [MUST MATCH control-plane <-> ai-gateway]
ADMIN_KEY_HMAC_SECRET=<generated-in-preflight>

# [MUST MATCH control-plane <-> ai-gateway]
CREDENTIAL_ENCRYPTION_KEY=<generated-in-preflight>

# [MUST MATCH control-plane <-> compliance-proxy]
COMPLIANCE_PROXY_API_TOKEN=<generated-in-preflight>

DATABASE_URL=postgresql://nexus:CHANGE_ME@127.0.0.1:5432/nexus_gateway?sslmode=disable
REDIS_MODE=standalone
REDIS_ADDRS=127.0.0.1:6379
NATS_URL=nats://127.0.0.1:4222
NEXUS_HUB_URL=http://127.0.0.1:3060
AI_GATEWAY_URL=http://127.0.0.1:3050
COMPLIANCE_PROXY_URL=http://127.0.0.1:3040
COMPLIANCE_PROXY_RUNTIME_URL=http://127.0.0.1:3040
AUTH_SERVER_URL=http://127.0.0.1:3001
AUTH_SERVER_JWKS_URL=http://127.0.0.1:3001/.well-known/jwks.json
AUTH_SERVER_ISSUER=https://nexus.internal   # Use your internal hostname
NODE_ENV=production
CONTROL_PLANE_CRYPTO_PRODUCTION=true
```

See `.env.example` at repo root for the full variable catalog and per-service annotations.

### 3c. Write per-service YAML config files

Minimal `nexus-hub.yaml`:

```yaml
server:
  port: 3060
authServer:
  issuer: "https://nexus.internal"
agentCA:
  dir: "/var/lib/nexus/agent-ca"
```

Minimal `control-plane.yaml`:

```yaml
server:
  port: 3001
authServer:
  issuer: "https://nexus.internal"
  keystoreDir: "/var/lib/nexus/authkeys"
auth:
  allowDevAuth: false
crypto:
  production: true
bff:
  complianceProxyUrl: "http://127.0.0.1:3040"
  aiGatewayUrl:       "http://127.0.0.1:3050"
  nexusHubUrl:        "http://127.0.0.1:3060"
```

Minimal `ai-gateway.yaml`:

```yaml
server:
  port: 3050
registry:
  controlPlaneUrl: "http://127.0.0.1:3060"
```

Minimal `compliance-proxy.yaml`:

```yaml
listener:
  address: ":3128"
ca:
  certPath: "/var/lib/nexus/proxy-ca/ca.crt"
  keyPath:  "/var/lib/nexus/proxy-ca/ca.key"
registry:
  controlPlaneUrl: "http://127.0.0.1:3060"
runtimeApi:
  listenAddress: "127.0.0.1:3040"
```

### 3d. Create systemd units

Create the following files under `/etc/systemd/system/`. All four service units share this template structure:

```ini
# /etc/systemd/system/nexus-hub.service
[Unit]
Description=Nexus Hub
After=network.target postgresql.service nats.service

[Service]
Type=simple
User=nexus
Group=nexus
EnvironmentFile=/etc/nexus/nexus-gateway.env
ExecStart=/usr/local/bin/nexus-hub -config /etc/nexus/nexus-hub.yaml
Restart=on-failure
RestartSec=5s
LimitNOFILE=1048576
StandardOutput=append:/var/log/nexus/nexus-hub.log
StandardError=append:/var/log/nexus/nexus-hub.log

[Install]
WantedBy=multi-user.target
```

Create parallel units for `nexus-control-plane`, `nexus-ai-gateway`, and `nexus-compliance-proxy`, adjusting the `After=` dependency to include `nexus-hub.service` for all three non-Hub services.

```bash
sudo systemctl daemon-reload
sudo systemctl enable nats nexus-hub nexus-control-plane \
    nexus-ai-gateway nexus-compliance-proxy nginx
```

### 3e. Start services in dependency order

```bash
sudo systemctl start nats
sudo systemctl start nexus-hub
sleep 3
sudo systemctl start nexus-control-plane
sleep 3
sudo systemctl start nexus-ai-gateway
sleep 3
sudo systemctl start nexus-compliance-proxy
```

---

## Step 4 — Apply database migrations

### Option A: Apply from the air-gapped host

If Node.js and Prisma CLI are available on the air-gapped host (installed from internal mirror):

```bash
tar xzf /tmp/nexus-bundle/db-migrate.tar.gz -C /opt/nexus/
cd /opt/nexus/tools/db-migrate
export DATABASE_URL="postgresql://nexus:CHANGE_ME@127.0.0.1:5432/nexus_gateway?sslmode=disable"
npx prisma migrate deploy
npx prisma db seed
```

### Option B: Apply via SSH tunnel from the build host

```bash
# On build host — tunnel local port 5555 to the air-gapped host's Postgres
ssh -L 5555:127.0.0.1:5432 <airgap-host> -N &

export DATABASE_URL="postgresql://nexus:CHANGE_ME@127.0.0.1:5555/nexus_gateway?sslmode=disable"
cd tools/db-migrate
npx prisma migrate deploy
npx prisma db seed
```

**Seed ordering constraint (binding):** The seed must complete before the Control Plane first starts, or the Control Plane must be restarted after the seed. The `mount.go` startup path calls `idps.GetLocal()` to register the `/authserver/password` route. If the local `IdentityProvider` row does not yet exist, password login returns `404` for the lifetime of that process. Always restart CP after seeding:

```bash
sudo systemctl restart nexus-control-plane
```

Verify schema state:

```bash
cd tools/db-migrate
npx prisma migrate status
# Expected: "All migrations have been applied"
```

---

## Step 5 — Configure provider credentials

Choose one of the two provider modes described in the introduction.

### Option A — Internal mirror of a public provider

In the Control Plane UI (Credentials section), create a new provider credential. In the **Base URL** field, enter your internal mirror endpoint instead of the default public API URL:

| Provider | Default public URL | Air-gapped replacement |
|---|---|---|
| OpenAI | `https://api.openai.com` | `https://openai-mirror.internal` |
| Anthropic | `https://api.anthropic.com` | `https://anthropic-mirror.internal` |

The AI Gateway provider adapters read the base URL from the credential row in PostgreSQL. No code changes are required — the adapter mechanism already supports per-credential base URL overrides. The canonical provider adapter architecture is in `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md`.

### Option B — Self-hosted LLM server (OpenAI-compatible)

Self-hosted OpenAI-compatible servers (Ollama, vLLM, llama.cpp via `llama-server`) expose the same `/v1/chat/completions` API shape. Configure in the Control Plane UI:

1. Under **Credentials**, create a new provider credential with:
   - Provider type: `openai` (or `openai-compat` if available in the catalog)
   - Base URL: your internal model server, for example `http://llm.internal:11434/v1`
   - API key: a placeholder value (most self-hosted servers do not require real keys)
2. Under **Routing Rules**, create a rule that directs traffic to this credential.

**Model catalog note:** The per-model price catalog (token cost per 1K tokens) is static in the database after seeding. Self-hosted model costs default to `$0.00` unless you manually update the `Model` row's `inputCostPer1kTokens` and `outputCostPer1kTokens` fields. This is expected for air-gapped deployments — see Day-2 Operations below.

---

## Step 6 — Deploy Desktop Agent (air-gapped)

### 6a. Auto-update is disabled by default

The agent's auto-updater is disabled unless an Ed25519 signing key is provisioned in the agent binary. In an air-gapped environment with no public update server, this is the correct steady state. Confirm in the Hub Nodes page that agents show their installed version; there is no auto-update attempt from the agent to any external URL. See `docs/developers/architecture/services/agent/agent-autoupdater-architecture.md` §2 for the mechanism.

### 6b. Distribute the agent package internally

Build the agent package on a connected build host using the `build-agent` skill (macOS `.pkg`) or the appropriate build procedure for Linux/Windows. Transfer the package to an internal distribution point:

- **macOS:** Host `NexusAgent-latest.pkg` on an internal nginx server. Distribute via MDM (Jamf, Kandji, Mosyle) or direct download URL.
- **Linux:** Package as `.deb` or `.rpm` and serve from an internal package mirror, or copy the binary directly.
- **Windows:** Package the installer and distribute via internal software deployment tooling (SCCM, Intune-offline, or direct copy).

### 6c. Configure the agent to reach the on-premises Hub

The agent connects to Hub via WebSocket. Ensure the Hub's internal hostname or IP is reachable from all agent endpoints. The agent reads the Hub URL from its config, typically set during enrollment:

```bash
# Agent enrollment — point at the internal Hub
nexus-agent enroll \
    --hub-url https://nexus.internal:3060 \
    --enrollment-token <token-from-cp-ui>
```

### 6d. Trust the internal Compliance Proxy CA

Deploy the Compliance Proxy CA certificate to all managed endpoints so TLS interception succeeds. See Step 8 for CA generation. Platform-specific trust deployment methods are in `docs/operators/ops/pki-and-certs.md` §Compliance Proxy CA Trust Deployment.

---

## Step 7 — Spillstore configuration

The AI Gateway and Compliance Proxy spill large bodies (>256 KiB) to an object store rather than writing them inline into PostgreSQL. In an air-gapped deployment, use the local filesystem driver instead of S3.

Set the spillstore driver to `localfs` in each service's YAML config:

```yaml
# In nexus-hub.yaml, ai-gateway.yaml, compliance-proxy.yaml
spillstore:
  driver: localfs
  localfs:
    baseDir: /var/lib/nexus/spillstore
    maxSizeMB: 10240   # 10 GB cap; tune to available disk
```

```bash
sudo mkdir -p /var/lib/nexus/spillstore
sudo chown nexus:nexus /var/lib/nexus/spillstore
```

**Disk planning:** Each body can be up to several MB. Budget at least 50 GB for the spillstore partition in a busy deployment; set up log rotation or a cron job to purge entries older than your retention policy. The Hub's `retention.purge.spillstore` job (`docs/developers/architecture/cross-cutting/foundation/jobs-architecture.md` §5) manages orphaned objects automatically.

**Note on S3-compatible on-premises stores:** If your air-gapped environment has an on-premises S3-compatible store (MinIO, Ceph RGW), you can configure the `s3` driver with an internal endpoint instead of the default AWS S3. See `docs/developers/architecture/cross-cutting/storage/spillstore-architecture.md` §2 for key shape and driver selection.

---

## Step 8 — TLS and certificate trust

### 8a. Generate the Compliance Proxy CA

The Compliance Proxy requires an ECDSA P-256 CA key. RSA keys are rejected at startup.

```bash
# On the air-gapped target (openssl must be installed)
sudo openssl ecparam -name prime256v1 -genkey -noout \
    -out /var/lib/nexus/proxy-ca/ca.key
sudo openssl req -new -x509 -days 1095 \
    -key /var/lib/nexus/proxy-ca/ca.key \
    -out /var/lib/nexus/proxy-ca/ca.crt \
    -subj "/C=US/O=YourOrg/OU=IT Security/CN=Nexus Proxy CA" \
    -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
    -addext "keyUsage=critical,keyCertSign,cRLSign"
sudo chown nexus:nexus /var/lib/nexus/proxy-ca/ca.*
sudo chmod 640 /var/lib/nexus/proxy-ca/ca.key
```

If your organization uses an internal root CA, sign this as a sub-CA so the trust chain flows from your root. Operators with an internal HSM or KMS can use the proxy's `kms.provider: "command"` mechanism (see `docs/operators/ops/pki-and-certs.md` §KMS Integration) so the private key never rests on disk in cleartext.

### 8b. Distribute the Compliance Proxy CA to endpoints

Every managed endpoint must trust the Compliance Proxy CA for TLS interception to succeed. Methods by platform:

| Platform | Distribution method |
|---|---|
| **macOS (MDM)** | Upload `ca.crt` as a Certificate payload in Jamf, Kandji, or Mosyle; install in System keychain |
| **Windows (GPO)** | Computer Configuration → Trusted Root Certification Authorities; import `ca.crt` |
| **Linux (Debian/Ubuntu)** | `sudo cp ca.crt /usr/local/share/ca-certificates/ && sudo update-ca-certificates` |
| **Linux (RHEL/CentOS)** | `sudo cp ca.crt /etc/pki/ca-trust/source/anchors/ && sudo update-ca-trust` |

Detailed commands and per-tool trust (Firefox, Node.js, Python, Java) are in `docs/operators/ops/pki-and-certs.md`.

### 8c. Hub Agent CA (auto-generated)

The Agent CA is managed by Nexus Hub. Hub auto-generates a self-signed ECDSA P-256 CA on first start at the path set in `agentCA.dir` (default `/var/lib/nexus/agent-ca`). No manual CA generation step is needed. See `docs/operators/ops/pki-and-certs.md` §Agent CA.

### 8d. Internal TLS for services (optional)

By default, services communicate over plain HTTP on loopback. If your security policy requires encryption between services on the same host, configure nginx as a TLS termination layer for each service's internal listener. For multi-host deployments, use your internal PKI to issue TLS certificates for each service. The `REDIS_TLS_*` environment variable family enables mTLS between services and Valkey (see `.env.example`).

---

## Step 9 — Bring up the Control Plane UI

The Control Plane UI is a compiled React SPA served as static files. It makes no outbound calls to public CDNs after the initial build.

### 9a. Deploy the static build

```bash
sudo mkdir -p /var/www/nexus-ui
sudo tar xzf /tmp/nexus-bundle/nexus-ui.tar.gz -C /var/www/nexus-ui
sudo chown -R nginx:nginx /var/www/nexus-ui
```

### 9b. Configure nginx

Follow `docs/operators/ops/ec2-single-node.md` §nginx Configuration, substituting your internal hostname for `nexus.example.com`. Key requirements:

- Every `server {}` block must have both `listen 80;` and `listen [::]:80;` for dual-stack.
- The `nexus.*` vhost serves static files from `/var/www/nexus-ui` and proxies `/api/*` to `:3001`.
- The `api.*` vhost proxies to AI Gateway `:3050`.
- The `hub.*` vhost proxies to Hub `:3060` with WebSocket upgrade support.

```bash
sudo nginx -t   # verify config
sudo systemctl enable --now nginx
```

### 9c. Verify no external CDN references

Confirm the built UI does not reference any external URLs that would fail in an air-gapped environment:

```bash
grep -r 'https://' /var/www/nexus-ui/assets/ | grep -v 'data:' | head -20
# Expected: no hits, or only data: URIs and internal API paths
```

The Vite production build bundles all JavaScript and CSS dependencies; no runtime CDN calls are made.

---

## Step 10 — Smoke test

Run these commands on the air-gapped target to verify end-to-end operation.

### 10a. Service health

```bash
curl -s http://127.0.0.1:3060/healthz   # Hub: {"status":"ok"}
curl -s http://127.0.0.1:3001/healthz   # Control Plane: {"status":"ok"}
curl -s http://127.0.0.1:3050/healthz   # AI Gateway: {"status":"ok","service":"ai-gateway"}
curl -s http://127.0.0.1:3040/healthz   # Compliance Proxy: {"status":"ok"}
```

### 10b. All services registered with Hub

```bash
# Requires an admin bearer token from the Control Plane (obtain via cp_login helper)
# Check the Hub Nodes page in CP UI → Infrastructure → Nodes
# All four services should show status "online"
curl -s -H "Authorization: Bearer <admin-token>" \
    http://127.0.0.1:3001/api/admin/infrastructure/nodes | \
    jq '[.[] | {name: .name, status: .status}]'
# Expected: hub, control-plane, ai-gateway, compliance-proxy all "online"
```

### 10c. NATS streams provisioned

```bash
# Hub auto-provisions NATS streams on start
curl -s http://127.0.0.1:8222/jsz?streams=1 | jq '.streams | length'
# Expected: 5 streams (nexus.traffic, nexus.audit, nexus.ops_metrics, nexus.alerts, nexus.heartbeat)
```

### 10d. Test AI traffic through the gateway

Using a virtual key created in the CP UI (Credentials → Virtual Keys → Create):

```bash
VK="<your-test-virtual-key>"
curl -s -X POST http://127.0.0.1:3050/v1/chat/completions \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $VK" \
    -d '{
        "model": "<your-configured-model>",
        "messages": [{"role": "user", "content": "hello"}]
    }' | jq '.choices[0].message.content'
```

### 10e. Verify traffic_event row written to PostgreSQL

```bash
psql -h 127.0.0.1 -U nexus -d nexus_gateway -c \
    "SELECT id, model, status_code, total_tokens, cost_usd, created_at \
     FROM traffic_event ORDER BY created_at DESC LIMIT 3;"
# Expected: rows with non-null tokens and cost values
```

---

## Day-2 operations in air-gapped mode

### No auto-update

Agent binaries do not self-update. Distribute new versions manually via your internal channel (same path as initial install). The Hub Nodes page shows each agent's current version; compare against your latest internal build.

### No telemetry beaconing

Nexus Gateway does not send telemetry or usage data to any external endpoint. No outbound calls originate from any service after initial deployment.

### Manual model catalog updates

The `Model` table in PostgreSQL is seeded at install time with the models bundled in `tools/db-migrate/seed/`. When a provider releases a new model:

1. Add the model row to the seed file on the connected build host.
2. Re-run `npx prisma db seed` against the air-gapped database (via SSH tunnel or on-host Node.js).

Alternatively, insert the row directly:

```sql
INSERT INTO "Model" (id, "providerId", name, "displayName",
    "inputCostPer1kTokens", "outputCostPer1kTokens",
    "contextWindow", "isActive", "createdAt", "updatedAt")
VALUES ('my-model-id', 'provider-id', 'model-name', 'Model Display Name',
    0.00150, 0.00200, 128000, true, NOW(), NOW());
```

### Manual price catalog updates

Token costs are seeded into the `Model` table at install time. In a connected deployment, operators update these values via the CP UI as providers change pricing. In an air-gapped deployment, update the database rows directly:

```sql
UPDATE "Model"
SET "inputCostPer1kTokens" = 0.00150, "outputCostPer1kTokens" = 0.00200
WHERE id = 'gpt-4o';
```

### Credential rotation

Provider API key rotation works identically in air-gapped mode. Update the credential in the CP UI → Credentials; the Hub signals every AI Gateway node over its persistent WebSocket, and each node hot-swaps the in-memory credential without restarting. See `docs/users/product/features.md` §Credentials for the rotation flow.

### CA rotation

Follow `docs/operators/ops/pki-and-certs.md` §CA Rotation. The key steps: generate new CA, distribute trust to endpoints (while keeping old CA trusted), update compliance-proxy config, restart compliance-proxy, clear Valkey cert cache (`DEL nexus:proxy:cert:*`), then remove old CA trust after a transition window.

### Log access

All services write structured JSON logs:

```bash
sudo tail -f /var/log/nexus/nexus-hub.log | jq 'select(.level=="ERROR")'
sudo tail -f /var/log/nexus/nexus-ai-gateway.log | jq 'select(.level=="ERROR")'
```

---

## Known limitations and follow-ups

The following gaps are known and honest. Operators must account for these before deploying in a regulated air-gapped environment.

1. **Model catalog updates are manual.** There is no automated sync of new models or pricing from provider APIs. Every new model addition or price change requires a manual database update or re-run of the seed. This is a documented gap compared to a connected deployment.

2. **Provider capability flags are static.** Streaming support, reasoning model availability, structured output support, and context window sizes are stored in the `Model` table at seed time. When a provider enables a new capability on an existing model, the database row must be updated manually.

3. **New provider APIs require a build/test cycle.** If a provider changes their API wire format (new fields, new streaming events, error shapes), the corresponding adapter in `packages/ai-gateway/internal/providers/specs/<name>/` must be updated, the binary rebuilt on the connected build host, and the new binary transferred and deployed. There is no in-place adapter hot-reload mechanism.

4. **No SIEM webhook over the internet.** The SIEM bridge in Hub forwards audit events to configured webhook or OTEL sinks. In an air-gapped environment, the SIEM sink endpoint must be an internal URL. External SIEM services (Splunk Cloud, Datadog SaaS) are not reachable. Configure an on-premises SIEM or use PostgreSQL-direct queries for audit access.

5. **No OSS E78 local inference integration yet.** E78 (self-hosted local inference) is planned on the roadmap. The current runbook requires an OpenAI-compatible server (Ollama, vLLM, llama.cpp) already running as an external service. A fully integrated on-premises inference path with model management via the CP UI is future work.

6. **S3-compatible spillstore requires additional setup.** If you use an on-premises S3-compatible store (MinIO, Ceph RGW) instead of the `localfs` driver, you must provision the store, create a bucket, and configure access credentials before starting the services. The `localfs` driver avoids this dependency and is recommended for initial air-gapped deployments.

---

## Verification commands

Run these after completing all steps to confirm the deployment is fully operational.

```bash
# 1. All four services healthy
for port in 3060 3001 3050 3040; do
    status=$(curl -s "http://127.0.0.1:${port}/healthz" | jq -r '.status')
    echo "Port ${port}: ${status}"
done
# Expected: four lines of "ok"

# 2. NATS streams present
curl -s http://127.0.0.1:8222/jsz?streams=1 | jq '[.streams[].config.name]'
# Expected: ["nexus.alerts","nexus.audit","nexus.heartbeat","nexus.ops_metrics","nexus.traffic"]

# 3. Database schema current
cd tools/db-migrate
npx prisma migrate status 2>&1 | grep -E '(applied|pending)'
# Expected: "All migrations have been applied"

# 4. Test request round-trip (requires a valid VK and configured provider)
VK="<test-virtual-key>"
curl -s -w "\nHTTP %{http_code}" \
    -X POST http://127.0.0.1:3050/v1/chat/completions \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $VK" \
    -d '{"model":"<model>","messages":[{"role":"user","content":"ping"}]}' \
    | tail -1
# Expected: HTTP 200

# 5. traffic_event row written
psql -h 127.0.0.1 -U nexus -d nexus_gateway \
    -c "SELECT COUNT(*) FROM traffic_event WHERE created_at > NOW() - INTERVAL '5 minutes';"
# Expected: count > 0 after the request in step 4
```
