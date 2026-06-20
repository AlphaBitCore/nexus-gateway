# AWS Deployment — Coworker Handoff + Prompt Library

**Purpose:** everything your coworker needs to take the Nexus Gateway benchmark from the GitHub repo to a running, validated benchmark on AWS — plus a large library of copy-paste prompts (for Claude Code / any AI coding assistant) covering the happy path, every edge case we already hit, fail-safes, and test cases.

**How to use this doc:**
- Sections 1–4 are the human-readable plan (requirements, access, deploy steps).
- Sections 5–9 are **copy-paste prompts**. Each one is self-contained — paste it into the AI assistant on the AWS box and it has enough context to act without seeing this chat.
- Section 10 is the definition of done.

**Source repo:** https://github.com/Kaushik985/nexus-gateway (branch `main`, commit `6b31264`)
The benchmark package lives at `benchmark/v2/`.

**Companion docs already in the repo (read these first):**
- `benchmark/v2/AWS_RUNBOOK.md` — the mechanical step-by-step (this doc is the prompt-driven companion).
- `benchmark/v2/STATUS-2026-06-15.md` — current state, all scenario statuses.
- `benchmark/v2/BENCHMARK_HANDOFF.md` — harness brief + gotchas.
- `benchmark/v2/results/hooks-ab-comparison.md` — the +850 ms TTFT explanation.

---

## 0. Context — what this benchmark is and why it exists

We are producing a **defensible, fair, single-machine comparison** of three AI gateways — **Nexus Gateway**, **LiteLLM**, and **Bifrost** — plus **Nexus-only feature benchmarks** (semantic cache, PII/compliance enforcement). The old v1 benchmark PDFs are baseline references only; they were invalidated because Nexus had caching on (44% hit rate) while the others didn't, the load generator ran off-AWS with uncontrolled jitter, and there was no warmup or config-parity validation.

This v2 harness fixes all of that. We validated it locally (S-01, S-03, S-08, hooks A/B all pass). The AWS run is the **publishable deliverable** — same hardware, same network path, all three gateways one at a time on one controlled instance, at proper sample size (n≥3000).

**Two categories of result that must stay separate:**
1. **Fair raw comparison** (S-01 / S-02 / S-03, cache-disabled): Nexus vs LiteLLM vs Bifrost on identical conditions.
2. **Nexus feature demonstrations** (S-08 cache, S-09 PII): Nexus only. NOT a head-to-head — Nexus is the only gateway with these features.

---

## 1. AWS requirements

### 1.1 Instance
| Requirement | Value / guidance |
|---|---|
| Instance type | A consistent, single instance for all three gateways. Recommend **c6i.2xlarge or m6i.2xlarge** (8 vCPU, 16–32 GB) — enough to run Nexus + LiteLLM + Bifrost containers concurrently without CPU starvation skewing latency. |
| Region | Whatever region the new AMI is published in. Record it. |
| Storage | ≥ 30 GB gp3. The Nexus stack + Postgres + 3 gateways + result artifacts fit comfortably; the bifrost SQLite alone can hit ~50 MB. |
| OS | As shipped in the AMI (likely Amazon Linux 2023 or Ubuntu). Record `cat /etc/os-release`. |
| Network | The benchmark calls **api.openai.com** outbound — confirm the security group / NAT allows egress 443. |

### 1.2 Access the coworker needs BEFORE starting
- [ ] **SSH or SSM access** to the AMI instance (key pair or Session Manager).
- [ ] **The new AMI ID** and the region it's in.
- [ ] **A funded OpenAI org API key** (from James / 1Password). Verify it's funded — see Prompt 5.1.
- [ ] **Nexus Control Plane admin credentials** (email/password) OR an admin API token, to mint a virtual key + set the OpenAI provider credential.
- [ ] **The ports** the AMI exposes: AI Gateway (default 3050), Control Plane (default 3001), Hub (default 3060).
- [ ] **Write access OR a fork** of the repo if they need to push result artifacts back.

### 1.3 Secrets that must NOT be committed
The harness reads everything from `benchmark/v2/.env.local`, which is **gitignored**. Never commit it. Required keys:
```
OPENAI_API_KEY=sk-proj-...          # funded org key
NEXUS_BASE_URL=http://localhost:3050
NEXUS_API_KEY=nvk_...               # virtual key minted against THIS AMI's CP
NEXUS_ADMIN_URL=http://localhost:3001
NEXUS_ADMIN_API_KEY=nxk_...         # optional; OAuth login also works
LITELLM_BASE_URL=http://localhost:4000
LITELLM_API_KEY=sk-local-dev
BIFROST_BASE_URL=http://localhost:8080
BIFROST_API_KEY=local-dev
```

