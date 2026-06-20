# AWS Day Runbook — Nexus Gateway Benchmark Verification

**Audience:** the person running benchmarks against the new Nexus AMI tomorrow.
**Goal:** end-to-end execution, not discovery. Every step has an expected output.

> **Read before starting:**
> - `BENCHMARK_HANDOFF.md` (one-page brief, gotchas)
> - `MERGE_LOG.md` (what changed today + why)
> - `results/bias-and-methodology-review-s01.md` (reporting standard)
> - `results/hooks-ab-comparison.md` (the +850 ms TTFT explanation)

---

## Phase 0 — Inventory check (do this BEFORE booting the AMI)

```bash
# 1. Confirm tarball is at hand
ls -la /tmp/nexus-benchmark-v2-handoff-*.tar.gz
shasum -a 256 /tmp/nexus-benchmark-v2-handoff-*.tar.gz

# 2. Confirm you have the funded OpenAI org key (from James / 1Password)
# 3. Confirm AWS access:
#    - SSH key or SSM access to the AMI instance
#    - Which port the AI Gateway listens on (default 3050)
#    - Which port Control Plane listens on (default 3001)
#    - The instance's public IP / DNS

# 4. Confirm Nexus team has provisioned a virtual key in the AMI's CP
#    OR confirm you have admin credentials to mint one yourself
```

If any of the above is missing → stop and chase it before continuing.

---

## Phase 1 — Bring up the AMI

```bash
# 1. Launch the AMI in the chosen region
#    Record: instance type, region, OS, kernel, CPU, RAM
INSTANCE_ID=i-XXXXXXXXXXXXXXXXX
REGION=us-west-2
PUBLIC_DNS=ec2-XX-XX-XX-XX.us-west-2.compute.amazonaws.com

# 2. SSH in
ssh ec2-user@$PUBLIC_DNS

# 3. Confirm services are up
#    (Nexus team will document the exact systemd unit names / docker compose file)
sudo systemctl status nexus-ai-gateway nexus-control-plane nexus-hub
# or
docker compose ps

# 4. Confirm the AI gateway responds
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:3050/v1/models
# Expected: 401 (no auth) or 200 — anything non-5xx confirms reachable

# 5. Capture build info
cat /etc/os-release
docker --version
docker compose version
# Record: AMI version / image digest / git commit of each service
sudo cat /var/log/nexus-ai-gateway.log | head -20  # boot info incl. version
```

---

## Phase 2 — Configure the AMI for benchmarking

```bash
# 1. Set the funded OpenAI key as the openai-prod credential
#    Get an admin OAuth token first
NEXUS_CP_URL=http://localhost:3001
NEXUS_ADMIN_EMAIL=admin@<your-org>
NEXUS_ADMIN_PASSWORD=<from secrets>
# Drive the OAuth flow — see tests/lib/auth.sh for the canonical implementation,
# or use a one-shot Python client; output an access token into $TOKEN

# 2. Find the openai-prod credential id
curl -s "$NEXUS_CP_URL/api/admin/credentials" -H "Authorization: Bearer $TOKEN" \
  | jq '.data[] | select(.name=="openai-prod") | .id'
# Save as $CRED_ID

# 3. Update the credential with the funded key
OPENAI_API_KEY="sk-proj-..."  # from James / 1Password
curl -s -X PUT "$NEXUS_CP_URL/api/admin/credentials/$CRED_ID" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d "{\"apiKey\":\"$OPENAI_API_KEY\"}" | jq '.rotationState'
# Expected: "completed"

# 4. Mint a benchmark virtual key (or use an existing one)
curl -s -X POST "$NEXUS_CP_URL/api/admin/virtual-keys" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"benchmark-aws","allowedModels":[]}' | jq '.key'
# Expected: an nvk_... value. Copy it.

# 5. Confirm hooks are at default state
curl -s "$NEXUS_CP_URL/api/admin/hooks" -H "Authorization: Bearer $TOKEN" \
  | jq '.data[] | {name, enabled, stage, failBehavior}'
# Expected: noop-baseline, pii-scanner, keyword-blocker, response-quality-signals
# all enabled (this is the hooks-ON baseline)
```

---

## Phase 3 — Unpack the harness and configure

