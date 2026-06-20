# S-02 v2 Long-Context Run — AWS Operational Notes

Everything the operator needs for the v2 long-context push, plus the optional
mock-provider setup. Read this top-to-bottom before running. Code changes from
this round are already in the repo (see "Code changes applied" at the bottom).

---

## 0. Optional but recommended: the mock LLM provider

The dataset/latency comparison is cleaner against a mock upstream that echoes the
prompt with a fixed token count — no OpenAI cost, no rate limits, no upstream
latency variance. Two options:

- **Use the shared instance:** `https://mockprovider.taskforce10x.com/`
  (`POST /v1/chat/completions`, `POST /v1/embeddings`, `GET /v1/models`).
- **Self-host:** deploy the bundle (`nexus-mock-provider.zip` → binary + systemd
  unit + nginx vhost + `DEPLOY.md`).

**Wiring gotchas (both gateways silently fall back to REAL OpenAI if mis-wired):**
- **LiteLLM:** `api_base` MUST include the `/v1` suffix.
- **Bifrost:** address the model as **`mock-provider/<model>`**, NOT
  `openai/<model>` — otherwise it routes to its auto-detected OpenAI key.
- **Nexus:** point the `openai-prod` provider's `baseUrl` at the mock (see the
  bundle's `CONFIGURE-NEXUS.md`).

**Verify you're actually hitting the mock** — the response `id` must be
`chatcmpl-llm-mock` and the `content` must echo your prompt back verbatim.
Anything else means you're still calling the real provider.

> The harness already trusts self-signed certs (`verify=False` in
> `engine/runner.py`), so the mock's HTTPS endpoint works out of the box.

---

## 1. S-02 halves VUs — set BENCH_VUS to DOUBLE what you want

`s02_long_context.py` runs at `virtual_users // 2` (one 16k-token request is far
heavier upstream than short chat). This is now **logged, not silent** — the run
prints `configured=N → effective=M VU(s)`.

- Want **3** effective VUs → set `BENCH_VUS=6`.
- Want the exact `BENCH_VUS` with no halving → set `BENCH_S02_NO_HALVE=1`.

## 2. Hooks A/B — disable ALL response-stage hooks, restore exactly

Use the updated `scripts/hooks_toggle.sh`. It now:
- **`off`**: snapshots the currently-enabled hooks, then disables the
  request-compliance hooks **and every response-stage hook** (including
  `response-quality-signals`, which drives the SSE hold-back — the v1.5 bug that
  made the delta ≈ 0).
- **`on`**: restores **exactly** the snapshot (so baseline-OFF hooks like
  `response-content-safety` / `pii-outbound-scanner` are NOT accidentally enabled).
- Prints `response-stage hooks: none ✓` from the runtime snapshot when the OFF
  arm is clean. **Do not start the OFF run until you see that line.**

Sequence:
```bash
# hooks-ON arm first (baseline state), then:
./scripts/hooks_toggle.sh off     # wait for "response-stage hooks: none ✓"
# ... run S-02 hooks-OFF arm ...
./scripts/hooks_toggle.sh on      # restores the exact snapshot
```

## 3. Long context amplifies the hold-back (expected, not a bug)

The SSE hold-back buffers until ~400 chars of response. Long-context prompts
produce longer responses, so with response hooks ON the hold-back on S-02 will be
**larger** than the ~0.7–0.9 s seen on short chat. That's expected — it's why the
hooks A/B matters: it isolates that cost.

## 4. Long runs WILL time out over an inline SSH session — use screen

S-02 warmup is 60 s; total run ≈ 360 s. Run detached and poll the log:
```bash
screen -dm -S bench_s02 bash -lc '
  cd ~/nexus-gateway/benchmark/v2 &&
  BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=6 BENCH_DURATION=300 BENCH_WARMUP=60 \
    python cli.py run --scenario s02 --gateway nexus --mode cache-disabled \
    --output results/aws > /tmp/bench.log 2>&1'

# poll — use wc -l, NOT grep -c (grep -c exits non-zero on zero matches and
# double-prints, which breaks `[[ ... -ge 1 ]]` guards):
grep 'Results written' /tmp/bench.log | wc -l
```

