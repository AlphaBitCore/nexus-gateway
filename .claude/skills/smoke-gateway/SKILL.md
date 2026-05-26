---
name: smoke-gateway
description: >
  Full-surface smoke test for the AI Gateway + Control Plane. Tests every
  model in the catalog (non-stream + SSE + 2-turn cache), auto-manages
  routing rules (all OFF for P3; per-rule isolation for P4), cross-checks
  traffic_event DB rows, diffs Prometheus counters, and auto-fixes failures
  (investigate → edit code → build → restart gateway → rerun). Includes
  P3E embeddings phase: per-model six-arm suite (Arm A non-stream basic,
  Arm B dimensions round-trip, Arm C batch input, Arm D traffic_event
  cross-check, Arm E Prometheus delta, Arm F cross-ingress consistency)
  plus reject-asymmetry negative tests (HTTP 400 on provider capability
  violations). Cache and dry-run arms are explicitly skipped for embeddings
  (no prompt-cache semantic). Trigger keywords: smoke gateway, full smoke,
  test all models, verify all providers, /smoke-gateway.
  Inputs: user provides a virtual key (vk) and optionally --routing to
  enable routing-ON tests. Output: Markdown report at
  /tmp/smoke-gateway-<UTC-timestamp>.md
user-invocable: true
---

# Smoke Gateway

Full smoke test for the AI Gateway. Wraps `tests/scripts/smoke-gateway.py`
and adds a self-healing loop: when tests fail, Claude investigates, fixes
code, rebuilds, restarts the service, and reruns until green (or reports
a root-cause it cannot auto-fix).

## When to use

- User types `/smoke-gateway <vk>` or asks to "smoke test all models",
  "verify gateway providers", "run full gateway test".
- After any change to `packages/ai-gateway/` or `packages/shared/`.
- Before declaring a gateway bug fixed.

## Inputs

| Arg | Required | Notes |
|---|---|---|
| `<vk>` | yes | Virtual key (`nvk_…`). |
| `--routing` | no | Also run P4 per-rule routing tests. |
| `--responses` | no | Also run P3R: full per-model /v1/responses suite for **every** catalog model (non-stream + SSE + multi-round cache, mirrors P3 chat-completions depth). Models in the OpenAI native-support prefix list (`gpt-5.x`, `gpt-4o`, `gpt-4.1`, `o1/o3/o4`) exercise same-shape passthrough; every other catalog model exercises the E56 cross-format canonical bridge (Responses → canonical chat-completions → target wire → response → Responses-shape on egress). The cross-format guard + structured outputs + reasoning matrix still live in `/test-openai-responses`. |
| `--messages` | no | Also run P3A: full per-model `/v1/messages` (Anthropic ingress) suite. Same depth as P3. Native (Anthropic targets) and cross-format (canonical bridge to OpenAI / Gemini / others) both tested. Cached-token field: `usage.cache_read_input_tokens`. |
| `--gemini` | no | Also run P3G: full per-model `/v1beta/models/{m}:generateContent` (Gemini ingress) suite. Same depth as P3. Native (Gemini/Vertex targets) and cross-format both tested. Cached-token field: `usageMetadata.cachedContentTokenCount`. |
| `--all-ingress` | no | **Umbrella**: turn on `--responses` + `--messages` + `--gemini` in one go (every public ingress exercised at P3 depth). Forces cache test ON per binding `feedback_cache_mandatory_all_ingress` — every ingress must run the cache arm; if you also pass `--no-cache` it is ignored with a warning. Use this flag for full-surface validation after any ingress, codec, or canonical-bridge change. |
| `--no-embeddings` | no | Skip P3E embeddings phase (useful for quick chat-only smokes). |
| `--all-upstream` | no | Force all fixture-mode phases to real upstream (forward-compat hook for E63/E64/E66). |
| `--no-stream` | no | Skip SSE phase. |
| `--no-cache` | no | Skip 2-turn cache phase (ignored when `--all-ingress` is also set — see binding). |
| `--cache-rounds N` | no | Read rounds per cache test (default 3); shows positive ROI. |
| `--fix-deprecated` | no | P8: auto-delete catalog models that fail non-stream in P3. |
| `--concurrent` | no | Run P3.5 concurrent test (N=5 parallel). |
| `--models m1,m2` | no | Restrict to specific models. |
| `--timeout N` | no | Per-request timeout in seconds (default 90). |

## Workflow

```
1. Preflight check
2. Run smoke script
3. Read report
4. On failure → fix loop (see below)
5. Final report path to user
```

### Step 1 — Preflight

```bash
# Verify gateway is up
curl -fsS http://localhost:3050/healthz

# Verify CP is up
curl -fsS http://localhost:3001/ready
```

