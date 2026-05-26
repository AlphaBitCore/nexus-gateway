# Redis Setup and Operations

## Role of Redis in Nexus Gateway

Redis is **pure cache only** in the Nexus Gateway system. There is no
Redis pub/sub anywhere in the platform — channel-based config
invalidation is forbidden and enforced by
`scripts/check-no-redis-pubsub.mjs` (lint blocks `.Publish(` /
`.Subscribe(` / `nexus:config*` channel literals).

Cross-service config invalidation runs via the Hub-shadow WebSocket
change-signal flow: Control Plane → Hub HTTP API → Hub updates the
Thing's desired-config shadow → WebSocket push → each service's
`thingclient.OnConfigChanged` callback. See
`docs/developers/architecture/cross-cutting/foundation/thing-config-sync-architecture.md`
for the canonical contract.

| Purpose | Used By | Key Pattern |
|---------|---------|------------|
| **Sessions** | Control Plane | `nexus:session:<sid>` |
| **IAM policy cache** | Control Plane | `nexus:iam:*` |
| **Rate-limit / quota counters** | AI Gateway | `nexus:quota:*` |
| **Response cache** | AI Gateway | `nexus:cache:resp:*` (Tier-1 prompt cache; E61 Tier-2 semantic cache uses Valkey-search keys) |
| **Desired-state cache** | Hub | `nexus:thing:desired:*` |
| **Certificate cache** | Compliance proxy | `nexus:proxy:cert:<hostname>` |

Redis is **not** on the critical path. All services degrade gracefully when Redis is unavailable (see Degradation section below).

---

## Connection Configuration

### Universal Redis configuration

Every Nexus Gateway service (Hub, Control Plane, AI Gateway, Compliance
Proxy) consumes the same `redis:` yaml block, materialised by
`packages/shared/storage/redisfactory`. Three deployment modes are
supported behind a single Config: `standalone`, `sentinel`, `cluster`.
ACL credentials (Redis 6+) and TLS / mTLS are first-class fields.

```yaml
redis:
  mode: standalone
  addrs: ["redis.example.com:6379"]
  username: ""              # Redis 6+ ACL; blank for legacy AUTH flow
  password: ""              # env REDIS_PASSWORD overrides
  db: 0
  sentinel:
    masterName: ""
    username: ""
    password: ""
  cluster:
    maxRedirects: 8
    routeRandomly: false
    readOnly: false
  tls:
    enabled: false
    insecureSkipVerify: false
    caFile: "/etc/redis/ca.crt"
    certFile: "/etc/redis/client.crt"
    keyFile: "/etc/redis/client.key"
    serverName: ""
  poolSize: 10
  minIdleConns: 0
  maxRetries: 3
  dialTimeout: 5s
  readTimeout: 3s
  writeTimeout: 3s
  poolTimeout: 4s
```

