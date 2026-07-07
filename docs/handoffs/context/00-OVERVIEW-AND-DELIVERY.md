# 00 · Nexus Delivery — Overview & Master Index

> **Start here.** This is the hub for everything built across the Nexus dashboard /
> benchmark / containerization program. The other four files go deep on each area.
> Last updated: 2026-07-06 (evening).

## Navigate

| File | What's in it |
|---|---|
| **00-OVERVIEW-AND-DELIVERY.md** (this) | The whole program at a glance: components, status, every repo + commit, roles, blockers, timeline. |
| **01-PLANS-EVOLUTION.md** | How the plan evolved (last 3 iterations) and the **current main plan** (Live Observatory + backend). |
| **02-BACKEND.md** | `obs-api` service, drivers, run state machine, the AWS "Arena", the Kanishk integration contract, phasing. |
| **03-FRONTEND.md** | The public comparison site + live Run button, and the Control Plane dashboard (Increment 1 + deferred). |
| **04-CONTAINERS-AND-DEPLOY.md** | The 4 Docker images (vectorscan), docker-compose, Helm chart, publish pipeline, deploy docs. |

## What this program is

A single push with three intertwined deliverables, which converged into one product:

1. **Container packaging** — package the Nexus gateway (+ competitors) as reproducible Docker images so benchmarks and deployments run against identical setups.
2. **The Dashboard / Live Observatory** — a public, continuously-verified **engineering evidence center**: hit **Run**, watch Nexus vs competitor gateways benchmark live, with an append-only history. The site is the display layer; AWS runs the actual benchmark.
3. **The methodology** — Nexus's story is **"one of the fastest gateways *with full audit + compliance*"** — not raw-fastest. Proven by testing every gateway at Agent-Gateway's audit-on saturation point.

## The one-paragraph status

Container images (4) + compose + Helm are **built, verified, and in review as PRs #81/#82**. The Observatory site + evidence pipeline + the **live-run backend skeleton (B0)** are **built, tested, and pushed** — runnable on a laptop today via a simulation driver. What remains is gated, not unbuilt: the AWS "Arena" (Kanishk), Tieben's final benchmark numbers, James's methodology sign-off, and GitHub-plan/Docker-Hub logistics.

## Components & status

| Component | Status | Where |
|---|---|---|
| 4 Docker images (console, hub, ai-gateway, compliance-proxy) + db-migrate | ✅ built + verified, **PR #81** (not published) | fork → AlphaBitCore/nexus-gateway |
| docker-compose.full.yml (dev/demo) | ✅ built, full stack healthy | same PR |
| Helm chart `deploy/helm/nexus-gateway` (prod) | ✅ lint + render verified | same PR |
| Control Plane dashboard — Increment 1 | ✅ built + verified, **PR #82** | same repo |
| Benchmark comparison site (gauges, honest claims) | ✅ built, pushed | Kaushik985/nexus-benchmark-site |
| Observatory evidence pipeline (history + provenance) | ✅ built + tested | same |
| Portal features (freshness, timeline, trace) | ✅ built + verified | same |
| Scheduled runner workflow (OBS-104) | ✅ authored + validated (staged for rig PR) | same, `rig/` |
| **obs-api live-run backend (B0)** | ✅ built + tested + pushed | same, `obs-backend/` |
| obs-api↔Arena integration contract | ✅ pinned | `obs-backend/INTEGRATION.md` |
| AWS "Arena" (ec2 driver + fleet) — B1 | ⛔ Kanishk (needs AWS) | — |
| Final benchmark numbers | ⛔ Tieben (running) | AlphaBitCore/llm-gateway-benchmark |

## Repos

| Repo | Role | Access |
|---|---|---|
| `AlphaBitCore/nexus-gateway` | product monorepo (public) | Kaushik985 = **read** (fork for PRs) |
| `Kaushik985/nexus-gateway` | fork used for PRs #81/#82 | owner |
| `Kaushik985/nexus-benchmark-site` | Observatory site + obs-backend + docs | owner (**private**) |
| `AlphaBitCore/llm-gateway-benchmark` | the AWS benchmark rig (CFN + Ansible) | read |
| `AlphaBitCore/nexus-loadtest`, `…/nexus-mock-provider` | load generator + mock upstream | read |

## Every commit made this program

**`AlphaBitCore/nexus-gateway` (via fork `Kaushik985/nexus-gateway`):**
- `bde69d5` — feat(deploy): containerize the gateway — 4 images + compose + Helm → **PR #81**
- `2dfe77e` — feat(control-plane-ui): dashboard-home benchmarking-aware enhancements → **PR #82**

**`Kaushik985/nexus-benchmark-site` (main), in order:**
- `0b0a030` — benchmark comparison site (Nexus vs LiteLLM/Bifrost/…)
- `37aa2b5` — export-benchmark-json.py (rig results → site data.json)
- `fafbddd` — public-data-only export w/ pinned provenance; strip agentgateway
- `e6d9629` — HANDOFF.md (delegation guide + AWS runbook §3)
- `6cfbc4a` — Observatory V1 (evidence history, freshness, timeline, scheduled runner)
- `9cafb34` — obs-backend B0 (obs-api + drivers + Run button)
- `7ca6b58` — pin obs-api↔Arena integration contract (Kanishk verify)
- `f41699e` — fix(pages): stop failing auto-deploy; scope artifact; PUBLISH.md
- `ec29ad8` — docs(integration): confirm Kanishk's values + scope ec2.go + flag methodology

## Standalone artifacts (on `~/Desktop/Desktop2/`)
- `nexus-delivery-handoff.md` — full delegation guide (workstreams A/B/C + AWS runbook §3 + OBS §4/§5).
- `nexus-observatory-backend-plan.pdf` — the 4-page backend plan (for the 1pm sync).
- `nexus-open-prs.sh` — reproducible push+PR flow (MODE=fork now, MODE=upstream on write access).
- `~/.claude/plans/hey-kash-a-few-sequential-sonnet.md` — the live working plan (v6).

## Team & roles
- **Kash** — frontend + obs-api backend + orchestration + all the above.
- **Kanishk** — AWS "Arena" (fleet, IAM, watchdog, billing alarm, ec2 driver).
- **Tieben** — the benchmark runs + final saturation numbers/methodology.
- **James / Chen Jin** — direction + methodology sign-off.

## Blockers / decisions outstanding
1. **James** — methodology sign-off (gates public launch) + cost cadence call.
2. **Tieben** — final gateway set + Agent-Gateway audit-on saturation RPS (one config value).
3. **GitHub Pages** — private repo on free plan can't serve Pages → decide: split a public site-only repo (recommended) / upgrade plan / make repo public (leaks internal docs — no). See `PUBLISH.md` in the site repo.
4. **Docker Hub** — confirm `alphabitcore` org + push token → then PR #81's publish workflow can run.
5. **Durable GitHub access** — per-repo write on nexus-gateway + llm-gateway-benchmark (forks work meanwhile).
6. **Sync decisions** — phone-home vs SSM (recommend phone-home); parallel vs sequential benchmark + mock sizing (tag Tieben).

## Timeline anchors (CST)
- Tieben streaming re-test done ~04:45; nostream ~09:45; data+report tomorrow evening.
- Next team sync: 10:30 AM.
