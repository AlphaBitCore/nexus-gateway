# Single-Node EC2 Production Deployment

Reference guide for deploying all Nexus Gateway services on a single Amazon Linux 2023 EC2 instance behind an Application Load Balancer.  
Current target: `example.com` — all four Go services + nginx + infra on one node.

---

## Architecture

```
Internet
    │
    ▼
AWS ALB  (HTTPS :443 → HTTP :80, TLS terminated at ALB)
    │  routes by Host header:
    ├─ nexus.example.com  ──┐
    ├─ api.example.com    ──┤
    └─ hub.example.com    ──┘
                                 │
                    EC2 (Amazon Linux 2023)
                    ┌────────────────────────────────────────┐
                    │                                        │
                    │   nginx :80  (dual-stack IPv4+IPv6)    │
                    │   ├─ nexus.*  →  static /var/www/nexus-ui │
                    │   │            + proxy /api/* → :3001  │
                    │   ├─ api.*    →  proxy :3050            │
                    │   └─ hub.*    →  proxy :3060 (+/ws WS)  │
                    │                                        │
                    │   nexus-hub          :3060  (HTTP+WS)  │
                    │   nexus-control-plane :3001  (HTTP)    │
                    │   nexus-ai-gateway   :3050  (HTTP)     │
                    │   nexus-compliance-proxy :3128 (CONNECT)│
                    │                    :3040 (runtime API) │
                    │                    :9090 (metrics)     │
                    │                                        │
                    │   PostgreSQL  :5432  (postgresql.svc)  │
                    │   Redis 6     :6379  (redis6.svc)      │
                    │   NATS        :4222  (nats.svc)        │
                    └────────────────────────────────────────┘
```

**Key design decisions:**
- TLS is terminated at the ALB; all inter-service traffic is plain HTTP on loopback.
- Control Plane UI is compiled (`npm run build`) and served as static files from nginx — no Node.js process in production.
- All four Go binaries run as the `nexus` OS user under systemd.
- Compliance proxy CA cert must be EC P-256 (not RSA); the proxy rejects RSA keys at startup.

---

## Prerequisites

### 1. OS user and directories

```bash
sudo useradd -r -s /sbin/nologin nexus
sudo mkdir -p /etc/nexus /var/log/nexus /var/lib/nexus/{authkeys,agent-ca,proxy-ca}
sudo chown -R nexus:nexus /var/log/nexus /var/lib/nexus
sudo chmod 750 /var/lib/nexus/authkeys /var/lib/nexus/proxy-ca
```

### 2. Go binaries

Build locally and SCP, or build on the instance:

```bash
# From repo root — derive version from the latest prod-* tag + commit:
VER="$(git describe --tags --match 'prod-*' --abbrev=0 2>/dev/null || echo dev)@$(git rev-parse --short HEAD)"
LDFLAGS="-X main.buildVersion=${VER}"

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o nexus-hub            ./packages/nexus-hub/cmd/nexus-hub/
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o nexus-control-plane  ./packages/control-plane/cmd/control-plane/
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o nexus-ai-gateway     ./packages/ai-gateway/cmd/ai-gateway/
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o nexus-compliance-proxy ./packages/compliance-proxy/cmd/compliance-proxy/

# SCP to server
scp nexus-{hub,control-plane,ai-gateway,compliance-proxy} ${PROD_DEPLOY_USER}@<IP>:/tmp/
ssh ${PROD_DEPLOY_USER}@<IP> 'sudo mv /tmp/nexus-* /usr/local/bin/ && sudo chmod +x /usr/local/bin/nexus-*'
```

The version string (`prod-20260508@fe88ec62` format) is stored in the `thing.version` column and displayed in the Hub Nodes page, so each service shows exactly which release is running.

### 3. NATS server

NATS is not in the Amazon Linux package repository; install manually:

```bash
# Download from GitHub releases (adjust version as needed)
curl -LO https://github.com/nats-io/nats-server/releases/download/v2.10.24/nats-server-v2.10.24-linux-amd64.zip
unzip nats-server-v2.10.24-linux-amd64.zip
sudo mv nats-server-v2.10.24-linux-amd64/nats-server /usr/local/bin/
sudo chmod +x /usr/local/bin/nats-server
```

### 4. Infrastructure packages

```bash
sudo dnf install -y postgresql16-server postgresql16 redis6 nginx
sudo postgresql-setup --initdb
sudo systemctl enable --now postgresql redis6
```

