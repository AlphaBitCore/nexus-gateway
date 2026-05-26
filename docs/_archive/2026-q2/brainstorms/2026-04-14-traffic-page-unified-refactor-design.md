# Traffic Page Unified Refactor — Design Spec

**Date**: 2026-04-14
**Status**: Draft
**Scope**: Full refactor of `/traffic` page, backend API rename/cleanup, seed data improvement

## Background

The `traffic_event` table was consolidated from 4 separate audit tables into a single unified table with a `source` discriminator (`vk | proxy | agent | admin | device-lifecycle`). The current `/traffic` page only shows `source='vk'` events, uses legacy `audit-logs` API naming, and displays raw UUIDs instead of resolved entity names.

## Goals

1. Show all data-plane traffic sources (vk, proxy, agent) on the `/traffic` page via source tabs
2. JOIN and resolve identity/provider fields to human-readable names (LEFT JOIN — all nullable)
3. Rename API endpoints from `audit-*` to `traffic/*` for clarity
4. Remove legacy/redundant endpoints (`audit-logs`, `audit/unified`, `audit/storage`)
5. Remove the `/audit-unified` UI page (functionality absorbed by refactored `/traffic`)
6. Improve seed data so all joinable fields have coverage for demo
7. Preserve existing Analytics and Metrics tabs unchanged

## Non-Goals

- Changing the AdminAuditLog page (`/audit-logs` UI route, `admin-audit-logs` API) — stays as-is
- Modifying the `traffic_event` schema itself — no migrations
- Real-time SSE push for non-VK sources (extend later if needed)

---

## 1. API Changes

### 1a. New Endpoints (replace old)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/admin/traffic` | List traffic events with filtering + pagination |
| `GET` | `/api/admin/traffic/:id` | Get single traffic event by ID |
| `GET` | `/api/admin/traffic/storage` | Traffic sink configuration |
| `GET` | `/api/admin/traffic/stream` | SSE live stream (already exists at this path) |

### 1b. Deleted Endpoints

| Path | Reason |
|------|--------|
| `GET /api/admin/audit-logs` | Replaced by `/api/admin/traffic` |
| `GET /api/admin/audit-logs/:requestId` | Replaced by `/api/admin/traffic/:id` |
| `GET /api/admin/audit/unified` | Functionality merged into `/api/admin/traffic` |
| `GET /api/admin/audit/storage` | Moved to `/api/admin/traffic/storage` |

### 1c. Unchanged Endpoints

| Path | Reason |
|------|--------|
| `GET /api/admin/admin-audit-logs` | Separate admin audit concern |
| `GET /api/admin/admin-audit-logs/export` | Separate admin audit concern |
| `GET /api/admin/me/admin-audit-logs` | Separate admin audit concern |

### 1d. `GET /api/admin/traffic` Query Parameters

All existing filter params carry over, plus:

| Param | Type | Description |
|-------|------|-------------|
| `source` | string | Comma-separated source filter: `vk`, `proxy`, `agent`. Empty = all data-plane sources |
| `targetHost` | string | Filter by target_host (exact match) |
| `deviceId` | string | Filter by device_id |
| `sourceProcess` | string | Filter by source_process (ILIKE) |
| `bumpStatus` | string | Filter by bump_status |

Existing params retained: `provider`, `userId`, `orgId`, `virtualKeyId`, `projectId`, `department`, `modelUsed`, `requestId`, `hookDecision`, `responseHookDecision`, `statusCode`, `statusRange`, `cacheHit`, `startTime`, `endTime`, `limit`, `offset`.

### 1e. Response Shape

The response shape stays the same: `{ data: TrafficEvent[], total: number, limit: number, offset: number }`.

Each `TrafficEvent` object includes resolved name fields from LEFT JOINs:

