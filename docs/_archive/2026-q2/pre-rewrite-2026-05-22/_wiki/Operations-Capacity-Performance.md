# Operations Capacity Performance

*Audience: operators tuning a production Nexus Gateway deployment for throughput, latency, and cost efficiency.*

Nexus Gateway's performance is dominated by two factors: upstream LLM provider latency (typically 300ms–12s depending on model and prompt length) and, for cached requests, the gateway's own processing time (sub-10ms p95 for cache hits). The performance runbooks in `docs/operators/ops/runbooks/` show that gateway-only overhead is 2ms p95 on cache-miss paths — the gateway is not the bottleneck under normal conditions. The tuning levers below affect throughput capacity, cache hit rate, provider concurrency, and tail latency.

---

## Gateway overhead profile

The AI Gateway decomposes every request latency into phases stored in `traffic_event`:

```
latency_ms  (total observed by the gateway)
  ├── usOverheadMs     = GREATEST(0, latency_ms − upstream_total_ms)
  │                      — pure gateway processing time
  ├── upstream_ttfb_ms = time-to-first-byte from upstream provider
  └── upstream_total_ms = full upstream call duration
```

Benchmark numbers from a production run against OpenAI `gpt-4o`:

| Scenario | Gateway-only p95 | Upstream total p95 | Cache hit rate |
|---|---|---|---|
| Cache hits (exact match) | 4–5 ms | N/A | ~100% |
| Cache miss (random prompts) | 2 ms | 11,382 ms | 0% |
| Streaming stress (30 VUs) | 548 ms | 9,001 ms | mixed |
| Long context (16k tokens) | 28 ms | 11,937 ms | 0% |

The 548ms gateway-only p95 for streaming includes time waiting for the response to stream through the gateway — it is not processing overhead. The W-03-mixed 2ms p95 is the cleanest baseline: gateway adds 2ms on top of an 11-second upstream call.

Query these metrics against a running deployment:

```bash
# Source admin auth helpers
source tests/lib/loadenv.sh && source tests/lib/auth.sh && cp_login

START=<iso-timestamp>
END=<iso-timestamp>

# Phase breakdown by virtual key
cp_curl "/api/admin/analytics/latency-phases?groupBy=virtual_key&start=$START&end=$END&source=ai-gateway"

# Cache effectiveness and cost savings
cp_curl "/api/admin/analytics/cache-roi?start=$START&end=$END"

# Aggregate summary
cp_curl "/api/admin/analytics/summary?start=$START&end=$END&source=ai-gateway"
```

---

## Response cache tuning

The response cache is the highest-leverage performance knob. A 70% cache hit rate translates directly to 70% fewer upstream calls — reducing cost and latency simultaneously.

Cache configuration is **fleet-wide** — there are no per-routing-rule overrides. Configure via the Control Plane UI at **AI Gateway → Cache Settings** or via:

```bash
cp_curl "/api/admin/settings/cache"
# Returns current fleet cache configuration

cp_curl -X PUT "/api/admin/settings/cache" \
  -H 'Content-Type: application/json' \
  -d '{
    "enabled": true,
    "extractCacheEnabled": true,
    "ttlSeconds": 3600,
    "maxBodyBytes": 65536
  }'
```

Key tuning levers:

| Setting | Effect | Default |
|---|---|---|
| `ttlSeconds` | How long a cached response is valid | 3600s |
| `maxBodyBytes` | Maximum response body size to cache | 65536 (64 KB) |
| `extractCacheEnabled` | Enable exact-match cache (L1) | true |

The singleflight coalescer operates automatically alongside the cache: when N concurrent requests arrive for the same prompt while the cache is cold, only one becomes the leader and issues the real upstream call. The other N−1 subscribe to the leader's stream (`hit_inflight`). This reduces upstream cost during cache warm-up bursts without any configuration.

### Monitoring cache hit rate

```bash
# Prometheus metric (AI Gateway)
curl -s http://<aigw-host>:3050/metrics | grep nexus_aigw_cache_hits_total

# Via analytics API (more detailed, includes savings)
cp_curl "/api/admin/analytics/cache-roi?start=<start>&end=<end>"
```

---

## Provider concurrency and rate limits

Each provider credential has a circuit breaker that opens when the provider returns repeated auth or rate-limit errors. A full circuit (all credentials for a provider/model pair are open) triggers the `provider.unavailable` alert and routes fail over per the configured fallback chain.

Check credential health:

```bash
cp_curl "/api/admin/credentials"
# Look for "healthStatus": "degraded" or "unavailable" rows
# and "circuitState": "open"
```

