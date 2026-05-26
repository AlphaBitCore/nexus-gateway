-- E61-FR-4.4 — AI Guard inputstaging adoption
--
-- Adds two columns to ai_guard_config so the classify pipeline can apply
-- inputstaging.Plan before rendering the judge prompt:
--
--   input_strategy      — one of the five inputstaging.Strategy constants
--                         (last_user, system_plus_last_user, recent_turns,
--                         head_plus_tail, full_truncated). Defaults to
--                         "system_plus_last_user" which preserves any
--                         guard system-prompt context.
--
--   model_context_limit — the judge model's context window in tokens.
--                         0 = unknown / not configured; the pipeline falls
--                         back to 8192 when zero.
--
-- Idempotent: uses ADD COLUMN IF NOT EXISTS so re-applying is safe.
-- Pre-GA: no backward compatibility (CLAUDE.md development-phase policy).

ALTER TABLE ai_guard_config
    ADD COLUMN IF NOT EXISTS input_strategy       TEXT NOT NULL DEFAULT 'system_plus_last_user';

ALTER TABLE ai_guard_config
    ADD COLUMN IF NOT EXISTS model_context_limit  INT  NOT NULL DEFAULT 0;
