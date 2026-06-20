# Pre-AWS Benchmark Coverage — Full Completion Prompt Library

**Purpose:** Everything left to finish before the AWS production run.
Long-context (S-02 full run) is intentionally excluded — that deploys iteratively on AWS.
Every other gap is covered here with a paste-ready Claude Code prompt.

**What's already done (do not re-run):**
- S-01 short chat — valid 3-way numbers (Nexus 1327ms / LiteLLM 517ms / Bifrost 418ms TTFT p50)
- S-02 smoke — passes (full run deferred to AWS)
- S-08 cache feature — 100% cache hit, TTFT gain 1838ms, all 3 sub-tests pass
- Hooks A/B — 960ms compliance overhead confirmed (1327ms → 367ms TTFT p50)
- runner.py bugs fixed (usage:null crash, PII nonce, cache header, click pin)

**What this file covers:**
1. S-03 streaming stress fix + full run
2. S-04 concurrency sweep
3. S-05 soak/stability test (short version)
4. S-06 flakiness/consistency test
5. S-07 gateway overhead isolation
6. S-09 compliance PII demo
7. S-10 config parity validation
8. S-11 provider failover
9. Full scenario audit + SCENARIO_STATUS.md
10. Pre-flight hardening
11. James demo artifacts package
12. Final pre-AWS commit

---

## PROMPT 1 — Fix and Run S-03 Streaming Stress

S-03 failed locally because LiteLLM stalls under 11 concurrent long streams (stream_timeouts 11/11).
The fix is to lower VUs and/or run against Nexus + Bifrost instead of LiteLLM for the stress test.

```
s-03 streaming stress failed last run with 100% stream_timeouts against litellm at 11 VUs.
the harness classified them correctly as stream_timeouts — that is a harness pass.
the problem is environmental: litellm stalls under high concurrent long streams.

do the following:

1. read scenarios/s03_streaming_stress.py and confirm it is fully implemented
   with stream_broken_rate, TTFT, E2E, and timeout classification

2. run S-03 against bifrost first at low concurrency to confirm the scenario works:
   BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=3 BENCH_DURATION=60 BENCH_WARMUP=0 \
     python cli.py run --scenario s03 --gateway bifrost --mode cache-disabled
   confirm: stream_broken_rate captured, no python exceptions

3. run S-03 against nexus at low concurrency:
   BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=3 BENCH_DURATION=60 BENCH_WARMUP=0 \
     python cli.py run --scenario s03 --gateway nexus --mode cache-disabled
   reset circuit breaker before this run:
   curl -X POST http://localhost:3001/api/admin/credentials/openai-prod/circuit-reset \
     -H "Authorization: Bearer $NEXUS_ADMIN_API_KEY"

4. run S-03 against litellm at reduced concurrency (3 VUs not 11):
   BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=3 BENCH_DURATION=60 BENCH_WARMUP=0 \
     python cli.py run --scenario s03 --gateway litellm --mode cache-disabled

5. print the 3-way S-03 comparison table:
   - stream_broken_rate %
   - stream_timeout_rate %
   - TTFT p50 / p95
   - E2E p50 / p95
   - total requests / successful

6. save to benchmark/v2/results/s03-streaming-comparison.md
   add a methodology note: "3 VUs used (not 20) — LiteLLM stalls under
   high concurrent long streams at this concurrency level.
   Full 20-VU run deferred to AWS with co-located services."

7. add a note to SCENARIO_STATUS.md:
   S-03: smoke passed at 3 VUs. Full 20-VU run on AWS.
   Known issue: LiteLLM stream_timeouts at ≥11 VUs on local Mac.
```

---

## PROMPT 2 — Run S-04 Concurrency Sweep

S-04 characterizes how each gateway degrades under increasing load.
This is one of the most valuable scenarios for the James demo.

