# PII Redaction & Traffic Event Payload Storage

**Date**: 2026-04-14
**Status**: Draft
**Scope**: PiiDetector hook refactor + traffic_event_payload table + redaction page removal

---

## Problem Statement

The current codebase has two overlapping PII detection/redaction systems:

1. **PiiDetector hook** (`shared/hooks/pii_detector.go`) — detects PII and rejects traffic
2. **Redactor engine** (`shared/compliance/redact.go`) — detects PII and replaces with placeholders, but is never wired into any runtime path

Additionally, the `/security/redaction` UI page saves config to `system_metadata` that no service reads, and `traffic_event` does not store request/response bodies — a requirement for enterprise audit compliance.

## Design Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| PII handling | Unify into PiiDetector hook | Same regex patterns, single config point |
| Redactor engine | Remove | Capabilities merged into PiiDetector |
| `/security/redaction` UI page | Remove | PII config managed via hooks admin UI |
| Body storage | New `traffic_event_payload` child table | Bodies are large, queried infrequently (detail view only) |
| Body redaction before storage | Ingress-dependent | ai-gateway uses pipeline output; compliance-proxy runs rules separately |
| `allowModify` in compliance-proxy | Keep `false` | Transparent proxy should not modify traffic; redact → reject (fail-closed) |

---

## Subsystem 1: PiiDetector Refactor

### Current State

- `PiiDetector` supports `action: reject_hard | reject_soft`
- `Decision` enum: `Approve`, `RejectHard`, `RejectSoft`, `Abstain`
- Pipeline does not recognize a `Modify` decision
- Built-in PII types: `email`, `phone`, `ssn`, `credit_card`
- Redactor has additional: `bearer_token`

### Changes

#### 1.1 Hook Types (`shared/hooks/types.go`)

- Add `Modify` to `Decision` constants
- Add `ModifiedContent []ContentBlock` field to `HookResult`

Decision priority (highest to lowest):
```
REJECT_HARD > REJECT_SOFT > MODIFY > APPROVE > ABSTAIN
```

#### 1.2 PiiDetector (`shared/hooks/pii_detector.go`)

New config shape:
```json
{
  "types": ["email", "phone", "ssn", "credit_card", "bearer_token"],
  "customPatterns": [
    { "name": "internal_id", "pattern": "ACCT-\\d{8}", "replacement": "[REDACTED_ID]" }
  ],
  "action": "reject_hard | reject_soft | redact"
}
```

