# Fair 3-Way Comparison — S-01 (short chat, cache-disabled)

**Generated:** 2026-06-15
**Config:** `BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=3 BENCH_DURATION=60 BENCH_WARMUP=0`
**Model:** `gpt-4o-mini` (real OpenAI upstream)
**Mode:** cache-disabled, streaming
**Methodology:** every request gets a unique nonce → defeats Nexus streaming-broker coalescing + any response cache → each gateway makes a real upstream OpenAI call per request

| Metric | **Nexus** | **LiteLLM** | **Bifrost** |
|---|--:|--:|--:|
| Total requests | 49 | 53 | 52 |
| Successful | 49 | 53 | 52 |
| HTTP failure % | 0.00 | 0.00 | 0.00 |
| Stream broken % | 0.00 | 0.00 | 0.00 |
| **TTFT p50 (ms)** | **1327.4** | **516.8** | **418.5** |
| TTFT p95 (ms) | 2274.6 | 1575.6 | 895.7 |
| TTFT p99 (ms) | 3493.8 | 2252.2 | 1737.2 |
| E2E p50 (ms) | 3398.7 | 3557.0 | 3359.1 |
| RPS | 0.79 | 0.82 | 0.84 |
| Cache hit % | N/A* | N/A | N/A |

\* Harness reads response header `x-cache-status`; Nexus actually emits `X-Nexus-Cache`. Separate harness bug — cache-hit telemetry inoperative, but irrelevant here (cache-disabled mode).

## Validity

- **Nexus TTFT p50 = 1327 ms** — realistic upstream round-trip, no broker coalescing.
- Previous broken run showed 61.7 ms p50 — that was the streaming dedupe broker collapsing 55 repeated prompts into single upstream calls. Fixed.
- E2E p50 ~3.4 s across all three is dominated by OpenAI generation time (~256 max_tokens at gpt-4o-mini speed).

## Comparison to production numbers (Tiebin, prod AMI)

| Source | Concurrency | p50 |
|---|--:|--:|
| Prod (Tiebin, 2026-06-15) | 1 | 1214 ms |
| Prod (Tiebin, 2026-06-15) | 50 | 1180 ms |
| Prod (Tiebin, 2026-06-15) | 100 | 1230 ms |
| **Local Nexus (this run)** | **3** | **1327 ms** |
| **Local LiteLLM** | **3** | **517 ms** |
| **Local Bifrost** | **3** | **418 ms** |

Local Nexus is in the same range as prod (~100 ms slower, plausibly Mac-vs-AWS network jitter and a less-optimized local build). The local LiteLLM/Bifrost being faster suggests Nexus carries ~800–900 ms of additional per-request overhead vs. these baselines locally — worth re-measuring after the new AMI lands tomorrow.
