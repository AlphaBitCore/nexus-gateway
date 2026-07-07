# 10 · Backend Master Plan — single authoritative backend spec (2026-07-06)

> **Authority order:** this file supersedes `08-BACKEND-INGESTION-PLAN.md` and
> `09-BACKEND-PLAN-RECONCILED.md` for anything they disagree on. It merges:
> the built obs-api B0 (`02-BACKEND.md`), the pinned Arena contract
> (`05-OBS-BACKEND-INTEGRATION.md`), the ingestion plan's net-new ideas (08),
> and the reconciliation decisions (09). Rule of the plan: **one contract, one
> exporter, one backend — evolve, never fork.**

---

## 0. What the backend is (final definition)

One system, two ingest paths, one data contract, one display layer:

```
STATIC PATH (exists, evolve):
  rig artifacts <run>/<gateway>/summary-*.json
    → scripts/export-benchmark-json.py        (the ONE normalizer)
    → validate_methodology (publish gate)
    → data.json + runs/<id>.json + runs.json + latest.json + history/<ts>.json
    → GitHub Pages (public site-only repo)

LIVE PATH (B0 built; B1 wiring):
  Run button → obs-api POST /api/runs
    → driver Start (ec2: StartInstances by tag nexus:arena-role)
    → health-gate → loadgen phone-home:
        GET  /internal/pending-run   (pull spec)
        POST /internal/metrics       (stream --live-json samples)
    → SSE relay to browser → on done: results.Persist writes the SAME
      runs/<id>.json + history/<ts>.json → committed to site repo (deploy key)
```

Honest by construction: both paths serve only what the rig/arena actually
produced; publish gates block anything methodologically unsound.

---

## 1. Contract v2 (the single data contract — freeze before code)

Decision from 09 §4.1 confirmed: **adopt the richer schema as the single
contract** and migrate exporter + `obs-api results.Persist` + `index.html` in
one pass. Refinement (new here): `gateways[]` is the **single source of
metrics**; the render-ready tier view is **derived, never hand-written**.

### 1a. Per-run object — `runs/<run_id>.json`

```jsonc
{
  "schema": "nexus-bench/v2",                    // version pin — REQUIRED
  "run_id": "v3-2026-07-08-001",
  "benchmark_version": "v3",
  "created_at": "2026-07-08T00:00:00Z",
  "operator": "tieben|obs-api",
  "trigger": "manual|cron|user|admin",           // live runs: from obs-api
  "preset": "quick-verify|soak-30m|null",        // live runs only
  "methodology_status": "draft|indicative|externally_referenced|publishable|invalid|superseded",
  "publish_status": "<same enum>",
  "agent_gateway_claims_public": false,
  "environment": { "cloud", "instance_type", "layout",
                   "run_mode": "sequential|parallel",   // pending Tieben (Q2)
                   "mock_provider", "loadgen", "region" },
  "provenance": { "sourceRepo", "sourceCommit", "remoteVerified",
                  "docker_image_digests": {}, "instance_ids": [],   // live runs
                  "rounds", "config_hash" },
  "gateways": [ {
      "name", "mode": "hooks-on|hooks-off|n/a", "image", "version",
      "rps", "lat_p50_ms", "lat_p99_ms", "ttft_p95_ms", "e2e_p95_ms",
      "itl_ms", "error_rate", "timeout_rate", "streaming_success_rate",
      "ok_pct", "memory_peak_mb",
      "hooks_enabled", "compliance_enabled", "audit_enabled",
      "cache_enabled", "pii_redaction_enabled",
      "traffic_events_captured": null,           // governance — null until
      "audit_loss_detected": null                // Tieben's V2 artifacts carry them
  } ],
  "scenarios": [ { "name", "prompt_type", "streaming", "concurrency",
                   "duration_s", "vu_count" } ],
  "tiers": [ /* DERIVED render view — today's shape:
                { id, label, streaming, rows: { <gw>: { rps, latP99Ms,
                  ttftP95Ms, okPct } } } — generated from gateways[]+scenarios[]
                by the exporter/results.Persist; index.html keeps reading it,
                which cuts the frontend migration to near-zero */ ],
  "methodology": { "same_machine", "same_instance_type", "configs_matched",
                   "caching_matched", "hooks_mode_labeled", "repeat_count",
                   "statistically_powered", "independent_verification", "notes" },
  "caveats": [ "..." ]
}
```

