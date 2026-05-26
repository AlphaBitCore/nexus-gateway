-- E53-s4: reasoning_tokens audit column.
--
-- Captures the per-request reasoning / chain-of-thought token count
-- reported by providers in usage.completion_tokens_details.reasoning_tokens
-- (OpenAI / DeepSeek / Moonshot / Kimi), usage.thoughtsTokenCount (Gemini),
-- and usage.thinking_tokens (Anthropic when present). NULL when upstream
-- does not report it.
--
-- ALTER ADD COLUMN INTEGER NULL is PG11+ O(1) metadata-only — safe on
-- the high-write traffic_event table. No index (analytics queries that
-- need this column will scan-with-filter on existing time-range indexes).
-- No backfill (historical rows stay NULL — providers don't expose
-- reasoning_tokens retroactively).

ALTER TABLE "traffic_event"
  ADD COLUMN "reasoning_tokens" INTEGER NULL;
