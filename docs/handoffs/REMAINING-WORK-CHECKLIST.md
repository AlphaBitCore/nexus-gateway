# Nexus Live Observatory — Remaining Work Checklist (hand-off prompt)

> **How to use this file:** this is a self-contained brief for whoever picks up
> the remaining backend/launch work (Claude Code, an executor, or a teammate).
> Read `docs/handoffs/program.md` and `context/10-BACKEND-MASTER-PLAN.md` first
> for the full design; this file is the *action list* of what is NOT yet done.
>
> **State as of 2026-07-07 (reconciled against `origin/dashboard-v1`).** All
> backend code is unified on `dashboard-v1` (site repo). Landed since the first
> draft of this file:
> - WP1–WP4 contract-v2 (`d0dcb8c`/`4efbe49`/`6732ea1`/`2da2747`) — merged via PR #1 (`9acc8da`).
> - Phone-home `run.AggregateIngested()` (`7a7054f`) + **`ec2.Run()` completed** (`2bd2460`) — A2 DONE.
> - 3 AWS ops docs (`c011c9d`) + **governance toggle doc** `NEXUS_GOVERNANCE_TOGGLE.md` — A5 doc DONE.
> - **Loadgen boot-runner** `obs-backend/aws/loadgen/` (`db6f35c`, Kanishk) — A1 DONE (caveats below).
> - `runMode` (parallel|sequential) wired profile→spec→pending-run→Run window (`59533a5`); default parallel to match the loadgen, pending Tieben G2.
>
> The build is now a working end-to-end loop. What remains is the **supervised
> first live run**, a few **best-effort metric-source gaps**, **provisioning**
> (bifrost DB + governance bake), and the **people-gated launch steps**.

## Goal (one line)

Get from "built backend + published report" to "a visitor clicks Run on the
public site and watches Nexus vs competitors benchmark live, with the result
folded into the Evidence Timeline" — without weakening any honesty/safety rule.

---

## BUCKET A — Critical path: first live run (blocks everything downstream)

### A1. Loadgen boot-runner (Kanishk) — ✅ DONE (`db6f35c`), 2 metric-source gaps open
Landed at `obs-backend/aws/loadgen/` — polls `/internal/pending-run`, resolves
each gateway's private IP by `nexus:arena-role` tag, drives load, posts to
`/internal/metrics`, handles all status codes (204/200/202/404/410/401/503),
unit-tested against the confirmed contract. Runs targets **concurrently**
(parallel mode) today. Still open (flagged in its README, not blockers for a
first run but must be closed before publishable numbers):
- [ ] **`--live-json` format** — the loadtest live-stats format is assumed;
      replace the parser once the real `nexus-loadtest --live-json` ships.
- [ ] **Per-gateway `memUsedPct` / `cpuPct` sourcing** — currently best-effort:
      `memUsedPct` via a Prometheus `process_resident_memory_bytes` scrape
      (works for Bifrost; **0, never fabricated**, for gateways without it);
      `cpuPct` has **no source yet**. The agentgateway-OOM memory chart depends
      on real memUsedPct across every gateway — decide the mechanism (host
      metrics agent per gateway box vs loadgen-side) and wire it. **Open question.**

### A2. ec2 driver `Run()` finalization — ✅ DONE (`2bd2460`, Kash)
`Run()` produces no metrics (loadgen does, via phone-home); it holds the run
open for the load window then returns so the orchestrator calls `Stop()`. A
`driver.PhoneHomeDriver` marker makes the orchestrator source the final Summary
from `run.AggregateIngested()`; zero samples ⇒ fail (no empty persist). Window
is `runMode`-aware (parallel = `durationSec`, sequential = `×gatewayCount`).
Covered by `ec2_run_test.go` + orchestrator phone-home tests; race-clean.

### A3. Supervised first FULL live run (Kanishk + Kash)
- [ ] End-to-end: Run button → Start fleet → health-gate all 5 gateways →
      loadgen pulls spec → streams metrics → SSE renders live → done → Stop →
      verify-stopped → snapshot written to `history/` and committed via deploy key.
- [ ] Watchdog must NOT fire (confirm layered timeouts hold: ~35m preset+overhead
      < 40m in-process < 45m watchdog).
- [ ] Confirm the snapshot validates against `schema/nexus-bench-v2.schema.json`
      and renders in the timeline with zero manual edits.

### A4. Bifrost fleet wiring (Kanishk)
- [ ] Stand up Bifrost's own PostgreSQL on its box so it can actually route
      (currently an unconfigured container).
- [ ] Confirm `arena-profile.json` health for bifrost (`/metrics` on 8080) still
      holds once it's routing; correct the profile if the readiness endpoint differs.

