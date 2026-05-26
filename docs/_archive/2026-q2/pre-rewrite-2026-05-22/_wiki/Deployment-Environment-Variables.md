# Deployment Environment Variables

Nexus Gateway services are configured entirely through environment variables and YAML files. Secrets — authentication tokens, HMAC keys, encryption keys, database passwords — live **only in environment variables, never in committed YAML**. YAML carries service-shape and non-secret tunings (ports, timeouts, feature flags, log levels). The single source of truth for every environment variable name and description is [`.env.example`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/.env.example) at the repo root. This page catalogs the most important variables and explains the `[MUST MATCH]` cross-service constraint that causes the majority of post-deploy 403 errors.

---

## The `[MUST MATCH]` constraint

Several secrets must be identical across multiple services. Drift between services is the most common source of 403 errors after a fresh deploy or key rotation. These variables are tagged `[MUST MATCH]` in `.env.example`.

Generate each secret once and paste the same value into every service that consumes it:

```bash
openssl rand -hex 32   # 64-character hex string; suitable for token and key fields
```

| Variable | Services | What it does | Drift consequence |
|---|---|---|---|
| `INTERNAL_SERVICE_TOKEN` | Hub, CP, AI Gateway, Compliance Proxy | Bearer token on inter-service HTTP calls and Hub WebSocket registration; also gates `/v1/ai-guard/classify` | Hub rejects Thing registration with `401`; Compliance Proxy admin passthrough returns `403` |
| `ADMIN_KEY_HMAC_SECRET` | Control Plane, AI Gateway | HMAC-SHA256 key used to hash Virtual Keys and Admin API keys before DB storage | VK validation fails; rotating this value invalidates all existing Virtual Keys |
| `CREDENTIAL_ENCRYPTION_KEY` | Control Plane, AI Gateway | 64-hex-char AES-256-GCM master key that encrypts provider credentials at rest | AI Gateway cannot decrypt stored provider keys; all provider calls fail with decryption errors |
| `AUTH_SERVER_ISSUER` | Control Plane (issuer), Hub (verifier) | OIDC issuer URL embedded in JWT tokens; Hub rejects tokens whose issuer does not match | Admin login succeeds but Hub WebSocket upgrades fail with `401` |
| `COMPLIANCE_PROXY_API_TOKEN` | Control Plane, Compliance Proxy | Bearer for Compliance Proxy `/runtime/*` admin endpoints | Admin UI proxy operations return `401` |

---

## Required variables

Every production node must have these set before any service starts. Services fail fast on missing required variables.

### Infrastructure URLs

```bash
# PostgreSQL DSN — all four services
DATABASE_URL=postgresql://nexus:PASSWORD@localhost:5432/nexus_gateway

# Valkey/Redis — all four services
REDIS_MODE=standalone        # standalone | sentinel | cluster
REDIS_ADDRS=localhost:6379

# NATS JetStream — all four services
NATS_URL=nats://localhost:4222

# Hub registration URL — read by AI Gateway, Control Plane, Compliance Proxy
NEXUS_HUB_URL=http://127.0.0.1:3060
```

### OIDC / auth server (Control Plane + Hub)

```bash
AUTH_SERVER_URL=https://nexus.example.com
AUTH_SERVER_ISSUER=https://nexus.example.com      # [MUST MATCH CP <-> Hub]
AUTH_SERVER_JWKS_URL=https://nexus.example.com/.well-known/jwks.json
```

### Service-to-service URLs (Control Plane BFF only)

```bash
AI_GATEWAY_URL=http://127.0.0.1:3050
COMPLIANCE_PROXY_URL=http://127.0.0.1:3040
COMPLIANCE_PROXY_RUNTIME_URL=http://127.0.0.1:3040
```

---

## Secrets (env-only, never YAML)

```bash
# [MUST MATCH all 4 services]
INTERNAL_SERVICE_TOKEN=<openssl rand -hex 32>

# [MUST MATCH Control Plane <-> AI Gateway]
ADMIN_KEY_HMAC_SECRET=<openssl rand -hex 32>
CREDENTIAL_ENCRYPTION_KEY=<openssl rand -hex 32>   # 64 hex chars = 32 bytes

# Control Plane only
COMPLIANCE_PROXY_API_TOKEN=<openssl rand -hex 32>

# AI Gateway only (optional — disables /runtime/* if empty)
AI_GATEWAY_API_TOKEN=<openssl rand -hex 32>
```