---

## 2. How to push / get the code onto AWS

Two options. Pick one.

**Option A — clone from the fork (simplest):**
```bash
git clone https://github.com/Kaushik985/nexus-gateway
cd nexus-gateway/benchmark/v2
```

**Option B — scp the handoff tarball** (if the box has no GitHub access):
```bash
# on your laptop:
scp /tmp/nexus-benchmark-v2-handoff-*.tar.gz ec2-user@<AMI_PUBLIC_DNS>:/tmp/
# on the box:
cd ~ && tar -xzf /tmp/nexus-benchmark-v2-handoff-*.tar.gz && cd benchmark/v2
```

---

## 3. End-to-end deployment sequence (high level)

1. Launch the AMI; record instance metadata.
2. Confirm the four Nexus services are up (Hub, Control Plane, AI Gateway; Compliance Proxy optional).
3. Set the funded OpenAI key as the Nexus `openai-prod` provider credential.
4. Mint a virtual key against the AMI's Control Plane.
5. Start LiteLLM + Bifrost containers on the same box with the same OpenAI key.
6. Drop all values into `benchmark/v2/.env.local`.
7. Python venv + `pip install -r requirements.txt` (+ `click==8.1.7`).
8. `python cli.py preflight` — must pass before any benchmark.
9. Run the sweep: S-01/S-02/S-03 head-to-head, S-08 cache, hooks A/B, PII demo.
10. Generate the report, pull artifacts off the box, compare to old PDFs.

The exact commands are in `AWS_RUNBOOK.md`. The prompts below drive it and handle everything that can go wrong.

---

## 4. The single most important lesson from local validation

> **Nexus latency only looks "slow" when you measure it wrong.** With the streaming dedupe broker coalescing repeated prompts, Nexus showed a fake 61 ms TTFT. With unique prompts (the harness sets this via `BENCH_UNIQUE_PROMPTS=1`), Nexus's real TTFT p50 is ~1327 ms with compliance hooks ON, and ~367 ms with hooks OFF — *faster than LiteLLM and Bifrost*. The ~960 ms difference is the compliance pipeline, not gateway overhead. **Always run with `BENCH_UNIQUE_PROMPTS=1` for fair comparison, and run the hooks A/B to explain the gap.**

---

# 5. PROMPT LIBRARY — paste these into your AI assistant on the AWS box

> Each prompt is standalone. The assistant on the AWS box has none of our chat history, so the prompts include the context it needs.

## 5.1 — First contact: verify the OpenAI key is funded

```
I'm setting up an AI gateway benchmark on an AWS instance. Before anything else,
verify the OpenAI API key in benchmark/v2/.env.local is funded and not rate-limited.
Run a single direct call to OpenAI (NOT through any gateway):

  curl -s -o /dev/null -w '%{http_code}\n' https://api.openai.com/v1/chat/completions \
    -H "Authorization: Bearer $OPENAI_API_KEY" -H "Content-Type: application/json" \
    -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"max_tokens":3}'

Interpret the result:
- 200  -> funded, proceed.
- 429 with "insufficient_quota" in the body -> the account is OUT OF CREDITS.
  This is NOT a transient rate limit and will not clear by waiting. Stop and tell
  me the account needs billing topped up or a different funded key.
- 401 -> the key is invalid/malformed.
Print the exact status and body, then tell me whether it's safe to proceed.
```

## 5.2 — Bring up and inventory the Nexus stack

```
This AWS instance runs a pre-built Nexus Gateway AMI. Confirm the stack is up and
capture reproducibility metadata. Do all of this and print a summary table:

1. Identify how the services run (systemd or docker compose):
   sudo systemctl status 'nexus-*' 2>/dev/null || docker compose ps 2>/dev/null || docker ps
2. Confirm the AI Gateway (port 3050) and Control Plane (port 3001) respond:
   curl -s -o /dev/null -w 'ai-gateway %{http_code}\n' http://localhost:3050/v1/models
   curl -s -o /dev/null -w 'control-plane %{http_code}\n' http://localhost:3001/healthz
   (anything < 500 means reachable; 401/404 is fine)
3. Capture: instance type (curl http://169.254.169.254/latest/meta-data/instance-type),
   region, cat /etc/os-release, nproc, free -h, docker --version, and the git commit
   or image digest of each Nexus service if available in logs.
Write all of this to benchmark/v2/results/aws_instance_metadata.txt
```

## 5.3 — Set the funded OpenAI key as the Nexus provider credential

