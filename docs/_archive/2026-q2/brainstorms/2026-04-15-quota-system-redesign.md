# Quota System Redesign — Design Specification

**Date:** 2026-04-15
**Status:** Approved (brainstorm complete)

## 1. Overview

Redesign the quota system from a flat per-entity model to a **policy-based, hierarchical budget control** system for enterprise AI usage management. The new design introduces:

- **QuotaPolicy**: rule-based templates that apply limits by condition (department, VK type)
- **QuotaOverride**: per-entity exceptions that override policy defaults
- **QuotaAlert**: threshold-based alerts when usage approaches limits
- **VK dual-type model**: `personal` (user-owned) vs `application` (project-owned) with separate management UIs and approval workflows
- **Hierarchical runtime enforcement**: cascading checks up the entity hierarchy
- **Analytics from rollup**: reuse existing metric rollup infrastructure instead of a separate usage table

## 2. Key Decisions (from brainstorm)

| # | Decision | Choice | Rationale |
|---|----------|--------|-----------|
| 1 | Hierarchy model | Independent limits + parent ceiling check | Flexible overcommit at lower levels; parent ceiling guarantees safety; simpler than budget envelope |
| 2 | VK types | `personal` (user-owned) / `application` (project-owned) | Personal cost → user; application cost → project. Different management flows, approval, and expiration |
| 3 | Personal VK project association | No project required | Users work across projects with one VK; can't auto-attribute cost to projects; user quota is the control lever |
| 4 | Quota data model | Policy + Override (not per-entity records) | 500 users = 1 policy, not 500 records; new joiners auto-covered; overrides only for exceptions |
| 5 | Policy matching | Deterministic fields (departmentId, vkType) | B-tree indexable, type-safe, schema-enforced; enterprise quota dimensions are finite and known |
| 6 | Enforcement modes | reject / downgrade / notify-and-proceed / track-only | track-only replaces "unlimited" — "monitor without blocking" is a common enterprise onboarding pattern |
| 7 | Threshold alerts | 80% / 90% configurable per policy | Proactive warning before hard limit hit |
| 8 | Usage data source | Existing metric rollup tables | Eliminates redundant QuotaUsage table; single source of truth; multi-granularity for free |
| 9 | Runtime hot path | Redis cache for current-period usage | Rollup has 60s+ delay; Redis INCR gives real-time accuracy for enforcement |
| 10 | Alert checking | Control Plane scheduler job (60s) | Decouples alerting from AI-Gateway hot path; cleaner separation of concerns |
| 11 | Application VK approval | Simple apply → approve/reject flow | IAM-gated; keep it minimal; no multi-step workflow engine |
| 12 | Application VK expiration | Mandatory, max 3 months, renewable | Security best practice: forced credential rotation for service keys |
| 13 | Personal VK management | User self-service in Settings page | No admin overhead; user controls their own tools |
| 14 | User-level quota for Application VK | Not needed | Application VK cost belongs to the project, not end-users of the app |

## 3. Scope

### In Scope

- Database schema changes: QuotaPolicy, QuotaOverride, QuotaAlert tables; VirtualKey type/status/expiry fields
- Deprecation of existing Quota table (migration path)
- Policy CRUD API + Override CRUD API + Alert API
- Runtime enforcement engine rewrite in AI-Gateway (policy resolution + hierarchical check)
- Redis-based usage cache for hot-path enforcement
- Alert scheduler job in Control Plane
- VK expiry scheduler job in Control Plane
- Application VK approval workflow API
- UI: Application VK management page (existing VK page, add approval flow)
- UI: Personal VK management in user Settings
- UI: Quota policy management page
- UI: Quota usage dashboard (leveraging rollup data)
- UI: Alert center

### Out of Scope

- Complex approval workflows (multi-step, delegation, escalation)
- External notification channels (Webhook, Slack, email) — future phase
- Cost chargeback / billing integration
- Cross-organization quota management (multi-tenant SaaS)
- Token-based enforcement (only cost-based in first phase; token limits stored but not enforced at runtime)

## 4. Entity Hierarchy & Check Chains

### 4.1 Hierarchy

```
Organization
  └── Department
        ├── User (personal VK owner)
        │     └── Personal VK
        └── Project
              └── Application VK
```

### 4.2 Runtime Check Chains

**Personal VK request:**
```
VK limit → User limit → Department limit → Organization limit
```

**Application VK request:**
```
VK limit → Project limit → Department limit → Organization limit
```