### 5. Compliance proxy CA cert (EC P-256 required)

```bash
# IMPORTANT: must be EC, not RSA — proxy rejects RSA keys at startup
sudo openssl ecparam -name P-256 -genkey -noout \
  -out /var/lib/nexus/proxy-ca/ca.key
sudo openssl req -new -x509 -days 3650 \
  -key /var/lib/nexus/proxy-ca/ca.key \
  -out /var/lib/nexus/proxy-ca/ca.crt \
  -subj '/C=US/ST=CA/O=Nexus/CN=Nexus Proxy CA'
sudo chown nexus:nexus /var/lib/nexus/proxy-ca/*
sudo chmod 640 /var/lib/nexus/proxy-ca/ca.key
```

### 6. PostgreSQL — create nexus user and database

```bash
sudo -u postgres psql -c "CREATE USER nexus WITH PASSWORD 'CHANGE_ME';"
sudo -u postgres psql -c "CREATE DATABASE nexus_gateway OWNER nexus;"
# Edit /var/lib/pgsql/data/pg_hba.conf:
#   local  all  postgres  peer
#   host   nexus_gateway  nexus  127.0.0.1/32  scram-sha-256
sudo systemctl restart postgresql
```

---

## Configuration Files

All runtime config lives under `/etc/nexus/`. Template files are in the repo at `packages/<service>/<service>.prod.yaml.example`.

```
/etc/nexus/
  nexus-hub.yaml
  control-plane.yaml
  ai-gateway.yaml
  compliance-proxy.yaml
```

### Shared secrets — must match across all services

Generate once and paste into every config file that references the secret:

```bash
# Internal service token (Hub ↔ CP ↔ AI Gateway ↔ Proxy)
openssl rand -hex 32   # → internalServiceToken

# HMAC secret for virtual key / API key hashing (CP ↔ AI Gateway)
openssl rand -hex 32   # → hmacSecret

# AES-256 credential encryption key (CP ↔ AI Gateway) — must be 64 hex chars
openssl rand -hex 32   # → encryptionKey / credentialMasterKey

# Bootstrap key (CP only, used for first admin key creation)
openssl rand -hex 32   # → bootstrapKey
```

| Secret | CP field | AI Gateway field | Proxy field | Hub field |
|--------|----------|-----------------|-------------|-----------|
| Internal service token | `auth.internalServiceToken` | `auth.internalServiceToken` | `auth.internalServiceToken` | — |
| HMAC secret | `auth.hmacSecret` | `auth.hmacSecret` | — | — |
| Encryption key | `crypto.encryptionKey` | `auth.credentialMasterKey` | — | — |

### Key per-service settings

**nexus-hub.yaml**
```yaml
server:
  port: 3060
authServer:
  issuer: "https://nexus.example.com"  # must match CP issuer
```

**control-plane.yaml**
```yaml
server:
  port: 3001
authServer:
  issuer: "https://nexus.example.com"
  keystoreDir: "/var/lib/nexus/authkeys"
auth:
  allowDevAuth: false   # must be false in production
crypto:
  production: true      # enforces key length validation
bff:
  complianceProxyUrl: "http://127.0.0.1:3040"
  aiGatewayUrl:       "http://127.0.0.1:3050"
  nexusHubUrl:        "http://127.0.0.1:3060"
```

**ai-gateway.yaml**
```yaml
server:
  port: 3050
registry:
  nexusHubUrl: "http://127.0.0.1:3060"   # field name is nexusHubUrl, not controlPlaneUrl
```

**compliance-proxy.yaml**
```yaml
listener:
  address: ":3128"
ca:
  certPath: "/var/lib/nexus/proxy-ca/ca.crt"
  keyPath:  "/var/lib/nexus/proxy-ca/ca.key"   # must be EC P-256
registry:
  nexusHubUrl: "http://127.0.0.1:3060"
runtimeApi:
  listenAddress: "127.0.0.1:3040"
```

---

## Database Migrations and Seed

Run from a machine that can reach the database (SSH tunnel if needed):

```bash
# Tunnel: local 5555 → remote postgres
ssh -L 5555:localhost:5432 ${PROD_DEPLOY_USER}@<IP> -N &

# In tools/db-migrate/, set DATABASE_URL to the tunnel
export DATABASE_URL="postgresql://nexus:PASSWORD@localhost:5555/nexus_gateway?sslmode=disable"

cd tools/db-migrate
npx prisma migrate deploy   # applies all migrations
npx prisma db seed          # seeds IdPs, OAuth clients, default data
```