If either is down, check `packages/<service>/logs/<service>.log` and offer
to restart (see Service Restart Authorization below).

### Step 2 — Run script (MANDATORY: live-monitorable)

Two binding rules from prod-20260520 incident (false-negative smoke run
where ai-gateway died mid-suite and 890 of the 455 failures were nginx
"connection refused" rather than real smoke fails — undetectable
because Claude was looking at a buffered tail-only log):

**Rule A — script output MUST be live-tail-able**. Always:
1. Invoke Python with `-u` (unbuffered stdin/stdout/stderr).
2. Redirect the FULL stream to a file with `>file 2>&1` —
   NEVER `| tee file | tail -N`, NEVER pipe to anything that
   buffers internally (head, tail without `-f`, less, etc.).
3. Launch via the Bash tool with `run_in_background: true` so the
   background task ID is recorded.

**Rule B — pair the script with a periodic prod-health watcher**.
Long-running smokes (especially `--all-ingress` against `--target prod`,
which can take 20-30 min) need a watcher that detects when the gateway
itself dies during the run. Spawn a separate background watcher script
that snapshots: (a) the smoke log tail, (b) running PASS/FAIL/WARN
counts, (c) `sudo systemctl is-active nexus-ai-gateway` on prod, (d)
recent ai-gateway ERROR/FATAL count. Refresh every 5 minutes into
`/tmp/smoke-watch.log` until the smoke pgrep exits.

#### Concrete launch sequence

Capture t0:
```bash
T0=$(date -u +%FT%TZ)
REPORT=/tmp/smoke-gateway-$(date -u +%Y%m%dT%H%M%SZ).md
echo "report → $REPORT"
echo "live log → /tmp/smoke-gateway-run.log"
```

Launch the smoke (run_in_background: true on the Bash tool call):
```bash
python3 -u tests/scripts/smoke-gateway.py \
  --vk <vk> \
  [--target prod|dev|local] \
  [--routing] [--responses] [--messages] [--gemini] [--all-ingress] \
  [--no-stream] [--no-cache] [--concurrent] \
  [--models <list>] [--timeout <N>] \
  --report "$REPORT" \
  > /tmp/smoke-gateway-run.log 2>&1
```

Spawn the watcher in a second (synchronous) Bash call. The watcher
auto-stops when the smoke python process exits:
```bash
cat > /tmp/smoke-watch.sh <<'WATCH'
#!/bin/bash
LOG=/tmp/smoke-watch.log
echo "=== watch start $(date -u +%FT%TZ) ===" >> $LOG
while pgrep -f 'python3 -u .*smoke-gateway.py' >/dev/null; do
  TS=$(date -u +%FT%TZ)
  {
    echo "----- $TS -----"
    echo "[smoke last 5 log lines]"
    tail -5 /tmp/smoke-gateway-run.log 2>/dev/null
    echo "[counts so far]"
    for kw in PASS FAIL WARN; do
      printf "  %s:%s\n" "$kw" "$(grep -c "$kw" /tmp/smoke-gateway-run.log 2>/dev/null)"
    done
    # Only check prod health when --target prod (skip for dev/local).
    if grep -q -- '--target prod' /tmp/smoke-gateway-run.log 2>/dev/null \
       || grep -q 'api.taskforce10x.com' /tmp/smoke-gateway-run.log 2>/dev/null; then
      echo "[prod ai-gw]"
      ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 \
        ec2-user@18.204.174.212 \
        "sudo systemctl is-active nexus-ai-gateway ; \
         sudo journalctl -u nexus-ai-gateway --since '5 min ago' --no-pager 2>&1 \
         | grep -cE '\"level\":\"(ERROR|FATAL)\"'" 2>&1 | tail -3
    fi
    echo ""
  } >> $LOG 2>&1
  sleep 300
done
echo "=== watch end $(date -u +%FT%TZ) ===" >> $LOG
WATCH
chmod +x /tmp/smoke-watch.sh
nohup /tmp/smoke-watch.sh >/dev/null 2>&1 &
echo "watcher PID=$!  log=/tmp/smoke-watch.log"
```

#### During the run

- Acknowledge that the smoke is running and the watcher is recording.
- DO NOT wait silently. Either:
  - On any user check-in, immediately read `/tmp/smoke-gateway-run.log`
    tail + `/tmp/smoke-watch.log` and surface deltas.
  - When the run_in_background notification fires, parse the full
    output and the .md report.
- If `/tmp/smoke-watch.log` shows `is-active inactive` for prod ai-gw
  while smoke is still running, STOP — report immediately to the user
  with the timestamp so they can decide whether to start the gateway
  or abort the run.

