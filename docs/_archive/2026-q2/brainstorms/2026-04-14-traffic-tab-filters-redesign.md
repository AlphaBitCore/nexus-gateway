# Traffic Page Tab Filters Redesign

## Problem

The /traffic page has 4 tabs (All, VK, Proxy, Agent) that share a single set of filter fields. Different source types have fundamentally different data models, so users see irrelevant filters on every tab (e.g., Provider/Model on Proxy tab, Device/BumpStatus on VK tab). Additionally, high-frequency filters like Organization/Project are buried in the Advanced section for VK traffic.

## Design

### Architecture

- **Per-tab filter configuration** — each tab declares which filter fields appear in Basic vs Advanced sections
- **Shared filter state type** (`LiveTrafficFiltersState`) remains as the API-layer superset — no backend changes
- **Dynamic rendering** — `LiveTrafficBasicFilters` and `LiveTrafficAdvancedFilters` accept a filter config and render only the relevant fields
- **Tab switch behavior** — reset all filters except time range when switching tabs
- **Columns** — already per-tab via `getColumnsForSource()`, no changes needed

### Tab 1: All Traffic (simplified)

**Purpose:** Cross-source overview. Minimal filters since data models differ.

**Basic filters:**

| Row | Fields |
|-----|--------|
| Row 1 | Time range (preset buttons: 1h, 24h, 7d + from/to inputs) |
| Row 2 | User (SearchableCombobox) + Hook Decision (select) |

**Advanced filters:** None.

**Columns:** timestamp, source (badge), targetHost, method, statusCode, latencyMs, hookDecision, userDisplayName

### Tab 2: VK Traffic

**Purpose:** AI Gateway virtual key traffic analysis. Core enterprise use case.

**Basic filters:**

| Row | Fields | Behavior |
|-----|--------|----------|
| Row 1 | Time range (preset + from/to) | — |
| Row 2 | Organization → Project → Virtual Key | Cascading: org filters project list, project filters VK list |
| Row 3 | Provider → Model | Cascading: provider filters model list |

**Advanced filters:**

| Group | Fields |
|-------|--------|
| HTTP/Cache | Status Class (select: 2xx/4xx/5xx), Status Code (input), Cache Hit (select) |
| Hooks | Request Hook Decision (select), Response Hook Decision (select) |
| Correlation | Gateway Request ID (text input, monospace) |

**Columns:** timestamp, providerName, modelName, userDisplayName, organizationName, projectName, virtualKeyName, statusCode, latencyMs, totalTokens, estimatedCostUsd, hookDecision, cacheHit

### Tab 3: Proxy

**Purpose:** Compliance proxy CONNECT tunnel monitoring.

**Basic filters:**

| Row | Fields |
|-----|--------|
| Row 1 | Time range (preset + from/to) |
| Row 2 | Target Host (text input) + Bump Status (select) + Hook Decision (select) |

**Advanced filters:**

| Group | Fields |
|-------|--------|
| Identity | Source IP (text input), Subject (text input) |
| Compliance | Data Classification (select), Response Hook Decision (select) |
| HTTP | Status Class (select), Status Code (input) |

**Columns:** timestamp, targetHost, sourceIp, method, statusCode, latencyMs, bumpStatus, hookDecision, dataClassification

### Tab 4: Agent

**Purpose:** Desktop agent network activity monitoring.

**Basic filters:**

| Row | Fields |
|-----|--------|
| Row 1 | Time range (preset + from/to) |
| Row 2 | Device (SearchableCombobox) + Source Process (text input) + Action (select) |

**Advanced filters:**

| Group | Fields |
|-------|--------|
| Network | Target Host (text input) |
| Compliance | Hook Decision (select), Data Classification (select), Bump Status (select) |
| Identity | Subject (text input) |

**Columns:** timestamp, targetHost, deviceHostname, userDisplayName, sourceProcess, action, statusCode, latencyMs, hookDecision, dataClassification

## Implementation approach

### Filter config type

Define a per-tab configuration that declares which fields appear where:

```typescript
interface TabFilterConfig {
  basic: FilterFieldConfig[];    // fields in basic section
  advanced: FilterGroupConfig[]; // groups in advanced section
}

interface FilterFieldConfig {
  field: keyof LiveTrafficFiltersState;
  type: 'text' | 'select' | 'combobox' | 'datetime';
  label: string;
  // for combobox: data source
  // for select: options
  // for cascading: dependsOn field
}

interface FilterGroupConfig {
  label: string;
  fields: FilterFieldConfig[];
}
```

### Files to modify

| File | Change |
|------|--------|
| `liveTrafficFilters.ts` | Add per-tab filter configs (TAB_FILTER_CONFIGS) |
| `LiveTrafficBasicFilters.tsx` | Accept config prop, render only configured fields |
| `LiveTrafficAdvancedFilters.tsx` | Accept config prop, render only configured groups |
| `LiveTrafficFilterPanel.tsx` | Pass source-based config to children |
| `TrafficTab.tsx` | Pass source to filter panel; reset filters on tab switch (keep time) |
| `TrafficAnalyticsPage.tsx` | Handle tab switch filter reset |

### What does NOT change

- `LiveTrafficFiltersState` type — remains the superset
- `buildTrafficAuditLogQueryParams()` — unchanged, still sends non-empty fields
- Backend API — no changes
- Per-tab column definitions — already correct
- Detail drawer — already correct
- Active filter chips bar — still reads from state, works as-is
