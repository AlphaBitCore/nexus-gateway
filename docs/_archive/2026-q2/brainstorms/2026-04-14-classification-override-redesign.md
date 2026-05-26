# Classification Override Redesign

**Date:** 2026-04-14  
**Status:** Draft  
**Scope:** Redesign the data classification rules feature into a comprehensive classification override system with AND-condition rules, expanded field support, forced preview workflow, and single-record manual override.

---

## Context & Problem

The current Classification Rules feature (`/security/classification-rules`) has two categories of issues:

### Functional Bugs (existing code)

| # | Severity | Issue |
|---|----------|-------|
| 1 | P0 | Priority semantics reversed — UI says "higher priority wins" but backend applies rules sequentially so lowest priority (highest number) overwrites |
| 2 | P1 | `ListClassificationRules` error silently discarded (`admin_classification.go:194`) |
| 3 | P1 | `start > end` not validated — negative duration passes 90-day check, returns 0 matches silently |
| 4 | P1 | ILIKE wildcard injection — `%` and `_` in user values treated as SQL wildcards |
| 5 | P1 | No transaction wrapping — partial Apply failure leaves data in inconsistent state |
| 6 | P2 | Apply button has no confirmation dialog (Delete does) |
| 7 | P2 | COUNT/UPDATE race condition between preview and apply |
| 8 | P2 | Audit action logged as `"reset"` instead of `"apply"` |
| 9 | P2 | `provider` and `targetHost` map to identical SQL |

### Design Issues

1. **Partial overlap with hooks** — `hookReasonCode` and `hookDecision` fields re-classify based on hook output, but those hooks already set classification at request time. However, these fields have independent value as **filter criteria for override rules** (e.g., "where PII was detected on test hosts, downgrade to PUBLIC").
2. **Single-condition rules** — Each rule matches one field only. Real-world use cases require AND combinations (e.g., `targetHost = X AND modelUsed = gpt-4`).
3. **Limited field set** — Only 5 fields (with 2 duplicates), missing valuable dimensions like `modelUsed`, `department`, `source`, `userId`, `projectId`.
4. **No single-record override** — No way to correct classification on individual records.
5. **No before→after audit trail** — Apply logs rule count but not what changed per row.
6. **No forced preview** — Admin can Apply without previewing impact.

---

## Design

### Two Operation Modes

| | Batch Rules | Single-Record Manual |
|---|-------------|---------------------|
| Use case | Policy/strategy changes, bulk false-positive correction | Individual record correction |
| Conditions | 1-N AND conditions per rule | Specific record by ID |
| Direction | Upgrade and downgrade both allowed | Upgrade and downgrade both allowed |
| Conflict resolution | Multiple rules match same row → highest classification wins | N/A |
| Safety | Forced preview → confirmation dialog → execute | Must provide reason |
| Audit | Per-rule before→after distribution | Per-record before→after + reason |

### Supported Fields

Expanded from 5 (with duplicates) to a clean set derived from `traffic_event`:

| Category | Field Key | DB Column | Description |
|----------|-----------|-----------|-------------|
| **Target** | `targetHost` | `target_host` | AI provider hostname |
| **Target** | `path` | `path` | Request URL path |
| **Target** | `modelUsed` | `model_used` | AI model name |
| **Identity** | `department` | `department` | User department |
| **Identity** | `userId` | `user_id` | User identifier |
| **Identity** | `projectId` | `project_id` | Project identifier |
| **Source** | `source` | `source` | Traffic origin (vk/proxy/agent) |
| **Compliance** | `hookDecision` | `hook_decision` | Hook pipeline decision |
| **Compliance** | `hookReasonCode` | `hook_reason_code` | Hook reason code |

**Removed:** `provider` (duplicate of `targetHost`).

**Operators** (unchanged): `equals`, `contains`, `startsWith`.

**Wildcard escaping:** `%` and `_` in user-provided values must be escaped before ILIKE queries to prevent unintended wildcard matching.

### Data Model Changes

#### Replace: `data_classification_rule` → `classification_override_rule` + `classification_override_condition`

```
classification_override_rule
  id              UUID PK
  name            String
  description     String?
  enabled         Boolean (default true)
  classification  DataClassification (PUBLIC | INTERNAL | CONFIDENTIAL | RESTRICTED)
  created_at      DateTime
  updated_at      DateTime
  created_by      String?
  updated_by      String?

classification_override_condition
  id              UUID PK
  rule_id         UUID FK → classification_override_rule.id (CASCADE DELETE)
  field           String (from supported fields enum)
  operator        String (equals | contains | startsWith)
  value           String
  ordinal         Int (display order within rule)
```

**Removed from rule:** `priority`, `field`, `operator`, `value` (moved to conditions table).  
**No priority field:** Conflict resolution is "highest classification wins", not order-based.

#### New: `classification_override_log`

```
classification_override_log
  id                    UUID PK
  mode                  String (batch | manual)
  rule_id               UUID? (NULL for manual overrides)
  traffic_event_id      UUID FK → traffic_event.id
  previous_classification  String?
  new_classification       String
  reason                String? (required for manual, optional for batch)
  performed_by          String
  performed_at          DateTime
```

