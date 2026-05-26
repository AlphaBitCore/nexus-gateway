# E18 — Story 11: Admin API + Control Plane UI Surface

## Context

The three new columns (`api_key_class`, `api_key_fingerprint`, `usage_extraction_status`) are now present in `traffic_event` (s5) and populated by all three data planes (s7–s10). They must be exposed via the admin traffic/analytics APIs and rendered in the Control Plane UI.

Depends on s5, s7, s8, s9, s10.

## User Story

**As a** compliance officer using the Dashboard,
**I want** the traffic log and analytics pages to show api-key class, fingerprint, and usage-extraction status,
**so that** I can spot non-gateway key usage and aggregate cost by fingerprint directly from the UI.

## Tasks

### 11.1 OpenAPI spec update

File: `docs/users/api/openapi/admin/e18-s11-traffic-signal-fields.yaml`

Document the new response schema additions for:

- `GET /api/admin/traffic` (list with filters) — add `apiKeyClass`, `apiKeyFingerprint`, `usageExtractionStatus` to response item schema; add optional query params `apiKeyFingerprint`, `usageExtractionStatus` for filtering.
- `GET /api/admin/traffic/{id}` — add fields to detail response.
- `GET /api/admin/analytics/cost` (if surfaces per-key attribution) — add grouping option `apiKeyFingerprint` and corresponding response breakdown.

### 11.2 Control Plane admin handler

Files:
- `packages/control-plane/internal/store/traffic_event.go` — add fields to `TrafficEvent` struct; extend `trafficEventSelectColumns`; add `ApiKeyFingerprint` / `UsageExtractionStatus` filter params to `TrafficEventListParams`; extend the `WHERE` builder.
- `packages/control-plane/internal/handler/admin_traffic.go` — parse `apiKeyFingerprint` and `usageExtractionStatus` query params into the store params; no DTO layer (the store row is returned as JSON directly via the struct tags).
- `packages/control-plane/internal/handler/admin_analytics.go` — extend `AnalyticsCost` to accept `groupBy=apiKeyFingerprint`.

### 11.3 Control Plane UI — Traffic page

File: `packages/control-plane-ui/src/pages/traffic/TrafficLog.tsx` (or the current Traffic log page — locate by its fetcher against `/api/admin/traffic`).

- Add three table columns: API Key Class, Fingerprint (abbreviated display — show first 4 bytes + `…`), Usage Status.
- Fingerprint click → filter the table by that fingerprint.
- Status is rendered as a colored badge: `ok` / `streaming_reported` green, `streaming_estimated` yellow, `streaming_unavailable` / `parse_failed` red, `non_llm` / `no_body` gray.
- Column visibility toggleable via the existing column picker.

### 11.4 Control Plane UI — Analytics / Cost page

File: `packages/control-plane-ui/src/pages/analytics/Cost.tsx` (approx path).

- Add a "By API Key" pivot option alongside existing "By Provider" / "By Model" groupings.
- When pivoted by fingerprint, row label is fingerprint + class (e.g. `sk-ant-…a1b2c3d4`); source column indicates `ai-gateway` vs `compliance-proxy` vs `agent` to disambiguate VK-fingerprint vs real-key-fingerprint.
- UI copy explains the VK / real-key semantic distinction in an info tooltip.

### 11.5 i18n

Mandatory per CLAUDE.md: add keys for the new strings to `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/pages.json`. Technical terms stay in English (`API key`, `fingerprint`, `streaming`), but label wrappers are localized. Copy keys to `public/locales/` and verify counts match across all three locales.

Example keys to add:
- `analytics:traffic.column.apiKeyClass`
- `analytics:traffic.column.apiKeyFingerprint`
- `analytics:traffic.column.usageStatus`
- `analytics:traffic.usageStatus.ok` / `.streamingReported` / `.streamingEstimated` / `.streamingUnavailable` / `.parseFailed` / `.noBody` / `.nonLlm`
- `analytics:cost.pivot.byApiKey`
- `analytics:cost.apiKey.semanticTooltip`

### 11.6 `useApi` query keys

Per CLAUDE.md binding rule, the analytics pages must use properly-prefixed query keys. Example:

```ts
useApi(fetchTrafficList, ['admin', 'traffic', 'list', filters, offset, limit]);
useApi(fetchTrafficDetail, ['admin', 'traffic', 'detail', id]);
useApi(fetchCostByApiKey, ['admin', 'analytics', 'cost', 'by-api-key', dateRange]);
```

### 11.7 Tests

- Admin handler unit tests for the new filters and response fields.
- Control Plane UI: Vitest tests for the traffic column rendering and cost pivot.
- OpenAPI spec linting: `npx @redocly/cli lint docs/users/api/openapi/admin/e18-s11-traffic-signal-fields.yaml`.

## Acceptance Criteria

- OpenAPI spec passes linter; documented schema matches handler implementation exactly.
- Traffic page renders the three new columns; clicking a fingerprint filters the table.
- Cost page pivot by API key works for all three source types and explains the VK/real-key distinction.
- All new user-visible strings are localized in `en/zh/es` with matching key counts; no hardcoded English in JSX.
- `npm run test:ui` and `go test -race -count=1 ./packages/control-plane/...` pass.

## Non-Goals

- Exporting traffic rows to CSV (separate backlog).
- SIEM bridge updates (happens automatically once fields exist in the MQ stream).
- A dedicated "API Keys" management page (beyond scope).
