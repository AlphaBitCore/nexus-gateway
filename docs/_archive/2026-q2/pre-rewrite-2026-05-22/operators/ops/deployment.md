# Deployment Guide

## Service Topology

```
                         Internet (AI Providers)
                                  ^
                                  |
                    +-------------+-------------+
                    |                           |
             +------+-------+          +-------+--------+
             | AI Gateway   |          | Compliance     |
             | (Go :3050)   |          | Proxy (Go)     |
             | /v1 explicit |          | :3128 CONNECT  |
             | proxy        |          | :3040 runtime  |
             +--------------+          +----------------+
                    |                           |
                    +-----+---------------------+
                          |
                    +-----+--------+
                    |  Nexus Hub   |  Platform Ops Center
                    |  (Go :3060)  |  Thing Registry, Shadow,
                    |              |  Jobs, MQ Consumers, CA
                    +------+-------+
                           |
              +------------+------------+
              |            |            |
        WebSocket         MQ       HTTP API
        (control)     (events)    (config)
              |            |            |
       +------+---+  +----+----+  +----+----+
       | CP  | AG |  | NATS JS |  | CP API  |
       | Proxy|Agt|  +---------+  +---------+
       +------+---+
              |
       +------+------+
       | Control Plane|  Admin API / BFF (port 3001)
       | UI (3000)    |  → proxies to Hub for Thing/config ops
       +------+-------+
              |
       +------+---+-------+
       |          |        |
    +--+---+ +---+---+ +--+---+
    | PG   | | Redis | | NATS |
    +------+ +-------+ +------+
```

### Services

| Service | Package | Default Port | Protocol | Purpose |
|---------|---------|-------------|----------|---------|
| Nexus Hub | `packages/nexus-hub` | 3060 | HTTP + WebSocket | Thing Registry, Device Shadow, config sync, scheduled jobs, MQ consumers, Agent CA, SIEM bridge |
| Control Plane | `packages/control-plane` | 3001 | HTTP | Admin API / BFF, IAM, SSO, credential vault, calls Hub HTTP API |
| Control Plane UI | `packages/control-plane-ui` | 3000 | HTTP | React dashboard (Vite dev, Nginx prod) |
| AI Gateway | `packages/ai-gateway` | 3050 | HTTP | /v1 AI proxy with VK auth, provider adapters, routing, compliance hooks |
| Compliance Proxy | `packages/compliance-proxy` | 3128 / 3040 | CONNECT / HTTP | Transparent TLS proxy for AI traffic compliance |
| Desktop Agent | `packages/agent` | N/A | N/A | Per-device traffic interception, enrolls with Hub |

### Dependencies

| Service | PostgreSQL | Redis | NATS JetStream | Hub |
|---------|-----------|-------|---------------|-----|
| Nexus Hub | Yes (primary) | Yes (cache) | Yes (consume + produce) | — |
| Control Plane | Yes (primary) | Yes (sessions, IAM cache) | Yes (audit publish) | Yes (HTTP API for config) |
| AI Gateway | Yes (config reads) | Yes (rate limit, quota, cache) | Yes (traffic events) | Yes (WebSocket shadow) |
| Compliance Proxy | Yes (audit writes) | Yes (cert cache) | Yes (compliance events) | Yes (WebSocket shadow) |
| Agent | No | No | No | Yes (WebSocket + HTTP) |
| Control Plane UI | No | No | No | No (connects to CP API) |

---

## Infrastructure Requirements

### PostgreSQL 16+
- Primary datastore for all services
- Prisma schema source of truth (`tools/db-migrate/schema.prisma`)
- Dev: Docker Compose on port 55532

### Redis 7+
- Pure cache: sessions, IAM, rate limiting, quota counters, desired state
- No pub/sub (replaced by WebSocket + MQ)
- Dev: Docker Compose on port 6437

### NATS JetStream 2+
- Event streaming: traffic, compliance, agent, admin-audit, metrics, hub signals
- Required for Hub consumers to function
- Dev: Docker Compose on port 4222

---

## Environment Variables

### Nexus Hub

