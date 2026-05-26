# Deployment Cache MQ

Nexus Gateway uses two separate messaging infrastructure components: Valkey 8 (or Redis 7 as a drop-in) for caching, and NATS JetStream 2+ for event streaming. These are strictly separated — Valkey is pure cache (sessions, IAM, rate-limit counters, response cache, cert cache), and NATS carries bulk events (traffic, audit, ops metrics, alerts). There is no Redis pub/sub anywhere in the platform; cross-service config invalidation runs via Hub WebSocket change-signals, not the cache layer.

---

## Valkey / Redis — cache layer

### Role

Valkey (or Redis 7 as a drop-in) serves as a pure cache across all four Go services. Redis pub/sub is actively banned in the codebase — `scripts/check-no-redis-pubsub.mjs` blocks `.Publish(`, `.Subscribe(`, and `nexus:config*` channel literals.

| Use | Service | Key pattern |
|---|---|---|
| Admin sessions | Control Plane | `nexus:session:<sid>` |
| IAM policy cache | Control Plane | `nexus:iam:*` |
| Rate-limit / quota counters | AI Gateway | `nexus:quota:*` |
| Response cache (L1 extract + L2 semantic) | AI Gateway | `nexus:cache:resp:*` |
| Desired-state cache | Hub | `nexus:thing:desired:*` |
| Certificate cache | Compliance Proxy | `nexus:proxy:cert:<hostname>` |

All services degrade gracefully when Valkey is unavailable — config changes still propagate via Hub WebSocket, and each service falls back to in-memory caches with their respective TTLs.

### Valkey 8 vs Redis 7

Valkey 8 ships with the `valkey-search` module (`valkey/valkey-bundle` Docker image) providing RediSearch-compatible `FT.CREATE` / `FT.SEARCH` commands for HNSW vector indexing, used for the semantic response cache (L2). Redis 7 is a drop-in for everything except the semantic cache. Both use the same connection configuration.

License: Valkey core and valkey-search are BSD 3-Clause. Redis Stack (SSPL) is not used.

### Connection configuration

All four services consume the same `redis:` YAML block, materialized by `packages/shared/storage/redisfactory`. Three modes are supported:

```yaml
redis:
  mode: standalone           # standalone | sentinel | cluster
  addrs: ["redis.example.com:6379"]
  username: ""               # Redis 6+ ACL; blank for legacy AUTH
  password: ""               # override: REDIS_PASSWORD env var
  tls:
    enabled: false
    caFile: "/etc/nexus/redis/ca.pem"
  poolSize: 10
  dialTimeout: 5s
  readTimeout: 3s
  writeTimeout: 3s
```

Every YAML field has a corresponding `REDIS_<UPPER_SNAKE>` environment variable that takes precedence over the YAML value. See [Deployment-Environment-Variables](Deployment-Environment-Variables) for the full env catalog.

### HA options

**Sentinel (recommended for most deployments):**
Deploy 3 Sentinel nodes (can colocate with Redis replicas or application servers). Primary + 2 replicas minimum. Target failover time: under 10 seconds.

```yaml
redis:
  mode: sentinel
  addrs: ["sentinel1:26379", "sentinel2:26379", "sentinel3:26379"]
  sentinel:
    masterName: mymaster
```

**Cluster (larger deployments):**
Minimum 6 nodes (3 primaries + 3 replicas). Nexus Gateway's cache usage — cert cache, sessions, IAM, quota counters, response cache — is cluster-compatible (no cross-slot multi-key operations).

**Cloud managed:** AWS ElastiCache for Redis, GCP Memorystore, Azure Cache for Redis all work with the standard connection configuration.

### Memory sizing and eviction

For most deployments, 256 MB–1 GB is sufficient:

- Certificate cache: ~2–4 KB per entry (AES-256-GCM encrypted key + PEM chain). Typical enterprise: 50–200 unique AI provider hostnames.
- Session/IAM/quota caches: KB per row, bounded by ACL prefix.

Set `maxmemory-policy allkeys-lru` so certificate cache entries evict under pressure rather than causing write failures. Evicted certificates are re-created on demand.

### Cutover from Redis 7 to Valkey 8

Valkey 8 reads Redis 7 RDB files natively — the data volume is shared between containers with no explicit import step. The production cutover procedure (preflight, snapshot, stop services, swap container, verify, rollback) is documented in the [Valkey migration runbook](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/e61-valkey-migration.md). Estimated downtime: 30–60 seconds.