```jsonc
{
  "id": "...",
  "source": "vk",
  "timestamp": "...",
  // Request
  "sourceIp": "10.0.0.1",
  "targetHost": "api.openai.com",
  "method": "POST",
  "path": "/v1/chat/completions",
  "statusCode": 200,
  "latencyMs": 420,
  // Identity (IDs + resolved names via LEFT JOIN)
  "userId": "nexus-user-agent-jdoe",
  "userDisplayName": "John Doe",          // LEFT JOIN NexusUser
  "organizationId": "org-engineering",
  "organizationName": "Engineering",       // LEFT JOIN Organization
  "projectId": "proj-alpha",
  "projectName": "Project Alpha",          // LEFT JOIN Project
  "virtualKeyId": "vk-001",
  "virtualKeySlug": "engineering-openai",  // LEFT JOIN VirtualKey
  "credentialId": "cred-001",
  "credentialName": "openai-prod",         // LEFT JOIN Credential
  "deviceId": "dev-001",
  "deviceHostname": "mac-eng-01.corp.local", // LEFT JOIN AgentDevice
  "subjectId": null,
  "department": "Engineering",
  // AI/Provider
  "provider": "openai",
  "modelUsed": "gpt-4o",
  "promptTokens": 1200,
  "completionTokens": 800,
  "totalTokens": 2000,
  "estimatedCostUsd": 0.034,
  "cacheHit": false,
  "routedProvider": "openai",
  "routedModel": "gpt-4o",
  "routingRuleId": "rule-001",
  "routingRuleName": "default-openai",     // LEFT JOIN RoutingRule
  // Compliance
  "hookDecision": "approve",
  "hookReason": null,
  "hookReasonCode": null,
  "responseHookDecision": null,
  "dataClassification": "INTERNAL",
  "bumpStatus": null,
  // Agent-specific
  "sourceProcess": null,
  "action": null,
  // JSONB
  "hooksPipeline": [...],
  "routingTrace": {...},
  "details": {...},
  "createdAt": "..."
}
```

---

## 2. Backend Changes

### 2a. Store Layer (`internal/store/audit_log.go`)

**Rename file** to `traffic_event.go` for clarity.

**Struct rename**: `AuditLog` → `TrafficEvent`. Add fields:
- `Source string`
- `TargetHost *string`
- `DeviceID *string`
- `DeviceHostname *string` (from LEFT JOIN AgentDevice)
- `SubjectID *string`
- `UserDisplayName *string` (from LEFT JOIN NexusUser)
- `CredentialName *string` (from LEFT JOIN Credential)
- `RoutingRuleName *string` (from LEFT JOIN RoutingRule)
- `SourceProcess *string`
- `Action *string`
- `BumpStatus *string`

**Params rename**: `AuditLogListParams` → `TrafficEventListParams`. Add fields:
- `Source string` (comma-separated, e.g. "vk,proxy")
- `TargetHost string`
- `DeviceID string`
- `SourceProcess string`
- `BumpStatus string`

**SQL changes in `ListTrafficEvents`**:
- Remove hardcoded `WHERE a.source = 'vk'`
- Default to `WHERE a.source IN ('vk','proxy','agent')` (exclude admin/device-lifecycle from traffic page)
- When `Source` param is set, filter to those specific sources
- Add LEFT JOINs:
  ```sql
  LEFT JOIN "NexusUser" u ON u.id = a.user_id
  LEFT JOIN "Credential" cred ON cred.id = a.credential_id
  LEFT JOIN "AgentDevice" dev ON dev.id = a.device_id
  LEFT JOIN "RoutingRule" rr ON rr.id = a.routing_rule_id
  ```
  (Organization, Project, VirtualKey JOINs already exist)

**Delete**: `GetUnifiedAudit` and `UnifiedAuditRow` from `misc_queries.go`.

### 2b. Handler Layer (`internal/handler/admin_audit.go`)

**Rename file** to `admin_traffic.go`.

