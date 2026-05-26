# D3d: Fleet Dashboard UI Design

**Date:** 2026-04-13
**Status:** Approved
**Scope:** Frontend React pages for fleet management
**Parent:** `docs/superpowers/specs/2026-04-13-d3-handoff.md`
**Backend:** D3a + D3c complete (sysinfo + fleet APIs)

---

## Constraints

- **Follow existing UI conventions exactly** — no new patterns, no design innovation
- Reuse existing components: DataTable, Card, PageHeader, Badge, Tabs, ListFilterToolbar, ListPagination, Stack, Breadcrumb
- Follow existing page patterns: DeviceListPage for lists, OrganizationDetail for detail pages
- CSS Modules + design tokens (no Tailwind, no inline styles)
- i18n via useTranslation() for all user-visible strings (English only)
- API calls via service modules + useApi/useMutation hooks
- Permissions via usePermission()

---

## API Service

New file: `src/api/services/fleetApi.ts`

```typescript
export const fleetApi = {
  listAgentUsers: (params?) => api.get('/api/admin/agent-users', params),
  getAgentUser: (id) => api.get(`/api/admin/agent-users/${id}`),
  getUserDevices: (id, params?) => api.get(`/api/admin/agent-users/${id}/devices`, params),
  getUserAudit: (id, params?) => api.get(`/api/admin/agent-users/${id}/audit`, params),
  suspendUser: (id) => api.post(`/api/admin/agent-users/${id}/suspend`),
  activateUser: (id) => api.post(`/api/admin/agent-users/${id}/activate`),
  getDeviceAudit: (id, params?) => api.get(`/api/admin/agent-devices/${id}/audit`, params),
  getDeviceConfig: (id) => api.get(`/api/admin/agent-devices/${id}/config`),
  getDeviceTimeline: (id) => api.get(`/api/admin/agent-devices/${id}/timeline`),
  reassignDevice: (id, userId) => api.post(`/api/admin/agent-devices/${id}/reassign`, { userId }),
};
```

Reuses existing `devicesApi` for device list, detail, fleet health.

---

## Pages

### 1. Fleet Overview (`/fleet`)

**Pattern:** Follows existing dashboard pages (overview, fleet-analytics).

- **Stat cards row:** 4 cards showing total/active/offline/revoked counts from `devicesApi.getFleetHealth()`
- **Charts row:** OS distribution (PieChart), agent version distribution (BarChart) — data derived from `devicesApi.list()` aggregated client-side
- **Recent offline:** Small DataTable of recently offline devices (status=OFFLINE, sorted by lastHeartbeat DESC)
- Uses Recharts with existing chartColors helpers (getSeriesColors, getPieColors, getTooltipStyle, getGridStroke)
- Uses useTheme().resolvedMode for chart theming

### 2. User List (`/fleet/users`)

**Pattern:** Follows DeviceListPage exactly.

- **Columns:** displayName, osUsername, status (Badge), device count, createdAt
- **Filters:** search input (q param), enabled status dropdown
- **Pagination:** ListPagination with server-side offset/limit
- **Row click:** navigate to `/fleet/users/:id`
- Uses useApi with fleetApi.listAgentUsers
- Uses useDebouncedValue for search

### 3. User Detail (`/fleet/users/:id`)

**Pattern:** Follows OrganizationDetail / ProviderDetailPage with tabs.

- **Header:** Breadcrumb (Fleet > Users > {displayName}) + PageHeader with Suspend/Activate action button
- **Info section:** Card with key-value pairs: displayName, osUsername, osDomain, email, status, createdAt
- **Tabs:** Devices | Audit
  - **Devices tab:** DataTable showing user's assigned devices (fleetApi.getUserDevices). Columns: hostname, os, status (Badge), agentVersion, lastHeartbeat, assignedAt. Row click navigates to `/fleet/devices/:id`
  - **Audit tab:** DataTable of audit events (fleetApi.getUserAudit). Columns: timestamp, source, targetHost, hookDecision. Optional time range filter (start/end query params)
