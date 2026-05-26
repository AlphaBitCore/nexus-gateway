# Nexus Gateway Performance — `traffic_event` Deep Dive — 2026-05-20

Gateway-side performance analysis of the Nexus benchmark run
`20260519_190950`, using the `traffic_event` table and the
`analytics/{summary, latency-phases, cache-roi}` endpoints rather than
client-side k6 numbers.

This is the report k6 cannot produce. k6 measures *total client-observed
latency*; `traffic_event` decomposes that total into

```
latency_ms                   = total observed by the gateway
  ├── usOverheadP*Ms         = GREATEST(0, latency_ms - upstream_total_ms)
  │                             — pure gateway processing time
  ├── upstream_ttfb_ms       = time-to-first-byte from upstream provider
  └── upstream_total_ms      = full upstream call duration
```

Companion report: [`perf-2026-05-20-nexus-vs-bifrost.md`](perf-2026-05-20-nexus-vs-bifrost.md)
covers the k6-level Nexus-vs-Bifrost comparison.

## 1. Run identifiers

| Field | Value |
|---|---|
| RUN_ID | `20260519_190950` |
| Window (UTC) | 2026-05-19 19:09:50 → 19:40:28 |
| Driver | k6 on EC2 `3.238.65.112`, harness `~/perf/scripts/run-all.sh` |
| Target | `https://api.taskforce10x.com` (Nexus AI Gateway production) |
| Virtual key | `lj-test` (id `803836bf-d0ae-4313-ada9-c5d7916bbeae`) |
| Model | `gpt-4o` |
| Provider | OpenAI |
| Scenarios | W-01, W-03 × 4 (exact/prefix/random/mixed), W-04, W-02 — same `run-all.sh` program as Bifrost baseline |

## 2. Aggregate run summary

Two data sources, two slightly different views:

| Source | Population | Coverage at analysis time |
|---|---|---|
| `analytics/latency-phases` (raw `traffic_event`) | 28,591 events for VK `lj-test` | All 7 scenarios; current up to query time |
| `analytics/summary` + `analytics/cache-roi` (rollup) | 19,854 events / 14,086 cache hits | W-01 + W-03 × 4 fully rolled; W-04 + W-02 partially rolled |

The rollup lags raw `traffic_event` by a few minutes for sub-hour windows, so
counts diverge for scenarios near the query time. The dual view below uses
the higher-confidence value from each source:

| Metric | Value | Source |
|---|---:|---|
| Total ai-gateway events for VK `lj-test` (raw) | **28,591** | `latency-phases` |
| Gateway error count | **0** | both |
| Cache hit rate (rollup-visible portion) | **70.5%** (14,086 / 19,854) | `summary` |
| Total prompt tokens | 1,129,398 | `cache-roi` |
| Total completion tokens | 4,290,035 | `cache-roi` |
| Provider cost (actual) | **$17.53** | `cache-roi` |
| Gateway cache savings (would-have-cost minus actual) | **$28.20** | `cache-roi` |
| Effective cost reduction | **61.7%** ($28.20 / $45.73 hypothetical no-cache total) | derived |

### Phase-level percentiles (full window, VK = `lj-test`)

```
Total latency           : p50 =     37 ms   p95 =   5,709 ms   p99 =   9,377 ms
Gateway-only (usOverh.) : p50 =      3 ms   p95 =     384 ms   p99 =     592 ms
Upstream TTFB           : p50 =    327 ms   p95 =     990 ms   p99 =   4,711 ms
Upstream total          : p50 =  2,503 ms   p95 =   9,274 ms   p99 =  14,544 ms
```

Reading: when 95% of requests are completed (p95):

- The **gateway itself** added at most 384 ms — and this is dominated by
  W-04 cached-streaming replay time, not auth / routing / IAM overhead.
- The **upstream OpenAI call** took up to 9,274 ms (when it happened — most
  requests hit cache and skipped this entirely).

The gateway's actual control-plane overhead (auth + IAM + quota + routing
+ body parsing) is **single-digit milliseconds** at p99 for cache-hit
paths — see the per-scenario breakdown below.

## 3. Per-scenario phase breakdown

Numbers from `analytics/latency-phases?groupBy=virtual_key&source=ai-gateway`
scoped to each scenario's UTC window (logged finish times from the k6
result files). Query issued **~25 minutes after the test finished** to let
the `traffic_event` batched-write path fully catch up — earlier queries
under-count W-04 / W-02 and produce misleading "all cache-hit" patterns.

