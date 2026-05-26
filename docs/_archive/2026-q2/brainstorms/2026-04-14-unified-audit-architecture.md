# Unified Audit Architecture — Design Spec

**Status: IMPLEMENTED** (2026-04-14)

## Goal

Consolidate 4 fragmented event/log tables (`AuditLog`, `audit_event`, `matrix_audit_event`, `AgentAuditEvent`) into a single `traffic_event` table. Keep `AdminAuditLog` separate. Fix all broken read paths (Traffic page, Analytics, VK access log). Ensure all writers (AI Gateway, Compliance Proxy, Agent) and all readers (Control Plane UI, SIEM, Alerting, Analytics) use the unified table.

## Architecture

Two tables for all audit/event data:
- **`traffic_event`** — all data-plane flow events (VK requests, proxy intercepts, agent traffic). Unified schema with `source` discriminator + first-class columns for all queryable fields. JSONB `details` for source-specific extras.
- **`AdminAuditLog`** — control-plane admin operations (unchanged).

`MetricRollup`, `ProviderHealth`, `ConfigVersion` are not log tables — they remain unchanged.

---

## 1. traffic_event Table Schema

### Common columns (all sources share)

| Column | Type | Description |
|--------|------|-------------|
| `id` | text PK | UUID |
| `source` | text NOT NULL | `"vk"`, `"proxy"`, `"agent"`, `"admin"`, `"device-lifecycle"` |
| `timestamp` | timestamptz NOT NULL | Event time |
| `source_ip` | text | Client IP |
| `target_host` | text | Destination host |
| `method` | text | HTTP method |
| `path` | text | Request path |
| `status_code` | int | HTTP status |
| `latency_ms` | int | Response latency |
| `created_at` | timestamptz DEFAULT NOW() | Insert time |

### Identity columns

| Column | Type | Description |
|--------|------|-------------|
| `user_id` | text | VK slug or user identifier |
| `organization_id` | text | Organization UUID |
| `project_id` | text | Project UUID |
| `department` | text | Department name |
| `virtual_key_id` | text | Virtual Key UUID |
| `credential_id` | text | Credential UUID used |
| `device_id` | text | Agent device UUID |
| `subject_id` | text | OS user / DSAR subject |

### AI/Provider columns (source=vk primarily)

| Column | Type | Description |
|--------|------|-------------|
| `provider` | text | Provider name (openai, anthropic, etc.) |
| `model_used` | text | Model identifier |
| `prompt_tokens` | int | Input tokens |
| `completion_tokens` | int | Output tokens |
| `total_tokens` | int | Total tokens |
| `estimated_cost_usd` | numeric(12,6) | Estimated cost |
| `cache_hit` | boolean | Response cache hit |
| `routed_provider` | text | Provider after routing |
| `routed_model` | text | Model after routing |
| `routing_rule_id` | text | Routing rule that matched |

### Compliance columns (all sources)

| Column | Type | Description |
|--------|------|-------------|
| `hook_decision` | text | Request hook decision |
| `hook_reason` | text | Hook reason |
| `hook_reason_code` | text | Hook reason code |
| `response_hook_decision` | text | Response hook decision |
| `data_classification` | text | Data sensitivity level |
| `bump_status` | text | TLS bump status (proxy/agent) |

### Agent-specific columns

| Column | Type | Description |
|--------|------|-------------|
| `source_process` | text | Process that made the request |
| `action` | text | Agent action (allow/block/log) |

### JSONB columns (non-indexed details)

| Column | Type | Description |
|--------|------|-------------|
| `hooks_pipeline` | jsonb | Full hook execution chain |
| `routing_trace` | jsonb | Routing decision trace |
| `details` | jsonb | Source-specific extras (replaces `sourceDetails`, `metadata`, `routingDecision`, `qualitySignals`, `complianceFlags`) |

### Indexes

```sql
CREATE INDEX idx_traffic_event_source_ts ON traffic_event (source, timestamp);
CREATE INDEX idx_traffic_event_ts ON traffic_event (timestamp);
CREATE INDEX idx_traffic_event_vk ON traffic_event (virtual_key_id, timestamp) WHERE virtual_key_id IS NOT NULL;
CREATE INDEX idx_traffic_event_org ON traffic_event (organization_id, timestamp) WHERE organization_id IS NOT NULL;
CREATE INDEX idx_traffic_event_project ON traffic_event (project_id, timestamp) WHERE project_id IS NOT NULL;
CREATE INDEX idx_traffic_event_device ON traffic_event (device_id, timestamp) WHERE device_id IS NOT NULL;
CREATE INDEX idx_traffic_event_provider ON traffic_event (provider, timestamp) WHERE provider IS NOT NULL;
CREATE INDEX idx_traffic_event_hook ON traffic_event (hook_decision, timestamp);
CREATE INDEX idx_traffic_event_subject ON traffic_event (subject_id) WHERE subject_id IS NOT NULL;
CREATE INDEX idx_traffic_event_target ON traffic_event (target_host, timestamp);
```

