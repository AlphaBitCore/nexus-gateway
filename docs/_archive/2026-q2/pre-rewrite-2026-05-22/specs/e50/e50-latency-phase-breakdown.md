# E50 — Traffic Event Latency Phase Breakdown

## 1. Background

Every Nexus `traffic_event` row records a single `latency_ms` column representing
total wall-clock from request entry to response written. That single number conflates
two very different categories of time:

- **Our overhead** — virtual-key auth, quota check, routing decision, request
  hooks pipeline, prompt cache lookup, format adapter, response hooks pipeline.
  Typically tens of milliseconds for AI traffic.
- **Upstream latency** — the time a provider (Anthropic, OpenAI, Gemini, …) spends
  thinking and streaming back tokens. Typically hundreds of ms to several seconds.

Customers and operators looking at "the gateway took 3.5 s" today have no way to
tell which category dominated. The default reading is "Nexus is slow" — which is
both factually wrong in the typical case and bad for the platform's reputation
internally and with end users.

Per-phase timing exists in pieces across the codebase:

- ai-gateway records per-hook `latencyMs` inside `request_hooks_pipeline` JSONB.
- ai-gateway records routing stage durations inside `routing_trace` JSONB.
- compliance-proxy records TLS handshake duration in Prometheus only.
- Various Prometheus histograms exist (`request_duration_ms`, `hook.duration`,
  `upstream.request_ms`, `tls.handshake_ms`) — none flow back into `traffic_event`.
- agent records nothing beyond total `duration_ms`.

E50 consolidates these signals into a unified, queryable, UI-renderable phase
taxonomy across all three forwarding services and surfaces it everywhere the
platform displays latency today (CP-UI Dashboard / Analytics / Model Detail /
Nodes Details / Traffic; agent desktop UI Overview / Stats / Traffic).

## 2. Scope

### Must

**M1 — Five phase columns on `traffic_event`.**

- `upstream_ttfb_ms`, `upstream_total_ms`, `request_hooks_ms`,
  `response_hooks_ms`, `latency_breakdown` (JSONB).
- All nullable; pre-E50 rows are NULL until backfill runs.
- `our_overhead_ms` is NOT stored — it's derived from
  `latency_ms - upstream_total_ms` at read time (admin API responses, UI
  computation).

**M2 — Closed JSONB key set per `source`.**

- `latency_breakdown` carries a fixed key set determined by row's `source`:
  - `ai-gateway`: `{auth_ms, quota_ms, routing_ms, cache_lookup_ms, req_adapter_ms, resp_adapter_ms}`
  - `compliance-proxy`: `{conn_setup_ms, tls_handshake_ms}`
  - `agent`: `{intercept_ms}`
- Keys are omitted when the phase did not run or measured zero.
- No ad-hoc keys at write time; the schema is the same enum across hot path
  and analytics.

**M3 — TTFB and upstream_total measured via `httptrace.ClientTrace`.**

- The forwarding code in all three services wraps its outbound `http.Transport`
  with `httptrace.ClientTrace.GotFirstResponseByte` to capture TTFB at the
  HTTP-roundtrip layer.
- Streaming responses: `upstream_ttfb_ms` is the first non-zero bytes read from
  the upstream response body (= first SSE chunk arrival). `upstream_total_ms` is
  the upstream connection close instant.
- Client-side abort during streaming: record `upstream_total_ms` at abort time
  and stamp `latency_breakdown.stream_aborted: true`.

**M4 — `our_overhead_ms` is the headline product metric.**

- Computed as `latency_ms - upstream_total_ms` at read time, clamped to ≥0.
- Every CP-UI surface that today shows a single Avg/P95 latency MUST add a
  parallel surface for our_overhead so operators see "Us vs Upstream" at a glance.

**M5 — Agent has parity with compliance-proxy in phase coverage.**

- Agent populates `upstream_ttfb_ms`, `upstream_total_ms`, `request_hooks_ms`,
  `response_hooks_ms`, and `latency_breakdown.intercept_ms`.
- Agent treats upstream as an opaque destination (no provider awareness).
- Cross-service correlation (agent → ai-gateway → provider) is reconstructed at
  query time by joining on `trace_id`, not by either side writing the other's
  fields.

