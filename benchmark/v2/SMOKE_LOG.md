# Pre-AWS Smoke Log

**Date:** 2026-06-15
**Goal:** Confirm each remaining scenario runs end-to-end before AWS day. Numbers discarded — harness validation only.

## Smoke results

| Scenario | Gateway | Mode | VU × Dur | n | success | TTFT p50 | Outcome |
|---|---|---|---|--:|--:|--:|---|
| S-02 long context | LiteLLM | cache-disabled | 1 × 30s | 6 | 100% | 748.6 ms | ✅ **harness OK, scenario runs** |
| S-03 streaming stress | LiteLLM | cache-disabled | 11 × 30s | 11 | 0% | — | ⚠️ **harness OK, env failure** — see notes |
| S-08 cache feature (3 sub-tests) | Nexus | cache-enabled | 1 × 30s* | 26,519 | 100% | 89.17 ms | ✅ **passes, AND cache fix verified** |

\* S-08 ignores `BENCH_DURATION` because each sub-test hardcodes `duration_seconds=120`. Documented in "Findings" below.

## S-08 — cache feature, full sub-test breakdown

The X-Nexus-Cache header merge (done earlier today) is **verified working**: cache hit rate is now populated.

| Sub-test | n | TTFT p50 | Cache hit % |
|---|--:|--:|--:|
| `S-08-exact` (identical prompts) | 5,507 | 89.17 ms | 100.0% |
| `S-08-prefix` (shared prefix, varying suffix) | 11,755 | 38.58 ms | 100.0% |
| `S-08-mixed` (40% repeated / 60% novel) | 9,257 | 43.38 ms | 99.94% |

- **Cache hit TTFT p95: 222 ms**
- **Cache miss TTFT p95: 2,060 ms**
- **TTFT gain p95: 1,838 ms** ← the Nexus product story for James

## S-03 — what actually happened

```
total_requests: 11
failed: 11        (100%)
http_4xx: 0
http_5xx: 0
stream_broken: 0
connection_timeouts: 0
stream_timeouts: 11   ← all hit the 60s timeout
wall_time_seconds: 60.48
```

The scenario forces `vus = min(30, virtual_users + 10)`, so with `BENCH_VUS=1` it still launched **11 concurrent streams** against LiteLLM. The dataset (`streaming_v2.json`) asks for 500-word essays, which under 11 concurrent SSE streams against the local LiteLLM container **stalled past the 60 s timeout**.

**This is not a harness bug.** The harness correctly:
1. Initiated 11 concurrent SSE streams.
2. Hit the 60 s `httpx.Timeout` per stream.
3. Classified each as `stream_timeouts` (the dedicated counter we already have) — *not* as a generic failure.
4. Reported the failure category cleanly in JSON/CSV.

LiteLLM's behavior under 11+ concurrent streaming requests is itself useful data — but it's data about the gateway, not the harness. For S-03 in the AWS run we should:
- Either lower concurrency (BENCH_VUS=3 → vus=13, still likely to stall LiteLLM)
- Or accept that LiteLLM tops out at lower concurrency than Nexus
- Or raise `timeout_seconds` to 120 in the YAML for streaming-heavy scenarios

## Harness fixes / observations needed before AWS day

| Issue | Severity | Action |
|---|---|---|
| `BENCH_DURATION` env override does **not** apply to scenarios that hardcode `duration_seconds=` in their `run()` (S-04, S-05, S-08, S-09, S-11) | Medium | Either change scenarios to read from `config.benchmark.test_duration_seconds`, or document that those scenarios ignore the env override. |
| `httpx.Timeout` of 60s is too tight for S-03 streaming under high concurrency | Low | Raise `timeout_seconds` in `config/*.yaml` to 120 for streaming-heavy scenarios, or expose a `--timeout` override. |
| S-08 returns a list of 3 sub-results — single JSON file contains all 3 | None — works as designed | Already handled by `cli.py` (`isinstance(result, list)` branch). |

## S-03 RESOLVED — re-run against Nexus + Bifrost (2026-06-16)

The LiteLLM stream-timeout was gateway-specific, not a harness problem. Re-running the identical S-03 config (11 concurrent streams, 30 s) against the other two gateways:

| Gateway | n | success | stream_timeouts | stream_broken | TTFT p50 |
|---|--:|--:|--:|--:|--:|
| **Nexus** | 101 | 100% | **0** | 0% | 1345 ms |
| **Bifrost** | 91 | 100% | **0** | 0% | 327 ms |
| LiteLLM (earlier) | 11 | 0% | 11 | 0% | — |

**Conclusion:** The harness handles concurrent SSE streaming correctly. Nexus and Bifrost sustain 11 concurrent streams with zero timeouts; only the local LiteLLM container stalls at that concurrency. For the AWS head-to-head, either raise `timeout_seconds` to 120 for LiteLLM specifically, or note LiteLLM's lower streaming-concurrency ceiling as a finding. **S-03 is no longer an open item.**

## What this confirms

1. **Harness is end-to-end functional** for S-01 (already proven), S-02, S-03 (correctly reports stream_timeouts), and S-08 (cache feature fully working).
2. **Cache-hit telemetry is no longer null** — today's merge of the dual `X-Nexus-Cache` / `x-cache-status` header read is verified by S-08 returning real 99.94–100% hit rates.
3. **S-03 against LiteLLM is environmentally constrained** — useful signal for the AWS run methodology.

## Files written

```
/tmp/nx-smoke/results_bb8137ae.{json,csv}   ← S-02 LiteLLM (kept for diagnostic only, not for ranking)
/tmp/nx-smoke/results_bcde1b4d.{json,csv}   ← S-03 LiteLLM (timeout case)
/tmp/nx-smoke/results_6a84e53a.{json,csv}   ← S-08 Nexus (cache feature)
```

These are diagnostic artifacts only. They will not be included in the final comparison — AWS-day runs at n≥3000 are the deliverables.