### Critical seed ordering constraint

**The seed must complete before the Control Plane first starts** (or CP must be restarted after the seed).

`mount.go` calls `idps.GetLocal()` at startup to register the `/authserver/password` route. If the local `IdentityProvider` row does not yet exist, the route is permanently skipped for that process lifetime and password login returns `{"message":"Not Found"}`. Fix: restart CP after the seed has run.

```bash
sudo systemctl restart nexus-control-plane
```

### OAuth client redirect URIs

`auth-seed.ts` registers the `cp-ui` OAuth client. Ensure the production URL is included:

```typescript
// tools/db-migrate/seed/auth-seed.ts — cp-ui redirectUris
redirectUris: [
  'https://nexus.example.com/auth/callback',  // ← production
  'https://localhost:3000/auth/callback',
  'http://localhost:3000/auth/callback',
  // ... other dev URIs
],
```

If the production URL is missing, the PKCE authorize flow returns `redirect_uri not registered`. Either re-run the seed or patch live:

```bash
sudo -u postgres psql -d nexus_gateway -c "
UPDATE \"OAuthClient\"
SET \"redirectUris\" = array_append(\"redirectUris\", 'https://nexus.example.com/auth/callback')
WHERE id = 'cp-ui'
  AND NOT ('https://nexus.example.com/auth/callback' = ANY(\"redirectUris\"));"
```

---

## Control Plane UI — Build and Deploy

The React SPA is compiled once and served as static files; no Node.js process runs in production.

```bash
# In packages/control-plane-ui/
npm install
npm run build          # outputs to dist/

# Deploy to server
rsync -av dist/ ${PROD_DEPLOY_USER}@<IP>:/tmp/nexus-ui-dist/
ssh ${PROD_DEPLOY_USER}@<IP> '
  sudo rm -rf /var/www/nexus-ui
  sudo mv /tmp/nexus-ui-dist /var/www/nexus-ui
  sudo chown -R nginx:nginx /var/www/nexus-ui
'
```

---

## Systemd Units

All units are in `/etc/systemd/system/`. After any edit: `sudo systemctl daemon-reload`.

```
nats.service                 — NATS JetStream Server
nexus-hub.service            — Nexus Hub (After: pg, redis, nats)
nexus-control-plane.service  — Control Plane (After: pg, redis, nats, hub)
nexus-ai-gateway.service     — AI Gateway (After: pg, redis, nats, hub)
nexus-compliance-proxy.service — Compliance Proxy (After: pg, redis, nats, hub)
```

All units use:
- `User=nexus`, `Group=nexus`
- `Restart=on-failure`, `RestartSec=5s`
- `LimitNOFILE=1048576` (1 M file descriptors)

Enable all for boot:

```bash
sudo systemctl enable nats nexus-hub nexus-control-plane \
    nexus-ai-gateway nexus-compliance-proxy nginx
```

---

## nginx Configuration

File: `/etc/nginx/conf.d/nexus.conf`

### Vhost routing

| Server name | Backend | Notes |
|-------------|---------|-------|
| default (catch-all) | — | Returns 404; used by ALB health check at `/health` → 200 |
| `nexus.example.com` | Static `/var/www/nexus-ui` + proxy `:3001` | SPA fallback + API prefix match |
| `api.example.com` | proxy `:3050` | AI Gateway |
| `hub.example.com` | proxy `:3060` | Hub HTTP; `/ws` with WebSocket upgrade |

### API prefix regex for nexus.*

```nginx
location ~ ^/(api|oauth|authserver|idp|\.well-known|healthz|metrics|ready|debug)(/|$) {
    proxy_pass http://127.0.0.1:3001;
    ...
}
```

### IPv6 requirement

Every `server {}` block must have **both** `listen 80;` and `listen [::]:80;`. ALB health checks can arrive via IPv6 internally. A missing `[::]:80` causes the named vhost to be ignored for IPv6 connections, which hit the default server instead and return 404.

### nginx.conf global settings

```nginx
worker_processes      auto;          # = number of CPU cores
worker_rlimit_nofile  65536;
events {
    worker_connections  16384;       # per worker; total = cores × 16384
    use epoll;
    multi_accept on;
}
```

### nginx log format — includes upstream info

```nginx
log_format main '$remote_addr ... "$request" $status $body_bytes_sent '
                '"$http_referer" "$http_user_agent" "$http_x_forwarded_for" '
                '$upstream_addr $upstream_status $upstream_response_time';
```

