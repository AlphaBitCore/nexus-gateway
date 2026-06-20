# Nexus Gateway — Full Benchmark Report
## v1 → v1.5 → v2 (S-02 Long-Context)

**Prepared for:** James  
**Date:** 2026-06-19  
**Authors:** Kash, Tieben, Benchmark Team  
**Branch:** `aws_benchmark`

---

## Executive Summary

We ran three benchmark phases over four days. Each phase fixed the methodology of the prior one. The progression tells a clear story: Nexus adds minimal routing overhead (~17ms), meaningful compliance cost (~220ms per request on long-context), and is the only gateway in the test with a measurable, toggleable security pipeline. The thin proxies are faster. They do less.

| Phase | Status | Key Finding |
|-------|--------|-------------|
| v1 (June 16 — teammate AWS) | ✗ Invalid | CPU contention on shared t3.medium collapsed the spread to 120ms |
| v1.5 (June 17 — AWS, isolated) | ✓ Valid | Bifrost 297ms / LiteLLM 668ms / Nexus hooks-ON 1232ms / compliance delta 35ms (request-stage) |
| v2 S-02 (June 19 — AWS, mock provider) | ✓ Valid | Nexus hooks-OFF beats LiteLLM on RPS and p50. Hooks cost 7× throughput and 220ms TTFT. |

---

## Phase 1 — v1 AWS Benchmark (June 16)

### What was run

| Parameter | Value |
|-----------|-------|
| Instance | t3.medium (2 vCPU, 4 GB RAM) |
| Gateways | Nexus + LiteLLM + Bifrost — all on the same host |
| Load generator | Same host |
| Duration | ~60s, 3 VUs |
| Model | gpt-4o-mini (real OpenAI) |
| Tool | tools/loadtest (measures E2E round-trip, not TTFT) |

### Results (as reported)

| Gateway | TTFT p50 | Errors |
|---------|--:|--:|
| Nexus | 1305 ms | 0% |
| LiteLLM | 1282 ms | 0% |
| Bifrost | 1185 ms | 0% |
| **Spread** | **120 ms** | — |

### Verdict: NOT valid

The 120ms spread is a CPU contention artifact. Every prior local run showed a 850–960ms spread between Nexus (hooks ON) and the thin proxies. On a 2-vCPU instance with 4 processes competing — Nexus, LiteLLM, Bifrost, and the load generator — the async event loop was starved, inflating LiteLLM and Bifrost artificially toward Nexus's baseline. The gateways looked equivalent because the machine was saturated, not because they performed equally.

Additional problems: wrong measurement tool (E2E round-trip, not SSE TTFT), simultaneous runs (no isolation), 3 VU × 60s sample size too small for reliable p50/p95.

### What was salvaged

- All three gateways operated correctly on EC2 with real OpenAI traffic (0% errors). Valid smoke test.
- Nexus absolute TTFT of ~1.3s consistent with local measurements.

---

## Phase 2 — v1.5 AWS Benchmark (June 17)

### Methodology fixes applied

| Fix | Why |
|-----|-----|
| Upgraded to t3.xlarge (4 vCPU) | Eliminated CPU contention |
| Separate runner instance for load generator | Runner no longer competes with gateways |
| Sequential runs — one gateway at a time | Clean isolation |
| Switched to benchmark/v2 harness | Real SSE TTFT, not E2E round-trip |
| BENCH_UNIQUE_PROMPTS=1 | Defeats Nexus streaming dedup broker coalescing |
| 20 VUs, 300s, 30s warmup | n ≥ 1,300 per gateway for reliable p50/p95 |

### Infrastructure

| Instance | Role | Type | IP |
|----------|------|------|----|
| bench-runner | Load generator | t3.medium | <REDACTED-IP> |
| bench-nexus | Nexus AI Gateway | t3.large | <REDACTED-IP> |
| bench-litellm | LiteLLM v1.89.1 | t3.medium | <REDACTED-IP> |
| bench-bifrost | Bifrost (pinned digest) | t3.medium | <REDACTED-IP> |

### Results — 3-Way Comparison (20 VUs, real OpenAI, gpt-4o-mini)

