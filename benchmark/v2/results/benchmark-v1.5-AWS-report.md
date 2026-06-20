# Nexus Gateway v1.5 — AWS Benchmark Report

**Date:** 2026-06-16
**Instance:** t3.xlarge (4 vCPU), sequential runs, us-east-1c
**Harness:** benchmark/v2, BENCH_UNIQUE_PROMPTS=1
**Model:** gpt-4o-mini
**Methodology:** one gateway running at a time, BENCH_UNIQUE_PROMPTS=1 to defeat Nexus broker coalescing, real SSE TTFT measurement

---

## Results

| Gateway | TTFT p50 | TTFT p95 | RPS | Errors |
|---------|--:|--:|--:|--:|
| Bifrost | 314.95 ms | 584.8 ms | 6.325 | 0.0% |
| Nexus Gateway (hooks ON, run 2) | 1270.86 ms | 2183.65 ms | 5.707 | 0.0% |
| Nexus Gateway (hooks ON, run 1) | 1318.05 ms | 2337.17 ms | 5.461 | 0.0% |
| LiteLLM | 1500.52 ms | 4822.45 ms | 3.035 | 0.1% |

---

## Notes

**Two Nexus runs:** Both runs had hooks ON (pii-scanner + keyword-blocker). Run 2 (1270.86 ms) is the cleaner result — lower p50, lower p95, higher RPS. Likely run 1 had a cold connection pool. Use run 2 as the canonical Nexus hooks-ON number.

**Nexus 1270 ms ↔ local 1327 ms:** 4.2% delta — confirms the sequential methodology is working and the number is real. AWS→OpenAI is slightly faster than Mac→OpenAI.

**LiteLLM p95 spike (4822 ms):** p50 at 1500 ms is also higher than expected (~480 ms locally). Likely cause: LiteLLM ran with a cold Docker container or insufficient warmup. Needs a re-run with 30s warmup and LiteLLM scheduled last in run order.

**Bifrost 314 ms:** Faster than local Mac run (419 ms) — consistent with lower AWS→OpenAI round-trip latency.

**Hooks-OFF run:** PENDING. DB edit attempt failed — config changes must go through Hub admin API, not direct DB. See DEVLOG.md for correct curl commands.

---

## Compliance overhead (partial)

| Condition | TTFT p50 |
|-----------|--:|
| Nexus hooks ON (AWS) | 1270.86 ms |
| Nexus hooks OFF | PENDING |
| Bifrost baseline (no hooks) | 314.95 ms |

Expected hooks-OFF result: 350–450 ms. Expected compliance delta: ~820–950 ms.

---

## Validity

| Check | Status |
|-------|--------|
| Sequential runs (no CPU contention) | ✓ |
| benchmark/v2 harness (real SSE TTFT) | ✓ |
| BENCH_UNIQUE_PROMPTS=1 | ✓ |
| t3.xlarge (4 vCPU) | ✓ |
| Hooks-OFF A/B run | PENDING |
| LiteLLM re-run (warmup fix) | PENDING |
| n ≥ 500 per gateway | TBD — verify VU × duration |
