# Nexus Delivery — Handoff & Delegation Guide

**Owner:** Kash · **Purpose:** hand off self-contained pieces of the Nexus containerization + benchmark + dashboard program so they can be executed in parallel. Each **Workstream** below is independent — pick one or more.

---

## 0. Context (read once)

**Nexus Gateway** (org: **AlphaBitCore**) is an enterprise AI traffic gateway — 4 services (Hub, AI Gateway, Compliance Proxy, Control Plane + its React Console) plus a Desktop Agent. Current program has four threads, three already built and committed, waiting to push:

| Thread | State | Where |
|---|---|---|
| Container packaging (4 Docker images + compose + Helm + docs) | **built, committed, verified** | branch `feature/container-deploy` |
| Dashboard "Increment 1" (console home enhancements) | **built, committed, verified** | branch `feature/dashboard-increment-1` |
| Benchmark comparison website | **built, committed** (local repo, push-ready) | new repo `nexus-benchmark-site` |
| Benchmark JSON exporter (rig → site data) | **built, tested** | in the site repo `scripts/` |

**The one gate on shipping all of it:** GitHub write access to the AlphaBitCore org for the pushing account. Until that lands, everything sits committed locally.

---

## 1. Resource inventory (links & locations)

**GitHub repos (org: AlphaBitCore):**
- `AlphaBitCore/nexus-gateway` — the product monorepo (services + Console UI + docs). Container + dashboard work targets this.
- `AlphaBitCore/llm-gateway-benchmark` — the AWS benchmark rig (CloudFormation + Ansible + `deploy.sh`/`down.sh`); produces the numbers. Also holds `scripts/bench/compare-gateways.py`.
- `AlphaBitCore/nexus-loadtest`, `AlphaBitCore/nexus-mock-provider` — the rig's load generator + mock upstream.
- `AlphaBitCore/nexus-benchmark-site` — **to be created**; the public comparison site (GitHub Pages → `benchmark.alphabitcore.com`).

**Reference specs (ask Kash for copies):**
- Delivery program tasks doc (`nexus-delivery-program-tasks.md`) — the rig + website + Docker task spec.
- Dashboard UI Product+UI Plan — the full dashboard design (operations + benchmarks + workloads surface). Increment specs are reproduced in Workstream C below.

