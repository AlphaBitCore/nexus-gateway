# E61-S6b — `<InputStagingSelector/>` Cross-Bundle Component

> Story: e61-s6b
> Epic: 61
> Status: Draft
> Requirements: `docs/developers/specs/e61/e61-smart-response-cache.md` §FR-7.6
> Architecture: `docs/developers/architecture/services/ai-gateway/response-cache-architecture.md` §3.4; `docs/developers/architecture/cross-cutting/ui/ui-shared-architecture.md`
> Blocked by: e61-s2b (Strategy enum lives in the shared Go primitive; the TS component uses the same enum values verbatim)
> Blocks: e61-s6

## User Story

As a CP-UI developer building the Cache Settings page (S6), I want a reusable `<InputStagingSelector>` component that takes a model's context limit + current strategy value, renders a dropdown of the five strategy options with the auto-recommended one highlighted, so that when the future Smart Routing rule editor or Ai-Guard rule editor needs the same control they consume the same React component and the admin's experience is uniform across cache / routing / ai-guard.

## Tasks

### T1 — Component location

- T1.1 `packages/ui-shared/src/components/InputStaging/InputStagingSelector.tsx`. Cross-bundle (consumed by both control-plane-ui and agent-ui if needed). Per CLAUDE.md `ui-shared-architecture.md` guidance, any cross-bundle component lives in `ui-shared`.
- T1.2 Index export from `packages/ui-shared/src/components/index.ts`.

### T2 — Props + types

- T2.1 Types match the Go enum from `e61-s2b`:
    ```ts
    export type InputStagingStrategy =
        | "last_user"
        | "system_plus_last_user"
        | "recent_turns"
        | "head_plus_tail"
        | "full_truncated";

    export type InputStagingProfile = "generic" | "short_answer" | "long_completion";

    export interface InputStagingSelectorProps {
        modelContextLimit: number;          // tokens
        value: InputStagingStrategy;
        onChange: (next: InputStagingStrategy) => void;
        profile?: InputStagingProfile;      // default 'generic'
        disabled?: boolean;
        helpKey?: string;                   // i18n key for the help link
    }
    ```

### T3 — Suggest() port to TS

- T3.1 `packages/ui-shared/src/components/InputStaging/suggest.ts` mirrors the Go `Suggest` function with the same heuristic table from `e61-s2b T5.1`. Keep the heuristic in one place by having a single source of truth — either:
    - (a) generate the TS heuristic from a shared JSON spec at build time (over-engineering for v1),
    - or (b) document the table in BOTH `staging.go` and `suggest.ts` with a comment `// MUST stay in sync with packages/shared/transport/inputstaging/staging.go Suggest()`. Choice (b) is fine for E61.
- T3.2 Unit test `suggest.test.ts` covering the same matrix as the Go test.

### T4 — Component behaviour

- T4.1 Render a label, a `<select>` with the 5 options, and a small "Recommended" badge next to the `Suggest()`-recommended option.
- T4.2 Show a tooltip with one-sentence description per strategy:
    - last_user: "Only the final user message is sent for cache lookup. Smallest, fastest."
    - system_plus_last_user: "System prompt + final user message. Balances persona context with query specificity."
    - recent_turns: "Most recent conversation turns that fit. Best for multi-turn flows."
    - head_plus_tail: "First and last portions of the conversation. Useful when both opening setup and recent context matter."
    - full_truncated: "Full conversation, hard-truncated from the head if needed. Legacy mode."
- T4.3 If `modelContextLimit` changes, re-evaluate `Suggest()` and update the badge — but do NOT auto-change the selected value (admin's choice is sticky).

### T5 — Design tokens + i18n

- T5.1 No hex / inline styles — everything via `*.module.css` design tokens.
- T5.2 i18n keys live in the shared namespace (`shared.inputStaging.*`). 3 locales.

### T6 — Tests

- T6.1 Vitest component test: renders all 5 options.
- T6.2 Suggest highlighting test: change `modelContextLimit` → "Recommended" badge moves.
- T6.3 `onChange` callback fires when user picks a different option.
- T6.4 `disabled` prop disables the select.
- T6.5 i18n key-presence test across EN/ZH/ES.

## Acceptance Criteria

- A1: Component exported from `packages/ui-shared`.
- A2: All 5 strategies render with translations in 3 locales.
- A3: `Suggest()` heuristic produces the same recommendation in TS as in the Go `Suggest()` for identical inputs.
- A4: Component consumes design tokens only.

## Out of Scope (S6b)

- Wiring the component into the Cache Settings page — that's S6.
- Future adoption by Smart Routing rule editor (task #14) and Ai-Guard rule editor (task #15) — those PRs import this component as-is.
