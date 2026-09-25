# 02 · Backend — obs-api, the Arena, and the integration contract

The backend that turns the static site into a **live benchmark surface**. Lives in
`Kaushik985/nexus-benchmark-site` → `obs-backend/` (Go, stdlib-only). Split:
**Kash owns `obs-api`** (the brain); **Kanishk owns the Arena** (the AWS fleet).

---

## Module layout (`obs-backend/`)

```
cmd/obs-api            HTTP + SSE entrypoint (main.go)
internal/api           endpoints + guards (concurrency=1, presets-only, rate limit, CORS)
internal/run           run store, SSE broadcaster, orchestrator (guarantees teardown)
internal/driver        Driver seam — sim (works now) · compose (local arena) · ec2 (B1)
internal/results       writes runs/<id>/summary.json + appends history/<ts>.json (site schema)
internal/arena         arena-profile.json loader (gateways + presets + discovery)
arena-profile.json     config-driven gateway set + presets (PLACEHOLDER rps → Tieben)
arena/                 local docker-compose Arena (OBS_DRIVER=compose)
README.md              how to run + full API contract + phasing
INTEGRATION.md         the obs-api↔Arena contract (Kanishk's build-against doc)
```

## The service (obs-api) — API contract

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/runs` | Start a preset run → `{runId, stream}`. **409** when busy (points at the active run to spectate); **429** cap/cooldown; **403** admin preset without `X-Admin-Token`. |
| `GET` | `/api/runs/{id}` | State: `queued→starting→running→collecting→done/failed`. |
| `GET` | `/api/runs/{id}/stream` | **SSE** — replay-then-live `state` + `metric` events. |
| `GET` | `/api/runs` | Recent runs. |
| `GET` | `/api/presets` | Available presets (public vs admin). |
| `GET` | `/healthz` | Liveness. |

**Metric event shape** (the shared `driver.Metric`):
`{gateway, rps, latP50Ms, latP99Ms, ttftP95Ms, memUsedPct, cpuPct, okPct, ts}` — **memory is first-class** (the Agent-Gateway audit-on OOM story is the live memory chart).

## Run orchestrator (the safety core)
- State machine: `starting → running → collecting → done/failed`.
- **Guaranteed teardown** — `Stop` is always called (deferred), even on failure/timeout/cancel. This is the property behind a public Run button.
- **Layered timeouts** (must stay ordered): longest preset (soak 30m) + overhead ≈35m **<** in-process ceiling **40m** **<** Kanishk's independent watchdog **45m**. Never equal.
- **Concurrency = 1** — a 2nd request 409s and is pointed at the live run; the SSE broadcaster **replays** buffered events so a mid-run joiner sees the whole run then streams live (spectator mode = feature).

## Driver seam (same state machine, three backends)
- **`sim`** — dependency-free; synthesizes realistic ramps and models the audit-on **OOM** (memory climbs past ceiling → throughput collapses, ok% drops). Runs the whole pipeline on a laptop. Dev/demo only, selected explicitly, never the prod default.
- **`compose`** — local docker Arena; real images at laptop scale. Remaining wire: loadtest `--live-json` relay.
- **`ec2`** — **B1 (Kanishk)**: interface + documented `Start/Run/Stop` contract; discovery by tag; returns a clear error until implemented (no fake success).

## Results pipeline
On completion: write `runs/<id>/summary.json` + append `history/<ts>.json` **in the
exact schema the portal renders** (meta/gateways/tiers[].rows), so live runs feed
the same Evidence Timeline as the daily cron. Provenance grows: preset, image
digests, trigger (`user|cron|admin`), runId. Idempotent history append.

## Config — `arena-profile.json`
- `discovery.tagKey = nexus:arena-role` (fleet found by tag, not hardcoded IDs).
- Per-gateway `port` + `healthPath` (health-gate contract).
- Presets: `quick-verify` (public, ~90s) + `soak-30m` (admin/cron).
- `saturationRps` / preset `rps` = **PLACEHOLDER** until Tieben's number (the only field B1 flips).

## Env (obs-api)
`OBS_DRIVER` (sim|compose|ec2) · `OBS_ADDR` (:8090) · `OBS_PROFILE` · `OBS_HISTORY_DIR` · `OBS_ALLOW_ORIGIN` (lock to site in prod) · `OBS_DAILY_CAP` (6) · `OBS_ADMIN_TOKEN`.

---

## The Arena (Kanishk's AWS side) — matches his 7/6 plan
1. **Fleet** — one `c6i.4xlarge` per gateway entry + mock + loadgen. **Each entry = one box** → `nexus-hooks-on` + `nexus-hooks-off` = 2 boxes; current lineup = **7 boxes**.
2. **Pre-baked** — images pre-pulled, config staged; start = power on (~60-90s) + `docker compose up`.
3. **Stopped between runs** — not destroyed; pennies/day. Lifecycle: start → run → stop.
4. **Scoped IAM** — `ec2:DescribeInstances/StartInstances/StopInstances`, tag-conditioned. Sufficient **iff phone-home** (see below).
5. **Independent auto-stop watchdog** — force-stops any Arena box running >45m regardless of obs-api state. The backstop.
6. **CloudWatch billing alarm.**

## obs-api ↔ Arena contract (`INTEGRATION.md`) — the four pins
1. **obs-api runs on a small always-on control box** (t3.small), NOT a fleet box (those are stopped).
2. **Discovery by tag** `nexus:arena-role` (IAM + watchdog scope on the same tag).
3. **Trigger + metrics = loadgen phone-home (pull)** — the loadgen box on boot pulls the run spec from obs-api `GET /internal/pending-run` and streams `--live-json` to `POST /internal/metrics`. This keeps obs-api's IAM at exactly 3 actions (SSM push would add 2). **Plan of record: phone-home.**
4. **Layered timeouts** — 40m in-process < 45m watchdog.

## Scope split for `ec2.go` (so Kanishk doesn't build blind)
- ✅ **Build now:** `Start` (DescribeInstances-by-tag → StartInstances → health-gate) + `Stop` (StopInstances → verify) + IAM + watchdog + provisioning.
- ⛔ **Wait:** `Run`'s metric collection depends on obs-api's `/internal/pending-run` + `/internal/metrics` (Kash's next task, gated on phone-home confirm). Don't implement against a guessed contract.

## Open decisions (for the sync)
- **phone-home vs SSM** — recommend phone-home (keeps 3-action IAM).
- **parallel vs sequential benchmark + mock sizing** (tag Tieben) — parallel is snappy but one mock box serves ~5× load and diverges from published mutual-exclusion; sequential matches published but is ~5× longer. Decides mock-box sizing.

## Verification done (B0)
`go vet` + `build` + `test` green: orchestrator happy-path, **teardown-on-failure**, **concurrency=1**, late-subscriber replay, results-schema + OOM-survives-to-snapshot. Live `sim` run streamed to `done` and wrote a valid history snapshot; guards fired (429/409); frontend JS syntax-checked.

## Run it locally
```bash
cd obs-backend
OBS_DRIVER=sim OBS_HISTORY_DIR=../history go run ./cmd/obs-api    # :8090
# then open the site with the backend wired in:
#   index.html?obsapi=http://localhost:8090
```

## Commits (site repo)
`9cafb34` (B0 backend) · `7ca6b58` (integration contract) · `ec29ad8` (Kanishk confirmations + scope + methodology flag).

## Kash's next backend task
Build `/internal/pending-run` + `/internal/metrics` (token-auth) once phone-home is confirmed — the concrete target Kanishk's loadgen boot-runner calls.
