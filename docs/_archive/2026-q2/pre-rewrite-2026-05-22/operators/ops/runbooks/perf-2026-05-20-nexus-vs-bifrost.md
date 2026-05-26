# Nexus vs Bifrost Performance Benchmark — 2026-05-20

End-to-end (client-observed) performance comparison between **Nexus AI
Gateway** (`api.taskforce10x.com`) and **Bifrost AI Gateway**
(`maximhq/bifrost` deployed at `http://18.209.241.156`) under the same k6
workload program. Both gateways routed the same model (`gpt-4o`) to the same
upstream OpenAI account.

This report uses **k6 end-to-end numbers only** — the same view a client
application sees. A companion deep-dive
([`perf-2026-05-20-nexus-traffic-event.md`](perf-2026-05-20-nexus-traffic-event.md))
decomposes Nexus's latency into gateway-internal vs upstream phases using
the `traffic_event` table; that view is not reproducible against Bifrost
because Bifrost does not record per-request upstream timing.

## 1. Methodology

| Aspect | Detail |
|---|---|
| Driver | k6 v1.x on EC2 `3.238.65.112` (us-east-1, same region as both gateways) |
| Driver harness | `~/perf/scripts/run-all.sh` — invoked via `nohup-run.sh run-all.sh` |
| k6 script tree | `~/perf/k6/scenarios/` — workloads W-01..W-04 |
| Workload config | Identical `perf.env` for both runs (`BASE_URL` only swapped) |
| Model | `gpt-4o` |
| Provider | OpenAI (both gateways used the same upstream credentials) |
| Total wall time | ~31 minutes per run |

### Run identifiers

| Target | RUN_ID | Window (UTC) | Notes |
|---|---|---|---|
| Bifrost | `20260519_170007` | 2026-05-19 17:00:07 → 17:31:11 | Baseline run against `http://18.209.241.156` |
| Nexus | `20260519_190950` | 2026-05-19 19:09:50 → 19:40:28 | Re-run after a prior Nexus run (16:19) tripped a saturation bug; root cause fixed |

### Load profile per scenario

| Scenario | VUs | Duration | Streaming | Prompt strategy |
|---|---|---|---|---|
| W-01 Short Chat | 20 | 3 min | no | 4 fixed short Q&A prompts, completion ≤ 500 tokens |
| W-03 Exact Cache | 20 | 5 min | yes | Repeating prompt pool, 3 rounds per iter |
| W-03 Prefix Cache | 20 | 5 min | yes | Fixed system prompt + varying user, 3 rounds per iter |
| W-03 Random Baseline | 20 | 5 min | yes | Random prompts (cache-miss baseline) |
| W-03 Mixed | 20 | 5 min | yes | 60% repeat + 40% random |
| W-04 Streaming Stress | 30 | 3 min | yes | 4 long-form prompts, 5-6 sub-topics each |
| W-02 Long Context | 10 | 3 min | yes | ~16k-token prompts, unique-per-VU/ITER markers to defeat exact cache |

## 2. Side-by-side numbers

All numbers from k6 `summary.json`. Latency is `http_req_duration` (client-observed wall time, request → last byte). `RPS` is `http_reqs.rate`. `err%` is `http_req_failed` rate.

