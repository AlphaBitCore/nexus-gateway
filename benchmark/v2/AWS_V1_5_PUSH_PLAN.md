# AWS Benchmark v1.5 — Push Plan & Teammate Briefing

**Date:** 2026-06-16  
**Author:** Kash  
**Purpose:** (1) Feedback on the June 16 AWS benchmark run, (2) complete methodology spec for the v1.5 re-run that produces publishable numbers. Long-context (S-02) is a separate workstream — not covered here.

---

## Part 1 — Feedback on the June 16 AWS Run

Hey — good work getting all three gateways running on EC2 and producing a clean 0% error run. That confirms the stack is stable on Linux/systemd and the Docker setup for LiteLLM/Bifrost works. Those are real and useful findings.

That said, there are three specific issues that prevent these numbers from going into any external communication as a fair comparison. Walking through each:

### Issue 1: The simple-scenario table shows Nexus as the fastest gateway

From your report:

| Gateway | TTFT p50 — simple scenario |
|---------|--:|
| **Nexus** | **1275 ms** |
| LiteLLM | 1313 ms |
| Bifrost | 1301 ms |

Nexus has PII scanner + keyword blocker running synchronously on every request. Those hooks alone measured **960 ms of overhead** in our local A/B (Nexus hooks ON: 1327 ms p50 → hooks OFF: 367 ms p50). For Nexus to be *faster* than the thin proxies on the same upstream is physically inconsistent with the hooks being active. This is the clearest sign that the measurement is capturing something other than gateway performance — almost certainly CPU contention (see Issue 2).

### Issue 2: t3.medium + simultaneous gateways = CPU contention artifact

t3.medium is 2 burstable vCPUs. Your run placed on that single instance simultaneously:

- Nexus AI Gateway (Go, multi-goroutine)
- LiteLLM (Docker container, Python)
- Bifrost (Docker container)
- Go load generator (`tools/loadtest`)

Under that load, the 4 processes compete for 2 vCPUs. The load generator's timing loop gets starved intermittently, adding artificial queueing delay to whichever gateway it's waiting on. The result: all three gateways converge toward the same apparent ~1.2–1.3 s TTFT with a 120 ms spread, instead of the 900 ms spread we see when each gateway runs in isolation.

This is not "Nexus got faster on AWS." It's "LiteLLM and Bifrost got slower because the machine was saturated."

Our local S-01 results, confirmed independently on the same day by two engineers:

| Gateway | TTFT p50 — local (isolated) | TTFT p50 — AWS (contended) |
|---------|--:|--:|
| Nexus | 1327–1342 ms | 1305 ms |
| LiteLLM | 454–517 ms | 1282 ms |
| Bifrost | 406–419 ms | 1185 ms |

LiteLLM jumped from ~480 ms to ~1282 ms. Bifrost jumped from ~410 ms to ~1185 ms. Nexus barely moved. The contention is inflating the thin proxies, not improving Nexus.

### Issue 3: The hooks overhead estimate in the notes is wrong

Your notes say: *"Disabling hooks would reduce Nexus latency by approximately 80-100ms based on prior local testing."*

The measured number is **960 ms**, not 80–100 ms. Here are the exact numbers from our hooks A/B run (June 15, same harness, same model, same 3 VU × 60 s config):

| Condition | TTFT p50 | TTFT p95 |
|-----------|--:|--:|
| Hooks ON (pii-scanner + keyword-blocker) | 1327 ms | 2275 ms |
| Hooks OFF | 367 ms | 891 ms |
| **Delta** | **960 ms** | **1384 ms** |

80–100 ms would be a footnote. 960 ms is the core story — it's what makes Nexus different from a pass-through proxy, and it's the overhead customers are trading for compliance enforcement. We should present it accurately, not minimize it.

### Issue 4: TTFT metric mismatch — `tools/loadtest` vs `benchmark/v2`

`tools/loadtest` (Go harness) measures full round-trip time, not streaming TTFT. For non-streaming requests, those are the same thing. For streaming requests, true TTFT is the time until the *first SSE token arrives*, which is ~200–600 ms earlier than E2E. The `benchmark/v2` Python harness measures real TTFT via SSE chunk parsing. The two harnesses produce different numbers for the same gateway under streaming load, so they can't be compared directly. All publishable comparison numbers need to come from the same harness.

