# Control Plane UI — Overview section

The OVERVIEW section is the at-a-glance landing surface for administrators. Five sidebar leaves: **Dashboard**, **Traffic**, **Analytics & Metrics**, **Quota Usage**, **Cache ROI**. Sidebar labels and route paths are defined in `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`; labels resolve through `packages/control-plane-ui/src/i18n/locales/en/nav.json`.

## Data freshness and rollups

Four of the five pages — Dashboard, Analytics & Metrics, Quota Usage, and Cache ROI — read from a **pre-aggregated metric rollup pipeline**, not from raw request rows. Only **Traffic** reads individual `traffic_event` rows live; it is the place to look when you need the exact, immediate record of a single request. Understanding the rollup pipeline explains why the aggregate pages can lag a few minutes behind live traffic.

**Granularity.** The finest rollup bucket is **5 minutes**. The Hub runs a cascade of jobs that seal each window in turn: a 5-minute rollup folds raw events into 5-minute buckets, then a 1-hour merge folds 5-minute buckets into hourly buckets, a 1-day merge folds hourly into daily, and a 1-month merge folds daily into calendar-month buckets (UTC). A page picks its bucket size from the selected time span: spans up to 6 hours use 5-minute buckets, up to 90 days use 1-hour, up to 365 days use 1-day, and anything longer uses 1-month.

**Lag.** Each merge step only writes a bucket once that bucket has fully closed, so coarse tables trail real time by their own window length. To keep charts current, queries blend the coarse rollup with the freshest 5-minute tail, so a request that just landed typically appears in the aggregate views within about 5–6 minutes. A daily correction job re-runs the cascade over a 24-hour lookback to fold in events that arrived after their bucket had already sealed. The current calendar month is always excluded from the 1-month view, and latency / error metrics have no 5-minute layer — their finest bucket is 1 hour.

**Staleness signals.** Only **Cache ROI** surfaces an explicit indicator: when the rollup tables hold no rows for the chosen window, the page serves totals directly from raw events, shows a banner, and offers a manual **Trigger Rollup** action. Every other page renders an empty chart ("no data in window") while the pipeline catches up.

## Dashboard

**Purpose.** A single-screen health and business overview of the gateway across a chosen time window.

**What you see.** A hero strip with four KPI stats and a window picker (1h, 1d, 7d, 30d). Below the hero: a **System Health** four-card grid, a conditional **Latency Health** three-card row (when latency data is available), a **Business Snapshot** four-to-five card grid, and a **Top Providers** table.