This table provides the full before→after audit trail per record. It enables:
- Compliance audit: "show me all classification changes in the last 30 days"
- Undo analysis: "what was this record's classification before the batch override?"

**Volume consideration:** For batch operations affecting thousands of rows, writing per-row log entries could be expensive. Two strategies:
- **v1 (simple):** For batch operations, write a single summary log entry (rule ID, time range, per-classification counts) instead of per-row entries. Manual overrides always write per-row entries.
- **v2 (full audit):** Write per-row entries using batch INSERT with a configurable threshold (e.g., if >10,000 rows affected, fall back to summary-only logging with a warning in the UI).

### Batch Apply Workflow

```
Step 1: Admin configures rules (CRUD)
  ↓
Step 2: Admin selects time window + clicks "Preview" (mandatory)
  ↓
Step 3: System executes dry-run, returns per-rule breakdown:
  ┌──────────────────────────────────────────────────────────────────┐
  │ Rule 1: targetHost = test-api.internal.com                      │
  │         AND hookReasonCode contains PII → PUBLIC                │
  │                                                                  │
  │   CONFIDENTIAL → PUBLIC:  488 rows  ⚠️ downgrade               │
  │   RESTRICTED → PUBLIC:     12 rows  ⚠️ downgrade               │
  │   Already PUBLIC:           0 rows                               │
  │                                                                  │
  │ Rule 2: modelUsed = gpt-4 AND department = finance → RESTRICTED │
  │                                                                  │
  │   (none) → RESTRICTED:  1,204 rows  ↑ new label                │
  │   INTERNAL → RESTRICTED:   67 rows  ↑ upgrade                  │
  │   Already RESTRICTED:      33 rows                               │
  │                                                                  │
  │ Total: 1,771 rows | Downgrades: 500 | Upgrades: 67 | New: 1,204│
  └──────────────────────────────────────────────────────────────────┘
  ↓
Step 4: Confirmation dialog (mandatory):
  "Confirm batch classification change?
   This will affect 1,771 records, including 500 downgrades.
   This operation is irreversible and will immediately affect
   compliance reports and SIEM data.
   [Cancel] [Confirm]"
  ↓
Step 5: Execute within a single database transaction
  - For each enabled rule, build AND-combined WHERE clause
  - Apply all rules; where multiple rules match same row, highest classification wins
  - Write per-row entries to classification_override_log
  - On any error, rollback entire transaction
  ↓
Step 6: Return execution results (same format as preview, with applied counts)
```

### Conflict Resolution: Highest Classification Wins

When multiple rules match the same `traffic_event` row:

```
Rule A: targetHost = api.openai.com → INTERNAL
Rule B: modelUsed = gpt-4 → RESTRICTED

Row matches both → final classification = RESTRICTED (higher rank)
```

Classification rank: `PUBLIC(0) < INTERNAL(1) < CONFIDENTIAL(2) < RESTRICTED(3)`

This eliminates the need for priority ordering and removes the P0 priority-reversal bug entirely.

**Edge case — conflicting downgrades:** If the row is currently RESTRICTED and two rules match:
- Rule A → PUBLIC
- Rule B → CONFIDENTIAL

Result: CONFIDENTIAL (highest among the rule outputs). The row is still downgraded from RESTRICTED, but to the least-aggressive downgrade among matching rules.

### Single-Record Manual Override

**API:** `PUT /api/admin/traffic-events/:id/classification`

```json
{
  "classification": "PUBLIC",
  "reason": "False positive — test environment PII detection"
}
```

- `reason` is required (non-empty string)
- Writes to `classification_override_log` with `mode = "manual"`
- No preview needed (single record, admin sees current value in UI)

**UI:** In the unified audit list, each row's classification badge becomes clickable → opens a small dialog showing current classification, dropdown for new value, required reason text field.

### SQL Generation for AND Conditions

For a rule with N conditions, generate:

```sql
-- Preview (count with before→after breakdown):
SELECT data_classification, COUNT(*) 
FROM traffic_event 
WHERE <condition_1> AND <condition_2> AND ... AND <condition_N>
  AND timestamp >= $start AND timestamp <= $end
GROUP BY data_classification

-- Apply:
UPDATE traffic_event 
SET data_classification = $target
WHERE <condition_1> AND <condition_2> AND ... AND <condition_N>
  AND timestamp >= $start AND timestamp <= $end
  AND (data_classification IS NULL 
       OR data_classification != $target)
```

Each condition clause is selected from a hardcoded template map (same approach as current code, but keyed by `field + operator`). Conditions are joined with `AND`. No string interpolation for field names or operators.

**Wildcard escaping** for `contains` and `startsWith` operators:

```go
// Escape ILIKE special characters in user value
escaped := strings.NewReplacer("%", "\\%", "_", "\\_").Replace(value)
```

SQL templates use `ILIKE '%' || $N || '%' ESCAPE '\'` for contains, `ILIKE $N || '%' ESCAPE '\'` for startsWith.

### Highest-Wins Application Strategy