If a credential's circuit is open due to rate limiting:

```bash
# Check the circuit state directly in the DB
docker exec $(docker ps --filter "name=postgres" -q | head -1) \
  psql -U postgres -d nexus_gateway -c \
  "SELECT name, circuit_state, circuit_reason, circuit_opened_at, circuit_next_probe_at
     FROM \"Credential\"
    WHERE circuit_state != 'closed';"
```

The circuit probe occurs at `circuitNextProbeAt`. If the provider is healthy again, the probe succeeds and the circuit closes automatically. To manually reset a circuit (use sparingly — the circuit exists for a reason):

```bash
cp_curl -X POST "/api/admin/credentials/<credId>/reset-circuit"
```

### Handling provider 429 (rate limit) responses

Nexus retries on 429 with exponential backoff per the routing fallback chain. If 429s are sustained:

1. Check `health_success_rate_5m` on the credential — a value below 50% indicates sustained rate limiting.
2. Add a second credential for the same provider to distribute load across the credential pool (pool uses weighted round-robin).
3. Reduce the effective request rate by tightening quota policies on high-volume virtual keys.

---

## NATS JetStream backpressure

The compliance proxy and AI Gateway write traffic events to NATS JetStream for Hub to persist. If the NATS consumer falls behind, Hub's pending queue grows.

Check NATS consumer lag:

```bash
curl -sS "http://127.0.0.1:8222/jsz?streams=true&consumers=true" | \
  python3 -c "
import json,sys
d=json.load(sys.stdin)
for acct in d.get('account_details',[]):
  for s in acct.get('stream_detail',[]):
    for c in s.get('consumer_detail',[]):
      print(c['name'], 'pending=', c.get('num_pending'), 'redelivered=', c.get('num_redelivered'))
"
```

A non-zero and growing `num_pending` means Hub is falling behind writing events to PostgreSQL. Check Hub logs for DB connection or write errors:

```bash
journalctl -u nexus-hub -p err --since "1 hour ago"
```

---

## Compliance proxy tunnel capacity

The compliance proxy limits concurrent CONNECT tunnels via `maxConcurrentTunnels`. When this limit is approached, new connections are rejected (the `tunnels.total{result="deny"}` counter increments).

Monitor:

```bash
curl -s http://<proxy-host>:9090/metrics | grep -E 'tunnels_(active|total)'
```

If `tunnels_active` exceeds 80% of the configured limit:

1. Check whether a small number of long-lived connections are monopolizing the pool (query `traffic_event` grouped by `target_host` for high connection counts).
2. Increase `maxConcurrentTunnels` in `compliance-proxy.yaml` and restart the compliance proxy.
3. If the proxy is CPU-bound on TLS cert signing, monitor `cert_sign_ms` p99 — values above 50ms indicate the L1 cert cache is undersized. Increase the L1 cache size in the compliance proxy config.

---

## Valkey (cache tier) sizing

Valkey holds sessions, IAM cache entries, rate-limit counters, response cache entries, and desired-state cache. The response cache is the largest consumer — each cached response body is up to `maxBodyBytes` (default 64 KB).

Estimate response cache memory: `(expected concurrent unique prompts) × 64 KB`. For 10,000 cached responses the budget is ~640 MB. If Valkey's `used_memory_human` approaches the container's memory limit:

1. Reduce `ttlSeconds` to evict stale cache entries sooner.
2. Reduce `maxBodyBytes` to cap per-entry size.
3. Increase the Valkey container's memory allocation.

Check current Valkey memory usage:

```bash
docker exec nexus-valkey valkey-cli INFO memory | grep used_memory_human
docker exec nexus-valkey valkey-cli DBSIZE
```

---

## Canonical docs

- [`perf-2026-05-20-nexus-traffic-event.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/perf-2026-05-20-nexus-traffic-event.md) — gateway-side phase decomposition: gateway-only overhead, upstream TTFB, and singleflight coalescer analysis
- [`perf-2026-05-20-nexus-vs-bifrost.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/perf-2026-05-20-nexus-vs-bifrost.md) — k6 side-by-side benchmark including cache and throughput comparison
- [`monitoring.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/monitoring.md) — Prometheus metrics catalog and thresholds

**Adjacent wiki pages**: [Operations Runbook Index](Operations-Runbook-Index) · [Operations Logs Metrics Traces](Operations-Logs-Metrics-Traces) · [AI Gateway Response Cache](AI-Gateway-Response-Cache) · [AI Gateway Cost Estimation](AI-Gateway-Cost-Estimation) · [Operations FAQ](Operations-FAQ)