```
Nexus stores provider API keys encrypted in its database, separate from the
benchmark's .env.local. The seeded AMI ships a FAKE OpenAI key (sk-fake-...), so
real calls will fail with "Incorrect API key" until I replace it.

Do this:
1. Get an admin token. Either use an NEXUS_ADMIN_API_KEY if one is configured, or
   drive the Control Plane OAuth login flow (see tests/lib/auth.sh in the repo for
   the canonical implementation - it uses admin email/password against
   /oauth/authorize -> /authserver/password -> /oauth/token).
2. Find the openai-prod credential id:
   curl -s "$NEXUS_ADMIN_URL/api/admin/credentials" -H "Authorization: Bearer $TOKEN" \
     | jq '.data[] | select(.name=="openai-prod") | .id'
3. Update it with the funded key from .env.local:
   curl -s -X PUT "$NEXUS_ADMIN_URL/api/admin/credentials/<id>" \
     -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
     -d "{\"apiKey\":\"$OPENAI_API_KEY\"}"
   Expect rotationState: "completed".
4. Verify with a direct streaming call through Nexus (port 3050) using the virtual
   key - it should return 200 with a real completion, NOT "Incorrect API key".
Print each step's result.
```

## 5.4 — Mint a virtual key against THIS AMI's Control Plane

```
The benchmark authenticates to Nexus with a virtual key (nvk_...). A virtual key
from a DIFFERENT Nexus deployment will be rejected with
"vkauth: virtual key invalid" (HTTP 401) - virtual keys are per-database. I need
one minted against THIS AMI.

Do this:
1. Get an admin token (OAuth flow via tests/lib/auth.sh, or NEXUS_ADMIN_API_KEY).
2. Create a virtual key with access to gpt-4o-mini:
   curl -s -X POST "$NEXUS_ADMIN_URL/api/admin/virtual-keys" \
     -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
     -d '{"name":"benchmark-aws","allowedModels":[]}'
   (allowedModels:[] = all models allowed. The response includes the plaintext
    key ONCE under "key".)
3. Confirm the returned key starts with nvk_ and the row shows vkStatus "active".
   If it's "pending", approve it via POST /api/admin/virtual-keys/<id>/approve.
4. Write the nvk_ value into NEXUS_API_KEY in benchmark/v2/.env.local.
Print the key prefix (first 12 chars) and confirm it's written.
```

## 5.5 — Start LiteLLM and Bifrost on the same box

```
For a fair single-machine comparison I need LiteLLM and Bifrost running locally on
this same AWS instance, both pointed at the same funded OpenAI key. The repo ships
their configs.

Start both as Docker containers:

LiteLLM (port 4000):
  docker run -d --name litellm -p 4000:4000 \
    -e OPENAI_API_KEY="$OPENAI_API_KEY" -e LITELLM_MASTER_KEY=sk-local-dev \
    -v "$PWD/gateways/litellm-config.yaml:/app/config.yaml" \
    ghcr.io/berriai/litellm:main-latest --config /app/config.yaml --port 4000

Bifrost (port 8080), config at gateways/bifrost-data/config.json references
env.OPENAI_API_KEY:
  docker run -d --name bifrost -p 8080:8080 \
    -e OPENAI_API_KEY="$OPENAI_API_KEY" -e APP_PORT=8080 -e APP_HOST=<REDACTED-IP> \
    -e LOG_LEVEL=info -e LOG_STYLE=json -e APP_DIR=/app/data \
    -v "$PWD/gateways/bifrost-data:/app/data" \
    maximhq/bifrost:latest

Then verify each returns a real completion:
  curl -s http://localhost:4000/v1/chat/completions -H "Authorization: Bearer sk-local-dev" \
    -H "Content-Type: application/json" \
    -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"say ok"}],"max_tokens":5}'
  curl -s http://localhost:8080/v1/chat/completions -H "Authorization: Bearer local-dev" \
    -H "Content-Type: application/json" \
    -d '{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"say ok"}],"max_tokens":5}'
Both should return a chat completion. Print both.
```

## 5.6 — Python env + the click version trap

```
Set up the Python environment for the benchmark in benchmark/v2/:
  python3 -m venv .venv && source .venv/bin/activate
  pip install -r requirements.txt

IMPORTANT: requirements.txt pins typer==0.12.3 but NOT click. The latest click
(>=8.4) breaks typer's option parsing with errors like
"Option '--scenario' does not take a value". requirements.txt already pins
click==8.1.7 - confirm that pin took:
  pip show click | grep Version
If it shows anything >= 8.2, run: pip install 'click==8.1.7'
Then verify the CLI parses:
  python cli.py run --help
It should print the options table, not an error.
```