- Suspend/Activate uses useMutation with confirmation AlertDialog

### 4. Device Detail (`/fleet/devices/:id`)

**Pattern:** Follows ProviderDetailPage with tabs.

- **Header:** Breadcrumb (Fleet > Devices > {hostname}) + PageHeader with status Badge
- **Summary card:** hostname, OS, osVersion, agentVersion, status, lastHeartbeat, enrolledAt
- **Tabs:** Sysinfo | Timeline | Audit | Config
  - **Sysinfo tab:** Renders parsed sysinfo JSON as a Card with key-value rows: machineId, osName, osVersion, cpuModel, cpuCores, totalMemMB, serialNumber, modelName. Network interfaces as a small sub-table (name, MAC, IPs). Shows "No sysinfo collected" if null.
  - **Timeline tab:** DataTable of assignment history (fleetApi.getDeviceTimeline). Columns: userDisplayName, userOsUsername, source, assignedAt, releasedAt (or "Current" badge). Reassign button opens dialog with user selector.
  - **Audit tab:** DataTable of device audit events (fleetApi.getDeviceAudit). Same pattern as user audit tab.
  - **Config tab:** Read-only JSON display of effective config from fleetApi.getDeviceConfig. Uses existing JsonEditor component in read-only mode, or simple `<pre>` with JSON.stringify if simpler.

### 5. Device Auth Settings (`/settings/device-auth`)

**Pattern:** Follows existing settings pages (SSO settings tab).

- Info card showing current auth mode: "mTLS Only (Default)"
- Note: "Enterprise Login mode will be available in a future release" 
- This is a placeholder — D3b enterprise login is not yet implemented
- Read-only, no form controls

---

## Routing

Add to `shellRouteConfig.tsx`:

```typescript
// Fleet section (new nav section)
{ path: 'fleet', LazyPage: lazy(() => import('@/pages/fleet/FleetOverviewPage')),
  nav: { sectionKey: 'fleet', labelKey: 'nav:fleet.overview', to: '/fleet', order: 0 } },
{ path: 'fleet/users', LazyPage: lazy(() => import('@/pages/fleet/FleetUserListPage')),
  nav: { sectionKey: 'fleet', labelKey: 'nav:fleet.users', to: '/fleet/users', order: 1 } },
{ path: 'fleet/users/:id', LazyPage: lazy(() => import('@/pages/fleet/FleetUserDetailPage')) },
{ path: 'fleet/devices/:id', LazyPage: lazy(() => import('@/pages/fleet/FleetDeviceDetailPage')) },

// Device Auth under existing system section
{ path: 'settings/device-auth', LazyPage: lazy(() => import('@/pages/settings/DeviceAuthSettingsPage')),
  nav: { sectionKey: 'system', labelKey: 'nav:system.deviceAuth', to: '/settings/device-auth', order: 50 } },
```

---

## i18n

Add keys to `src/i18n/en/pages.json` under a `fleet` section. English only per repo rules.

Key groups: fleet.overview.*, fleet.users.*, fleet.userDetail.*, fleet.deviceDetail.*, fleet.deviceAuth.*

---

## File Structure

```
src/pages/fleet/
  FleetOverviewPage.tsx
  FleetOverviewPage.module.css
  FleetUserListPage.tsx
  FleetUserListPage.module.css
  FleetUserDetailPage.tsx
  FleetUserDetailPage.module.css
  FleetDeviceDetailPage.tsx
  FleetDeviceDetailPage.module.css
src/pages/settings/
  DeviceAuthSettingsPage.tsx
  DeviceAuthSettingsPage.module.css
src/api/services/fleetApi.ts
```

---

## Out of Scope

- D3b enterprise login (no auth mode toggle API exists)
- Bulk operations on user list (future enhancement)
- Real-time SSE updates on fleet dashboard
- Export functionality