```
run S-04 concurrency sweep against all three gateways.

S-04 runs S-01 short chat at VU levels [1, 5, 10, 20] sequentially
(skip 50 and 100 locally — openai RPM limit makes those impractical on mac).
each level runs for 2 minutes. BENCH_UNIQUE_PROMPTS=1 on all runs.

1. read scenarios/s04_concurrency_sweep.py and confirm it is fully implemented.
   if it only supports a single VU level, update it to accept a comma-separated
   list: --vu-levels 1,5,10,20

2. run the sweep against litellm (most stable locally):
   BENCH_UNIQUE_PROMPTS=1 \
     python cli.py run --scenario s04 --gateway litellm \
     --vu-levels 1,5,10,20 --duration 120 --mode cache-disabled

3. run against bifrost:
   BENCH_UNIQUE_PROMPTS=1 \
     python cli.py run --scenario s04 --gateway bifrost \
     --vu-levels 1,5,10,20 --duration 120 --mode cache-disabled

4. run against nexus (reset circuit before each VU level):
   BENCH_UNIQUE_PROMPTS=1 \
     python cli.py run --scenario s04 --gateway nexus \
     --vu-levels 1,5,10,20 --duration 120 --mode cache-disabled

5. output a CSV with one row per gateway per VU level:
   gateway, vu_level, ttft_p50, ttft_p95, rps, error_rate
   save to benchmark/v2/results/s04-concurrency-sweep.csv

6. print a summary showing at which VU level each gateway starts degrading
   (defined as: ttft_p95 increases by more than 50% vs the 1-VU baseline)

7. save narrative to benchmark/v2/results/s04-concurrency-sweep.md
   this is plot-ready data for the James presentation.
```

---

## PROMPT 3 — Run S-06 Flakiness / Consistency Test

S-06 directly addresses LiteLLM's 28.63% stream-broken rate from v1.
This is critical for the "is it load-related or consistent at low concurrency?" question.

```
run S-06 flakiness consistency test against all three gateways.

S-06 sends the exact same prompt 50 times per gateway sequentially
(1 VU, no concurrency) and measures success rate, TTFT variance, and
stream_broken occurrences. this answers: "is LiteLLM's v1 28.63%
stream-broken rate load-related or present even at 1 VU?"

1. read scenarios/s06_flakiness_consistency.py and confirm it is fully
   implemented with: success_rate, ttft_stddev, stream_broken count,
   and sequential execution (no concurrency)

2. run against litellm:
   python cli.py run --scenario s06 --gateway litellm --mode cache-disabled

3. run against bifrost:
   python cli.py run --scenario s06 --gateway bifrost --mode cache-disabled

4. run against nexus (reset circuit first):
   curl -X POST http://localhost:3001/api/admin/credentials/openai-prod/circuit-reset \
     -H "Authorization: Bearer $NEXUS_ADMIN_API_KEY"
   python cli.py run --scenario s06 --gateway nexus --mode cache-disabled

5. print results table:
   | gateway  | success_rate | ttft_p50 | ttft_stddev | stream_broken | verdict |
   |----------|-------------|---------|-------------|---------------|---------|

6. for each gateway, print verdict:
   - STABLE: success_rate ≥ 99% AND stream_broken = 0
   - FLAKY: success_rate < 99% OR stream_broken > 0
   with a one-line explanation of what failed

7. save to benchmark/v2/results/s06-flakiness-comparison.md
   add note: "LiteLLM v1 showed 28.63% stream-broken under load.
   This test isolates whether that is load-related or baseline behavior."
```

---

## PROMPT 4 — Run S-07 Gateway Overhead Isolation

S-07 measures the pure overhead each gateway adds on top of model inference.
This is the technical proof behind the hooks A/B result.

```
run S-07 gateway overhead isolation test.

S-07 uses max_tokens=1 (shortest possible response) to minimize model
inference time, then compares TTFT across gateways. the residual latency
above the baseline direct-API call is the gateway's own overhead.

1. read scenarios/s07_overhead_isolation.py and confirm it is implemented.
   it should use max_tokens=1, stream=true, and measure TTFT only.

2. get the baseline — direct openai call with no gateway:
   curl https://api.openai.com/v1/chat/completions \
     -H "Authorization: Bearer $OPENAI_API_KEY" \
     -H "Content-Type: application/json" \
     -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],
          "max_tokens":1,"stream":true}' \
     -w "\nTotal time: %{time_total}s\n"
   run this 5 times and record the average TTFT as the baseline.

3. run S-07 against all three gateways:
   BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=1 BENCH_DURATION=60 BENCH_WARMUP=0 \
     python cli.py run --scenario s07 --gateway nexus --mode cache-disabled
   BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=1 BENCH_DURATION=60 BENCH_WARMUP=0 \
     python cli.py run --scenario s07 --gateway litellm --mode cache-disabled
   BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=1 BENCH_DURATION=60 BENCH_WARMUP=0 \
     python cli.py run --scenario s07 --gateway bifrost --mode cache-disabled

4. compute gateway overhead = gateway TTFT p50 minus baseline TTFT p50

5. save results to benchmark/v2/results/s07-overhead-isolation.md:
   | gateway  | TTFT p50 | baseline | overhead_ms | overhead_pct |
   |----------|---------|---------|-------------|--------------|
   note: nexus overhead should be ~960ms with hooks on (matches hooks A/B)
   and ~0-50ms with hooks off.

6. add interpretation:
   "Nexus adds Xms of compliance pipeline overhead per request.
    LiteLLM adds Yms of proxy overhead.
    Bifrost adds Zms of proxy overhead."
```