### Step 3 — Read and parse report

Read the generated `.md` report file (path printed as last line of stdout).
Identify:
- Models with `❌` in P3 (non-stream or SSE failures)
- DB cross-check misses (P6)
- Error counter growth (P7)
- Any cache / routing anomalies

### Step 4 — Fix loop (if failures found)

**You are pre-authorized to do the following without asking, scoped to local
dev services only:**

#### Investigate

1. Read `packages/ai-gateway/logs/ai-gateway.log` for ERROR/WARN lines
   matching the failed model's request window (use `grep -A5 -B2 <model>`).
2. Read the relevant Go handler under `packages/ai-gateway/internal/ingress/proxy/`
   and `packages/shared/` for the suspected failure path.
3. Form a hypothesis. State it in one line before editing.

#### Edit

Follow CLAUDE.md rules: real fix only, no TODOs, no stubs. English only.
Update SDD/OpenAPI only if the fix changes documented behavior.

#### Build

```bash
cd packages/ai-gateway && go build ./... 2>&1
# If build fails, fix compiler errors before restarting.

# Run affected unit tests:
go test -race -count=1 ./internal/... 2>&1
```

#### Restart gateway

```bash
# Find and kill the running ai-gateway
PID=$(lsof -nP -iTCP:3050 -sTCP:LISTEN | awk 'NR>1{print $2}')
kill $PID 2>/dev/null
sleep 3
kill -9 $PID 2>/dev/null || true

# Relaunch in background
cd /Users/nexus/workspaces/workspace-nexus/nexus-gateway-refactor/packages/ai-gateway
go run ./cmd/ai-gateway/ -config ai-gateway.dev.yaml \
  >>/Users/nexus/workspaces/workspace-nexus/nexus-gateway-refactor/packages/ai-gateway/logs/ai-gateway.log 2>&1 &

# Wait for healthy
for i in $(seq 1 20); do
  curl -fsS http://localhost:3050/healthz >/dev/null 2>&1 && echo "UP" && break
  sleep 2
done
```

Never restart: Postgres, Redis, NATS, Vite dev server, or Control Plane
(unless it is itself the failing service — CP restart follows same pattern
on port 3001).

#### Rerun

Re-run the smoke script for the affected model only first:

```bash
python tests/scripts/smoke-gateway.py \
  --vk <vk> --models <failed-model> --no-cache \
  --report /tmp/smoke-gateway-rerun.md 2>&1
```

If that passes, rerun the full suite to confirm no regression.

#### Stop fixing if

- The failure is a **provider-side upstream error** (5xx from the LLM
  provider, rate-limit, quota exhausted). Report the error to the user with
  the provider name and HTTP status — this is not a gateway bug.
- The fix requires **schema migrations** or **CP changes** outside
  `packages/ai-gateway/`. Surface the finding and ask the user.
- You have attempted 3 fix iterations and the test still fails. Report
  the remaining failure and log with a detailed root-cause hypothesis.

## Service Restart Authorization

Governed by CLAUDE.md "Service lifecycle (local dev / debugging)":

- **Identify before killing**: confirm binary is under `packages/ai-gateway/`
  via `lsof` or `ps`.
- **Never kill a debugger** (`dlv`, `__debug_bin*`) — tell the user instead.
- **Only restart**: ai-gateway (3050), control-plane (3001), compliance-proxy
  (3040), nexus-hub (3060). Never touch Postgres, Redis, NATS, Docker.
- **After restart**: verify `/healthz` returns 200 before retesting.

## Common failure patterns

| Symptom | Likely cause | Fix hint |
|---|---|---|
| non-stream 500 for specific provider | Provider adapter error or bad credential | Check logs for `upstream` ERROR, verify credential in DB |
| SSE 200 but `[DONE]` not seen | SSE accumulator / flusher bug | `packages/shared/transport/streaming/` or `cross_format.go` |
| `usage_extraction_status=parse_failed` | Token extraction regex broke | `packages/shared/transport/streaming/` usage extractor |
| DB row missing after 45s | Audit pipeline wedged | Check `nexus_ai_gateway_audit_*` Prometheus counters; check Hub log |
| All models 401 | VK revoked or expired | Verify VK in DB: `SELECT status FROM "VirtualKey" WHERE key='...'` |
| Routing rule fires during P3 | default-kmini rule still enabled | Script should have disabled it; check logs for `routing_rule_id` |

## Output format

Final reply must include:
1. Pass/fail summary (N passed, N warnings, N failed)
2. Per-model status table
3. Any fixes applied (file + line + what changed)
4. Full report path
5. CLAUDE.md commit reminder: "Ready to commit? Suggested message: …"