### Column naming convention

All columns use **snake_case** (PostgreSQL convention), not camelCase. This differs from the old `AuditLog` table but matches `audit_event` and `matrix_audit_event`.

---

## 2. Migration Plan — Tables

| Old Table | Action |
|-----------|--------|
| `AuditLog` | DROP (zombie, never written by runtime) |
| `audit_event` | RENAME to `traffic_event`, ADD missing columns via ALTER |
| `matrix_audit_event` | DROP (absorbed into traffic_event) |
| `AgentAuditEvent` | DROP (absorbed into traffic_event) |
| `AdminAuditLog` | KEEP unchanged |

The migration renames `audit_event` → `traffic_event` and adds columns. This preserves existing data in `audit_event`.

---

## 3. Writers — Who Changes What

### AI Gateway (`packages/ai-gateway/internal/observability/audit/`)

Currently writes to `audit_event` with VK-specific fields in `sourceDetails` JSONB.

**Change:** Write VK fields as first-class columns instead of JSONB:
- `virtual_key_id`, `credential_id`, `project_id`, `department`, `provider`, `model_used`
- `prompt_tokens`, `completion_tokens`, `total_tokens`, `estimated_cost_usd`
- `cache_hit`, `routed_provider`, `routed_model`, `routing_rule_id`
- `response_hook_decision`
- Keep `routing_trace`, `hooks_pipeline`, `details` as JSONB for non-indexed extras

### Compliance Proxy (`packages/compliance-proxy/internal/audit/`)

Currently writes to BOTH `audit_event` AND `matrix_audit_event`.

**Change:** Write only to `traffic_event` (source=proxy). Map `matrix_audit_event` columns:
- `transaction_id` → `details.transactionId`
- `connection_id` → `details.connectionId`
- `traffic_source` → `source` (already "proxy")
- `bump_status` → `bump_status` (first-class column)
- `ingress_type` → `details.ingressType`
- `user_agent` → `details.userAgent`

### Agent (`packages/agent/`, `packages/control-plane/internal/store/agent_audit_event.go`)

Currently writes to BOTH `audit_event` AND `AgentAuditEvent`.

**Change:** Write only to `traffic_event` (source=agent). Map `AgentAuditEvent` columns:
- `sourceProcess` → `source_process`
- `sourceUser` → `subject_id`
- `destHost` → `target_host`
- `destIp` → `details.destIp`
- `destPort` → `details.destPort`
- `action` → `action`
- `policyRuleId` → `details.policyRuleId`
- `bumpStatus` → `bump_status`
- `bytesIn`/`bytesOut` → `details.bytesIn`/`details.bytesOut`
- `duration` → `latency_ms`

### Control Plane audit writer (`packages/control-plane/internal/audit/`)

Currently writes to `audit_event` (source=admin, device-lifecycle).

**Change:** Write to `traffic_event` (same table, renamed). No schema change needed — existing columns cover it.

---

## 4. Readers — Who Changes What

### Traffic Page + VK Access Log

Currently: `ListAuditLogs` queries `AuditLog` table.

**Change:** Query `traffic_event` WHERE source='vk'. Use first-class columns for filtering (virtual_key_id, provider, model_used, etc.). LEFT JOIN VirtualKey, Organization, Project for names (same as current enrichment).

### Analytics (summary, by-provider, by-user, usage, cost)

Currently: `GetAnalyticsSummary`, `GetAnalyticsByProvider`, `GetAnalyticsGroupBy` query `AuditLog`.

**Change:** Query `traffic_event` WHERE source='vk'. Same aggregation logic, different table name + snake_case column names.

### Compliance Coverage + Hook Health

Currently: `GetComplianceCoverage` queries `matrix_audit_event`, `GetHookHealth` queries `AuditLog`.

**Change:** Both query `traffic_event`. Coverage: WHERE source IN ('proxy','agent'). Hook health: WHERE hook_decision IS NOT NULL.

### SIEM Bridge

Currently: queries `audit_event`.

**Change:** Query `traffic_event` (renamed, no functional change).

### Alerting

Currently: queries `audit_event`.

**Change:** Query `traffic_event` (renamed, no functional change).

### Agent Events Page

Currently: queries `audit_event` WHERE source='agent'.

**Change:** Query `traffic_event` WHERE source='agent' (renamed, no functional change).

### Proxy Audit Tab

Currently: queries `matrix_audit_event`.

**Change:** Query `traffic_event` WHERE source='proxy'.

### DSAR (data subject access requests)

Currently: queries both `AuditLog` and `matrix_audit_event` for subject data.

**Change:** Query `traffic_event` WHERE subject_id = $1.