**Ground rules (from the repo's CLAUDE.md — non-negotiable):**
- Public naming is **AlphaBitCore** everywhere; no internal-only org or repo names may appear in anything public.
- Console UI: every user-visible string via `t()` (EN/ES/ZH parity), **design tokens only** (no hex/raw numbers in CSS/inline styles), `useApi` queryKeys domain-prefixed, and an **IAM impact review** for any new admin route/nav item.
- Benchmark claims must be **honest** — no metric the data doesn't support; show hooks-on and hooks-off; keep the governance caveat.

---

## 2. Recommended split

- **Workstream A — Infra/DevOps person:** Docker Hub publish + benchmark rig re-run (full AWS runbook in §3) + wire the exporter into the rig repo. Mostly ops, scriptable, no deep app knowledge needed. **Needs AWS account access + EC2 vCPU quota headroom.**
- **Workstream C — Frontend (React/TS) person:** the deferred Dashboard increments (Benchmarks / Workloads / Deployments tabs). The biggest remaining build; fully specced below.
- **Workstream B — Whoever owns DNS/GitHub org:** create the site repo, enable Pages, add the DNS record, get the methodology sign-off. Small but gating.

Kash keeps: pushing the three ready branches once access lands (Workstream 0), and final review/merge of everyone's PRs.

---

## Workstream 0 — Push the ready work (Kash, on access grant)

Already committed locally; nothing to build. On GitHub write access:
1. `feature/container-deploy` → push → PR into `AlphaBitCore/nexus-gateway`.
2. `feature/dashboard-increment-1` → push → PR.
3. Create `AlphaBitCore/nexus-benchmark-site`, push the site repo → GitHub Pages auto-deploys a preview.
Everything below assumes these are pushed so others can branch off them.

---

## Workstream A — Docker Hub publish + benchmark rig refresh (Infra/DevOps)

**A1. Docker Hub publishing.** The images are built and a publish workflow exists (`.github/workflows/docker-publish.yml`, tag-triggered). To go live:
1. Confirm/create the **`alphabitcore`** Docker Hub org (check with James — org existence is unconfirmed).
2. Create a **Read & Write** access token under that org.
3. Add repo Actions secrets on `AlphaBitCore/nexus-gateway`: `DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN` (needs repo admin).
4. Push a `v*` tag (or run the workflow manually). It builds 4 images (`nexus-console`, `nexus-hub`, `nexus-ai-gateway`, `nexus-compliance-proxy`) + a `nexus-db-migrate` helper, runs the vectorscan self-test + a compose smoke, then publishes.
- **Acceptance:** all images `docker pull` from Docker Hub; the workflow's redaction smoke passes.
- **Gotcha:** the ai-gateway + compliance-proxy images compile Vectorscan (libhs) — the workflow hard-fails if the scan engine doesn't link. Don't "fix" that by removing the gate; it's protecting against a silent PII-passthrough failure mode.

**A2. Benchmark rig re-run + data refresh.** See the full **AWS Runbook** (section 3) below — it's the one AWS-heavy task in this program.

**A3. Land the exporter in the rig repo.** `scripts/export-benchmark-json.py` currently lives in the site repo as source-of-record. Open a small PR adding it to `llm-gateway-benchmark/scripts/bench/` so the rig produces `benchmark-latest.json` alongside its Markdown `REPORT.md`. It's self-contained and reuses `compare-gateways.py`'s summary-reading logic verbatim; a synthetic-tree test is documented in its header.

---

## Workstream B — Site launch plumbing (DNS / GitHub org owner)

Depends on Workstream 0.3 (repo created + Pages deploying).
1. **Enable Pages:** repo Settings → Pages → Source = GitHub Actions (the `pages.yml` workflow is already in the repo).
2. **Custom domain:** add a Route53 record `benchmark.alphabitcore.com` → `alphabitcore.github.io` (CNAME), and confirm the repo's `CNAME` file matches. Let Pages provision the TLS cert.
3. **Methodology sign-off (James):** the site is marked internal-draft. Before flipping it public, James reviews the "Methodology & honest disclosure" section — the data is indicative (n=2), rig operated by us, feature parity not equal. Internal preview link is fine to share now.
- **Acceptance:** `https://benchmark.alphabitcore.com` serves the site over valid HTTPS; the draft banner is removed only after sign-off.
- **Do not:** publish externally before sign-off, or add gateways to the exporter's `EXCLUDE` list back in without verification (e.g. `agentgateway` is excluded pending sign-off — that's deliberate).

---

## Workstream C — Dashboard deferred increments (Frontend, React/TS)

Builds on `feature/dashboard-increment-1` (Console home already enhanced). All work is in `AlphaBitCore/nexus-gateway` → `packages/control-plane-ui`. **Ship each increment as its own PR.**