Any level exceeding its limit triggers that level's enforcement mode. The most restrictive action wins (reject > downgrade > notify-and-proceed > track-only).

### 4.3 Limit Resolution per Level

For each level in the chain:
1. Check for a **QuotaOverride** matching (targetType, targetId) → use its limits
2. If no override, find the **QuotaPolicy** matching (scope, departmentId, vkType) with highest priority → use its limits
3. If no policy matches → no limit at this level (pass through, parent levels still checked)

## 5. Data Model

### 5.1 QuotaPolicy

Defines budget rules that apply to classes of entities.

```sql
CREATE TABLE "QuotaPolicy" (
    id                TEXT PRIMARY KEY DEFAULT gen_random_uuid(),
    name              TEXT NOT NULL,               -- "Engineering personal user monthly limit"
    description       TEXT,                        -- Optional human-readable description

    -- What this policy governs
    scope             TEXT NOT NULL,               -- "user" | "vk" | "project" | "department" | "organization"

    -- Matching conditions (deterministic fields, NULL = match all)
    organizationId    TEXT,                        -- NULL = all orgs (for multi-org future)
    departmentId      TEXT,                        -- NULL = all departments
    vkType            TEXT,                        -- "personal" | "application" | NULL = both

    -- Limits
    periodType        TEXT NOT NULL,               -- "daily" | "weekly" | "monthly"
    costLimitUsd      DECIMAL(24,6),               -- NULL = no cost limit at this policy
    tokenLimit        BIGINT,                      -- NULL = no token limit (stored, not enforced in phase 1)

    -- Enforcement
    enforcementMode   TEXT NOT NULL DEFAULT 'reject',  -- "reject" | "downgrade" | "notify-and-proceed" | "track-only"
    alertThresholds   JSONB DEFAULT '[80, 90]',        -- Percentage thresholds for alerts

    -- Priority (higher number wins when multiple policies match)
    priority          INT NOT NULL DEFAULT 0,

    -- Status
    enabled           BOOLEAN NOT NULL DEFAULT true,

    -- Metadata
    createdBy         TEXT,
    createdAt         TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updatedAt         TIMESTAMP(3) NOT NULL
);

CREATE INDEX "QuotaPolicy_scope_idx" ON "QuotaPolicy" (scope);
CREATE INDEX "QuotaPolicy_departmentId_idx" ON "QuotaPolicy" ("departmentId");
CREATE INDEX "QuotaPolicy_enabled_idx" ON "QuotaPolicy" (enabled) WHERE enabled = true;
```

**Example policies:**

| name | scope | departmentId | vkType | periodType | costLimitUsd | enforcementMode |
|------|-------|-------------|--------|------------|-------------|-----------------|
| Eng dept personal user limit | user | dept-eng | personal | monthly | 300 | reject |
| Eng dept total budget | department | dept-eng | NULL | monthly | 20000 | reject |
| CSM AI project cap | project | NULL | NULL | monthly | 10000 | notify-and-proceed |
| Org-wide ceiling | organization | NULL | NULL | monthly | 50000 | reject |
| All application VKs default | vk | NULL | application | monthly | 5000 | downgrade |

### 5.2 QuotaOverride

Per-entity exceptions that override the matching policy.

```sql
CREATE TABLE "QuotaOverride" (
    id                TEXT PRIMARY KEY DEFAULT gen_random_uuid(),
    targetType        TEXT NOT NULL,               -- "user" | "vk" | "project" | "department" | "organization"
    targetId          TEXT NOT NULL,                -- Specific entity ID
    reason            TEXT,                         -- "AI lead needs higher limit" — audit trail

    -- Override values (NULL = inherit from policy)
    costLimitUsd      DECIMAL(24,6),
    tokenLimit        BIGINT,
    enforcementMode   TEXT,                        -- NULL = inherit from policy
    periodType        TEXT,                        -- NULL = inherit from policy

    -- Metadata
    createdBy         TEXT,
    createdAt         TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updatedAt         TIMESTAMP(3) NOT NULL,

    CONSTRAINT "QuotaOverride_target_uq" UNIQUE (targetType, targetId)
);

CREATE INDEX "QuotaOverride_targetType_idx" ON "QuotaOverride" ("targetType");
```

### 5.3 QuotaAlert

Records of threshold-crossing events.

