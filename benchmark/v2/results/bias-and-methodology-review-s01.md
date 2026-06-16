# Bias & Methodology Review — Two S-01 Benchmark Reports

**Reviewed:** 2026-06-15
**Reports compared:**
1. `results/fair-comparison-s01.md` — this session, post-fix harness
2. `Untitled` — teammate report (same date, same config: 3 VUs × 60s, `BENCH_UNIQUE_PROMPTS=1`, gpt-4o-mini, all three gateways local)

**TL;DR:** The two reports use the same harness on the same day but reach divergent rankings. Most of the spread is **sample-size noise** (n≈50 produces an unreliable p95). On top of that, the teammate's report introduces **non-data-driven framing bias** — selective bolding, directional language, an unverified causal claim, and a metric from prose that isn't in the data table. The numerical findings overlap on the dominant fact (Nexus TTFT p50 ≈ 1.3 s vs ~0.5 s for LiteLLM / Bifrost); the divergence is downstream of that and is interpretive, not numerical.

---

## 1. Side-by-side metrics (same scenario, two runs)

| Metric | Ours | Teammate | Δ vs ours | Within sampling noise? |
|---|--:|--:|--:|---|
| Nexus — total req | 49 | 55 | +12% | yes |
| Nexus — success % | 100 | 100 | 0 | yes |
| Nexus — TTFT p50 (ms) | 1327 | 1342 | +1% | yes |
| Nexus — TTFT p95 (ms) | 2275 | 1866 | **−18%** | borderline (see §3) |
| LiteLLM — total req | 53 | 53 | 0 | yes |
| LiteLLM — TTFT p50 (ms) | 517 | 454 | −12% | yes (within run variance) |
| LiteLLM — TTFT p95 (ms) | 1576 | 1420 | −10% | yes |
| Bifrost — total req | 52 | 50 | −4% | yes |
| Bifrost — TTFT p50 (ms) | 418 | 406 | −3% | yes |
| Bifrost — TTFT p95 (ms) | 896 | **1444** | **+61%** | NO — outlier-driven |
| RPS — Nexus | 0.79 | 0.89 | +13% | yes |
| RPS — LiteLLM | 0.82 | 0.84 | +2% | yes |
| RPS — Bifrost | 0.84 | 0.79 | −6% | yes |

**Agreement is strong on the headline TTFT p50** (within 1–12% across all three gateways). The visible disagreement is concentrated in **p95**, which is the noisiest statistic at this sample size.

---

## 2. The dominant fact both runs confirm

Across both reports, on this same machine, same day:

```
TTFT p50:  Bifrost ≈ LiteLLM  <<  Nexus
            ~410 ms   ~480 ms      ~1335 ms
```

Nexus carries roughly **+850 ms** of TTFT overhead vs. the thin pass-through proxies on this hardware. Both reports show this. **The question is what to do with that number** — that's where the bias creeps in.

---

## 3. Where the reports diverge — and why

### 3a. Sample size limits what you can claim

n ≈ 50 requests per arm is **too small for reliable p95 / p99**:

- p95 at n=50 is the **48th sorted value out of 50** — i.e. the 3rd worst. One outlier moves it by hundreds of ms.
- p99 at n=50 is the **single worst value**. Not statistically estimable; reporting it as a property of the gateway is misleading.
- Empirical evidence in this very comparison: Bifrost TTFT p95 reported as **896 ms in our run** and **1444 ms in the teammate's** — same config, same day. That ±60% spread on the same population is the noise floor. Any ranking based on a single ~50-request run is suspect.

**The teammate's report does not disclose sample size limits**, treats p95 differences of ~400 ms across gateways as evidence ("Nexus wins E2E p95"), and cites a Bifrost p99 of 8256 ms in the Observations text — but **p99 is not in the data table** (so the reader can't independently verify it). Citing a number-from-prose to support a "predictability" narrative without exposing it for review is itself a methodology issue.

### 3b. Selective bolding pattern

The teammate's table uses `**bold**` to mark winners. Counting them:

| Metric | Bold cell | Bold gateway | Bold justified by data? |
|---|---|---|---|
| TTFT p50 | 406 ms | Bifrost | ✅ Bifrost is lowest |
| TTFT p95 | 1420 ms | LiteLLM | ✅ LiteLLM is lowest |
| **TTFT avg** | **819 ms** | **Bifrost** | ❌ **LiteLLM is lower at 634 ms** |
| TTFT stddev | 319 ms | Nexus | ✅ Nexus is lowest |
| E2E p50 | 3296 ms | Nexus | ✅ Nexus is lowest |
| E2E p95 | 4787 ms | Nexus | ✅ Nexus is lowest |
| RPS | 0.89 | Nexus | ✅ Nexus is highest |

Two issues:

1. **One bolded "winner" doesn't match the data** (TTFT avg — Bifrost bolded at 819 ms while LiteLLM at 634 ms is lower).
2. **The bolding sums to 4× Nexus, 2× Bifrost, 1× LiteLLM** — which makes the table visually read as "Nexus wins overall," even though the metric most users feel (TTFT p50) is **3.3× slower** on Nexus than on Bifrost. A neutral table format would either (a) bold nothing, or (b) bold every row's actual minimum consistently with no editorial.

### 3c. Directional language in interpretation

Words that frame Nexus's losses as features and Nexus's wins as facts:

| Teammate language | Direction | What a neutral phrasing would say |
|---|---|---|
| "This is **expected**" (re Nexus TTFT p50 being 3× higher) | Pre-emptive justification | "Nexus TTFT p50 is 3.3× LiteLLM. Cause not isolated in this run." |
| "the **cost of its feature set**" | Asserts causation without measurement | "Possible contributors: compliance hooks, VK auth, routing, traffic-event writes. Not measured." |
| "**consistent with** the per-request compliance pipeline" | Frames difference as natural | "Higher; magnitude not attributable to any specific subsystem in this data." |
| "Nexus **wins** at E2E p95" | Used only for Nexus | Same word ("Bifrost wins TTFT p50") would be a parallel construction; the report doesn't use it. |
| "Nexus **leads** at 0.89 RPS" | Used only for Nexus | Same comment. |
| "more **predictable**" (re stddev) | Frames stddev win as virtue | True but incomplete: see §3d. |

Words for Nexus losses are softer ("expected", "consistent with", "cost"); words for Nexus wins are stronger ("wins", "leads", "predictable"). That asymmetry is the bias signal.

### 3d. The "predictability" claim is technically true but practically misleading

> "Nexus has the lowest stddev (319 ms vs 643 ms / 1823 ms), meaning its latency is more predictable."

Stddev compared in absolute terms across populations with **different means** is not a like-for-like comparison. Coefficient of variation (CV = stddev / mean) corrects for this:

| Gateway | TTFT mean | TTFT stddev | CV |
|---|--:|--:|--:|
| Nexus | 1395 ms | 319 ms | **23%** |
| LiteLLM | 634 ms | 643 ms | **101%** |
| Bifrost | 819 ms | 1823 ms | **223%** |

Yes, Nexus is more predictable as a percentage of its own mean. But Nexus's "predictable" floor is **~1.3 s** while LiteLLM/Bifrost have unpredictable distributions around **~0.4–0.6 s**. For a real user, "predictably 1.3 s" is worse than "0.4 s mean with some 1.5 s outliers" most of the time. Reporting absolute stddev without the CV (or without showing the underlying distribution) sells one true property and quietly obscures the more relevant comparison.

Additionally, Bifrost's stddev of **1823 ms with mean 819 ms and p95 of 1444 ms** is mathematically inconsistent with a unimodal distribution — it indicates one or two large outliers in a 50-sample run. The teammate uses Bifrost's outliers to make Nexus look stable, without flagging that **n=50 is the cause** of the outlier influence.

### 3e. Unverified causal attribution

> "Context: The TTFT difference between Nexus and the others reflects Nexus's compliance pipeline (PII scanning, virtual key validation, traffic event writes) running before the first token."

This is presented as the explanation. **No A/B isolation was performed.** Other plausible contributors to the +850 ms gap, not ruled out:

- VK lookup + active-key cache miss path
- Routing rule evaluation + provider selection
- Canonical-form ↔ wire-form translation in adapters
- Synchronous `traffic_event` row write before first token (vs after)
- Connection pool warmup state (cold pool penalty)
- The local Nexus Go binary being a `go run` debug build, not `go build -ldflags="-s -w"` release