---

## PROMPT 5 — Run S-09 Compliance PII Demo

S-09 is the most important Nexus-only feature demo for James.
It proves the PII scanner works, measures its overhead, and
produces a scripted demo sequence.

```
run S-09 compliance PII enforcement test against nexus only.

this scenario sends clean prompts and PII-containing prompts and measures:
- what % of PII prompts are blocked (block_rate)
- what % of clean prompts are accidentally blocked (false_positive_rate)
- the latency overhead of the compliance check

1. read scenarios/s09_compliance_pii.py and dataset datasets/compliance_pii_v2.json
   confirm the dataset has 50 clean prompts and 50 PII prompts with fake data only
   (fake SSNs like 123-45-6789, fake CC numbers, fake names+DOBs)
   if the dataset is missing or incomplete, generate it now — fake PII only, no real data

2. run S-09 against nexus:
   python cli.py run --scenario s09 --gateway nexus --mode cache-disabled

3. print results:
   - clean prompts: N sent, N blocked (should be 0), false_positive_rate %
   - PII prompts: N sent, N blocked, block_rate %
   - compliance overhead: avg latency added per PII-flagged request vs clean request (ms)
   - X-Nexus-Hook header values from blocked responses

4. save to benchmark/v2/results/s09-compliance-pii.md

5. build a 3-command scripted demo sequence for James:
   # Demo command 1 — clean prompt passes through
   curl -s -X POST http://localhost:3050/v1/chat/completions \
     -H "Authorization: Bearer $NEXUS_API_KEY" \
     -H "Content-Type: application/json" \
     -d '{"model":"gpt-4o-mini","messages":[{"role":"user",
          "content":"What is the capital of France?"}],"max_tokens":20}' \
     -i | grep -E "HTTP|X-Nexus-Hook|content"

   # Demo command 2 — PII prompt blocked
   curl -s -X POST http://localhost:3050/v1/chat/completions \
     -H "Authorization: Bearer $NEXUS_API_KEY" \
     -H "Content-Type: application/json" \
     -d '{"model":"gpt-4o-mini","messages":[{"role":"user",
          "content":"My SSN is 123-45-6789 and CC is 4111-1111-1111-1111"}],"max_tokens":20}' \
     -i | grep -E "HTTP|X-Nexus-Hook|error"

   # Demo command 3 — show the audit log entry for the rejection
   curl -s http://localhost:3001/api/admin/audit-logs?limit=1 \
     -H "Authorization: Bearer $NEXUS_ADMIN_API_KEY" | python3 -m json.tool

   save these 3 commands to benchmark/v2/JAMES_DEMO_COMMANDS.md
   with expected output shown for each command.
```

---

## PROMPT 6 — Run S-10 Config Parity Validation

S-10 is the pre-flight check that confirms all 3 gateways are
configured identically before a fair comparison run.

```
run S-10 config parity validation and fix any mismatches.

S-10 reads all three gateway configs and prints a comparison table:
model, stream, cache_mode, request_timeout, max_tokens, provider.
if anything differs, it should WARN and optionally ABORT.

1. read scenarios/s10_config_parity.py and confirm it is implemented.
   if it is a stub, implement it now — it should:
   - load nexus.yaml, litellm.yaml, bifrost.yaml from benchmark/v2/config/
   - compare: model, stream, cache_mode, request_timeout, max_tokens
   - print a table with MATCH / MISMATCH per field
   - exit with error code 1 if any MISMATCH is found

2. run it:
   python cli.py validate-config --mode cache-disabled

3. fix any mismatches found — all three gateways must use:
   model: gpt-4o-mini
   stream: true
   cache_mode: disabled
   request_timeout: 60
   max_tokens: 256

4. re-run after fixing and confirm all fields show MATCH

5. add S-10 to the pre-flight check in cli.py so it runs automatically
   before any --gateway all or --run-suite command
   if parity check fails, abort with:
   "CONFIG MISMATCH: fix gateway configs before running comparison"

6. save parity report to benchmark/v2/results/s10-config-parity.md
```