| Variable | Description | Default |
|----------|-------------|---------|
| `DATABASE_URL` | PostgreSQL connection string | (required) |
| `REDIS_*` | Universal Redis configuration (see `redis-setup.md` + `packages/shared/storage/redisfactory`). Minimal: `REDIS_MODE=standalone`, `REDIS_ADDRS=host:6379`. | (required) |
| `INTERNAL_SERVICE_TOKEN` | Shared secret for inter-service auth | (required in prod) |
| `MQ_DRIVER` | Message queue driver (`nats`) | `nats` |
| `NATS_URL` | NATS server URL | `nats://localhost:4222` |
| `SCHEDULER_ENABLED` | Enable scheduled jobs | `true` |
| `LOG_LEVEL` | Log level | `info` |

### Control Plane

| Variable | Description | Default |
|----------|-------------|---------|
| `DATABASE_URL` | PostgreSQL connection string | (required) |
| `REDIS_*` | Universal Redis configuration (see `redis-setup.md`). Empty `REDIS_ADDRS` disables Redis — CP falls back to in-memory sessions + IAM cache. | (optional) |
| `ADMIN_BOOTSTRAP_KEY` | Initial admin API key | (empty) |
| `ALLOW_DEV_AUTH` | Dev-mode auth bypass | `false` |
| `ADMIN_KEY_HMAC_SECRET` | HMAC key for API key hashing | (required in prod) |
| `CREDENTIAL_ENCRYPTION_KEY` | AES encryption key for credentials | (required in prod) |
| `INTERNAL_SERVICE_TOKEN` | Token for Hub API calls | (required in prod) |
| `NEXUS_HUB_URL` | Hub HTTP API URL | `http://localhost:3060` |
| `MQ_DRIVER` | Message queue driver | `nats` |
| `NATS_URL` | NATS server URL | `nats://localhost:4222` |
| `NODE_ENV` | Environment (`production` for security enforcement) | (empty) |
| `LOG_LEVEL` | Log level | `info` |
| `AI_GATEWAY_INTERNAL_URL` | Ai-gateway base URL for control-plane AI Guard dry-run proxy | `http://ai-gateway:3050` |

### AI Gateway

| Variable | Description | Default |
|----------|-------------|---------|
| `DATABASE_URL` | PostgreSQL connection string | (required) |
| `REDIS_*` | Universal Redis configuration (see `redis-setup.md`). Empty `REDIS_ADDRS` disables Redis — ai-gateway falls back to local-only response cache + in-memory rate limiter. | (optional) |
| `ADMIN_KEY_HMAC_SECRET` | HMAC key for VK hashing | (required in prod) |
| `CREDENTIAL_ENCRYPTION_KEY` | AES key for provider credentials | (required in prod) |
| `NEXUS_HUB_URL` | Hub URL for thingclient | (empty) |
| `INTERNAL_SERVICE_TOKEN` | Token for Hub WebSocket auth | (required in prod) |
| `MQ_DRIVER` | Message queue driver | `nats` |
| `NATS_URL` | NATS server URL | `nats://localhost:4222` |
| `AI_GATEWAY_PORT` | HTTP listener port | `3050` |
| `NODE_ENV` | Environment | (empty) |

### AI Guard (P-B)

`/v1/ai-guard/classify` is an internal service-to-service endpoint mounted
on ai-gateway. It is called by compliance-proxy in-process hooks
(semantic prompt-injection / jailbreak / secret-leak judgments) and by
the control-plane dry-run proxy. It is NOT a customer-facing surface.