**M6 — Historical backfill (Hub + agent local).**

- Hub-side: a batched, resumable SQL script
  (`tools/db-migrate/manual-scripts/e50_backfill_latency_phases.sql`) reconstructs
  `request_hooks_ms`, `response_hooks_ms`, `latency_breakdown.routing_ms`
  (exact), and `upstream_total_ms` (approximate residual). `upstream_ttfb_ms` is
  not reconstructable and stays NULL.
- Agent-side: an equivalent one-shot backfill runs once per agent on first boot
  post-upgrade against the local SQLite `audit_events` table.
- Historical-vs-measured rows are NOT visually distinguished in UI. No
  `is_estimated` badge, no Analytics filter — the platform decision is that
  reconstructed data is treated as equivalent to measured data.

**M7 — CP-UI fleet pages surface phase data.**

- **Dashboard**: new 4-tile "Latency Health" row beneath System Health
  (`Our Overhead P95` / `Upstream TTFB P95` / `Upstream Total P95` /
  `Slowest Upstream Provider` callout).
- **Analytics**: new "Latency" tab beside Analytics / Metrics, containing
  4 phase KPIs, stacked-area time-series, Provider Leaderboard card, breakdown
  table extended with phase columns.
- **Model Detail Usage**: existing Avg Latency tile splits into Us / Upstream;
  3 breakdown tables (project / VK / model) gain phase columns.
- **Nodes Details Stats**: `ThingStatsTab` metric catalog adds 4 phase metrics;
  trend charts render stacked-area for traffic-processing Things.
- **Traffic list**: existing single latency column replaced by `Us · Upstream`
  dual chip; row click opens a Detail Waterfall drawer with 5-segment phase
  bar (request_hooks / our_other / upstream_ttfb / upstream_body /
  response_hooks).

**M8 — Agent desktop UI surfaces phase data.**

- **Overview**: 4th tile "Today's latency" with `Us · Upstream` + sparkline; the
  Recent Activity table gains a latency chip column.
- **Stats**: Avg Latency KPI splits into Avg Us / Avg Upstream; MiniLineChart
  gains a metric switcher (Requests / Our Overhead / Upstream / Both); the
  breakdown table (by `target_host`) gains `Avg Us` / `Avg Upstream` columns —
  giving single-laptop users a per-destination latency view.
- **Traffic list**: identical UX to CP-UI Traffic list (dual chip + Waterfall
  drawer), implemented in the Wails React UI.

**M9 — New admin API endpoint + extensions.**

- `GET /api/admin/analytics/latency-phases?groupBy=<dim>&start=<iso>&end=<iso>&source=<filter>`
  returns P50/P95/P99 for each phase per dimension row.
- Existing `summary`, `sparkline`, `by-provider` responses are extended with
  phase fields (`usOverheadP95`, `upstreamTtfbP95`, `upstreamTotalP95`, plus
  per-bucket sums on sparkline).
- All endpoints gated by `admin:observability.read` (same as existing
  analytics endpoints).

**M10 — Per-Thing stats rollup carries phase metrics.**

- `thing_metric_rollup_local_*` (agent SQLite) and `thing_metric_rollup_*`
  (Hub) gain matching phase metric names (`latency_us`,
  `latency_upstream_ttfb`, `latency_upstream_total`, `latency_hooks`).
- The metric catalog (`thingStatsMetricCatalog.ts`) surfaces these as KPI
  cards + trend charts on `ThingStatsTab`.

### Should

**S1 — TTFB tick on the Detail Waterfall.**

- The upstream segment of the waterfall renders a vertical tick at the
  `upstream_ttfb_ms` boundary so operators see header-time vs body-streaming time
  inside the upstream slice.

**S2 — Provider Leaderboard click-through.**

- Clicking a provider row in the Analytics Latency Tab leaderboard drills
  through to a pre-filtered Analytics Latency view (groupBy=model,
  provider=<clicked>).

### Could

**C1 — Hook-level P95 trend on agent Policies → Hook Detail.**

- The agent's Policies page hook detail (currently no traffic data) could show
  P95 hook execution time across recent traffic. Useful for hook authors;
  optional V1.

**C2 — Inter-chunk P95 for streaming responses.**

