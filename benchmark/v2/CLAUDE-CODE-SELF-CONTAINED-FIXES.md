# Claude Code Task — Benchmark v2 Self-Contained Fixes

**Goal:** Three self-contained codebase edits. No external dependencies, no AWS access needed, no waiting on Tieben. All changes are in `benchmark/v2/` on branch `aws_benchmark`.

**Branch:** `aws_benchmark`  
**Worktree:** create one at `./worktrees/bench-fixes` before starting

---

## Task 1 — Move invalid S-02 result to `results/invalid/`

The run `a4601b32` is a known-invalid hooks-OFF result. The hooks toggle never fired so it ran with hooks ON. The result file currently sits alongside valid results, which is misleading.

**What to do:**
1. Create directory `benchmark/v2/results/invalid/` if it doesn't exist
2. Move any files matching `*a4601b32*` from `benchmark/v2/results/` into `benchmark/v2/results/invalid/`
3. Add a `benchmark/v2/results/invalid/README.md` with this content:

```markdown
# Invalid Results

Run artifacts here were collected but are NOT valid benchmark measurements.

| Run ID | Reason | Valid replacement |
|--------|--------|-------------------|
| a4601b32 | hooks-OFF run — hooks_toggle.sh never fired, hooks remained ON. Result is identical to hooks-ON (11.4 RPS, 237ms p50). | bd89b7da |
```

If the result files for `a4601b32` don't exist locally (they're still on the AWS runner), just create the `invalid/` directory and the `README.md` — that's enough to document the intent and block the run ID from being treated as valid.

---

## Task 2 — Add S-02 dataset preflight validation

**File to edit:** `benchmark/v2/scenarios/s02_long_context.py`

**What to add:** Before the scenario starts sending requests, validate that the loaded dataset has prompts with at least 10,000 tokens each. The root cause of the unpadded-dataset bug (Bug 5 in the S-02 report) was that a 41-token stub was silently used for a full 300s run.

**Where to add it:** In the scenario's setup or initialization block, after the dataset is loaded and before the first request is sent.

**Logic:**
- Count tokens using a simple word-split approximation: `len(prompt.split()) * 1.3` is close enough for a preflight guard (real tokenizer not needed)
- Assert every prompt in the dataset has at least 10,000 estimated tokens
- If any prompt fails: print a clear error message showing the actual estimated token count and the file path, then raise a `SystemExit` (not just a warning — this should hard-stop the run)
- If all pass: print a one-line confirmation like `  [S-02] dataset preflight: 10 prompts, min ~12,570 tokens ✓`

**Error message format:**
```
[S-02] PREFLIGHT FAILED: dataset prompt 0 has ~41 estimated tokens (expected >= 10,000)
Dataset file: /path/to/long_context_v2.json
Fix: run benchmark/v2/scripts/pad_long_context_dataset.py to regenerate the padded dataset,
     or fetch the padded version from the repo.
```

---

## Task 3 — Add mock provider co-location note to S-02 methodology output

**File to edit:** `benchmark/v2/scenarios/s02_long_context.py`

**What to add:** When the S-02 scenario starts, print a methodology note if the upstream URL appears to be on the same host as the Nexus gateway. This is important context: in the June 19 AWS run, the mock provider ran on port 3062 on the Nexus AMI, giving Nexus a loopback latency advantage over LiteLLM and Bifrost.

**Logic:**
- Read the current gateway's upstream URL from the config (wherever the scenario has access to it — check how other scenarios access gateway config)
- If the URL contains `localhost`, `127.0.0.1`, or the same IP as `NEXUS_BASE_URL`: print a methodology warning
- The warning should appear in the scenario startup output, not as a test failure

**Warning format:**
```
  [S-02] METHODOLOGY NOTE: mock provider appears to be co-located with the Nexus gateway
         (upstream: http://localhost:3062). Nexus will have lower upstream RTT than
         LiteLLM/Bifrost. Nexus hooks-OFF vs LiteLLM comparison is partially affected.
         For neutral comparison: move mock provider to a separate instance.
```

If the upstream URL is a different IP from the Nexus instance (i.e., it's a neutral instance), no warning is needed — just proceed silently.

---

## Self-audit before reporting done

**Round 1:**
- Q1: All 3 tasks completed (or explicitly noted as partially done with reason)?
- Q2: No TODO/FIXME/stub strings added to production code?
- Q3: Tasks 1 and 3 don't need unit tests (file move + print statement). Task 2 (preflight validation) should have a unit test asserting: (a) valid dataset passes without error, (b) stub dataset raises SystemExit with the correct message format.
- Q4: No "fix this later" deferred work?

**Round 2:** Re-verify each item after fixing any Round 1 issues.

---

## Do not do

- Do not run the benchmark
- Do not touch `engine/`, `cli.py`, or any file outside `benchmark/v2/`
- Do not commit — ask Kash whether to commit after work is complete
- Do not modify `long_context_v2.json` or any result files other than moving `a4601b32`
