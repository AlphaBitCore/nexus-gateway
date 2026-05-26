# Overview section — CP-UI feature doc

> Audience: operators using the dashboard. The "Overview" sidebar section is the day-one landing for traffic and analytics surfaces. Architecture references at the bottom.

## Pages in this section

| Page | Path | IAM action | Purpose |
|---|---|---|---|
| Dashboard | `/` (index) | — (open to any authenticated user) | Landing page for the section: top-level health + activity summary, links into the surfaces below |
| Traffic | `/traffic` | `admin:traffic-log.read` | Real-time + historical traffic events with filters, drill-down to body, hook decisions, routing trace |
| Analytics | `/analytics` | `admin:analytics.read` | Aggregate views: volume, latency, top providers/models, error rates, cost trend |
| Metrics Explorer | `/metrics` | `admin:analytics.read` | Prometheus-backed metric explorer for ad-hoc dashboards |
| Quota Usage | `/quota-usage` | `admin:quota-analytics.read` | Current usage vs limit per quota policy + projected exhaustion |
| Cache ROI | `/cache-roi` | `admin:analytics.read` | Prompt-cache hit-rate by tier, cost saved estimates |

Route table source of truth: `packages/control-plane-ui/src/routes/shellRouteConfig.tsx:88-115`. The Dashboard entry at line 90 declares no `allowedActions`, so every authenticated principal lands here regardless of permissions; the remaining rows are gated as shown.

## Common workflows

- **Investigate a specific request** — Traffic page → filter by VK / request_id → drill into row → view body / hook trace / routing trace. The `request_id` joins to admin audit and agent audit (cross-ref `trace-id-propagation-architecture.md`).
- **Spot a cost spike** — Analytics → time-bucketed cost grouped by provider / model / org. Drill to the org with the spike. Optionally cross to Quota Usage to see whether a quota is being burned.
- **Tune prompt-cache** — Cache ROI → identify routes with low hit rates → adjust the fleet-wide policy under Cache settings (AI Gateway → Cache; one global setting, never per-route).
- **Detect provider degradation** — Analytics → error rate filter on `error_class=Network|5xx|Timeout` → cross-reference Alerts page if rules fired.

## Data sources & API endpoints

- `GET /api/admin/traffic-events` — main traffic page.
- `GET /api/admin/analytics/*` — analytics aggregations.
- `GET /api/admin/quota-usage` — quota-usage snapshot.
- `GET /api/admin/cache-roi` — prompt-cache hit-rate aggregations.
- `GET /api/admin/metrics/prom-query` — Prometheus passthrough for Metrics Explorer.

## Failure modes & gotchas

- **Body fetch on large rows** — bodies ≥ 256 KiB are in spillstore; the UI fetches via presigned URL on click (cross-ref `audit-pipeline-architecture.md` §7). A failed body fetch surfaces as "Body unavailable" with the underlying error reason.
- **Late-arriving traffic events** — Agent-originated events can lag (offline → drain) by minutes-to-hours. The Traffic page surfaces a "Last drained at" badge per device.
- **Analytics window vs raw** — Analytics queries use materialised rollups for windows > 7 days; raw scans run live for shorter windows. A query that spans the boundary may show a tiny seam.
- **Per-VK cost requires `usage` stamped** — providers without complete `usage.cost_usd` produce best-effort cost estimates flagged with an asterisk.

## Architecture references

- `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md` — where traffic events come from.
- `docs/developers/architecture/cross-cutting/observability/trace-id-propagation-architecture.md` — how rows correlate.
- `docs/developers/architecture/cross-cutting/safety/quota-architecture.md` — what Quota Usage reads.
- `docs/developers/architecture/services/ai-gateway/prompt-cache-architecture.md` — what Cache ROI reads.
- `docs/developers/architecture/services/control-plane/tenancy-architecture.md` — org-rollup grouping.