To attribute the +850 ms to "compliance," the report would need at minimum: a Nexus run with all hooks disabled, same harness, same conditions. None was done. Presenting the explanation without that isolation lets the reader anchor on a flattering narrative ("worth it for compliance") that the data does not support.

### 3f. Critical methodology gotchas not disclosed

Neither report enumerates these preflight conditions, but the teammate's report is published as a conclusion-grade comparison without acknowledging:

| Gotcha | Effect if missed | Disclosed by us | Disclosed by teammate |
|---|---|---|---|
| Streaming broker coalescing — w/o `BENCH_UNIQUE_PROMPTS=1`, Nexus collapses repeated prompts → fake ~60 ms TTFT | Invalidates the comparison entirely | ✅ disclosed in `fair-comparison-s01.md` and harness `BENCHMARK_HANDOFF.md` | ✅ config line confirms `BENCH_UNIQUE_PROMPTS=1` (implicitly disclosed) |
| PII-scanner phone regex false-positive on digit-only request nonces | Blanket 403 on every request (would not have produced 100% success, so probably OK in their run) | ✅ disclosed | ❌ not mentioned |
| `usage: null` in SSE chunks crashed runner.py | Every chunk fails to parse → uncategorized failures | ✅ disclosed | ❌ not mentioned |
| Cache-hit telemetry: harness reads wrong header (`x-cache-status` vs Nexus's `X-Nexus-Cache`) | Always returns `null` for cache-hit rate even when cache is on | ✅ disclosed | ❌ not mentioned (the table just shows the metric absent) |
| Circuit-breaker reset between runs | If previous run drained quota or tripped the breaker, the new run fails 100% | ✅ procedure documented in handoff | ❌ not mentioned |
| Order in which the 3 gateways were run | First gateway has cold pools; last gateway may benefit from warmed-up macOS/Docker | ❌ not in our report either | ❌ not in teammate's report either — should be in both |
| OpenAI quota / RPM headers at run time | Determines whether observed latency is gateway or upstream throttling | ❌ not in our report | ❌ not in teammate's report |

The teammate's run could be perfectly valid — but **without those preflights stated**, a downstream reader can't tell. A report that presents conclusions but does not let a reader audit the preconditions is not reproducibility-grade.

---

## 4. Where our own report (`fair-comparison-s01.md`) is also weak

To be even-handed: our report has its own issues that should be fixed before publication:

1. **No CI / no statement of n=49–53 limits.** Same flaw the teammate's has. Our report quotes p95 / p99 as point estimates without acknowledging the noise floor.
2. **No A/B isolation either.** We attribute the gap to "Nexus carries ~800–900 ms of additional per-request overhead" — that's a measurement, not an attribution. Fine in context, but should be explicit.
3. **The cache-hit telemetry footnote is honest but tucked away.** Should be a top-of-doc methodology callout, not a `*` footnote.
4. **No mention of run order or warmup state for the cold pool.** Same gap as the teammate.

---

## 5. The data both reports can support (with no editorial)

Restating ONLY claims that survive both samples and acknowledge n≈50:

1. **All three gateways are reliable** at 3 VU × 60 s × cache-disabled × gpt-4o-mini — 100% success in both runs.
2. **Nexus TTFT p50 is ~3× the thin proxies** on the same machine (≈ 1.3 s vs ≈ 0.4–0.5 s). This is the only ranking that is stable across both runs and is unambiguous in either sample.
3. **Rankings on p95 / E2E p95 / stddev are NOT statistically separable at n≈50** between the three gateways — the two runs disagree on which gateway "wins" Bifrost p95 by 60%.
4. **E2E p50 across all three is dominated by OpenAI generation time** (~3.4 s with max_tokens=256), so differences ≤ ~200 ms at this sample size are upstream noise, not gateway signal.
5. **The cause of the +850 ms Nexus TTFT gap is unmeasured.** "Compliance pipeline" is a hypothesis, not a finding.

---

## 6. Enforcement — proposed reporting standard

The two reports diverging is not the real problem; the real problem is that there is **no agreed reporting contract**. If two teammates can produce different conclusions from the same harness on the same day, the contract is too loose. Proposed standards going forward:

### 6.1 Required preflight disclosure (top of every report)

```
Run preflight (REQUIRED to publish):
- Gateway versions (commit SHA per gateway, image digest for containerized ones)
- Circuit-breaker state confirmed reset before each gateway's run? Y/N + endpoint output
- Run order (first-to-last) — and inter-run gap in seconds
- Warmup duration per gateway
- OpenAI RPM/TPM headers observed before run and after run
- Whether harness fixes are present: runner.py usage:null guard? PII-safe nonce? Y/N
- BENCH_UNIQUE_PROMPTS, BENCH_VUS, BENCH_DURATION, BENCH_WARMUP values
- Hooks active on Nexus during run (list by name) and any that were disabled
```

### 6.2 Required sample-size + uncertainty disclosure

For any percentile reported, the report must include:
- **n** for the percentile (total successful requests per arm).
- A statement of percentile reliability: "p95 at n=50 has approximate 95% CI of ±X% based on order-statistic bounds" — or, simpler, a rule: **percentiles above p50 are not reportable below n=500**; **p99 is not reportable below n=2000**.
- For our scenario suite, this means S-01 fair-comparison runs should be 20 VU × 300 s minimum (≈3000 requests), not 3 VU × 60 s.

### 6.3 No winner bolding; no editorial words in tables

- Tables present numbers only — no `**bold**` for the lowest cell, no asterisks, no parenthetical "best."
- Words "wins / leads / dominates / costs / expected" are banned from the data section.
- Interpretation lives in a clearly labeled **"Interpretation"** section after the data, with each claim qualified by sample size.

### 6.4 No causal attribution without isolation

- A claim of the form "X causes Y" requires a paired A/B run (X on / X off) with identical other conditions.
- Without that, the report says "Possible contributors include: a, b, c. Not isolated in this run."

### 6.5 Equal coverage of wins and losses

- For every metric where one gateway is best, the table must also report **the spread** (best − worst) and **the worst** gateway's number, with no formatting difference.
- Sub-metrics measuring the same underlying dimension (e.g. TTFT p50/p95/avg/stddev) are grouped and reported together; you cannot report only the favorable sub-metrics.

### 6.6 Reproducibility hash

- Every report ends with a `results/results_<id>.json` filename + its sha256. Anyone re-running the harness with the same env and the same `id` should reproduce the table.

### 6.7 Mandatory cross-reviewer sign-off before publication

- Any S-XX comparison report headed for production-decision use must be signed off by a second engineer who ran the same scenario independently (or who audited the JSON results file). This is the same gate Tiebin's prod run implicitly satisfies (multiple eyes); it should be formalized for cross-gateway comparisons.

---

## 7. What this means for the AMI verification tomorrow

When the new AMI ships and we hand the harness to Tiebin's team:

1. **Re-run with n ≥ 3000** (20 VU × 300 s × cache-disabled × `BENCH_UNIQUE_PROMPTS=1`). Stop using 3 VU × 60 s for ranking — fine for smoke, not for comparison.
2. **Run all three gateways within a 30-minute window** (so OpenAI-side load variance is shared).
3. **Reset circuit breaker between each gateway's run** and record the response.
4. **Capture `git rev-parse HEAD`** for the Nexus binary + image digests for LiteLLM/Bifrost.
5. **Apply the reporting template** in §6 — no `**bold**` winners, no causal attribution without A/B.
6. **A/B Nexus with hooks disabled** in one of the runs. If the TTFT p50 gap drops to <200 ms, the "compliance overhead" hypothesis is supported. If it doesn't, the gap is elsewhere — and that's important for what the AMI should optimize.

If the AMI improves Nexus TTFT p50 to ~600–700 ms (closing the gap with LiteLLM by ~50%), we'll have evidence that the new build is materially better. If it lands at 1200 ms again, we know the +850 ms is structural and not addressable by the AMI changes.

---

## 8. Bottom line

- **The teammate's numerical data is not wrong** — it's a valid n≈50 run.
- **The teammate's conclusions are overreach** — bolding + directional language + an unverified causal claim push a narrative the data doesn't support.
- **Our own report has the same n-too-small problem** — we should also re-run at n ≥ 3000 before claiming the +850 ms gap is structural.
- **The fix is process, not refutation.** Adopt §6 as a reporting contract before the AMI verification.