### A5. Nexus governance on/off toggle — ✅ DOC DONE (`fa15837`); BAKE remains (Kanishk)
- [x] Kash: mechanism verified + written up in `obs-backend/NEXUS_GOVERNANCE_TOGGLE.md`.
      It's the CP admin API (`GET /api/admin/hooks` → `PUT {enabled}` → verify via
      node runtime snapshot), exactly as `benchmark/v2/scripts/hooks_toggle.sh`
      drives it. Both nexus boxes = SAME image; toggle is per-deployment config.
      Hooks: `pii-scanner`, `keyword-blocker` (request), `response-quality-signals`,
      `response-content-safety`, `pii-outbound-scanner` (response). Governance-off
      must disable ALL response-stage hooks (the SSE hold-back). NOT the same as
      `NEXUS_AUDIT_DISABLED` (a separate diagnostic — keep unset).
- [ ] **Kanishk: bake it in provisioning** — hooks-on box at seed baseline (audit
      ENABLED), hooks-off box gets the `hooks_toggle.sh off` equivalent on first
      boot. Set once per box (not a per-run toggle, since both run simultaneously).

---

## BUCKET B — Branch & merge hygiene

- [ ] Rename `dashboard-v1` → `feat/b1-arena` before merge (it is the Arena
      backend, not a dashboard).
- [ ] Merge order: `feat/backend-contract-v2` (`2da2747`) and `feat/b1-arena`
      (`98707f5`) both touch `obs-backend/` — reconcile and merge to main; run
      `go test ./...` green post-merge.
- [ ] Confirm no untracked head commits exist beyond `2da2747`/`98707f5`
      (`git log --oneline -20` on each branch).

---

## BUCKET C — Deferred backend work (spec'd, not blocking first run)

- [ ] WP6 — Comparisons builder (`comparisons/<a>-vs-<b>.json`) OR client-side
      derivation (decide by payload size); each pair carries the governance note.
- [ ] WP6 — Populate governance fields (`traffic_events_captured`,
      `audit_loss_detected`, `memory_peak_mb`) once Tieben's V2/V3 artifacts
      carry them. Until then they stay `null` — never fabricated.
- [ ] Drop the real preset RPS into `arena-profile.json` (`saturationRps` +
      preset `rps`) — single-field swap once Tieben confirms the number.

---

## BUCKET D — People-gated launch steps (not code)

- [ ] **Tieben (G2):** final preset RPS (agentgateway audit-on saturation point);
      streaming vs non-streaming demo mode; parallel-vs-sequential run mode +
      mock-box sizing. The report v1.0 numbers now exist, so this is close.
- [ ] **James (G3):** methodology sign-off (gates public launch); decide whether
      agentgateway rows go public (flips the single `PUBLIC_GATEWAYS` allowlist);
      set the V1 publish label (recommend `externally_referenced`).
- [ ] **James (G5):** confirm `alphabitcore` Docker Hub org + push token → add
      `DOCKERHUB_USERNAME`/`DOCKERHUB_TOKEN` as Actions secrets so PR #81's
      publish workflow can run. (Container side — not required for the live run.)
- [ ] **Infra (G4):** create the PUBLIC site-only repo (private-repo-on-free-plan
      blocks Pages — see `context/07-SITE-PUBLISH.md`); add Route53 CNAME
      `benchmark.alphabitcore.com → alphabitcore.github.io`. On split,
      `obs-backend/` (incl. `aws/`) STAYS private — only the static site + data
      go public.

---

## BUCKET E — Public hardening (WP7, do at launch, after A3 works)

- [ ] Lock `OBS_ALLOW_ORIGIN` to the site origin (CORS).
- [ ] Soft launch: admin-token-only Run button for ~1 week, then open to public.
- [ ] Daily cap (6 public runs/day) + per-IP cooldown live and tested.
- [ ] Repoint the daily OBS-104 cron at the Arena (cheaper than full CFN deploys).
- [ ] CI runs `validate_methodology.py` before any deploy (publish gate).

---

## Hard rules (must survive all of the above)
Serve only what the rig/arena produced — never fabricated numbers. No public
agentgateway claims before James (single `PUBLIC_GATEWAYS` switch). Never mix
live gateway traffic with benchmark traffic in one data model. No secrets in any
artifact or API response. Nothing `publishable` without James sign-off. Layered
timeouts stay strictly ordered. Guaranteed teardown + concurrency=1 +
presets-only are non-negotiable for the public button.

## Definition of done (first milestone)
A non-admin visitor clicks Run on `benchmark.alphabitcore.com`, watches the 5
gateways benchmark live (RPS + p99 + climbing memory), the run completes, the
fleet auto-stops, and the result appears as a new Evidence Timeline snapshot —
all within cost/safety rails, with James's methodology sign-off recorded.