### 1b. Derived files (all emitted by the one normalizer)
- `runs.json` — index: per run `{run_id, benchmark_version, created_at,
  methodology_status, publish_status, gateways_tested[], scenarios[],
  best_rps_gateway, best_ttft_p95_gateway}` + `latest_run`,
  `latest_publishable_run`, `generated_at`.
- `latest.json` — most recent **publishable-or-externally_referenced** run.
- `history/<ts>.json` — unchanged role (append-only, idempotent), now carrying
  `schema: nexus-bench/v2`.
- `comparisons/` — optional prebuilt pairs (WP6; client-side derivation is the
  fallback — decide by payload size).

### 1c. Status derivation + publish gate (from 08, binding)
- `configs_matched|caching_matched|same_machine == false` → `indicative`.
- `repeat_count == null` → warn, treat as `indicative`.
- PUBLISH BLOCKERS (deploy fails): config/caching mismatch;
  `agent_gateway_claims_public == true` without James sign-off; any run marked
  `publishable` without recorded sign-off.
- **One switch:** the exporter `EXCLUDE` list and `agent_gateway_claims_public`
  collapse into a single `PUBLIC_GATEWAYS` allowlist in the exporter — a code
  diff to change, never a data edit.

### 1d. Live-metric event (unchanged — `driver.Metric`)
`{gateway, rps, latP50Ms, latP99Ms, ttftP95Ms, memUsedPct, cpuPct, okPct, ts}`
— memory stays first-class (the agentgateway audit-on OOM story is the live
memory chart). Field names here are the SSE/wire shape; §1a names are the
at-rest shape; the mapping table lives in `obs-backend/README.md`.

---

## 2. Work packages (specific, ordered, owned)

### WP1 — Contract freeze (Kash, ~0.5d) — DO FIRST
- Write `DATA_CONTRACT.md` in the site repo = §1 verbatim + JSON Schema file
  (`schema/nexus-bench-v2.schema.json`) + 2 fixture runs (one static v2-rig run,
  one synthetic live run).
- Acceptance: fixtures validate against the JSON Schema; team thumbs-up at sync.

### WP2 — Evolve the exporter (Kash, ~1d)
File: `scripts/export-benchmark-json.py` (keep its REAL rig reader —
`<run>/<gateway>/summary-*.json`; the 08-plan's `raw/<gateway>.json` layout is
wrong and must not be implemented).
- Emit §1a run objects + §1b derived files; keep `--embed`, `--history`,
  idempotence, provenance.
- Add `scripts/validate_methodology.py` — runs in CI before any deploy; exits
  non-zero on publish blockers.
- Add `scripts/migrate-history.py` — one-shot idempotent rewrite of existing
  `history/*.json` to v2 (so the Evidence Timeline never has two schemas).
- Acceptance: synthetic-tree test extended to v2 (schema-validate every output;
  re-run on unchanged input mints no snapshot); migrated history renders.

### WP3 — Migrate obs-api `results.Persist` + `index.html` (Kash, ~1d)
- Go structs mirror §1a; `results.Persist` emits v2 (derives `tiers` view);
  golden-file test asserts byte-stable v2 output for a fixed sim run.
- `index.html`: read v2 (`tiers` still present → render change is minimal);
  runs list/detail read `runs.json`/`runs/<id>.json`; provenance tooltips read
  the richer provenance block.
- Acceptance: local `OBS_DRIVER=sim` run → done → snapshot validates against
  the JSON Schema → timeline + gauges render it with zero manual edits.