The `$upstream_*` fields are essential for distinguishing "nginx returned 404" vs "backend returned 404".

---

## OS Tuning

### `/etc/sysctl.d/99-nexus.conf`

```
net.core.somaxconn              = 65536
net.core.netdev_max_backlog     = 65536
net.ipv4.tcp_max_syn_backlog    = 65536
net.ipv4.ip_local_port_range    = 10000 65535   # ~55K ephemeral ports
net.ipv4.tcp_fin_timeout        = 15
net.ipv4.tcp_keepalive_time     = 300
net.ipv4.tcp_tw_reuse           = 1
net.core.rmem_max               = 16777216      # 16 MB TCP read buffer
net.core.wmem_max               = 16777216
net.ipv4.tcp_rmem               = 4096 87380 16777216
net.ipv4.tcp_wmem               = 4096 65536 16777216
```

Applied on boot by `systemd-sysctl`. Reload live: `sudo sysctl --system`.

### `/etc/security/limits.d/nexus.conf`

```
nexus soft nofile 1048576
nexus hard nofile 1048576
root  soft nofile 1048576
root  hard nofile 1048576
*     soft nofile 65536
*     hard nofile 65536
```

Systemd services override this via `LimitNOFILE=1048576` in the unit file (takes precedence over PAM limits for daemonized processes). The PAM entry covers interactive sessions and scripts.

**Why 1 M (1048576)?**  
The compliance proxy can have O(10K) concurrent CONNECT tunnels, each consuming 3–5 file descriptors. The AI Gateway and Hub maintain long-lived WebSocket connections. 65536 would be a hard ceiling at ~15K concurrent tunnels; 1M removes the constraint entirely at current traffic levels.

---

## Operations

### Management script

`${NEXUS_HOME}/nexus.sh` — wraps `systemctl` with human-friendly aliases:

```bash
./nexus.sh status                 # all service states + PIDs
./nexus.sh restart cp             # restart control-plane
./nexus.sh restart all            # restart hub + cp + gw + proxy + nats + nginx
./nexus.sh stop proxy             # stop compliance-proxy
./nexus.sh logs gw                # tail -f /var/log/nexus/ai-gateway.log
./nexus.sh logs cp                # tail -f /var/log/nexus/control-plane.log
```

Service aliases: `hub` `cp` `gw` `proxy` `nats` `nginx`.

### Log files

| Service | Log file |
|---------|----------|
| Nexus Hub | `/var/log/nexus/nexus-hub.log` |
| Control Plane | `/var/log/nexus/control-plane.log` |
| AI Gateway | `/var/log/nexus/ai-gateway.log` |
| Compliance Proxy | `/var/log/nexus/compliance-proxy.log` |
| nginx access | `/var/log/nginx/access.log` |
| nginx error | `/var/log/nginx/error.log` |

All Go service logs are structured JSON (log/slog). Parse with `jq`:

```bash
sudo tail -f /var/log/nexus/control-plane.log | jq 'select(.level=="ERROR")'
```

### Health check quick reference

```bash
curl -s http://127.0.0.1:3060/healthz   # Hub
curl -s http://127.0.0.1:3001/healthz   # Control Plane
curl -s http://127.0.0.1:3050/healthz   # AI Gateway
curl -s http://127.0.0.1:3040/healthz   # Compliance Proxy runtime API
curl -s http://127.0.0.1:80/health      # nginx ALB health check → "ok"
```

### Database access (direct)

```bash
sudo -u postgres psql -d nexus_gateway
# or via nexus user:
psql -h 127.0.0.1 -U nexus -d nexus_gateway
```

---

## Deployment Checklist

Use this list for a fresh deploy or after a major upgrade.

### Infrastructure
- [ ] PostgreSQL running, `nexus` user and `nexus_gateway` DB created
- [ ] Redis running and reachable on `127.0.0.1:6379`
- [ ] NATS JetStream running on `:4222`

### Secrets
- [ ] `internalServiceToken` — same value in nexus-hub, control-plane, ai-gateway, compliance-proxy configs
- [ ] `hmacSecret` — same value in control-plane and ai-gateway configs
- [ ] `encryptionKey` / `credentialMasterKey` — same 64-hex-char value in both
- [ ] `authServer.issuer` — same URL in nexus-hub and control-plane configs
- [ ] `crypto.production: true` set in control-plane config