## 5.7 — Mandatory preflight before any benchmark

```
Run the benchmark's built-in preflight check from benchmark/v2/ and do not start any
benchmark until it passes:
  python cli.py preflight

It verifies: OpenAI key has quota (one real call), all 3 gateways are reachable,
the Nexus virtual key has the right shape (nvk_ prefix), the Nexus credential
circuit breaker is reset, and a probe call through Nexus returns 200.

If any row shows FAIL, do not proceed - diagnose that specific failure first
(there are dedicated prompts for each failure mode). Print the full pass/fail table.
```

---

# 6. EDGE-CASE / FAIL-SAFE PROMPTS — the things we ALREADY hit locally

> Every one of these is a real failure we debugged this session. If your coworker hits the same symptom, the matching prompt diagnoses and fixes it.

## 6.1 — All Nexus requests return 401 "virtual key invalid"

```
Every request to the Nexus gateway (port 3050) returns HTTP 401 with
"vkauth: virtual key invalid". The OpenAI key is funded and LiteLLM/Bifrost work.
Diagnose: the NEXUS_API_KEY in .env.local is almost certainly a virtual key from a
DIFFERENT Nexus deployment - virtual keys are stored per-database and don't transfer.
Confirm by checking whether a key with that prefix exists in THIS instance's database
(table "VirtualKey", column "keyPrefix"), then mint a fresh virtual key against THIS
Control Plane (POST /api/admin/virtual-keys with an admin token) and write the new
nvk_ value into .env.local. Re-probe to confirm 200.
```

## 6.2 — All Nexus requests return 429, but a single curl works

```
The Nexus gateway returns HTTP 429 (or the benchmark shows ~100% http_4xx at very
high RPS like 400+), but a single manual curl to the gateway returns 200. This is
the credential circuit breaker stuck OPEN from a prior burst of failures (e.g. an
earlier run on a drained OpenAI key). Reset it:
  curl -s -X POST "$NEXUS_ADMIN_URL/api/admin/credentials/<openai-prod-cred-id>/circuit-reset" \
    -H "Authorization: Bearer $TOKEN"
Then send one probe call and confirm 200 before re-running. Note: the benchmark's
cli.py already auto-resets the circuit before each nexus run, so if you're seeing
this, confirm the admin token/URL the harness uses is valid (set NEXUS_ADMIN_URL and
NEXUS_ADMIN_API_KEY, or NEXUS_OPENAI_CREDENTIAL_ID if the credential id differs from
the local-seed default abff2f77-5506-4d73-99a3-6b60ed756bac).
```

## 6.3 — All requests 429 with "insufficient_quota" (the persistent one)

```
Requests fail with 429 and the body says "insufficient_quota" / "You exceeded your
current quota". This is NOT a rate limit and will NOT clear by waiting - the OpenAI
ACCOUNT is out of credits. A new key from the SAME account won't help (quota is
per-account, not per-key). Confirm by calling api.openai.com DIRECTLY (bypass all
gateways) with the key. If it's insufficient_quota, stop and tell me: we need
billing topped up on the OpenAI org account, or a key from a different funded
account. Don't burn time on gateway-side debugging - it's upstream.
```

## 6.4 — All Nexus requests return 403 "pii-detected"

```
Every Nexus request returns HTTP 403 with header
"X-Nexus-Hook: rejected:pii-scanner:pii-detected", even for benign prompts. Cause:
the request contains a digit pattern the pii-scanner's phone-number regex matches
(\b\d{3}[-.\s]?\d{4}\b - any 7+ digit run with an optional separator). We hit this
when a benchmark nonce was all-digits. The shipped harness uses a letter-heavy nonce
(secrets.token_urlsafe) so it should be fine - but if you added a custom prompt set or
changed the nonce in engine/runner.py, make sure no prompt contains 7+ consecutive
digits. Verify with an A/B: send the same prompt with a letters-only suffix vs a
digit-only suffix and compare the X-Nexus-Hook header.
```

## 6.5 — Benchmark shows Nexus at ~60 ms TTFT / 100+ RPS (too good to be true)

```
The Nexus benchmark shows TTFT p50 around 60 ms and RPS over 100, while LiteLLM/Bifrost
are ~400-600 ms and < 1 RPS. This is NOT real performance - it's the Nexus streaming
dedupe broker coalescing repeated prompts into one upstream call and fanning out the
result. The prompt dataset only has ~55 unique prompts, so concurrent VUs collide on
the same cache key. Confirm BENCH_UNIQUE_PROMPTS=1 is set (it makes every request a
unique prompt, defeating the broker). Re-run with it set. Real Nexus TTFT p50 should
land around 1300 ms with compliance hooks on. If it's still ~60 ms, the env var isn't
reaching the runner - check engine/runner.py reads it.
```