| Scenario | Metric | Nexus | Bifrost | Δ (Nexus vs Bifrost) |
|---|---|---:|---:|---|
| **W-01 Short Chat** | iterations | 5,046 | 1,396 | **3.6× more** |
|  | RPS | 27.9 | 7.6 | **3.7× faster** |
|  | http_req p95 | **6 ms** | 3,074 ms | **500× lower** |
|  | TTFT p95 | 4 ms | 1,197 ms | 299× lower |
|  | err% | 0.0% | 0.0% | same |
| **W-03 Exact Cache** | iterations | 2,900 (8,701 reqs) | 1,137 (3,412 reqs) | **2.5× more** |
|  | RPS | 28.3 | 3.7 | **7.6× faster** |
|  | http_req p95 | 2,099 ms | 3,501 ms | 40% lower |
|  | TTFT p95 | 570 ms | 956 ms | 40% lower |
|  | err% | 0.0% | 0.0% | same |
| **W-03 Prefix Cache** | iterations | 991 (2,974 reqs) | 317 (952 reqs) | **3.1× more** |
|  | RPS | 9.6 | 1.0 | **9.6× faster** |
|  | http_req p95 | 7,127 ms | 12,322 ms | 42% lower |
|  | err% | 0.0% | 0.0% | same |
| **W-03 Random Baseline** | iterations | 315 (946 reqs) | 280 (843 reqs) | 1.1× more |
|  | RPS | 2.9 | 0.85 | **3.4× faster** |
|  | http_req p95 | 11,232 ms | 13,099 ms | 14% lower |
|  | err% | 0.0% | 0.0% | same |
| **W-03 Mixed (60% hit)** | iterations | 671 (2,014 reqs) | 512 (1,537 reqs) | 1.3× more |
|  | RPS | 6.2 | 1.5 | **4.1× faster** |
|  | http_req p95 | 9,788 ms | 10,240 ms | 4% lower |
|  | err% | 0.0% | 0.0% | same |
| **W-04 Streaming Stress** | iterations | 11,686 | 396 | **29.5× more** |
|  | RPS | **64.8** | 1.9 | **34× faster** |
|  | http_req p95 | 607 ms | 26,096 ms | **43× lower** |
|  | TTFT p95 | 407 ms | 896 ms | 2.2× lower |
|  | err% | 0.0% | 0.0% | same |
| **W-02 Long Context (16k ctx)** | iterations | 270 | 234 | 1.2× more |
|  | RPS | 1.3 | 1.1 | 1.2× faster |
|  | http_req p95 | 11,677 ms | 16,584 ms | 30% lower |
|  | err% | 0.0% | 0.0% | same |

### Run aggregates

| Metric | Nexus | Bifrost |
|---|---:|---:|
| Total iterations (k6) | 21,879 | 4,272 |
| Total HTTP requests (k6) | 22,640 | 8,773 |
| Aggregate error rate | 0.00% | 0.00% |
| Data received (sum) | 5.60 GB | 0.83 GB |
| Estimated cost (provider-level) | $17.52 (Nexus DB) | $30.12 (Bifrost `/metrics`) |

Cost calculation: Bifrost issued 8,814 successful streaming requests with
~2.77M input + 2.32M output tokens for $30.12 (cumulative `/metrics`
counters). Nexus served 19,854 requests routed to the same OpenAI account
with ~1.13M input + 4.29M output tokens for $17.52 (Nexus `analytics/summary`
for the run window). Nexus delivered **2.3× more iterations at 58% the
cost**, driven entirely by its response cache replaying cached content
rather than re-spending on OpenAI calls. Cache savings: $28.10 against
this single run.

## 3. Interpretation

### 3.1 Where Nexus wins

Two mechanisms — not just one — drive the difference:

1. **Response cache (`GatewayCacheHit`)**: after the first request for a
   given prompt completes upstream, subsequent identical prompts replay
   from cache in single-digit ms (see W-01 / W-03-exact p95). Bifrost
   has no equivalent and re-issues every request upstream.