When multiple rules match the same row, applying "highest wins" in SQL:

```sql
-- Step 1: For each rule, compute its candidate classification for each matching row
-- Step 2: For each row, take the MAX classification across all matching rules
-- Step 3: UPDATE only if the result differs from current value

-- Implementation: iterate rules in Go, build a map[rowID]highestClassification,
-- then do a single bulk UPDATE. This avoids sequential overwrites.
```

Alternative (simpler, acceptable for v1): apply rules sequentially sorted by classification rank ascending (PUBLIC first, RESTRICTED last). Since higher classifications run last, they naturally overwrite lower ones. This achieves "highest wins" without an in-memory map.

### Validation Fixes (from bug audit)

| Bug | Fix |
|-----|-----|
| Priority reversal | Eliminated — no priority field, highest classification wins |
| Error silently discarded | Check and return error from `ListClassificationRules` |
| `start > end` not validated | Add `if !end.After(start)` check, return 400 |
| ILIKE wildcard injection | Escape `%` and `_` with backslash, add `ESCAPE '\'` to ILIKE |
| No transaction | Wrap entire Apply in `BEGIN ... COMMIT`, rollback on error |
| No confirmation dialog | UI: Apply button triggers preview first, then confirmation dialog |
| Audit action "reset" | Change to `"batch_reclassify"` for batch, `"manual_reclassify"` for single |

### API Endpoints

```
# Rule CRUD (unchanged paths, updated payloads)
GET    /api/admin/classification-rules          → list rules with conditions
POST   /api/admin/classification-rules          → create rule with conditions
PUT    /api/admin/classification-rules/:id      → update rule with conditions
DELETE /api/admin/classification-rules/:id      → delete rule (cascades conditions)

# Batch apply (updated payload and response)
POST   /api/admin/classification-rules/apply    → preview or execute

# Single-record override (new)
PUT    /api/admin/traffic-events/:id/classification → manual override

# Override history (new)
GET    /api/admin/traffic-events/:id/classification-history → list changes for a record
```

### Apply Request/Response

**Request:**
```json
{
  "startTime": "2026-04-01T00:00:00Z",
  "endTime": "2026-04-14T23:59:59Z",
  "dryRun": true
}
```

**Response (preview):**
```json
{
  "dryRun": true,
  "period": { "start": "...", "end": "..." },
  "results": [
    {
      "ruleId": "uuid",
      "ruleName": "Test env PII false positives",
      "targetClassification": "PUBLIC",
      "breakdown": {
        "CONFIDENTIAL→PUBLIC": 488,
        "RESTRICTED→PUBLIC": 12,
        "unchanged": 0
      },
      "totalAffected": 500,
      "downgrades": 500,
      "upgrades": 0
    }
  ],
  "summary": {
    "totalAffected": 1771,
    "totalDowngrades": 500,
    "totalUpgrades": 67,
    "totalNewLabels": 1204
  }
}
```

### Rule CRUD Request

**Create/Update request:**
```json
{
  "name": "Test env PII false positives",
  "description": "Override false PII detections from test environment",
  "enabled": true,
  "classification": "PUBLIC",
  "conditions": [
    { "field": "targetHost", "operator": "equals", "value": "test-api.internal.com" },
    { "field": "hookReasonCode", "operator": "contains", "value": "PII" }
  ]
}
```

### UI Changes

1. **Rule list page** — Each rule row shows all its conditions (joined with "AND" badges)
2. **Rule create/edit dialog** — Dynamic condition rows with + / - buttons to add/remove conditions
3. **Preview & Apply panel** — Apply button triggers forced preview → results table with before→after breakdown → confirmation dialog → execute
4. **Unified audit list** — Classification badge becomes clickable → manual override dialog (classification dropdown + required reason field)

---

## Files Affected

### Database
- `tools/db-migrate/schema.prisma` — Add `ClassificationOverrideRule`, `ClassificationOverrideCondition`, `ClassificationOverrideLog`; mark old `DataClassificationRule` for removal
- New Prisma migration

### Backend (Go)
- `packages/control-plane/internal/store/classification_rule.go` — Rewrite: new table queries, AND-condition SQL builder, transaction-wrapped Apply, override log writes
- `packages/control-plane/internal/handler/admin_classification.go` — Updated CRUD payloads (conditions array), new preview response format, new single-record override endpoint, validation fixes
- `packages/control-plane/internal/handler/admin_routes.go` — Register new route for single-record override

### Frontend (React)
- `packages/control-plane-ui/src/pages/security/ClassificationRulesPage.tsx` — Rewrite: multi-condition rule form, forced preview workflow, confirmation dialog, updated results table
- `packages/control-plane-ui/src/api/services/classification-rules.ts` — Updated types and API calls
- Unified audit list component — Add classification override button/dialog

---

## Out of Scope

- Scheduled/automatic rule application (cron) — rules are applied on-demand only
- Rule versioning or change history — rule CRUD is audited via existing admin audit log
- Bulk undo — `classification_override_log` enables future undo capability but no UI for it in this iteration
- Real-time classification from rules (hook integration) — rules remain a post-hoc governance layer