```bash
# 1. Copy the handoff tarball to the AMI instance (from your laptop)
scp /tmp/nexus-benchmark-v2-handoff-*.tar.gz ec2-user@$PUBLIC_DNS:/tmp/

# 2. On the instance, unpack
ssh ec2-user@$PUBLIC_DNS
cd ~
tar -xzf /tmp/nexus-benchmark-v2-handoff-*.tar.gz
cd benchmark/v2

# 3. Python env
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
# NB: requirements.txt pins click==8.1.7 — required for typer 0.12.3

# 4. Configure .env.local from the example
cp .env.local.example .env.local
nano .env.local  # set:
#   OPENAI_API_KEY=<funded key>
#   NEXUS_BASE_URL=http://localhost:3050
#   NEXUS_API_KEY=<nvk_... from Phase 2 step 4>
#   NEXUS_ADMIN_URL=http://localhost:3001
#   NEXUS_ADMIN_API_KEY=<admin api key if available; OAuth flow otherwise>
#   LITELLM_BASE_URL=http://localhost:4000   (start container if needed)
#   LITELLM_API_KEY=sk-local-dev
#   BIFROST_BASE_URL=http://localhost:8080
#   BIFROST_API_KEY=local-dev

# 5. Start LiteLLM and Bifrost on the same AMI (for fair-comparison)
docker run -d --name litellm -p 4000:4000 \
  -e OPENAI_API_KEY="$OPENAI_API_KEY" -e LITELLM_MASTER_KEY=sk-local-dev \
  -v "$PWD/gateways/litellm-config.yaml:/app/config.yaml" \
  ghcr.io/berriai/litellm:main-latest --config /app/config.yaml --port 4000

docker run -d --name bifrost -p 8080:8080 \
  -e OPENAI_API_KEY="$OPENAI_API_KEY" -e APP_PORT=8080 -e APP_HOST=<REDACTED-IP> \
  -e LOG_LEVEL=info -e LOG_STYLE=json -e APP_DIR=/app/data \
  -v "$PWD/gateways/bifrost-data:/app/data" \
  maximhq/bifrost:latest

sleep 10
docker ps | grep -E "litellm|bifrost"
```

---

## Phase 4 — Preflight (MANDATORY)

```bash
cd ~/benchmark/v2 && source .venv/bin/activate
python cli.py preflight
# Expected: PASS on OpenAI quota, all 3 gateways reachable, nexus VK shape OK,
#           nexus probe call returns 200
# If any FAIL → STOP. Resolve before continuing.

python cli.py validate-all --dry-run
# Confirms all 11 scenarios show ✅ implemented
```

---

## Phase 5 — The benchmark sweep (full deliverable)

### 5a. Fair head-to-head (S-01 / S-02 / S-03) at full n

```bash
mkdir -p results/aws-$(date +%Y%m%d)
RESULTS=results/aws-$(date +%Y%m%d)

# S-01 short chat — 20 VU × 300 s × 30 s warmup ≈ ~3,000 reqs/gateway
for GW in nexus litellm bifrost; do
  BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=20 BENCH_DURATION=300 BENCH_WARMUP=30 \
    python cli.py run --scenario s01 --gateway $GW --mode cache-disabled --output $RESULTS
done

# S-02 long context — scenario internally halves VUs to protect upstream
for GW in nexus litellm bifrost; do
  BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=20 BENCH_DURATION=300 BENCH_WARMUP=60 \
    python cli.py run --scenario s02 --gateway $GW --mode cache-disabled --output $RESULTS
done

# S-03 streaming stress — scenario forces vus=min(30, vus+10)
# Raise timeout to 120 s in nexus.yaml / litellm.yaml / bifrost.yaml if S-03 stream_timeouts is high
for GW in nexus litellm bifrost; do
  BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=20 BENCH_DURATION=300 BENCH_WARMUP=30 \
    python cli.py run --scenario s03 --gateway $GW --mode cache-disabled --output $RESULTS
done
```

Expected wall-clock: 3 scenarios × 3 gateways × ~6 min = **~54 minutes**.

### 5b. Nexus cache feature (S-08) — feature benchmark, not head-to-head

```bash
# Cache must be enabled for this
BENCH_UNIQUE_PROMPTS= python cli.py run --scenario s08 --gateway nexus --mode cache-enabled --output $RESULTS
```

Wall-clock: ~7 min (3 sub-tests × 130 s each).

### 5c. Hooks A/B on the AMI (~12 min)

```bash
# Capture current hook state
curl -s "$NEXUS_CP_URL/api/admin/hooks" -H "Authorization: Bearer $TOKEN" \
  > $RESULTS/hooks_state_before.json

# Disable all enabled hooks
for ID in $(jq -r '.data[] | select(.enabled) | .id' $RESULTS/hooks_state_before.json); do
  curl -s -X PUT "$NEXUS_CP_URL/api/admin/hooks/$ID" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d '{"enabled":false}' >/dev/null
done

# Reset circuit and run S-01 with hooks OFF
curl -s -X POST "$NEXUS_CP_URL/api/admin/credentials/$CRED_ID/circuit-reset" \
  -H "Authorization: Bearer $TOKEN"

BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=20 BENCH_DURATION=300 BENCH_WARMUP=30 \
  python cli.py run --scenario s01 --gateway nexus --mode cache-disabled --output $RESULTS

# RESTORE hooks (CRITICAL — do this even if something fails above)
for ID in $(jq -r '.data[] | select(.enabled) | .id' $RESULTS/hooks_state_before.json); do
  curl -s -X PUT "$NEXUS_CP_URL/api/admin/hooks/$ID" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d '{"enabled":true}' >/dev/null
done

# Confirm hooks restored
curl -s "$NEXUS_CP_URL/api/admin/hooks" -H "Authorization: Bearer $TOKEN" \
  | jq '.data[] | select(.enabled) | .name'
# Expected: noop-baseline, pii-scanner, keyword-blocker, response-quality-signals
```

