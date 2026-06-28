# Nexus Gateway — Single-Host Install (Test / Staging)

Step-by-step install for the full Nexus Gateway stack on **one** Amazon
Linux 2023 EC2 instance. Tested against AL2023 (kernel 6.1.x, x86_64).

Produces a self-contained box running:

| Role              | Software                                           | Listen        |
|-------------------|----------------------------------------------------|---------------|
| Hub               | `nexus-hub` (Go)                                   | `:3060`       |
| Control Plane     | `nexus-control-plane` (Go, REST + SPA proxy)       | `:3001`       |
| AI Gateway        | `nexus-ai-gateway` (Go, OpenAI-compatible `/v1/*`) | `:3050`       |
| Compliance Proxy  | `nexus-compliance-proxy` (Go, TLS CONNECT)         | `127.0.0.1:3040` |
| Edge router       | `nginx`                                            | `:80`         |
| Database          | `PostgreSQL 15`                                    | `127.0.0.1:5432` |
| KV / cache        | `redis/redis-stack-server:latest` (docker)         | `127.0.0.1:6379` |
| Message bus       | `nats-server 2.10.24` (JetStream)                  | `:4222`, `:8222` |

This document covers the **test / staging** install. Production deploys
add: hardened TLS at an upstream load balancer, off-host PostgreSQL,
external object store for spill, multi-AZ NATS, secrets in a real
manager (Vault / AWS Secrets Manager). See the `prod-deploy` runbook
for those differences.

---

## 0. Conventions used in this guide

Throughout, replace placeholders with values from your environment:

| Placeholder              | Example                                  | Where to set                                       |
|--------------------------|------------------------------------------|----------------------------------------------------|
| `<HOST_IP>`              | `203.0.113.10`                           | EC2 public IP for ssh                              |
| `<DOMAIN>`               | `nexus.example.com`                      | Base domain you own (3 subdomains under it)        |
| `<DB_PASSWORD>`          | random 32 hex chars                      | DB role + `DATABASE_URL`                           |
| `<INTERNAL_SERVICE_TOKEN>` | random 64 hex chars                    | Shared bearer between Hub / CP / AI-GW / Proxy     |
| `<ADMIN_KEY_HMAC_SECRET>` | random 64 hex chars                     | Signs admin virtual-key derivations                |
| `<CREDENTIAL_ENCRYPTION_KEY>` | random 64 hex chars                 | AES key for upstream-provider credentials at rest  |
| `<AI_GATEWAY_API_TOKEN>` | `ngw_api_` + random 48 hex chars         | Internal Hub→AI-GW call token                      |
| `<S3_BUCKET>`            | `nexus-payload-capture-bucket-test`      | Object store for request/response spill            |
| `<IAM_ROLE_NAME>`        | `ec2-s3-fullaccess-iam-role`             | EC2 instance profile granting S3 write on bucket   |

Generate every random value with `openssl rand -hex 32` (or `-hex 24`
for the API token suffix) on your laptop; never re-use prod values.

All commands run as `ec2-user` on the test host unless prefixed `sudo`.
The convention `[OLD] $` indicates a command on the source host when
migrating; `[NEW] $` indicates the fresh test host.

---

## 1. Prerequisites

### 1.1 EC2 instance

- Amazon Linux 2023 (x86_64)
- Recommended: **8 GB RAM, 50 GB gp3** for a low-traffic test box. The
  4 Go services + Postgres + Valkey + NATS will run on 4 GB but you'll
  want headroom for compile steps (Section 5.2) and per-request memory
  spikes.
- Open inbound: `22/tcp` (ssh), `80/tcp` (HTTP for nginx). HTTPS
  termination is on an upstream load balancer in prod; for a quick
  test, you can terminate TLS in nginx — out of scope here.

### 1.2 IAM role for S3 spill

The 4 Go services write spillover request/response bodies to S3.
**Attach an instance profile** that grants `s3:PutObject` on
`<S3_BUCKET>/test/*` (and `s3:GetObject` if you want to replay
captured payloads).

```bash
[NEW] $ aws sts get-caller-identity
{
    "UserId": "AROA...:i-...",
    "Arn": "arn:aws:sts::...:assumed-role/<IAM_ROLE_NAME>/i-..."
}
```

If `aws sts get-caller-identity` returns `Unable to locate
credentials`, the instance has no IAM role attached — fix this in the
AWS console (EC2 → instance → Actions → Security → Modify IAM role)
before going further.

### 1.3 DNS records

Three sub-hostnames must resolve to the host:

| Hostname               | nginx → backend |
|------------------------|-----------------|
| `nexus.<DOMAIN>`       | `127.0.0.1:3001` (Control Plane + SPA UI) |
| `api.<DOMAIN>`         | `127.0.0.1:3050` (AI Gateway, OpenAI-compatible) |
| `hub.<DOMAIN>`         | `127.0.0.1:3060` (Hub WebSocket + REST) |

Either point all three at `<HOST_IP>` directly, or front them with a
load balancer whose target group includes the host. The Hub's
`hub.allowedOrigins` list (see Section 7.1) **must** include the
`https://nexus.<DOMAIN>` you publish.

---

