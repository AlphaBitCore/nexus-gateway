# v2.5 Benchmark Run Plan

**Date:** 2026-06-19  
**Status:** Ready to execute — one infrastructure blocker remaining (upstream mock URL update)

---

## Infrastructure state

| Instance | Role | IP | Type | Status |
|----------|------|----|------|--------|
| <REDACTED-INSTANCE-ID> | bench-runner | (runner) | m6i.large | Up |
| <REDACTED-INSTANCE-ID> | Nexus AMI + old mock | (nexus) | t3.xlarge | Up — mock must be re-pointed |
| <REDACTED-INSTANCE-ID> | LiteLLM | <REDACTED-IP> | t3.xlarge | Up — config must update |
| <REDACTED-INSTANCE-ID> | Bifrost | <REDACTED-IP> | t3.xlarge | Up — config must update |
| <REDACTED-INSTANCE-ID> | Neutral mock | <REDACTED-IP> | c6i.xlarge | Up ✓ |

**Neutral mock:** nexus-mock-provider-v2 on port 3062, us-east-1d. Returns OpenAI SSE
with `[DONE]` in under 100ms. Verified working. `stream:true` re-enabled in all
gateway configs.

---

## ONE BLOCKER before Run 1

All three gateways still point at the old mock on the Nexus AMI at `localhost:3062`.
They must be updated to the neutral box at `<REDACTED-IP>:3062`.

**Nexus:** Mock provider URL is stored as a provider credential in the DB. Update via
CP admin API or UI — find the mock-provider credential and change `base_url` to
`http://<REDACTED-IP>:3062`.

**LiteLLM (<REDACTED-IP>:4000):** SSH in → update `api_base` for the mock model in
`config.yaml` → restart.

**Bifrost (<REDACTED-IP>:8080):** SSH in → update mock provider `base_url` in Bifrost
config → set `allow_private_network: true` on that provider (Bifrost rejects private
IPs by default — Tieben flagged this) → restart.

**Who does this:** Kash (SSH access to LiteLLM + Bifrost) or Tieben (CP admin API
for Nexus credential). SSM only reaches runner and mock box.

---

## Run sequence

### Run 1 — stream:true re-baseline (all 4 gateways, S-02)
**Prerequisite:** blocker above resolved  
**Purpose:** New ground truth. Neutral mock + stream:true replaces all v2 numbers.
New mock returns bounded responses so all gateways will be faster. Every downstream
comparison (per-hook, VU sweep, before/after) is relative to this baseline.

```bash
# Sequential — one gateway at a time
python cli.py run --scenario s02 --gateway bifrost   --duration 300 --warmup 30
python cli.py run --scenario s02 --gateway litellm   --duration 300 --warmup 30
python cli.py run --scenario s02 --gateway nexus     --duration 300 --warmup 30  # hooks-ON
# hooks-OFF: run hooks_toggle.sh off on Nexus AMI first (EC2 Instance Connect, not SSM)
python cli.py run --scenario s02 --gateway nexus     --duration 300 --warmup 30  # hooks-OFF
```

**Expected:** Nexus hooks-OFF RPS increases (bounded mock response). Bifrost remains
the ceiling. All numbers shift — update the full report after this run.

---

### Run 2 — p95 tail confirmation (Nexus hooks-OFF, audit disabled)
**Prerequisite:** `NEXUS_AUDIT_DISABLED=1` flag merged (see
`CLAUDE-CODE-V25-RUN2-AUDIT-DISABLE.md`)  
**Purpose:** Confirm GC pause hypothesis. Tieben's profiling shows GC STW pauses of
200–900ms matching the p95/p99 tail (183ms p95, 377ms p99 in v2 S-02). Disabling
Enqueue skips body retention on heap. If p95 collapses → ~20ms: GC is confirmed as
the driver. If p95 stays high: the audit path is not the cause.

```bash
# On Nexus AMI:
NEXUS_AUDIT_DISABLED=1 systemctl restart nexus-ai-gateway
journalctl -u nexus-ai-gateway -n 5 | grep AUDIT_DISABLED  # confirm startup warn

# From runner:
python cli.py run --scenario s02 --gateway nexus --duration 300 --warmup 30  # hooks-OFF
```

**Expected:** p95 drops from 183ms → 20–30ms if hypothesis holds.

---

### Run 3 — per-hook isolation (Nexus only, S-02 × 4)
**Prerequisite:** Run 1 complete (new baseline), `per_hook_sweep.sh` merged  
**Purpose:** Identify which hook owns the 220ms compliance cost. Tieben's hypothesis:
`response-quality-signals` owns ~160ms due to SSE hold-back buffering.

```bash
# On Nexus AMI (EC2 Instance Connect as ec2-user):
cd /home/ec2-user/bench-v2
./scripts/per_hook_sweep.sh
# Produces 4 result sets: pii-scanner / keyword-blocker / response-quality-signals / noop-baseline
```

**Expected:** response-quality-signals run will show highest TTFT delta vs hooks-OFF
baseline. noop-baseline run isolates the hook framework overhead itself.

---

### Run 4 — S-01 short-context (all 4 gateways)
**Prerequisite:** Run 1 complete  
**Purpose:** Complete the scenario matrix. Shows how the compliance cost and gateway
gap scale with prompt size (100-token prompts vs 12,570-token S-02 prompts).

