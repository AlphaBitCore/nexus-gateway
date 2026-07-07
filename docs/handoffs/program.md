# Nexus Delivery Program — Master Handoff

> **Single entry point for any future session.** Read this file, then the
> `context/` files it indexes, before doing any program work. Last updated:
> 2026-07-06 (evening). Supersedes reliance on chat history / auto-compact.

## Goal

One program, three converged deliverables: (1) container packaging (4 Docker
images + compose + Helm — PR #81), (2) the Nexus Live Observatory — a public
engineering-evidence center with a live **Run** button (site + obs-api backend),
(3) the methodology — "one of the fastest gateways **with full audit +
compliance**", proven by running every gateway at Agent-Gateway's audit-on
saturation RPS.

## Context files (read in this order)

| File | Content |
|---|---|
| `context/00-OVERVIEW-AND-DELIVERY.md` | Program at a glance: components, status, repos, commits, roles, blockers, timeline |
| `context/01-PLANS-EVOLUTION.md` | Plan v4→v5→v6 evolution + the current main plan (Live Observatory) |
| `context/02-BACKEND.md` | obs-api design, drivers, orchestrator, the Arena, phasing B0/B1/B2 |
| `context/03-FRONTEND.md` | Public comparison site + Run button; Control Plane dashboard Increment 1 (PR #82) |
| `context/04-CONTAINERS-AND-DEPLOY.md` | 4 images (vectorscan discipline), compose, Helm, publish pipeline (PR #81) |
| `context/05-OBS-BACKEND-INTEGRATION.md` | **The pinned obs-api↔Arena contract** (Kanishk builds against this; snapshot of site-repo `obs-backend/INTEGRATION.md` @ `ec29ad8`) |
| `context/06-SITE-HANDOFF.md` | Site-repo HANDOFF.md (delegation guide + AWS runbook) |
| `context/07-SITE-PUBLISH.md` | GitHub Pages blocker + publish options |
| `context/08-BACKEND-INGESTION-PLAN.md` | Chat-proposed ingestion backend plan — partially superseded; keep the enum/validation/redaction ideas |
| `context/09-BACKEND-PLAN-RECONCILED.md` | Portable reconciliation: pasted plan vs built system, the 3 divergences, single-contract decision |
| `context/10-BACKEND-MASTER-PLAN.md` | **AUTHORITATIVE backend spec** — contract v2, WP1–WP7 work packages, gates G1–G5, hard rules. Supersedes 08/09 where they disagree |

Source-of-truth repos: `Kaushik985/nexus-benchmark-site` (site + obs-backend,
private), fork `Kaushik985/nexus-gateway` → PRs #81/#82 on
`AlphaBitCore/nexus-gateway`, `AlphaBitCore/llm-gateway-benchmark` (rig).
This local checkout (June 19) is STALE relative to the active repos — treat it
as reference; verify against the current repo before building.

## State (2026-07-06)

- Built + verified + in review: PR #81 (containers/compose/Helm), PR #82
  (dashboard Increment 1). Not published (Docker Hub secrets pending).
- Built + pushed: site, evidence pipeline (history/provenance), portal
  (freshness/timeline/trace), OBS-104 runner workflow, **obs-api B0** (sim
  driver end-to-end on a laptop), integration contract pinned.
- **NEW (Jul 7): Arena B1 largely landed.** Branch `dashboard-v1` (Kanishk,
  `98707f5`) implements ec2.go Start/Stop + 3-action IAM + independent watchdog
  Lambda + 7-box provisioning; live-verified on AWS; matches INTEGRATION.md;
  1-ahead/0-behind main (clean merge — rename → `feat/b1-arena` first). G1
  (phone-home) is now settled-by-implementation.
- **NEW (Jul 7): contract-v2 WP1–WP4 landed** on branch `feat/backend-contract-v2`
  (off `dashboard-v1`): WP1 schema+fixtures+DATA_CONTRACT (`d0dcb8c`), WP2
  exporter evolution + `validate_methodology.py`/`migrate-history.py` (`4efbe49`),
  WP3 `results.Persist`→contract-v2 + `index.html` normalizeData shim (`6732ea1`),
  WP4 `/internal/pending-run` + `/internal/metrics` + `run.IngestMetric`
  (`2da2747`). All obs-backend tests green. WP4 landed with ZERO touch to
  Kanishk's `ec2.go`/`aws/` (phone-home = loadgen resolves IPs in-VPC, obs-api
  serves only the logical spec). **Critical path now = Kanishk's loadgen
  boot-runner** (the only thing left before a first full live run).
- **NEW (Jul 7, later): WP1–WP4 MERGED into `dashboard-v1`** (site PR #1, merge
  `9acc8da`) — the v1 dashboard line now carries Arena B1 + the full contract-v2
  backend. On top of it (pushed to `origin/dashboard-v1`):
  - `7a7054f` — `run.AggregateIngested()`: folds ingested phone-home samples
    into the final `driver.Summary` (mean RPS, max p99/ttft, min okPct, max
    mem; OOM never inferred). The "summarize" half of the ec2 path; the Run()
    wiring that calls it stays Kanishk's.
  - `c011c9d` — 3 ops docs in `obs-backend/`: `OBS_API_AWS_DEPLOY.md`,
    `LOADGEN_PHONE_HOME_CONTRACT.md`, `ARENA_FIRST_RUN_CHECKLIST.md` (incl.
    failure-mode audit appendix). These make the internal AWS backend
    executable by Kanishk without this session's context.
  - `2bd2460` — **the obs-api side is now FULLY wired**: `ec2.Run()` no longer
    returns `errRunPending` — it holds the run open for the load window
    (phone-home; produces no metrics itself), a `driver.PhoneHomeDriver` marker
    makes the orchestrator source the Summary from `run.AggregateIngested()`,
    and zero ingested samples fail the run rather than persist empty. Full
    obs-backend suite race-clean. `dashboard-v1` = Arena B1 + WP1–WP4 +
    aggregation + completed Run + ops docs = one unified, working AWS backend.
    **Only Kanishk's loadgen boot-runner + the supervised first live run
    remain** — everything obs-api needs to serve them is built and tested.
- Gated, not unbuilt: saturation RPS (Tieben
  — streaming ~04:45 CST, nostream ~09:45 CST, data+report next evening), James
  methodology sign-off (gates B2/public), Pages hosting decision, Docker Hub
  org/token.

## Answers to the open-questions block (decided/recommended 2026-07-06)

1. **Phone-home vs SSM (metric collection)** — **Phone-home is plan of record**
   (INTEGRATION.md): loadgen boots, pulls the run spec from obs-api
   `GET /internal/pending-run` (token-auth), streams `--live-json` to
   `POST /internal/metrics`. Keeps obs-api IAM at exactly 3 tag-scoped EC2
   actions; SSM would add `ssm:SendCommand` + `ssm:GetCommandInvocation`.
   Confirm at the sync, then Kash builds the two `/internal/*` endpoints.
2. **Parallel vs sequential gateway runs** — Owner: Tieben. **Recommend
   sequential**: it matches the published mutual-exclusion methodology and keeps
   one mock box sufficient; parallel makes the single mock serve ~5× preset RPS
   (bottleneck risk) and diverges from published numbers. Cost of sequential is
   run length — mitigate by keeping the public `quick-verify` preset short
   per-gateway. Not a blocker for Start/Stop/IAM/watchdog.
3. **V2 artifact format** — Owner: Tieben. Constraint from our side: whatever
   lands must map into the existing exporter/site schema
   (`meta/gateways/tiers[].rows` + provenance). Baseline until then: delivery-doc
   SUMMARY.csv columns (`run_id,gateway,hooks,mode,rps,p95_ms,p99_ms,memory_mb,
   success_pct,date,nexus_commit,bifrost_commit`). Extend the existing
   `export-benchmark-json.py` — do NOT stand up a second normalizer.
4. **Agent Gateway claims public?** — Owner: James. Current enforcement: exporter
   `EXCLUDE` list strips agentgateway from public data (code diff to change).
   Per the 7/6 call agentgateway JOINS the public set under the saturation
   methodology — but the flag flips only after Tieben's verified numbers AND
   James sign-off. Until both: `agent_gateway_claims_public = false`.
5. **S3 bucket + CloudFront?** — **Superseded.** Hosting is locked to GitHub
   Pages; no S3/CloudFront/ACM needed for the site. The real blocker is the
   private-repo-Pages problem (see `07-SITE-PUBLISH.md`) — recommendation:
   split a public site-only repo. S3 remains only as the rig's run-artifact
   archive (kickoff value, Task 1).
6. **benchmark.alphabitcore.com DNS?** — CNAME file already staged in the site
   repo. Needs one Route53 CNAME → `alphabitcore.github.io` once the public
   site repo exists. Owner: James/infra; ask alongside the Docker Hub token.
7. **V1 publish label** — Owner: James. Recommend `externally_referenced` for
   the V1/V2 numbers already used in AlphaBitCore public materials;
   `indicative` for anything failing methodology checks; reserve `publishable`
   exclusively for James-signed V3 saturation runs.

## Next steps (queue order — detail in context/10-BACKEND-MASTER-PLAN.md §2)

0. ✅ DONE — Kash: WP1 contract-v2 freeze → WP2 exporter evolution → WP3
   results.Persist + index.html migration → WP4 `/internal/*` ingest
   (`d0dcb8c`/`4efbe49`/`6732ea1`/`2da2747`) — **MERGED into `dashboard-v1`
   via site PR #1** (`9acc8da`). Follow-ups also on `dashboard-v1`:
   aggregation seam (`7a7054f`) + 3 AWS ops docs (`c011c9d`).
1. Sync (10:30 CST): ratify phone-home (G1, formality — already implemented);
   tag Tieben on parallel-vs-sequential + mock sizing; Pages decision.
2. **Kanishk (critical path): loadgen boot-runner** — build against
   `obs-backend/LOADGEN_PHONE_HOME_CONTRACT.md`, then the supervised first full
   run per `obs-backend/ARENA_FIRST_RUN_CHECKLIST.md`. NOTE: `ec2.Run()` + the
   orchestrator wiring are now DONE (`2bd2460`) — obs-api holds the run open and
   aggregates the samples; the only thing left is the box that boots, pulls the
   spec, runs `nexus-loadtest --live-json`, and POSTs metrics.
3. Kanishk: merge `dashboard-v1` (rename → `feat/b1-arena`) into site main
   (Arena Start/Stop + IAM + watchdog already DONE + live-verified).
4. Tieben's numbers land → set `saturationRps` + preset `rps` in
   `arena-profile.json`; final gateway set; methodology page update.
5. James: methodology sign-off → B2 (lock CORS, enable public Run button,
   retarget daily OBS-104 cron at the Arena).
6. Logistics: Docker Hub org + token → PR #81 publish workflow; durable GitHub
   write access; Route53 CNAME.

## Binding rules to pre-load (from CLAUDE.md + program)

Honesty rules on the site are non-removable; layered timeouts stay ordered
(preset ≤30m < in-process 40m < watchdog 45m); concurrency=1 + presets-only +
daily cap for the public button; vectorscan images ship only with the redaction
smoke green (FAT_RUNTIME=OFF, hard-fail on missing link); ic/internal-repo-first
for unfinished work; nothing marked publishable without James sign-off.

## Memory anchors

[[project_parallel_worktree_sessions]] · [[feedback_cache_mandatory_all_ingress]]
· program anchor: Nexus Live Observatory (NEXUS-OBS-001).