```sql
CREATE TABLE "QuotaAlert" (
    id                TEXT PRIMARY KEY DEFAULT gen_random_uuid(),

    -- What triggered
    alertType         TEXT NOT NULL,               -- "quota-threshold" | "vk-expiring"
    targetType        TEXT NOT NULL,               -- "user" | "vk" | "project" | "department" | "organization"
    targetId          TEXT NOT NULL,
    targetName        TEXT,                        -- Denormalized for display

    -- Quota context (for quota-threshold alerts)
    policyId          TEXT,
    overrideId        TEXT,
    periodKey         TEXT,                        -- "2026-04" | "2026-W16" | "2026-04-15"
    thresholdPct      INT,                         -- 80 or 90
    currentUsagePct   FLOAT,                       -- Actual percentage, e.g. 83.5
    costLimitUsd      DECIMAL(24,6),
    currentCostUsd    DECIMAL(24,6),

    -- VK expiry context (for vk-expiring alerts)
    expiresAt         TIMESTAMP(3),

    -- Status
    status            TEXT NOT NULL DEFAULT 'active',  -- "active" | "acknowledged" | "resolved"
    acknowledgedBy    TEXT,
    acknowledgedAt    TIMESTAMP(3),

    createdAt         TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,

    -- Same target + period + threshold = one alert only
    -- For vk-expiring alerts: periodKey = expiresAt date string (e.g. "2026-07-15"), thresholdPct = 0
    CONSTRAINT "QuotaAlert_dedup_uq" UNIQUE (alertType, targetType, targetId, periodKey, thresholdPct)
);

CREATE INDEX "QuotaAlert_status_idx" ON "QuotaAlert" (status) WHERE status = 'active';
CREATE INDEX "QuotaAlert_createdAt_idx" ON "QuotaAlert" ("createdAt");
```

### 5.4 VirtualKey Changes

Add fields to the existing VirtualKey table:

```sql
ALTER TABLE "VirtualKey" ADD COLUMN
    "vkType"      TEXT NOT NULL DEFAULT 'personal',    -- "personal" | "application"
    "status"      TEXT NOT NULL DEFAULT 'active',      -- "pending" | "active" | "expired" | "rejected" | "revoked"
    "expiresAt"   TIMESTAMP(3),                        -- Required for application VKs, max 3 months from creation
    "approvedBy"  TEXT,                                -- userId of approver (application VKs)
    "approvedAt"  TIMESTAMP(3),                        -- When approved
    "rejectedBy"  TEXT,                                -- userId of rejector (if rejected)
    "rejectedAt"  TIMESTAMP(3),                        -- When rejected
    "rejectReason" TEXT;                               -- Why rejected

-- Application VKs must have a projectId
-- Enforced in application code: if vkType = 'application' then projectId IS NOT NULL

CREATE INDEX "VirtualKey_vkType_idx" ON "VirtualKey" ("vkType");
CREATE INDEX "VirtualKey_status_idx" ON "VirtualKey" ("status");
CREATE INDEX "VirtualKey_expiresAt_idx" ON "VirtualKey" ("expiresAt") WHERE "expiresAt" IS NOT NULL;
```

### 5.5 Deprecated Table

The existing `Quota` table is deprecated. Migration strategy:

1. Deploy new tables alongside existing `Quota` table
2. Migrate existing Quota records to QuotaPolicy + QuotaOverride equivalents
3. Switch AI-Gateway enforcement to new policy engine
4. Drop `Quota` table after verification period

## 6. API Design

### 6.1 Quota Policy CRUD

```
GET    /api/admin/quota-policies                    → List policies (filterable by scope, departmentId, enabled)
POST   /api/admin/quota-policies                    → Create policy
GET    /api/admin/quota-policies/:id                → Get policy
PUT    /api/admin/quota-policies/:id                → Update policy
DELETE /api/admin/quota-policies/:id                → Delete policy

IAM actions: admin:ReadQuotaPolicy, admin:CreateQuotaPolicy, admin:UpdateQuotaPolicy, admin:DeleteQuotaPolicy
```

### 6.2 Quota Override CRUD

```
GET    /api/admin/quota-overrides                   → List overrides (filterable by targetType)
POST   /api/admin/quota-overrides                   → Create override
GET    /api/admin/quota-overrides/:id               → Get override
PUT    /api/admin/quota-overrides/:id               → Update override
DELETE /api/admin/quota-overrides/:id               → Delete override

IAM actions: admin:ReadQuotaOverride, admin:CreateQuotaOverride, admin:UpdateQuotaOverride, admin:DeleteQuotaOverride
```