- `INTERNAL_SERVICE_TOKEN` — the platform-wide internal service token
  (also used for CP↔Hub, CP↔/internal/* on ai-gateway, etc.) gates
  `/v1/ai-guard/classify` as well. Compliance-proxy and control-plane
  (for the dry-run proxy) send this value via the `X-RS-Token` header.
  Empty value on ai-gateway → endpoint returns 503
  `RS_TOKEN_NOT_CONFIGURED` on every call. Must be set to the **same**
  value on all three services (ai-gateway, control-plane,
  compliance-proxy).
- `AI_GATEWAY_INTERNAL_URL` — URL the control-plane uses to reach
  ai-gateway for the dry-run proxy (`Settings → AI Guard Backend → Dry-run`
  panel). Default `http://ai-gateway:3050`. Only consumed by the
  control-plane process.

Backend configuration itself (backend mode, provider/model, prompt
template, timeout, cache TTL) lives in the `ai_guard_config` singleton
row and is edited via `Settings → AI Guard Backend`, not env vars.
Updates flow: control-plane PUT → Nexus Hub HTTP API → Hub updates the
relevant Thing's desired-config shadow → WebSocket push to every
ai-gateway → `thingclient.OnConfigChanged` callback applies the new
ai_guard_config in-process. Redis pub/sub is **not** used (banned by
the "Redis cache only" binding). Ai-gateways with a transiently
disconnected WebSocket still converge on the next reconnect or the
2-minute TTL refresh from Postgres.

---

## Docker Deployment

### Available Dockerfiles

| Service | Dockerfile | Runtime Image | User | Ports |
|---------|-----------|--------------|------|-------|
| Nexus Hub | `packages/nexus-hub/Dockerfile` | alpine:3.21 | root | 3060 |
| Control Plane | `packages/control-plane/Dockerfile` | alpine:3.21 | nonroot | 3001 |
| AI Gateway | `packages/ai-gateway/Dockerfile` | alpine:3.21 | nonroot | 3050 |
| Compliance Proxy | `packages/compliance-proxy/Dockerfile` | distroless | nonroot | 3128, 9090 |
| Control Plane UI | `packages/control-plane-ui/Dockerfile` | nginx | — | 3000 |

All Go Dockerfiles use go.work workspace builds to ensure shared module consistency.

### Building Images

```bash
# All Go service builds must be run from the repo root
docker build -t nexus-hub:latest -f packages/nexus-hub/Dockerfile .
docker build -t nexus-control-plane:latest -f packages/control-plane/Dockerfile .
docker build -t nexus-ai-gateway:latest -f packages/ai-gateway/Dockerfile .
docker build -t nexus-compliance-proxy:latest -f packages/compliance-proxy/Dockerfile .
docker build -t nexus-control-plane-ui:latest -f packages/control-plane-ui/Dockerfile .
```

### Development with Docker Compose

```bash
docker compose up -d   # Starts PostgreSQL, Redis, NATS
```

This starts:
- PostgreSQL on port 55532 (mapped from 5432)
- Redis on port 6437 (mapped from 6379)
- NATS JetStream on port 4222 (monitoring on 8222)

Services run outside Docker in development:
```bash
cd packages/nexus-hub && go run ./cmd/nexus-hub/              # port 3060
cd packages/control-plane && go run ./cmd/control-plane/      # port 3001
npm run dev:control-plane-ui                                   # port 3000
cd packages/ai-gateway && go run ./cmd/ai-gateway/            # port 3050
cd packages/compliance-proxy && go run ./cmd/compliance-proxy/ # port 3040
```

---

## Startup Order

1. **Infrastructure**: PostgreSQL → Redis → NATS
2. **Database migrations**: `cd tools/db-migrate && npx prisma migrate deploy`
3. **Nexus Hub** (must start before other services for Thing registration)
4. **Control Plane** (registers with Hub, serves admin API)
5. **AI Gateway** (registers with Hub, serves /v1 API)
6. **Compliance Proxy** (registers with Hub, serves CONNECT proxy)
7. **Control Plane UI** (connects to CP API on port 3001)
8. **Agents** (enroll with Hub, connect via WebSocket)

---

## Health Check Endpoints

| Service | Endpoint | Port | Response |
|---------|----------|------|----------|
| Nexus Hub | `GET /healthz` | 3060 | `{"status":"ok"}` |
| Nexus Hub | `GET /readyz` | 3060 | `{"database":"ok","redis":"ok","consumers":"ok"}` |
| Control Plane | `GET /healthz` | 3001 | `{"status":"ok"}` |
| AI Gateway | `GET /healthz` | 3050 | `{"status":"ok","service":"ai-gateway"}` |
| Compliance Proxy | `GET /healthz` | 9090 | `{"status":"ok"}` or `{"status":"shutting_down"}` (503) |
| All Go services | `GET /metrics` | — | Prometheus metrics |

---

## Scheduled Jobs

Jobs are run by the **Nexus Hub scheduler** (not Control Plane). With `pg_try_advisory_lock`, only one Hub instance runs each job in multi-instance deployments.

| Job | Interval | Description |
|-----|----------|-------------|
| Drift Detector | 30s | Checks shadow desired vs reported, auto-pushes config |
| Identity Enricher | 5m | Backfills traffic_event identity from session/VK data |
| Enrollment Token Cleanup | 1h | Deletes expired enrollment tokens |

The Control Plane also runs optional jobs (retention, SIEM bridge, alert evaluator) when `scheduler.enabled: true`.