- Compute `inter_chunk_p95_ms` from streaming reads and store under
  `latency_breakdown`. Useful for detecting upstream mid-stream stalls. Deferred
  pending demand.

### Won't (V1)

- Per-request waterfall chart on the *agent desktop UI* (the CP-UI Traffic
  Detail Waterfall is the primary deep-dive surface; the agent UI keeps to KPI
  chip + segmented bar in the Traffic Detail drawer without a full Gantt).
- Distributed-trace stitching across services. Each row owns its hop; cross-hop
  views are SQL-time joins on `trace_id`, not new endpoints.
- Per-hook latency rendered on CP-UI (the data is in `request_hooks_pipeline`
  JSONB already; UI work is out of scope for E50).
- Backfilling `upstream_ttfb_ms` on historical rows (not reconstructable).

## 3. Non-Functional Requirements

- **Performance** — Phase capture overhead on the hot path must stay below
  100µs per request (httptrace + a few `time.Now()` calls). Verified by
  microbenchmark.
- **Storage** — Each row grows by ~40 bytes (5 Int columns + small JSONB).
  Acceptable for the existing hot table.
- **Indexing** — Add GIN index on `latency_breakdown` JSONB only if a Phase
  Breakdown query proves slow in practice. Initial deploy ships without it
  to avoid write amplification.
- **Backfill safety** — Hub SQL script runs in 10k-row batches with a
  500ms inter-batch sleep; resumable via `id` cursor; full table backfill
  must complete in ≤2h on prod-scale data without affecting write latency.
- **API compatibility** — The existing `analytics/summary` /
  `analytics/sparkline` / `analytics/by-provider` responses are extended
  *additively*; pre-E50 clients see the new fields as ignored extras.

## 4. User Roles & Personas

| Role | What E50 gives them |
|---|---|
| **End customer (looking at admin UI)** | Trust signal: "Nexus added 80ms; the provider was the 3.4s" |
| **SRE / on-call** | Operational triage: which provider's TTFB is degrading right now? Which agent node is slow? |
| **Provider admin** | Vendor selection: P95 TTFB and upstream_total per provider over the last 7d |
| **Compliance officer** | Performance budget tracking: how much do hooks cost in the median request? |
| **Agent end user (laptop)** | Self-debug: "Is the agent slow on my machine, or is the network/provider slow?" — visible in agent UI Stats per `target_host` |
| **Platform developer** | Performance regression detection: did a change to the routing engine add 5ms? |

## 5. Constraints & Assumptions

- All three forwarding services share `shared/traffic/phasetimer.go` so the
  phase taxonomy is single-sourced. Diverging definitions across services is
  explicitly forbidden.
- The agent UI does not call Hub APIs; all phase data is captured locally,
  stored in SQLite, and rendered by Wails IPC. The same applies to backfill.
- Pre-GA, no installed user base, no backward-compat layer. Pre-E50 rows are
  backfilled in-place; the wire format adds fields additively but old rows
  with NULL upstream_ttfb render as "—" in dual chips.
- Prometheus histogram instrumentation is kept untouched — it remains the
  authoritative metric pipeline. E50 adds DB-row capture *in parallel*, so
  alerting based on Prometheus continues to work.

## 6. Glossary

- **Phase** — A named segment of the request lifecycle (auth, hooks,
  upstream, …). Captured as a duration in milliseconds.
- **Our overhead** — `latency_ms - upstream_total_ms`. The time Nexus itself
  spent on this request, excluding upstream provider time.
- **Upstream** — From the HTTP forwarding layer's POV: the destination this
  service called over the wire. For ai-gateway and compliance-proxy that's a
  real LLM provider; for the agent it's whatever the user's machine sent
  the request to (often ai-gateway or compliance-proxy).
- **TTFB** — Time-To-First-Byte: elapsed from request send to first byte
  of response read. For streaming, the first SSE chunk's arrival.
- **Phase backfill** — One-time SQL/code job that reconstructs phase
  fields on pre-E50 rows from existing JSONB and total latency.

## 7. Priority (MoSCoW)

- Must: M1–M10
- Should: S1, S2
- Could: C1, C2
- Won't (V1): per-request waterfall on agent UI, distributed-trace stitching,
  per-hook UI surfacing on CP-UI, TTFB backfill.