**Route registration**: rename `RegisterAuditRoutes` → `RegisterTrafficRoutes`:
```go
g.GET("/traffic", h.ListTrafficEvents, iamMW("admin:ReadTrafficLog"))
g.GET("/traffic/:id", h.GetTrafficEvent, iamMW("admin:ReadTrafficLog"))
g.GET("/traffic/storage", h.TrafficStorage, iamMW("admin:ReadTrafficLog"))
```

**Delete**: `AuditUnified` handler, `ListAuditLogs` handler, `GetAuditLog` handler.

**Source param parsing**: support comma-separated values:
```go
source := c.QueryParam("source") // "vk", "proxy", "agent", "vk,proxy", or ""
```

### 2c. IAM Action Rename

- `admin:ReadAuditLog` → `admin:ReadTrafficLog` for traffic endpoints
- Keep `admin:ReadAuditLog` for admin-audit-logs endpoints
- Update IAM seed policies accordingly

---

## 3. Frontend Changes

### 3a. Types (`api/types.ts`)

Rename `AuditLogEntry` → `TrafficEvent`. Add all new fields from Section 1e response shape. Remove the old type.

### 3b. API Service Layer

**`api/services/system.ts`**:
- `listAuditLogs()` → `listTrafficEvents()`, pointing to `/api/admin/traffic`
- `getAuditStorage()` → `getTrafficStorage()`, pointing to `/api/admin/traffic/storage`
- Add `getTrafficEvent(id)` → `/api/admin/traffic/:id`

**Delete**: `api/services/unified-audit.ts` (entire file).

### 3c. Page Structure

Current: `TrafficAnalyticsPage` has 3 outer tabs: `Live Traffic | Analytics | Metrics`.

After refactor: the outer tab structure stays. Inside `Live Traffic`, add **source sub-tabs**:

```
TrafficAnalyticsPage
├── Tab: Live Traffic
│   ├── Sub-tab: All        (source = vk,proxy,agent)
│   ├── Sub-tab: VK Traffic (source = vk)
│   ├── Sub-tab: Proxy      (source = proxy)
│   └── Sub-tab: Agent      (source = agent)
├── Tab: Analytics           (unchanged)
└── Tab: Metrics             (unchanged)
```

### 3d. Tab Column Definitions

**All (generic):**

| Column | Field | Render |
|--------|-------|--------|
| Time | `timestamp` | formatted datetime |
| Source | `source` | badge (vk/proxy/agent) |
| Target | `targetHost` | text or "-" |
| Method | `method` | text |
| Path | `path` | truncated |
| Status | `statusCode` | colored badge |
| Latency | `latencyMs` | "{ms} ms" |
| Hook | `hookDecision` | text |
| User | `userDisplayName` | fallback to userId, then "-" |
| Organization | `organizationName` | fallback to orgId prefix, then "-" |

**VK Traffic:**

| Column | Field | Render |
|--------|-------|--------|
| Time | `timestamp` | formatted datetime |
| Provider | `provider` | text |
| Model | `modelUsed` | text |
| User | `userDisplayName` | fallback chain |
| Organization | `organizationName` | fallback chain |
| Project | `projectName` | fallback chain |
| Virtual Key | `virtualKeySlug` | fallback chain |
| Status | `statusCode` | colored badge |
| Latency | `latencyMs` | "{ms} ms" |
| Tokens | `totalTokens` | formatted number |
| Cost | `estimatedCostUsd` | "$X.XXXX" |
| Hook | `hookDecision` | text |
| Cache | `cacheHit` | HIT/MISS badge |

**Proxy:**

| Column | Field | Render |
|--------|-------|--------|
| Time | `timestamp` | formatted datetime |
| Target Host | `targetHost` | text |
| Source IP | `sourceIp` | text |
| Method | `method` | text |
| Status | `statusCode` | colored badge |
| Latency | `latencyMs` | "{ms} ms" |
| Bump Status | `bumpStatus` | badge |
| Hook | `hookDecision` | text |
| Classification | `dataClassification` | badge |

**Agent:**

