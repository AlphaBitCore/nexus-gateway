# Nexus Live Observatory — Pre-launch verification checklist

> Reconciled against the actual repo state on 2026-07-08 (site `dashboard-v1`,
> nexus-gateway `feature/container-deploy`). This is the go/no-go list for a
> **public** first run. Status legend:
> - ✅ **DONE** — implemented + evidence (commit / test / file).
> - 🟡 **PARTIAL** — works but has a named gap before *publishable*.
> - ⬜ **OPEN** — not started; fix + owner named.
> - ⛔ **BLOCKED** — waiting on a person/resource.
>
> Companion docs: `program.md`, `context/10-BACKEND-MASTER-PLAN.md`,
> `REMAINING-WORK-CHECKLIST.md`, `CLAUDE-CODE-TASKS.md`.

## 1. Critical blockers

- ⛔ **Nexus images pullable by the arena.** Both nexus boxes use the same
  `nexus-ai-gateway` image; until it's published, governance-on/off boxes can't
  start. **Path is now private ECR** (`deploy/docker/ecr-publish.sh` +
  `ecr-pull-policy.json`, `feature/container-deploy@c53f96e8`; Kanishk unblocked
  the pull side in `241cac7`). Gated on: **CC-Task1 (failing build test) green**
  + arena-account AWS creds. Docker Hub (public) stays James-gated, NOT the
  live-run blocker.
- ✅ **Bifrost DB provisioned.** Real Bifrost datastore added + verified live
  (`dashboard-v1@4adcc31`): Bifrost went 0% → 100% success under the same load,
  boots + routes + survives health-gate.
- 🟡 **First full live run.** A *competitor* end-to-end run is DONE and
  live-verified (`4adcc31`, 2026-07-08: sequential bifrost→litellm, metrics
  streamed, fleet auto-stopped, no watchdog fire). **Still needed:** the FULL
  5-gateway run **including the two Nexus boxes** (blocked on Nexus images) and
  a validated `history/` snapshot rendering in the timeline.

## 2. Gateway crash / failure classification

- ⬜ **Gateway-scoped error taxonomy** (loadgen/runner). Distinguish
  `failed_startup_health_gate` (infra) vs `target_unavailable_under_load`
  (benchmark terminal result — the expected agentgateway OOM) vs `loadgen_error`
  / `mock_upstream_error` / `orchestrator_error` (run failures). **Owner:
  Kanishk** (loadgen/runner). *Verify* whether `241cac7` (agentgateway setup)
  already classifies the crash-under-load case; if not, add the taxonomy.
- ⬜ **No blanket crash suppression.** Nexus crash → fail / severe-anomaly flag;
  Bifrost startup crash → fail health-gate; loadgen/upstream crash → fail the
  run. No broad catch-ignore around gateway process failures. **Owner: Kanishk.**

## 3. Metrics gaps (the main quality gate before *publishable*)

- 🟡 **`memUsedPct` honest-missing.** Today: Prometheus scrape works for Bifrost,
  reports `0` elsewhere — `0` reads as real. **Fix:** nullable +
  `metricStatus: "unavailable"`, and a real source (per-box host-metrics agent /
  loadgen-side / CloudWatch). **Owner: Kanishk** (loadgen) + Kash (contract shape).
- ⬜ **`cpuPct` has no source.** Add sampling + schema semantics (`cpuPct`,
  `cpuSource`, `sampleIntervalMs`, `missingReason`). **Owner: Kanishk + Kash.**
- ⬜ **agentgateway OOM memory chart needs real memory.** Capture the ramp before
  OOM; persist last samples before crash; chart must not drop final samples when
  the target disappears. **Blocks public launch** (headline visual).

## 4. Loadgen / runner

- ✅ **Sequential vs parallel — RESOLVED.** Tieben confirmed sequential
  (2026-07-08): one gateway at a time vs a single shared mock. `arena-profile.json`
  set `runMode: "sequential"`; obs-api `ec2.Run()` window = `durationSec ×
  gatewayCount` for sequential (`dashboard-v1@4e18432`); loadgen honors it
  (`runSequential`, `4adcc31`); both sides mirror the same parallel-as-default
  fallback. Live-confirmed in prod logs.
- 🟡 **`--live-json` parser provisional.** Real `nexus-loadtest --live-json`
  format not yet shipped; parser is behind a marked adapter + fixture test.
  Replace + add real-sample fixtures once it lands. **Owner: Kanishk.**
- ⬜ **No infinite retry on a dead target.** Terminal condition on repeated
  connection-refused/timeout (esp. agentgateway audit-on); persist failure reason
  + last successful RPS; move to next gateway. **Owner: Kanishk.**

## 5. Governance toggle / Nexus config

- ✅ **Mechanism documented** — `obs-backend/NEXUS_GOVERNANCE_TOGGLE.md`
  (`fa15837`): CP admin API (`GET /api/admin/hooks` → `PUT {enabled}` → verify
  via node runtime snapshot). Governance-off disables ALL response-stage hooks
  (incl. the SSE hold-back); `NEXUS_AUDIT_DISABLED` is NOT a substitute and stays
  unset.