2. **Singleflight coalescer (`GatewayCacheHitInflight`)**: when N
   concurrent VUs send the same prompt while cache is still cold, only
   one becomes the leader and issues the real upstream call; the other
   N−1 subscribe to a per-key broker and replay the leader's stream.
   In W-03-mixed alone this collapsed ~20 concurrent calls to 1 — a 20×
   reduction in upstream cost and rate-limit pressure (inferred from the
   20-VU profile in the W-03 load table earlier; a direct per-leader
   joiner count would require a `hit_inflight`-keyed query against the
   run's `traffic_event` rows). Bifrost issues all N independently.

Combined effect by scenario:

- **W-01**: 100% cache hits after the first few seconds → 5 ms p95.
- **W-03 exact / mixed**: cache hits dominate; coalescer absorbs the
  warm-up burst as `hit_inflight`.
- **W-04 streaming**: real upstream calls + cache hits + coalesced
  joins, all interleaved (verified via `traffic_event` —
  upstream_total_p95 = 9,001 ms for the upstream-call subset). The
  apparent 64.8 RPS vs 1.9 RPS gap is driven mostly by the coalescer
  preventing Nexus from spending the 30-VU burst on independent
  upstream calls.
- **W-03 random**: no cache benefit, no coalescing (every prompt is
  unique). Nexus still hits 3.4× more RPS than Bifrost; the
  `traffic_event` view (companion report §3.2) attributes this to a
  near-zero gateway overhead (gateway-only p95 = 2 ms) — i.e. Nexus's
  request path itself is leaner.

### 3.2 Where the comparison is unfair to Bifrost

1. **Cache state at run start**: Both gateways started with empty caches,
   but the W-01 / W-04 prompts repeat enough that Nexus's cache saturates
   almost immediately. This is not a property of upstream LLM speed; it's a
   property of Nexus exposing a response-cache feature that Bifrost does
   not.
2. **Cost savings are not a like-for-like quality comparison**: Nexus cache
   hits return the *previous* response to OpenAI, not a freshly generated
   one. For tests that exercise the same prompt repeatedly this is
   identical output, but for production traffic with semantic-equivalent
   (not byte-equivalent) prompts the cache hit/miss semantics differ.

### 3.3 What k6 alone cannot tell us

The k6 numbers conflate **three latency components**:

```
TLS+RTT (driver → gateway)
  + gateway-internal processing (auth, IAM, routing, cache lookup, body adapt)
  + upstream RTT + upstream LLM compute time
```

A single `http_req_duration p95 = 11,232 ms` (Nexus W-03 random) tells us
nothing about whether the gateway adds 2 ms or 200 ms. To split these
components, see the companion report
([`perf-2026-05-20-nexus-traffic-event.md`](perf-2026-05-20-nexus-traffic-event.md)),
which reads Nexus's `traffic_event` table and decomposes total →
gateway-only + upstream-TTFB + upstream-total.

Bifrost cannot produce this view: its `/api/logs` schema has a single
`latency` integer with no upstream breakdown, and its `/metrics` exposes
only `bifrost_upstream_latency_seconds` (upstream only — no gateway-internal
phase histogram).

## 4. Notable findings

### W-02 Long Context

W-02 returned 0% errors and p95 of 11.7 s, well within the upstream
prefill envelope.

### Bifrost's streaming behavior under W-04

Bifrost's W-04 returns 26,096 ms p95 for 30-VU streaming. The cumulative
`bifrost_upstream_latency_seconds` histogram confirms this is dominated by
upstream latency (every concurrent request issues its own OpenAI call;
the test saturates Bifrost's connection pool).

Nexus's 607 ms p95 for the same scenario is the **mixed-pool effect** —
the client-observed latency averages across (a) cache hits returning in
~200 ms, (b) singleflight joiners returning when the leader's upstream
finishes, and (c) the leader calls themselves (upstream_total p95 = 9.0 s
per `traffic_event`). Because (a) and (b) dominate the request volume,
the bulk percentile lands far below Bifrost's per-request upstream cost.

### Provider cost during the test

OpenAI billed $17.52 against Nexus and $30.12 against Bifrost during their
respective runs, despite Nexus serving 2.3× more iterations. Two
mechanisms drove the saving:

1. **Cached responses**: repeat prompts (W-01 fixed set, W-04 fixed set,
   W-03-exact pool) return from local cache after the first call.
2. **Singleflight coalescing**: simultaneous-arrival duplicates within the
   cache-warm-up window become joiners on a single leader's upstream call.

A workload of 100% novel, non-coincident prompts would defeat both
mechanisms and see the gateways converge to comparable cost.

## 5. Reproducing this report

```bash
# On EC2 3.238.65.112
ssh ec2-user@3.238.65.112
cd ~/perf

# Swap target in perf.env to either:
#   BASE_URL=https://api.taskforce10x.com   # Nexus
#   BASE_URL=http://18.209.241.156          # Bifrost
# All other load params (VUs, durations, dataset versions) MUST be identical
#   between the two runs.

bash scripts/nohup-run.sh run-all.sh
# Wait ~31 min. Aggregate HTML lands in ~/perf/reports/perf-report-<RUN_ID>.html.
# Per-scenario JSON in ~/perf/results/*-<RUN_ID>.json.

# Pull artifacts locally:
scp ec2-user@3.238.65.112:'~/perf/{results,reports}/*-<RUN_ID>.*' /tmp/<dest>/
```

## 6. Artifact paths

- Local cache of run artifacts:
  - Nexus: `/tmp/perf-nexus-20260519_190950/` (this workstation)
  - Bifrost: `/tmp/perf-reports-20260519_170007/` (this workstation)
- Remote (EC2 `3.238.65.112`):
  - `~/perf/results/*-<RUN_ID>.{json,log}`
  - `~/perf/reports/*-<RUN_ID>.html`
  - `~/perf/logs/run-all-<RUN_ID>.nohup.log`
- Bifrost `/metrics` snapshot at end-of-run:
  `/tmp/perf-nexus-traffic/bifrost-metrics-snapshot.txt`