## 6.6 — Benchmark reports 100% failures but 0 in every error category

```
A benchmark run shows failed == total_requests, but http_4xx, http_5xx, stream_broken,
and connection_timeouts are all 0. That means requests are throwing exceptions that
fall into the generic handler without a category counter. Most likely an SSE parsing
crash. We hit this when Nexus sent "usage": null in stream chunks and the runner called
.get() on None. The shipped runner.py guards this (usage = chunk.get("usage"); if usage:).
If you're seeing it, check the gateway log for the actual response, and check whether
the runner's streaming parser is crashing on a chunk shape this gateway produces.
Capture one failing request's raw response body to identify the chunk format.
```

## 6.7 — S-03 streaming shows 100% stream_timeouts on one gateway

```
The S-03 streaming-stress scenario shows 100% stream_timeouts against one gateway (we
saw this on local LiteLLM at 11 concurrent streams). The harness is fine - it correctly
classified them as stream_timeouts, not generic failures. The gateway itself is stalling
under concurrent SSE load. Options:
1. Raise request.timeout_seconds to 120 in that gateway's config/<gw>.yaml.
2. Lower concurrency (BENCH_VUS) - though S-03 forces vus=min(30, vus+10) so the floor
   is 11 concurrent.
3. Note it as a real finding: that gateway has a lower streaming-concurrency ceiling.
Re-run S-03 against Nexus and Bifrost too - if they pass at the same concurrency (they
did locally: 0 timeouts), the limitation is gateway-specific, not the harness.
```

## 6.8 — cache_hit_rate comes back null even with caching on

```
S-08 (cache feature) reports cache_hit_rate_pct as null even though Nexus caching is
enabled. Cause: the harness reads the cache-status header, and Nexus emits
"X-Nexus-Cache" while older code only checked "x-cache-status". The shipped runner.py
checks BOTH (X-Nexus-Cache OR x-cache-status). If you still see null, the new AMI may
emit a different header name - inspect the response headers of a cached request:
  curl -sD - -o /dev/null http://localhost:3050/v1/chat/completions -H "Authorization: Bearer $NEXUS_API_KEY" \
    -H "Content-Type: application/json" \
    -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"repeat this exact prompt"}],"max_tokens":10}'
Send it twice; the second should show a cache-hit header. Update engine/runner.py
lines ~200 and ~233 with the actual header name if it differs.
```

## 6.9 — typer CLI errors immediately on any command

```
Running any `python cli.py ...` command fails with a typer/click error like
"Option '--scenario' does not take a value" or "Got unexpected extra arguments".
This is a click version incompatibility - typer 0.12.3 needs click < 8.2. Fix:
  pip install 'click==8.1.7'
Then re-run. This is already pinned in requirements.txt; it only happens if a later
click got installed over it.
```

## 6.10 — Docker container won't start / port already in use

```
A gateway container (litellm or bifrost) fails to start with "port is already
allocated" or exits immediately. Diagnose:
  docker ps -a | grep -E 'litellm|bifrost'
  docker logs litellm --tail 50    (or bifrost)
  sudo lsof -i :4000               (or :8080)
If a stale container holds the port: docker rm -f litellm bifrost, then re-run the
docker run command. If Bifrost crashes on startup, its config.db may be corrupt -
the config.db* files in gateways/bifrost-data/ are runtime artifacts and can be
deleted (Bifrost regenerates them from config.json on next start).
```

## 6.11 — OpenAI egress blocked from the AWS instance

```
Requests to api.openai.com time out or connection-refuse from this AWS box, but the
gateways themselves are up. The security group or NAT is likely blocking outbound 443.
Test raw egress:
  curl -sS -o /dev/null -w '%{http_code}\n' https://api.openai.com/v1/models -H "Authorization: Bearer $OPENAI_API_KEY"
If this hangs or fails, it's a network egress problem, not a benchmark problem. Tell me
so I can fix the security group / route table to allow HTTPS egress to api.openai.com.
```

---

# 7. TEST-CASE PROMPTS — validation to run before trusting any numbers

## 7.1 — Smoke every scenario before the full sweep