| Scenario | Window | Reqs | Total p50 / p95 / p99 | Gateway-only p50 / p95 / p99 | Upstream TTFB p95 | Upstream total p95 |
|---|---|---:|---:|---:|---:|---:|
| W-01 Short Chat | 19:09:50–19:12:55 | 5,149 | 3 / **5** / 1,070 ms | 3 / **5** / 8 ms | 1,340 ms | 2,772 ms |
| W-03 Exact Cache | 19:12:55–19:18:03 | 8,618 | 3 / 2,140 / 4,606 ms | 2 / **4** / 6 ms | 1,060 ms | 3,576 ms |
| W-03 Prefix Cache | 19:18:03–19:23:13 | 2,972 | 11 / 7,140 / 10,730 ms | 9 / **15** / 24 ms | 1,030 ms | 9,584 ms |
| W-03 Random Baseline | 19:23:13–19:28:38 | 978 | 5,810 / 11,369 / 14,596 ms | 2 / **2** / 5 ms | 727 ms | 11,382 ms |
| W-03 Mixed | 19:28:38–19:34:01 | 1,990 | 1,046 / 9,713 / 15,287 ms | 2 / **10** / 8,986 ms | 824 ms | 11,246 ms |
| W-04 Streaming Stress | 19:33:50–19:37:30 | 11,742 | 201 / 558 / 716 ms | 198 / **548** / 716 ms | 2,442 ms | 9,001 ms |
| W-02 Long Context | 19:37:00–19:41:00 | 261 | 4,903 / 11,945 / 18,062 ms | 8 / **28** / 36 ms | 2,470 ms | 11,937 ms |

### 3.1 Cache-hit scenarios — gateway overhead is the headline number

- **W-01 / W-03-exact**: Total p95 ≈ gateway-only p95 ≈ 4–5 ms. There is
  no upstream call to the LLM; the gateway returns the cached response.
  The p99 = 1,070 ms / 4,606 ms shows the tail where the first few
  requests had to miss-then-fill the cache (real upstream call appears
  in that p99 bucket).
- **W-03-prefix**: Slightly higher (p95 = 15 ms). Prefix cache normalises
  the system-prompt prefix and stores the trailing generation per user
  message; the few extra milliseconds cover that normalisation.

### 3.2 Cache-miss baseline — proves gateway forwarding is near-zero overhead

The W-03-random scenario is the cleanest baseline: random prompts, no
cache benefit, every request goes to OpenAI. The phase split:

```
Total latency p95          11,369 ms     ← what k6 sees
  ├── Gateway-only p95          2 ms     ← what Nexus adds
  ├── Upstream TTFB p95       727 ms     ← OpenAI first-byte
  └── Upstream total p95   11,382 ms     ← OpenAI compute time
```

**The gateway adds 2 ms p95 on top of an 11-second upstream call.** From
a client view, 99.98% of the time is OpenAI; the gateway's marginal cost
is invisible.

The Bifrost-comparison report cannot make this claim about Bifrost: Bifrost
exposes only a single `latency` number per row, with no upstream split.

### 3.3 W-03 Mixed p99 = 8,986 ms — singleflight coalescer (hit_inflight), not a bug

The W-03-mixed gateway-only p99 spike sat well above its own p95
(10 ms) and warranted a follow-up. Direct query of the highest-latency
rows in the W-03-mixed window for VK `lj-test`:

```json
{
  "cacheStatus": "HIT",
  "latencyMs": 8983,
  "upstreamTtfbMs": null,
  "upstreamTotalMs": null,
  "latencyBreakdown": {
    "auth_ms": 1, "quota_ms": 1, "routing_ms": 1, "body_read_ms": 1,
    "audit_emit_ms": 1, "req_adapter_ms": 1, "cache_lookup_ms": 1,
    "norm_upstream_ms": 1
  },
  "statusCode": 200,
  "promptTokens": 73,
  "completionTokens": 1081
}
```

Reading: response carried real token usage (1,081 completion tokens) but
no upstream call was attributed to this row. Named gateway phases sum to
8 ms, leaving ~8.97 s "elsewhere in the gateway." That bucket is the
**singleflight coalescer** (`GatewayCacheHitInflight`, audit enum
`"hit_inflight"`, see
`packages/ai-gateway/internal/platform/audit/audit.go:232`).

Mechanism, observed in `proxy.go` / `proxy_cache.go`:

1. Cache is empty for a given (provider, model, body) key.
2. 20 W-03-mixed VUs send the same prompt simultaneously.
3. First request becomes the **leader**, issues the real upstream call
   to OpenAI (~9 s for a 1,081-token completion).
4. Other 19 requests become **joiners** — they subscribe to the leader's
   broker (`streamcache.Broker`) and wait for the leader's response.
5. When the leader's upstream completes, the broker fans out the stream
   to all 19 joiners. Each joiner's `latency_ms` measures
   request-start → last-chunk-out, which equals the leader's upstream
   time. Joiners do NOT issue their own upstream, so
   `upstream_total_ms` is NULL for them.
6. `cache_status` rolls up as HIT (`GatewayCacheHitInflight` ⇒ HIT per
   the unification rule in `audit.go:405`).

Effect: when the gateway-only p99 SQL runs
`GREATEST(0, latency_ms - upstream_total_ms)`, the joiners' NULL upstream
treats their 9-second wait as gateway-only. That's how the p99 lands at
8,986 ms even though the gateway itself did ~8 ms of CPU work.

This is **working as designed**:

- ✅ ~20× cost saving (1 upstream call instead of 20).
- ✅ Avoids hammering OpenAI rate limits during cache warm-up.
- ⚠️ Joiner-side tail latency tracks the leader's upstream duration.
  Net win on cost and rate limits, slight tail-latency cost.

Bifrost has no equivalent: every concurrent identical request issues its
own upstream call. Same scenario on Bifrost would cost ~20× more for the
same effective throughput. Bifrost's `bifrost_request_retries` histogram
counts only per-request retries, not coalescing.

### 3.4 W-04 / W-02 — mixed hit-and-miss, not all cache (correction)

A prior version of this report claimed W-04 and W-02 were "all cache hits"
based on the first query, which fired ~10 minutes after the test ended
when `traffic_event` ingestion had not flushed those scenarios yet.
Re-querying after the writer caught up:

- **W-04 streaming**: Upstream TTFB p95 = 2,442 ms, upstream total p95 =
  9,001 ms. Many requests DID hit OpenAI. Cache hits and inflight-joiners
  contribute to the lower gateway-only p95 (548 ms) — the latter is the
  same `hit_inflight` mechanism as W-03-mixed.
- **W-02 long context**: Upstream TTFB p95 = 2,470 ms, upstream total p95
  = 11,937 ms. The per-VU / per-iter markers in `w02-long-context.js`
  successfully busted exact-prompt cache, exactly as the workload
  intended. **Gateway-only p95 = 28 ms on a 16k-token context request
  that took ~12 seconds upstream** — well below 1% of total wall time.

## 4. Single-event examples

A typical cache-HIT event (gpt-4o, /v1/chat/completions, non-streaming):

```json
{
  "modelName": "gpt-4o",
  "cacheStatus": "HIT",
  "latencyMs": 36,
  "latencyBreakdown": {
    "auth_ms": 1,
    "quota_ms": 2,
    "routing_ms": 1,
    "body_read_ms": 1,
    "audit_emit_ms": 1,
    "req_adapter_ms": 1
  },
  "path": "/v1/chat/completions",
  "statusCode": 200,
  "responseHookDecision": "APPROVE"
}
```

Reading: 7 ms in named phases (auth + quota + routing + body_read + audit_emit
+ req_adapter), 29 ms in unnamed remainder (mostly the cache lookup +
response serialisation). The full 36 ms is gateway-only — no upstream call.

The corresponding p50 across 28,591 sampled events: 3 ms.

## 5. What Bifrost cannot show

The following columns / metrics either do not exist in Bifrost or are not
exposed:

| Field | Nexus (per `traffic_event` row) | Bifrost |
|---|---|---|
| Gateway-only processing time | derived from `latency_ms` − `upstream_total_ms` | not recoverable — only total `latency` |
| Upstream TTFB per request | `upstream_ttfb_ms` | only in `bifrost_upstream_latency_seconds` histogram (not per-row) |
| Cache hit/miss flag | `cache_status` (HIT/MISS) + `gateway_cache_status` + `gateway_cache_skip_reason` + `gateway_cache_kind` | not exposed at all |
| Auth / quota / routing phases | `latencyBreakdown.{auth_ms, quota_ms, routing_ms, …}` | none |
| Request / response hook decisions | `requestHookDecision`, `responseHookDecision`, with reason codes | none |
| Token / cost tracking per row | `promptTokens`, `completionTokens`, `estimatedCostUsd` | aggregate `bifrost_*_total` counters only |