**What already exists (don't rebuild):** the Console has 68 routes — Dashboard home, Traffic, Analytics, Quota, Cache ROI, all AI-Gateway pages, all Compliance pages, Status/Health, Infrastructure. The reusable kit: `components/ui/{Card,DataTable,Grid,Stack,Sparkline,PageHeader,ListFilterToolbar,Tabs}`, `components/charts/{TimeSeriesChart,LatencyWaterfall}`, `useApi` hook, i18n namespaces (`pages`/`nav`), design tokens via `@nexus-gateway/ui-shared`. New-route recipe: add to `src/routes/shellRouteConfig.tsx` + lazy import + page component + i18n keys (all 3 locales) + `allowedActions` + run the IAM impact review.

**C1 — Benchmarks tab** (highest value). New routes `/benchmarks`, `/benchmarks/:runId`, `/benchmarks/compare`.
- Run list: run id, date, git commit, environment, gateways tested, scenario, hooks on/off, and a **publishable** flag.
- Run detail: summary cards + per-gateway charts (RPS, TTFT p95, E2E p99, error rate) + a scenario matrix + a **methodology box** that flags "not publishable" when parity/config mismatches exist.
- Compare view: Nexus hooks-on/off vs each competitor.
- **Data source:** local `benchmark-latest.json` (the exact schema the comparison site uses — see the site repo `data.json`), loaded via a small adapter. No backend needed for v1.

**C2 — Workloads / Agent Teams tab.** New route `/workloads`. A migration-status table (workload, owner, status, traffic source, virtual key, last-seen). **Critical:** the UI must clearly disambiguate three things people keep confusing — *Agent Teams* (orchestrated multi-agent workloads), *Agent Gateway* (a competitor being benchmarked), and *Nexus AI Gateway* (our product). Derive workloads from virtual-key naming conventions + `traffic_event` source metadata (read-only; no new DB entity). Migration status is a manual/config field for now.

**C3 — Deployments / Operations tab.** New route `/deployments`. Health of the 4 `alphabitcore/nexus-*` images + Postgres/Valkey/NATS + config-sync status + the Vectorscan self-test / PII-smoke status. Ties directly to Workstream A's container work.

**C4 — Compliance health cards** (smallest). Add hooks-enabled / redactions / vectorscan-self-test / PII-smoke cards to the existing Compliance overview page. PII stays a hook implementation (don't add a separate page).

- **Acceptance (each):** `npm run build` + `vitest` green; `npm run check:i18n` parity; `npm run check:design-tokens` clean; IAM review recorded in the PR; new nav item visible with correct permission gating.
- **Gotcha:** don't ship dead controls or fake data — if an analytics dimension doesn't exist yet, render an honest empty state (this is exactly what Increment 1 did with the env selector). Follow the `feature/dashboard-increment-1` diff as the pattern.

---

## 3. AWS Runbook (Workstream A2 — the benchmark rig)

> **What actually touches AWS.** Only the **benchmark rig** runs on AWS (throwaway EC2 to generate the numbers). The comparison **site is GitHub Pages — not AWS**; the *only* AWS resource the site needs is a **Route53 DNS record** for the custom domain (Workstream B). The Docker images publish to **Docker Hub, not ECR**. So "AWS work" here = run the rig, collect results, tear it down.

### 3.1 Prerequisites
- An **AWS account + named CLI profile** with permissions for: EC2 (run/terminate instances, security groups, key pairs, volumes), CloudFormation (create/update/delete stacks), IAM (create the SSM instance roles the stack defines → needs `CAPABILITY_IAM`), and SSM (Session Manager access to the boxes). Confirm with `aws sts get-caller-identity --profile <profile>`.
- Local tools: `awscli v2`, `ansible` (core ≥2.15), `jq`, `git`, `python3`. macOS: `brew install awscli ansible jq`.
- Default region **`us-east-1`** (the rig menu offers others; keep us-east-1 unless told otherwise — it's also where an ACM cert would live if we ever moved off Pages).
- An EC2 **key pair** (or let `deploy.sh` create one; it writes the `.pem` locally).

### 3.2 What the rig deploys (so you know what you're paying for)
`cloudformation/perf-matrix-stack.yaml` launches into an **existing VPC/subnet** (or `network-stack.yaml` makes a throwaway one):
- Up to **10 EC2 instances**, each toggled by a `Deploy<X>` parameter: `Mock`, `Nexus`, `Bifrost`, `Litellm`, `Kong`, `Portkey`, `Tensorzero`, `Agentgateway`, `Loadtest`, `Control`. Gateway boxes are **`c6i.4xlarge` (16 vCPU each)** on gp3 root volumes.
- One security group (opens 22/80/443 + gateway ports **to the CIDRs you pass** — do not widen to `0.0.0.0/0`), an intra-SG all-traffic rule, and **SSM-only IAM roles** (no inbound SSH needed if you use Session Manager).
- **No S3 / CloudFront / Route53 / ACM** — the rig has zero hosting infra by design.

### 3.3 vCPU quota — check BEFORE you deploy
Running the full matrix is **~8 gateway boxes × 16 vCPU + mock + loadgen + control ≈ 150+ vCPUs** of On-Demand Standard instances. Most fresh accounts cap well below that.
- Check: `aws service-quotas get-service-quota --service-code ec2 --quota-code L-1216C47A --profile <profile> --region us-east-1` (L-1216C47A = "Running On-Demand Standard instances", measured in **vCPUs**).
- `deploy.sh` runs a preflight and will stop if you're short. If so, request an increase in **Service Quotas → EC2 → Running On-Demand Standard (A, C, D, H, I, M, R, T, Z) instances** and wait for approval, **or** deploy a subset (e.g. just Nexus + Bifrost + Mock + Loadtest) via the `Deploy<X>` toggles for a partial run.
- This is the program's known escalation trigger (`VcpuLimitExceeded`) — flag it to Kash early, quota bumps can take a day.

### 3.4 Deploy → run → collect
```bash
git clone https://github.com/AlphaBitCore/llm-gateway-benchmark
cd llm-gateway-benchmark
cp deploy.env.example deploy.env      # set AWS_PROFILE, region, key name, CIDRs, which Deploy<X> toggles
PROVISION=1 ./deploy.sh               # CFN stack up → gen inventory → ansible-playbook site.yml (installs gateways) → load test
```
`deploy.sh` is interactive (confirms the target account by printed ID before acting), resolves VPC/subnet/SG/AMI, then deploys. Results land under `benchmark-results/`. The load generator + mock are `AlphaBitCore/nexus-loadtest` and `AlphaBitCore/nexus-mock-provider` (installed by Ansible; nothing to run by hand).

### 3.5 Refresh the site data from a run
```bash
# from the site repo, pointed at the rig checkout's results:
python3 scripts/export-benchmark-json.py ../llm-gateway-benchmark/benchmark-results \
  -o data.json --embed index.html
```
- **Acceptance:** `benchmark-results/REPORT.md` regenerates; `data.json` `meta.provenance` shows the rig commit SHA with `remoteVerified: true`; the inline copy in `index.html` matches (the `--embed` flag keeps them from drifting).
- **Guardrail:** the exporter's `EXCLUDE` list keeps non-public/unsigned-off gateways (currently `agentgateway`) out of the site even if the results dir contains them. Don't remove an entry without verification + sign-off — it's a reviewable code diff on purpose.

### 3.6 TEAR IT DOWN (do not skip)
```bash
./down.sh          # cloudformation delete-stack (+ net-stack); typed-name confirmation
```
- **~8× c6i.4xlarge is real money per hour** — tear down the moment the run + data export are done. Verify zero instances remain: `aws ec2 describe-instances --filters Name=instance-state-name,Values=running --profile <profile> --region us-east-1 --query 'Reservations[].Instances[].InstanceId'`.
- Confirm the CloudFormation stack(s) reached `DELETE_COMPLETE`.

### 3.7 Optional / not now — AMI appliance path
The delivery-program doc also describes a Packer-built **AMI appliance** (single-instance, systemd) for a different distribution model. The **rig does not use it** (it installs host-native via Ansible). Ignore the AMI path unless Kash explicitly asks for the Marketplace/appliance track — it's out of scope for refreshing benchmark numbers.

---

## 4. Nexus Live Observatory (NEXUS-OBS-001) — work items & state

The benchmark site has been reframed as the **Live Observatory**: a public,
continuously re-verified engineering evidence center (daily scheduled runs,
append-only history, full provenance). V1 skeleton is **built**; these items
are extension/maintenance handoffs. Decisions locked: daily cadence · full
6-gateway set · in-repo history snapshots · GitHub Pages hosting.

| Item | What exists (built + verified) | What's next (the handoff) |
|---|---|---|
| **OBS-101 Evidence pipeline** | `scripts/export-benchmark-json.py` with `--history` (immutable `history/<ISO>.json` + `index.json`, idempotent — unchanged evidence mints no snapshot), provenance pinning (repo+SHA+remoteVerified), public-data `EXCLUDE` policy | Enrich provenance with per-gateway versions + run-config hash; land the exporter in the rig repo (`scripts/bench/`) |
| **OBS-102 Metrics API** | Static-file API on Pages: `data.json` + `history/` (contract documented in the site README) | Nothing until scale demands it; V2 = ClickHouse alignment with James's storage backend |
| **OBS-103 Portal** | Freshness badge (red when >48h stale), Evidence Timeline (per-metric trend charts from history, real snapshots only), Verification-status block, per-number provenance tooltips — all verified headlessly | Visual polish; per-point drill-in; "next scheduled run" once cron is live |
| **OBS-104 Runner** | `rig/observatory-run.yml` (daily cron + dispatch, OIDC-only AWS auth, vCPU-quota preflight, deploy→measure→snapshot→teardown, verify-zero-instances gate + always() failsafe cleanup job, 150-min cost ceiling) — YAML validated; staged in the site repo | The rig-repo PR: this workflow + `deploy.env.ci` + non-interactive `deploy.sh` (`CI=1`) / `down.sh --yes`; then a supervised first `workflow_dispatch` (AWS coworker, per §3); then merge to enable the cron |

**Cost note for James (with methodology sign-off):** full 6-gateway set daily ≈
$15–25/run → ~$450–750/mo at the current instance shape. Lever if too high: run
the daily verification on a smaller instance tier (relative rankings hold
within a tier) and keep quarterly deep-dives on c6i.4xlarge for absolute numbers.

---

## 5. Observatory BACKEND (live Run button) — B0 built, B1 = AWS

Per the 7/6 call, the site gains a **Run** button: a visitor triggers a preset benchmark and watches every gateway live; the completed run joins the evidence timeline. Backend lives in `obs-backend/` (Go service `obs-api`).

| Phase | State |
|---|---|
| **B0 — done + verified** | `obs-api` service (POST/GET runs + SSE stream), run state machine with **guaranteed teardown** + **concurrency=1**, presets, results pipeline into `history/`, and the frontend Run button + live cards. Runs end-to-end on a laptop via `OBS_DRIVER=sim` (no AWS/Docker). `go vet`+`build`+`test` green; live SSE + history-write verified. |
| **B1 — Kanishk (AWS) + Tieben (numbers)** | Implement `internal/driver/ec2.go` against the pre-baked stopped `c6i.4xlarge` Arena (start→health-gate→loadtest→stop→verify); set preset RPS = Tieben's Agent-Gateway audit-on saturation point in `arena-profile.json`. Orchestrator/API/results unchanged — it's one driver + one config value. |
| **B2 — public** | Lock CORS to the site origin, enable the public button, point the daily scheduled run at the Arena. |

Contract for B1 is documented in `obs-backend/internal/driver/ec2.go` + `obs-backend/README.md`. Teardown is guaranteed by the orchestrator and backstopped by the OBS-104 auto-stop watchdog — a failed run can't leave instances running.

---

## Shared blockers (surface early)

| Blocker | Owner | Unblocks |
|---|---|---|
| GitHub write access for the pushing account | org admin | Workstream 0 → everything |
| `alphabitcore` Docker Hub org + token | James | Workstream A1 |
| Methodology sign-off | James | Workstream B public launch |
| Route53 record for the custom domain | DNS owner | Workstream B custom domain |

---

## How to verify you're done (per workstream)
- **A:** images pull from Docker Hub; a fresh rig run regenerates `REPORT.md` + `data.json` (provenance SHA updated); **and the AWS stack is torn down (zero running instances, stack `DELETE_COMPLETE`).**
- **B:** site live at the custom domain over HTTPS; sign-off recorded.
- **C:** each increment is a merged PR passing build + i18n + design-token + IAM checks, with a working nav entry.