```
Before running full benchmarks, smoke-test that every scenario runs end-to-end without
crashing. From benchmark/v2/, run each at minimal load (don't keep the numbers):

  python cli.py validate-all --dry-run      # confirm all 11 scenarios show implemented

Then for s01, s02, s03 (head-to-head) against each of nexus, litellm, bifrost:
  BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=1 BENCH_DURATION=20 BENCH_WARMUP=0 \
    python cli.py run --scenario s0X --gateway <gw> --mode cache-disabled --output /tmp/smoke

And the nexus-only ones:
  python cli.py run --scenario s08 --gateway nexus --mode cache-enabled --output /tmp/smoke
  python demo/pii_compliance_demo.py

Confirm: no python tracebacks, results JSON written for each, TTFT captured for chat
scenarios, cache_hit_rate populated for s08, and 5/5 PII cases reject in the demo.
Report any scenario that crashes BEFORE running the full sweep.
```

## 7.2 — Config-parity check (proves it's a fair comparison)

```
Verify the three gateways are configured identically so the comparison is fair, not
apples-to-oranges. Run:
  python cli.py validate-config --mode cache-disabled
It checks model, streaming mode, and cache state across nexus/litellm/bifrost. All
three must use gpt-4o-mini, stream=true, and caching disabled. If Nexus has caching on
while the others don't, the run must be labeled a Nexus FEATURE benchmark, not a fair
comparison. Print the parity table and confirm all rows match.
```

## 7.3 — The fair head-to-head at full sample size

```
Run the publishable fair comparison. All three gateways, one at a time, on this same
AWS instance, with unique prompts (no cache/broker contamination), at proper sample
size. From benchmark/v2/:

  mkdir -p results/aws
  for SCN in s01 s02 s03; do
    for GW in nexus litellm bifrost; do
      BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=20 BENCH_DURATION=300 BENCH_WARMUP=30 \
        python cli.py run --scenario $SCN --gateway $GW --mode cache-disabled --output results/aws
    done
  done

Each run is ~6 min, so ~54 min total. After each gateway, the harness auto-resets the
Nexus circuit breaker. If any run shows >5% failures, stop and diagnose with the edge-case
prompts before continuing. Report TTFT p50/p95/p99, E2E p50/p95, RPS, and failure rate
per gateway per scenario.
```

## 7.4 — Nexus cache feature (separate, NOT head-to-head)

```
Run the Nexus semantic cache feature benchmark. This is a Nexus-only feature
demonstration - do NOT present it as a head-to-head against LiteLLM/Bifrost (they have
no cache). From benchmark/v2/:
  python cli.py run --scenario s08 --gateway nexus --mode cache-enabled --output results/aws
It runs 3 sub-tests (exact-match, prefix-match, mixed 40/60 traffic). Report cache hit
rate, cache-hit TTFT p95, cache-miss TTFT p95, and TTFT gain. Locally we saw ~100% hit
rate and ~1800 ms TTFT gain. Label the output clearly as a feature benchmark.
```

## 7.5 — Hooks A/B (the compliance-overhead number)

```
Measure how much latency Nexus's compliance pipeline adds, so we can explain why Nexus
TTFT is higher than the thin proxies. From benchmark/v2/:

1. Save current hook state:
   curl -s "$NEXUS_ADMIN_URL/api/admin/hooks" -H "Authorization: Bearer $TOKEN" > /tmp/hooks_before.json
2. Disable all enabled hooks (PUT /api/admin/hooks/<id> {"enabled":false} for each).
3. Reset the Nexus circuit, then run S-01 hooks-OFF:
   BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=20 BENCH_DURATION=300 BENCH_WARMUP=30 \
     python cli.py run --scenario s01 --gateway nexus --mode cache-disabled --output results/aws
4. CRITICAL: re-enable every hook you disabled (even if step 3 fails).
5. Confirm all 4 hooks are back ON.
Compare hooks-OFF TTFT p50 to the hooks-ON S-01 run. Locally the delta was ~960 ms
(72% of total). Report the delta and confirm hooks were restored.
```

## 7.6 — PII / compliance demo (for the James-facing deck)

```
Run the compliance demo that proves Nexus blocks PII while the thin proxies don't.
From benchmark/v2/:
  python demo/pii_compliance_demo.py
It sends 1 clean prompt (expect 200) and 4 prompts with fake PII - SSN, credit card,
phone, email (expect 403 with X-Nexus-Hook: rejected:pii-scanner). Expect 5/5 pass.
It writes evidence to demo/pii_demo_evidence.json. This is the artifact James asked for.
Optionally, send one of the PII prompts through LiteLLM and Bifrost to show they return
200 (no compliance layer) - that contrast is the selling point.
```

## 7.7 — Compare new AWS numbers against the old PDFs