### 6.3 Quota Alerts

```
GET    /api/admin/quota-alerts                      → List alerts (filterable by status, alertType, targetType)
POST   /api/admin/quota-alerts/:id/acknowledge      → Acknowledge alert

IAM actions: admin:ReadQuotaAlert, admin:AcknowledgeQuotaAlert
```

### 6.4 Quota Analytics (reuses rollup)

```
GET    /api/admin/quota-analytics/overview          → Current period usage overview by scope
       ?scope=department&periodKey=2026-04
       Returns: [{targetType, targetId, targetName, costLimitUsd, currentCostUsd, usagePercent, alertLevel}]

GET    /api/admin/quota-analytics/drill-down        → Children of a parent entity
       ?parentType=department&parentId=dept-eng&periodKey=2026-04

GET    /api/admin/quota-analytics/trend             → Usage trend across periods
       ?targetType=user&targetId=xxx&periods=6

GET    /api/admin/quota-analytics/top               → Top N consumers
       ?scope=user&periodKey=2026-04&limit=10

IAM actions: admin:ReadQuotaAnalytics
```

Implementation: these endpoints query `metric_rollup_*` tables using the existing `QueryRollup` function, filtering by dimension (`user=xxx`, `virtual_key=xxx`, `project=xxx`, `department=xxx`, `organization=xxx`) and metric name (`estimated_cost_usd`, `total_tokens`).

### 6.5 Application VK Approval Workflow

```
POST   /api/admin/virtual-keys                      → Create VK (application VKs start as "pending")
POST   /api/admin/virtual-keys/:id/approve          → Approve pending VK
POST   /api/admin/virtual-keys/:id/reject           → Reject pending VK (with reason)
POST   /api/admin/virtual-keys/:id/renew            → Renew expiring VK (extend up to 3 months)
POST   /api/admin/virtual-keys/:id/revoke           → Revoke active VK

IAM actions: admin:ApproveVirtualKey, admin:RejectVirtualKey, admin:RenewVirtualKey, admin:RevokeVirtualKey
```

### 6.6 Personal VK (User Self-Service)

```
GET    /api/user/virtual-keys                       → List user's own personal VKs
POST   /api/user/virtual-keys                       → Create personal VK (auto-active, no approval)
PUT    /api/user/virtual-keys/:id                   → Update own VK (name, allowed models, etc.)
DELETE /api/user/virtual-keys/:id                   → Delete own VK
POST   /api/user/virtual-keys/:id/regenerate        → Regenerate VK secret

IAM actions: user:ReadOwnVirtualKey, user:CreateOwnVirtualKey, user:UpdateOwnVirtualKey, user:DeleteOwnVirtualKey
```

## 7. Runtime Enforcement Engine

### 7.1 Architecture

```
AI-Gateway request flow:
  VK Auth → identify VK metadata (vkType, userId, projectId, departmentId, orgId)
    │
    ├─ if vkType == "application" AND status != "active" → reject (pending/expired/revoked)
    │
    └─ Quota Check:
         buildCheckChain(vkMeta) → returns ordered list of (targetType, targetId)
           Personal:    [(vk, vkId), (user, userId), (department, deptId), (organization, orgId)]
           Application: [(vk, vkId), (project, projectId), (department, deptId), (organization, orgId)]
         │
         for each level in chain:
         │  resolveLimit(targetType, targetId) → Override or Policy match → (costLimit, enforcementMode)
         │  getCurrentUsage(targetType, targetId, period) → Redis cache
         │  if usage + estimatedCost > costLimit → record enforcement action
         │
         apply most restrictive action across all levels
```

### 7.2 Policy Resolution (per level)

```go
func resolveLimit(targetType, targetId string, vkMeta VKMeta) *ResolvedQuota {
    // 1. Check override
    override := cache.GetOverride(targetType, targetId)
    if override != nil {
        return override.toResolvedQuota()
    }

    // 2. Find matching policy (highest priority wins)
    // Match criteria: scope=targetType, departmentId matches or NULL, vkType matches or NULL, enabled=true
    // Order by priority DESC, take first
    policy := cache.FindPolicy(targetType, vkMeta.DepartmentID, vkMeta.VKType)
    if policy != nil {
        return policy.toResolvedQuota()
    }

    // 3. No limit at this level
    return nil
}
```

### 7.3 Policy & Override Cache

Policies and overrides change infrequently. Cache in AI-Gateway memory, refresh via Redis pub/sub on changes (existing pattern used for other config).