| Column | Field | Render |
|--------|-------|--------|
| Time | `timestamp` | formatted datetime |
| Target Host | `targetHost` | text |
| Device | `deviceHostname` | fallback to deviceId prefix |
| User | `userDisplayName` | fallback chain |
| Process | `sourceProcess` | text |
| Action | `action` | text |
| Status | `statusCode` | colored badge |
| Latency | `latencyMs` | "{ms} ms" |
| Hook | `hookDecision` | text |
| Classification | `dataClassification` | badge |

**Fallback chain**: resolved name → ID prefix (first 8 chars + "...") → "-"

### 3e. Filter Panel

The filter panel adapts based on the active source sub-tab:

| Filter | All | VK | Proxy | Agent |
|--------|-----|-----|-------|-------|
| Time range | Y | Y | Y | Y |
| Provider | Y | Y | - | - |
| Model | Y | Y | - | - |
| User | Y | Y | - | Y |
| Organization | Y | Y | - | - |
| Project | Y | Y | - | - |
| Virtual Key | Y | Y | - | - |
| Department | Y | Y | - | - |
| Device | Y | - | - | Y |
| Process | - | - | - | Y |
| Hook decision | Y | Y | Y | Y |
| Response hook | Y | Y | - | - |
| Status code/range | Y | Y | Y | Y |
| Cache hit | - | Y | - | - |
| Target host | Y | - | Y | Y |
| Bump status | - | - | Y | - |
| Classification | Y | - | Y | Y |

### 3f. Drawer Refactor (`trafficAuditDrawer.tsx`)

The detail drawer shows **all non-null fields grouped by section**. Sections with zero non-null fields are hidden entirely.

**Sections:**
1. **Basic** — id, source (badge), timestamp, createdAt
2. **Request** — method, path, targetHost, sourceIp, statusCode, latencyMs
3. **Identity** — userDisplayName (+ userId), organizationName (+ orgId), projectName (+ projectId), virtualKeySlug (+ virtualKeyId), credentialName (+ credentialId), deviceHostname (+ deviceId), subjectId, department
4. **AI / Provider** — provider, modelUsed, promptTokens, completionTokens, totalTokens, estimatedCostUsd, cacheHit
5. **Routing** — routedProvider, routedModel, routingRuleName (+ routingRuleId), routingTrace (formatted JSON)
6. **Compliance** — hookDecision, hookReason, hookReasonCode, responseHookDecision, dataClassification, bumpStatus, hooksPipeline (formatted JSON)
7. **Agent** — sourceProcess, action
8. **Details** — details (formatted JSON)

For fields with both resolved name and ID, show: `"Engineering" (org-abc12345)` — name first, ID in parentheses/muted.

### 3g. Pages to Delete

- `pages/audit/UnifiedAuditPage.tsx` — functionality absorbed into `/traffic`
- Route entry `audit-unified` in `shellRouteConfig.tsx`
- Lazy page entry `LazyUnifiedAuditPage` in `lazyPages.tsx`

### 3h. Pages to Keep Unchanged

- `pages/audit/AuditLogPage.tsx` — admin audit, uses `admin-audit-logs` API
- `pages/traffic/AnalyticsTab.tsx` — analytics charts, unchanged
- `pages/traffic/TrafficAnalyticsPage.tsx` — outer tab container, minor update to integrate source sub-tabs into Live Traffic tab

---

## 4. Seed Data Changes

### 4a. Promote Fields to Top Level

Current seed puts several fields inside `details` JSON. Move them to top-level `traffic_event` columns:

**VK events**: `virtual_key_id`, `project_id`, `model_used`, `prompt_tokens`, `completion_tokens`, `total_tokens`, `estimated_cost_usd`, `cache_hit`, `credential_id`, `routed_provider`, `routed_model`, `routing_rule_id`

**Proxy events**: `bump_status` (currently in `details.bumpStatus`)

**Agent events**: `source_process`, `action` (currently in `details.sourceProcess`, `details.action`), `bump_status`

### 4b. Ensure JOIN Coverage