### 5d. Compliance / PII demo (~30 seconds, for James)

```bash
python demo/pii_compliance_demo.py
# Expected: 5/5 cases pass. Evidence JSON saved to demo/pii_demo_evidence.json
```

---

## Phase 6 — Reporting

```bash
# Generate the combined markdown
python cli.py report --results-dir $RESULTS --format markdown
# Writes a comparison table + per-scenario breakdown.

# Pull off-AMI for archiving
exit  # back to your laptop
scp -r ec2-user@$PUBLIC_DNS:~/benchmark/v2/$RESULTS ./aws-results-$(date +%Y%m%d)
```

The final deliverables to assemble locally:

| File | What it contains |
|---|---|
| `aws-results-YYYYMMDD/results_*.{json,csv}` | Raw per-run data |
| `aws-results-YYYYMMDD/comparison_*.md` | Auto-generated comparison table |
| `aws-results-YYYYMMDD/hooks_state_before.json` | Hooks A/B reproducibility evidence |
| `aws-results-YYYYMMDD/pii_demo_evidence.json` | Compliance demo evidence |
| `aws-results-YYYYMMDD/aws_instance_metadata.txt` | Instance type, region, kernel, AMI ID (capture manually) |

---

## Phase 7 — Compare against the old PDFs

Build a table comparing:

| Metric | Old PDF (Nexus v1.0) | New AWS (Nexus current) | Old PDF (LiteLLM) | New AWS (LiteLLM) | Old PDF (Bifrost) | New AWS (Bifrost) |
|---|---|---|---|---|---|---|
| S-01 TTFT p95 | (extract from PDF) | (from new run) | … | … | … | … |
| S-01 fail rate | … | … | … | … | … | … |
| 7-threshold pass rate | 7/7 | (new) | 2/7 | (new) | 3/7 | (new) |
| Cache hit rate (Nexus) | 44.16% (old, mixed cache) | (S-08 result) | n/a | n/a | n/a | n/a |
| LiteLLM streaming fail rate | 28.63% | (S-03 result) | — | — | — | — |

**Critically**: label which comparisons are **fair raw gateway** (S-01 / S-02 / S-03 cache-disabled) vs **Nexus feature demonstrations** (S-08 cache, S-09 PII). The old PDF mixed these — the new report must not.

---

## Common things that can go wrong (and the fix)

| Symptom | Cause | Fix |
|---|---|---|
| All Nexus requests return HTTP 429 with `insufficient_quota` | OpenAI account drained | Top up billing or use a different funded org key |
| All Nexus requests return HTTP 401 `vkauth: virtual key invalid` | VK in `.env.local` is from a different Nexus deployment | Mint a new VK against THIS AMI's Control Plane (Phase 2 step 4) |
| All Nexus requests return HTTP 4xx at >400 RPS, manual curl returns 200 | Credential circuit breaker open from a prior failure burst | `curl -X POST $NEXUS_CP_URL/api/admin/credentials/$CRED_ID/circuit-reset` — also runs automatically via `cli.py run` for Nexus |
| All Nexus requests return HTTP 403 `pii-scanner:pii-detected` | Request contains a digit pattern the pii-scanner phone regex matches | Use the harness as-is — `runner.py` uses a letter-heavy nonce. If using a custom prompt set, avoid 7+ consecutive digits |
| `cache_hit_rate_pct` is `null` even in S-08 | Old version of `runner.py` (pre-cache-header merge) | Confirm `engine/runner.py` has BOTH `X-Nexus-Cache` AND `x-cache-status` in the cache header read (lines 200-204 and 233-236) |
| `typer` errors with "Option '--scenario' does not take a value" | `click >= 8.4` installed | `pip install 'click==8.1.7'` |
| S-03 returns 100% `stream_timeouts` on a gateway | Gateway's streaming throughput at the configured concurrency is below test rate | Raise `request.timeout_seconds` to 120 in the gateway's YAML, OR lower `BENCH_VUS` |

---

## Definition of done

✅ Phase 4 preflight PASS
✅ Phase 5a results files for S-01 / S-02 / S-03 on all 3 gateways
✅ Phase 5b S-08 result file for Nexus
✅ Phase 5c hooks-OFF Nexus S-01 result + hooks restored
✅ Phase 5d PII demo: 5/5 cases pass
✅ Phase 6 comparison markdown generated
✅ Phase 7 old-vs-new table populated
✅ All Nexus hooks confirmed ON at end of run

Once all 8 boxes tick, send the `aws-results-YYYYMMDD/` directory + the generated markdown to James + Tiebin's team. That is the final deliverable.

---

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

**Per-hook cost isolation:** to attribute the compliance overhead to individual hooks,
use `./scripts/per_hook_sweep.sh` (enables one hook at a time, runs S-02, tags each
result `_hook_<name>`). Run it the same way — on the AMI via EC2 Instance Connect.