**Key data.** System Health: combined-requests volume with a VK-vs-proxy split bar, error rate, P95 latency (subtitled with the busiest provider's `us / TTFB / upstream` breakdown), and compliance coverage. Latency Health: own overhead P95, upstream P95, slowest upstream provider. Business Snapshot: total cost, total tokens, active and total providers, cache hit rate, cache savings. Top Providers table columns: provider, requests, average latency, tokens, cost.

**Key actions.** Switch the time window. Click the slowest-provider card to jump to Analytics & Metrics. Click "View all" on Top Providers to jump to Analytics & Metrics.

**Where the data comes from.** Aggregates from the Control Plane admin analytics API: `analyticsApi.summary`, `analyticsApi.byProvider`, `analyticsApi.sparkline`, `analyticsApi.cacheROI`, `analyticsApi.latencyPhases`; provider list from `providerApi.list`; compliance proxy coverage from `proxyApi.getComplianceCoverage` and `proxyApi.getRejectStats`.

## Traffic

**Purpose.** A live event-by-event log of every request handled by any of the three intercept paths, with per-event drill-down.

**What you see.** A page header followed by **source tabs** — `All`, `VK`, `Proxy`, `Agent` — and below the active tab a filter panel, an active-filters chip bar, a paginated data table, and a slide-in event drawer triggered by row click. The selected source is mirrored in the URL via `?source=`. Each active filter is its own removable chip (click the chip's × to drop just that filter) and a Clear All control resets them together; chip labels are localized.

**Key data.** Each source has its own column set:

- **VK**: time, requested model, routed target, user, organization, project, virtual key, status, latency mini-bar, tokens, derived cost, hook decision, cache hit/miss.
- **Proxy**: time, target host, source IP, method, path, status, latency, bump status, hook decision, compliance tags.
- **Agent**: time, target host, path, device, user, source process, action, status, latency, hook decision, compliance tags.
- **All**: time, source badge, target, method, path, status, latency, hook decision, entity, organization.

The **Provider** and **Model** filters — and the Analytics group-by axes — attribute by the model/provider that actually **served** the request (the routed target), so a filter on "Provider X" returns everything X handled regardless of what the client originally asked for. The **requested** provider/model is shown only as the separate "requested model" column and is blank when the client did not pin one (for example `model="auto"` or an OpenAI-style request).

A **Modality** column shows each row's request type — a chip for image / TTS / STT / video / rerank / guardrail / realtime, or a dash for plain chat — so multimodal traffic is scannable at a glance; a matching `endpointType` filter narrows the list to one modality.

**Key actions.** Switch source tab. Open the filter panel for time range and advanced filters; apply, clear, or refresh. On the VK tab the advanced panel's CORRELATION group takes three exact-match ids at three grains — **Gateway request ID** (the `x-nexus-request-id` response header), **End-user ID** (the caller's own customer tag, from the `X-Nexus-End-User-Id` header or the protocol's native user field), and **Session ID** (the caller's conversation tag, `X-Nexus-Session-Id` header only) — so a support ticket that quotes any of the three lands directly on the matching slice. Click any row to open the event drawer (full request and response payload, hook trace, downstream timings). The drawer's Overview tab includes a **Correlation** section listing the request id, end-user id, session id (both gateway-only; an em dash means the caller sent no tag), and — when present — the client's own request id and the trace id, each with one-click copy. The first three also click-to-pivot: clicking the end-user id refilters the list to that user's entire traffic, the session id to that conversation's requests (most recent first), the request id to just that event. A pivot replaces any previously applied filters with the single clicked id and closes the drawer, so the pivoted list is immediately visible and shows the whole slice rather than an intersection with the old filters. The hook trace lists each hook that ran with its decision and execution time shown to microsecond precision (hooks routinely complete in well under a millisecond, so a millisecond figure would read as zero); a hook that a streamed response evaluated repeatedly is shown once with a ×N count and its total time, rather than one row per evaluation. For multimodal events (image generation, speech synthesis) the drawer's compliance section leads with a **coverage badge** stating what was actually scanned: *prompt-only* (the prompt text went through a content-scanning hook; the binary output is not inspectable) or *none* (no content hook evaluated the request — for example a hooks-off pipeline or an emergency bypass). The badge reflects what ran at request time, so coverage degradation is always visible rather than silently assumed. In the drawer's normalized payload view, content that a compliance hook redacted is marked inline as a badge over the replacement text, with a tooltip naming the rule, source, action, and reason. When the storage policy kept no readable copy at all, the payload view shows a notice instead of the content, and the notice distinguishes why: content dropped because the operator's policy says to drop it; or — when the policy was to redact but the redaction could not be safely applied to the stored copy — a separate notice explaining that the copy was dropped as the safe fallback, with the reason in plain words (the machine token shown alongside), the parts of the payload that could not be resolved, and the rules that matched. Events recorded before this distinction existed show a neutral "content not stored per the storage policy" notice that does not guess between the two. Paginate. Deep-link to a single event via `?thingId=`.

**Artifact preview.** For an image-generation or speech (TTS) event, the payload tab renders the captured artifact inline above the text views — the generated image as a picture, the synthesized speech as an audio player. The bytes are fetched through the authenticated admin API (`GET /traffic/{id}/artifact`, gated by the same traffic-log read permission — no new IAM surface) and served with an image/audio-only content type and `nosniff`, so a captured provider body never renders as active content; a returned image URL (which the gateway never fetches) shows a notice pointing to the response body instead. Events with no stored artifact (chat, embeddings, transcription, video, guardrail, realtime) show no preview.

**Normalized view.** The drawer's payload tab renders each direction as a typed, readable projection rather than raw bytes. A provenance badge says which decoder produced the view: **Tier 1** (exact protocol decoder, matched by key, host, or content sniff), **Tier 2** (pattern probe for consumer web surfaces) — both shown with a confidence score meaning the fraction of the input the decoder recognized — or the neutral **Structural** badge for rows where no AI protocol was identified and the body is shown as a typed projection of the raw HTTP content (JSON tree, text, form fields, binary digest, or an event-stream frame list). Structural rows deliberately carry no confidence numeral — the projection is faithful by construction, but it makes no claim about AI semantics. The normalized view is a derived projection: it is recomputed automatically when decoders improve, so a historical row's rendering (and its confidence) can get better over time — the raw bytes in the Raw tab are the immutable audit record and never change. An unrecognized event stream renders as a frame list — one row per frame with its event-name chip and the frame data pretty-printed when it is JSON — collapsed beyond the first 50 frames behind a "show all" control; very long streams note that the frame view is truncated while the full stream remains available in the Raw tab. Chat-style rows render role bubbles; a tool call's multi-line inputs (for example a shell command an agent ran) display with real line breaks, and the usage row includes reasoning tokens when the provider reported them. Multimodal rows read the same way: an image request shows the prompt as a user bubble and the response shows the provider's revised prompt plus an artifact card naming the image's size and type (never the multi-megabyte base64 itself), a speech (TTS) request shows the input text, and a transcription (STT) response shows the returned transcript when response-body capture is enabled (like every payload view, it needs the body to have been captured). A returned image URL is shown as plain, non-clickable text — the gateway never fetches it. Video job responses and reranking results render as their typed structural projection.

**Where the data comes from.** `systemApi.getTrafficStorage` (storage banner state, e.g. file-sink notice), `systemApi.listTrafficEvents` (the table query).

## Analytics & Metrics

**Purpose.** Time-bounded cost, usage, and latency breakdown across configurable group-by axes, with multi-tab depth for charts and rollups.

**What you see.** A page header and an inner tab group: **Analytics**, **Latency**, **Metrics**. Each tab carries its own filter bar (time range, source dropdown of `All Traffic / AI Gateway / Compliance Proxy / Agent`, plus a group-by axis on the Analytics tab); filters are independent per tab and do not carry across tabs. When a window has no data the page shows a plain-text empty state rather than an illustration.

**Key data.** The **Analytics** tab shows KPI stat cards (total requests, total cost, total tokens, average latency, cache hit rate, cache net savings), a cost-by-axis pie (top-N plus "Other"), a token-usage stacked bar (prompt and completion), and a breakdown table with per-row search and CSV export (requests, tokens, cost, cache hit rate, cache savings). The **Latency** tab shows the `LatencyPhasesPanel` — own-overhead / TTFB / upstream-body split. The **Metrics** tab embeds the rollup explorer (`MetricsRollupsSection`) with KPI cards, a system-overview chart set, and per-provider grids including a latency-phase stacked area.

**Key actions.** Select time range (24h, 7d, 30d, custom). Select group-by axis (`provider`, `model`, `user`, `organization`, `virtual_key`, `host`, `device`, `project` — the available axes filter by the chosen source). Toggle the source filter. Switch tab. Search and export CSV from the breakdown table.

**Where the data comes from.** `analyticsApi.summary`, `analyticsApi.cost`, `analyticsApi.usage`, `analyticsApi.cacheROI`, and `analyticsApi.metricsAggregates` (for the embedded Metrics tab).

## Quota Usage

**Purpose.** Show quota burn (spend versus configured limit) for a chosen entity scope and period, plus a top-consumers table.

**What you see.** A page header followed by two select filters, an **Overview** data table card, and a **Top Consumers** data table card.

**Key data.** Overview table columns: entity name, entity type, cost limit USD, current cost USD, usage percentage (progress bar coloured by alert level), and alert-level badge (`normal`, `warning`, `critical`). Top Consumers table columns: entity name, entity type, total cost USD.

**Key actions.** Pick the **period** (`monthly` or `weekly`) and the **scope** — the entity axis the burn is measured against (`user`, `project`, or `vk`).

**Where the data comes from.** `quotaAnalyticsApi.overview` and `quotaAnalyticsApi.top`.

## Cache ROI

**Purpose.** Quantify cache savings — both the gateway response cache and provider-native prompt cache — over a chosen window, with daily trend and per-adapter breakdown.

**What you see.** A page header with inline range buttons (7d, 30d, 90d), an optional rollup-not-ready banner that surfaces a "Trigger Rollup" action when the data source is the live store, a hero grid of four summary cards, a **Gateway Cache** section block (two cards), a **Provider Prompt Cache** section block (up to nine cards), a daily-savings line chart, and a per-adapter breakdown table.

**Key data.** Hero: combined savings (USD), savings rate (%), ROI multiplier (×), average savings per hit (USD). Gateway Cache section: gateway savings (USD), gateway cache hits. Provider Prompt Cache section: net savings, read savings, write cost, cache hits, read tokens, creation tokens, read multiplier, strip count, markers injected. Daily line chart series: gateway savings, read savings, write cost, net savings, total net. Adapter table: input and output tokens, gateway hits and savings, prompt-cache net savings / read savings / write cost / hits / read tokens / creation tokens, per-adapter savings rate.

**Key actions.** Switch the range. When the page is reading from the live data source (no recent rollup), click **Trigger Rollup** to fire the four cache-related rollup jobs through `hubApi.triggerJob`; the page auto-refetches after a short delay.

**Where the data comes from.** `analyticsApi.cacheROI`. The rollup trigger goes through `hubApi.triggerJob`.

## Bill Reconciliation

**Purpose.** Cross-check the gateway's own estimated spend against each provider's *authoritative* billing API, so a pricing-table drift, tokenizer mismatch, or silent provider price change shows up as a visible dollar difference instead of going unnoticed. A daily job pulls each covered provider's billed total; this page is the read-only report over the result.

**What you see.** A **reporting period** selector — **7d / 30d / 90d / 180d**, defaulting to 30d — and a table with one row per provider per day: the provider, the day, our estimate (USD), the vendor-billed amount (USD), the difference and difference %, a **coverage** badge, a status, and a review action. Below the table, a **"Providers not covered in v1"** panel names the providers that expose no usable cost API yet, with the reason — so the report's coverage boundary is explicit rather than implied by silence.

**Reporting period.** Day-count tokens, matching the Cache ROI dashboard's range selector so both Overview reports read the same way; reconciliation additionally offers **180d**, because billing questions are often raised a quarter or two after the fact and one row per provider per day stays small over a long window. The window counts whole UTC days back from today. It is useful beyond browsing history: after you narrow a provider's billing scope — moving the gateway into its own project, workspace, or API key — days before the cutover are not comparable to days after it, and a shorter window is how you look at only the comparable era without deleting the older rows. An empty result distinguishes the two cases it could mean: nothing *in this window* versus nothing at all (the latter only when the widest window is also empty).

**Key concepts.** The reconcilable unit is **provider × day × USD total** — not per-model or per-request, because neither our rollup nor the vendors' dollar endpoints break down finer. The **coverage** badge tells you how far to trust a row: `scoped` (narrowed to the gateway's own project/workspace — the only rows that can raise a drift alert), `org_only` (the vendor reported an organization-wide total the gateway couldn't narrow — shown for reference, never alerted), `fetch_failed` (the vendor lookup failed — no vendor number, no difference), and `priority_tier_undercount` (the vendor total is known to exclude a tier). A day that the vendor has not finalized yet simply has no row until it settles; an empty money cell shows `—`, never a fake `$0`.

**Key actions.** For an open row, type an optional note and click **Mark reviewed** to acknowledge a difference; the row records who reviewed it. A drift that later self-corrects (e.g. after a late vendor revision) resolves on its own.

**Coverage in v1.** Reconciled: **OpenAI** and **Anthropic** (both expose a dollar-denominated daily cost API). Not reconciled: **Google Gemini** (direct API has no cost API — only Vertex/BigQuery does), **DeepSeek**, and **Moonshot** (balance snapshot only).

**Where the data comes from.** `analyticsApi.vendorBillReconciliation` (read) and `analyticsApi.reviewVendorBillReconciliation` (the review acknowledgement). Rows are produced by the Hub `vendor-bill-reconcile` job; drift alerts by the `vendor-bill-drift-alerts` job (rule `vendor.bill_drift`).

## References

- `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` — route registry, including the `nav: { sectionKey: 'overview', ... }` blocks that define this section's sidebar entries
- `packages/control-plane-ui/src/i18n/locales/en/nav.json` — sidebar labels
- `packages/control-plane-ui/src/pages/dashboard/DashboardPage.tsx` — Dashboard
- `packages/control-plane-ui/src/pages/traffic/analytics/TrafficAnalyticsPage.tsx` — Traffic page shell (source tabs)
- `packages/control-plane-ui/src/pages/traffic/list/TrafficTab.tsx` — Traffic table and event drawer
- `packages/control-plane-ui/src/pages/traffic/filters/` — filter panel components
- `packages/control-plane-ui/src/pages/traffic/audit-drawer/` — per-event audit drawer
- `packages/control-plane-ui/src/pages/analytics/AnalyticsPage.tsx` — Analytics & Metrics
- `packages/control-plane-ui/src/pages/analytics/quota-usage/QuotaUsageDashboard.tsx` — Quota Usage
- `packages/control-plane-ui/src/pages/analytics/CacheROIDashboard.tsx` — Cache ROI
- `packages/control-plane-ui/src/pages/metrics/MetricsRollupsSection.tsx` — embedded rollup explorer used by Analytics & Metrics → Metrics tab
- `packages/control-plane-ui/src/api/` — `analyticsApi`, `quotaAnalyticsApi`, `systemApi`, `providerApi`, `proxyApi`, `hubApi`
- `packages/nexus-hub/internal/jobs/defs/rollup/` — the rollup and merge jobs (5-minute rollup, 1-hour / 1-day / 1-month merge, correction, retention)
- `packages/control-plane/internal/settings/store/metricsstore/metrics_rollup.go` — rollup-aware query that blends coarse buckets with the fresh 5-minute tail
- `packages/control-plane/internal/traffic/analytics/handler/cache_roi.go` — Cache ROI direct-vs-rollup data-source fallback
- `packages/shared/core/metrics/instruments/types.go` — bucket-size selection by query span
- `tools/db-migrate/schema/observability.prisma` — `MetricRollup5m` / `1h` / `1d` / `1mo`, per-node `ThingMetricRollup*`, and `RollupWatermark`
- `packages/control-plane-ui/src/pages/analytics/VendorBillReconciliationPage.tsx` — Bill Reconciliation report
- `packages/control-plane/internal/traffic/analytics/handler/vendor_bill_reconciliation.go` — Bill Reconciliation read + review-ack endpoints
- `packages/nexus-hub/internal/vendorbill/` — provider cost-API sources (`VendorBillSource`, OpenAI / Anthropic)
- `packages/nexus-hub/internal/jobs/defs/vendorbill/` — `vendor-bill-reconcile` daily job + `vendor-bill-drift-alerts` alert job
- `tools/db-migrate/schema/vendor_bill.prisma` — `vendor_bill_reconciliation` table
