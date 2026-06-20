# Claude Code — v2.5 Codebase Tasks

**Goal:** Three active codebase tasks for the v2.5 benchmark push. Two additional tasks
are blocked on Tieben's WIP build (documented below — do not touch those files).

**Branch discipline:**
- Tasks 1–2 (packages/ changes): create worktree at `./worktrees/v25-hotpath` on a new
  branch `feature/v25-hotpath` branched from `main`.
- Task 3 (runbook doc): can go on `aws_benchmark` — it's a doc-only change.
- Do NOT commit without asking first.
- Do NOT touch `benchmark/v2/` (already handled on `aws_benchmark`).

---

## ⛔ BLOCKED — Do not implement these until Tieben shares his WIP build

### looksLikeNDJSON() zero-alloc rewrite
**File:** `packages/shared/transport/normalize/codecs/generic_http.go` lines ~411–451  
**Status:** Tieben has already rewritten this — zero-alloc byte scan + `json.Valid`,
tests green, in his WIP build. Implementing our own version now = guaranteed merge
conflict.  
**Action:** Wait for Tieben to share the diff or push his branch. Review and benchmark
before/after. Do NOT touch this file.

### HookConfigCache.Reload() alloc fix
**File:** `packages/shared/policy/pipeline/config_cache.go`  
**Status:** Tieben is actively profiling — `HookConfigCache.Reload()` is showing ~20%
of request-path allocs. He is investigating whether it reloads per-request. Fix design
depends on his profiling output.  
**Action:** Wait for Tieben's findings. Do NOT touch this file.

### Before/after benchmark run
**Status:** Blocked on both of the above. Once Tieben shares his WIP build, run S-02
on old binary vs new binary and diff the RPS/p95 numbers. That's the only job here —
no codebase change needed.

---

## Active tasks (unblocked — implement these now)

## Task 2 — NEXUS_TRACE_LATENCY=1 flag

**Context:** Per-stage timing already exists.
`packages/shared/traffic/latencybreakdown.go` has a typed `LatencyBreakdown` map
(phase name → duration in ms) populated by `PhaseTimer.Snapshot()` on every request.
The result is already stored in `traffic_event.latency_breakdown` JSONB. What does NOT
exist is a way to see it in live structured logs during a benchmark run without querying
Postgres.

**What to build:**
1. Read `NEXUS_TRACE_LATENCY` from env at AI gateway startup (one `os.Getenv` check,
   stored as a package-level `bool`).
2. In the AI gateway request handler — wherever the `LatencyBreakdown` / `PhaseTimer`
   snapshot is collected after the request completes — add a conditional log line:

```go
if traceLatencyEnabled {
    logger.Info("request_latency_breakdown",
        zap.String("request_id", requestID),
        zap.Any("breakdown", latencyBreakdown),   // the existing LatencyBreakdown map
        zap.Float64("total_ms", totalElapsedMs),
    )
}
```

3. The flag must be `false` by default. No behavior change when unset.

**How to find the right insertion point:**
```bash
grep -rn "PhaseTimer\|LatencyBreakdown\|latency_breakdown" \
  packages/ai-gateway/internal/ | grep -v "_test.go"
```
The call site where `PhaseTimer.Snapshot()` is invoked or where `LatencyBreakdown` is
attached to the traffic event is the right place to add the log line.

**Do NOT:**
- Add new timing instrumentation — the timing already exists
- Log on every request unconditionally — gate on the env var
- Touch `packages/shared/traffic/latencybreakdown.go`

**Unit test required:**
- With `NEXUS_TRACE_LATENCY=1`: log line is emitted after a handled request
- Without the env var: no log line (default off)

---

## Task 3 — per_hook_sweep.sh

**File to create:** `benchmark/v2/scripts/per_hook_sweep.sh`

**What it does:** Enables exactly one compliance hook at a time, runs S-02, saves the
result with a hook-name suffix, disables, repeats for all 4 hooks.

Hooks live in the DB table `HookConfig`. Per-hook variants = enable one row at a time
via the admin API (the same admin API that `hooks_toggle.sh` uses). Do NOT use yaml.

**Script requirements:**