Every field has an env override of the form `REDIS_<UPPER_SNAKE>` (env
wins over yaml per the project's L3>L2 precedence). See `.env.example`
at repo root for the full env catalog, or
`docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md` §9 for the
authoritative contract.

---

## ACL Configuration

For production, disable the default Redis user and create scoped accounts:

```
# Apply via redis.conf: aclfile /path/to/redis-acl.conf

# Compliance proxy account
# - Read/write cert cache only (Redis is cache-only)
user nexus-proxy on >CHANGE_PASSWORD ~nexus:proxy:* ~nexus:hook:* +@read +@write +ping +info

# Control plane account
# - Read/write sessions + IAM cache + admin state
user nexus-control on >CHANGE_PASSWORD ~nexus:cache:* ~nexus:admin:* ~nexus:session:* ~nexus:iam:* +@read +@write +ping +info +expire +ttl

# Disable default user
user default off
```

---

## Degradation Without Redis

All services continue operating when Redis is unavailable:

| Component | Behavior Without Redis |
|-----------|----------------------|
| **Certificate cache** | LRU-only (in-memory). Cold hostnames require on-the-fly signing. Higher CPU usage. No cross-instance cache sharing. |
| **Sessions / IAM cache** | Control Plane falls back to in-memory sessions + IAM cache (single-instance only). |
| **Hook shared state** | Rate limiters and counters fall back to per-instance local state. Limits become approximate. |
| **Config invalidation** | Unaffected by Redis state — invalidation runs over the Hub-shadow WebSocket, not Redis. |

### Monitoring Degradation

| Metric | Meaning |
|--------|---------|
| `nexus_compliance_proxy_redis_available` | 1 = connected, 0 = unreachable |
| `nexus_compliance_proxy_config_staleness_seconds` | Seconds since last config refresh (per category) |

---

## Per-Component Cache TTL Degradation

When Redis is unavailable, each component continues serving from its local cache
with the following TTL windows before data becomes stale:

| Component | Config Category | Cache TTL (no Redis) | Mechanism |
|-----------|----------------|---------------------|-----------|
| **AI Gateway** | Hook configs | 2 min | `ConfigLoader` TTL-based refresh |
| **AI Gateway** | Routing rules | 30 min | In-memory snapshot with periodic reload |
| **AI Gateway** | Credentials | 30 min | In-memory snapshot with periodic reload |
| **Compliance Proxy** | Hook configs | 5 min | `configcache.Manager` TTL |
| **Compliance Proxy** | Domain allowlist | 5 min | `configcache.Manager` TTL |
| **Compliance Proxy** | Certificates | LRU eviction | In-memory LRU cache |
| **Agent** | All config | 300s (default) | `ConfigRefreshSec` polling interval |
| **Control Plane** | IAM policies | 10s (L1) / 60s (L2) | L1 process cache, L2 Redis cache |

**Impact:** During a Redis outage, config changes made in the Control Plane
take effect only after each component's TTL expires and it reloads from the DB.
The maximum staleness equals the longest TTL above (30 min for AI Gateway
routing/credentials).

---

## HA Setup

### Redis Sentinel (Recommended)

Deploy 3 Sentinel nodes (can colocate with Redis replicas or application servers):

1. Primary + 2 replicas minimum.
2. Configure `down-after-milliseconds` (e.g. 5000ms).
3. Configure `failover-timeout` (e.g. 30000ms).
4. Application connects via Sentinel-aware client for automatic primary discovery.

Target failover time: under 10 seconds.

### Redis Cluster

For larger deployments:

- Minimum 6 nodes (3 primaries + 3 replicas).
- Provides automatic sharding and failover.
- Nexus Gateway Redis usage (cert cache, session/IAM/quota counters, response cache) is cluster-compatible (no cross-slot multi-key operations).

### Cloud Managed

| Provider | Service |
|----------|---------|
| AWS | ElastiCache for Redis |
| GCP | Memorystore for Redis |
| Azure | Azure Cache for Redis |

---

## Memory Sizing

Redis memory usage depends on:

- **Certificate cache**: Each entry is approximately 2-4 KB (encrypted key + PEM chain). Size = number of unique AI provider hostnames x entry size. Typical enterprise: 50-200 entries.
- **Session / IAM / quota caches**: KB per row, bounded by ACL prefix.

For most deployments, a Redis instance with 256 MB - 1 GB of memory is sufficient.

### Eviction Policy

Set `maxmemory-policy allkeys-lru` so that certificate cache entries are evicted under memory pressure rather than causing write failures. Evicted entries are re-created on demand.

---

## Development Setup

The `docker-compose.yml` provides a Redis instance for local development:

```bash
docker compose up -d redis
```

- Port: 6437 (mapped from container port 6379)
- No password
- No TLS

Connect from services — every service consumes the universal Redis
schema. The dev yaml files ship with `addrs: ["localhost:6437"]` baked
in; the same can be set per-environment via `REDIS_ADDRS=localhost:6437`.

---

## Monitoring Checklist

| Check | Threshold | Severity |
|-------|-----------|----------|
| Connection reachability | Unreachable > 30s | High |
| Memory usage | > 80% of `maxmemory` | High |
| Connected clients | > 80% of `maxclients` | Warning |
| Eviction rate | > 0 (unexpected) | Warning |
| Cert cache hit rate | < 50% sustained | Warning |
| Config staleness | > 1 hour | High |