### Certificates
- [ ] Proxy CA cert exists at `/var/lib/nexus/proxy-ca/ca.{crt,key}`
- [ ] CA key is **EC P-256** (compliance-proxy rejects RSA)
- [ ] Auth keystore dir exists at `/var/lib/nexus/authkeys` (owned by `nexus`)

### Database
- [ ] `npx prisma migrate deploy` completed with no errors
- [ ] `npx prisma db seed` completed — local `IdentityProvider` row created
- [ ] Seed output confirms: `cp-ui` OAuth client registered with production redirect URI

### Services
- [ ] All 8 systemd units enabled (`systemctl is-enabled`)
- [ ] Start order: nats → nexus-hub → nexus-control-plane → nexus-ai-gateway → nexus-compliance-proxy
- [ ] **Control Plane started (or restarted) AFTER seed** — required for password login route
- [ ] All services show `online` in Hub thing registry (check via CP UI → Infrastructure → Nodes)

### nginx
- [ ] `/var/www/nexus-ui` contains the production Vite build
- [ ] Each `server {}` block has `listen [::]:80;` (dual-stack)
- [ ] `nginx -t` passes cleanly
- [ ] ALB health check: `curl http://127.0.0.1/health` → `200 ok`

### Smoke tests
- [ ] `https://nexus.example.com` loads the React dashboard
- [ ] Login with `admin@nexus.ai` succeeds and redirects to dashboard
- [ ] `https://api.example.com/healthz` → `{"status":"ok",...}`
- [ ] `https://hub.example.com/healthz` → `{"status":"ok"}`

---

## Known Issues and Gotchas

### 1. Password login returns 404 after fresh deploy

**Cause:** Control Plane's `authserver.Mount()` calls `idps.GetLocal()` at startup. If the seed has not yet run, there is no local `IdentityProvider` row and the `/authserver/password` route is never registered.

**Fix:** Run the seed first, then restart CP:
```bash
npx prisma db seed
sudo systemctl restart nexus-control-plane
```

### 2. `thing` rows missing after DB reset

**Cause:** If the `thing` table is truncated or reset (e.g., re-migration with `--force-reset`) while services are running, the services do not auto-re-register because they are already connected via WebSocket. The Hub and CP detect the missing row via their heartbeat loops and re-register themselves, but AI Gateway and Compliance Proxy do not.

**Fix:** Restart both services after any DB reset:
```bash
sudo systemctl restart nexus-ai-gateway nexus-compliance-proxy
```

### 3. Compliance proxy won't start — "no EC PRIVATE KEY PEM block found"

**Cause:** The CA key was generated as RSA instead of EC.

**Fix:** Regenerate with `openssl ecparam -name P-256`:
```bash
sudo openssl ecparam -name P-256 -genkey -noout -out /var/lib/nexus/proxy-ca/ca.key
sudo openssl req -new -x509 -days 3650 \
  -key /var/lib/nexus/proxy-ca/ca.key \
  -out /var/lib/nexus/proxy-ca/ca.crt \
  -subj '/C=US/ST=CA/O=Nexus/CN=Nexus Proxy CA'
sudo systemctl restart nexus-compliance-proxy
```

### 4. nginx routes to wrong vhost (404 from default server)

**Cause:** A `server {}` block is missing `listen [::]:80;`. ALB nodes may connect via IPv6 internally, hitting the default catch-all instead of the named vhost.

**Fix:** Ensure every `server {}` in `nexus.conf` has both `listen 80;` and `listen [::]:80;`.

### 5. Cannot diagnose nginx proxy issues from access log

**Cause:** Default nginx `log_format` omits `$upstream_addr $upstream_status`. Without these, a `404` in the access log is ambiguous (nginx itself, or the backend?).

The current config includes upstream fields:
```
$upstream_addr $upstream_status $upstream_response_time
```
Example: `127.0.0.1:3001 404 0.003` vs `- - -` (nginx returned the 404 itself).

### 6. "redirect_uri not registered" on OAuth authorize

**Cause:** `cp-ui` OAuth client `redirectUris` array in the DB does not include the production URL.

**Fix:** Re-run seed (after adding the URL to `auth-seed.ts`) or patch the DB directly — see [OAuth client redirect URIs](#oauth-client-redirect-uris) above.

### 7. NATS not available in dnf/yum

NATS server is not packaged for Amazon Linux. Must be downloaded from GitHub releases and installed to `/usr/local/bin/` manually — see [Prerequisites → NATS server](#3-nats-server).
