# AWS Benchmark Verification — v1 Launch Validity

**Date:** 2026-06-16  
**Reviewer:** Claude (Kash's session)  
**Subject:** Teammate's AWS EC2 run — valid for v1 launch publication?

---

## Run as reported

| Parameter | Value |
|---|---|
| Machine | EC2 t3.medium (2 vCPU, 4 GB RAM), us-east-1 |
| Gateways | Nexus + LiteLLM + Bifrost — all on the same instance |
| Load generator | Same instance |
| Duration | ~60 s, 3 VU |
| Model | gpt-4o-mini |
| Cache mode | disabled (claimed) |

| Gateway | TTFT p50 | Errors |
|---|--:|--:|
| Nexus | 1305 ms | 0% |
| LiteLLM | 1282 ms | 0% |
| Bifrost | 1185 ms | 0% |
| **Spread** | **120 ms** | — |

---

## Verdict: NOT valid for v1 launch publication

These numbers cannot be published as a fair comparison. They contain a structural artifact that makes Nexus look competitively close to the thin proxies — which directly contradicts every other measurement we have.

---

## The primary red flag: the spread collapsed

On every prior run (local Mac, local AWS, Tiebin's prod), the Nexus TTFT p50 gap vs thin proxies has been 850–960 ms:

| Source | Nexus p50 | LiteLLM p50 | Bifrost p50 | Spread |
|---|--:|--:|--:|--:|
| Our local run (S-01, this session) | 1327 ms | 517 ms | 419 ms | **908 ms** |
| Teammate local run (same day) | 1342 ms | 454 ms | 406 ms | **936 ms** |
| Hooks A/B (this session) | 367 ms (hooks off) | — | — | — |
| **AWS run (teammate)** | **1305 ms** | **1282 ms** | **1185 ms** | **120 ms** |

A spread that collapses from ~900 ms to ~120 ms on AWS is not a performance improvement — it is a measurement artifact. The only mechanisms that could produce this:

### Cause 1: CPU contention on t3.medium (most likely)

t3.medium has 2 vCPUs (burstable). Running on that single instance simultaneously:
- Nexus AI Gateway (Go, multi-goroutine)
- LiteLLM (Python, Docker)
- Bifrost (Docker)
- Python load generator (async httpx)

Under this load, all four processes compete for 2 vCPUs. The load generator's async event loop is starved — its `asyncio.sleep` timers and SSE read loops yield unpredictably. This creates artificial queueing delay that adds ~700–800 ms to every thin proxy's measured TTFT while barely affecting Nexus (which was already slow). The result: all three gateways converge toward the same apparent ~1.2 s TTFT with a narrow spread.

This is not "Nexus got faster on AWS" — it is "LiteLLM and Bifrost got slower because the machine was saturated."

### Cause 2: Sample size too small

3 VU × 60 s = ~100 requests per gateway. As established in `bias-and-methodology-review-s01.md`, at n≈100 the p95 CI is roughly ±40% and p50 CI is ±15–20%. The 120 ms spread (8% of ~1.3 s) is within p50 noise at this sample size — it could be real or it could be noise.

### Cause 3: Run order / warmup unknown

Not documented: which gateway ran first (cold pool penalty), whether there was a warmup phase, whether circuit breaker was reset before the Nexus run, whether `BENCH_UNIQUE_PROMPTS=1` was active.

---

## What IS valid from this run

Despite the comparison being unpublishable, these are genuine findings:

1. **All three gateways operate correctly on Linux/EC2 with real OpenAI traffic.** 0% errors across all three is a meaningful stability confirmation.
2. **Nexus works end-to-end on a fresh EC2 instance.** This is useful validation before a larger AWS run.
3. **Absolute TTFT of ~1.2–1.3 s matches our local Nexus numbers**, which is consistent with Nexus's real-world latency including hooks.

---

## What this means for v1 launch

### Do not publish these numbers as a 3-way comparison.

The 120 ms spread will be read as "Nexus is equivalent to LiteLLM/Bifrost in TTFT" — which is factually wrong. When we get a clean run, the real spread is ~850–960 ms. Publishing the t3.medium numbers would:
- Set a false customer expectation that Nexus has no TTFT overhead
- Become an embarrassment the moment a customer runs their own benchmark

### What to publish instead

For v1 launch, the cleanest defensible set of facts is already in our local results:

1. **All three gateways: 100% reliability at 3 VU × 60 s** (both our run and the teammate's local run confirm this independently).
2. **Nexus TTFT p50 ~1.3 s vs LiteLLM ~0.5 s vs Bifrost ~0.4 s** — confirmed by two independent same-day local runs with BENCH_UNIQUE_PROMPTS=1.
3. **The ~850 ms gap is compliance overhead**, not routing: Nexus with hooks disabled runs at ~367 ms p50 (hooks A/B, this session).
4. **Nexus semantic cache: TTFT gain of 1838 ms on repeat prompts** (S-08, measured locally).
5. **Nexus PII/compliance enforcement: 100% block rate on test SSN/CC payloads; LiteLLM/Bifrost forward them through** (S-09).

Points 3–5 are genuine differentiators that the thin proxies cannot match. Frame the story around those, not TTFT parity.

---

## Minimum bar for publishable AWS results

If you need AWS numbers specifically (e.g., for a "tested on EC2" claim), the re-run needs:

| Requirement | Why |
|---|---|
| **One gateway per instance** (3× EC2) | Eliminates CPU contention artifact |
| **n ≥ 500 per gateway** (20 VU × 120 s minimum) | p50 reliable; p95 borderline acceptable |
| **BENCH_UNIQUE_PROMPTS=1** | Prevents Nexus broker coalescing |
| **Circuit breaker reset before Nexus run** | `POST /api/admin/credentials/openai-prod/circuit-reset` |
| **Warmup ≥ 30 s** before measurement window | Fills connection pools |
| **Run order documented; inter-run gap < 5 min** | Shares OpenAI variance across gateways |
| **Nexus hooks ON — explicitly labeled** | The overhead is real and must be disclosed, not hidden |
| **Hooks A/B run included** | Proves the 850 ms gap is compliance, not routing bug |
| **Git SHA + image digests in report** | Reproducibility |

With this setup, you will see the real picture: Nexus ~1.3 s TTFT with hooks, ~350–400 ms with hooks off. That is still a story worth telling — compliance at cost, or speed without it, customer's choice.

---

## Recommendation to Kash

**For the v1 launch meeting:** use the local S-01 numbers (independently confirmed by two engineers) + the hooks A/B finding + S-08 cache + S-09 PII results. That package is honest, reproducible, and shows Nexus's actual value proposition.

**Tell the teammate:** the AWS run confirms the stack works on EC2, but the t3.medium concurrent-gateway methodology created a CPU contention artifact. Good smoke test; not a publishable comparison. Needs one-gateway-per-instance re-run at n≥500 to be publication-grade.
