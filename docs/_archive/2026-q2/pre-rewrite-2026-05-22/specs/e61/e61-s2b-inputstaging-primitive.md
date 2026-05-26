# E61-S2b — Shared `inputstaging` Primitive

> Story: e61-s2b
> Epic: 61
> Status: Draft
> Requirements: `docs/developers/specs/e61/e61-smart-response-cache.md` §FR-4
> Architecture: `docs/developers/architecture/services/ai-gateway/response-cache-architecture.md` §3.4 (consumer perspective); memory `project_inputstaging_shared_primitive`
> Blocked by: none
> Blocks: e61-s4 (semantic cache embeds via this), e61-s6b (UI selector consumes the strategy enum)

## User Story

As a future implementer of routing-decision LLM calls or ai-guard classification, I want a single Go package that decides how to truncate a multi-turn conversation to fit a model's context window, so I don't have to invent my own truncation policy and the admin UI offers a uniform "strategy selector" component across cache / routing / ai-guard.

## Tasks

### T1 — Package skeleton

- T1.1 Create `packages/shared/transport/inputstaging/` with files: `staging.go`, `staging_test.go`, `strategy.go`, `tokenize.go`, `tokenize_test.go`, `doc.go`.
- T1.2 Public API:
    ```go
    type Strategy string
    const (
        StrategyLastUser           Strategy = "last_user"
        StrategySystemPlusLastUser Strategy = "system_plus_last_user" // default
        StrategyRecentTurns        Strategy = "recent_turns"
        StrategyHeadPlusTail       Strategy = "head_plus_tail"
        StrategyFullTruncated      Strategy = "full_truncated"
    )

    type OverflowKind string
    const (
        OverflowNone                OverflowKind = ""
        OverflowSingleMessageTooBig OverflowKind = "single_message_too_big"
        OverflowAfterStrategy       OverflowKind = "after_strategy"
    )

    type PlanResult struct {
        Messages     []canonical.Message
        InputTokens  int
        Truncated    bool
        OverflowKind OverflowKind
    }

    type Profile string
    const (
        ProfileGeneric        Profile = "generic"        // default
        ProfileShortAnswer    Profile = "short_answer"   // most output ≤256 tokens
        ProfileLongCompletion Profile = "long_completion" // expects ~2k output
    )

    func Plan(messages []canonical.Message, modelContextLimit int, strategy Strategy, reserveOutput int) (PlanResult, error)
    func Suggest(modelContextLimit int, profile Profile) Strategy
    ```

### T2 — Token estimation

- T2.1 `tokenize.go` exposes `EstimateTokens(text string) int`. v1 implementation: cl100k_base BPE approximation — or, simpler v0: 1 token ≈ 4 characters for English, 1 token ≈ 2 characters for ZH/JP/KR. v0 is acceptable for staging decisions; precise tokenization is the embedding provider's job.
- T2.2 The estimator is internal; downstream callers consume `PlanResult.InputTokens`.
- T2.3 Document the approximation choice in `tokenize.go` doc comment so future contributors don't think we're trying to match the upstream tokenizer exactly.

### T3 — Strategy implementations

- T3.1 `StrategyLastUser`:
    - Keep only the last message with `role="user"`. Drop everything else.
- T3.2 `StrategySystemPlusLastUser` (default):
    - Keep all `role="system"` messages (typically 1) + the last `role="user"` message.
    - If multiple system messages, keep all in order.
- T3.3 `StrategyRecentTurns`:
    - Keep recent turns from the end; stop when adding one more would exceed `modelContextLimit - reserveOutput`.
    - A "turn" is a `user` + assistant pair (or single user message at the end).
    - Always include the first `system` message regardless.
- T3.4 `StrategyHeadPlusTail`:
    - Take the first N tokens worth of head + last M tokens worth of tail with a small "..." marker in between.
    - Default split: head 30% / tail 70% of the budget.
- T3.5 `StrategyFullTruncated`:
    - From the end backwards, drop messages until total tokens ≤ budget.
    - If the final remaining message alone exceeds the budget, return `OverflowKind=single_message_too_big`.

### T4 — Plan overflow handling

- T4.1 If the chosen strategy returns messages totaling ≤ budget → `OverflowKind=none`, `Truncated` reflects whether anything was dropped.
- T4.2 If even after applying the strategy the messages still exceed budget → `OverflowKind=after_strategy`, `Messages` is the partial truncation result, callers decide whether to use it or skip.
- T4.3 If the LAST remaining message alone (the user query in `LastUser`/`SystemPlusLastUser`) exceeds the budget → `OverflowKind=single_message_too_big`, `Messages` is the result of hard-truncating that message from the head (best-effort).

### T5 — Suggest()

- T5.1 Heuristic table:
    | context_limit | profile | suggested |
    |---|---|---|
    | ≤ 1024 | any | `last_user` |
    | 1024-4096 | generic | `system_plus_last_user` |
    | 1024-4096 | long_completion | `last_user` |
    | 4096-16384 | generic / short_answer | `system_plus_last_user` |
    | 4096-16384 | long_completion | `recent_turns` |
    | > 16384 | any | `recent_turns` |
- T5.2 The CP-UI consumer (e61-s6b InputStagingSelector) calls this to highlight the recommended option but always lets admin override.

### T6 — Tests

- T6.1 Table-driven test per strategy with synthetic conversations (small, medium, oversize).
- T6.2 `OverflowKind` test matrix: each strategy × each overflow scenario.
- T6.3 `Suggest()` test against the matrix in T5.1.
- T6.4 Benchmark: `Plan` on a 100-message conversation completes in <500µs.
- T6.5 Coverage ≥95%.

## Acceptance Criteria

- A1: All five strategies implemented and unit-tested.
- A2: `OverflowKind` values are emitted correctly for every overflow scenario.
- A3: Package imports only `unicode`, `strings`, `errors`, and the canonical-message type — no upstream service dependencies.
- A4: Benchmark passes the 500µs budget.
- A5: Documented as "first-consumer-is-E61-semantic-cache; routing + ai-guard adopt later" in the package doc comment (so future contributors know the design intent).

## Out of Scope (S2b)

- The InputStagingSelector React component — that's S6b.
- Wiring semantic cache to call `Plan` — S4.
- Adoption by routing or ai-guard — captured as future tasks #14 / #15.
