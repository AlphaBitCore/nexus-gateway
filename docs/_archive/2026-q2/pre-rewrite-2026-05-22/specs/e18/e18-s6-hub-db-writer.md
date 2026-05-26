# E18 — Story 6: Hub DB Writer INSERT Update

## Context

`packages/nexus-hub/internal/jobs/consumer/traffic.go` is the single point that inserts rows into `traffic_event`. It consumes from three MQ queues (`nexus.event.ai-traffic` / `nexus.event.compliance` / `nexus.event.agent`) and batch-inserts. This story extends the INSERT to cover the three new columns and updates the MQ message type to carry them.

Depends on s5 (columns must exist in DB).

## User Story

**As a** Hub maintainer,
**I want** every `TrafficEventMessage` field to map cleanly to a `traffic_event` column,
**so that** data planes can publish LLM signal data end-to-end without touching Hub code again.

## Tasks

### 6.1 Extend `TrafficEventMessage`

File: `packages/shared/transport/mq/messages.go`

Add fields to the existing `TrafficEventMessage` struct:

```go
type TrafficEventMessage struct {
    // ... existing fields ...
    ApiKeyClass           string  `json:"api_key_class,omitempty"`
    ApiKeyFingerprint     string  `json:"api_key_fingerprint,omitempty"`
    UsageExtractionStatus string  `json:"usage_extraction_status,omitempty"`
}
```

Per CLAUDE.md `shared` API stability rule — these are additive, which is allowed.

### 6.2 Extend `insertTrafficEventSQL`

File: `packages/nexus-hub/internal/jobs/consumer/traffic.go`

- Add the three columns to the INSERT column list.
- Add three placeholders to each VALUES tuple.
- Add the three fields to the per-row argument vector.

Since the function uses `ON CONFLICT (id) DO NOTHING`, no upsert changes needed.

### 6.3 Consumer group compatibility

The existing consumer group (`"hub-db-writer"`) may have in-flight messages from old producers that lack the new fields. Since the fields are `omitempty` and all new columns are nullable, old messages deserialize with zero-value strings and bind as `""` → we must treat `""` as NULL when binding:

```go
func nullIfEmpty(s string) any {
    if s == "" {
        return nil
    }
    return s
}
```

Apply in the argument construction for the three new fields.

### 6.4 Tests

- Update the existing `traffic_test.go` with a case that publishes a message carrying populated new fields and asserts the DB row has them.
- Add a regression case where the message has empty new fields — assert DB row has NULLs, not empty strings.

## Acceptance Criteria

- `go build ./packages/nexus-hub/...` and `go test -race -count=1 ./packages/nexus-hub/...` pass.
- Manual smoke: `nats pub nexus.event.ai-traffic '{"id":"...","source":"ai-gateway","api_key_class":"nvk_","api_key_fingerprint":"a1b2c3d4e5f60718","usage_extraction_status":"ok"}'` results in a row with those fields populated.
- An old-style message missing the new fields still inserts successfully with NULLs.
- No producer side code is touched in this story (that's s7/s8/s9).

## Non-Goals

- Schema migration (s5 owns that).
- Publishing side in data planes (s7/s8/s9/s10 own that).
- Admin query handler surfacing (s11 owns that).