```
Compare the new AWS benchmark results in results/aws/ against the old v1 benchmark PDFs.
Build a table with columns: metric | old PDF Nexus | new AWS Nexus | old PDF LiteLLM |
new AWS LiteLLM | old PDF Bifrost | new AWS Bifrost. Cover: S-01 TTFT p95, S-01 failure
rate, the 7-threshold pass rate (old: Nexus 7/7, Bifrost 3/7, LiteLLM 2/7), Nexus cache
hit rate (old 44.16%), and LiteLLM streaming failure rate (old 28.63%). CRITICALLY: label
which comparisons are fair raw-gateway (S-01/S-02/S-03 cache-disabled) vs Nexus feature
demos (S-08 cache, S-09 PII). The old PDF mixed these - the new report must not.
```

---

# 8. REPORTING + WRAP-UP PROMPTS

## 8.1 — Generate the final report

```
Generate the consolidated benchmark report from the AWS run. From benchmark/v2/:
  python cli.py report --results-dir results/aws --format markdown
Then assemble the final deliverable package containing: the raw results JSON/CSV, the
generated comparison markdown, the hooks A/B result, the PII demo evidence, the instance
metadata file, and a one-paragraph methodology note stating: same machine, unique prompts,
n>=3000, cache disabled for fair comparison, OpenAI quota verified, circuit reset between
runs. Print the list of deliverable files.
```

## 8.2 — Pull artifacts off the box and push results

```
Copy the AWS results directory off the instance to my laptop:
  scp -r ec2-user@<AMI_DNS>:~/nexus-gateway/benchmark/v2/results/aws ./aws-results
Then, if I have write access or a fork, commit the results to a branch and either push
or open a PR. Do NOT commit .env.local or any *.db runtime artifacts (they're gitignored).
Verify no OpenAI/virtual keys leak into the committed files before pushing.
```

---

# 9. SAFETY / GUARDRAIL PROMPTS

## 9.1 — Before committing anything, scan for secrets

```
Before committing or pushing anything from the benchmark directory, scan for leaked
secrets. Search for: sk-proj- / sk- (OpenAI keys), nvk_ (virtual keys), nxk_ (admin
keys), and any password literals. Confirm .env.local is gitignored and NOT staged.
Confirm no *.db runtime artifacts are staged (they're large and regenerated). If
anything sensitive is staged, unstage it and tell me before proceeding.
```

## 9.2 — Don't trust a single low-n run

```
I have one benchmark run per gateway at low sample size (n < 100). Before I report any
ranking beyond TTFT p50, remind me that at n < 500 the p95 is unreliable and p99 is
essentially the single worst sample. We proved this: the same gateway's p99 varied 130%
between two runs. For publishable numbers, runs must be n >= 3000 (BENCH_VUS=20
BENCH_DURATION=300). Only TTFT p50 is stable at low n. Flag any claim I make that the
data can't support.
```

## 9.3 — Always restore state after destructive experiments

```
I'm about to disable Nexus compliance hooks (or change a provider credential, or modify
config) for an experiment. Before I do: save the current state to a file. After the
experiment: restore the original state and verify it's restored. Never leave the gateway
in a modified state - the next person (or the next benchmark) assumes defaults. For hooks
specifically, the pii-scanner being left disabled is a compliance hole.
```

---

# 10. Definition of done (AWS day)

- [ ] `aws_instance_metadata.txt` captured (type, region, OS, CPU, RAM, versions, commits).
- [ ] `python cli.py preflight` → all PASS.
- [ ] S-01 / S-02 / S-03 results for all 3 gateways at n≥3000, <5% failures.
- [ ] S-08 Nexus cache feature result (labeled as feature, not head-to-head).
- [ ] Hooks A/B result + hooks confirmed restored ON.
- [ ] PII demo: 5/5 pass + evidence JSON.
- [ ] Final comparison markdown generated.
- [ ] Old-PDF vs new-AWS comparison table built, fair-vs-feature clearly labeled.
- [ ] Artifacts pulled off the box; no secrets committed.
- [ ] All Nexus hooks ON, no stray modified state, containers cleaned up if needed.

When all boxes tick, send `aws-results/` + the comparison markdown to James + the team. That's the publishable deliverable.

---

## Appendix — quick reference: the env knobs

| Env var | Effect |
|---|---|
| `BENCH_UNIQUE_PROMPTS=1` | Per-request unique nonce. **Required for fair comparison.** |
| `BENCH_VUS=N` | Virtual users (concurrency). 20 for full runs. |
| `BENCH_DURATION=N` | Timed-run seconds. 300 for full runs. |
| `BENCH_WARMUP=N` | Warmup seconds (excluded from metrics). 30 for full runs. |
| `NEXUS_OPENAI_CREDENTIAL_ID` | Override the openai-prod credential id if it differs from the local-seed default. |

