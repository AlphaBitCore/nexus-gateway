# SIEM Integration Gaps — Design Spec

**Date:** 2026-04-15
**Status:** Draft
**Scope:** Close 4 implementation gaps in the existing SIEM forwarding system

## Background

The SIEM integration has two forwarding paths:

1. **Compliance-proxy forwarder** (`packages/compliance-proxy/internal/siem/`) — real-time per-event forwarding from the proxy data plane
2. **Control-plane bridge** (`packages/control-plane/internal/siem/`) — polling-based forwarding from PostgreSQL

Both paths work end-to-end for JSON output. However, four gaps exist between what the UI exposes and what the backend actually implements.

## Gaps

| # | Gap | Current State | Target State |
|---|-----|---------------|--------------|
| 1 | Event type filtering | UI stores `eventTypes[]`; bridge ignores it | Both paths filter events by type |
| 2 | Format conversion | UI offers JSON/CEF/Syslog; only JSON is sent | All three formats implemented |
| 3 | Spool replay | Failed batches written to NDJSON; no replay tool | CLI subcommand to replay spool files |
| 4 | Admin audit → SIEM | `AdminAuditLog` table not read by bridge; login/logout not audited | Bridge polls both tables; auth events audited |

## Design Decision: traffic_event scope

The `traffic_event` table contains **every AI API request** — high volume, not suitable for full SIEM forwarding. The bridge will only query **security-relevant** rows from `traffic_event`:

```sql
WHERE hook_decision = 'block'
   OR hook_reason_code IN ('rate_limited', 'budget_exceeded')
```

This maps to the three `proxy.*` event types in the UI. Normal (allowed) proxy traffic stays out of SIEM — it belongs in analytics, not security monitoring.

---

## Gap 1: Event Type Filtering

### Event Type Classification

The UI defines 14 event types. Each maps to a source table and filter condition:

**From `traffic_event`** (security subset only):

| Event Type | Condition |
|---|---|
| `proxy.request_rejected` | `hook_decision = 'block'` |
| `proxy.rate_limited` | `hook_reason_code = 'rate_limited'` |
| `proxy.budget_exceeded` | `hook_reason_code = 'budget_exceeded'` |

**From `AdminAuditLog`** (gap 4 enables these):

| Event Type | Condition |
|---|---|
| `auth.login_success` | `action = 'login'` |
| `auth.login_failure` | `action = 'login_failed'` |
| `auth.key_created` | `action = 'create' AND entityType = 'apiKey'` |
| `auth.key_revoked` | `action = 'delete' AND entityType = 'apiKey'` |
| `iam.policy_changed` | `entityType = 'iamPolicy'` |
| `iam.group_changed` | `entityType IN ('iamGroup', 'iamGroupMember')` |
| `iam.privilege_escalation_attempt` | Reserved — not implemented this iteration |
| `credential.accessed` | `action = 'export' AND entityType = 'credential'` |
| `credential.rotated` | `action = 'update' AND entityType = 'credential'` |
| `config.changed` | `entityType = 'settings' OR entityType = 'siemConfig'` |
| `config.rollback` | `action = 'rollback'` |

### Implementation

**New file: `packages/control-plane/internal/siem/classify.go`**

```go
// ClassifyTrafficEvent returns the SIEM event type for a traffic_event row,
// or "" if the event does not map to any known type.
func ClassifyTrafficEvent(evt Event) string

// ClassifyAdminEvent returns the SIEM event type for an AdminAuditLog row,
// or "" if the event does not map to any known type.
func ClassifyAdminEvent(evt Event) string

// FilterByEventTypes returns only events whose classified type is in the
// allowed set. If allowedTypes is empty, all events pass (backward compat).
func FilterByEventTypes(events []Event, allowedTypes []string) []Event
```

**Changes to `bridge.go`**:
- `BridgeConfig` gains `EventTypes []string` (loaded from `siem.config`)
- After querying both tables, call `FilterByEventTypes` before sending to sink
- The `queryEvents` method adds the security-subset WHERE clause

**Changes to compliance-proxy `forwarder.go`**:
- `ForwarderConfig` gains `EventTypes []string`
- `Enqueue()` checks classification before queuing (if EventTypes is set)
- Empty EventTypes = forward all (current behavior preserved)

---

## Gap 2: Format Conversion (CEF / Syslog)

### Formatter Interface

**New file: `packages/control-plane/internal/siem/formatter.go`**

