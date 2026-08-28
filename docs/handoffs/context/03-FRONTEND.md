# 03 · Frontend — the comparison site, the Run button, and the Console dashboard

Two frontends: (A) the **public comparison site / Observatory portal** (standalone,
GitHub Pages) and (B) the **Control Plane admin dashboard** (inside the product,
`packages/control-plane-ui`). They are separate surfaces.

---

## A. Public comparison site / Observatory portal

Repo `Kaushik985/nexus-benchmark-site`, single self-contained `index.html`
(inline CSS/JS, SVG gauges — no framework, no build step). Data from `data.json`
(with an inline `file://` fallback so it works opened directly).

### Design language (Bifrost-style, honest)
- Teal = Nexus, gray = competitor; semicircle gauge cards; improvement badges.
- **Honesty rules baked in (must not be removed):** never claim a metric the data doesn't support (success-rate + memory are NOT headlined — all gateways hit 100% at tested load); show hooks-on AND hooks-off; render **"behind"** honestly where Nexus-with-governance trails; keep the governance caveat.

### Sections built
- **Hero + controls** — "Nx the throughput of <competitor>" headline; live toggles: competitor selector, scenario selector (6 tiers), hooks on/off.
- **4 gauge cards** — Throughput / P99 latency / TTFT p95 / Success rate (Nexus vs competitor + badge).
- **Full comparison table** — every gateway at the scenario; `bare`/`governed` tags.
- **Governance caveat** — raw vs governed speed; names any bare-proxy that ran without audit.
- **Methodology & honest disclosure** — rig facts, n=2, "operated by us," provenance.
- **Observatory additions (v5):** freshness badge (green ≤48h, red when stale — currently red: last verified 2026-06-28); **Evidence Timeline** (per-metric trend charts from real `history/` snapshots, section hides until history exists); verification-status block; per-number provenance tooltips.
- **Live Arena (v6):** the **Run button** + live per-gateway cards (RPS + p99 + climbing memory bar), SSE-driven from obs-api. Activates only when an API base is set (`window.OBS_API_BASE` or `?obsapi=…`); otherwise degrades honestly to "backend connects in B1" — no fake activity.

### Real numbers it renders (v2 run, illustrative until Tieben's V3)
vs **LiteLLM**: ~32× throughput (23× with hooks on), ~13× lower P99, ~21× lower TTFT.
vs **Bifrost**: 3–6× on streaming, ~1.2× non-streaming hooks-off, ~par with hooks on.
**Agent Gateway excluded** from public data pending verification (raw-faster but drops audit / OOMs) — enforced by the exporter's `EXCLUDE` list (a code diff to change, not a data edit).

### Data flow
Rig summaries → `scripts/export-benchmark-json.py --embed index.html --history history/`
→ `data.json` + `history/<ts>.json`. `meta.provenance` pins source repo + commit;
`remoteVerified` true only for the canonical public repo.

### Hosting status (see `PUBLISH.md`)
Private repo on free plan → **GitHub Pages unavailable** (deploy workflow disabled to stop failures). Options: split a **public** site-only repo (recommended), upgrade plan, or make repo public (leaks internal docs — no). Custom domain `benchmark.alphabitcore.com` (CNAME staged). **Local preview:** `python3 -m http.server 8080`.

---

## B. Control Plane admin dashboard (`packages/control-plane-ui`)

The in-product admin console. **Increment 1 shipped as PR #82** (`2dfe77e`).

### What already existed (68 routes — not rebuilt)
Dashboard home (Hero/Health/Latency/BusinessSnapshot/Providers), Traffic, Analytics, Quota, Cache ROI, all AI-Gateway pages, all Compliance pages, Status/Health, Infrastructure.

### Increment 1 (built, PR #82) — dashboard-home enhancements, no new routes
1. **Compliance-coverage warning banner** — when windowed coverage is 0%, an explicit callout so the KPI reads as the governance gap it is.
2. **End-to-End P95 card** — alongside Gateway Overhead P95 + Upstream P95, making the overhead-vs-upstream split first-class (from `analyticsApi.latencyPhases`).
3. **Traffic-source selector** — All / Virtual keys / Proxy / Agent, wired into the analytics queries.
- **Deferred deliberately** (no dead controls / fake data): environment selector + Active-Workloads KPI.
- **Bindings honored:** i18n EN/ES/ZH parity, design tokens only, domain-prefixed queryKeys; `tsc` + `vite build` green.

### Deferred increments (spec'd, not built) — each its own future PR
- **Benchmarks** `/benchmarks{,/:runId,/compare,/artifacts}` — reads local SUMMARY.csv/JSON; publishable flag + methodology box + per-gateway charts.
- **Workloads / Agent Teams** `/workloads` — derived from VK naming + traffic_event; disambiguates Agent Teams vs Agent Gateway vs Nexus AI Gateway.
- **Deployments** `/deployments` — health of the 4 images + Postgres/Valkey/NATS + config-sync + vectorscan/PII self-test.
- **Compliance health cards** — hooks-enabled / redactions / vectorscan-self-test / PII-smoke.

The full design spec (operations + benchmarking + workloads control surface) lives
in the "Dashboard UI — Product + UI Plan" doc and is summarized as workstream C in
`nexus-delivery-handoff.md`.

---

## Reusable kit (for both, confirmed paths)
`components/ui/{Card,DataTable,Grid,Stack,Sparkline,PageHeader,ListFilterToolbar,Tabs}`,
`components/charts/{TimeSeriesChart,LatencyMini,LatencyWaterfall}`, `useApi` hook,
i18n namespaces (`pages`/`nav`/`common`/`shared`), design tokens via
`@nexus-gateway/ui-shared`. New route recipe: `shellRouteConfig.tsx` + lazy import
+ page + i18n keys (3 locales) + `allowedActions` + IAM review.
