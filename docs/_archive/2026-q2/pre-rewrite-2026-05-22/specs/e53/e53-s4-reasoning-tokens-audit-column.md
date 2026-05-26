# E53-S4 — reasoning_tokens audit column + Hub consumer write

**Epic:** E53 Reasoning Content Passthrough
**Type:** Schema migration + audit pipeline change
**Owner:** nexus
**Depends on:** none (independent of s1/s2/s3, but most useful when those
ship together so the column starts populating immediately).

## User story

> As a billing / finance operator, I want `traffic_event.reasoning_tokens`
> populated for every reasoning-model call, so I can compute per-provider
> reasoning cost separately and detect runaway reasoning budgets early.

> As an audit operator, I want the same column surfaced in the
> per-Thing stats dashboard alongside `prompt_tokens` and `completion_tokens`.

## Tasks

### T4.1 — Migration

**File:** `tools/db-migrate/migrations/<TS>_e53_reasoning_tokens_column/migration.sql`

```sql
ALTER TABLE "traffic_event"
  ADD COLUMN "reasoning_tokens" INTEGER NULL;
```

`<TS>` follows the unique-timestamp rule (per
`memory/feedback_migration_timestamp_unique.md`). Pre-allocate at SDD
time: `20260526000000` (verified unique against `ls migrations/ | cut -c1-14 | sort | uniq -d`).

ALTER ADD COLUMN NULL is PG11+ O(1) metadata-only — safe on high-write
`traffic_event` table. No index, no backfill, no application downtime.

### T4.2 — AI Gateway audit Record field

**File:** `packages/ai-gateway/internal/observability/audit/audit.go`

Add field to the audit record struct:

```go
type Record struct {
    ...existing fields...
    ReasoningTokens *int `json:"reasoningTokens,omitempty"`
}
```

Pointer type so NULL semantics survive the JSON envelope to Hub.

### T4.3 — Populate from upstream usage

**File:** `packages/ai-gateway/internal/handler/proxy.go` (audit-populate
section, near where `PromptTokens` / `CompletionTokens` are set)

Read from the normalized response:
- OpenAI shape: `usage.completion_tokens_details.reasoning_tokens`
- Anthropic shape: `usage.thinking_tokens` (when present; absent for non-thinking responses)
- Gemini shape: `usage.thoughtsTokenCount`
- DeepSeek/Moonshot/Kimi: `usage.completion_tokens_details.reasoning_tokens` (OpenAI-compat)

The normalizer in `packages/shared/transport/normalize/` already projects all
shapes into a canonical usage object. Read it from there and stamp
into `rec.ReasoningTokens`.

### T4.4 — Hub consumer write

**File:** `packages/nexus-hub/internal/jobs/consumer/traffic.go`

Extend the INSERT statement (near line 269 / 326 per the earlier grep)
to include `reasoning_tokens`:

```go
const insertSQL = `
    INSERT INTO traffic_event (
        id, source, timestamp, ...,
        prompt_tokens, completion_tokens, reasoning_tokens, total_tokens,
        ...
    ) VALUES (
        $1, $2, $3, ...,
        $X, $Y, $NEW, $Z,
        ...
    )`
```

Add `e.ReasoningTokens` to the args slice and bump positional placeholders.

### T4.5 — MQ message schema

**File:** `packages/shared/transport/mq/messages.go`

Add `ReasoningTokens *int \`json:"reasoningTokens,omitempty"\`` to the
TrafficEvent message struct. Backward compatible: older publishers
don't set the field, consumer treats absent as NULL.

### T4.6 — UI / dashboard surface (optional follow-up)

If the per-Thing Stats tab and Cache ROI dashboard widgets should show
reasoning_tokens separately, add columns/widgets in a follow-up — NOT
required for the SDD acceptance. Schema readiness is the goal of s4.

## Acceptance criteria

- AC-4.1: After migration, `\d traffic_event` shows `reasoning_tokens
  integer` column.
- AC-4.2: A request to `o3` returns 200; the corresponding `traffic_event`
  row has `reasoning_tokens > 0` matching the upstream
  `usage.completion_tokens_details.reasoning_tokens` value byte-for-byte.
- AC-4.3: A request to `gpt-4o-mini` (no reasoning) returns 200; the
  corresponding row has `reasoning_tokens IS NULL` or `= 0`.
- AC-4.4: A request to `gemini-2.5-pro` with thinking enabled has
  `reasoning_tokens > 0` matching `usage.thoughtsTokenCount`.
- AC-4.5: A request to `claude-opus-4-7` with thinking enabled has
  `reasoning_tokens > 0` matching the Anthropic thinking_tokens (if
  reported) or the equivalent computed value.
- AC-4.6: No regression in existing analytics queries (SELECT * does not
  fail on old rows; SUM(reasoning_tokens) treats NULL as 0 when GROUPED
  with `COALESCE(reasoning_tokens, 0)`).
- AC-4.7: Hub consumer log shows no INSERT errors on rows with NULL
  reasoning_tokens (older publishers).

## Verification

- Migration applied surgically on prod (per `prod-deploy` skill's
  "Applying a single DB migration" path), pg_dump backup taken first.
- `go test ./packages/ai-gateway/internal/observability/audit/...
  ./packages/nexus-hub/internal/jobs/consumer/... -race -count=1`
- Live curl against prod gateway with o3, gemini-2.5-pro,
  claude-opus-4-7. Query `traffic_event` to confirm column population.
- Smoke run after deploy: existing smoke ROI assertions still pass.

## Risks

- **R-4.1**: Migration timestamp collision (per
  `memory/feedback_migration_timestamp_unique.md`). Verify `20260526000000`
  prefix is unique at PR-prep time by running `ls migrations/ | cut -c1-14
  | sort | uniq -d` — output must be empty.
- **R-4.2**: Publisher / consumer skew during rolling deploy. The MQ
  message struct is additive (optional field with `omitempty`); the new
  consumer accepts absent (NULL written). The new publisher emits the
  field; older consumers ignore unknown JSON keys. No skew-induced data
  loss.
- **R-4.3**: Hub consumer SQL placeholder mismatch. Mitigation: add a
  consumer-side unit test that verifies the placeholder count matches
  the args slice length (already a common pattern in the codebase).