### What to keep from this run

- 0% error rate across all three gateways on AWS — confirmed and valuable
- Absolute TTFT range (~1.2–1.3 s) for Nexus is consistent with local measurements
- The EC2 + systemd + Docker setup works end-to-end — no environment issues to solve before v1.5

---

## Part 2 — AWS v1.5 Re-Run Specification

This is the complete spec for the benchmark run that produces publishable v1 comparison numbers on AWS.

### 2.1 Infrastructure — one instance, sequential runs

**You do not need separate instances.** The problem with the June 16 run was not using one instance — it was running all three gateways *simultaneously*. Stop the other two gateway processes before each measurement window and contention disappears entirely.

**Use the existing instance, resized to t3.xlarge.**

| Instance | Type | vCPU | RAM | Cost |
|----------|------|-----:|----:|------|
| Existing `bench` instance (resized) | t3.xlarge | 4 | 16 GB | ~$0.17/hr |

Total cost for a 3-hour benchmark session: **~$0.50**. No budget approval needed — this is within any AWS dev account's loose change.

**Why t3.xlarge and not the current t3.medium?**  
4 vCPUs give Nexus's compliance hook goroutines room to breathe alongside the main request path, which is closer to a real deployment. t3.medium (2 vCPU) undersizes Nexus specifically. The resize takes ~2 minutes (stop instance → change type → start).

**How to resize:**
```bash
# Stop the instance first (from AWS console or CLI)
aws ec2 stop-instances --instance-ids <your-instance-id>

# Change type
aws ec2 modify-instance-attribute \
  --instance-id <your-instance-id> \
  --instance-type t3.xlarge

# Start it back up
aws ec2 start-instances --instance-ids <your-instance-id>
```

### 2.2 Software versions — pin everything

Before starting, record:

```bash
# On bench-nexus
git rev-parse HEAD          # Nexus commit SHA
./nexus-gateway --version   # build version string

# On bench-litellm
docker inspect ghcr.io/berriai/litellm:main-latest --format '{{.Id}}'

# On bench-bifrost
docker inspect maximhq/bifrost:latest --format '{{.Id}}'

# On bench-runner
python --version
pip show httpx httpx-sse numpy
git rev-parse HEAD          # harness commit SHA
```

These go in the report header. Without them, the run is not reproducible.

### 2.3 Harness — benchmark/v2 only

Use `benchmark/v2/cli.py` exclusively. Do not mix results from `tools/loadtest`.

```bash
# On bench-runner — install
git clone <repo> nexus-gateway
cd nexus-gateway/benchmark/v2
pip install -r requirements.txt

# Set env
cp .env.local.example .env.local
# Edit .env.local:
#   NEXUS_BASE_URL=http://<bench-nexus-private-ip>:3050
#   LITELLM_BASE_URL=http://<bench-litellm-private-ip>:4000
#   BIFROST_BASE_URL=http://<bench-bifrost-private-ip>:8080
#   NEXUS_API_KEY=<virtual key from Nexus CP>
#   LITELLM_API_KEY=sk-local-dev
#   BIFROST_API_KEY=local-dev
#   OPENAI_API_KEY=<real key>
#   BENCH_UNIQUE_PROMPTS=1
```

### 2.4 Required environment flags

Every run must have these set:

| Flag | Value | Why |
|------|-------|-----|
| `BENCH_UNIQUE_PROMPTS` | `1` | Injects a per-request hex nonce — defeats Nexus streaming dedup broker coalescing. Without this, Nexus serves cached stream responses (~60 ms fake TTFT instead of real ~1.3 s). |
| `BENCH_VUS` | `20` | Minimum for stable p50/p95 estimates. |
| `BENCH_DURATION` | `300` | 300 s at 20 VUs = ~3000 requests per gateway. p95 becomes reliable at n≥500; p99 at n≥2000. |
| `BENCH_WARMUP` | `30` | 30 s warmup fills connection pools before measurement window opens. |

### 2.5 Run order, sequential isolation, and preflight checklist

**The single most important change from the June 16 run: stop the other two gateways before starting each measurement.** This is what "sequential" means — only one gateway is running at any given time.