- 🟡 **Bake script exists, not yet applied.** `aws/nexus-governance-bake.sh`
  (`4adcc31`) — apply per box on first boot (one hooks-on, one hooks-off). Marked
  UNTESTED until the Nexus image is publishable. **Blocked on Nexus images.**
- ⬜ **Full four-image Nexus stack in the arena.** Governance uses the CP admin
  API → needs console + hub + ai-gateway + compliance-proxy, not just the gateway.
  Confirm the arena Nexus-box compose uses the full set (match
  `docker-compose.full.yml`). **Owner: Kanishk + Kash.**

## 6. Observatory API / orchestration — mostly DONE

- ✅ **Zero-sample runs fail.** `run.AggregateIngested()` returns empty Rows on no
  samples; the orchestrator fails the run (never persists empty). Test:
  `TestOrchestrator_PhoneHome_NoSamples_FailsNoPersist` +
  `TestAggregateIngested_EmptyAndPostTerminal`.
- ✅ **Phone-home auth.** 503 when `OBS_INTERNAL_TOKEN` unset, 401 on wrong token,
  routes not CORS-wrapped (VPC-private, server-to-server). Tests:
  `TestInternal_DisabledWhenNoToken`, `TestInternal_AuthAndPendingRunLifecycle`.
- ✅ **Concurrency = 1 (HTTP level).** Duplicate `POST /api/runs` → 409 + spectate
  link to the live run (no second fleet); the run is fetchable to join. Test:
  `TestPostRun_ConcurrencyOne_SecondVisitorSpectates` (`8135bdc`).
- ✅ **Teardown after partial failure.** Orchestrator's `defer Stop()` is
  unconditional; watchdog is the backstop, not normal cleanup. Tests:
  `TestOrchestrator_RunFailure_StillTearsDown_NoPersist` + phone-home no-samples
  asserts stopped.

## 7. Data contract / report correctness

- ⬜ **Failure rows must not be null rows.** On crash, persist samples-up-to-crash
  + `terminalStatus` + `failureReason` + `failedAtRps` + `lastSampleAt` (+ audit/
  log loss if available). No empty or fabricated metrics. **Owner: Kash (contract/
  results) + Kanishk (loadgen emits the terminal state).**
- ⛔ **`PUBLIC_GATEWAYS` allowlist review.** Confirm the agentgateway-public
  decision with James before it can appear publicly; single switch guards it.
- 🟡 **Methodology validator coverage.** `validate_methodology.py` exists; extend
  it to catch config mismatch, governance mode, run mode, public-gateway rows,
  missing CPU/memory sources, zero-sample runs, and agentgateway-without-signoff.
  **Owner: Kash.**

## 8. Dashboard / frontend (deferred increments)

- ⬜ **Missing metrics rendered honestly** — no `0` for unavailable; dashed/omitted
  series + source tooltip; preserve crash marker.
- ⬜ **Live SSE failure-state UI** — render `starting|healthy|running|completed|
  crashed_under_load|failed_health_gate|run_failed`.
- ⬜ **Timeline snapshot rendering** — confirm a new `history/` snapshot appears
  post-run and that static report + live run share the `nexus-bench/v2` shape.

## 9. Branch / repo hygiene

- ⛔ **`dashboard-v1` → `feat/b1-arena` rename + reconcile with main.** Kanishk is
  actively pushing to `dashboard-v1` (multiple commits 2026-07-08) — **coordinate
  the window first**, then rename + merge + `go test ./...`.
- ⛔ **PR #81 image publish path.** Merge/tag once CC-Task1 is green; confirm the
  vectorscan smoke gate can't be bypassed.
- ℹ️ **PR #82 shipped Increment 1 only.** Benchmarks/Workloads/Deployments tabs
  deferred — not A3-blocking unless public launch depends on them.

## 10. External comparison / Bifrost wording

- ⬜ **Don't overstate "Bifrost lacks logging."** Bifrost documents logging/
  observability; our finding is *durability / record-retention failure under the
  tested outage/load conditions*. Fix the wording in report + site. **Owner: Kash.**
- ⬜ **Bifrost logging config — document the absence.** Preserve the email thread;
  state "requested recommended logging config; no response as of <date>"; do not
  guess undocumented settings. **Owner: Kash.**

---

## Go / no-go summary

**Green (verified):** orchestration safety (zero-sample fail, phone-home auth,
concurrency=1, guaranteed teardown), sequential run-mode end-to-end, Bifrost
routing, competitor live run.

**Before a full live run:** Nexus images published (ECR) → governance bake
applied → full 5-gateway supervised run incl. both Nexus boxes.

**Before *publishable* numbers:** real per-gateway memory + CPU (honest-missing),
the crash/terminal-status data-contract fields, methodology-validator coverage,
Bifrost wording fix.

**Before *public*:** James methodology sign-off + `PUBLIC_GATEWAYS` decision;
CORS locked to the site origin; soft-launch (admin-token) window.
