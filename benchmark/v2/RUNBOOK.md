# Benchmark v2 Runbook

The operational checklist for a valid, comparable benchmark run. Every item here maps
to a gate the result-integrity validator enforces — skip one and either the result is
blocked or a caveat rides on it. Deep rationale: [HOOKS_MODE_METHODOLOGY.md](HOOKS_MODE_METHODOLOGY.md)
and [LITELLM_STABILITY_GUIDE.md](LITELLM_STABILITY_GUIDE.md).

---

## Pre-run checklist

- [ ] **`BENCH_UNIQUE_PROMPTS=1`** — appends a per-request nonce so Nexus's broker
      does not coalesce identical prompts. Without it the load is not comparable and
      the validator **BLOCKS** the result.
- [ ] **Sequential run mode** — one gateway at a time. `environment_capture` stamps
      `run_mode: "sequential"`; any other value **BLOCKS**.
- [ ] **`NEXUS_AUDIT_DISABLED` unset** — it drops audit enqueue (an orthogonal
      diagnostic). Present on a governed run → **BLOCK**.
- [ ] **`NEXUS_CP_URL`** set to the Control Plane; **`NEXUS_ADMIN_API_KEY`** set so
      the hooks-off arm can independently read the gateway runtime (otherwise its
      result is flagged unverified and blocked).
- [ ] **LiteLLM runs last** and with a **≥30s warmup** (see the stability guide).
- [ ] Upstream mock reachable and returning bounded responses; the same mock for all
      gateways (no co-location advantage — set the neutral mock IP, not localhost).

## Running

**Standard single-gateway run** (flags override `BENCH_*` env, which override YAML):

```bash
BENCH_UNIQUE_PROMPTS=1 python cli.py run \
  --scenario s02 --gateway bifrost --duration 300 --warmup 30
```

**Nexus hooks-OFF — the VALID path** (toggles via CP, proves runtime reload, restores
after):

```bash
BENCH_UNIQUE_PROMPTS=1 python cli.py run-nexus-hooks-off \
  --scenario s02 --duration 300 --warmup 30
```

Do **not** toggle hooks with a DB edit — it does not propagate to the running
gateway. See the methodology doc for why.

**Full suite** (honors `run_order`, wraps each gateway in resource sampling, forces
LiteLLM last):

```bash
BENCH_UNIQUE_PROMPTS=1 python cli.py run-suite \
  --gateways nexus,bifrost,litellm --scenarios s01,s02
```

## Post-run — validate before quoting anything

```bash
python scripts/validate_benchmark.py results/results_*.json
```

- **BLOCK** = the result is INVALID for comparison/publishing. Do not quote it. Causes:
  hooks-off without runtime proof, residual response-stage hooks, hooks-on with 0
  hooks, `NEXUS_AUDIT_DISABLED` present, `BENCH_UNIQUE_PROMPTS != 1`, non-sequential.
- **WARN** = LiteLLM stability caveats (no warmup / not last / telemetry unavailable /
  p95:p50 over threshold / nonzero timeouts). Read them before quoting the number.

Confirm hooks are restored after a hooks-off run:

```bash
python scripts/nexus_hooks_control.py status
```

If `results/HOOKS_NOT_RESTORED.json` exists, the box may be UNHOOKED — run
`python scripts/nexus_hooks_control.py on` and re-check `status` before any production
traffic.

## Hard rules (carried across all runs)

- No invented RPS/thresholds — RPS is a chosen test input, not a measured result.
- Missing metrics stay `null`/`unavailable`, never a fabricated `0`.
- Secrets (admin key, OAuth password, tokens) are never printed or committed.
- Nothing publishable without methodology sign-off.

## Notes / drift

- `audit_check.py` referenced by older plans is **absent**; the result-integrity gate
  is `scripts/validate_benchmark.py`.
- `cli.py run` and `run-nexus-hooks-off` both accept `--vus/--duration/--warmup`
  (flag > `BENCH_*` env > YAML). This is what the V25_RUN_PLAN commands rely on.
