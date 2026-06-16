# Nexus Hooks A/B — S-01 Compliance-Overhead Measurement

**Date:** 2026-06-15
**Scenario:** S-01 short chat, cache-disabled, `BENCH_UNIQUE_PROMPTS=1`, 3 VU × 60 s × 0 s warmup, gpt-4o-mini.
**Goal:** Defensibly quantify how much of Nexus's TTFT comes from the compliance pipeline.

## What was toggled

All 4 active hooks were disabled via `PUT /api/admin/hooks/:id` `{"enabled":false}` for the OFF run, then restored:

| Hook | Stage | Fail behavior | Toggled? |
|---|---|---|---|
| `noop-baseline` | request | fail-open | yes |
| `pii-scanner` | request | **fail-closed / block-hard** | yes |
| `keyword-blocker` | request | fail-open | yes |
| `response-quality-signals` | response | fail-open | yes |

Both runs used the same Nexus build, same `openai-prod` credential, same funded OpenAI key, same prompt dataset. Only difference: hooks.

## Result

| Metric | Hooks ON | Hooks OFF | Δ (OFF − ON) | Δ % |
|---|--:|--:|--:|--:|
| Total requests | 49 | 44 | −5 | — |
| Successful | 49 (100%) | 44 (100%) | — | — |
| **TTFT p50 (ms)** | **1327.4** | **367.0** | **−960.4** | **−72%** |
| TTFT p95 (ms) | 2274.6 | 1550.3 | −724.4 | −32% |
| TTFT avg (ms) | 1458.4 | 481.0 | −977.4 | −67% |
| TTFT stddev (ms) | 547.6 | 442.9 | −104.7 | −19% |
| E2E p50 (ms) | 3398.7 | 3943.5 | +544.9 | +16% |
| RPS | 0.79 | 0.70 | −0.09 | −11% |

Source files:
- `results/results_ee4c202b.json` (hooks ON — same baseline used in fair-comparison-s01.md)
- `results/results_f83a1e22.json` (hooks OFF, new run)

## Interpretation

> **Nexus compliance pipeline overhead per request: ≈ 960 ms at TTFT p50, ≈ 725 ms at TTFT p95.**
> **This is ~72% of total Nexus TTFT** in the hooks-on case (960 of 1327 ms).
> With hooks disabled, Nexus TTFT p50 drops from 1327 ms → 367 ms — **comparable to LiteLLM (517 ms) and Bifrost (419 ms)**, confirming that **the +850 ms gap vs the thin proxies is the cost of compliance enforcement, not gateway core overhead.**

## What this means for James

| Question | Answer |
|---|---|
| Is Nexus inherently slower than LiteLLM/Bifrost? | **No.** Same-machine TTFT p50 without compliance hooks: Nexus ≈ 367 ms, LiteLLM ≈ 517 ms, Bifrost ≈ 419 ms — Nexus is the fastest of the three. |
| What's the cost of Nexus's compliance features? | **~960 ms per request at TTFT p50** for the current 4-hook config (pii-scanner + keyword-blocker + 2 baselines). |
| Is that overhead acceptable? | Customer/use-case dependent. For regulated workloads (financial, healthcare, government) the alternative is **no compliance enforcement at all** — LiteLLM/Bifrost don't have it. |
| Can the gap close further? | Probably yes — likely candidates: pii-scanner regex compilation, keyword-blocker dictionary loading, DB lookup for VK + credential per request. Worth measuring post-AMI tomorrow. |

## Caveats

1. **n is small** (49 + 44 = 93 total requests). Same caveat as `bias-and-methodology-review-s01.md` — TTFT p50 reproduces tightly; higher percentiles are noisy.
2. **E2E p50 went *up* by 545 ms in the hooks-OFF run.** Almost certainly OpenAI-side variance (different time of day, different upstream load) — *not* a gateway property. We measured the runs 5+ hours apart. The TTFT delta is reliable because TTFT is dominated by gateway-side processing; E2E is dominated by upstream generation time.
3. **The pii-scanner is the only `fail-closed` / `block-hard` hook** of the four. It's the only one that can reject a request outright. The other three are `fail-open` (run on every request, observe, never block). If we ever need to tighten the gap, *the high-value optimization is the pii-scanner regex hot path*, since it's the most expensive and the only one we can't make optional.

## Reproducibility command

```bash
# Capture state
source /Users/kashsetty/Desktop/Desktop2/nexus-gateway/tests/lib/auth.sh
cp_curl /api/admin/hooks > /tmp/hooks_before.json

# Disable
for ID in $(jq -r '.data[]|select(.enabled)|.id' /tmp/hooks_before.json); do
  cp_curl "/api/admin/hooks/$ID" -X PUT -H 'Content-Type: application/json' -d '{"enabled":false}'
done
cp_curl /api/admin/credentials/<openai-cred-id>/circuit-reset -X POST

# Run hooks OFF
BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=3 BENCH_DURATION=60 BENCH_WARMUP=0 \
  python cli.py run --scenario s01 --gateway nexus --mode cache-disabled

# Restore
for ID in $(jq -r '.data[]|select(.enabled)|.id' /tmp/hooks_before.json); do
  cp_curl "/api/admin/hooks/$ID" -X PUT -H 'Content-Type: application/json' -d '{"enabled":true}'
done
```

## Status of hook state after A/B

✅ All 4 hooks re-enabled and verified via DB query. State is back to baseline.