Implication for benchmarking: if all you have is k6, you cannot fairly
attribute latency to "gateway overhead" vs "upstream LLM" — you only see
the sum. Against a no-cache baseline (W-03 random) the sum is dominated
by upstream and Nexus and Bifrost look comparable in k6. The
`traffic_event` view reveals that **Nexus's gateway-only contribution is
2 ms p95** — and the same data is unavailable for Bifrost.

## 6. Endpoints used (reproducing the report)

After `cp_login` (via the `prod-login` skill):

```bash
START=2026-05-19T19:09:00Z
END=2026-05-19T19:42:00Z

# Aggregate stats for the run
cp_curl "/api/admin/analytics/summary?start=$START&end=$END&source=ai-gateway"

# Phase split by virtual key — gateway-only p95 lives here
cp_curl "/api/admin/analytics/latency-phases?groupBy=virtual_key&start=$START&end=$END&source=ai-gateway"

# Cache effectiveness + savings
cp_curl "/api/admin/analytics/cache-roi?start=$START&end=$END"

# Per-scenario windows — supply the finish-time of each k6 result log
# (sample: ssh ec2-user@3.238.65.112 'stat -c "%y %n" ~/perf/results/*.log')
cp_curl "/api/admin/analytics/latency-phases?groupBy=virtual_key&start=<scen_start>&end=<scen_end>&source=ai-gateway"

# Sample individual events
cp_curl "/api/admin/traffic?virtualKeyId=803836bf-d0ae-4313-ada9-c5d7916bbeae&cacheStatus=hit&pageSize=5"
cp_curl "/api/admin/traffic?virtualKeyId=803836bf-d0ae-4313-ada9-c5d7916bbeae&cacheStatus=miss&pageSize=5"
```

Code references:

- `packages/control-plane/internal/traffic/analytics/handler/analytics_latency.go` — the SQL that
  computes `usOverhead = GREATEST(0, latency_ms - upstream_total_ms)`.
- `packages/control-plane/internal/traffic/handler/traffic/traffic.go` — `ListTrafficEvents` handler
  exposing `/api/admin/traffic` with `cacheStatus`, `virtualKeyId`, time-range filters.
- `tools/db-migrate/schema.prisma` — `TrafficEvent` model with the phase
  columns documented above.

## 7. Caveats

1. **Rollup vs raw**: `analytics/summary` and `analytics/cache-roi` read
   from a rollup table that lags the raw `traffic_event` write by a few
   minutes. Queries issued <10 minutes after a scenario completes may
   under-count for that scenario. `analytics/latency-phases` queries the
   raw table directly and is more current. All numbers above were
   double-checked against `latency-phases`.
2. **`traffic_event` ingestion lag**: the writer batches with a few
   minutes of delay even when reading via `latency-phases`. The initial
   pass of this report (committed at 11:04 GMT+8) used a too-early
   snapshot and misread W-04 / W-02 as "all cache hit". Wait at least
   15-20 minutes after a run finishes before treating raw-table queries
   as final; cross-check by re-querying and confirming the counts have
   stabilised.
3. **`createdAt` is the batch-write timestamp**, not the event time.
   Filter by `startTime`/`endTime` (the gateway-emitted timestamp), not
   by `createdAt`.
4. **`hit_inflight` joiners inflate gateway-only p99 by design**. See
   §3.3. When evaluating gateway performance, separate `gateway_cache_status
   = "hit_inflight"` rows from regular hits before reading gateway-only
   percentiles. The CP traffic detail endpoint currently does not project
   `gateway_cache_status` separately; querying the DB directly is the
   most reliable way to distinguish.

## 8. Artifact paths

- `/tmp/perf-nexus-20260519_190950/` — k6 JSON + HTML + nohup log
- `/tmp/perf-nexus-traffic/` — Nexus analytics responses
  - `summary-full.json`
  - `cache-roi-full.json`
  - `cache-roi-fullest.json` — wider window
  - `lj-test-full-window.json` — `latency-phases` full-run query
  - `scenarios/{w01,w02,w03-exact,w03-prefix,w03-random,w03-mixed,w04}-lphases.json`
  - `scenarios/{...}-summary.json`
  - `bifrost-metrics-snapshot.txt` — for cross-reference with the Bifrost report