### Retention/Purge Jobs

Currently: DELETE from `AuditLog`, `matrix_audit_event`, `AgentAuditEvent` separately.

**Change:** DELETE from `traffic_event` WHERE timestamp < $1.

---

## 5. Seed Data

Update `tools/db-migrate/seed/seed.ts`:
- Remove AuditLog seed (section that creates 200 fake audit logs)
- Write seed data directly to `traffic_event` with proper source discriminators
- Remove AgentAuditEvent seed (write to traffic_event source=agent)
- Remove matrix_audit_event seed (write to traffic_event source=proxy)

---

## 6. Front-End Changes

### API Types (`packages/control-plane-ui/src/api/types.ts`)

`AuditLogEntry` interface stays mostly the same but field names change to match snake_case → camelCase mapping from the API:

```typescript
export interface TrafficEvent {
  id: string;
  source: string; // "vk" | "proxy" | "agent" | "admin"
  timestamp: string;
  sourceIp?: string;
  targetHost?: string;
  method?: string;
  path?: string;
  statusCode?: number;
  latencyMs?: number;
  // Identity
  userId?: string;
  organizationId?: string;
  organizationName?: string; // enriched by API
  projectId?: string;
  projectName?: string; // enriched by API
  virtualKeyId?: string;
  virtualKeyName?: string; // enriched by API
  department?: string;
  deviceId?: string;
  // AI/Provider
  provider?: string;
  modelUsed?: string;
  promptTokens?: number;
  completionTokens?: number;
  totalTokens?: number;
  estimatedCostUsd?: number;
  cacheHit?: boolean;
  routedProvider?: string;
  routedModel?: string;
  // Compliance
  hookDecision?: string;
  dataClassification?: string;
  bumpStatus?: string;
  // Agent
  sourceProcess?: string;
  action?: string;
  // JSONB
  hooksPipeline?: unknown;
  routingTrace?: unknown;
  details?: unknown;
}
```

### Pages that need updates

- Traffic page: change `AuditLogEntry` → `TrafficEvent`, update column references
- VK detail access log: filter by `virtualKeyId`
- Analytics: API already returns aggregated data, minimal UI changes
- Proxy audit tab: change from `matrix_audit_event` endpoint to `traffic_event` filtered
- Agent events: already using `audit_event`, just type rename

---

## 7. File Changes Summary

### Migration
- Create: `tools/db-migrate/migrations/20260414150000_unified_traffic_event/migration.sql`

### AI Gateway
- Modify: `packages/ai-gateway/internal/observability/audit/audit.go` — INSERT into `traffic_event` with all first-class columns

### Compliance Proxy
- Modify: `packages/compliance-proxy/internal/audit/sql.go` — INSERT into `traffic_event`
- Modify: `packages/compliance-proxy/internal/audit/writer.go` — remove matrix_audit_event writes
- Modify: `packages/compliance-proxy/internal/audit/retention.go` — purge from `traffic_event`

### Control Plane Store
- Modify: `packages/control-plane/internal/store/audit_log.go` — query `traffic_event` instead of `AuditLog`
- Modify: `packages/control-plane/internal/store/analytics.go` — query `traffic_event`
- Modify: `packages/control-plane/internal/store/misc_queries.go` — compliance/hook queries on `traffic_event`
- Modify: `packages/control-plane/internal/store/dsar.go` — query `traffic_event`
- Modify: `packages/control-plane/internal/store/cross_path_governance.go` — already uses `audit_event`
- Modify: `packages/control-plane/internal/store/agent_audit_event.go` — write to `traffic_event`
- Modify: `packages/control-plane/internal/siem/bridge.go` — query `traffic_event`
- Modify: `packages/control-plane/internal/alerting/evaluator.go` — query `traffic_event`
- Modify: `packages/control-plane/internal/jobs/scheduler.go` — purge `traffic_event`
- Modify: `packages/control-plane/internal/audit/writer.go` — INSERT into `traffic_event`

### Control Plane Handlers
- Modify: `packages/control-plane/internal/handler/admin_audit.go` — use traffic_event queries
- Modify: `packages/control-plane/internal/handler/admin_proxy.go` — proxy audit from traffic_event
- Modify: `packages/control-plane/internal/handler/admin_analytics.go` — analytics from traffic_event

### Seed
- Modify: `tools/db-migrate/seed/seed.ts` — write to traffic_event

### Prisma Schema
- Modify: `tools/db-migrate/schema.prisma` — rename model, add columns, drop old models

### Frontend
- Modify: `packages/control-plane-ui/src/api/types.ts` — TrafficEvent type
- Modify: Traffic page, VK detail, Proxy audit tab, Agent events (column references)

### Delete (after migration)
- `AuditLog` model from schema
- `AgentAuditEvent` model from schema
- `matrix_audit_event` model from schema