### WP4 — Phone-home ingest endpoints (Kash) — **DONE** (commit `2da2747`, branch `feat/backend-contract-v2`)
The concrete target Kanishk's loadgen boot-runner calls (INTEGRATION.md §3).
G1 ratified by implementation (phone-home, no SSM).
- ✅ `GET /internal/pending-run` — token auth (`X-Obs-Internal-Token` =
  `OBS_INTERNAL_TOKEN` env, distinct from `OBS_ADMIN_TOKEN`); returns
  `{runId, rps, durationSec, targets[{gateway, port, healthPath}]}` for the
  active `starting|running` run, else **204**. Idempotent — loadgen re-polls.
  **Delta vs original spec:** `targets` carries `{port, healthPath}` (NOT
  `baseUrl`) — under phone-home the loadgen resolves gateway IPs itself in-VPC
  by tag, so obs-api serves only the LOGICAL spec (no instance IPs). This is
  what dissolved the WP4↔WP5 coupling and let WP4 land with zero touch to
  `ec2.go`.
- ✅ `POST /internal/metrics` — token auth; body `{runId, metrics[]driver.Metric}`;
  404 unknown runId, **410** finished (trailing batches after done dropped),
  202 accepted; each metric injected via new `run.IngestMetric` → existing SSE
  broadcaster.
- ✅ `internalAuth` wrapper: **503** when `OBS_INTERNAL_TOKEN` unset (never
  accidentally open), **401** wrong token. `/internal/*` is NOT CORS-wrapped —
  server-to-server, VPC-private, unreachable from the browser.
- **Dropped from spec:** `GET /internal/runs/{id}/status` — the orchestrator
  holds the run in-process (it launched it) and reads `run.State()` directly;
  no HTTP self-poll needed. The public `GET /api/runs/{id}` already exposes
  status for spectators.
- ✅ Tests (`internal/api/internal_test.go`): 503-disabled, 401-bad-token,
  pending-run 204-idle vs 200-active (asserts targets carry ports/paths, NO
  IPs), metrics 400-bad-body / 404-unknown / 410-finished / 202-and-reaches-a-
  subscriber. Full `go test ./...` green; readonly-mode build clean.
- ⏳ Remaining (Kanishk, now unblocked): the fake-loadgen curl-loop end-to-end
  against `OBS_DRIVER=compose` is the loadgen boot-runner's job (WP5 remainder).

### WP5 — ec2 driver + Arena wiring (Kanishk; Kash reviews)
**STATUS: Start/Stop/IAM/watchdog/provisioning DONE** — branch `dashboard-v1`
(commit `98707f5`, Kanishk, 2026-07-07), +1,419/−36, all in `obs-backend/`,
1-ahead/0-behind main, live-verified on AWS acct 511092106101. Matches
INTEGRATION.md exactly. Rename branch → `feat/b1-arena` before merge (it's the
Arena backend, not a dashboard).
- ✅ DONE: `Start` (DescribeInstances by `nexus:arena-role` → StartInstances →
  wait-running 3m → private IPs → health-gate per-gateway `port`+`healthPath`
  2m) + `Stop` (+verify) in `internal/driver/ec2.go` (+ `ec2_test.go`);
  `Gateway` gained `Port`/`HealthPath` (additive). 3-action tag-conditioned IAM
  (`aws/iam-policy.json`, NO SSM). Independent 45m watchdog Lambda
  (`aws/watchdog/`, EventBridge 5m, own IAM, `sweep_test.go`). 7-box
  `aws/provision.sh`. CORS preflight fix (`internal/api/api.go` — explicit
  OPTIONS routes; real Go-1.22-mux bug). Live-verified: fleet start+stop,
  watchdog invoked, bifrost+litellm health-gated, litellm 728/728 through mock.
- ⏳ REMAINING (blocked on WP4): loadgen boot-runner (pull spec → run
  `nexus-loadtest --live-json` → stream to `/internal/metrics`) + a supervised
  first *full* run (start→healthy→run→collect→stop→verify-stopped, snapshot in
  history, watchdog never fires).