## 2. Install base packages

```bash
[NEW] $ sudo dnf install -y \
    postgresql15-server postgresql15-contrib \
    nginx docker \
    tar wget gzip rsync jq
[NEW] $ sudo systemctl enable --now docker
[NEW] $ sudo usermod -aG docker ec2-user  # log out + back in for group to take effect
```

NATS is not in the AL2023 repos — install from the upstream release in
Section 6. The cache layer (Section 5) runs as a docker container, so
we install `docker` here too.

---

## 3. Create the `nexus` system user + runtime directories

The four Go services all run as `nexus:nexus`. PostgreSQL has its
own `postgres` user (already created by the `postgresql15-server`
package). Valkey runs as `valkey:valkey` (also from the package).

```bash
[NEW] $ sudo groupadd -g 990 nexus 2>/dev/null || true
[NEW] $ sudo useradd -u 990 -g 990 -r -s /sbin/nologin -d /home/nexus -m nexus 2>/dev/null || true

[NEW] $ sudo mkdir -p \
    /etc/nexus \
    /etc/nexus-gateway \
    /etc/nats \
    /var/lib/nexus/authkeys \
    /var/lib/nexus/agent-ca \
    /var/lib/nexus/proxy-ca \
    /var/lib/nats/jetstream \
    /var/log/nexus \
    /var/www/nexus-ui/downloads

[NEW] $ sudo chown -R nexus:nexus \
    /var/lib/nexus /var/lib/nats /var/log/nexus \
    /etc/nexus /etc/nexus-gateway
[NEW] $ sudo chmod 700 /var/lib/nexus/authkeys /var/lib/nexus/agent-ca /var/lib/nexus/proxy-ca
[NEW] $ sudo chmod 640 /etc/nexus-gateway   # env file mode (created in Section 7.3)
```

Pinning `uid=990 / gid=990` matches the prod build convention so a
restore from a prod `pg_dump` (which references the uid via filesystem
spill paths) doesn't drift. If you don't care about cross-host
parity, drop the explicit `-u` / `-g` flags.

---

## 4. PostgreSQL

### 4.1 Initialize the cluster

```bash
[NEW] $ sudo /usr/bin/postgresql-setup --initdb
[NEW] $ sudo systemctl enable --now postgresql
```

### 4.2 Switch authentication to scram-sha-256

The default `pg_hba.conf` uses `ident` for local TCP, which won't
match our app config. Replace it:

```bash
[NEW] $ sudo tee /var/lib/pgsql/data/pg_hba.conf > /dev/null <<'EOF'
local   all             postgres                                peer
local   all             all                                     scram-sha-256
host    all             all             127.0.0.1/32            scram-sha-256
host    all             all             ::1/128                 scram-sha-256
local   replication     all                                     peer
host    replication     all             127.0.0.1/32            scram-sha-256
host    replication     all             ::1/128                 scram-sha-256
EOF
[NEW] $ sudo chown postgres:postgres /var/lib/pgsql/data/pg_hba.conf
[NEW] $ sudo chmod 600 /var/lib/pgsql/data/pg_hba.conf
[NEW] $ sudo systemctl reload postgresql
```

`password_encryption=scram-sha-256` is already the PG 15 default —
verify with:

```bash
[NEW] $ sudo -u postgres psql -c "SHOW password_encryption;"
```

### 4.3 Create the role + database

```bash
[NEW] $ sudo -u postgres psql -c \
    "CREATE ROLE nexus WITH LOGIN PASSWORD '<DB_PASSWORD>';"
[NEW] $ sudo -u postgres psql -c \
    "CREATE DATABASE nexus_gateway OWNER nexus;"

# Confirm password login works (the apps will use this exact form)
[NEW] $ PGPASSWORD='<DB_PASSWORD>' psql -h localhost -U nexus -d nexus_gateway \
        -c "SELECT current_user, current_database();"
```

### 4.4 Load schema

Two paths:

**(a) Fresh schema from the repo.** Apply the schema from a
checkout that matches the binaries you'll install in Section 8 (clone
the repo, then `cd tools/db-migrate && npm run db:push`, followed by
`npm run seed`). This is the right path for a clean test environment.

**(b) Restore from an existing host's `pg_dump`.** Use this when you're
mirroring an existing environment. Generate the dump on the source host:

```bash
[OLD] $ PGPASSWORD='<DB_PASSWORD>' pg_dump \
        -h localhost -U nexus -d nexus_gateway \
        --no-owner --no-privileges --clean --if-exists \
        | gzip -6 > /tmp/nexus_gateway.dump.gz
```

`--clean --if-exists` makes the dump idempotent: it drops every
relation before recreating, so partial restores recover cleanly.
`--no-owner --no-privileges` lets the dump restore into a database
owned by any role (handy when the source/target role names differ).

Copy the dump to the new host (via your laptop, or directly if both
hosts can reach each other) and restore:

```bash
[NEW] $ gunzip -c /tmp/nexus_gateway.dump.gz | \
        PGPASSWORD='<DB_PASSWORD>' psql -h localhost -U nexus -d nexus_gateway \
            -v ON_ERROR_STOP=0
```

