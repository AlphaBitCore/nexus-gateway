# E48 S5 — traffic_event audit columns + MQ wire fields

**Epic:** E48
**Requirements:** [e48-emergency-passthrough.md](../../../../docs/developers/specs/e48/e48-emergency-passthrough.md) — Must M7
**OpenAPI:** none in this story (S6 adds the admin API + UI badge)
**Status:** Approved
**Date:** 2026-05-13
**Builds on:** [e48-s4-bypass-branches.md](e48-s4-bypass-branches.md)

---

## Architecture summary

S4 added in-memory `PassthroughFlags []string` + `PassthroughReason string` to `audit.Record`. S5 wires them onto:

1. **`traffic_event` DB columns** — `passthrough_flags TEXT[]` + `passthrough_reason TEXT`. Nullable.
2. **MQ wire envelope** (`mq.TrafficEventMessage`) — corresponding `PassthroughFlags` + `PassthroughReason` JSON fields, omitempty.
3. **AI Gateway `audit.Writer.recordToMessage`** — copy fields from `*audit.Record` onto the wire message.
4. **Nexus Hub `consumer/traffic.go`** — extend `insertTrafficEventSQL` with the two new columns + bind the values from the wire message.

S6 adds the admin UI badge that reads these columns. Backend-side persistence ships here so operators can SQL-filter on day-1 even before the UI is updated.

## Schema

```sql
ALTER TABLE traffic_event
  ADD COLUMN passthrough_flags TEXT[],
  ADD COLUMN passthrough_reason TEXT;

-- Partial index optimised for the common operator query
-- "show every request that fired any bypass in the last 30 days".
CREATE INDEX traffic_event_passthrough_active_idx
  ON traffic_event (timestamp DESC)
  WHERE passthrough_flags IS NOT NULL;
```

Nullable on both columns; an empty array would still match a `IS NOT NULL` filter. The AI Gateway audit Writer sets the column to `NULL` when `len(rec.PassthroughFlags) == 0` so the partial index doesn't grow with no-bypass rows.

## Story

### S5 — Audit fields

**Tasks:**

- **T5.1** — Migration `tools/db-migrate/migrations/20260517000010_e48_traffic_event_passthrough_columns/migration.sql`: the ALTER + the partial index.
- **T5.2** — Prisma schema: add `passthroughFlags String[] @map("passthrough_flags")` + `passthroughReason String? @map("passthrough_reason")` to the `TrafficEvent` model.
- **T5.3** — `packages/shared/transport/mq/messages.go` — `TrafficEventMessage` gains `PassthroughFlags []string json:"passthroughFlags,omitempty"` + `PassthroughReason string json:"passthroughReason,omitempty"`.
- **T5.4** — `packages/ai-gateway/internal/observability/audit/audit.go` `recordToMessage` — copy fields from `rec.PassthroughFlags` / `rec.PassthroughReason` onto `msg`.
- **T5.5** — `packages/nexus-hub/internal/jobs/consumer/traffic.go` — extend `insertTrafficEventSQL` with the two columns + the corresponding `batch.Queue` parameters. Pass `e.PassthroughFlags` ([]string maps to PostgreSQL text[]) and `stripNulPtr(&e.PassthroughReason)` (or empty-string-to-nil).
- **T5.6** — Build + test:
  - `go build ./...`
  - `go test -race -count=1 ./packages/...`

**Acceptance:**

- DB migration applies clean on local DB.
- A traffic_event row produced by a request whose passthrough was active in any tier has `passthrough_flags = ['bypassHooks']` (or whichever bypass(es) fired) AND `passthrough_reason` matching the operator's text.
- A normal traffic_event row has both columns `NULL`.
