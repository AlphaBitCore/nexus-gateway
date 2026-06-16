# Nexus Benchmark v2 — Handoff for Verification Runs

**Purpose:** runnable harness + reproduction notes for verifying the new Nexus AMI under load. Hand this off to whoever runs the verification (e.g. Tiebin's team) so they can reproduce the same comparison on their side.

**TL;DR — what to know before running:**
1. `pip install -r requirements.txt` then **also `pip install 'click==8.1.7'`** — `typer==0.12.3` is pinned, but `click` isn't, and `click >= 8.4` breaks option parsing.
2. To get a fair multi-gateway comparison, you **must** set `BENCH_UNIQUE_PROMPTS=1`. Without it, Nexus's in-flight streaming dedupe broker coalesces repeated prompts (the dataset cycles 55 prompts) — you'll measure ~60 ms TTFT instead of the real ~1200 ms.
3. The nonce format used to inject uniqueness must be **letter-heavy**. The PII-scanner hook (`fail-closed`, `block-hard`) false-positives on digit-only nonces — see "Gotcha #2" below. Our `runner.py` uses `secrets.token_urlsafe(8)` now; if you change it, keep digit runs short.
4. The harness sends `Authorization: Bearer <virtual key>` to Nexus. The VK must exist in the gateway's DB and have `allowedModels=[]` (or include `gpt-4o-mini`). The OpenAI provider credential inside Nexus must hold a funded key — the seed `sk-fake-…` placeholder needs to be replaced before any traffic.

## What this harness does

S-01 is the short-chat scenario:
- Sends OpenAI-shape `/v1/chat/completions` requests (stream=true, max_tokens=256, temperature=0).
- Drives concurrent VUs for a timed window; records TTFT (first content delta), E2E (stream `[DONE]`), HTTP class, broken-stream, JSON errors.
- Reports per-gateway: total / success / fail-rate / TTFT p50/p95/p99 / E2E p50/p95/p99 / RPS / cache-hit (telemetry — see Gotcha #4).
- Adapters at `gateway_adapters/{nexus,litellm,bifrost}.py` — same OpenAI-compatible body, gateway-specific auth.

## Setup

```bash
# 1. Python env (3.11+)
python3 -m venv .venv && source .venv/bin/activate
pip install -r requirements.txt
pip install 'click==8.1.7'   # see Gotcha #1

# 2. Env file
cp .env.local.example .env.local   # we ship a sanitized .env.local.example
# Fill in:
#   OPENAI_API_KEY=sk-...           (funded; the gateways under test forward to this)
#   NEXUS_BASE_URL=http://...3050   (or your AMI's endpoint)
#   NEXUS_API_KEY=nvk_...           (active virtual key in the gateway's DB)
#   NEXUS_ADMIN_URL=http://...3001  (CP)
#   NEXUS_ADMIN_API_KEY=nxk_...     (optional; only used by validate-config)
#   LITELLM_BASE_URL / LITELLM_API_KEY
#   BIFROST_BASE_URL / BIFROST_API_KEY

# 3. Start local LiteLLM + Bifrost (skip if hitting prod)
# LiteLLM:
docker run -d --name litellm -p 4000:4000 \
  -e OPENAI_API_KEY="$OPENAI_API_KEY" -e LITELLM_MASTER_KEY=sk-local-dev \
  -v "$PWD/gateways/litellm-config.yaml:/app/config.yaml" \
  ghcr.io/berriai/litellm:main-latest --config /app/config.yaml --port 4000
# Bifrost (config in gateways/bifrost-data/config.json references env.OPENAI_API_KEY):
docker run -d --name bifrost -p 8080:8080 \
  -e OPENAI_API_KEY="$OPENAI_API_KEY" -e APP_PORT=8080 -e APP_HOST=0.0.0.0 \
  -e LOG_LEVEL=info -e LOG_STYLE=json -e APP_DIR=/app/data \
  -v "$PWD/gateways/bifrost-data:/app/data" \
  maximhq/bifrost:latest
```

## Run the fair 3-way comparison

```bash
# Reset Nexus's openai-prod credential circuit breaker first (only matters
# if you've previously slammed it with an unfunded key — fresh AMI is clean).
# Replace <openai-prod-credential-id> with the ID from your Nexus DB.
curl -X POST "$NEXUS_ADMIN_URL/api/admin/credentials/<openai-prod-credential-id>/circuit-reset" \
  -H "Authorization: Bearer $NEXUS_ADMIN_API_KEY"

# Run all three sequentially (avoids contention on the same OpenAI key)
BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=3 BENCH_DURATION=60 BENCH_WARMUP=0 \
  python cli.py run --scenario s01 --gateway nexus --mode cache-disabled

BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=3 BENCH_DURATION=60 BENCH_WARMUP=0 \
  python cli.py run --scenario s01 --gateway litellm --mode cache-disabled

BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=3 BENCH_DURATION=60 BENCH_WARMUP=0 \
  python cli.py run --scenario s01 --gateway bifrost --mode cache-disabled
```

Results land as `results/results_<id>.json` and `.csv`. Compare TTFT p50 / p95 / RPS / failure-rate across the three.

To scale up like Tiebin's prod run (1 → 50 → 100 VUs), run three times per gateway varying `BENCH_VUS`:
```bash
for VU in 1 50 100; do
  BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=$VU BENCH_DURATION=60 BENCH_WARMUP=10 \
    python cli.py run --scenario s01 --gateway nexus --mode cache-disabled
done
```
(Warmup recommended at higher VU counts so the connection pool stabilizes before measurement.)

## Reference numbers from our local mac run (2026-06-15)

`BENCH_VUS=3, BENCH_DURATION=60`, gpt-4o-mini, all three gateways localhost:

| | Nexus | LiteLLM | Bifrost |
|---|--:|--:|--:|
| TTFT p50 (ms) | 1327 | 517 | 418 |
| TTFT p95 (ms) | 2275 | 1576 | 896 |
| RPS | 0.79 | 0.82 | 0.84 |
| HTTP fail % | 0 | 0 | 0 |

Use as a sanity baseline only — local Mac, not directly comparable to the AMI. Tiebin's prod numbers (1180–1230 ms p50 across 1/50/100 VUs, 100% success) sit cleanly in this same range, so the methodology aligns.

## Gotchas (read before you run)

### 1. `click` version pin
`requirements.txt` pins `typer==0.12.3` but not `click`. Latest `click 8.4` breaks typer's option parsing (`Option '--scenario' does not take a value`). Pin to `click==8.1.7`. We should add this to `requirements.txt`.

### 2. PII scanner false-positive on digit nonces ⚠️
When we first added unique-prompt injection, our nonce was `f"{int(time.time())}-{os.getpid()}-{req_id}"` → e.g. `1781536537-75191-3` (10 digits + dash + 5 digits + dash + 1+ digit). The Nexus **`pii-scanner` hook (`block-hard`, `fail-closed`)** has these patterns:

```
phone: \b(?:\+?1[-.\s]?)?(?:\(?\d{3}\)?[-.\s]?)?\d{3}[-.\s]?\d{4}\b
ssn:   \b\d{3}[-\s]?\d{2}[-\s]?\d{4}\b
cc:    \b(?:\d{4}[-\s]?){3}\d{4}\b
```

Our digit-heavy nonce matched the phone regex on every request → blanket HTTP 403 `X-Nexus-Hook: rejected:pii-scanner:pii-detected`, every request, before any upstream call. This is **not** a circuit-breaker problem and **not** a methodology problem — it's the nonce content tripping the compliance hook.

**Two fixes:**
- (Done in this harness) Use a letter-heavy nonce: `_RUN_NONCE = secrets.token_urlsafe(8)`; per-request `req_id:x` (hex). No long digit run can survive.
- (Recommendation for the gateway side) Either keep the PII scanner with these regexes (which is the conservative default) and require benchmarks to use letter nonces, or tighten the phone regex (the current one matches any `\d{3}[-.\s]?\d{4}` — 7 digits with optional separator — which is broad enough to hit lots of legitimate IDs/timestamps).

A/B reproduction:
```
prompt = "What is the capital of France?\n\n[benchmark nonce {NONCE}]"
NONCE = "1781536537-75191-3"  → 403, X-Nexus-Hook: rejected:pii-scanner:pii-detected
NONCE = "abcd1234-pqrs-y"     → 200, X-Nexus-Hook: passed:noop-baseline,pii-scanner,keyword-blocker
```

### 3. SSE chunks with `usage: null` crashed the runner
Nexus passes OpenAI's spec-compliant `"usage": null` in stream chunks. Our original `runner.py` called `.get("completion_tokens")` on that None and threw on every chunk. The exception bubbled up as a generic failure without a 4xx/5xx counter, producing the confusing "100% failures, 0 categorized" result. LiteLLM hid the bug by omitting the `usage` key entirely. Fixed in `engine/runner.py` (guard with `chunk.get("usage") or {}`).

### 4. Cache-hit header mismatch
`engine/runner.py` reads response header `x-cache-status` but Nexus emits `X-Nexus-Cache: HIT|MISS|...`. Cache-hit telemetry is therefore always `null`. Doesn't affect cache-disabled tests but matters for S-08 (cache feature). Easy fix — change the header name in `_execute_request`.

### 5. Streaming broker coalescing
Nexus has an in-flight streaming dedupe broker (`cache.broker: true` in `ai-gateway.dev.yaml`). With **cache-disabled** mode AND `stream=true`, callers with the same cache key still share one upstream session. Since our prompt dataset has only 55 unique entries, repeated requests across VUs collide. **Without `BENCH_UNIQUE_PROMPTS=1` your numbers will be invalid** — you'll see Nexus at ~60 ms p50 and 119 RPS, which is the broker fan-out, not real per-request latency.

### 6. OpenAI quota exhaustion
A single 5.5-minute s01 run can fire 30k+ requests at OpenAI. A free-tier or low-credit account drains fast and you'll start getting `insufficient_quota` 429s that look like rate-limiting but never clear. Verify the OpenAI key has billing before running:
```bash
curl -s -o /dev/null -w '%{http_code}\n' https://api.openai.com/v1/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}],"max_tokens":3}'
# 200 = funded, 429 + insufficient_quota = drained
```

When the OpenAI key 429s heavily, the Nexus credential circuit breaker will trip and *stay* tripped even after you swap to a funded key — reset via:
```bash
curl -X POST "$NEXUS_ADMIN_URL/api/admin/credentials/<id>/circuit-reset" \
  -H "Authorization: Bearer $NEXUS_ADMIN_API_KEY"
```

## What's in this package

```
benchmark/v2/
├── BENCHMARK_HANDOFF.md     (this file)
├── README.md                (original feature/usage docs)
├── LOCAL_SETUP.md           (original local-bring-up notes)
├── cli.py                   (entrypoint; BENCH_VUS/DURATION/WARMUP env overrides added)
├── requirements.txt         (NB: add click==8.1.7)
├── engine/
│   ├── runner.py            (SSE + concurrency. PII-safe nonce; usage:null fix)
│   ├── models.py            (config + .env.local auto-load)
│   ├── metrics.py           (per-request record + per-scenario aggregation)
│   └── ...
├── gateway_adapters/
│   ├── nexus.py             (Bearer VK + nexus.cache override body)
│   ├── litellm.py
│   └── bifrost.py
├── scenarios/
│   └── s01_short_chat.py    (the scenario run above; THRESHOLDS for pass/fail)
├── datasets/
│   └── short_chat_v2.json   (55 prompts)
├── config/
│   ├── nexus.yaml           (timeouts, default VUs/duration, env-resolved keys)
│   ├── litellm.yaml
│   └── bifrost.yaml
├── gateways/
│   ├── litellm-config.yaml  (LiteLLM model registry: gpt-4o-mini)
│   └── bifrost-data/
│       └── config.json      (Bifrost provider config)
└── results/
    └── fair-comparison-s01.md  (local Mac numbers, 2026-06-15)
```

Excluded from the package:
- `.env.local` (contains secrets — recreate from `.env.local.example`).
- `gateways/bifrost-data/config.db*` (54 MB of SQLite; recreated by Bifrost on startup).
- `__pycache__/` and `.venv/`.
- `results/results_*.{json,csv}` (our run artifacts).

## Open work / known harness limitations

1. Fix the cache header mismatch (Gotcha #4) so cache-hit telemetry works for S-08.
2. Add `click==8.1.7` (or `>=8.1,<8.4`) to `requirements.txt`.
3. The harness swallows non-4xx/5xx exceptions into a generic "failed" counter — make sure new failure modes surface a category rather than getting lost.
4. Scenarios S-02 through S-11 exist but weren't part of this hand-off; verify them after S-01 looks right.

## Contact

Local runner: Kash (kash@alphabitcore.com). Findings and the digit-nonce PII bug were diagnosed against the prod-equivalent Nexus build on 2026-06-15 — apply the lessons in Gotchas #2, #4, #5 above when verifying the new AMI.