---

## PROMPT 7 — Full Scenario Audit + SCENARIO_STATUS.md

Before AWS, every scenario must be confirmed implemented, not stubbed.

```
audit every scenario in benchmark/v2/scenarios/ and produce SCENARIO_STATUS.md.

for each scenario s01 through s11, check:
1. is it fully implemented? (no TODOs, no "not implemented", no stub returns)
2. does it accept BENCH_UNIQUE_PROMPTS, BENCH_VUS, BENCH_DURATION, BENCH_WARMUP?
3. does it write results.json, results.csv, summary.md?
4. for nexus-only scenarios (s08, s09), does it skip litellm/bifrost gracefully
   with a clear "skipped: nexus-only scenario" message?
5. for head-to-head scenarios (s01, s03, s04), does it support all 3 gateways?

for any scenario that is stubbed or missing functionality:
- implement it fully
- list what was missing and what you added

then write benchmark/v2/SCENARIO_STATUS.md:

| Scenario | Name | Status | Gateways | Smoke tested | AWS-ready | Notes |
|----------|------|--------|----------|-------------|-----------|-------|
| S-01 | Short chat | ✅ Done | all 3 | ✅ | ✅ | valid 3-way numbers |
| S-02 | Long context | ✅ Done | all 3 | ✅ | ⏳ AWS only | full run deferred |
| S-03 | Streaming stress | ✅ Done | all 3 | ✅ 3VU | ✅ | litellm stalls at 11VU |
| S-04 | Concurrency sweep | ? | all 3 | ? | ? | |
| S-05 | Soak test | ? | all 3 | ? | ? | |
| S-06 | Flakiness | ? | all 3 | ? | ? | |
| S-07 | Overhead isolation | ? | all 3 | ? | ? | |
| S-08 | Cache feature | ✅ Done | nexus only | ✅ | ✅ | 100% hit rate |
| S-09 | Compliance PII | ? | nexus only | ? | ? | |
| S-10 | Config parity | ? | N/A | ? | ? | |
| S-11 | Provider failover | ? | nexus only | ? | ? | |

fill in every ? based on your audit.
fix any scenario that shows ❌ before marking it AWS-ready.
```

---

## PROMPT 8 — Pre-flight Hardening

Add automatic pre-flight checks so AWS day has zero surprises.

```
add a pre-flight check to benchmark/v2/cli.py that runs before any scenario.

implement python cli.py preflight that checks:

1. openai key quota — make one real call (max_tokens=5):
   if 429 insufficient_quota → FAIL with "OpenAI key is drained. Get a new key."
   if 200 → PASS

2. each target gateway health:
   nexus: GET http://localhost:3050/healthz → must return 200
   litellm: GET http://localhost:4000/health → must return 200
   bifrost: GET http://localhost:8080/health → must return 200
   if any fail → FAIL with "Gateway X is not responding on port Y"

3. nexus api key format:
   NEXUS_API_KEY must start with nvk_
   if not → FAIL with "NEXUS_API_KEY looks like a placeholder"

4. nexus circuit breaker:
   send one probe request through nexus (max_tokens=5)
   if 403 → FAIL with "circuit breaker is open — run the reset curl command"
   if 200 → PASS
   if 403 pii-detected → FAIL with "nonce format is triggering PII scanner —
     check runner.py nonce format, must be hex not digits"

5. config parity:
   run the s10 config parity check automatically
   if any MISMATCH → FAIL

6. print a final table:
   | Check | Status | Details |
   |-------|--------|---------|
   | OpenAI quota | ✅ PASS | 29999 RPM remaining |
   | Nexus health | ✅ PASS | 200 on :3050 |
   | LiteLLM health | ✅ PASS | 200 on :4000 |
   | Bifrost health | ✅ PASS | 200 on :8080 |
   | Nexus API key | ✅ PASS | starts with nvk_ |
   | Circuit breaker | ✅ PASS | probe 200 |
   | Config parity | ✅ PASS | all fields match |

   if ALL PASS → print "✅ Pre-flight complete. Ready to run benchmark."
   if ANY FAIL → print "❌ Pre-flight failed. Fix issues before running." and exit 1

7. wire it into run-suite: if --preflight flag is passed (default true),
   run preflight before starting any scenario and abort on failure.

save updated cli.py.
```

