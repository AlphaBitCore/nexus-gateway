# v2.5 — Nexus Hooks-OFF Ablation Study Plan

**Goal:** Close the 3.6× RPS gap between Nexus hooks-OFF (80 RPS) and Bifrost (289 RPS).  
Both are Go. James expected parity. The gap is infrastructure overhead, not compliance. Find it and fix it.

---

## The gap in numbers

| Gateway | RPS | TTFT p50 | TTFT p95 |
|---------|--:|--:|--:|
| Bifrost | 288.9 | 9.3 ms | 14.5 ms |
| Nexus hooks-OFF | 80.3 | 17.4 ms | 183.0 ms |
| **Gap** | **3.6×** | **1.9×** | **12.6×** |

p95 gap (12.6×) is more telling than p50 (1.9×). This pattern points to intermittent blocking on a shared resource — almost certainly a synchronous DB write or lock contention — not a flat per-request overhead.

---

## Step 1 — Instrument the hot path

Add per-stage timing to `packages/ai-gateway/` request handler. Wrap each stage with a timer and emit it as a structured log field on every request (only when `NEXUS_TRACE_LATENCY=1`).

Stages to time:

```
[T1] virtual key lookup        → PostgreSQL SELECT
[T2] rate limit check          → Redis INCRBY + EXPIRE
[T3] request normalization     → codec IngressChatToCanonical
[T4] upstream dispatch         → time from send to first byte back
[T5] response normalization    → codec CanonicalToWire
[T6] traffic_event write       → PostgreSQL INSERT
[T7] cost/token tracking       → any compute after response completes
```

Total request time = T1+T2+T3+T4+T5+T6+T7. Compare to Bifrost's ~9ms. The delta is what we're hunting.

**Where to add:** `packages/ai-gateway/internal/` — find the main request handler (likely in `handler.go` or `proxy.go`). Add `time.Now()` anchors at each stage boundary, collect into a struct, log at request end.

---

## Step 2 — Run the benchmark with tracing ON

```bash
# On the Nexus AMI, restart gateway with tracing enabled
NEXUS_TRACE_LATENCY=1 systemctl restart nexus-ai-gateway

# Run S-01 short-context (lower prompt overhead = cleaner per-request signal)
BENCH_VUS=6 BENCH_DURATION=120 BENCH_WARMUP=15 \
  python cli.py run --scenario s01 --gateway nexus --mode cache-disabled

# Pull the timing breakdown from journalctl
journalctl -u nexus-ai-gateway --since "5 min ago" | \
  grep '"trace_latency"' | \
  python3 -c "
import sys, json, statistics
stages = {}
for line in sys.stdin:
    try:
        obj = json.loads(line.split('} ')[1] if '} ' in line else line)
        tl = obj.get('trace_latency', {})
        for k, v in tl.items():
            stages.setdefault(k, []).append(v)
    except: pass
for k, v in sorted(stages.items()):
    print(f'{k:30s} p50={statistics.median(v):.1f}ms  p95={sorted(v)[int(len(v)*.95)]:.1f}ms')
"
```

This gives a p50/p95 per stage from a real run. The highest-p95 stage is the bottleneck.

---

## Step 3 — Check if traffic_event write is synchronous

This is the highest-probability culprit. Every request writes a row to `TrafficEvent` — if that INSERT blocks the response path, it adds DB write latency (typically 2–10ms mean, but spikes to 50–200ms under concurrent load) to every single request.

**Check in code:**

```bash
# In the repo
grep -rn "TrafficEvent\|traffic_event\|WriteEvent\|InsertEvent" \
  packages/ai-gateway/internal/ | grep -v "_test.go" | grep -v ".pb.go"
```

Look for whether the write happens:
- **Before** sending the response back to the caller → synchronous, blocks the request
- **After** `w.Write()` or `c.Send()` returns → still potentially synchronous if not goroutine-wrapped
- **In a goroutine** `go func() { writeEvent(...) }()` → fire-and-forget, doesn't block