Multi-key credential rotation is supported via:

```bash
CREDENTIAL_KEY_MAP=v1:<hex64>,v2:<hex64>   # takes precedence over CREDENTIAL_ENCRYPTION_KEY
```

---

## Optional tuning knobs

These have sensible defaults baked into YAML and code. Override only when the default diverges from the deployment's needs.

### Redis advanced options

```bash
# Auth (Redis 6+ ACL; blank for legacy AUTH-only)
# REDIS_USERNAME=
# REDIS_PASSWORD=
# REDIS_DB=0

# Sentinel (when REDIS_MODE=sentinel)
# REDIS_SENTINEL_MASTER_NAME=mymaster

# Cluster (when REDIS_MODE=cluster)
# REDIS_CLUSTER_MAX_REDIRECTS=8

# TLS/mTLS
# REDIS_TLS_ENABLED=false
# REDIS_TLS_CA_FILE=/etc/nexus/redis/ca.pem

# Pool tuning
# REDIS_POOL_SIZE=10
# REDIS_DIAL_TIMEOUT=5s
# REDIS_READ_TIMEOUT=3s
```

### Per-service knobs

```bash
# Hub
# NEXUS_HUB_PORT=3060
# NEXUS_HUB_SCHEDULER_ENABLED=true

# Control Plane
# CONTROL_PLANE_PORT=3001
# CONTROL_PLANE_CRYPTO_PRODUCTION=true   # enforces key-length validation
# NODE_ENV=production

# AI Gateway
# AI_GATEWAY_PORT=3050
# AI_GATEWAY_CACHE_ENABLED=true
# AI_GATEWAY_CACHE_TTL=5m

# Compliance Proxy
# COMPLIANCE_PROXY_PORT=3040
```

### Shared operational knobs

```bash
# Logging
# LOG_LEVEL=info     # debug | info | warn | error
# LOG_FORMAT=json

# OpenTelemetry
# OTEL_ENDPOINT=http://otel-collector:4318
# OTEL_SERVICE_NAME=nexus-ai-gateway

# MQ driver (only NATS supported today)
# MQ_DRIVER=nats
```

---

## How services load variables

**Local development:** copy `.env.example` to `.env` at the repo root and fill in the `CHANGE_ME_*` placeholders. `bootenv.LoadFromRepoRoot()` is called at service startup and auto-loads the `.env` file — no `source .env` needed. A one-off override takes precedence: `LOG_LEVEL=debug go run ./cmd/ai-gateway/`.

**Production (systemd):** each service unit has `EnvironmentFile=/etc/nexus-gateway/env`. The application does not read `.env` in production — variables are injected by the deployment system before the binary starts.

**Kubernetes:** use `envFrom: [{secretRef: {name: nexus-secrets}}]` in the pod spec. The same binding applies: secrets in a `Secret` object, non-secrets in a `ConfigMap`.

Precedence rule (godotenv non-overload): existing process env > `.env` file values. A variable already set in the process environment (e.g., via systemd `EnvironmentFile`) is never overwritten by the `.env` file.

The agent CA directory is shared between Hub (which mints agent certs) and Control Plane (which reads them for admin UI introspection):

```bash
AGENT_CA_DIR=/var/lib/nexus/agentca
AGENT_CA_CERT_FILE=/etc/nexus/agentca/ca.crt
AGENT_CA_KEY_FILE=/etc/nexus/agentca/ca.key
```

---

## Canonical docs

- [`.env.example`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/.env.example) — single source of truth: every variable name, description, and default
- [`deployment.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/deployment.md) — per-service variable tables and Docker deployment options
- [`local-dev-debugging.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/local-dev-debugging.md) — `bootenv` contract, `tests/.env.<target>` loader, and service log paths

**Adjacent wiki pages**: [Deployment-Single-Node-Production](Deployment-Single-Node-Production) · [Deployment-TLS-Certificates](Deployment-TLS-Certificates) · [Deployment-Cache-MQ](Deployment-Cache-MQ) · [Security-Secrets-Handling](Security-Secrets-Handling)