```go
// Formatter converts a batch of events into a wire format.
type Formatter interface {
    // ContentType returns the HTTP Content-Type header value.
    ContentType() string
    // FormatBatch encodes a batch of events.
    FormatBatch(events []Event) ([]byte, error)
}
```

Three implementations in the same file:

#### JSONFormatter (existing behavior)
- `Content-Type: application/json`
- Output: JSON array `[{...}, {...}]`

#### CEFFormatter
- `Content-Type: text/plain`
- One line per event, newline-separated
- Format: `CEF:0|NexusGateway|ControlPlane|1.0|{eventType}|{summary}|{severity}|{extensions}`
- Severity mapping: `auth.login_failure` → 7, `proxy.request_rejected` → 5, `iam.*` → 6, default → 3
- Extensions: `src={sourceIp} dst={targetHost} act={action} msg={hookReason} suser={actorLabel} rt={timestamp}`

#### SyslogFormatter
- `Content-Type: text/plain`
- One line per event, newline-separated
- Format: RFC 5424 — `<{pri}>1 {timestamp} nexus-gateway control-plane - {eventType} - {message}`
- Priority: facility=local0 (16), severity derived from event type
- Structured data: `[nexus@0 eventType="{type}" source="{source}" actor="{actor}"]`

### Integration

**Control-plane bridge** (`sink.go`):
- `HTTPSink` constructor takes a `Formatter` parameter
- `Send()` calls `formatter.FormatBatch()` instead of `json.Marshal()`
- Sets `Content-Type` from `formatter.ContentType()`

**Compliance-proxy sinks** (`sinks.go`):
- Same pattern: `HTTPSink` and `FileSink` accept a `Formatter`
- `CommandSink` always uses JSON (temp file is JSON regardless of format)

**Compliance-proxy formatter**: The compliance-proxy uses `audit.AuditEvent` (not `Event = map[string]any`). Create a parallel `packages/compliance-proxy/internal/siem/formatter.go` with the same three formatters adapted for `audit.AuditEvent`.

---

## Gap 3: Spool Replay

### CLI Subcommand

Add `replay` subcommand to `packages/compliance-proxy/cmd/compliance-proxy/`:

```
compliance-proxy replay [flags]

Flags:
  --spool-dir string    Directory containing spool-*.ndjson files (required)
  --sink string         Sink type: http | file | command (default "http")
  --url string          HTTP sink URL (required for http sink)
  --header key=value    HTTP headers (repeatable)
  --file-path string    File sink output path (required for file sink)
  --batch-size int      Events per batch (default 100)
  --dry-run             Parse and validate without sending
```

### Logic

1. Glob `*.ndjson` in spool-dir, sort by filename (lexicographic = chronological since filenames include timestamps)
2. For each file:
   a. Read line by line, unmarshal each JSON line into `audit.AuditEvent`
   b. Accumulate into batches of `--batch-size`
   c. Send each batch via configured sink
   d. On success of all batches: delete the file
   e. On failure: log error, stop processing, exit non-zero
3. `--dry-run`: parse all files, report count and any malformed lines, do not send

### File Location

New file: `packages/compliance-proxy/cmd/compliance-proxy/replay.go`

Reuses existing sink implementations from `packages/compliance-proxy/internal/siem/sinks.go`.

---

## Gap 4: Admin Audit Events → SIEM

### 4a. Add Login/Logout Audit Logging

**Problem**: `AuthHandler` has no `*audit.Writer`; login, logout, and refresh are not audited.

**Changes to `auth_routes.go`**:

Add `Audit *audit.Writer` field to `AuthHandler` struct:

```go
type AuthHandler struct {
    DB       *store.DB
    Sessions *auth.SessionStore
    Audit    *audit.Writer  // NEW
}
```

Wire it in `main.go` where `AuthHandler` is constructed.

**Login success** (after session creation):
```go
ae := audit.Entry{
    ActorID:    user.ID,
    ActorLabel: user.DisplayName,
    ActorRole:  "super_admin",
    SourceIP:   c.RealIP(),
    Action:     "login",
    EntityType: "session",
    EntityID:   sessionID,
    AfterState: map[string]any{"method": "password", "userId": user.ID},
}
h.Audit.Log(ctx, ae)
```

**Login failure** (after invalid credentials):
```go
ae := audit.Entry{
    ActorID:    "anonymous",
    ActorLabel: req.Email,
    SourceIP:   c.RealIP(),
    Action:     "login_failed",
    EntityType: "session",
    AfterState: map[string]any{"method": "password", "email": req.Email, "reason": "invalid_credentials"},
}
h.Audit.Log(ctx, ae)
```