**If synchronous:** wrap in `go func() { ... }()` with a context that has a separate deadline. The caller should never wait for the audit trail write. Bifrost never writes anything — that's the baseline.

---

## Step 4 — Check virtual key lookup caching

Every request hits PostgreSQL to resolve the virtual key. If there's no in-memory cache:

```bash
grep -rn "virtual_key\|VirtualKey\|FindKey\|GetKey" \
  packages/ai-gateway/internal/ | grep -v "_test.go"
```

If the lookup goes to PostgreSQL on every request, add an in-process LRU cache with a 30s TTL. Virtual keys don't change mid-request. A cache hit drops this from ~2–5ms (PostgreSQL round-trip) to ~0.01ms.

**Expected impact:** 2–5ms reduction in p50, significant p95 improvement since DB latency spikes under concurrent load.

---

## Step 5 — Check rate limit check path

Redis INCRBY is fast (~0.5ms) but if it's using a connection pool that blocks on checkout under concurrency, p95 spikes. Check:

```bash
grep -rn "rate.limit\|RateLimit\|redis.*incr\|redis.*limit" \
  packages/ai-gateway/internal/ | grep -v "_test.go"
```

Look for pool size configuration. Under 80 RPS with 3 VUs, a pool of 5 is fine. At higher concurrency, pool exhaustion causes queuing.

---

## Step 6 — Profile under load (optional but definitive)

If stages 1–5 don't isolate the culprit, run Go's built-in pprof:

```bash
# Add pprof endpoint to AI gateway (if not already present)
# Then hit it during a benchmark run:
go tool pprof http://localhost:3050/debug/pprof/profile?seconds=30

# Look at the top functions by cumulative time
# The hot path should be obvious — anything unexpected at >5% is worth investigating
```

---

## What to send Tieben

> Hey Teven — following up on James's question about the Nexus vs Bifrost gap.
> 
> James flagged that both being Go, he expected hooks-OFF to be much closer to Bifrost's 289 RPS. We're at 80 RPS — 3.6× gap. The p95 gap is even bigger (183ms vs 14ms), which points to intermittent blocking on a shared resource.
> 
> My top hypothesis: `traffic_event` write is synchronous and blocking the response path under concurrent load. Can you check in `packages/ai-gateway/internal/` whether the TrafficEvent INSERT happens before the response is sent back, or if it's wrapped in a goroutine? If it's blocking, making it fire-and-forget would likely close a big chunk of the gap.
> 
> Secondary: is there an in-memory cache for virtual key lookups, or does every request hit PostgreSQL?
> 
> This is what James wants to see closed in v2.5.

---

## Expected outcome

| Fix | Estimated RPS gain | Confidence |
|-----|--------------------|------------|
| traffic_event async | +50–100 RPS | High — p95 pattern matches DB blocking |
| Virtual key LRU cache | +20–40 RPS | Medium — depends on cache miss rate |
| Connection pool tuning | +10–20 RPS | Low — likely fine at current VU count |
| Combined | **~150–160 RPS hooks-OFF** | — |

If 150–160 RPS lands, that closes the gap to ~1.8× vs Bifrost — much more defensible as "enterprise gateway overhead vs thin proxy," which is the honest story.

---

## Files to touch for fixes

| Fix | File(s) |
|-----|---------|
| traffic_event async | `packages/ai-gateway/internal/` request handler + wherever TrafficEvent write is called |
| Virtual key cache | `packages/ai-gateway/internal/` key resolution layer + add LRU (e.g., `golang.org/x/exp/cache` or `sync.Map` with TTL) |
| Timing instrumentation | Same request handler, gated on `NEXUS_TRACE_LATENCY` env var |

**Do not touch:** `packages/shared/`, compliance hooks, anything in `benchmark/v2/`. This is purely an AI gateway hot-path investigation.