```bash
#!/usr/bin/env bash
# per_hook_sweep.sh — isolate cost of each compliance hook
# Run on the Nexus AMI as ec2-user from /home/ec2-user/bench-v2/
# Requires: .env.local (same as hooks_toggle.sh)

HOOKS=(pii-scanner keyword-blocker response-quality-signals noop-baseline)

# 1. Source .env.local (fail loudly if missing — same pattern as hooks_toggle.sh)
# 2. Run PKCE OAuth exchange to get access token (copy from hooks_toggle.sh)
# 3. For each hook in HOOKS:
#    a. Disable all 4 hooks (PUT size:0)
#    b. Enable only this one hook (PUT its UUID to enabled:true)
#    c. Assert: size == 1 in journalctl (same assertion as hooks_toggle.sh)
#    d. Run S-02: python cli.py run --scenario s02 --gateway nexus \
#                   --duration 300 --warmup 30 \
#                   --output-suffix "_hook_${hook_name}"
#    e. Disable all hooks again (PUT size:0)
#    f. Sleep 5s before next hook
# 4. Print summary of result files produced

# Result naming: the --output-suffix flag should append to the run ID filename.
# Check cli.py / runner.py for the correct flag name — if it doesn't exist,
# use BENCH_OUTPUT_SUFFIX env var or rename after the run using the run ID.
```

**OAuth / admin API pattern:** Copy the 3-step PKCE S256 exchange and UUID-resolution
pattern verbatim from `benchmark/v2/scripts/hooks_toggle.sh` — do not reinvent it.

**Tieben's hypothesis:** `response-quality-signals` owns ~160ms of the 220ms compliance
cost due to SSE hold-back buffering. This script produces the data to confirm or refute.

**No unit test required** (shell script + AWS-bound). Add a `--dry-run` flag that
prints what it would do without calling the API or running the benchmark.

---

## Task 4 — Update AWS_RUNBOOK.md

**File:** `benchmark/v2/AWS_RUNBOOK.md`  
**Branch:** `aws_benchmark` (doc-only, fine to land here)

Add a new section **"Correct hooks_toggle workflow"** immediately before or after any
existing section that mentions hooks. Content:

```markdown
## Correct hooks_toggle workflow

hooks_toggle.sh must run ON the Nexus AMI directly, not from the bench-runner.

**Why the runner-side approach fails silently:**  
The script sources `.env.local` at the top and exits before touching any hook if the
file doesn't exist at that path. When run via SSM send-command as root, `.env.local`
doesn't exist at `/root/bench-v2/.env.local` — the file lives at
`/home/ec2-user/bench-v2/.env.local`. The script exits cleanly (exit 0), Nexus hooks
remain at their prior state, and the benchmark proceeds as if the toggle succeeded.
This was the root cause of invalid run a4601b32 (hooks-OFF run that ran hooks-ON).

**Correct steps:**
1. In the AWS console, select the Nexus AMI instance → Connect → EC2 Instance Connect
2. This opens a browser terminal as ec2-user — no PEM key needed
3. `cd /home/ec2-user/bench-v2`
4. `./scripts/hooks_toggle.sh off`   # sets size:0, verifies in journalctl
5. Confirm `"size": 0` appears in the gateway log before starting the run
6. From the bench-runner, launch the S-02 run
7. After the run: `./scripts/hooks_toggle.sh on`   # restores all 4 hooks

**CP URL inside the AMI:** `http://localhost:3001`  
**NEXUS_OAUTH_REDIRECT_URI:** `https://<nexus-ami-public-ip>/auth/callback`

**Deprecated — do not use:**  
Running hooks_toggle.sh via `aws ssm send-command` from the runner. Fails silently
(see above). All references to SSM-based toggle are superseded by EC2 Instance Connect.
```

---

## Self-audit before reporting done

**Round 1:**
- Q1: All 4 tasks completed (or explicitly noted partial with reason)?
- Q2: No TODO/FIXME/XXX/stub/unimplemented in production code? (`grep -rn 'TODO\|FIXME\|XXX\|unimplemented\|not implemented\|stub' packages/` on your diff)
- Q3: Tasks 1 and 2 have unit tests. Task 3 (shell) has `--dry-run`. Task 4 is doc-only.
- Q4: No "fix later" deferrals?

**Round 2:** Re-verify each after fixing Round 1 issues.

---

## Do not do

- Do not run the benchmark
- Do not touch `benchmark/v2/scenarios/`, `engine/`, `cli.py`, or result files
  (already handled on `aws_benchmark`)
- Do not commit — ask before committing
- Do not implement `HookConfigCache.Reload()` alloc fix — Tieben is actively
  profiling it; fix design depends on his output (documented in BLOCKED section above)
- Do not touch `looksLikeNDJSON()` — Tieben already has a WIP rewrite; merge conflict
  guaranteed (documented in BLOCKED section above)
- Do not add a VK LRU cache — Tieben confirmed it didn't surface in profiling
