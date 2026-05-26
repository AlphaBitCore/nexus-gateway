# System Integrity Fixes — Design Specification

**Date:** 2026-04-13
**Scope:** Fix 10 cross-component issues identified during product-level system review, ensuring all components (control-plane, ai-gateway, compliance-proxy, agent, shared) form a complete, correctly integrated business loop.

---

## Table of Contents

1. [Problem Summary](#1-problem-summary)
2. [Architecture Principles](#2-architecture-principles)
3. [Epoch A: Infrastructure Hardening](#3-epoch-a-infrastructure-hardening)
4. [Epoch B: Content Extraction Unification + Agent Sync Merge](#4-epoch-b-content-extraction-unification--agent-sync-merge)
5. [Epoch C: Compliance Capability Completion](#5-epoch-c-compliance-capability-completion)
6. [Epoch D: Data Model Unification + Feature Completion](#6-epoch-d-data-model-unification--feature-completion)
7. [Unified Pipeline Architecture](#7-unified-pipeline-architecture)
8. [Cross-Epoch Dependencies](#8-cross-epoch-dependencies)
9. [Documentation Updates](#9-documentation-updates)

---

## 1. Problem Summary

| # | Severity | Problem | Epoch |
|---|----------|---------|-------|
| 1 | 🔴 Critical | macOS Agent compliance hooks never execute — NE IPC protocol lacks body inspection | C |
| 2 | 🔴 Critical | Compliance Proxy non-SSE response hooks are observational only, never block | C |
| 3 | 🟡 Medium | Two parallel content extraction paths (ExtractorRegistry vs traffic.Adapter) | B |
| 4 | 🟡 Medium | IAM policy cache is process-local; multi-replica inconsistency up to 60s | A |
| 5 | 🟡 Medium | Config invalidation is best-effort; Redis errors silently swallowed | A |
| 6 | 🟡 Medium | Agent has two redundant config sync paths polling the same endpoint | B |
| 7 | 🟢 Minor | shared rate-limiter hook is process-local; effective limit = N × configured under horizontal scaling | A |
| 8 | 🟢 Minor | OIDC/SSO JWT validation not wired in auth middleware despite config storage | D |
| 9 | 🟢 Minor | shared/store and shared/access packages are stubs; configloader SQL duplicated across services | A |
| 10 | 🟢 Minor | Three separate audit tables (AuditLog, matrix_audit_event, AgentAuditEvent) complicate cross-mode analysis | D |
| 11 | — Enhancement | Agent response pipeline missing; hook consistency gap across services | C |
| 12 | — Enhancement | Agent device registration lacks OS user/system info collection | D |
| 13 | — Enhancement | Device management observability (fleet dashboard, user/device detail, logs) | D |
| 14 | — Enhancement | Unified user model (NexusUser) with organization association | D |
| 15 | — Enhancement | Dual-mode device authentication (os-identity / enterprise-login) | D |

---

## 2. Architecture Principles

### Unified Pipeline Architecture (Target State)

All three data plane services share the same hook pipeline with different pre-processing:

```
                    Pre-processing           Hook Pipeline           Post-processing
                    (differentiated)         (fully unified)

AI Gateway:     VK Auth → RateLimit       → Request Hooks         → Upstream
                → Quota → Routing           → Response Hooks       → Audit

Compliance      Domain/Path Filter        → Request Hooks         → Upstream
Proxy:          → Access Control            → Response Hooks       → Audit + SIEM

Agent:          Domain/Path Filter        → Request Hooks         → Upstream
                → Policy Engine             → Response Hooks       → Audit
```

### Content Extraction (Target State)

Single `traffic.Adapter` system across all services. Built-in adapters for all mainstream AI providers:

- `openai-compat` (existing)
- `anthropic` (new)
- `gemini` (new)
- `azure-openai` (new)
- `deepseek` (new)
- `glm` (new)
- `minimax` (new)
- `generic-jsonpath` (existing, fallback)

### Unified User Model (Target State)

Single `NexusUser` table anchoring all identities:

```
NexusUser (unified identity)
  ├── canAccessControlPlane = true  →  Admin UI access (permissions via IAM)
  ├── has AgentDevices              →  Endpoint user (managed devices)
  └── both                          →  Admin who also has agent devices
```

---

## 3. Epoch A: Infrastructure Hardening

No external dependencies. Lays foundation for subsequent epochs.

### A1: IAM Cache Redis-Backed + Pub/Sub Broadcast

**Problem:** `packages/control-plane/internal/iam/engine.go` uses an in-process `map[string]*cachedPolicies` with 60s TTL. Multi-replica deployments have inconsistent IAM state for up to 60 seconds after a policy change.

**Design:**

1. **Two-layer cache:**
   - L1: in-process map, 10s TTL (hot path, zero-latency)
   - L2: Redis, 60s TTL (shared across replicas)
   - Redis unavailable → degrade to L1-only (current behavior preserved)

2. **Pub/Sub invalidation:**
   - IAM mutations publish `{"topic":"iam"}` to `nexus:config:shared`
   - All replicas subscribe; on `iam` topic: flush L1 + delete Redis keys
   - Reuse existing `pubsub.Publisher.PublishInvalidation()`

3. **Retry on Redis write failure:** in-process cache still works; next request retries Redis write

**Files changed:**
- `packages/control-plane/internal/iam/engine.go` — add Redis cache read/write
- `packages/control-plane/internal/iam/cache.go` — new: L1/L2 cache abstraction
- `packages/control-plane/internal/handler/iam_routes.go` — publish invalidation on IAM mutations
- `packages/control-plane/cmd/control-plane/main.go` — subscribe to `iam` topic

### A2: Config Invalidation Retry + Prometheus Metrics

**Problem:** `pubsub/invalidation.go` silently swallows Redis publish errors.

**Design:**

1. **Retry:** up to 3 attempts with exponential backoff (100ms, 200ms, 400ms)
2. **Prometheus counters:**
   - `nexus_config_invalidation_published_total{topic}` — successful publishes
   - `nexus_config_invalidation_failed_total{topic}` — final failures (after 3 retries)
3. **Ops documentation:** document cache TTL degradation behavior per component during Redis outage

**Files changed:**
- `packages/control-plane/internal/pubsub/invalidation.go` — retry + metrics
- `docs/ops/redis.md` — document Redis failure degradation behavior

### A3: Shared Rate Limiter — Redis Distributed Mode

**Problem:** `packages/shared/policy/hooks/rate_limiter.go` uses a process-local sliding window. Under horizontal scaling, effective limit = N × configured value.

**Design:**

1. **Extract common sliding window module:** move Redis Lua script logic from `packages/ai-gateway/internal/pipeline/ratelimit/` to `packages/shared/ratelimit/`
2. **Optional Redis backend for rate-limiter hook:**
   - With Redis: distributed sliding window (Lua script)
   - Without Redis: process-local (current behavior)
   - Redis failure: auto-degrade to process-local
3. **Redis client injection:** via `HookConfig.Config["_redis"]` or factory option pattern

**Files changed:**
- `packages/shared/ratelimit/` — new: extracted from ai-gateway
- `packages/shared/policy/hooks/rate_limiter.go` — add Redis mode
- `packages/ai-gateway/internal/pipeline/ratelimit/` — refactor to import shared

### A4: shared/store + shared/access Consolidation

**Problem:** Each service has its own `configloader` with duplicated SQL queries. `shared/store` and `shared/access` are declared stubs.

**Design:**

1. **shared/store — common config loaders:**
   - `LoadHookConfigs(ctx, pool) ([]hooks.HookConfig, error)`
   - `LoadInterceptionDomains(ctx, pool) ([]configtypes.InterceptionDomain, error)`
   - `LoadInterceptionPaths(ctx, pool) ([]configtypes.InterceptionPath, error)`
   - `LoadDomainAllowlist(ctx, pool) ([]DomainAllowlistEntry, error)`
   - Interface: `type ConfigStore interface { ... }`, implementation: `PgConfigStore`

2. **shared/access — common access control utilities:**
   - Extract IP allowlist/domain allowlist matching from compliance-proxy `access/checker.go`
   - `ParseCIDRList()`, `MatchIP()`, `MatchDomain()` public functions

3. **Consumer migration:** compliance-proxy and ai-gateway configloaders delegate to shared/store

**Files changed:**
- `packages/shared/store/` — new: ConfigStore implementation
- `packages/shared/access/` — new: access control utilities
- `packages/compliance-proxy/internal/configloader/` — migrate to shared
- `packages/ai-gateway/internal/pipeline/hooks/config_loader.go` — migrate to shared

---

## 4. Epoch B: Content Extraction Unification + Agent Sync Merge

Depends on Epoch A (shared/store consolidation).

### B1: New traffic.Adapter Implementations for All Mainstream Providers

**Problem:** `compliance.ExtractorRegistry` has Anthropic and Gemini extractors, but `traffic.Adapter` system only has `openai-compat` and `generic-jsonpath`. Other providers supported by AI Gateway (Azure, DeepSeek, GLM, MiniMax) also lack adapters.

**Design — new adapters:**

| Adapter ID | Package | Request Extraction | Response Extraction | Stream Chunk |
|------------|---------|-------------------|--------------------|--------------| 
| `anthropic` | `traffic/adapters/anthropic/` | `system` + `messages[].content` (string/blocks) | `content[].text` | `content_block_delta.delta.text` |
| `gemini` | `traffic/adapters/gemini/` | `contents[].parts[].text` + `systemInstruction` | `candidates[].content.parts[].text` | Same as response (full candidate object) |
| `azure-openai` | `traffic/adapters/azure/` | Same as OpenAI (body is identical) | Same as OpenAI | Same as OpenAI |
| `deepseek` | `traffic/adapters/deepseek/` | OpenAI-compatible format | OpenAI-compatible format | OpenAI-compatible delta |
| `glm` | `traffic/adapters/glm/` | Provider-specific format extraction | Provider-specific format | Provider-specific delta |
| `minimax` | `traffic/adapters/minimax/` | Provider-specific format extraction | Provider-specific format | Provider-specific delta |

All registered in `adapters/builtins.go` via `RegisterBuiltins()`.

**Files changed:**
- `packages/shared/traffic/adapters/anthropic/anthropic.go` — new
- `packages/shared/traffic/adapters/gemini/gemini.go` — new
- `packages/shared/traffic/adapters/azure/azure.go` — new
- `packages/shared/traffic/adapters/deepseek/deepseek.go` — new
- `packages/shared/traffic/adapters/glm/glm.go` — new
- `packages/shared/traffic/adapters/minimax/minimax.go` — new
- `packages/shared/traffic/adapters/builtins.go` — register all new adapters
- Corresponding `*_test.go` files for each adapter

### B2: Compliance Proxy Migration to traffic.Adapter System

**Problem:** Compliance proxy uses `compliance.ExtractorRegistry` (hardcoded host→function), inconsistent with agent's `traffic.Adapter` + `DomainSnapshot`.

**Design:**

1. **Initialize AdapterRegistry + DomainSnapshot** in `cmd/compliance-proxy/init.go`:
   - Create `traffic.AdapterRegistry`, call `adapters.RegisterBuiltins()`
   - Load `InterceptionDomain` + `InterceptionPath` from DB (via shared/store from A4)
   - Build `traffic.DomainSnapshot`
   - Store in `atomic.Pointer[*traffic.DomainSnapshot]`; Redis pub/sub triggers rebuild on `interceptionDomains` topic

2. **Replace ExtractorRegistry calls** in `forward_handler.go`:
   - `snapshot.FindInstance(targetHost)` → get adapter
   - `adapter.ExtractRequest(ctx, rawBody, path)` → get `NormalizedContent`
   - Write `NormalizedContent.Segments` to `tx.NormalizedContent`
   - Same for response: `adapter.ExtractResponse()`

3. **Replace streaming extraction** in `streaming/live.go`:
   - `adapter.ExtractStreamChunk(ctx, chunkData, path)` replaces hardcoded `choices[0].delta.content` gjson extraction

4. **Fallback:** unknown domains use `generic-jsonpath` adapter's recursive fallback (equivalent to current `extractFallback`)

**Files changed:**
- `packages/compliance-proxy/cmd/compliance-proxy/init.go` — initialize AdapterRegistry + DomainSnapshot
- `packages/compliance-proxy/internal/proxy/forward_handler.go` — replace ExtractorRegistry
- `packages/compliance-proxy/internal/streaming/live.go` — replace chunk extraction
- `packages/compliance-proxy/internal/configcache/subscriber.go` — add DomainSnapshot hot-swap callback

### B3: Deprecate compliance.ExtractorRegistry

After B2 migration is complete:

1. Delete `packages/shared/compliance/normalize.go` and `normalize_test.go`
2. Remove `NewExtractorRegistry()` from compliance-proxy init
3. Update architecture docs

**Files changed:**
- `packages/shared/compliance/normalize.go` — delete
- `packages/shared/compliance/normalize_test.go` — delete

### B4: Agent Config Sync Merge into Single Syncer

**Problem:** Agent has two independent sync paths polling the same `GET /api/agent/config` endpoint: a config refresh ticker in `main.go` and `configsync.Syncer`.

**Design:**

1. **Merge into `configsync.UnifiedSyncer`:**
   - Single HTTP fetch with ETag caching
   - Parse into two parts: policy rules (merge to `config.Manager`) + hook/domain configs (apply to `AgentPipeline`)
   - Single ticker at `ConfigRefreshSec` interval
   - Preserve offline fallback (`OfflineFallback`)

2. **Atomic consistency:** policy rules and hook/domain parsed from same response, guaranteed same config version

3. **Remove redundant path:** delete config refresh ticker goroutine from `main.go`

**Files changed:**
- `packages/agent/core/sync/configsync/syncer.go` — refactor to UnifiedSyncer
- `packages/agent/core/sync/configsync/unified.go` — new: merged logic
- `packages/agent/cmd/agent/main.go` — remove old config refresh ticker
- `packages/agent/core/sync/config/config.go` — adjust MergeConfig interface

---

## 5. Epoch C: Compliance Capability Completion

Depends on Epoch B (unified extractors).

### C1: Compliance Proxy Non-SSE Response Hook Blocking

**Problem:** Non-SSE response REJECT_HARD decisions are observational only; the original response is forwarded to the client despite the violation.

**Design:**

1. **Block and replace:** when response pipeline returns REJECT_HARD:
   - Do NOT write original response body to client
   - Return compliance block response:
     ```json
     {
       "error": {
         "type": "compliance_blocked",
         "message": "Response blocked by compliance policy",
         "reason": "<hookReason>",
         "reason_code": "<hookReasonCode>",
         "transaction_id": "<transactionID>"
       }
     }
     ```
   - HTTP status 451 (Unavailable For Legal Reasons)
   - HTML block page if original Content-Type was HTML

2. **REJECT_SOFT remains observational:** forward original response + audit record (unchanged)

3. **Configurable via `responseBlockMode`:**
   - `block` (default, new behavior): REJECT_HARD returns replacement response
   - `observe` (legacy behavior): REJECT_HARD only audits
   - Manageable from UI, hot-reloads via Redis pub/sub

4. **Audit event:** marks `responseBlocked: true`, records original status code

**Files changed:**
- `packages/compliance-proxy/internal/proxy/forward_handler.go` — response hook blocking logic
- `packages/compliance-proxy/internal/config/config.go` — add `responseBlockMode`
- `packages/compliance-proxy/internal/proxy/reject.go` — extend reject response generator

### C2: macOS NE IPC Protocol Extension (Body Inspection)

**Problem:** macOS NE IPC protocol only transfers connection metadata (host/port/pid). Go agent's `InspectRequest()` and `InspectResponse()` are never called on macOS. Compliance hooks do not execute.

**Design:**

1. **New IPC message types:**

   **NE → Go (request inspection):**
   ```json
   {
     "type": "flow_inspect",
     "flowId": "...",
     "method": "POST",
     "path": "/v1/chat/completions",
     "contentType": "application/json",
     "bodyBase64": "<base64-encoded request body>",
     "bodySize": 4096
   }
   ```

   **Go → NE (inspection result):**
   ```json
   {
     "flowId": "...",
     "type": "inspect_result",
     "decision": "approve|reject_hard|reject_soft",
     "reason": "...",
     "reasonCode": "..."
   }
   ```

   **NE → Go (response inspection):**
   ```json
   {
     "type": "flow_inspect_response",
     "flowId": "...",
     "statusCode": 200,
     "contentType": "application/json",
     "bodyBase64": "..."
   }
   ```

2. **Body size limit:** bodies > 10MB are not sent; Go executes metadata-only hooks (rate-limiter, ip-access); body-dependent hooks (PII, keyword) are skipped with audit log

3. **Go-side handler:** `darwin.go` adds `handleFlowInspect()` and `handleFlowInspectResponse()`:
   - Base64 decode body
   - Call `connectionBridge.InspectRequest()` / `connectionBridge.InspectResponse()`
   - Return inspection result JSON
   - Timeout: 5s default, returns fail-open/fail-closed per config

4. **Swift NE changes:** after TLS decryption:
   - Parse HTTP request headers and body
   - Send `flow_inspect` to Go agent
   - Wait for `inspect_result` (with timeout)
   - On `reject_hard`: return block response to client
   - On timeout: default fail-open (configurable)

5. **Backward compatibility:** NE detecting old Go agent (no `flow_inspect` support) falls back to current behavior (`flow_new` decision only)

6. **Audit enrichment:** `flow_closed` message adds `hookDecision`, `hookReason`, `hookReasonCode` fields

**Files changed:**
- `packages/agent/core/platform/darwin.go` — add `handleFlowInspect`, `handleFlowInspectResponse`
- `packages/agent/core/platform/platform.go` — add `ResponseInspector` interface
- `packages/agent/cmd/agent/main.go` — connectionBridge implements ResponseInspector
- `packages/agent/platform/darwin/NexusAgent/` — Swift NE extension changes

### C3: Agent Response Pipeline

**Problem:** Agent only has request-stage hooks. Windows/Linux TLS MITM can inspect responses but don't run hooks. This breaks hook consistency across all three services.

**Design:**

1. **`intercept.Handler.ProcessResponse()`** — symmetric with `ProcessRequest()`:
   - `snapshot.FindInstance(host)` → get adapter
   - `adapter.ExtractResponse(ctx, body, path)` → NormalizedContent
   - `resolver.BuildResponsePipeline(tx, ...)` → Pipeline
   - `pipeline.Execute(ctx, tx)` → decision

2. **`ResponseInspector` interface:**
   ```go
   type ResponseInspector interface {
       InspectResponse(host, path, method string, body []byte) InspectionResult
   }
   ```

3. **Windows/Linux `proxy.MITMRelay()` changes:**
   - After reading upstream response, before writing to client: call `InspectResponse`
   - REJECT_HARD → return replacement response (consistent with C1)
   - REJECT_SOFT → forward + audit
   - SSE streaming → reuse compliance-proxy's checkpoint pattern (holdback + incremental inspection)

4. **macOS:** handled by C2's `flow_inspect_response` IPC message

5. **Audit:** agent audit events now include `responseHookDecision`, `responseHookReason`, `responseHookReasonCode`

**Files changed:**
- `packages/agent/core/network/intercept/handler.go` — add `ProcessResponse()`
- `packages/agent/core/platform/platform.go` — add `ResponseInspector` interface
- `packages/agent/core/network/proxy/proxy.go` — `MITMRelay` add response inspection
- `packages/agent/core/platform/windows.go` — pass ResponseInspector
- `packages/agent/core/platform/linux.go` — pass ResponseInspector
- `packages/agent/cmd/agent/main.go` — connectionBridge implements ResponseInspector

### C4: macOS Hook Pipeline Integration Tests

1. IPC protocol tests: mock NE sending `flow_inspect` / `flow_inspect_response`, verify Go returns correct decisions
2. End-to-end flow: `flow_new` → `flow_inspect` → `inspect_result` → `flow_inspect_response` → `inspect_result` → `flow_closed`
3. Timeout and degradation: timeout, oversized body, hook failure
4. Cross-platform consistency: same request produces same hook decision on macOS/Windows/Linux

**Files changed:**
- `packages/agent/core/platform/darwin_test.go` — IPC protocol tests
- `packages/agent/core/network/intercept/handler_test.go` — cross-platform decision consistency

---

## 6. Epoch D: Data Model Unification + Feature Completion

Involves DB migrations. Ordered to minimize risk.

### D0: NexusUser Unified User Model

**Problem:** System has disconnected identity pools: `AdminUser` (control plane admins) and no formal user entity for agent-managed end users. Users lack organizational association. A single person may have admin capability AND managed devices but these are not linked.

**Design:**

#### NexusUser Table

```sql
CREATE TABLE "NexusUser" (
  "id"                      TEXT PRIMARY KEY,
  "organizationId"          TEXT REFERENCES "Organization"("id"),

  -- Core identity
  "displayName"             TEXT NOT NULL,
  "email"                   TEXT UNIQUE,
  "department"              TEXT,
  "status"                  TEXT NOT NULL,             -- active / suspended
  "canAccessControlPlane"   BOOLEAN NOT NULL DEFAULT false,

  -- OS identity (agent-collected)
  "osUsername"               TEXT,
  "osDomain"                TEXT,

  -- SSO identity
  "ssoSubject"               TEXT,
  "ssoProvider"              TEXT,
  "ssoLastAuthAt"            TIMESTAMPTZ,

  -- Password (only for canAccessControlPlane=true non-SSO-only users)
  "passwordHash"             TEXT,

  -- Timestamps
  "firstSeenAt"              TIMESTAMPTZ NOT NULL,
  "lastSeenAt"               TIMESTAMPTZ NOT NULL,
  "createdAt"                TIMESTAMPTZ NOT NULL DEFAULT now(),

  UNIQUE("osUsername", "osDomain"),
  UNIQUE("ssoSubject", "ssoProvider")
);
```

#### AgentDevice Table (Pure Hardware/Software — No Direct User FK)

A device is a physical machine identified by `machineId`/`serialNumber`. It does NOT directly reference a user. The user-device relationship is managed through the `DeviceAssignment` junction table to support device transfer between users.

```sql
CREATE TABLE "AgentDevice" (
  "id"                 TEXT PRIMARY KEY,

  -- Device identity (immutable per machine)
  "hostname"           TEXT NOT NULL,
  "serialNumber"       TEXT,
  "machineId"          TEXT UNIQUE,

  -- System info
  "os"                 TEXT,              -- darwin / windows / linux
  "osVersion"          TEXT,              -- "macOS 15.4" / "Windows 11 23H2"
  "arch"               TEXT,              -- arm64 / amd64

  -- Network info (refreshed by heartbeat)
  "localIPs"           TEXT[],
  "publicIP"           TEXT,
  "macAddresses"       TEXT[],
  "domain"             TEXT,              -- AD/LDAP domain

  -- Agent status
  "agentVersion"       TEXT,
  "status"             TEXT NOT NULL,     -- ONLINE / OFFLINE / PENDING_AUTH / REVOKED
  "configVersion"      TEXT,
  "lastHeartbeat"      TIMESTAMPTZ,
  "certSerial"         TEXT UNIQUE,       -- mTLS cert serial
  "certExpiresAt"      TIMESTAMPTZ,

  -- Runtime metrics (refreshed by heartbeat)
  "memoryMB"           INT,
  "cpuUsagePercent"    REAL,
  "diskFreeGB"         REAL,
  "auditQueueDepth"    INT,

  -- Convenience: current user (denormalized for fast queries)
  "currentUserId"      TEXT REFERENCES "NexusUser"("id"),
  "currentAssignedAt"  TIMESTAMPTZ,

  -- Group and timestamps
  "deviceGroupId"      TEXT REFERENCES "DeviceGroup"("id"),
  "enrolledAt"         TIMESTAMPTZ NOT NULL,
  "lastConfigSync"     TIMESTAMPTZ,
  "createdAt"          TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

#### DeviceAssignment Table (User-Device Junction with History)

Tracks the full assignment history. A device belongs to exactly one user at any given time. When the device changes hands, the current assignment is closed (`releasedAt` set) and a new one is created.

```sql
CREATE TABLE "DeviceAssignment" (
  "id"          TEXT PRIMARY KEY,
  "deviceId"    TEXT NOT NULL REFERENCES "AgentDevice"("id"),
  "userId"      TEXT NOT NULL REFERENCES "NexusUser"("id"),
  "assignedAt"  TIMESTAMPTZ NOT NULL,     -- start of usage
  "releasedAt"  TIMESTAMPTZ,              -- end of usage (NULL = current user)
  "source"      TEXT NOT NULL,            -- 'agent-enroll' / 'heartbeat-detect' / 'admin-reassign'
  "createdAt"   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Indexes
CREATE INDEX idx_da_device ON "DeviceAssignment"("deviceId", "assignedAt" DESC);
CREATE INDEX idx_da_user ON "DeviceAssignment"("userId", "assignedAt" DESC);
-- Ensures at most one active assignment per device
CREATE UNIQUE INDEX idx_da_active ON "DeviceAssignment"("deviceId") WHERE "releasedAt" IS NULL;
```

**Assignment transfer logic:**
- Agent heartbeat reports `osUser` change → Control Plane detects current user differs:
  1. Set `releasedAt = now()` on the current active assignment
  2. Match/create `NexusUser` for new `osUser`
  3. Create new `DeviceAssignment` (`releasedAt = NULL`, `source = 'heartbeat-detect'`)
  4. Update `AgentDevice.currentUserId` (denormalized convenience field)
- Admin manual reassign → same logic, `source = 'admin-reassign'`
- Initial enrollment → first assignment created, `source = 'agent-enroll'`

**Query patterns:**
- "Who is currently using this device?" → `AgentDevice.currentUserId` (O(1) lookup)
- "History of users for this device?" → `SELECT * FROM DeviceAssignment WHERE deviceId = ? ORDER BY assignedAt DESC`
- "What devices does this user currently have?" → `SELECT * FROM DeviceAssignment WHERE userId = ? AND releasedAt IS NULL`
- "All devices this user has ever used?" → `SELECT * FROM DeviceAssignment WHERE userId = ?`

#### Device Lifecycle Events in Unified Audit

Device system events (enrollment, user switches, config changes, status transitions) are recorded in the unified `audit_event` table with `source = 'device-lifecycle'`:

```json
{
  "source": "device-lifecycle",
  "sourceDetails": {
    "deviceId": "...",
    "eventType": "enrolled | user_changed | config_updated | status_changed | cert_renewed | group_changed",
    "previousUser": "john.smith",
    "newUser": "jane.doe",
    "previousStatus": "ONLINE",
    "newStatus": "OFFLINE",
    "detail": "User switch detected via heartbeat"
  }
}
```

This means:
- Device detail page timeline = `audit_event WHERE sourceDetails->>'deviceId' = ?`
- User timeline across all devices = `audit_event WHERE nexusUserId = ?`
- No additional table needed; reuses the unified audit infrastructure
```

#### Capability Logic

- `canAccessControlPlane = false` → no Control Plane UI access (vast majority of users)
- `canAccessControlPlane = true` → can login to admin UI; actual permissions governed by IAM (IamGroup + IamPolicyAttachment)
- Agent-enrolled NexusUser always created with `canAccessControlPlane = false`
- Only existing `super_admin` equivalent (via IAM policy) can set `canAccessControlPlane = true`
- Login middleware (`adminauth.go`) enforces `canAccessControlPlane = true` at authentication layer, before IAM evaluation

#### Relationships

```
NexusUser
  ├── Organization (N:1)           -- single org membership; NULL = global
  ├── DeviceAssignment (1:N)       -- current + historical device assignments
  │     └── AgentDevice (N:1)      -- each assignment points to one device
  ├── AdminApiKey (1:N)            -- API keys (only if canAccessControlPlane)
  ├── AdminAuditLog (1:N)          -- admin operation audit (as actor)
  ├── audit_event (1:N)            -- traffic audit (as subject)
  ├── IamGroupMembership (N:M)     -- IAM group memberships
  └── IamPolicyAttachment (1:N)    -- directly attached IAM policies

AgentDevice
  ├── currentUserId → NexusUser    -- denormalized current user (fast lookup)
  ├── DeviceAssignment (1:N)       -- full assignment history
  ├── DeviceGroup (N:1)            -- device group membership
  └── audit_event (via sourceDetails.deviceId) -- device audit events
```

#### Migration Strategy

```sql
-- Migrate existing AdminUser records to NexusUser
INSERT INTO "NexusUser" (id, displayName, email, canAccessControlPlane, passwordHash, status, firstSeenAt, lastSeenAt, createdAt)
SELECT id, username, email, true, "passwordHash", 'active', "createdAt", "createdAt", "createdAt"
FROM "AdminUser";
```

- AdminApiKey `userId` FK → `NexusUser.id`
- AdminAuditLog `actorId` → `NexusUser.id`
- AgentDevice `currentUserId` → `NexusUser.id` (denormalized); full history via `DeviceAssignment` table
- Old `AdminUser` table kept 30 days then dropped

#### Affected Existing Features

| Feature | Change |
|---------|--------|
| Login auth | Query `NexusUser WHERE canAccessControlPlane = true` |
| IAM evaluation | Principal = `NexusUser.id` |
| API keys | FK from `AdminUser` to `NexusUser` |
| Admin audit | actorId = `NexusUser.id` |
| Agent enrollment | Create/associate `NexusUser` (canAccessControlPlane = false) + create `DeviceAssignment` |
| Agent heartbeat | Update `NexusUser.lastSeenAt`; detect user change → close old assignment, create new one |
| Admin user CRUD | API paths unchanged; underlying table = `NexusUser WHERE canAccessControlPlane = true` |

**Files changed:**
- `tools/db-migrate/prisma/schema.prisma` — NexusUser model, AgentDevice enhancements, DeviceAssignment table
- `tools/db-migrate/prisma/migrations/` — migration scripts (create tables + AdminUser data migration)
- `tools/db-migrate/codegen-go.mjs` — generate new types
- `packages/shared/schemas/configtypes/` — generated NexusUser, AgentDevice, DeviceAssignment types
- `packages/control-plane/internal/middleware/adminauth.go` — query NexusUser
- `packages/control-plane/internal/handler/` — all handlers referencing AdminUser
- `packages/control-plane/internal/iam/engine.go` — principal = NexusUser.id
- `packages/control-plane/internal/store/` — user queries

### D1: Unified Audit Table (3 Tables → 1)

**Problem:** `AuditLog`, `matrix_audit_event`, `AgentAuditEvent` have different schemas. Unified query (`GetUnifiedAudit`) runs 3 separate SQL queries, merges in-memory, and loses most fields.

**Design:**

#### Unified `audit_event` Table

```sql
CREATE TABLE "audit_event" (
  "id"                        TEXT PRIMARY KEY,
  "source"                    TEXT NOT NULL,        -- 'vk' | 'proxy' | 'agent' | 'device-lifecycle'
  "timestamp"                 TIMESTAMPTZ NOT NULL,
  "sourceIp"                  TEXT,
  "targetHost"                TEXT NOT NULL,
  "method"                    TEXT,
  "path"                      TEXT,
  "statusCode"                INT,
  "latencyMs"                 INT,

  -- Unified identity
  "nexusUserId"               TEXT REFERENCES "NexusUser"("id"),
  "subjectId"                 TEXT,                 -- display label: userId / osUser@hostname
  "organizationId"            TEXT,
  "projectId"                 TEXT,

  -- Unified hook pipeline (request + response)
  "hookDecision"              TEXT,
  "hookReason"                TEXT,
  "hookReasonCode"            TEXT,
  "hooksPipeline"             JSONB,                -- {request: [...], response: [...]}
  "responseHookDecision"      TEXT,
  "responseHookReason"        TEXT,
  "responseHookReasonCode"    TEXT,
  "dataClassification"        TEXT,

  -- Source-specific details
  "sourceDetails"             JSONB NOT NULL DEFAULT '{}',

  -- Time management
  "createdAt"                 TIMESTAMPTZ NOT NULL DEFAULT now(),
  "dsarDeleteRequested"       BOOLEAN DEFAULT false
);
```

**`sourceDetails` JSONB per source:**
- `source=vk`: virtualKeyId, credentialId, provider, modelUsed, promptTokens, completionTokens, totalTokens, estimatedCostUsd, cacheHit, routingRuleId, routedProvider, routedModel, routingTrace, qualitySignals, sourceApp
- `source=proxy`: connectionId, transactionId, trafficSource, ingressType, bumpStatus, userAgent
- `source=agent`: deviceId, deviceHostname, deviceOS, sourceProcess, destIp, destPort, action, bytesIn, bytesOut, policyRuleId, bumpStatus
- `source=device-lifecycle`: deviceId, eventType (enrolled/user_changed/config_updated/status_changed/cert_renewed/group_changed), previousUser, newUser, previousStatus, newStatus, detail

**`hooksPipeline` JSONB structure (unified):**
```json
{
  "request": [
    {"hookId": "...", "implId": "keyword-filter", "decision": "APPROVE", "latencyMs": 2, "dataClassification": "INTERNAL"},
    {"hookId": "...", "implId": "pii-detector", "decision": "REJECT_HARD", "reason": "SSN detected", "latencyMs": 5}
  ],
  "response": [
    {"hookId": "...", "implId": "quality-checker", "decision": "APPROVE", "latencyMs": 8}
  ]
}
```

**Indexes:**
- `(source, timestamp DESC)` — per-source timeline
- `(timestamp DESC)` — unified timeline
- `(nexusUserId, timestamp DESC)` — per-user query
- `(subjectId, timestamp DESC)` — DSAR query
- `(targetHost, timestamp DESC)` — per-target query
- `(hookDecision, timestamp DESC)` — filter by compliance decision
- GIN on `sourceDetails` — source-specific field queries

**Migration:**
- Create `audit_event` table
- Data migration script: INSERT INTO ... SELECT from three old tables with field mapping
- Keep old tables 30 days (read-compatible); new data writes only to `audit_event`
- Drop old tables after 30 days

**Writer migration:**
- AI Gateway `audit/audit.go` → write to `audit_event`, `source='vk'`
- Compliance Proxy `audit/writer.go` → write to `audit_event`, `source='proxy'`
- Agent audit upload `POST /api/agent/audit` → Control Plane writes to `audit_event`, `source='agent'`

**Query migration:**
- `GetUnifiedAudit()` simplifies to single-table query with `WHERE source IN (...)`
- Per-source queries use `source` filter + `sourceDetails` JSONB operators
- DSAR queries unify to `WHERE nexusUserId = $1`

**Files changed:**
- `tools/db-migrate/prisma/schema.prisma` — new `audit_event` model
- `tools/db-migrate/prisma/migrations/` — migration + data migration script
- `packages/shared/schemas/configtypes/audit_event.go` — generated type
- `packages/ai-gateway/internal/observability/audit/audit.go` — write to new table
- `packages/compliance-proxy/internal/audit/writer.go` — write to new table
- `packages/control-plane/internal/handler/agent_api.go` — audit upload writes new table
- `packages/control-plane/internal/store/misc_queries.go` — simplify unified query
- `packages/control-plane/internal/store/audit_queries.go` — new: per-source queries
- `packages/control-plane/internal/handler/audit_routes.go` — adapt to new queries

### D2: OIDC/SSO JWT Validation

**Problem:** Auth middleware has OIDC placeholder comment. SSO config stored in SystemMetadata but no runtime JWT validation.

**Design:**

#### SSO Configuration Model (stored in SystemMetadata)

```json
{
  "enabled": true,
  "provider": "okta|azure-ad|google|auth0|keycloak|onelogin|generic-oidc",
  "issuerUrl": "https://example.okta.com",
  "clientId": "...",
  "clientSecret": "...(encrypted)",
  "allowedDomains": ["company.com"],
  "autoCreateUsers": true,
  "scopes": "openid profile email groups",
  "roleMapping": {
    "admin-group": "super_admin IAM group",
    "compliance-group": "compliance_admin IAM group"
  }
}
```

#### JWT Validation Middleware

Added to `adminauth.go` authentication chain (after session, before bootstrap):
- Extract `Authorization: Bearer <jwt>` or SSO session cookie
- JWKS endpoint auto-discovery from `issuerUrl/.well-known/openid-configuration`
- JWKS cache (1 hour TTL, background refresh)
- Validate: signature, expiration, audience (= clientId), issuer
- Extract claims: `sub`, `email`, `groups`/`roles`

#### User Auto-Creation / Mapping

- JWT validated → find NexusUser by email
- Not found + `autoCreateUsers=true` → create NexusUser with `canAccessControlPlane=true`
- Found → update `ssoLastAuthAt`, `lastSeenAt`
- SSO groups → sync to IAM Groups per `roleMapping` (differential: add/remove memberships)
- SSO groups NOT persisted on NexusUser table; synced to IAM on each login

#### Login Flow — OIDC Authorization Code Flow

- `GET /api/admin/auth/sso/authorize` → redirect to IdP
- IdP callback → `GET /api/admin/auth/sso/callback` → exchange code for tokens → validate id_token → create session
- Reuse existing SessionStore

#### Device Authentication (enterprise-login mode)

- Agent local OAuth callback server on `127.0.0.1:{random_port}`
- Construct OIDC Authorization URL with redirect_uri to local port
- Open system browser
- Receive callback, extract auth code
- `POST /api/agent/authenticate { deviceId, authCode }` → Control Plane validates, associates SSO identity with NexusUser, device status → ONLINE

#### Control Plane UI — SSO Settings Page (`/settings/sso`)

- Provider selection dropdown with preset templates (Okta, Azure AD, Google, Auth0, Keycloak, OneLogin, Generic OIDC)
- Template auto-fills issuer URL format, known scopes, help links
- Configuration form: Issuer URL, Client ID, Client Secret (password field, encrypted), Allowed Email Domains, Auto-Create Users toggle, Role Mapping (dynamic key-value table)
- "Test Connection" button → `POST /api/admin/settings/sso/test` (validates OIDC discovery endpoint reachable, client_id valid)
- Status indicator: recent SSO logins count, last validation time, JWKS cache status
- Login page: "Sign in with SSO" button when SSO enabled

**Files changed:**
- `packages/control-plane/internal/middleware/adminauth.go` — JWT validation layer
- `packages/control-plane/internal/middleware/jwt.go` — new: JWKS fetch + cache + validation
- `packages/control-plane/internal/handler/auth_routes.go` — SSO callback endpoint
- `packages/control-plane/internal/handler/sso.go` — new: OIDC flow
- `packages/control-plane/internal/config/config.go` — SSO config loading
- `packages/control-plane-ui/src/pages/settings/SSOSettings.tsx` — new: SSO config page
- `packages/control-plane-ui/src/components/auth/SSOLoginButton.tsx` — new
- `packages/control-plane-ui/src/api/sso.ts` — new: SSO API service

### D3: Device Management Observability + Dual-Mode Authentication

**Problem:** No fleet visibility. No per-device/per-user audit view. Agent enrollment doesn't collect OS user/system info. No way to require enterprise login for device activation.

#### Device Authentication Modes (System Config)

Stored in SystemMetadata as `device-auth-config`:

```json
{
  "mode": "os-identity | enterprise-login",
  "osIdentity": {
    "trustedDomains": ["company.local", "corp.example.com"],
    "usernameFormat": "email | samAccountName | upn",
    "autoApprove": true
  },
  "enterpriseLogin": {
    "provider": "sso | local",
    "ssoConfigId": "references-d2-sso-config",
    "loginUrl": "https://nexus.company.com/agent-auth",
    "tokenTTLDays": 90,
    "reAuthIntervalDays": 30
  }
}
```

**Mode 1: `os-identity`**
```
Agent starts → collect OS user info → POST /api/agent/enroll { csr, token, deviceInfo, osIdentity }
  → Control Plane: validate token, match/create NexusUser by (osUsername, osDomain)
  → trustedDomains check (if configured)
  → Sign cert, device status → ONLINE → device immediately operational
```

**Mode 2: `enterprise-login`**
```
Agent starts → collect OS user info → POST /api/agent/enroll { csr, token, deviceInfo, osIdentity }
  → Control Plane: validate token, sign cert, device status → PENDING_AUTH (restricted mode)
  → Agent enters restricted mode (heartbeat + config sync only; no traffic interception or observe-only hooks)
  → GUI shows "Sign In Required"
  → User clicks login → browser opens OIDC URL → auth code → POST /api/agent/authenticate
  → Control Plane validates, associates SSO identity with NexusUser, device → ONLINE
  → Token expires (tokenTTLDays) → device back to PENDING_AUTH, re-login required
```

**Heartbeat auth status directives:** `valid` / `expiring_soon` / `expired` / `revoked`

**User change detection:** Agent monitors `os/user.Current()` changes (e.g., macOS fast user switching), triggers immediate heartbeat with new user info.

#### Agent Sysinfo Collection

New `packages/agent/core/sysinfo/` module with platform-specific implementations:

| Info | macOS | Windows | Linux |
|------|-------|---------|-------|
| OS User | `os/user.Current()` | `os/user.Current()` | `os/user.Current()` |
| Full Name | `dscl . -read RealName` | `GetUserNameExW` | `/etc/passwd` GECOS |
| Hostname | `os.Hostname()` | `os.Hostname()` | `os.Hostname()` |
| OS Version | `sw_vers` | `RtlGetVersion` | `/etc/os-release` |
| Serial | `ioreg IOPlatformSerialNumber` | `wmic bios` | `/sys/class/dmi/id/product_serial` |
| Machine ID | `IOPlatformUUID` | `MachineGuid` registry | `/etc/machine-id` |
| Local IPs | `net.Interfaces()` | `net.Interfaces()` | `net.Interfaces()` |
| MAC Addresses | `net.Interfaces()` | `net.Interfaces()` | `net.Interfaces()` |
| Domain | `dsconfigad -show` | `NetGetJoinInformation` | realm/sssd check |

#### Control Plane API Extensions

**User dimension (new):**

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/api/admin/agent-users` | User list (device count, online count, last active) |
| GET | `/api/admin/agent-users/:id` | User detail + all associated devices |
| PUT | `/api/admin/agent-users/:id` | Update user info (department, email, status) |
| GET | `/api/admin/agent-users/:id/devices` | All devices for this user |
| GET | `/api/admin/agent-users/:id/audit` | Cross-device audit events for this user |
| POST | `/api/admin/agent-users/:id/suspend` | Suspend user (all devices → REVOKED) |
| POST | `/api/admin/agent-users/:id/activate` | Reactivate user |

**Device dimension (enhanced):**

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/api/admin/agent-devices/:id` | Enhanced: full sysinfo + associated user info |
| GET | `/api/admin/agent-devices/:id/audit` | Device audit events (filter audit_event by deviceId) |
| GET | `/api/admin/agent-devices/:id/config` | Device's current effective config snapshot |
| GET | `/api/admin/agent-devices/:id/timeline` | Device event timeline (enroll, heartbeat changes, config changes, anomalies) |
| POST | `/api/admin/agent-devices/:id/reassign` | Reassign device to another user (device ownership transfer) |

**Device auth config:**

| Method | Path | Purpose |
|--------|------|---------|
| PUT | `/api/admin/settings/device-auth` | Configure device auth mode |
| GET | `/api/admin/settings/device-auth` | Read device auth mode config |
| POST | `/api/agent/authenticate` | Device identity verification (SSO code or username/password) |
| GET | `/api/agent/auth-status` | Query current device auth status and expiry |
| POST | `/api/admin/agent-users/:id/merge` | Manually merge two user records (OS + SSO identity) |
| POST | `/api/admin/agent-devices/:id/force-reauth` | Force device re-authentication |

#### Control Plane UI Pages

**Fleet Overview Dashboard** — `/fleet`
- Total devices / online / offline / revoked
- Total users / active users
- OS distribution pie chart
- Agent version distribution (identify devices needing updates)
- Recently registered devices list
- Anomaly alerts (long-offline, audit queue backlog, cert expiring)

**User List** — `/fleet/users`
- Table: username, full name, department, device count, online devices, last active, status
- Search/filter by username, department, status
- Bulk operations: suspend, group assignment

**User Detail** — `/fleet/users/:id`
- User info card
- Device list (each: hostname, OS, IP, online status, agent version, last heartbeat)
- Cross-device audit event timeline
- Compliance stats: hook block count, data classification distribution

**Device Detail** — `/fleet/devices/:id`
- Device info card (hostname, OS/version/arch, serial, machine ID, IPs, MACs)
- Associated user + device group
- Runtime status (agent version, uptime, memory, audit queue depth, config version)
- Current effective config viewer (policy rules + hook configs, read-only)
- Device audit event list (filterable by hookDecision)
- Device event timeline (enrollment → config changes → status changes → anomalies)

**Settings: Device Management** — `/settings/device-auth`
- Authentication Mode toggle (os-identity / enterprise-login)
- os-identity sub-config: Trusted Domains, Username Format dropdown, Auto Approve toggle
- enterprise-login sub-config: Provider selection (reuse D2 SSO config / Local Auth), Token TTL, Re-auth Interval

**Files changed:**
- `packages/agent/core/sysinfo/` — new: cross-platform sysinfo collection
- `packages/agent/core/sysinfo/sysinfo_darwin.go` — macOS implementation
- `packages/agent/core/sysinfo/sysinfo_windows.go` — Windows implementation
- `packages/agent/core/sysinfo/sysinfo_linux.go` — Linux implementation
- `packages/agent/core/security/enrollment/enroll.go` — include deviceInfo
- `packages/agent/core/heartbeat/sender.go` — include system info fields
- `packages/agent/core/gateway/client.go` — update API request structs
- `packages/agent/core/sync/statusapi/server.go` — add AUTHENTICATE command
- `packages/control-plane/internal/handler/agent_api.go` — receive/store new fields, authenticate endpoint
- `packages/control-plane/internal/handler/fleet.go` — new: fleet/user/device APIs
- `packages/control-plane/internal/store/fleet_queries.go` — new: fleet SQL queries
- `packages/control-plane-ui/src/pages/fleet/` — new: Fleet Dashboard, User List, User Detail, Device Detail
- `packages/control-plane-ui/src/pages/settings/DeviceAuthSettings.tsx` — new
- `packages/control-plane-ui/src/api/fleet.ts` — new: fleet API service

---

## 7. Unified Pipeline Architecture

### Target State Summary

| Layer | Responsibility | Shared Code |
|-------|---------------|-------------|
| **Pre-processing** (differentiated) | Auth/routing/filtering | Service-specific |
| **Content extraction** (unified) | Body parsing and normalization | `shared/traffic` Adapter + DomainSnapshot |
| **Compliance hooks** (unified) | Request hooks → Response hooks | `shared/compliance` Pipeline + `shared/hooks` Registry |
| **Audit** (unified) | Unified event recording | `audit_event` table + `hooksPipeline` JSONB |

### Hook Pipeline Consistency

All three services execute the same hooks in the same order with the same decision semantics:

- **Request stage:** PII detector, keyword filter, content safety, rate limiter, request size validator, IP access filter, webhook forward (AI Gateway only), custom hooks
- **Response stage:** quality checker, content safety, PII detector, custom hooks
- **Decision merging:** first REJECT_HARD wins; any REJECT_SOFT without REJECT_HARD → REJECT_SOFT; all APPROVE → APPROVE
- **Data classification:** highest across all hooks

### Differences (by design)

| Aspect | AI Gateway | Compliance Proxy | Agent |
|--------|-----------|-----------------|-------|
| Pre-processing | VK auth, routing, quota | Domain/path filter, access control | Domain/path filter, policy engine |
| MODIFY decisions | Supported | Downgraded to APPROVE | Downgraded to APPROVE |
| Pipeline execution | Sequential | Parallel (request), sequential configurable | Sequential |
| Response blocking | Yes (always) | Configurable (block/observe) | Yes (always) |
| Streaming inspection | Holdback + checkpoint | Holdback + checkpoint | Holdback + checkpoint |
| Local-only hooks | webhook-forward, quality-checker | None | None |

---

## 8. Cross-Epoch Dependencies

```
Epoch A (Infrastructure)
  ├── A1: IAM Redis cache
  ├── A2: Config invalidation retry
  ├── A3: Shared rate limiter Redis mode
  └── A4: shared/store + shared/access ──────────┐
                                                  │
Epoch B (Extraction Unification)                  │
  ├── B1: New provider adapters                   │
  ├── B2: Compliance proxy migration ◄────────────┘ (depends on A4)
  ├── B3: Deprecate ExtractorRegistry ◄── (depends on B2)
  └── B4: Agent config sync merge
                    │
Epoch C (Compliance Completion)
  ├── C1: Response hook blocking ◄──────── (depends on B2 for unified extractors)
  ├── C2: macOS NE IPC extension ◄──────── (depends on B1 for adapters)
  ├── C3: Agent response pipeline ◄──────── (depends on B1 for adapters)
  └── C4: Integration tests ◄──────────── (depends on C2, C3)
                    │
Epoch D (Data Model)
  ├── D0: NexusUser unified model
  ├── D1: Unified audit table ◄──────────── (depends on D0 for nexusUserId)
  ├── D2: OIDC/SSO ◄────────────────────── (depends on D0 for NexusUser)
  └── D3: Device management ◄───────────── (depends on D0, D1, D2)
```

---

## 9. Documentation Updates

Every epoch must update relevant documentation:

### Epoch A
- `docs/ops/redis.md` — Redis failure degradation behavior per component
- `docs/dev/architecture.md` — IAM cache architecture, rate limiter distributed mode

### Epoch B
- `docs/dev/architecture.md` — unified content extraction architecture
- `docs/dev/project-structure.md` — new shared/traffic/adapters packages

### Epoch C
- `docs/dev/architecture.md` — three traffic paths with full hook pipeline (request + response)
- `docs/product/` — full-platform unified compliance
- `docs/ops/` — NE IPC troubleshooting guide, response blocking configuration

### Epoch D
- `docs/dev/architecture.md` — NexusUser model, unified audit, SSO auth flow, device auth modes
- `docs/product/` — device management features, SSO support, fleet dashboard
- `docs/ops/` — SSO configuration guide, audit table migration runbook, device auth mode setup
