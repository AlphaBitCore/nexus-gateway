# SIEM Event Taxonomy Redesign

**Date:** 2026-04-16
**Status:** Approved
**Scope:** Replace hardcoded SIEM event types with dynamic taxonomy derived from audit log data; rename `proxy.*` to `traffic.*`

## Goals

1. Replace the hardcoded 14-type classification with a dynamic `{entityType}.{action}` taxonomy that automatically covers all admin audit events
2. Rename `proxy.*` traffic event types to `traffic.*` to reflect they come from the full traffic pipeline (ai-gateway + compliance-proxy), not just the proxy
3. Frontend SIEM settings page dynamically loads event types from the backend instead of hardcoding
4. Zero maintenance — new audit events automatically appear as SIEM event types

## Non-Goals

- Forwarding successful traffic requests (`traffic.request_success`) or upstream errors to SIEM — too high volume, already in traffic_event table
- Backward compatibility with old event type names — this is a new system
- Translating event type names in i18n — they are technical identifiers matching the audit log

---

## 1. Event Type Taxonomy

### Traffic Events (from traffic_event table — fixed set)

| Event Type | Trigger |
|---|---|
| `traffic.rate_limited` | hook_decision=block, hook_reason_code=rate_limited |
| `traffic.budget_exceeded` | hook_decision=block, hook_reason_code=budget_exceeded |
| `traffic.request_blocked` | hook_decision=block, any other reason |

These are classified by both compliance-proxy and control-plane from traffic data. They are a fixed set — not derived from the database.

### Admin Audit Events (from AdminAuditLog table — dynamic)

**Format: `{entityType}.{action}`**

Derived directly from the `entityType` and `action` columns of AdminAuditLog. Examples:

| Event Type | Description |
|---|---|
| `session.login` | Login success |
| `session.login_failed` | Login failure |
| `session.logout` | Logout |
| `apiKey.create` | API key created |
| `apiKey.delete` | API key revoked |
| `credential.create` | Credential created |
| `credential.update` | Credential updated/rotated |
| `credential.export` | Credential exported |
| `credential.delete` | Credential deleted |
| `iamPolicy.create` | IAM policy created |
| `iamPolicy.update` | IAM policy updated |
| `iamPolicy.delete` | IAM policy deleted |
| `iamGroup.create` | IAM group created |
| `iamGroup.update` | IAM group updated |
| `iamGroup.delete` | IAM group deleted |
| `iamGroupMember.create` | Member added to group |
| `iamGroupMember.delete` | Member removed from group |
| `routingRule.create` | Routing rule created |
| `routingRule.update` | Routing rule updated |
| `routingRule.delete` | Routing rule deleted |
| `hookConfig.create` | Hook created |
| `hookConfig.update` | Hook updated |
| `hookConfig.delete` | Hook deleted |
| `settings.update` | System settings changed |
| `ssoConfig.update` | SSO config changed |
| `siemConfig.update` | SIEM config changed |
| `provider.create` | Provider added |
| `provider.delete` | Provider removed |
| `virtualKey.create` | Virtual key created |
| `virtualKey.revoke` | Virtual key revoked |
| `agentDevice.create` | Device enrolled |
| `agentDevice.authenticate` | Device enterprise login |
| `nexusUser.create` | User created |
| `nexusUser.delete` | User deleted |
| ... | All 80+ combinations auto-generated |

The full list is not enumerated here — it is whatever exists in the AdminAuditLog table's distinct `(entityType, action)` pairs.

---

## 2. Backend Changes

### compliance-proxy `classify.go`

Rename constants only:
- `proxy.rate_limited` → `traffic.rate_limited`
- `proxy.budget_exceeded` → `traffic.budget_exceeded`
- `proxy.request_rejected` → `traffic.request_blocked`

### control-plane `classify.go`

**Replace the entire classification logic:**

```go
func ClassifyTrafficEvent(hookDecision, hookReasonCode string) string {
    if hookDecision != "block" {
        return ""
    }
    switch hookReasonCode {
    case "rate_limited":
        return "traffic.rate_limited"
    case "budget_exceeded":
        return "traffic.budget_exceeded"
    default:
        return "traffic.request_blocked"
    }
}

func ClassifyAdminEvent(action, entityType string) string {
    if action == "" || entityType == "" {
        return ""
    }
    return entityType + "." + action
}
```

