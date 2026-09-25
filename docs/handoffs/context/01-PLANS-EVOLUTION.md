# 01 · Plan Evolution — the last 3 iterations, and the current main plan

The working plan (`~/.claude/plans/hey-kash-a-few-sequential-sonnet.md`) went
through several revisions as the team's intent sharpened. This file captures the
**last three** and, most importantly, the **current main plan** you're executing now.

---

## ▶ CURRENT MAIN PLAN (v6) — Nexus Live Observatory + live-run backend

**The product:** not a static benchmark page — a **public, continuously-verified
engineering-evidence center**. A visitor hits **Run**, watches Nexus vs competitor
gateways benchmark live, and every completed run appends to an immutable history.
The pitch it proves: *"Nexus is one of the fastest gateways **with full audit +
compliance**."*

**Architecture (three layers):**
1. **Display layer** — the static site (GitHub Pages): gauges, comparison table, evidence timeline, and the **Run button**. Built.
2. **Brain** — `obs-api` (Kash): decides when to run, drives the state machine, streams live results over SSE, folds completed runs into the history. **Built (B0).**
3. **Arena** — the AWS fleet `obs-api` controls (Kanishk): one `c6i.4xlarge` per gateway + mock + loadgen, pre-baked, **stopped between runs**, started per run, auto-stopped. **B1.**

**Methodology (binding, from the 7/6 call):** find Agent-Gateway's audit-on
**saturation RPS**, then run every gateway at that fixed point. Agent Gateway is
raw-fastest but OOMs with logging on — that's the story. **The numbers aren't
final until Tieben's run lands** (only one config value changes when it does).

**Phasing:**
- **B0 (done):** obs-api + state machine + drivers (sim works now) + results pipeline + frontend Run button. Runs on a laptop.
- **B1 (Kanishk + Tieben):** the `ec2` driver against the real Arena + the saturation RPS in `arena-profile.json`.
- **B2 (public):** lock CORS, enable the public button, daily scheduled verification reuses the Arena. Gated on James's sign-off.

**Cost safety (non-negotiable for a public button):** concurrency=1, daily cap +
per-IP cooldown, **layered timeouts** (preset ≤30m < in-process ceiling 40m <
independent watchdog 45m), billing alarm, presets-only.

See **02-BACKEND.md** for the full backend design and **03-FRONTEND.md** for the UI.

---

## Plan v5 — Nexus Live Observatory (the reframing, 7/5)

**What changed:** a PM issue reframed the benchmark *site* from a static display
into Phase 0 of an **Engineering Evidence Center** (NEXUS-OBS-001). Competitor
comparison is the *mechanism*; the deliverable is **trust infrastructure** — live,
timestamped, reproducible evidence that answers "why believe Nexus's claims?"
with "because it's re-verified on a schedule, publicly."

**Decisions locked:** daily cadence · full 6-gateway set · in-repo history
snapshots (git = audit trail) · GitHub Pages hosting.

**Built for it (all pushed):**
- **OBS-101** — exporter `--history`: immutable `history/<ISO>.json` + `index.json`, idempotent (unchanged evidence → no new snapshot).
- **OBS-102** — static-file API (`data.json` + `history/` served by Pages; no server in V1).
- **OBS-103** — portal: freshness badge (red when >48h stale), Evidence Timeline (per-metric trend charts from real snapshots), verification-status block, per-number provenance tooltips.
- **OBS-104** — `rig/observatory-run.yml`: daily scheduled verification (OIDC-only AWS auth, quota preflight, deploy→measure→snapshot→teardown, verify-zero-instances + `always()` failsafe cleanup, 150-min ceiling). Authored + validated; lands as a rig-repo PR.
- **OBS-105** — HANDOFF + README documentation.

The v6 backend (Run button) then extended this: the same `history/` + portal now
also receive **live user-triggered runs**, not just the daily cron.

---

## Plan v4 — Dashboard UI (7/4)

**What it was:** build dashboard enhancements **on top of** the existing Nexus
admin console (`packages/control-plane-ui`), not a rebuild. Exploration confirmed
the console already has 68 routes; the genuine gaps were **Benchmarks**,
**Workloads/Agent Teams**, **Deployments**, plus dashboard-home polish.

**Decisions locked:** build inside `control-plane-ui`; ship **Increment 1**
(dashboard-home enhancements) first; Benchmarks reads local SUMMARY.csv/JSON;
Workloads derives from VK-naming + traffic_event (no new entity).

**Built:** **Increment 1** (PR #82) — compliance-coverage warning banner,
End-to-End P95 card (gateway-overhead-vs-upstream split), traffic-source selector;
i18n EN/ES/ZH, design tokens, build green. Deferred (spec'd, not built):
Benchmarks / Workloads / Deployments / Compliance-health tabs.

See **03-FRONTEND.md**.

---

## Earlier context (pre-v4, for completeness)
The program began as the **container delivery** work (Chen Jin's Docker publish +
compose/Helm ask, 7/2–7/3) — 4 images with the vectorscan discipline, compose,
Helm, docs. That's fully built (PR #81) and documented in **04-CONTAINERS-AND-DEPLOY.md**.
It became the *deployment mechanism* for the Observatory: the Arena runs gateways
from these same images so live results match published numbers.

---

## Why the plan kept moving (the throughline)
Each revision was a **scope sharpening from the team**, not a reversal:
static site → evidence center (v5) → live user-triggered runs (v6). Everything
built at each stage carried forward — the exporter, history, portal, and images
are all reused by the live-run backend. Nothing was thrown away.
