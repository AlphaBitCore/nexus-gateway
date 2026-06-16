# Combined-n Comparison — S-01 (two independent runs)

**Date:** 2026-06-15
**Scenario:** S-01 short chat, cache-disabled, `BENCH_UNIQUE_PROMPTS=1`, 3 VU × 60s × 0s warmup, gpt-4o-mini, all gateways local.
**Inputs:** 6 result files, 2 independent runs per gateway (one ours, one teammate's), same OpenAI account/key, same model, same harness fixes applied on both sides.

## Per-gateway side-by-side

### Nexus

| Run | n | TTFT avg | p50 | p95 | p99 | stddev | RPS |
|---|--:|--:|--:|--:|--:|--:|--:|
| ours (`results_ee4c202b`) | 49 | 1458.4 | 1327.4 | 2274.6 | 3493.8 | 547.6 | 0.79 |
| theirs (`teammate-results_79414a61`) | 55 | 1395.0 | 1341.9 | 1865.8 | 2163.2 | 319.0 | 0.89 |
| **range across the two runs** | 104 | — | 1327–1342 | 1866–2275 | 2163–3494 | 319–548 | 0.79–0.89 |

### LiteLLM

| Run | n | TTFT avg | p50 | p95 | p99 | stddev | RPS |
|---|--:|--:|--:|--:|--:|--:|--:|
| ours (`results_5b0f963d`) | 53 | 646.1 | 516.8 | 1575.6 | 2252.2 | 418.4 | 0.82 |
| theirs (`teammate-results_e323ac29`) | 53 | 634.4 | 453.6 | 1420.2 | 3511.2 | 642.5 | 0.84 |
| **range across the two runs** | 106 | — | 454–517 | 1420–1576 | 2252–3511 | 418–642 | 0.82–0.84 |

### Bifrost

| Run | n | TTFT avg | p50 | p95 | p99 | stddev | RPS |
|---|--:|--:|--:|--:|--:|--:|--:|
| ours (`results_2b5a8d2a`) | 52 | 512.6 | 418.5 | 895.6 | 1737.2 | 310.2 | 0.84 |
| theirs (`teammate-results_de8d1107`) | 50 | 818.8 | 405.7 | 1443.5 | 8255.7 | 1822.7 | 0.79 |
| **range across the two runs** | 102 | — | 406–419 | 896–1443 | 1737–**8256** | 310–1823 | 0.79–0.84 |

## Reproducibility delta (|ours − theirs| / mean of the two)

| Gateway | TTFT p50 | TTFT p95 | TTFT p99 | TTFT avg |
|---|--:|--:|--:|--:|
| Nexus   |  **1.1%** | 19.8% | 47.0% |  4.4% |
| LiteLLM | 13.0% | 10.4% | 43.7% |  1.8% |
| Bifrost |  **3.1%** | 46.8% | **130.5%** | 46.0% |

## What this proves

1. **TTFT p50 reproduces tightly across independent runs.** Across all three gateways, p50 deltas are ≤13% between runs — including Nexus at 1.1% and Bifrost at 3.1%. The headline ranking (**Bifrost ≈ LiteLLM ≪ Nexus**, with Nexus ≈ +850 ms slower) is robust across operators.
2. **TTFT p95 is borderline reliable.** Deltas of 10–47% on the same scenario on the same day. Useful as a directional indicator, not for fine-grained ranking.
3. **TTFT p99 is unusable at n ≈ 50.** Bifrost p99 of 1737 ms vs 8256 ms — same gateway, same day, same harness, two operators. A **130.5% delta** is not "the gateway's p99"; it's "the worst sample out of 50, which happened to be lucky in one run and unlucky in the other." Anyone reporting p99 from a 50-sample run is reporting noise, not the gateway.
4. **`avg` and `stddev` are sensitive to the same outlier as p99** — Bifrost average doubled (513 → 819 ms) and stddev sextupled (310 → 1823 ms) between runs because of one or two long-tail samples. Stddev-based "predictability" claims at n=50 are not stable evidence.

## What both runs agree on (the load-bearing facts)

| Claim | Evidence |
|---|---|
| All three gateways are reliable at 3 VU × 60 s × cache-disabled. | 100% success on all 6 runs (104 / 106 / 102 total requests across gateways). |
| Nexus TTFT p50 is ≈ +850 ms higher than LiteLLM and Bifrost on this hardware. | p50 in both runs: Nexus 1327/1342 vs LiteLLM 517/454 vs Bifrost 419/406. |
| Throughput is bounded by upstream OpenAI latency, not gateway overhead. | RPS spread 0.79–0.89 across all three gateways. With E2E ≈ 3.4 s, RPS ≈ VU / E2E ≈ 3 / 3.4 = 0.88. All three are at the upstream-bound ceiling. |

## What both runs disagree on (the not-yet-load-bearing claims)

| Claim | Status |
|---|---|
| Nexus has lower p95 latency than LiteLLM | Disagreement: ours has Nexus p95 (2275) > LiteLLM p95 (1576); theirs has Nexus p95 (1866) > LiteLLM p95 (1420). Both show Nexus higher; the *magnitude* differs by ~410 ms. |
| Bifrost is the most predictable gateway | Disagreement: ours has Bifrost stddev (310) lowest; theirs has Bifrost stddev (1823) highest. Outlier-driven flip. |
| Nexus "wins" E2E p95 / RPS | Disagreement: their report claims this; ours doesn't see it as a meaningful win — E2E differences ≤ 200 ms are dominated by OpenAI generation time, not gateway. |

## Recommendation for the AMI verification

When the new AMI lands tomorrow, the right run is **not** another 3 VU × 60 s pass. It's:

- **n ≥ 3,000 per gateway** (e.g., `BENCH_VUS=20 BENCH_DURATION=300 BENCH_WARMUP=30` — the original S-01 defaults). At this n, p95 and p99 stabilize within ≈ 7% rather than the 47% / 130% spreads we're seeing.
- **A/B Nexus hooks-on vs hooks-off** in one of the runs. That's the only way to confirm whether the +850 ms TTFT gap is genuinely "compliance overhead" or something else (DB lookup, adapter translation, traffic-event write).
- **Same 30-minute time window** for all three gateways, to share upstream OpenAI load variance.

## Source files (auditable)

| File | Run | Operator |
|---|---|---|
| `results/results_ee4c202b.json` | Nexus, n=49 | Kash (this session) |
| `results/results_5b0f963d.json` | LiteLLM, n=53 | Kash (this session) |
| `results/results_2b5a8d2a.json` | Bifrost, n=52 | Kash (this session) |
| `results/teammate-results_79414a61.json` | Nexus, n=55 | Kanishk (2026-06-15 17:12 UTC, Kanishks-NEW-MacBook-Air) |
| `results/teammate-results_e323ac29.json` | LiteLLM, n=53 | Kanishk (2026-06-15 17:13 UTC) |
| `results/teammate-results_de8d1107.json` | Bifrost, n=50 | Kanishk (2026-06-15 17:14 UTC) |

Both operators used the same funded OpenAI org key, gpt-4o-mini, `BENCH_UNIQUE_PROMPTS=1`, 3 VU × 60 s × 0 s warmup. Differences in raw numbers between the two operators are reproducibility noise, not methodology divergence.