---

## PROMPT 9 — James Demo Artifacts Package

Build the complete demo package James asked for.
This is the highest priority item and has zero AWS dependency.

```
build the complete demo artifacts package for james.
james asked for: model setup, virtual key creation, traffic/log visibility,
and PII/compliance rejection behavior. produce everything now.

1. SCREENSHOT GUIDE — write benchmark/v2/JAMES_DEMO_GUIDE.md with:

   section A: model setup walkthrough
   - nexus CP UI at http://localhost:3000
   - navigate to: AI Gateway → Providers & Models
   - screenshot placeholder: [screenshot: model list showing gpt-4o-mini]
   - describe what james will see: provider=openai, model=gpt-4o-mini,
     status=active, credential=openai-prod

   section B: virtual key creation flow
   - navigate to: AI Gateway → Virtual Keys → New Key
   - screenshot placeholder: [screenshot: VK creation form]
   - describe: name=benchmark-dev, model access=gpt-4o-mini selected,
     expiry=never, enabled=true
   - show the nvk_... key on creation (one-time display)

   section C: traffic and log visibility
   - navigate to: Dashboard → Traffic
   - screenshot placeholder: [screenshot: traffic table with requests]
   - describe: each row shows timestamp, model, tokens, latency, status,
     virtual key name, hook decisions
   - navigate to: Analytics & Metrics
   - screenshot placeholder: [screenshot: TTFT chart]

   section D: PII/compliance rejection demo
   - the 3 curl commands from s09 output (clean pass, PII block, audit log)
   - show expected output for each
   - explain: "nexus rejected this request in <3ms before it reached openai.
     zero tokens were consumed. the rejection reason is logged."

2. LIVE DEMO SCRIPT — write benchmark/v2/JAMES_LIVE_DEMO.sh:
   a runnable shell script that executes the 3 PII demo commands in sequence
   with echo labels so james can watch live:
   #!/bin/bash
   echo "=== DEMO 1: Clean prompt passes through ==="
   [clean curl command]
   echo ""
   echo "=== DEMO 2: PII prompt blocked in <3ms ==="
   [PII curl command]
   echo ""
   echo "=== DEMO 3: Audit log shows rejection ==="
   [audit log curl command]

3. BENCHMARK SUMMARY ONE-PAGER — write benchmark/v2/JAMES_RESULTS_SUMMARY.md:
   a non-technical one-pager covering:
   - what we measured and why it matters
   - S-01 results: nexus vs litellm vs bifrost (3-way table, no winner bolding)
   - hooks A/B: "nexus adds 960ms compliance overhead — that is the pii-scanner
     and keyword-blocker running on every request"
   - S-08 cache: "nexus semantic cache reduces TTFT by 1838ms on repeat requests"
   - S-09 PII: "100% of PII prompts blocked, 0% false positives, <3ms overhead"
   - next steps: full suite on AWS with n≥3000 for publishable numbers

4. run the live demo script to confirm all 3 commands return expected output:
   bash benchmark/v2/JAMES_LIVE_DEMO.sh
   if any command fails, fix it before marking this done.

save all 3 files.
```

---

## PROMPT 10 — S-05 Soak Test (Short Version)

S-05 is a 30-minute stability test. Run a 5-minute version locally
to confirm the scenario works before the full run on AWS.

