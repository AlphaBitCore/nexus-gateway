# E38-S1 — Provider Cache Metric Extraction + Cost Model

> Story: e38-s1
> Epic: 38 (Prompt Cache Friendliness)
> Status: Approved

## User Story

As a Platform Admin, I want to see upstream provider cache hit/miss
token counts and net cost savings on every traffic event, so that I
can measure the current state and build the ROI case before enabling
normalisation rules.

## Tasks

- T1: Add `CacheCreationTokens *int` to `providers.Usage`
- T2: Wire `cache_creation_input_tokens` into Anthropic streaming path
- T3: Add `CacheCreationTokens *int` to `shared/traffic.UsageMeasurement`; wire Anthropic traffic adapter
- T4: Add 8 new columns to `traffic_event` in Prisma schema + migration
- T5: Add `provider_pricing` table to Prisma schema + migration + seed data
- T6: Extend `audit.Record` with cache fields
- T7: Extend `mq.TrafficEventMessage` and `consumer.TrafficEventMessage`
- T8: Extend Hub `insertTrafficEventSQL` to write new columns
- T9: Add `SnapshotCache[ProviderPricing]` to cachelayer; compute cost in audit write path

## Acceptance Criteria

- AC1: A real Anthropic request via the gateway with `cache_control` markers set by the client produces `cache_creation_tokens > 0` on the `traffic_event` row after the first turn, and `cache_read_tokens > 0` on subsequent turns hitting the same prefix.
- AC2: An OpenAI request with prefix caching active produces `cache_read_tokens > 0` when the provider reports cached tokens.
- AC3: `cache_net_savings_usd` = `cache_read_savings_usd - cache_write_cost_usd` is correct to 8 decimal places.
- AC4: All 8 new columns are NULL when the provider does not report cache usage (no data loss).
- AC5: `go test -race -count=1 ./packages/ai-gateway/...` passes.

## Data Model

### `providers.Usage` (packages/ai-gateway/internal/providers/types.go)

```go
type Usage struct {
    PromptTokens         *int
    CompletionTokens     *int
    TotalTokens          *int
    CachedTokens         *int  // existing: cache_read tokens
    ReasoningTokens      *int
    CacheCreationTokens  *int  // NEW: cache_creation_input_tokens (Anthropic)
}
```

### `shared/traffic.UsageMeasurement` (packages/shared/traffic/detect.go)

```go
type UsageMeasurement struct {
    // ... existing fields ...
    CachedTokens         *int  // existing
    CacheCreationTokens  *int  // NEW
}
```

### `traffic_event` new columns

```sql
cache_creation_tokens   INT         -- provider wrote N tokens to cache (write cost)
cache_read_tokens       INT         -- provider served N tokens from cache (savings)
normalized_strip_count  INT         -- rules that matched and stripped
normalized_strip_bytes  INT         -- bytes removed by normaliser
cache_marker_injected   SMALLINT    -- 0-4: cache_control markers injected this request
cache_write_cost_usd    NUMERIC(12,8)
cache_read_savings_usd  NUMERIC(12,8)
cache_net_savings_usd   NUMERIC(12,8)  -- savings - write_cost
```

### `provider_pricing` new table

```sql
CREATE TABLE provider_pricing (
    id              TEXT PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id     TEXT REFERENCES "Provider"(id) ON DELETE CASCADE,
    model_pattern   TEXT NOT NULL,    -- regex matched against ProviderModelID
    input_usd_per_m NUMERIC(12,8) NOT NULL DEFAULT 0,
    output_usd_per_m NUMERIC(12,8) NOT NULL DEFAULT 0,
    cache_write_usd_per_m NUMERIC(12,8) NOT NULL DEFAULT 0,
    cache_read_usd_per_m  NUMERIC(12,8) NOT NULL DEFAULT 0,
    priority        INT NOT NULL DEFAULT 0,  -- higher wins on pattern conflict
    created_at      TIMESTAMPTZ DEFAULT NOW()
);
-- NULL provider_id = global default for adapter_type
CREATE INDEX ON provider_pricing(provider_id, priority DESC);
```

Seed data covers: claude-3-5-sonnet, claude-3-5-haiku, claude-3-opus,
gpt-4o, gpt-4o-mini, gpt-4-turbo, deepseek-v3, with generic
fallback rows.

## Implementation Notes

- The Anthropic streaming codec already emits `cache_creation_input_tokens`
  via `nexus.ext.anthropic.cache_creation_input_tokens` on the canonical
  body. For `providers.Usage`, the non-streaming `responseToUsage` function
  in `spec_anthropic/codec.go` must additionally set `CacheCreationTokens`.
  The streaming path (`spec_anthropic/stream.go`) must accumulate the field
  from the final `message_delta.usage` frame alongside existing token accumulation.
- Cost computation happens in `audit.Writer.recordToMessage` (or a helper
  called from it) immediately before MQ publish. It reads from a
  `SnapshotCache[ProviderPricing]` injected into the Writer at construction.
- `CacheReadTokens` on `audit.Record` is sourced from
  `providers.Usage.CachedTokens` (the existing field); the rename is
  semantic only in the DB column name, not in the Go struct.