```
Control Plane → writes Policy/Override → publishes invalidation to Redis channel
AI-Gateway   → subscribes to channel → reloads policy cache from DB
```

Cache structure:
- `policiesByScope map[string][]QuotaPolicy` — keyed by scope, sorted by priority DESC
- `overridesByTarget map[string]*QuotaOverride` — keyed by "targetType:targetId"

### 7.4 Usage Cache (Redis)

```
Key:    quota:usage:{targetType}:{targetId}:{periodKey}
Value:  current cost in cents (int64, avoids float precision issues)
TTL:    auto-expire at period end (daily=midnight, weekly=Monday, monthly=1st)

Operations:
  Reserve:    GET key → compare with limit
  Reconcile:  INCRBY key actualCostCents
  Cold start: query rollup → SET key with current period sum
```

Multi-level update on reconcile (Personal VK example):
```
INCRBY quota:usage:vk:{vkId}:2026-04 costCents
INCRBY quota:usage:user:{userId}:2026-04 costCents
INCRBY quota:usage:department:{deptId}:2026-04 costCents
INCRBY quota:usage:organization:{orgId}:2026-04 costCents
```

### 7.5 Enforcement Action Priority

When multiple levels trigger different actions, the most restrictive wins:

```
reject > downgrade > notify-and-proceed > track-only > (no limit)
```

Response behavior per action (unchanged from current):
- **reject**: HTTP 429 with quota message
- **downgrade**: re-route to cheapest model within budget, set `x-nexus-quota-downgrade` header
- **notify-and-proceed**: allow request, set `x-nexus-quota-warning` header
- **track-only**: allow request, no headers (usage still tracked)

## 8. Scheduler Jobs

### 8.1 Quota Alert Check (new, 60s interval)

```
For each enabled QuotaPolicy:
  Determine all entities in scope (e.g., scope=user, deptId=eng → all users in eng dept)
  For each entity:
    currentUsage = query rollup for current period
    limit = override?.costLimitUsd ?? policy.costLimitUsd
    usagePct = currentUsage / limit * 100
    For each threshold in policy.alertThresholds:
      if usagePct >= threshold:
        UPSERT QuotaAlert (dedup by target+period+threshold)
    if usagePct < min(alertThresholds):
      Mark existing alerts as "resolved"
```

Optimization: only check entities that have rollup data for the current period (skip inactive entities).

### 8.2 VK Expiry Check (new, hourly)

```
-- Expire overdue VKs
UPDATE "VirtualKey"
SET status = 'expired', "updatedAt" = NOW()
WHERE "expiresAt" <= NOW() AND status = 'active';

-- Generate expiry warnings (7 days ahead)
SELECT id, name, "expiresAt"
FROM "VirtualKey"
WHERE "expiresAt" <= NOW() + INTERVAL '7 days'
  AND "expiresAt" > NOW()
  AND status = 'active'
  AND "vkType" = 'application';
→ UPSERT QuotaAlert (alertType='vk-expiring')
```

### 8.3 Deprecated: Quota Reset Job

The existing `resetExpiredQuotas` job is no longer needed — usage is tracked in rollup tables and Redis cache (which auto-expires by TTL). Remove this job after migration.

## 9. UI Design

### 9.1 Application VK Management (existing VK page, enhanced)

**Location:** Config → Virtual Keys (existing page)

Changes:
- Filter to show only `vkType=application` VKs
- Add status badge: pending (yellow), active (green), expired (gray), rejected (red), revoked (red)
- Add "Approve" / "Reject" action buttons for pending VKs (visible only to users with `admin:ApproveVirtualKey` permission)
- Add "Renew" action for active VKs nearing expiry
- Add "Revoke" action for active VKs
- Show expiration date column
- Creation form: add project selection (required), expiration date (required, max 3 months)

### 9.2 Personal VK Management (new, user Settings)

**Location:** User menu → Settings → Virtual Keys (new tab)

Features:
- List user's own personal VKs only
- Create / edit / delete / regenerate
- No approval workflow
- Optional expiration date
- Show current personal usage summary (from rollup)

### 9.3 Quota Policy Management (new page)

**Location:** Config → Quota Policies (new nav item)

Features:
- List all policies with: name, scope, department, vkType, periodType, costLimit, enforcement mode, enabled status
- Create / edit / delete policies
- Create override from policy context ("Add exception for specific entity")
- Override list as sub-section or tab

### 9.4 Quota Usage Dashboard (new page)