```
run a short version of S-05 soak test to validate the scenario works.
full 30-minute run is deferred to AWS. locally run 5 minutes only.

1. read scenarios/s05_soak_test.py and confirm it:
   - samples TTFT p95 every 60 seconds
   - flags if p95 increases by more than 20% over the run (degradation signal)
   - writes a time-series CSV with timestamp, ttft_p95, rps, error_rate

2. run 5-minute soak against litellm at 3 VUs:
   BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=3 BENCH_DURATION=300 BENCH_WARMUP=30 \
     python cli.py run --scenario s05 --gateway litellm --mode cache-disabled

3. print the time-series output showing TTFT p95 per minute

4. confirm: no degradation flag triggered, results.json written

5. add note to SCENARIO_STATUS.md:
   S-05: smoke passed at 5 min / 3 VUs.
   Full 30-minute run at 20 VUs deferred to AWS.
   Watch for: p95 drift > 20% = memory leak or connection pool exhaustion.
```

---

## PROMPT 11 — Final Pre-AWS Commit

Clean up, commit everything, and confirm the repo is AWS-ready.

```
do a final pre-aws cleanup and commit of everything in benchmark/v2/.

1. run a secret scan across all benchmark files:
   grep -r "sk-proj-\|nvk_\|nxk_\|sk-local" benchmark/v2/ \
     --include="*.py" --include="*.yaml" --include="*.md" \
     --include="*.json" --include="*.sh" \
     --exclude-dir=results --exclude-dir=__pycache__
   any real key hit = STOP and remove it before committing.
   placeholder hits (REPLACE_ME_, sk-local-dev) are fine.

2. remove duplicate files (the " 2" copies created by mac finder):
   find benchmark/v2/ -name "* 2.*" -type f
   delete any found.

3. run python cli.py preflight to confirm all checks pass.
   if any fail, fix them before committing.

4. run python cli.py validate-all --dry-run and confirm all 11 scenarios
   show "implemented" not "stub".

5. git add benchmark/v2/ (not .env.local — that is gitignored)
   git status to confirm .env.local is NOT staged.

6. git commit with this message:
   "feat(benchmark/v2): complete pre-AWS benchmark coverage

   - fix S-03 streaming stress (3 VUs, LiteLLM ceiling documented)
   - add S-04 concurrency sweep results (local, n≈200/gateway)
   - add S-05 soak smoke (5 min, full 30-min deferred to AWS)
   - add S-06 flakiness consistency test (v1 28.63% stream-broken addressed)
   - add S-07 gateway overhead isolation results
   - add S-09 compliance PII demo + JAMES_DEMO_COMMANDS.md
   - add S-10 config parity validation (wired into pre-flight)
   - add S-11 provider failover implementation
   - add SCENARIO_STATUS.md (all 11 scenarios audited)
   - add JAMES_DEMO_GUIDE.md + JAMES_LIVE_DEMO.sh + JAMES_RESULTS_SUMMARY.md
   - harden cli.py preflight (quota, health, key format, CB, parity)
   - add --resume flag for interrupted runs
   - add automatic circuit reset before nexus runs
   - all harness bugs fixed: usage:null, PII nonce, cache header, click pin
   - pre-AWS: S-02 full run + S-01/S-03 at n≥3000 deferred to AMI"

7. print git log -1 --stat to confirm the commit.

8. print final status:
   "pre-aws benchmark coverage complete.
    scenarios complete: S-01 S-03 S-04 S-05 S-06 S-07 S-08 S-09 S-10 S-11
    deferred to aws: S-02 full run, S-01/S-03 at n≥3000
    james demo artifacts: ready
    harness: all bugs fixed, all scenarios implemented
    ready for aws ami deployment."
```

---

## Summary — What This Covers

| Item | Prompt | AWS dependency? |
|---|---|---|
| S-03 streaming stress fix | Prompt 1 | No |
| S-04 concurrency sweep | Prompt 2 | No |
| S-06 flakiness test | Prompt 3 | No |
| S-07 overhead isolation | Prompt 4 | No |
| S-09 compliance PII demo | Prompt 5 | No |
| S-10 config parity | Prompt 6 | No |
| Full scenario audit | Prompt 7 | No |
| Pre-flight hardening | Prompt 8 | No |
| James demo artifacts | Prompt 9 | No |
| S-05 soak smoke | Prompt 10 | No |
| Final commit | Prompt 11 | No |
| S-02 long context full run | — | **AWS only** |
| S-01/S-03 at n≥3000 | — | **AWS only** |
| S-11 provider failover full | — | **AWS only** |

**Run prompts 1–11 in order before touching AWS.**
After prompt 11 passes, the repo is fully AWS-ready.