No switch/case mapping. Every audit event with non-empty action+entityType gets classified.

### control-plane SIEM bridge (`bridge.go`)

No logic changes. The bridge already reads all AdminAuditLog rows and calls ClassifyAdminEvent. Now every row produces a non-empty classification (instead of most returning "").

### New API: `GET /api/admin/settings/siem/event-types`

Returns all available SIEM event types for the settings UI.

```sql
SELECT DISTINCT "entityType", "action"
FROM "AdminAuditLog"
WHERE "entityType" != '' AND "action" != ''
ORDER BY "entityType", "action"
```

Response:
```json
{
  "eventTypes": [
    { "type": "traffic.rate_limited", "group": "traffic" },
    { "type": "traffic.budget_exceeded", "group": "traffic" },
    { "type": "traffic.request_blocked", "group": "traffic" },
    { "type": "session.login", "group": "session" },
    { "type": "session.login_failed", "group": "session" },
    { "type": "session.logout", "group": "session" },
    { "type": "credential.create", "group": "credential" },
    { "type": "credential.export", "group": "credential" },
    ...
  ]
}
```

Traffic types are always prepended (fixed). Admin types are dynamic from the query.

Route: registered on `admin` group with `admin:ReadSettings` IAM permission.

### Store method: `ListDistinctAuditEventTypes`

```go
type AuditEventTypePair struct {
    EntityType string
    Action     string
}

func (db *DB) ListDistinctAuditEventTypes(ctx context.Context) ([]AuditEventTypePair, error) {
    rows, err := db.Pool.Query(ctx, `
        SELECT DISTINCT "entityType", "action"
        FROM "AdminAuditLog"
        WHERE "entityType" != '' AND "action" != ''
        ORDER BY "entityType", "action"
    `)
    // ...scan and return
}
```

---

## 3. Frontend Changes

### `system.ts`

- Remove hardcoded `SiemEventType` union type
- Add `SiemEventTypeInfo` interface: `{ type: string; group: string }`
- Change `SiemConfig.eventTypes` from `SiemEventType[]` to `string[]`
- Add API method: `listSiemEventTypes()`

### `SettingsSiemTab.tsx`

- Remove hardcoded `ALL_EVENT_TYPES` array
- On load: call `listSiemEventTypes()` to fetch dynamic list
- Display grouped by `group` field:
  - **Traffic Events** group always at top (fixed 3 types)
  - Remaining groups sorted alphabetically by group name
  - Each group has a header and checkboxes for its event types
  - Each group has a "select all / deselect all" toggle
- Empty `eventTypes[]` (nothing checked) = forward all events (existing behavior preserved)

### i18n

- Remove old per-event-type i18n keys if any
- Add group-level label for "Traffic Events" group: `pages:settingsSiem.trafficGroup`
- Other groups display their entityType name directly (technical identifier)

---

## 4. File Change List

### Modified Files

| File | Changes |
|---|---|
| `packages/compliance-proxy/internal/siem/classify.go` | Rename `proxy.*` → `traffic.*` constants |
| `packages/control-plane/internal/siem/classify.go` | Replace switch/case with `entityType + "." + action`; rename traffic constants |
| `packages/control-plane/internal/siem/classify_test.go` | Update tests for new classification logic |
| `packages/control-plane/internal/store/admin_audit.go` or `traffic_event.go` | Add `ListDistinctAuditEventTypes()` |
| `packages/control-plane/internal/handler/admin_settings.go` | Add `ListSiemEventTypes` handler + route registration |
| `packages/control-plane-ui/src/api/services/system.ts` | Replace `SiemEventType` with `SiemEventTypeInfo`, add `listSiemEventTypes()` |
| `packages/control-plane-ui/src/pages/settings/SettingsSiemTab.tsx` | Dynamic event type loading, grouped display |
| `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/pages.json` | Remove old event type keys, add traffic group label |

### No New Files

All changes are modifications to existing files.

---

## 5. Testing

- **classify.go tests**: Verify `ClassifyAdminEvent("create", "credential")` returns `"credential.create"`; empty action/entityType returns `""`
- **classify.go tests**: Verify traffic classification with renamed constants
- **ListDistinctAuditEventTypes**: Query returns expected pairs from seeded data
- **API endpoint**: Returns traffic types + dynamic admin types
- **Frontend**: Event types load dynamically, grouped display, save/load round-trip