Local-seed credential id (likely differs on AWS): `abff2f77-5506-4d73-99a3-6b60ed756bac` — confirm the real one via Prompt 5.3 step 2.

---

# 11. Field notes — real bugs hit on the 2026-06 AWS run (read before deploying)

These are actual failures from the first AWS deployment. Each is environmental
or wiring (not a harness code bug, except where noted), and each has a one-line
avoidance. The two harness code issues (dataset under-padding, preflight health
false-negative) are already fixed in the repo.

| # | Symptom | Cause | Avoid it |
|---|---------|-------|----------|
| 1 | Bifrost provider insert rolled back | SQLite UNIQUE on `config_keys.name`; a second key with `name=''` collided with the existing empty-name key, rolling back the whole txn | Give every Bifrost provider key a **non-empty, unique name** (e.g. `mock-provider-key`). |
| 2 | `curl /health` → no response right after `docker restart bifrost` → false `BIFROST_UNHEALTHY` | Container in `health: starting` (~8s) | **Fixed in `cli.py preflight`** (retries ~12s). For ad-hoc checks, wait/retry or `docker inspect --format '{{.State.Health.Status}}'`. |
| 3 | Couldn't load Nexus CP; added `:3001` to the security group | CP is served by **nginx on :443** (`https://<ip>`); `:3001` is internal only | Use the nginx HTTPS URL. The `:3001` SG rule was unnecessary — check nginx config first. |
| 4 | Dataset copy: "No such file or directory" at `~/v2/` | **SSM runs as root**, so `~` = `/root`, not `/home/ssm-user`; harness is at `/root/benchmark/v2/` | Use absolute paths under SSM; the harness lives at `/root/benchmark/v2/`. |
| 5 | S-02 ran on ~41-token (then ~12.5k-token) prompts | Local dataset stale / `len//4` over-counted real tokens | **Fixed**: dataset now ~16k REAL cl100k tokens. Always pull the committed `long_context_v2.json`; re-pad with `scripts/pad_long_context_dataset.py` (token-targeted). |
| 6 | Heredoc `SSHEOF: command not found` | Opening delimiter quoted `'"SSHEOF"'` but closing was bare `SSHEOF` — they must match exactly | Use identical delimiters: `<<'EOF' … EOF`. Better: use `scripts/hooks_toggle.sh` (no heredoc). |
| 7 | PKCE Python script got 400 on `/oauth/authorize` | `urllib.request` auto-follows the 302 to the callback (which 400s with no code yet) | **Don't hand-roll it** — `scripts/hooks_toggle.sh` already does this correctly (curl `-o /dev/null -w '%{redirect_url}'`, no follow). Reuse it. |
| 8 | Wrong LiteLLM master key | `docker inspect` env order ≠ `.env`; `grep LITELLM_MASTER_KEY` matched `POSTGRES_PASSWORD` first (both long-random) | Read the key from `~/litellm/.env`, or anchor the grep: `grep -E '^LITELLM_MASTER_KEY='`. |
| 9 | SSM "background" tasks returned instantly | The local command only printed the SSM **command ID**; the real work ran inside SSM | Poll the SSM command: `aws ssm get-command-invocation --command-id <id> --instance-id <id>` until `Status` is `Success`. |
| 10 | `hooks_toggle.sh` would hit wrong CP / PKCE would fail | `NEXUS_CP_URL` + `NEXUS_OAUTH_REDIRECT_URI` missing from `.env.local` (it had `NEXUS_BASE_URL`/`NEXUS_ADMIN_URL` only) | **Fixed in `.env.local.example`** (now documents both + the AWS nginx-HTTPS gotcha). Copy the template and fill them. |

## Repo fixes shipped in response to these (commit after this message)

- `scripts/pad_long_context_dataset.py` — now **token-targeted** (sums real cl100k
  tokens via tiktoken-if-available, calibrated fallback otherwise); dataset
  regenerated to ~16,000 real tokens/prompt (was ~12,570). *(Bug 5)*
- `engine/runner.py` — `verify=False` (self-signed Nexus/mock certs). *(supports Bug 3)*
- `cli.py preflight` — health check now retries ~12s + uses `verify=False`, so a
  starting container doesn't false-fail. *(Bug 2)*
- `.env.local.example` — `NEXUS_CP_URL`/`NEXUS_OAUTH_REDIRECT_URI` documented with
  the AWS nginx-HTTPS gotcha. *(Bugs 3, 10)*