Sanity check after restore — these counts should match the source:

```sql
SELECT 'traffic_event' AS tbl, count(*) FROM traffic_event
UNION ALL SELECT 'thing', count(*) FROM thing
UNION ALL SELECT 'Credential', count(*) FROM "Credential"
UNION ALL SELECT 'VirtualKey', count(*) FROM "VirtualKey"
UNION ALL SELECT 'Model', count(*) FROM "Model"
ORDER BY tbl;
```

The thing rows from the source host stay in the table; once you start
the services on the new host they'll register **new** rows keyed by
the new hostname. The source-host rows turn to `status='offline'`
naturally as their last-seen timestamps age out.

---

## 5. Cache / KV: redis-stack-server (docker)

The semantic-cache subsystem in the AI Gateway and Hub uses
`FT.CREATE` (vector index) with `TEXT`, `TAG`, and `VECTOR` field
types. The supported deployment is the
`redis/redis-stack-server:latest` docker image — Redis 7 + full
RediSearch, RedisJSON, RedisBloom, RedisTimeSeries.

Why not native packages on AL2023:

- The AL2023 `valkey-8.0.6` package ships only the core server; the
  `valkey-search` module is not in the dnf repo. Building it from
  source requires Clang 16+ or GCC 12+ and pulls gRPC / Abseil /
  Protobuf as embedded source — 30–90 minutes of compile on a
  2 vCPU / 4 GB box, plus it currently lacks TEXT field support which
  Hub's semantic-cache schema requires.
- The AL2023 `redis6-6.2.20` package lacks module support entirely
  (RediSearch needs Redis 7+).

A docker container is the path that "just works" on a fresh AL2023
box. Production deployments with more compile capacity may prefer to
package a custom `valkey-server + libsearch.so` rpm or run a managed
service (ElastiCache, MemoryDB) — out of scope here.

### 5.1 Run the container

```bash
[NEW] $ sudo mkdir -p /var/lib/valkey
[NEW] $ sudo chown 999:999 /var/lib/valkey      # uid 999 = `redis` inside the image

[NEW] $ sudo docker pull redis/redis-stack-server:latest

# --network host keeps the bind on the host's loopback. The
# REDIS_ARGS env restricts the server to 127.0.0.1 + ::1 so it never
# accepts external connections even if the security group changes.
[NEW] $ sudo docker run -d \
    --name valkey \
    --restart unless-stopped \
    --network host \
    -v /var/lib/valkey:/data \
    -e REDIS_ARGS="--bind 127.0.0.1 ::1 --protected-mode yes --port 6379" \
    redis/redis-stack-server:latest
```

The container name remains `valkey` for forward-compatibility — if
you later swap to native Valkey + `valkey-search`, the service
contract on `:6379` is identical.

### 5.2 Smoke

```bash
[NEW] $ sudo docker exec valkey redis-cli PING                  # → PONG
[NEW] $ sudo docker exec valkey redis-cli MODULE LIST | head    # → search, json, bf, timeseries, ReJSON, redisgears_2
[NEW] $ sudo docker exec valkey redis-cli FT.CREATE smoke_idx \
        ON HASH PREFIX 1 smoke: SCHEMA name TEXT             # → OK
[NEW] $ sudo docker exec valkey redis-cli FT.DROPINDEX smoke_idx  # → OK
```

If `FT.CREATE` fails with `Invalid field type for field 'name'`,
you're running `valkey/valkey-bundle:8-trixie` instead of
`redis/redis-stack-server:latest`. The bundle image's `search`
module is `valkey-search` (does not yet support TEXT); the
redis-stack image bundles the full RediSearch.

### 5.3 If you also did `dnf install valkey`

The native valkey systemd service will fight for `:6379`. Stop +
disable it before starting the container:

```bash
[NEW] $ sudo systemctl stop valkey 2>/dev/null
[NEW] $ sudo systemctl disable valkey 2>/dev/null
[NEW] $ sudo dnf remove -y valkey valkey-devel 2>/dev/null
```

---

## 6. NATS JetStream