Key points for the cutover:
- Use `valkey/valkey-bundle:8-trixie` (not plain `valkey/valkey`) to get the search module.
- Mount the same data volume used by Redis so Valkey loads the existing RDB on startup.
- Do not use `MODULE LOAD` at runtime — the bundle image auto-discovers modules at startup.
- After cutover, verify `MODULE LIST` includes `search` before re-enabling semantic cache.

### Degradation without Redis

| Component | Behavior |
|---|---|
| Certificate cache | LRU-only (in-memory); higher CPU for cold hostnames |
| Sessions / IAM cache | Control Plane falls back to in-memory (single-instance only) |
| Hook shared state | Rate limiters fall back to per-instance local state; limits become approximate |
| Config invalidation | **Unaffected** — runs over Hub WebSocket, not Redis |

During a Redis outage, config changes take effect after each component's TTL expires (maximum 30 minutes for AI Gateway routing/credentials).

---

## NATS JetStream — event streaming

### Role

NATS JetStream carries bulk events across services. It is not the config-sync transport.

| Stream | Subjects | Retention | Purpose |
|---|---|---|---|
| `nexus.traffic` | `traffic.event.<thing_type>.*` | 24 h hot + Postgres durable | Per-request traffic events from AI Gateway, Compliance Proxy, Agent |
| `nexus.audit` | `audit.<event_type>.*` | 7 d hot + Postgres durable | Admin audit and agent audit events |
| `nexus.ops_metrics` | `ops.metrics.<thing_type>.*` | 24 h hot + Hub rollup | Per-service ops metrics samples |
| `nexus.alerts` | `alerts.<channel>.*` | 24 h hot + Hub alert inbox | Outbound alerts from rule evaluation |
| `nexus.heartbeat` | `heartbeat.<thing_id>` | 1 h | Heartbeats when WebSocket link is unavailable |

Stream definitions live in `packages/shared/transport/mq/streams.go` and are applied by Hub on boot.

### Hub-side consumers

Hub owns the durable pull consumers for each stream and forwards messages to downstream sinks:

| Consumer | Sink |
|---|---|
| `traffic-event-sink` | Postgres `traffic_event` + spillstore |
| `audit-sink` | Postgres `admin_audit` / `agent_audit` |
| `ops-metrics-rollup` | Postgres `ops_metrics_rollup` |
| `alert-dispatcher` | Webhook / SIEM / email channels |

On handler failure, the consumer NAKs and retries with backoff. After max retries, messages move to a DLQ subject and an alert fires.

### Installation

NATS is not in most Linux package managers (including Amazon Linux). Install from GitHub releases:

```bash
curl -LO https://github.com/nats-io/nats-server/releases/download/v2.10.24/nats-server-v2.10.24-linux-amd64.zip
unzip nats-server-v2.10.24-linux-amd64.zip
sudo mv nats-server-v2.10.24-linux-amd64/nats-server /usr/local/bin/
```

Run as a systemd service with `Restart=on-failure`. Services connect via `NATS_URL=nats://localhost:4222`.

### Capacity sizing

NATS JetStream stores messages in the configured retention window before downstream consumers acknowledge them. At 1 req/s, each traffic event is approximately 1–4 KB; 24-hour hot retention for the traffic stream uses roughly 350 MB at that rate. Scale linearly with request rate.

Producer failure handling: if a service cannot reach NATS, it buffers events in a local outbox and falls back to HTTP for critical audit events.

### MQ vs Hub WebSocket — the decision boundary

| Carrier | Use for |
|---|---|
| Hub WebSocket change-signal | Config-sync notifications (small payload, server-to-service, immediate) |
| HTTP (CP ↔ Hub) | Admin CRUD, shadow read/write, body overflow presign |
| MQ (NATS) | Bulk events: traffic, audit, ops metrics, alerts |

`metrics_sample` payloads travel over the WebSocket link from each service to Hub, not over NATS — the samples are KB-scale and benefit from the WebSocket link's low latency. See [`mq-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/foundation/mq-architecture.md) §6 for the full decision rationale.

---

## Canonical docs

- [`redis-setup.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/redis-setup.md) — connection config, ACL setup, degradation table, HA options, memory sizing
- [`mq-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/foundation/mq-architecture.md) — stream layout, subject taxonomy, consumer groups, failure modes, MQ vs WS decision
- [`valkey-migration runbook`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/e61-valkey-migration.md) — Redis 7 to Valkey 8 production cutover runbook

**Adjacent wiki pages**: [Deployment-Hardware-Sizing](Deployment-Hardware-Sizing) · [Deployment-Environment-Variables](Deployment-Environment-Variables) · [Deployment-Spillstore-Setup](Deployment-Spillstore-Setup) · [Storage-Cache-MQ-Stack](Storage-Cache-MQ-Stack)