- Add `bearer_token` to `builtinPII` (from Redactor's `DefaultRedactionRules`)
- Each built-in type gets a default replacement string (e.g., `[REDACTED_EMAIL]`)
- `customPatterns` gains optional `replacement` field
- When `action=redact`:
  - Clone `input.Content` blocks
  - Apply regex replacements on each block's Text
  - Return `Decision: Modify` + `ModifiedContent` with replaced content
  - If no PII found, return `Approve` as usual

#### 1.3 Pipeline (`shared/compliance/pipeline.go`)

In `Execute()`, handle `Modify` decision:
- `allowModify=true` (ai-gateway): replace `input.Content` with `ModifiedContent` for subsequent hooks
- `allowModify=false` (compliance-proxy): downgrade `Modify` to `RejectHard` (fail-closed, existing behavior for unknown decisions)

Decision merging update:
- `REJECT_HARD` wins globally (unchanged)
- `REJECT_SOFT` wins over `MODIFY` (unchanged principle)
- `MODIFY`: accumulate content changes across multiple hooks (each hook modifies the output of the previous)
- If any hook rejects, modifications are discarded

#### 1.4 Deletions

- Delete `packages/shared/compliance/redact.go` and `redact_test.go`
- Delete `packages/control-plane-ui/src/pages/security/redaction/` directory
- Remove GET/PUT `/api/admin/redaction` routes from `admin_extras.go`
- Remove frontend route for `/security/redaction`
- Remove `system_metadata` key `"redaction.config"` from seed data

---

## Subsystem 2: Traffic Event Payload Storage

### 2.1 New Table

```sql
CREATE TABLE traffic_event_payload (
    traffic_event_id UUID PRIMARY KEY REFERENCES traffic_event(id) ON DELETE CASCADE,
    request_body     JSONB,
    response_body    JSONB,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

- 1:1 relationship with `traffic_event`
- `traffic_event_id` is both PK and FK (no separate id column)
- `ON DELETE CASCADE` — payload deleted when parent event deleted
- Each body field independently nullable (can store one without the other)

### 2.2 System Configuration

Stored in `system_metadata`, key = `"audit.payload"`:

```json
{
  "storeRequestBody": true,
  "storeResponseBody": false
}
```

Default when key is missing: `{ storeRequestBody: false, storeResponseBody: false }` (store nothing).

### 2.3 Write Flow

```
AuditEmitter receives body + pipeline result
  │
  ├─ Check audit.payload config (cached, refreshed via Redis pub/sub)
  │
  ├─ storeRequestBody = true?
  │    ├─ ai-gateway (allowModify=true):
  │    │    └─ Store pipeline output body (already redacted by MODIFY)
  │    ├─ compliance-proxy (allowModify=false):
  │    │    └─ Apply PiiDetector rules to original body → store redacted copy
  │    └─ false: skip
  │
  ├─ storeResponseBody = true?
  │    └─ Same logic as above
  │
  └─ INSERT INTO traffic_event_payload
```

The compliance-proxy reuses PiiDetector's compiled regex patterns (from hook config) for storage-time redaction. This is not a separate Redactor — it reads the same PiiDetector hook config and applies the replacement patterns.

If no PiiDetector hook is configured (or `action` is not `redact`), the body is stored as-is — no implicit redaction. The platform provides the capability; the admin decides the policy.

### 2.4 Config Propagation

Same pattern as existing hook config:
1. Control Plane writes `system_metadata` key `"audit.payload"`
2. Control Plane publishes Redis invalidation on `"nexus:config:shared"` (topic: `"audit"`)
3. Data plane services receive invalidation → reload config → atomic swap

### 2.5 Query API

```
GET /api/admin/traffic-events/:id
```

Response includes payload via LEFT JOIN:
```json
{
  "id": "...",
  "targetHost": "api.openai.com",
  "hookDecision": "APPROVE",
  "requestBody": { "model": "gpt-4", "messages": [...] },
  "responseBody": null
}
```

- `requestBody` / `responseBody` present when stored, `null` when not stored
- No special "not configured" indicator — `null` is self-explanatory

### 2.6 UI Detail View

- `requestBody != null` → render body content block
- `requestBody == null` → do not render (no placeholder, no message)
- Same for `responseBody`

---

## Implementation Order

These two subsystems are independent but have a runtime dependency (subsystem 2 uses subsystem 1's rules for storage-time redaction).

Recommended order:
1. **Subsystem 1** — PiiDetector refactor (types → pipeline → pii_detector → deletions)
2. **Subsystem 2** — Payload storage (migration → config → write flow → query API → UI)

---

## Files Affected

### Subsystem 1 (PiiDetector Refactor)
| Action | File |
|--------|------|
| Modify | `packages/shared/policy/hooks/types.go` |
| Modify | `packages/shared/policy/hooks/pii_detector.go` |
| Modify | `packages/shared/policy/hooks/pii_detector_test.go` |
| Modify | `packages/shared/compliance/pipeline.go` |
| Delete | `packages/shared/compliance/redact.go` |
| Delete | `packages/shared/compliance/redact_test.go` |
| Modify | `packages/control-plane/internal/handler/admin_extras.go` |
| Delete | `packages/control-plane-ui/src/pages/security/redaction/` |
| Modify | `packages/control-plane-ui/src/router.tsx` (or equivalent route config) |
| Modify | `packages/control-plane-ui/src/api/services/system.ts` |
| Modify | `packages/control-plane-ui/src/api/types.ts` |
| Modify | `tools/db-migrate/seed/seed.ts` (remove redaction.config seed) |

### Subsystem 2 (Payload Storage)
| Action | File |
|--------|------|
| Create | `tools/db-migrate/migrations/YYYYMMDD_traffic_event_payload/migration.sql` |
| Modify | `tools/db-migrate/schema.prisma` |
| Modify | `packages/compliance-proxy/internal/audit/types.go` |
| Modify | `packages/compliance-proxy/internal/audit/writer.go` |
| Modify | `packages/compliance-proxy/internal/audit/sql.go` |
| Modify | `packages/control-plane/internal/handler/admin_traffic.go` (or equivalent) |
| Modify | `packages/control-plane/internal/store/` (traffic event queries) |
| Modify | `packages/control-plane-ui/src/pages/` (traffic event detail page) |
| Modify | `packages/control-plane-ui/src/api/` (types + service for payload config) |