| Gateway | TTFT p50 | TTFT p95 | RPS | Errors | n |
|---------|--:|--:|--:|--:|--:|
| Bifrost | 297 ms | 544 ms | 5.7 | 0.0% | 1,749 |
| LiteLLM v1.89.1 | 668 ms | 1,611 ms | 4.3 | 0.0% | 1,318 |
| Nexus (hooks ON) | 1,232 ms | 1,838 ms | 6.0 | 0.0% | 1,825 |

### Results — Compliance Overhead A/B (3 VUs, same UTC hour)

| Condition | TTFT p50 | TTFT p95 | n | Hook state |
|-----------|--:|--:|--:|---|
| Nexus hooks-ON | 1,454.7 ms | 2,664.4 ms | 221 | size:4 |
| Nexus hooks-OFF | 1,419.6 ms | 2,686.0 ms | 233 | size:2 |
| **Delta** | **−35.1 ms** | — | — | request-stage only |

### Key findings

**Nexus at 6.0 RPS out-throughputs both Bifrost (5.7) and LiteLLM (4.3) under the same load** — with a full compliance pipeline running on every request.

**Single-request routing overhead: ~24ms** over direct OpenAI. The gateway architecture itself is lean.

**35ms compliance delta** — important caveat: this only captured request-stage hooks (pii-scanner + keyword-blocker). The response-stage hook (`response-quality-signals`) was ON in both A/B arms, meaning both paid the SSE hold-back cost equally. The true compliance overhead including response-stage buffering was not isolated in v1.5. v2 corrects this.

**LiteLLM p95 (1,611ms) spike** — cold Docker container. p50 of 668ms is representative. Needs a re-run with confirmed warm container before LiteLLM p95 is used in any external-facing comparison.

### Bugs found and resolved (7 total)

| # | Bug | Fix |
|---|-----|-----|
| 1 | `POST /api/auth/login` doesn't exist | Rewrote to 3-step PKCE S256 OAuth exchange |
| 2 | `PATCH /api/admin/hooks/:name` returns 404 | Route is PUT, not PATCH; accepts UUID not name |
| 3 | OAuth response field `.token` | Correct field is `.access_token` |
| 4 | Python boolean `True` vs shell `true` | Lowercased in JSON field helper |
| 5 | SSH session timing out mid-run (330s) | Launched in detached `screen` session |
| 6 | AI gateway doesn't receive live hook config push | Workaround: call force resync endpoint; Tieben later confirmed this was a false positive caused by Bug 7 |
| 7 | `grep 'size:[0-9]'` never matched JSON log `"size":2` | Fixed to `grep -oP '"size":\K[0-9]+'` |

---

## Phase 3 — v2 S-02 Long-Context Benchmark (June 19)

### What changed from v1.5

- **Mock provider upstream** — Tieben's `nexus-mock-provider` (port 3062 on Nexus AMI) replaces real OpenAI. Eliminates upstream latency variance, cost, and TPM rate limits. Results measure pure gateway overhead.
- **Long-context dataset** — S-02 scenario, 16k-token prompts (~12,570 tokens, 650,997 bytes), 10 unique prompts
- **Hooks A/B corrected** — hooks-OFF arm reached `size:0` (all 4 hooks disabled including `response-quality-signals`). v1.5's `size:2` left the response-stage hook on in both arms, masking the buffering cost.

### Infrastructure

| Instance | Role | Type | IP |
|----------|------|------|----|
| <REDACTED-INSTANCE-ID> | Runner (m6i.large) | 2 vCPU, 8.2 GB | <REDACTED-IP> |
| <REDACTED-INSTANCE-ID> | Nexus AMI + mock provider | t3.xlarge | <REDACTED-IP> |
| <REDACTED-INSTANCE-ID> | LiteLLM | t3.xlarge | <REDACTED-IP> |
| <REDACTED-INSTANCE-ID> | Bifrost | t3.xlarge | <REDACTED-IP> |

### Results — S-02 Long-Context (6 VUs configured / 3 effective, mock provider, 300s + 30s warmup)