```bash
# Pattern for each run:
# 1. Stop the two gateways you are NOT measuring
docker stop litellm bifrost   # before Nexus run
# OR
docker stop litellm && systemctl stop nexus-gateway  # before Bifrost run
# etc.

# 2. Confirm only the target gateway is running
docker ps
systemctl is-active nexus-gateway

# 3. Run the benchmark
python cli.py run --scenario s01 --gateway <target> --mode cache-disabled

# 4. Start the next gateway's containers before its run
docker start litellm
```

Run this checklist before each gateway's measurement window. Do not skip steps — each one has caused a failed or invalid run in prior sessions.

**Before Nexus run:**
- [ ] `POST http://<nexus>:3050/api/admin/credentials/openai-prod/circuit-reset` — confirm 200 response. A tripped circuit breaker causes 100% failures that look like gateway slowness.
- [ ] Confirm hooks are active: `GET /api/admin/hooks` — response should list `pii-scanner` and `keyword-blocker` as enabled.
- [ ] Note hook state in the report (hooks ON / hooks OFF).
- [ ] Confirm virtual key is valid: `curl -H "Authorization: Bearer $NEXUS_API_KEY" http://<nexus>:3050/v1/models` — should return 200.

**Before LiteLLM run:**
- [ ] `docker ps` — confirm container is running and healthy.
- [ ] `curl http://<litellm>:4000/health` — confirm 200.

**Before Bifrost run:**
- [ ] `docker ps` — confirm container is running and healthy.
- [ ] `curl http://<bifrost>:8080/health` — confirm 200.

**Run order:** LiteLLM → Bifrost → Nexus (hooks ON) → Nexus (hooks OFF). Running Nexus last avoids any circuit-breaker state carrying over from a prior run. The inter-run gap should be under 10 minutes so OpenAI API conditions are comparable.

### 2.6 Scenarios for v1.5

These are the scenarios to run for the v1.5 AWS push. Long-context (S-02) is explicitly excluded — that's a separate workstream.

| # | Scenario | Purpose | Required? |
|---|---------|---------|----------|
| S-01 | Short chat, cache-disabled | Core 3-way latency comparison | **Yes — primary publishable result** |
| S-01-B | Short chat, Nexus hooks OFF | Isolates compliance overhead from routing overhead | **Yes — required to support causal claim** |
| S-04 | Concurrency sweep (1/5/10/20/50 VU) | Shows how each gateway degrades under load | Yes |
| S-05 | Sustained load (20 VU × 300 s) | Confirms no drift / memory leak over time | Yes |
| S-08 | Semantic cache hit rate | Nexus differentiator — TTFT gain on repeat prompts | Yes |
| S-09 | PII compliance enforcement | Nexus differentiator — block rate on SSN/CC payloads | Yes |
| S-03 | Streaming TTFT (SSE) | Streaming-specific latency | Nice to have |
| S-06 | Error recovery | Circuit breaker behavior under upstream 429 flood | Nice to have |

S-01 + S-01-B are the non-negotiable pair. Every other scenario adds depth, but without S-01-B you cannot claim "X ms overhead is compliance" — you're just claiming overhead exists.

### 2.7 S-01-B: hooks OFF run

This is a new named run, not a separate scenario file. It's S-01 with hooks disabled on the Nexus side:

```bash
# 1. Disable hooks via admin API
curl -X PATCH http://<nexus>:3050/api/admin/hooks/pii-scanner \
  -H "Authorization: Bearer $NEXUS_ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"enabled": false}'

curl -X PATCH http://<nexus>:3050/api/admin/hooks/keyword-blocker \
  -H "Authorization: Bearer $NEXUS_ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"enabled": false}'

# 2. Run S-01 with mode label "hooks-off"
BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=20 BENCH_DURATION=300 BENCH_WARMUP=30 \
  python cli.py run --scenario s01 --gateway nexus --mode cache-disabled \
  --label "hooks-off"

# 3. Re-enable hooks immediately after
curl -X PATCH http://<nexus>:3050/api/admin/hooks/pii-scanner \
  -H "Authorization: Bearer $NEXUS_ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"enabled": true}'

curl -X PATCH http://<nexus>:3050/api/admin/hooks/keyword-blocker \
  -H "Authorization: Bearer $NEXUS_ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"enabled": true}'
```

