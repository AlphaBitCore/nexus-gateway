# Benchmark v2 — Dev Log

---

## 2026-06-16 — AWS v1.5 Re-Run (teammate update)

### What was fixed from the June 16 invalid run

All four methodology fixes from `AWS_V1_5_PUSH_PLAN.md` were applied:

| Fix | Applied |
|-----|---------|
| Upgraded instance to t3.xlarge (4 vCPU, 15 GB RAM) | ✓ |
| Sequential runs — one gateway at a time | ✓ |
| Switched to `benchmark/v2` (real SSE TTFT, not round-trip) | ✓ |
| `BENCH_UNIQUE_PROMPTS=1` (prevents Nexus broker coalescing) | ✓ |

### Valid results

| Gateway | Condition | TTFT p50 | TTFT p95 | RPS | Errors |
|---------|-----------|--:|--:|--:|--:|
| Bifrost | No hooks | 314.9 ms | 584.8 ms | 6.3 | 0% |
| Nexus | Hooks ON | 1270.9 ms | 2183.7 ms | 5.7 | 0% |
| LiteLLM | No hooks | 1500.5 ms | 4822.4 ms | 3.0 | 0.1% |
| Nexus | Hooks OFF | INVALID — see blocker below | — | — | — |

### Validations

- **Nexus 1270ms ↔ Kash local 1327ms** — within 4.2%. Strong confirmation the fix worked and the measurement is real. The ~57ms delta is plausible AWS vs Mac network variance.
- **Spread restored** — Nexus hooks-ON vs Bifrost = ~956ms. Compare to the invalid June 16 run which showed 120ms spread. We're back to the real picture.
- **Bifrost at 314.9ms p50** — faster than the local Mac run (418ms), consistent with lower AWS→OpenAI round-trip latency vs Mac→OpenAI.

### Anomalies to investigate

**LiteLLM p95 = 4822ms** — the p50 of 1500ms is already higher than expected (~480ms locally). Two possible causes:
1. LiteLLM container was cold or under-resourced during its sequential run window. Did the run order put LiteLLM first? If so, no warmup = cold connection pool penalty.
2. LiteLLM's Docker container on t3.xlarge may have a memory pressure issue — 15 GB is shared with the load generator. Check `docker stats` during the run.
3. The 0.1% error rate is suspicious alongside the p95 spike — suggests intermittent upstream timeouts, not a steady degradation. Check if OpenAI returned any 429s or 500s during the LiteLLM window.

**Action for next run:** put LiteLLM last in run order (after Nexus) so connection pools are warm, and capture `docker stats` output during the run.

---

## BLOCKER — Nexus hooks OFF run

### What was attempted
Direct DB edit to disable `pii-scanner` and `keyword-blocker` hook records.

### Why it didn't work
Nexus does not poll the database for config changes at runtime. The config flow is:

```
Admin API → Control Plane → Hub HTTP API
                                 ↓
                          Hub updates Thing shadow
                                 ↓
                          WebSocket push to Nexus AI Gateway
                                 ↓
                          thingclient.OnConfigChanged callback
                                 ↓
                          Gateway reloads hook config in memory
```

Editing the DB directly skips all of this. The Hub never sees the change, never pushes it, and the gateway keeps running from its last in-memory config — hooks stay ON regardless of what the DB says.

### Correct approach to disable hooks for the hooks-OFF run

Use the admin API, not the DB. The CP exposes hook toggle endpoints. The admin API call propagates through CP → Hub → gateway via the shadow mechanism above.

```bash
# Disable hooks (run from anywhere with network access to the CP)
NEXUS_ADMIN_API_KEY=<your-admin-key>
CP_URL=http://<nexus-instance-ip>:3001   # control plane port, not gateway port

curl -X PATCH "$CP_URL/api/admin/hooks/pii-scanner" \
  -H "Authorization: Bearer $NEXUS_ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"enabled": false}'

curl -X PATCH "$CP_URL/api/admin/hooks/keyword-blocker" \
  -H "Authorization: Bearer $NEXUS_ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"enabled": false}'

# Confirm the gateway received the update (wait ~2s for WebSocket push)
curl "$CP_URL/api/admin/hooks" \
  -H "Authorization: Bearer $NEXUS_ADMIN_API_KEY"
# Both should show "enabled": false

# Run the hooks-OFF benchmark
BENCH_UNIQUE_PROMPTS=1 BENCH_VUS=20 BENCH_DURATION=300 BENCH_WARMUP=30 \
  python cli.py run --scenario s01 --gateway nexus --mode cache-disabled

# Re-enable immediately after
curl -X PATCH "$CP_URL/api/admin/hooks/pii-scanner" \
  -H "Authorization: Bearer $NEXUS_ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"enabled": true}'

curl -X PATCH "$CP_URL/api/admin/hooks/keyword-blocker" \
  -H "Authorization: Bearer $NEXUS_ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"enabled": true}'
```

**Important:** the PATCH endpoint is on the Control Plane (port 3001), not the AI Gateway (port 3050). If you only have the gateway port exposed, you'll need to also open or SSH-tunnel to 3001.

### Expected hooks-OFF result
Based on local A/B: Nexus hooks-OFF p50 should land in **350–450ms range**, matching Bifrost. If it doesn't, the ~956ms overhead gap is not coming from the compliance pipeline and needs a deeper investigation (routing eval, traffic_event write, adapter overhead, etc.).

---

## Next run checklist (hooks-OFF completion)

- [ ] Confirm CP admin API (port 3001) is accessible from the t3.xlarge instance or via SSH tunnel
- [ ] Obtain `NEXUS_ADMIN_API_KEY` from CP UI → Settings → API Keys (if not already stored)
- [ ] Disable hooks via admin API, confirm gateway received update before starting run
- [ ] Fix LiteLLM run order — put it last (after Nexus), add 30s warmup
- [ ] Capture `docker stats` during LiteLLM run to check memory pressure
- [ ] Run hooks-OFF Nexus S-01, record results
- [ ] Re-enable hooks immediately after
- [ ] Compute compliance overhead delta: Nexus hooks-ON p50 − Nexus hooks-OFF p50 (expect ~820–950ms on AWS)
- [ ] Commit all result JSON files + this devlog update

---

## Running results summary (all sessions)

| Source | Gateway | Condition | TTFT p50 | Valid? |
|--------|---------|-----------|--:|--:|
| Local Mac (2026-06-15) | Nexus | Hooks ON | 1327 ms | ✓ |
| Local Mac (2026-06-15) | LiteLLM | No hooks | 517 ms | ✓ |
| Local Mac (2026-06-15) | Bifrost | No hooks | 419 ms | ✓ |
| Local Mac A/B (2026-06-15) | Nexus | Hooks OFF | 367 ms | ✓ |
| AWS t3.medium concurrent (2026-06-16) | All | — | 1185–1305 ms | ✗ CPU contention |
| AWS t3.xlarge sequential (2026-06-16) | Bifrost | No hooks | 315 ms | ✓ |
| AWS t3.xlarge sequential (2026-06-16) | Nexus | Hooks ON | 1271 ms | ✓ |
| AWS t3.xlarge sequential (2026-06-16) | LiteLLM | No hooks | 1501 ms | ⚠ p95 anomaly |
| AWS t3.xlarge sequential (2026-06-16) | Nexus | Hooks OFF | — | PENDING |
