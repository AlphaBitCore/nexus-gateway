# Deployment Hardware Sizing

Nexus Gateway services are stateless at runtime — state lives in PostgreSQL and Valkey; NATS JetStream carries in-flight events. Hardware sizing is therefore dominated by the database write rate, the number of concurrent connections the Compliance Proxy holds, and the upstream call concurrency the AI Gateway handles. This page covers per-service CPU and memory baselines, Compliance Proxy file-descriptor math, database disk planning, and observed gateway overhead numbers from production.

---

## Baseline sizing (single node)

The following numbers represent a comfortable single-node deployment for moderate AI traffic workloads. They are derived from production observation, not theoretical maximums.

| Resource | Minimum | Comfortable |
|---|---|---|
| CPU | 2 vCPU | 4 vCPU |
| RAM | 4 GB | 8 GB |
| Disk (OS + binaries) | 20 GB | 20 GB |
| Disk (PostgreSQL data + WAL) | 40 GB | 100 GB+ |
| Open file descriptors | 65 536 | 1 048 576 |

The AI Gateway and Hub hold long-lived WebSocket connections to each other and to enrolled Desktop Agents; each connection consumes a file descriptor. The Compliance Proxy can hold O(10K) concurrent CONNECT tunnels at 3–5 file descriptors each. Set `LimitNOFILE=1048576` in every systemd unit and matching PAM limits. The EC2 runbook includes the exact `sysctl.d` and `limits.d` configuration.

---

## Per-service resource profile

### Nexus Hub

Hub is the most memory-intensive server-side service: it maintains the shadow state for every enrolled node, runs scheduled jobs, and holds NATS JetStream consumers. Memory grows with the number of enrolled Desktop Agents; at O(100) agents expect 300–500 MB resident.

CPU spikes during bulk config sync (many agents reconnecting simultaneously) and during JetStream consumer catch-up after a backlog.

### AI Gateway

The AI Gateway is CPU-bound during high-concurrency SSE streaming: each active stream holds a goroutine and a file descriptor. At 100 concurrent streaming requests with typical 2–10 second upstream durations, expect 200–400 MB resident.

The response cache (Valkey Tier-1 exact match; semantic search Tier-2) reduces upstream call count significantly. The production benchmark (28 591 requests over 30 minutes) showed a 70.5% cache hit rate with gpt-4o, yielding a 61.7% effective cost reduction. Cache-hit paths add **3 ms p50 / 4–5 ms p95** gateway-only overhead; cache-miss paths forwarding to OpenAI add **2 ms p95** gateway overhead on top of the upstream call.

### Compliance Proxy

The Compliance Proxy is file-descriptor and CPU bound. File descriptors: 3–5 per active CONNECT tunnel. CPU: ECDSA P-256 certificate signing (per-hostname, cached after first issue) and TLS handshake processing.

At 1 000 concurrent tunnels: ~5 000 file descriptors, ~50 MB resident, <0.5 vCPU for cached-certificate paths.

### Control Plane

Lightest server-side service. Admin API traffic is bursty, not sustained. Expect 100–200 MB resident at idle.

### PostgreSQL

Write rate is dominated by `traffic_event` inserts: each AI request emits one or more events. At 1 req/s sustained the write rate is low (few KB/s). At 100 req/s expect ~5–15 MB/s write throughput (JSONB bodies + indexes). Bodies ≥ 256 KiB overflow to spillstore (S3 or local FS) to keep Postgres row size bounded.

WAL archiving adds ~1–3× the data write rate to disk throughput. Provision at least 3× the estimated data size for WAL + indexes + system overhead.

### Valkey/Redis

Pure cache. Session tokens, IAM policy cache, rate-limit counters, response cache, cert cache, and desired-state cache together use 256 MB–1 GB for typical enterprise traffic. Set `maxmemory-policy allkeys-lru` so certificate cache entries evict under pressure rather than causing write failures. Evicted certificates are re-created on demand.

### NATS JetStream

Stream retention is 24 hours for traffic and ops-metrics streams, 7 days for audit. At 1 req/s each event is ~1–4 KB; 24-hour hot retention at that rate uses ~350 MB. Scale linearly with request rate and message size.

---

## Gateway overhead — production numbers

The traffic_event table decomposes total observed latency into:

- `gateway_overhead` — pure Nexus processing time (auth + quota + routing + body parsing + cache lookup + audit emit)
- `upstream_ttfb_ms` — time to first byte from the upstream LLM provider
- `upstream_total_ms` — full upstream call duration

Production measurements against OpenAI gpt-4o:

| Path type | Gateway-only p50 | Gateway-only p95 | Gateway-only p99 |
|---|---|---|---|
| Cache HIT (exact match) | 3 ms | 4–5 ms | 8 ms |
| Cache MISS — no upstream call overhead | 2 ms | 2 ms | 5 ms |
| Cache MISS — live upstream call | 2 ms | 2 ms | 5 ms |
| Streaming (SSE, cache replay) | ~200 ms | 548 ms | 716 ms |

The 548 ms streaming p95 is dominated by cache-replay buffer time (the gateway replays a cached SSE stream), not by Nexus control-plane overhead. On a live upstream call against a 16K-token context (W-02 long context), gateway-only p95 was 28 ms on top of an 11 937 ms upstream call — well under 1% of total wall time.

These numbers are from the performance analysis in [`perf-2026-05-20-nexus-traffic-event.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/perf-2026-05-20-nexus-traffic-event.md).

---

## OS tuning

The key OS-level settings for the single-node deployment:

```
# /etc/sysctl.d/99-nexus.conf
net.core.somaxconn              = 65536
net.ipv4.ip_local_port_range    = 10000 65535
net.ipv4.tcp_fin_timeout        = 15
net.ipv4.tcp_tw_reuse           = 1
net.core.rmem_max               = 16777216
net.core.wmem_max               = 16777216
```

```
# /etc/security/limits.d/nexus.conf
nexus soft nofile 1048576
nexus hard nofile 1048576
```

nginx should be tuned to match: `worker_rlimit_nofile 65536`, `worker_connections 16384` per worker, `use epoll`, `multi_accept on`. Full configuration is in the EC2 runbook.

---

## Scaling beyond single node

The current production topology is single-instance. The stateless service tier (all four Go services) and the pull-only config model are designed for horizontal scaling: multiple AI Gateway instances behind a load balancer share Valkey for response cache consistency and NATS for event streaming. Hub itself is the coordination bottleneck; the roadmap includes horizontal Hub scalability as a future item.

---

## Canonical docs

- [`ec2-single-node.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/ec2-single-node.md) — full OS tuning, systemd `LimitNOFILE`, nginx `worker_connections` config
- [`perf-2026-05-20-nexus-traffic-event.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/perf-2026-05-20-nexus-traffic-event.md) — gateway overhead decomposition from production benchmarks
- [`redis-setup.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/redis-setup.md) — Valkey/Redis memory sizing and eviction policy

**Adjacent wiki pages**: [Deployment-Single-Node-Production](Deployment-Single-Node-Production) · [Deployment-Cache-MQ](Deployment-Cache-MQ) · [Deployment-Spillstore-Setup](Deployment-Spillstore-Setup) · [Operations-Capacity-Performance](Operations-Capacity-Performance)