Each source type must have records where joinable FK fields reference real seeded entities:

| FK Field | Source | Must reference |
|----------|--------|---------------|
| `organization_id` | vk | Seeded Organization IDs |
| `project_id` | vk | Seeded Project IDs |
| `virtual_key_id` | vk | Seeded VirtualKey IDs |
| `credential_id` | vk | Seeded Credential IDs |
| `user_id` | vk, agent | Seeded NexusUser IDs |
| `device_id` | agent | Seeded AgentDevice IDs |
| `routing_rule_id` | vk | Seeded RoutingRule IDs |

Also ensure some records have these fields as NULL to demonstrate the "-" fallback rendering.

### 4c. Volume

- VK: 30 events (as-is, but with fields promoted)
- Proxy: 20 events (as-is, but with bump_status promoted)
- Agent: 20 events (as-is, but with source_process/action promoted)
- Total: 70 data-plane events

Admin and device-lifecycle events remain in the seed (unchanged) — they stay in `traffic_event` but are excluded from the traffic page query by the default `source IN ('vk','proxy','agent')` filter.

---

## 5. Test Updates

### 5a. MSW Handlers (`test/msw-handlers.ts`)

- Update mock endpoints: `/api/admin/audit/storage` → `/api/admin/traffic/storage`
- Update mock endpoints: `/api/admin/audit-logs` → `/api/admin/traffic`
- Remove unified audit mock if present

### 5b. Existing Tests

- `TrafficAnalyticsPage.test.tsx` — update API mock paths

---

## 6. Files Changed Summary

### Backend (Go)

| File | Action |
|------|--------|
| `internal/store/audit_log.go` | Rename to `traffic_event.go`, refactor structs/queries |
| `internal/store/misc_queries.go` | Remove `GetUnifiedAudit`, `UnifiedAuditRow` |
| `internal/handler/admin_audit.go` | Rename to `admin_traffic.go`, refactor routes/handlers |
| `internal/handler/admin_routes.go` | Update registration call |
| `internal/handler/admin_traffic_stream.go` | Extend source param support |

### Frontend (TypeScript)

| File | Action |
|------|--------|
| `api/types.ts` | Rename `AuditLogEntry` → `TrafficEvent`, add fields |
| `api/services/system.ts` | Rename API methods, update paths |
| `api/services/unified-audit.ts` | **Delete** |
| `pages/traffic/TrafficAnalyticsPage.tsx` | Update Live Traffic tab to include source sub-tabs |
| `pages/traffic/TrafficTab.tsx` | Refactor to accept `source` + `columns` config |
| `pages/traffic/TrafficPage.tsx` | Remove (if unused) or refactor |
| `pages/traffic/liveTrafficFilters.ts` | Add source, targetHost, deviceId, sourceProcess, bumpStatus filters |
| `pages/traffic/LiveTrafficFilterPanel.tsx` | Adapt filters per source tab |
| `pages/traffic/LiveTrafficBasicFilters.tsx` | Add new filter controls |
| `pages/traffic/LiveTrafficAdvancedFilters.tsx` | Add new filter controls |
| `pages/traffic/LiveTrafficActiveFiltersBar.tsx` | Update chip labels for new filters |
| `pages/traffic/trafficAuditDrawer.tsx` | Refactor to grouped sections, hide null fields |
| `pages/audit/UnifiedAuditPage.tsx` | **Delete** |
| `routes/shellRouteConfig.tsx` | Remove `audit-unified` route |
| `routes/lazyPages.tsx` | Remove `LazyUnifiedAuditPage` |
| `test/msw-handlers.ts` | Update mock endpoint paths |
| `pages/traffic/TrafficAnalyticsPage.test.tsx` | Update API mock paths |

### Seed

| File | Action |
|------|--------|
| `tools/db-migrate/seed/seed.ts` | Promote fields from details to top-level columns |
| `tools/db-migrate/seed/seed-matrix-audit.ts` | **Delete** (already unused, import commented out) |