- Fleet: 7 boxes (5 gateway entries incl. nexus on/off as 2 + mock + loadgen),
  c6i.4xlarge, pre-baked, stopped between runs. Timeouts stay LAYERED:
  ~35m preset+overhead < 40m in-process < 45m watchdog (verified consistent).

### WP6 — Comparisons + governance fields (Kash, later) — GATED on G2
- `comparisons/` builder (or client-side; decide by payload) with the
  governance note per pair.
- Populate `traffic_events_captured` / `audit_loss_detected` /
  `memory_peak_mb` when Tieben's V2/V3 artifacts carry them; saturation preset
  RPS → `arena-profile.json` (`saturationRps` + preset `rps`) — the only field
  B1 flips.

### WP7 — Hosting + public hardening (Kash + James/infra) — GATED on G3/G4
- Split **public site-only repo** (recommended per PUBLISH.md) → Pages deploy
  workflow re-enabled; CI = validate_methodology gate before deploy;
  Route53 CNAME `benchmark.alphabitcore.com → alphabitcore.github.io`.
  **On split: `obs-backend/` (incl. `aws/` from dashboard-v1) STAYS private —
  only the static site + data go public. The Arena backend must not leak.**
- S3/CloudFront: NOT built. S3 reappears only if the arena later pushes raw
  artifacts (V2+); do not build both hosting paths.
- obs-api B2: lock `OBS_ALLOW_ORIGIN` to the site origin; soft launch =
  admin-token-only Run button for ~1 week, then public; daily cap 6; daily
  OBS-104 cron retargets the Arena.

---

## 3. Gates (who unblocks what)

| Gate | What | Owner | Unblocks |
|---|---|---|---|
| G1 | phone-home vs SSM — **SETTLED BY IMPLEMENTATION**: `dashboard-v1` ships phone-home (3-action IAM, no SSM). Ratify at sync (formality). | sync (James/Kanishk) | WP4, WP5 run-pump |
| G2 | final numbers + gateway set + V2/V3 artifact format + parallel-vs-sequential | Tieben | WP6, preset RPS, mock sizing |
| G3 | methodology sign-off + agent-gateway-public + V1 label (`externally_referenced` recommended) | James | public launch, PUBLIC_GATEWAYS flip |
| G4 | public site-only repo + DNS CNAME | James/infra | WP7 hosting |
| G5 | Docker Hub org + token | James | PR #81 publish (container side, not this plan) |

Nothing in WP1–WP3 waits on any gate — **start immediately.** WP1–WP3 landed
(commits `d0dcb8c`/`4efbe49`/`6732ea1`) and **WP4 is now DONE** (`2da2747`) —
the `/internal/*` ingest surface exists, so the critical path shifts to
Kanishk's **loadgen boot-runner** (WP5 remainder): boot → pull `pending-run` →
`nexus-loadtest --live-json` → POST `metrics` → supervised first full live run.
That is the only thing left between the built Arena and a first live run.

---

## 4. Hard rules (survive any refactor — from 08/09, unchanged)
Serve only what the rig/arena produced. No public Agent-Gateway claims before
James (single PUBLIC_GATEWAYS switch). Never mix live gateway traffic with
benchmark traffic in one model. No `/internal/*` before G1. No secrets in
artifacts or responses. Nothing `publishable` without sign-off. Layered
timeouts stay strictly ordered. Guaranteed teardown + concurrency=1 +
presets-only are non-negotiable for the public button.

## 5. Explicitly rejected (do not resurrect)
- A second normalizer (`normalize.py`) parallel to the exporter.
- The 08-plan's `benchmark-results/<v>/raw/<gateway>.json` input layout (wrong
  vs the real rig output).
- S3+CloudFront V1 hosting; Lambda/API-Gateway ingest.
- Reshelving obs-api as "V2/future" — B0 is built; only `ec2` + `/internal/*`
  remain.
- User-chosen RPS/scenarios, multi-tenant parallel runs, K8s for the arena,
  a TSDB in V1 (ClickHouse alignment is V2 with James's storage work).