Download the static `nats-server` binary (or copy from an existing
host — it's a self-contained Go binary). The version matters less
than the bin/conf pair being internally consistent.

```bash
[NEW] $ NATS_VERSION=2.10.24
[NEW] $ curl -fsSL -o /tmp/nats.tar.gz \
    https://github.com/nats-io/nats-server/releases/download/v${NATS_VERSION}/nats-server-v${NATS_VERSION}-linux-amd64.tar.gz
[NEW] $ tar -xzf /tmp/nats.tar.gz -C /tmp/
[NEW] $ sudo install -m 0755 \
        /tmp/nats-server-v${NATS_VERSION}-linux-amd64/nats-server \
        /usr/local/bin/nats-server
[NEW] $ /usr/local/bin/nats-server --version
```

### 6.1 Server config

```bash
[NEW] $ sudo tee /etc/nats/nats-server.conf > /dev/null <<'EOF'
port: 4222
server_name: nexus-test
http_port: 8222

jetstream {
  store_dir: /var/lib/nats/jetstream
  max_memory_store: 256MB
  max_file_store: 32GB
}
EOF
```

### 6.2 systemd unit

```bash
[NEW] $ sudo tee /etc/systemd/system/nats.service > /dev/null <<'EOF'
[Unit]
Description=NATS JetStream Server
After=network.target

[Service]
Type=simple
User=nexus
Group=nexus
ExecStart=/usr/local/bin/nats-server -c /etc/nats/nats-server.conf
Restart=on-failure
RestartSec=5s
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
[NEW] $ sudo systemctl daemon-reload
[NEW] $ sudo systemctl enable --now nats
[NEW] $ curl -s http://localhost:8222/varz | jq '.version, .jetstream.config.store_dir'
```

---

## 7. Application config: yaml + env + secrets

### 7.1 The four service yamls (`/etc/nexus/`)

Copy the four yamls from `docs/operators/ops/sample-configs/` (or
from an existing host — they're the same shape across environments)
and edit per-environment values. Each yaml is independent; the four
services do **not** share a config file.

Key per-yaml fields you must set on a fresh test box:

**`nexus-hub.yaml`** — server, db, redis (valkey), nats, hub identity:
```yaml
publicURL: "https://hub.<DOMAIN>"
server:           { port: 3060 }
database:         { url: "postgres://nexus:<DB_PASSWORD>@localhost:5432/nexus_gateway?sslmode=disable" }
redis:            { mode: standalone, addrs: ["localhost:6379"] }
mq:               { driver: nats, nats: { url: "nats://localhost:4222" } }
auth:             { internalServiceToken: "<INTERNAL_SERVICE_TOKEN>" }
authServer:       { url: "https://nexus.<DOMAIN>",
                    jwksURL: "https://nexus.<DOMAIN>/.well-known/jwks.json",
                    issuer:  "https://nexus.<DOMAIN>" }
agentCA:          { dir: "/var/lib/nexus/agent-ca" }
hub:              { id: "hub-test", advertiseAddr: "127.0.0.1:3060",
                    allowedOrigins: ["https://nexus.<DOMAIN>"] }
spill:            { enabled: true, backend: s3,
                    s3: { bucket: "<S3_BUCKET>", region: us-east-1,
                          prefix: "test/", perObjectCap: 268435456 } }
```

**`control-plane.yaml`** — admin REST + SPA proxy:
```yaml
publicURL: "https://nexus.<DOMAIN>"
server:           { port: 3001, advertiseHost: "127.0.0.1" }
database:         { url: "postgresql://nexus:<DB_PASSWORD>@localhost:5432/nexus_gateway?sslmode=disable",
                    maxConns: 50, minConns: 10 }
redis:            { mode: standalone, addrs: ["localhost:6379"] }
bff:              { complianceProxyUrl: "http://127.0.0.1:3040",
                    complianceProxyRuntimeUrl: "http://127.0.0.1:3040",
                    aiGatewayUrl: "http://127.0.0.1:3050" }
registry:         { nexusHubUrl: "http://127.0.0.1:3060" }
authServer:       { issuer: "https://nexus.<DOMAIN>",
                    keystoreDir: "/var/lib/nexus/authkeys" }
agent:            { caDir: "/var/lib/nexus/agent-ca" }
mq:               { driver: nats, nats: { url: "nats://localhost:4222" } }
```

**`ai-gateway.yaml`** — OpenAI-compatible `/v1/*` surface:
```yaml
publicURL: "https://api.<DOMAIN>"
server:           { port: 3050, advertiseHost: "127.0.0.1" }
database:         { url: "postgresql://nexus:<DB_PASSWORD>@localhost:5432/nexus_gateway?sslmode=disable" }
redis:            { mode: standalone, addrs: ["localhost:6379"] }
auth:             { hmacSecret: "<ADMIN_KEY_HMAC_SECRET>",
                    credentialMasterKey: "<CREDENTIAL_ENCRYPTION_KEY>",
                    internalServiceToken: "<INTERNAL_SERVICE_TOKEN>" }
registry:         { nexusHubUrl: "http://127.0.0.1:3060" }
mq:               { driver: nats, nats: { url: "nats://localhost:4222" } }
cors:             { enabled: true, allowedOrigins: ["https://nexus.<DOMAIN>"] }
cache:            { enabled: true, ttl: 5m, prefix: "ai-gw:", broker: true }
```

**`compliance-proxy.yaml`** — TLS-CONNECT intercept (out-of-band):
```yaml
listener:         { address: ":3128" }
ca:               { certPath: "/var/lib/nexus/proxy-ca/ca.crt",
                    keyPath:  "/var/lib/nexus/proxy-ca/ca.key" }
database:         { url: "postgresql://nexus:<DB_PASSWORD>@localhost:5432/nexus_gateway?sslmode=disable" }
redis:            { mode: standalone, addrs: ["localhost:6379"] }
runtimeApi:       { listenAddress: "127.0.0.1:3040" }
mq:               { driver: nats, nats: { url: "nats://localhost:4222" } }
registry:         { nexusHubUrl: "http://127.0.0.1:3060" }
auth:             { internalServiceToken: "<INTERNAL_SERVICE_TOKEN>" }
```

### 7.2 Generate the runtime key material

Three on-disk artifacts the services expect:

```bash
# (1) JWT signing key — used by Control Plane to sign tokens. Any
#     RSA 2048 keypair works; the file basename gets used as `kid`.
[NEW] $ sudo -u nexus openssl genrsa -out /var/lib/nexus/authkeys/key-$(date +%s).pem 2048
[NEW] $ sudo chmod 600 /var/lib/nexus/authkeys/*.pem

# (2) Agent CA — signs the per-device certs the desktop agent uses
#     for mTLS to Hub. Lifetime is long; rotating it invalidates all
#     enrolled agents.
[NEW] $ sudo -u nexus bash -c '
  cd /var/lib/nexus/agent-ca
  openssl ecparam -genkey -name prime256v1 -noout -out ca-key.pem
  openssl req -x509 -new -nodes -key ca-key.pem -days 3650 \
    -subj "/CN=nexus-agent-ca-test" -out ca.pem
'
[NEW] $ sudo chmod 600 /var/lib/nexus/agent-ca/*.pem

# (3) Compliance-proxy CA — signs per-domain leaf certs at intercept
#     time so the proxy can MITM HTTPS for org-managed devices.
#     Devices must trust this CA explicitly. pathlen:0 is required:
#     the proxy CA only ever signs leaf certs, and the constraint
#     stops a stolen CA key from minting an intermediate CA that
#     trusting devices would accept.
[NEW] $ sudo -u nexus bash -c '
  cd /var/lib/nexus/proxy-ca
  openssl ecparam -genkey -name prime256v1 -noout -out ca.key
  openssl req -x509 -new -nodes -key ca.key -days 3650 \
    -subj "/CN=nexus-compliance-proxy-ca-test" \
    -addext "basicConstraints=critical,CA:TRUE,pathlen:0" -out ca.crt
'
[NEW] $ sudo chmod 600 /var/lib/nexus/proxy-ca/ca.key
[NEW] $ sudo chmod 644 /var/lib/nexus/proxy-ca/ca.crt
```

If you're restoring from a `pg_dump` (Section 4.4 path b), you'll
also need the source host's key material — DB rows in `Credential`
were encrypted with the source `<CREDENTIAL_ENCRYPTION_KEY>` and
won't decrypt under a fresh key. tar the source-host
`/var/lib/nexus/` and restore it whole instead of regenerating:

```bash
[OLD] $ sudo tar -czf /tmp/nexus-keys.tgz -C /var/lib nexus
# Move the tarball to the new host, then:
[NEW] $ sudo tar -xzf /tmp/nexus-keys.tgz -C /var/lib/
[NEW] $ sudo chown -R nexus:nexus /var/lib/nexus
[NEW] $ sudo chmod 700 /var/lib/nexus/{authkeys,agent-ca,proxy-ca}
```

### 7.3 The shared EnvironmentFile (`/etc/nexus-gateway/env`)

All four services read `/etc/nexus-gateway/env` (the systemd units
declare `EnvironmentFile=-/etc/nexus-gateway/env`). Several yaml
fields above duplicate what's in this env file; the services prefer
env over yaml when both are present, so the env is the source of
truth for secrets.

```bash
[NEW] $ sudo tee /etc/nexus-gateway/env > /dev/null <<EOF
INTERNAL_SERVICE_TOKEN=<INTERNAL_SERVICE_TOKEN>
ADMIN_KEY_HMAC_SECRET=<ADMIN_KEY_HMAC_SECRET>
CREDENTIAL_ENCRYPTION_KEY=<CREDENTIAL_ENCRYPTION_KEY>
AI_GATEWAY_API_TOKEN=<AI_GATEWAY_API_TOKEN>

NEXUS_HUB_URL=http://127.0.0.1:3060
DATABASE_URL=postgresql://nexus:<DB_PASSWORD>@localhost:5432/nexus_gateway?sslmode=disable
NATS_URL=nats://localhost:4222
REDIS_MODE=standalone
REDIS_ADDRS=localhost:6379

AUTH_SERVER_URL=https://nexus.<DOMAIN>
AUTH_SERVER_JWKS_URL=https://nexus.<DOMAIN>/.well-known/jwks.json
AUTH_SERVER_ISSUER=https://nexus.<DOMAIN>
AUTH_SERVER_KEYSTORE_DIR=/var/lib/nexus/authkeys
AGENT_CA_DIR=/var/lib/nexus/agent-ca
EOF
[NEW] $ sudo chown nexus:nexus /etc/nexus-gateway/env
[NEW] $ sudo chmod 640 /etc/nexus-gateway/env
```

**Do not** put any secret in a yaml that gets committed to a repo —
the env file is the only place secrets live on disk on the host, and
even there it's `0640 root:nexus`.

---

## 8. Application binaries + systemd units

### 8.1 Binaries

Get the four `nexus-*` binaries by:

- Building from source: `GOOS=linux GOARCH=amd64 go build -o nexus-hub
  ./packages/nexus-hub/cmd/nexus-hub/` (and likewise for the other
  three), or
- Copying from an existing host: `scp ec2-user@<source>:/usr/local/bin/nexus-{hub,control-plane,ai-gateway,compliance-proxy} .`

Install them:

```bash
[NEW] $ sudo install -m 0755 nexus-hub /usr/local/bin/nexus-hub
[NEW] $ sudo install -m 0755 nexus-control-plane /usr/local/bin/nexus-control-plane
[NEW] $ sudo install -m 0755 nexus-ai-gateway /usr/local/bin/nexus-ai-gateway
[NEW] $ sudo install -m 0755 nexus-compliance-proxy /usr/local/bin/nexus-compliance-proxy
```

### 8.2 systemd units (4 files, one per service)

Write all four under `/etc/systemd/system/`. They differ only in the
`Description`, `ExecStart`, and the implicit `nexus-hub` start
ordering. Hub comes up first; the other three `After=` Hub but don't
hard-`Requires=` it (so they can stay up across a Hub restart).

```ini
# /etc/systemd/system/nexus-hub.service
[Unit]
Description=Nexus Hub
After=network.target postgresql.service docker.service nats.service
Requires=postgresql.service docker.service nats.service

[Service]
EnvironmentFile=-/etc/nexus-gateway/env
Type=simple
User=nexus
Group=nexus
ExecStart=/usr/local/bin/nexus-hub -config /etc/nexus/nexus-hub.yaml
Restart=on-failure
RestartSec=5s
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
```

`docker.service` is listed instead of a `valkey.service` because the
cache runs as a docker container (Section 5). systemd will start
`docker.service` before `nexus-hub` and the container's
`--restart unless-stopped` policy then ensures the container itself
is up. (If you later switch to a native cache service, change the
`Requires=` line accordingly.)

For the other three services, change `Description`, `ExecStart`, and
add `After=nexus-hub.service` so systemd boots them after Hub.
Example for the AI Gateway:

```ini
# /etc/systemd/system/nexus-ai-gateway.service
[Unit]
Description=Nexus AI Gateway
After=network.target postgresql.service docker.service nats.service nexus-hub.service
Requires=postgresql.service docker.service nats.service

[Service]
EnvironmentFile=-/etc/nexus-gateway/env
Type=simple
User=nexus
Group=nexus
ExecStart=/usr/local/bin/nexus-ai-gateway -config /etc/nexus/ai-gateway.yaml
Restart=on-failure
RestartSec=5s
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
```

No `GOMEMLIMIT` is set by default. On a memory-tight host (4 GB) you
can drop in `/etc/systemd/system/nexus-ai-gateway.service.d/gomemlimit.conf`
with `[Service]\nEnvironment=GOMEMLIMIT=512MiB` to cap a runaway, but
the trade-off is that Go starts forcing GC sooner — usually you want
the OS to be the limiter, not the runtime.

```bash
[NEW] $ sudo systemctl daemon-reload
[NEW] $ sudo systemctl enable nexus-hub nexus-control-plane nexus-ai-gateway nexus-compliance-proxy
```

---

## 9. nginx

Single nginx vhost file routes the three sub-hostnames + an
`/health` endpoint for upstream load-balancer health probes.

```bash
[NEW] $ sudo tee /etc/nginx/conf.d/nexus.conf > /dev/null <<'EOF'
# Health check for an upstream load balancer
server {
    listen 80 default_server;
    listen [::]:80 default_server;
    location /health { access_log off; return 200 'ok'; add_header Content-Type text/plain; }
    location /       { return 404; }
}

# Control Plane: SPA UI under /, REST under /api /oauth /idp /.well-known /healthz /metrics /ready /debug
server {
    listen 80; listen [::]:80;
    server_name nexus.<DOMAIN>;
    root /var/www/nexus-ui;
    index index.html;
    gzip on; gzip_vary on; gzip_proxied any;
    gzip_types text/plain text/css text/javascript application/javascript application/json image/svg+xml;
    client_max_body_size 50M;
    location ~* ^/assets/ { expires 1y; add_header Cache-Control "public, immutable"; access_log off; }
    location ~ ^/(api|oauth|authserver|idp|\.well-known|healthz|metrics|ready|debug)(/|$) {
        proxy_pass http://127.0.0.1:3001;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 120s; proxy_send_timeout 120s;
        proxy_buffering off; proxy_cache off;
    }
    # /downloads/ — static agent .pkg drop-in dir (preserved across deploys)
    location /downloads/ {
        alias /var/www/nexus-ui/downloads/;
        autoindex off;
        add_header Content-Disposition "attachment" always;
        add_header Cache-Control "no-store" always;
        types { application/octet-stream pkg; }
        default_type application/octet-stream;
    }
    location / {
        try_files $uri $uri/ /index.html;
        add_header Cache-Control "no-cache, no-store, must-revalidate";
    }
}

# AI Gateway — OpenAI-compatible /v1/*
server {
    listen 80; listen [::]:80;
    server_name api.<DOMAIN>;
    client_max_body_size 50M;
    location / {
        proxy_pass http://127.0.0.1:3050;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 300s; proxy_send_timeout 300s;
        proxy_buffering off;
    }
}

# Hub — REST + WebSocket
server {
    listen 80; listen [::]:80;
    server_name hub.<DOMAIN>;
    client_max_body_size 200M;
    location /ws {
        proxy_pass http://127.0.0.1:3060;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 3600s; proxy_send_timeout 3600s;
    }
    location / {
        proxy_pass http://127.0.0.1:3060;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 120s; proxy_send_timeout 120s;
    }
}
EOF
[NEW] $ sudo nginx -t
[NEW] $ sudo systemctl enable --now nginx
```

---

## 10. Control Plane UI (static SPA)

The CP UI is a Vite-built React app. Build artifacts go under
`/var/www/nexus-ui/` and are served by nginx (Section 9).

Two paths:

- **Build from source** in a checkout that matches the binaries:
  `npm install && npm run build -w packages/control-plane-ui`, then
  `sudo rsync -a --delete --exclude='/downloads' packages/control-plane-ui/dist/ /var/www/nexus-ui/`.
- **Copy from an existing host** (preserves the agent `.pkg`
  downloads directory):
  ```bash
  [OLD] $ sudo tar -czf /tmp/ui.tgz -C /var/www nexus-ui
  # Move the tarball over, then:
  [NEW] $ sudo rm -rf /var/www/nexus-ui
  [NEW] $ sudo tar -xzf /tmp/ui.tgz -C /var/www
  [NEW] $ sudo chown -R nginx:nginx /var/www/nexus-ui
  ```

The `/var/www/nexus-ui/downloads/` subdirectory holds the desktop
agent `.pkg` files served from `https://nexus.<DOMAIN>/downloads/NexusAgent-latest.pkg`.
The symlink `NexusAgent-latest.pkg` points to the current build —
preserve this on every UI deploy (the rsync `--exclude=/downloads`
above is exactly for this reason).

---

## 11. Start everything in the right order

Hub must reach a steady listener on `:3060` before the other three
services come up — they each open a WebSocket to Hub on boot to
register themselves.

```bash
# 1. Hub first; wait for the port
[NEW] $ sudo systemctl start nexus-hub
[NEW] $ for i in $(seq 1 30); do
          sudo ss -ltn 'sport = :3060' | grep -q ':3060' && { echo "hub up after ${i}s"; break; }
          sleep 1
        done

# 2. The other three
[NEW] $ sudo systemctl start nexus-control-plane nexus-ai-gateway nexus-compliance-proxy

# 3. Status snapshot
[NEW] $ sudo systemctl status nexus-hub nexus-control-plane nexus-ai-gateway nexus-compliance-proxy \
        --no-pager | grep -E 'Active:|Main PID'
```

---

## 12. Verification

### 12.1 All four services registered as Things in the DB

The `thing` table is the canonical service-registry. Within ~10 s of
boot, four new rows keyed by this host's hostname should appear:

```bash
[NEW] $ PGPASSWORD='<DB_PASSWORD>' psql -h localhost -U nexus -d nexus_gateway -c "
SELECT id, type, status, version, last_seen_at
FROM thing
WHERE id LIKE '%-' || (SELECT split_part(hostname, '.', 1)
                       FROM (SELECT 'ip-' || regexp_replace(inet_client_addr()::text, '\\.', '-', 'g') AS hostname) _) || '%'
   OR id = 'hub-test'
ORDER BY type;"
```

(Substitute the actual hostname pattern — the exact id format is
`<type>-<hostname>-<port>` for ai-gateway / control-plane,
`<type>-<hostname>` for compliance-proxy, and the global `hub.id`
for Hub. Make sure all four rows show `status='online'`.)

### 12.2 No startup errors

```bash
[NEW] $ sudo journalctl -u nexus-hub --since '1 min ago' --no-pager | grep -iE 'error|fatal|panic' | head
[NEW] $ sudo journalctl -u nexus-control-plane --since '1 min ago' --no-pager | grep -iE 'error|fatal|panic' | head
[NEW] $ sudo journalctl -u nexus-ai-gateway --since '1 min ago' --no-pager | grep -iE 'error|fatal|panic' | head
[NEW] $ sudo journalctl -u nexus-compliance-proxy --since '1 min ago' --no-pager | grep -iE 'error|fatal|panic' | head
```

An empty block on each is success. A few WARN lines about cache
warm-up are normal.

### 12.3 Edge endpoints reachable

```bash
[NEW] $ curl -s -o /dev/null -w 'CP /healthz HTTP %{http_code}\n' -H 'Host: nexus.<DOMAIN>' http://localhost/healthz
[NEW] $ curl -s -o /dev/null -w 'CP /ready   HTTP %{http_code}\n' -H 'Host: nexus.<DOMAIN>' http://localhost/ready
[NEW] $ curl -s -o /dev/null -w 'CP /        HTTP %{http_code}\n' -H 'Host: nexus.<DOMAIN>' http://localhost/
[NEW] $ curl -s -o /dev/null -w 'AI GW /    HTTP %{http_code}\n' -H 'Host: api.<DOMAIN>'   http://localhost/
[NEW] $ curl -s -o /dev/null -w 'Hub /      HTTP %{http_code}\n' -H 'Host: hub.<DOMAIN>'   http://localhost/
```

Expected: `200` on the CP `/healthz` and `/ready`; `200` on CP `/`;
the bare AI Gateway and Hub roots respond `404` (no handler on `/`
itself — that's intentional).

### 12.4 Audit pipeline alive

The Hub writes a `thing_diag_event` row when each service starts and
periodically thereafter. Within ~30 s of restart you should see new
rows:

```bash
[NEW] $ PGPASSWORD='<DB_PASSWORD>' psql -h localhost -U nexus -d nexus_gateway -c "
SELECT max(occurred_at) AS latest_diag,
       EXTRACT(EPOCH FROM (now() - max(occurred_at)))::int AS seconds_since_latest,
       count(*) FILTER (WHERE occurred_at >= now() - interval '10 minutes') AS rows_last_10min
FROM thing_diag_event;"
```

A `seconds_since_latest` under 60 s and `rows_last_10min` ≥ 4
indicates the audit pipeline (NATS → Hub consumer → PG) is healthy
end-to-end.

### 12.5 Semantic-cache index created

The Hub's `semantic-cache-reindex` job creates the
`nexus:semantic-cache:v2` vector index on its first run (within ~5 s
of Hub boot):

```bash
[NEW] $ sudo docker exec valkey redis-cli FT._LIST
nexus:semantic-cache:v2
[NEW] $ sudo docker exec valkey redis-cli FT.INFO nexus:semantic-cache:v2 | head -30
```

The schema should include a `vector` field (`type=VECTOR`,
`algorithm=HNSW`, `dim=3072`, `distance_metric=COSINE`) plus several
TAG fields (`upstream_provider`, `upstream_model`, `vk_scope`, …).

If the list is empty, check `sudo journalctl -u nexus-hub --since '1
min ago' --no-pager | grep -iE 'semantic|FT\.'` for the error. The
most common failures:

| Error in Hub log | Cause | Fix |
|---|---|---|
| `unknown command FT.CREATE` | Cache is plain `redis6` (no modules) | Switch to `redis-stack-server` (Section 5) |
| `Invalid field type for field … Unknown argument 'TEXT'` | Cache is `valkey/valkey-bundle:8-trixie` whose `valkey-search` module doesn't yet support TEXT fields | Switch to `redis-stack-server` (Section 5) |
| `RDB file contains AUX module data I can't load: no matching module 'Vk-Search'` | The `/var/lib/valkey/dump.rdb` was written by an earlier `valkey-bundle` container; redis-stack can't read it | `sudo rm -rf /var/lib/valkey/* && sudo docker restart valkey` |

---

## 13. Common failure patterns

| Symptom | Cause / fix |
|---------|-------------|
| All four services `Active: failed` on boot, `journalctl` shows "A dependency job for nexus-hub.service failed" | Unit `Requires=redis6.service` but you swapped to `valkey.service`. Edit each unit, run `daemon-reload`, restart. |
| `psql: ident authentication failed for user "nexus"` | `pg_hba.conf` still has the AL2023 default `ident` for local TCP. Section 4.2 fix. |
| Service comes up but every 5 s the Hub logs `unknown command FT.CREATE` | Section 5.2 was skipped or the `loadmodule` line is missing from `/etc/valkey/valkey.conf`. |
| `Unable to locate credentials` from any service trying to spill to S3 | The instance has no IAM role attached, or the role lacks `s3:PutObject` on `<S3_BUCKET>`. Section 1.2. |
| `Credential` rows from a restored prod dump won't decrypt; AI Gateway returns 503 on every provider | The `<CREDENTIAL_ENCRYPTION_KEY>` env doesn't match the one the source host used to encrypt those rows. Either restore the source `/var/lib/nexus/` directory (Section 7.2) and reuse its env keys, or re-enter every credential through the Control Plane UI. |
| Internal calls between services return 401 / 403 | One of `INTERNAL_SERVICE_TOKEN` / `ADMIN_KEY_HMAC_SECRET` / `CREDENTIAL_ENCRYPTION_KEY` differs across `/etc/nexus-gateway/env` versus a per-service yaml. These five values must match exactly in every place they appear. |
| `dmesg` shows OOM kills under load | Drop `--jobs=2` to `--jobs=1` for the valkey-search build (Section 5.2), or move PostgreSQL to a separate host. The 4 GB baseline has no headroom for `analyze` over large tables while the four services are serving traffic. |

---

## 14. Tear-down

```bash
[NEW] $ sudo systemctl stop nexus-compliance-proxy nexus-ai-gateway nexus-control-plane nexus-hub
[NEW] $ sudo systemctl stop nginx nats valkey postgresql
[NEW] $ sudo systemctl disable nexus-{compliance-proxy,ai-gateway,control-plane,hub} nats valkey nginx postgresql
[NEW] $ sudo rm -rf /etc/nexus /etc/nexus-gateway /etc/nats /etc/systemd/system/nexus-*.service /etc/systemd/system/nats.service
[NEW] $ sudo rm -rf /var/lib/nexus /var/lib/nats /var/lib/valkey /var/lib/pgsql
[NEW] $ sudo rm -rf /var/www/nexus-ui /var/log/nexus
[NEW] $ sudo dnf remove -y nexus-* postgresql15-server valkey nginx
```

That removes data permanently. Stop here unless you want to keep the
DB cluster for a re-install.