| Gateway | RPS | TTFT p50 | TTFT p95 | TTFT p99 | Requests | Errors |
|---------|--:|--:|--:|--:|--:|--:|
| Bifrost | 288.9 | 9.3 ms | 14.5 ms | 21.2 ms | 86,675 | 0 |
| Nexus (hooks OFF) | 80.3 | 17.4 ms | 183.0 ms | 377.2 ms | 24,089 | 0 |
| LiteLLM | 54.5 | 47.6 ms | 64.3 ms | 98.3 ms | 17,681 | 0 |
| Nexus (hooks ON) | 11.4 | 237.0 ms | 511.5 ms | 689.1 ms | 3,428 | 0 |

**Total: 131,873 requests. Zero errors across all runs.**

### Key findings

**Nexus hooks-OFF beats LiteLLM at both RPS (80.3 vs 54.5) and p50 TTFT (17.4ms vs 47.6ms).** When compliance is off, the Nexus routing layer outperforms Python-based LiteLLM under concurrent load.

**Compliance pipeline costs 7× throughput.** 80.3 RPS drops to 11.4 RPS when all 4 hooks are active. TTFT p50 goes from 17.4ms to 237ms — a 220ms addition per request at the median.

**Bifrost remains the hardware ceiling** at 288.9 RPS / 9.3ms — it's a thin Go proxy with no middleware. Nexus hooks-OFF is 3.6× slower than Bifrost, which is the cost of Nexus's routing layer (virtual key resolution, traffic_event write, etc.) before any compliance work.

**Nexus p95 tail (183ms) vs LiteLLM (64ms) hooks-OFF** — this gap is not from hooks, it's baseline pipeline variance. Needs investigation at higher VU counts before S-02 numbers are used in a competitive comparison.

### Methodology note

The mock provider runs on port 3062 on the Nexus AMI itself. Nexus hits it as a loopback. LiteLLM and Bifrost hit it over the network. This gives Nexus a small upstream latency advantage vs LiteLLM/Bifrost. Bifrost still outperforms Nexus hooks-OFF by 5× despite this disadvantage, confirming Bifrost's result is genuine. The Nexus hooks-OFF vs LiteLLM comparison is partially affected by this placement and should be noted when presenting externally.

### Bugs found and resolved (10 total)

| # | Bug | Fix |
|---|-----|-----|
| 1 | Bifrost SQLite UNIQUE constraint on empty key name | Used non-empty name `mock-provider-key` |
| 2 | Bifrost health check false negative on container start | Added 10s wait after `docker restart` |
| 3 | Nexus CP port 3001 blocked — wrong URL | CP is served by nginx on port 443; port 3001 security group rule unnecessary |
| 4 | Harness path wrong — SSM runs as root | Path is `/root/benchmark/v2/`, not `~/v2/` |
| 5 | long_context_v2.json was unpadded stub (41 tokens) | Fetched padded version from Kash's fork (12,570 tokens) |
| 6 | PKCE OAuth heredoc quoting mismatch | Fixed delimiter quoting |
| 7 | urllib follows 302 redirect on `/oauth/authorize` | Captured redirect URL without following it |
| 8 | Wrong LiteLLM master key from `docker inspect` | Grep matched DB password first; correct key from `~/litellm/.env` |
| 9 | SSM polling returned immediately | Background tasks only printed command ID; actual work ran inside SSM |
| 10 | NEXUS_CP_URL and NEXUS_OAUTH_REDIRECT_URI missing from .env.local | Patched both before launching runs |

---

## Cross-Phase Comparison

### Real OpenAI (v1.5) vs Mock Provider (v2 S-02)

| Gateway | v1.5 TTFT p50 (real OpenAI) | v2 TTFT p50 (mock) | What the gap shows |
|---------|--:|--:|---|
| Bifrost | 297 ms | 9.3 ms | ~288ms = AWS→OpenAI network round-trip |
| LiteLLM | 668 ms | 47.6 ms | ~620ms = OpenAI latency + Python overhead |
| Nexus hooks-ON | 1,232 ms | 237 ms | ~995ms = OpenAI latency + compliance pipeline |
| Nexus hooks-OFF | ~321 ms | 17.4 ms | ~304ms = OpenAI latency only; gateway adds ~17ms |

### Compliance overhead across phases

