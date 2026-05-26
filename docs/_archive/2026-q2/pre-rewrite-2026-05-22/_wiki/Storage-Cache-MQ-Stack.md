# Storage Cache MQ Stack

*Audience: contributors and operators who need to understand what Nexus stores where, how caches invalidate, and how events flow.*

Nexus Gateway uses three storage systems — PostgreSQL for durable data, Valkey (Valkey 8, a BSD-3 Redis-compatible store) as pure cache, and NATS JetStream as the message queue for bulk events. Each system has a strictly bounded role. Valkey is **not** used for pub/sub anywhere in the system; config invalidation signals travel over the Hub WebSocket channel. NATS carries traffic events, audit, and alerts — not config sync.

---

## PostgreSQL — the durable store

PostgreSQL is the single source of truth for all persistent business data:

- **Traffic events** (`traffic_event`, `traffic_event_payload`) — every AI request handled by any of the three traffic paths.
- **Admin audit** (`admin_audit`) — every admin action with before/after state.
- **Thing registry** (`thing`, `thing_service`, `thing_agent`) — the node registry, shadow blobs, and drift state.
- **Config templates** (`thing_config_template`, `thing_config_override`) — L4 operational policies.
- **IAM data** — organizations, projects, users, policies, virtual keys, credentials.
- **Job queue** — Hub scheduler jobs, reconcile state.

Nexus uses hand-written SQL via `pgx`/`pgxpool` at runtime (no `sqlc`). Prisma manages schema migrations; Go struct mirrors are hand-maintained under `packages/shared/schemas/configtypes/`. Pool configuration is uniform across services:

```yaml
database:
  url: ""          # env DATABASE_URL
  maxConns: 50
  minConns: 10
  maxConnLifetime: 30m
```

## Valkey — pure cache, no pub/sub

The cache store is **Valkey 8** (BSD-3 licensed, Redis-compatible with vector-search support). Valkey is pure cache — it holds no authoritative state. Adding a new "Redis pub/sub for X" pattern is forbidden. Config invalidation signals go over the Hub WebSocket change-signal, not a pub/sub channel.

Valkey holds:

| Cache | Eviction | Notes |
|---|---|---|
| Admin UI sessions | TTL (session duration) | Short-lived bearer tokens |
| IAM policy cache | TTL 60s | Per-principal; acceptable 60s stale window |
| Rate-limit counters | Sliding window | Per-VK, per-org |
| Quota counters | Sliding window (Lua-scripted) | Per-scope, per-window |
| Response cache — Extract tier (L1) | TTL + LRU | AI Gateway exact-match response cache |
| Response cache — Semantic tier (L2) | TTL + LRU (HNSW KNN) | AI Gateway vector-similarity cache (valkey-search) |
| Desired-state snapshot | TTL + change-signal | Per-Thing shadow snapshot for fast lookup |
| Leaf cert cache (Compliance Proxy) | LRU + cert validity | In-proc LRU + Valkey two-tier |
| Credstate (decrypted credentials) | Dirty-set | Lazy refresh on credential update |

Valkey client configuration uses a universal schema supporting `standalone`, `sentinel`, and `cluster` modes, plus ACL and TLS. The factory lives in `packages/shared/storage/redisfactory/`. The key env variables are `REDIS_MODE`, `REDIS_ADDRS` (comma-separated), `REDIS_PASSWORD` (never in yaml), and `REDIS_SENTINEL_MASTER_NAME` for sentinel mode.

### Invalidation patterns

| Pattern | Mechanism | Example |
|---|---|---|
| **TTL** | Natural expiry | IAM policy (60s), session |
| **Hub WS change-signal (full refresh)** | `OnConfigChanged` callback, reload entire cache | Provider catalog, IAM action catalog |
| **Hub WS change-signal (per-key)** | Signal includes affected key id; service evicts one entry | Routing-rule cache |
| **Dirty-set (lazy refresh)** | Mark entry dirty; next access fetches fresh | Credstate after credential rotation |
| **Implicit** | Entry carries its own expiry | Leaf cert cache |

When Valkey is down, in-process caches continue working. Valkey-backed caches fail-open: lookups return miss, requests continue to upstream. No data is lost; only cache hit-rate degrades.

## NATS JetStream — bulk events

NATS JetStream carries all bulk events between services and Hub. It is not used for config sync (that is Hub WebSocket). The MQ interface in `packages/shared/transport/mq/` is deliberately minimal:

```go
type Producer interface { Publish(ctx, subject, payload, headers) (Ack, error) }
type Consumer interface { Subscribe(ctx, subject, handler) error }
type Driver   interface { Producer(name) (Producer, error); Consumer(name) (Consumer, error) }
```

Stream layout:

| Stream | Subjects | Retention |
|---|---|---|
| `nexus.traffic` | `nexus.event.ai-traffic`, `.compliance`, `.agent` | 24 h hot + Postgres durable |
| `nexus.audit` | `nexus.event.admin-audit` | 7 d hot + Postgres durable |
| `nexus.ops_metrics` | `nexus.event.diag` | 24 h hot |
| `nexus.alerts` | `nexus.event.alert` | 24 h hot |
| `nexus.heartbeat` | `heartbeat.<thing_id>` | 1 h |

Stream definitions are in `packages/shared/transport/mq/streams.go`; Hub applies them on boot.

Every NATS message carries a structured envelope with `event_id` (UUID v7, dedup key), `trace_id`, `request_id`, `schema_version`, `emitted_at`, and `thing_id`. Consumers rely on the DB `UNIQUE` constraint on `event_id` to swallow duplicates from dual-write paths (NATS primary + HTTP fallback).

Hub owns all durable pull consumers. Pull consumers (not push) give Hub full backpressure control. Failed messages NAK with backoff; after max retries they go to a DLQ subject and an alert fires.

## Cache hit-rate monitoring

Each cache exposes Prometheus counters:

```
nexus_cache_hits_total{cache="iam_policy"}
nexus_cache_misses_total{cache="iam_policy"}
nexus_cache_size{cache="cert_cache_lru"}
```

An alert fires when any cache drops below 80% hit rate — a signal that eviction is too aggressive or invalidation is firing too often.

---

## Canonical docs

- [`cache-multi-tier-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md) — full cache catalogue, invalidation patterns, failure modes
- [`mq-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/foundation/mq-architecture.md) — NATS JetStream interface, streams, dedup, failure modes

**Adjacent wiki pages**: [Hub Coordination](Hub-Coordination) · [Service Call Framework](Service-Call-Framework) · [Spillstore](Spillstore) · [Configuration Architecture](Configuration-Architecture) · [Observability Stack](Observability-Stack)