## 5. .env.local — required fields (now in .env.local.example)

For `hooks_toggle.sh`: `NEXUS_ADMIN_EMAIL`, `NEXUS_ADMIN_PASSWORD`,
`NEXUS_OAUTH_REDIRECT_URI` (on AWS use the instance scheme/host, e.g.
`https://<nexus-ip>/auth/callback`). Optional: `NEXUS_GW_NODE_ID` to skip
auto-discovery — format `gw-ip-<private-ip-dashes>.ec2.internal-3050`, from
`GET /api/admin/nodes`. Optional `NEXUS_OPENAI_CREDENTIAL_ID` for circuit-reset.

## 6. Virtual key before anything runs

Create the VK in the admin UI: **no RPM limit, no quota cap, approved, long
expiry**. Set it as `NEXUS_API_KEY`. (A VK from a different deployment fails with
`vkauth: virtual key invalid` — VKs are per-database.)

## 7. BENCH_UNIQUE_PROMPTS=1 is still required

Even though `long_context_v2.json` has a `[REQUEST-uuid]` prefix per prompt, the
harness only generates a **per-request** unique nonce when this is set — otherwise
it cycles the 10 padded prompts in a repeating pattern that can hit the semantic
cache. Always set it for fair runs.

## 8. OAuth token expires in 1 hour

`hooks_toggle.sh` re-authenticates on every call, so the toggle is safe across a
>60-min A/B. But any **custom** admin API calls in your orchestration must fetch a
fresh token each time — don't reuse a token grabbed at startup to re-enable hooks
at the end of a long run.

## 9. verify=False is in engine/runner.py

The harness disables TLS verification (self-signed Nexus cert on AWS + self-signed
mock provider). Confirm before running:
```bash
grep -n 'verify' engine/runner.py    # expect: verify=False, in the AsyncClient
```

---

## Code changes applied this round

| File | Change |
|---|---|
| `engine/runner.py` | Added `verify=False` to the httpx client (self-signed Nexus/mock certs). |
| `scenarios/s02_long_context.py` | VU-halving is now logged + `BENCH_S02_NO_HALVE=1` opt-out (was silent). |
| `scripts/hooks_toggle.sh` | Added to repo; now disables ALL response-stage hooks on `off` and snapshot-restores exact baseline on `on` (fixes the v1.5 ≈0 delta). |
| `scripts/pad_long_context_dataset.py` + `datasets/long_context_v2.json` | Long-context prompts padded to ~16k tokens (real domain content). |
| `.env.local.example` | Added `NEXUS_ADMIN_EMAIL/PASSWORD`, `NEXUS_OAUTH_REDIRECT_URI`, `NEXUS_CP_URL`, `NEXUS_OAUTH_CLIENT_ID`, optional `NEXUS_GW_NODE_ID` / `NEXUS_OPENAI_CREDENTIAL_ID`. |

## Pre-run checklist (tick before the v2 S-02 push)

- [ ] Upstream chosen (mock or real); if mock, verified `id == chatcmpl-llm-mock`.
- [ ] LiteLLM `api_base` has `/v1`; Bifrost uses `mock-provider/<model>`.
- [ ] `.env.local` filled (VK + admin OAuth fields).
- [ ] `python cli.py preflight` → all PASS.
- [ ] `grep -n verify engine/runner.py` shows `verify=False`.
- [ ] Hooks A/B: `hooks_toggle.sh off` printed `response-stage hooks: none ✓`.
- [ ] `BENCH_UNIQUE_PROMPTS=1` set; `BENCH_VUS=6` (= 3 effective on S-02).
- [ ] Running under `screen`; polling with `... | wc -l`.
- [ ] After A/B: `hooks_toggle.sh on`; confirm baseline restored.