```bash
python cli.py run --scenario s01 --gateway bifrost   --duration 300 --warmup 30
python cli.py run --scenario s01 --gateway litellm   --duration 300 --warmup 30
python cli.py run --scenario s01 --gateway nexus     --duration 300 --warmup 30  # hooks-ON
python cli.py run --scenario s01 --gateway nexus     --duration 300 --warmup 30  # hooks-OFF
```

---

### Run 5 — VU sweep (Nexus hooks-OFF, S-02)
**Prerequisite:** Run 1 complete. Upgrade instances to c6i.2xlarge first — t3.xlarge
is burstable and throttles silently under sustained high-VU load (depletes CPU credits
→ throttles to ~1.6 vCPU with no warning in CloudWatch).

**Purpose:** Find RPS ceiling and p95 behavior at higher concurrency.

```bash
# Upgrade all instances: stop → change instance type to c6i.2xlarge → start
# Then:
BENCH_VUS=6  python cli.py run --scenario s02 --gateway nexus --duration 300 --warmup 30
BENCH_VUS=12 python cli.py run --scenario s02 --gateway nexus --duration 300 --warmup 30
BENCH_VUS=24 python cli.py run --scenario s02 --gateway nexus --duration 300 --warmup 30
```

**Expected:** RPS scales with VUs until hitting the bottleneck. p95 will show where
the ceiling is.

---

### Run 6 — before/after alloc benchmark (Nexus hooks-OFF)
**Prerequisite:** Tieben's WIP zero-alloc build  
**Blocked until:** Tieben shares his branch

**Purpose:** Quantify the allocation fix gains. Two fixes in his build:
1. `looksLikeNDJSON()` rewrite — `generic_http.go` lines 448–481, zero-alloc byte
   scan replacing 64KB scanner + `json.Unmarshal` per request
2. `HookConfigCache.Reload()` alloc reduction — ~20% of request-path allocs

```bash
# Run S-02 hooks-OFF on current binary → record RPS/p50/p95
# Swap in Tieben's binary → restart gateway → run again → diff
```

**Expected:** RPS increase, p95 improvement from reduced GC pressure.

---

## Known bugs / things to watch during runs

| # | Issue | Mitigation |
|---|-------|------------|
| B1 | hooks_toggle.sh fails silently via SSM (runner-side) | Always run on Nexus AMI via EC2 Instance Connect as ec2-user |
| B2 | Bifrost rejects private IPs by default | Set `allow_private_network: true` on mock provider before Run 1 |
| B3 | t3.xlarge CPU credit exhaustion under high VU | Upgrade to c6i.2xlarge before Run 5 |
| B4 | S-02 dataset: validate long_context_v2.json is padded (650,997 bytes, not 2,794) | Preflight guard now in s02_long_context.py — will SystemExit if stub |
| B5 | BENCH_UNIQUE_PROMPTS=1 required to defeat Nexus streaming dedup broker | Set in .env.local — confirm before each run |
| B6 | Mock co-location: if MOCK_PROVIDER_URL=localhost, Nexus has unfair upstream RTT advantage | Now prints methodology warning — should show neutral IP after blocker resolved |
| B7 | SSM send-command returns immediately (work runs async in SSM) | Use --no-wait + poll get-command-invocation, or stream output via SSM |
| B8 | S3 bucket must be pre-created in CloudShell before runner can upload results | `aws s3 mb s3://nexus-bench-results-kash --region us-east-1` from CloudShell |
| B9 | Invalid run a4601b32 still in results/ alongside valid runs | Quarantined in results/invalid/README.md — will move files when pulled from runner |

---

## Open result files (still on AWS runner)

Run IDs on `<REDACTED-INSTANCE-ID>` at `/root/benchmark/v2/results/`:
`0e30a3ef`, `71136241`, `730f9815`, `a4601b32` (invalid), `bd89b7da`

JSON + CSV for each (10 files). Retrieve via:
```bash
aws ssm send-command --instance-id <REDACTED-INSTANCE-ID> \
  --document-name AWS-RunShellScript \
  --parameters 'commands=["aws s3 cp /root/benchmark/v2/results/ s3://nexus-bench-results-kash/v2-s02/ --recursive"]'
# Then from CloudShell:
aws s3 sync s3://nexus-bench-results-kash/v2-s02/ ./v2-s02-results/
```

---

## Codebase tasks status

| Task | File | Status |
|------|------|--------|
| NEXUS_TRACE_LATENCY=1 flag | `proxy.go` | Merged (Tieben confirmed via aws_benchmark branch) |
| per_hook_sweep.sh | `benchmark/v2/scripts/` | Merged |
| AWS_RUNBOOK.md update | `benchmark/v2/` | Merged |
| S-02 dataset preflight | `scenarios/s02_long_context.py` | Merged |
| Mock co-location note | `scenarios/s02_long_context.py` | Merged |
| **NEXUS_AUDIT_DISABLED=1 flag** | `proxy.go` | **Pending — see CLAUDE-CODE-V25-RUN2-AUDIT-DISABLE.md** |
| looksLikeNDJSON() rewrite | `generic_http.go` | Blocked on Tieben's WIP |
| HookConfigCache.Reload() fix | `config_cache.go` | Blocked on Tieben's profiling |