Same pattern for API key login success/failure.

**Logout**:
```go
ae := audit.Entry{
    ActorID:    aa.KeyID,  // from middleware context if available
    ActorLabel: aa.KeyName,
    SourceIP:   c.RealIP(),
    Action:     "logout",
    EntityType: "session",
    EntityID:   sessionID,
}
h.Audit.Log(ctx, ae)
```

**Refresh**: Not audited (too noisy, auto-triggered by frontend timer). Can be added later if needed.

### 4b. Bridge Polls AdminAuditLog

**Changes to `bridge.go`**:

Add a second query method and checkpoint:

```go
const adminCheckpointKey = "siem.bridge.admin_checkpoint"

func (b *Bridge) queryAdminEvents(ctx context.Context, since time.Time) ([]Event, time.Time, error) {
    rows, err := b.pool.Query(ctx, `
        SELECT id, timestamp,
               "actorId", "actorLabel", "actorRole",
               "sourceIp", action, "entityType", "entityId",
               "beforeState", "afterState"
        FROM "AdminAuditLog"
        WHERE timestamp > $1
        ORDER BY timestamp ASC
        LIMIT $2
    `, since, b.cfg.BatchSize)
    // ... scan into Event maps with source="admin" ...
}
```

**Updated `Poll()` flow**:

1. Load both checkpoints (traffic + admin)
2. Query `traffic_event` (security subset only)
3. Query `AdminAuditLog`
4. Classify all events → assign `eventType` field to each Event map
5. Filter by `cfg.EventTypes` (if non-empty)
6. Merge into single batch, send to sink
7. Update both checkpoints independently (only if their respective queries returned data)

---

## Files Changed

| File | Change |
|------|--------|
| `packages/control-plane/internal/siem/bridge.go` | Add admin query, security-subset WHERE, event type filtering, dual checkpoints |
| `packages/control-plane/internal/siem/sink.go` | HTTPSink takes Formatter; uses it in Send() |
| `packages/control-plane/internal/siem/classify.go` | **NEW** — event classification + filtering |
| `packages/control-plane/internal/siem/formatter.go` | **NEW** — JSON/CEF/Syslog formatters |
| `packages/control-plane/internal/siem/bridge_test.go` | **NEW** — tests for classification, filtering, admin query |
| `packages/control-plane/internal/siem/formatter_test.go` | **NEW** — tests for CEF/Syslog output |
| `packages/control-plane/internal/handler/admin_siem.go` | Pass Format to bridge config |
| `packages/control-plane/internal/handler/auth_routes.go` | Add Audit field; log login/logout/failure |
| `packages/control-plane/cmd/control-plane/main.go` | Wire Audit into AuthHandler; pass EventTypes+Format to bridge |
| `packages/compliance-proxy/internal/siem/forwarder.go` | Add EventTypes filtering in Enqueue |
| `packages/compliance-proxy/internal/siem/sinks.go` | HTTPSink/FileSink accept Formatter |
| `packages/compliance-proxy/internal/siem/formatter.go` | **NEW** — JSON/CEF/Syslog for audit.AuditEvent |
| `packages/compliance-proxy/internal/siem/formatter_test.go` | **NEW** — formatter tests |
| `packages/compliance-proxy/internal/siem/classify.go` | **NEW** — classification for AuditEvent |
| `packages/compliance-proxy/cmd/compliance-proxy/replay.go` | **NEW** — replay subcommand |
| `packages/compliance-proxy/cmd/compliance-proxy/replay_test.go` | **NEW** — replay tests |

## Not In Scope

- `iam.privilege_escalation_attempt` — requires defining what "escalation" means; deferred
- Session refresh audit — too noisy (auto-triggered by frontend)
- Hot-reload of SIEM config — requires restart (existing behavior, documented)
- UI changes — the existing UI already has all the right controls
- DB schema changes — none needed

## Risks

- **CEF/Syslog field mapping** — specific SIEM vendors may expect slightly different field names. The standard formats are a good starting point; operators can adjust via SIEM-side transforms.
- **AdminAuditLog volume** — low (human-triggered), no concern for bridge polling overhead.
- **Dual checkpoint** — if admin checkpoint advances but traffic checkpoint fails (or vice versa), events from the failed source will be retried next cycle. This is correct behavior.