**Location:** Analytics → Quota Usage (new nav item)

Features:
- **Overview cards**: org total usage, number of active alerts, top departments by spend
- **Hierarchical table**: org → department → project/user drill-down
  - Columns: entity name, limit, current usage, usage %, alert status, trend sparkline
  - Click to expand children
- **Trend chart**: selected entity's usage over past N periods
- **Top N panel**: highest consumers by scope (user / VK / project)

Data source: rollup tables via `/api/admin/quota-analytics/*` endpoints.

### 9.5 Alert Center (new component)

**Location:** Header notification bell + dedicated Alerts page

Features:
- Bell icon with active alert count badge
- Dropdown: recent alerts with quick acknowledge
- Full page: filterable alert list (by type, status, target)
- Alert detail: which policy triggered, current vs limit, trend context

## 10. Migration Strategy

### Phase 1: Schema & API (non-breaking)

1. Add new tables (QuotaPolicy, QuotaOverride, QuotaAlert) via Prisma migration
2. Add new VirtualKey columns (vkType, status, expiresAt, approval fields)
3. Set all existing VKs to `vkType='personal'`, `status='active'` as default
4. Deploy new Policy/Override/Alert CRUD APIs
5. Deploy new scheduler jobs (alert check, VK expiry)

### Phase 2: Enforcement Switch

1. Deploy new policy resolution engine in AI-Gateway (reads QuotaPolicy + QuotaOverride)
2. Run in shadow mode: log new engine decisions alongside old Quota table decisions, compare
3. Switch enforcement to new engine
4. Migrate existing Quota records to equivalent policies/overrides

### Phase 3: UI & Cleanup

1. Deploy updated VK management page (application VK with approval)
2. Deploy personal VK settings page
3. Deploy quota policy management page
4. Deploy quota usage dashboard
5. Deploy alert center
6. Drop deprecated Quota table after verification period

## 11. IAM Permissions

New permissions to add to the IAM engine:

```
admin:ReadQuotaPolicy
admin:CreateQuotaPolicy
admin:UpdateQuotaPolicy
admin:DeleteQuotaPolicy
admin:ReadQuotaOverride
admin:CreateQuotaOverride
admin:UpdateQuotaOverride
admin:DeleteQuotaOverride
admin:ReadQuotaAlert
admin:AcknowledgeQuotaAlert
admin:ReadQuotaAnalytics
admin:ApproveVirtualKey
admin:RejectVirtualKey
admin:RenewVirtualKey
admin:RevokeVirtualKey
user:ReadOwnVirtualKey
user:CreateOwnVirtualKey
user:UpdateOwnVirtualKey
user:DeleteOwnVirtualKey
```

## 12. Data Flow Summary

```
                        ┌─────────────────────────────┐
                        │      Control Plane UI        │
                        │                              │
                        │  Policy Mgmt  │  VK Mgmt    │
                        │  Usage Dash   │  Alerts      │
                        │  User Settings (Personal VK) │
                        └──────────┬──────────────────┘
                                   │
                        ┌──────────▼──────────────────┐
                        │      Control Plane API       │
                        │                              │
                        │  Policy/Override CRUD        │
                        │  Alert CRUD                  │
                        │  VK Approval Workflow        │
                        │  Analytics (→ rollup query)  │
                        │                              │
                        │  Scheduler:                  │
                        │   - Alert check (60s)        │
                        │   - VK expiry (1h)           │
                        │   - Rollup jobs (existing)   │
                        └──────────┬──────────────────┘
                                   │
                    ┌──────────────┼──────────────────┐
                    │              │                   │
              ┌─────▼─────┐  ┌────▼────┐  ┌──────────▼──────────┐
              │ PostgreSQL │  │  Redis  │  │     AI-Gateway       │
              │            │  │         │  │                      │
              │ QuotaPolicy│  │ usage   │  │ Policy cache (mem)   │
              │ QuotaOver. │  │ cache   │  │ Override cache (mem) │
              │ QuotaAlert │  │ (INCR)  │  │                      │
              │ VirtualKey │  │         │  │ Reserve:             │
              │ rollup_*   │  │ pub/sub │  │  resolve limits      │
              │            │  │ config  │  │  check Redis usage   │
              └────────────┘  │ inval.  │  │  enforce action      │
                              └─────────┘  │                      │
                                           │ Reconcile:           │
                                           │  INCRBY all levels   │
                                           │  pending → flush     │
                                           └──────────────────────┘
```