Expected result: Nexus hooks-OFF p50 should land in the 350–450 ms range (matching LiteLLM/Bifrost), confirming that the compliance pipeline — not routing or infra — accounts for the ~900 ms gap.

If hooks-OFF Nexus is still ≥800 ms, that's a new finding: the gap is *not* compliance overhead, it's something structural. That would need investigation before v1 ships (routing evaluation? synchronous traffic_event write before first token? adapter overhead?). Better to know now than after launch.

### 2.8 Report template

Every result file must open with this header before any data table:

```
## Run preflight

| Field | Value |
|-------|-------|
| Date | |
| Nexus commit SHA | |
| LiteLLM image digest | |
| Bifrost image digest | |
| Harness commit SHA | |
| BENCH_UNIQUE_PROMPTS | 1 |
| BENCH_VUS | 20 |
| BENCH_DURATION | 300 |
| BENCH_WARMUP | 30 |
| Circuit breaker reset (Nexus) | Y — response: <paste 200 body> |
| Hooks active (Nexus) | pii-scanner: Y / keyword-blocker: Y |
| Run order | LiteLLM → Bifrost → Nexus (hooks ON) → Nexus (hooks OFF) |
| Inter-run gap | <N> minutes |
| OpenAI RPM observed (start) | |
| OpenAI RPM observed (end) | |
```

No result table is publishable without this header. If any field is unknown or was skipped, mark it `UNKNOWN — see notes` and explain why. Don't leave it blank.

**Percentile disclosure rule:** p95 is reportable at n≥500. p99 is reportable at n≥2000. At 20 VU × 300 s we expect ~2500–3000 requests per gateway, so p95 and p99 are both reportable.

**No bolded winners in tables.** Present raw numbers. Interpretation goes in a clearly labeled section below the table.

### 2.9 What publishable numbers should look like

Based on everything measured so far, the expected AWS v1.5 results at 20 VU × 300 s from a dedicated runner instance:

| Gateway | Condition | TTFT p50 (expected) | TTFT p95 (expected) |
|---------|---------|--:|--:|
| LiteLLM | No hooks | 400–550 ms | 900–1400 ms |
| Bifrost | No hooks | 380–500 ms | 850–1300 ms |
| Nexus | Hooks ON | 1100–1400 ms | 2000–2800 ms |
| Nexus | Hooks OFF | 350–500 ms | 800–1300 ms |

If Nexus hooks-OFF lands in the same range as LiteLLM/Bifrost, the story is: *Nexus matches thin-proxy latency without compliance; the compliance pipeline costs ~900 ms — a tradeoff customers explicitly opt into.*

That is a publishable, defensible, honest result.

---

## Part 3 — What Is Out of Scope for v1.5

To be explicit about what this push does NOT cover:

- **S-02 long-context** — separate workstream. Dataset needs true 16k-token padding; full run deferred.
- **S-07 multi-region** — not planned for v1.
- **S-10 / S-11** — advanced policy scenarios; post-v1.
- **Production traffic replay** — post-v1.
- **Cost analysis** — post-v1.

The v1.5 AWS push is purely: S-01 + S-01-B + S-04 + S-05 + S-08 + S-09, on dedicated instances, with the Python v2 harness, at n≥2500.

---

## Summary checklist for teammate

- [ ] Spin up 4 EC2 instances (3 gateways + 1 runner), same AZ
- [ ] Pin and record all software versions before starting
- [ ] Install `benchmark/v2` on runner instance, configure `.env.local`
- [ ] Set `BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=20 BENCH_DURATION=300 BENCH_WARMUP=30`
- [ ] Run preflight checklist before each gateway (circuit reset, health check, hook state)
- [ ] Run order: LiteLLM → Bifrost → Nexus (hooks ON) → Nexus (hooks OFF)
- [ ] Fill in the report header template (§2.8) for every result file
- [ ] Do not report p95/p99 if n < 500/2000
- [ ] No bolded winners; interpretation in a separate labeled section
- [ ] Send raw JSON results files alongside the markdown report

Questions / blockers: ping Kash directly.