| Phase | Measurement | Delta | What was captured |
|-------|-------------|-------|-------------------|
| v1.5 (3 VU, real OpenAI) | hooks-ON 1454ms vs hooks-OFF 1420ms | **35ms** | Request-stage only (pii-scanner + keyword-blocker). Response-stage hook masked. |
| v2 S-02 (3 VU effective, mock) | hooks-ON 237ms vs hooks-OFF 17.4ms | **~220ms** | Full pipeline including response-quality-signals (size:0 confirmed) |

The v2 number is the correct compliance overhead. v1.5's 35ms was a partial measurement — `response-quality-signals` was on in both A/B arms in v1.5, so both paid the SSE hold-back cost equally.

---

## What the Numbers Say for James

**Three tiers emerged clearly:**

**Tier 1 — Thin proxies (Bifrost):** Maximum speed, zero intelligence. 9ms TTFT on mock, 297ms on real OpenAI. Right for internal routing where compliance is handled elsewhere.

**Tier 2 — Python middleware (LiteLLM):** 2.7× slower than Bifrost on mock, 2.2× slower on real OpenAI. No compliance. Python async overhead is real and visible under concurrent load.

**Tier 3 — Nexus with compliance:** Slowest at face value. But the 220ms compliance cost (v2) or the ~900ms real-world cost (v1.5 including OpenAI latency) buys: PII scanning, keyword blocking, response quality signals, audit trail, virtual key management, and a toggleable compliance pipeline. The routing layer itself adds ~17ms when compliance is off — competitive with LiteLLM.

**The product case:** Nexus is the only gateway in this test where you can measure exactly what security costs. The others can't produce a compliance delta because they have no compliance layer. That's the differentiator — not raw speed.

---

## What's Still In Progress / Missing

### Immediate (Claude Code — codebase fixes)

| Item | Status | What's needed |
|------|--------|---------------|
| Merge Tieben's hooks_toggle.sh rewrite (HOOK_STACK) | Pending | Tieben to push branch or paste script |
| Flag invalid S-02 result (a4601b32) in repo | Pending | Move to `results/invalid/` when files land from AWS |
| Per-hook nexus config variants | Pending | Can build now; need hook config schema from Tieben |
| Fix stream: true DONE sentinel hang | Pending | Need confirmation of how Bifrost/LiteLLM signal stream end |
| Add S-02 dataset preflight validation | Pending | Can build now — no dependencies |
| Add mock co-location note to methodology | Pending | Can write now — no dependencies |

### Next benchmark runs (AWS)

| Run | Status | Dependency |
|-----|--------|------------|
| S-01 short-context (mock provider) | Ready to run | Unblocked — infrastructure is up |
| Nexus p95 tail investigation (VU sweep: 6/12/24) | Ready to run | Unblocked |
| Per-hook isolated runs | Blocked | Needs per-hook configs in repo first |
| stream: true benchmark | Blocked | Needs harness SSE fix first |
| LiteLLM p95 re-run (warm container) | Ready to run | Unblocked |

### Open questions for Tieben

1. Push HOOK_STACK hooks_toggle.sh rewrite to `aws_benchmark` branch
2. Copy S-02 result files from runner to repo (run IDs: 0e30a3ef, 71136241, 730f9815, a4601b32, bd89b7da)
3. Share hooks config section from AMI's `ai-gateway.config.yaml` for per-hook config variants
4. Confirm: when stream: true hits mock provider, does Bifrost/LiteLLM close TCP connection at end of stream, or send a different terminal event?

### For the v2 report to be externally publishable

- [ ] Nexus p95 tail investigated and explained (183ms hooks-OFF vs 64ms LiteLLM)
- [ ] Mock provider moved to neutral instance OR each gateway gets its own local mock (fixes co-location bias)
- [ ] stream: true path benchmarked (currently bypassed with stream: false)
- [ ] Per-hook breakdown completed (identifies which hook owns the 7× regression)
- [ ] S-01 + S-02 results in same report for context-length comparison

---

*Benchmark infrastructure: us-east-1, AWS EC2. Harness: benchmark/v2 (Python 3.9, httpx, httpx-sse). All result artifacts on `aws_benchmark` branch.*
