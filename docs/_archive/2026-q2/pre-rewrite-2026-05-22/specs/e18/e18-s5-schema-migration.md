# E18 — Story 5: Schema Migration for LLM Signal Columns

## Context

`traffic_event` already carries `provider_name`, `provider_id`, `model_name`, `model_id`, `prompt_tokens`, `completion_tokens`, `total_tokens`, `source`, and `trace_id` (from the 2026-04-14 audit consolidation). This story adds the three missing LLM signal columns plus their index and the Go type mirroring.

## User Story

**As a** platform owner,
**I want** `traffic_event` to have the columns needed to store API key identity and extraction tier for every row,
**so that** analytics SQL can filter and aggregate on these fields without JSON extraction from `details`.

## Tasks

### 5.1 Prisma migration — `tools/db-migrate/`

Update `schema.prisma` model `traffic_event`:

```prisma
api_key_class              String?  @map("api_key_class")
api_key_fingerprint        String?  @map("api_key_fingerprint")
usage_extraction_status    String?  @map("usage_extraction_status")
```

Add index:

```prisma
@@index([api_key_fingerprint, timestamp], map: "idx_traffic_event_apikey_fingerprint")
```

### 5.2 CHECK constraint via raw migration

`npx prisma migrate dev --create-only --name traffic_event_llm_signal_columns`, then hand-edit the generated SQL to add:

```sql
ALTER TABLE traffic_event
  ADD CONSTRAINT chk_traffic_event_usage_status
  CHECK (usage_extraction_status IN
    ('ok','streaming_reported','streaming_estimated',
     'streaming_unavailable','parse_failed','no_body','non_llm')
    OR usage_extraction_status IS NULL);
```

Plus a partial index for fingerprint lookups (automatic from Prisma `@@index` is non-partial; replace with hand-written):

```sql
DROP INDEX IF EXISTS idx_traffic_event_apikey_fingerprint;
CREATE INDEX idx_traffic_event_apikey_fingerprint
  ON traffic_event (api_key_fingerprint, timestamp)
  WHERE api_key_fingerprint IS NOT NULL;
```

### 5.3 Go codegen — `tools/db-migrate/codegen-go.mjs`

No action needed if `TrafficEvent` is already in the codegen whitelist. Re-run `npx prisma migrate dev && node codegen-go.mjs` to regenerate `packages/shared/schemas/configtypes/traffic_event.go`.

### 5.4 Control Plane read struct

Update `packages/control-plane/internal/store/traffic_event.go` to include the three new fields in the returned struct and to `SELECT` them in the list/detail queries.

### 5.5 Seed data (optional)

If `tools/db-migrate/seed.mjs` populates sample traffic rows, add representative values for the three new fields so admin UI dev work has something to render.

## Acceptance Criteria

- `npx prisma migrate dev` succeeds on a clean DB.
- `SELECT column_name FROM information_schema.columns WHERE table_name = 'traffic_event'` returns the three new columns with TEXT type, nullable.
- `CHECK` constraint rejects values outside the enum; direct `INSERT INTO traffic_event (..., usage_extraction_status) VALUES (..., 'bogus')` fails.
- Partial index exists and is used by `EXPLAIN` on a query `WHERE api_key_fingerprint = '...' AND timestamp > ...`.
- `packages/shared/schemas/configtypes/traffic_event.go` (or the equivalent generated file) contains the three new fields.
- Control Plane admin traffic list API returns non-error responses with the new fields present (values are NULL until data planes start writing them — that's s7/s8/s9/s10).

## Non-Goals

- Backfilling historical rows. `api_key_fingerprint` / `api_key_class` / `usage_extraction_status` remain NULL on pre-migration rows forever.
- Changing existing column types, renaming, or dropping anything.
