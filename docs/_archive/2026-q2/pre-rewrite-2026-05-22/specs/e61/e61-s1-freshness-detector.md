# E61-S1 — Time-Sensitive Prompt Skip (Freshness Detector)

> Story: e61-s1
> Epic: 61 (Smart Response Cache)
> Status: Draft
> Requirements: `docs/developers/specs/e61/e61-smart-response-cache.md` §FR-1
> Architecture: `docs/developers/architecture/services/ai-gateway/response-cache-architecture.md` §4
> Blocked by: none
> Blocks: e61-s2 (skip-reason audit constant) — but only the audit constant; algorithm work proceeds independently.

## User Story

As a Gateway Admin, I want time-sensitive prompts ("what's the current stock price?", "今天的天气怎么样？") to skip the response cache entirely, so that users never receive a stale answer from L1 and L2 is never poisoned with content that's only valid for a short window.

## Tasks

### T1 — Package skeleton

- T1.1 Create `packages/ai-gateway/internal/cache/freshness/` with files: `detector.go`, `rules.go`, `detector_test.go`, `rules_test.go`, `doc.go`.
- T1.2 Public API:
    ```go
    type Rule struct {
        ID                  string   `json:"id"`
        Keywords            []string `json:"keywords"`
        RequireQuestionMark bool     `json:"require_question_mark"`
        RequireEntity       bool     `json:"require_entity"`
        Languages           []string `json:"languages"`
    }

    type Detector struct { /* compiled rules + regex cache */ }

    func NewDetector(rules []Rule, log *slog.Logger) (*Detector, error)
    func (d *Detector) IsTimeSensitive(messages []canonical.Message) (matched bool, ruleID string)
    ```
- T1.3 `NewDetector` compiles regex per rule once; rule reload calls `NewDetector` and atomically swaps via `atomic.Pointer[Detector]` in the wrapping wiring (S2 does the wiring).

### T2 — Detection algorithm

- T2.1 Extract the last user message's plain text from `[]canonical.Message`.
- T2.2 For each compiled rule, in declared order:
    - 2.2.1 If keyword regex matches the text (case-insensitive, word-boundary): proceed.
    - 2.2.2 If `RequireQuestionMark` and text has no `?` / `？`: rule doesn't fire.
    - 2.2.3 If `RequireEntity` and the entity heuristic returns false: rule doesn't fire. Entity heuristic v1: presence of any of {capitalized words, numbers ≥2 digits, ticker-like uppercase tokens, currency symbols, ZH currency words 元/美元/欧元}. Documented in code comments.
    - 2.2.4 If all checks pass: return `(true, rule.ID)`.
- T2.3 Default: return `(false, "")`.
- T2.4 Performance: detection runs in ≤2ms p99 on a typical 200-word prompt with 50 rules. Verified by benchmark in `detector_test.go`.

### T3 — Seed rule list

- T3.1 Add `packages/ai-gateway/internal/cache/freshness/seed_rules.json` with the EN+ZH seed:
    - `time-current`: keywords `[now, today, latest, current, this week, this month, 现在, 今天, 最新, 当前, 本周, 本月]`, require_question_mark=true, require_entity=false.
    - `stock-price`: keywords `[stock price, share price, market cap, 股价, 市值]`, require_question_mark=true, require_entity=true.
    - `exchange-rate`: keywords `[exchange rate, USD, EUR, JPY, 汇率]`, require_question_mark=true, require_entity=true.
    - `weather`: keywords `[weather, temperature, forecast, 天气, 温度, 预报]`, require_question_mark=true, require_entity=true.
    - `news`: keywords `[news, headline, breaking, 新闻, 头条]`, require_question_mark=true, require_entity=false.
    - `score`: keywords `[score, match, game result, 比分, 比赛结果]`, require_question_mark=true, require_entity=true.
- T3.2 The Hub shadow seed (`tools/db-migrate/seed/data/...` — wired in S2) starts from this list. Admin can edit individual rules; cluster restart not required.

### T4 — Unit tests

- T4.1 Table-driven test covering each seed rule with a positive (should-skip) prompt and 2-3 negative (should-not-skip) prompts.
- T4.2 The "discourse particle" pitfall: tests must include
    - "How does DI work now?" → not time-sensitive (no entity, but the question word "DI" is technical, not an entity per the heuristic — verify).
    - Note: heuristic is imperfect by design. If false-positives surface in prod telemetry (`nexus_cache_freshness_skips_total`), admin can tune the rule.
- T4.3 ZH test cases must include both questions ("现在的股价是多少？") and discourse particles ("现在我们来讨论...").
- T4.4 Performance benchmark: `BenchmarkDetector_IsTimeSensitive_50Rules_200Words` < 2ms.
- T4.5 Coverage ≥95%.

### T5 — Observability

- T5.1 New Prometheus counter (registered in `packages/ai-gateway/internal/platform/metrics`):
    ```go
    nexus_cache_freshness_skips_total{rule_id, language}
    ```
- T5.2 Counter increments inside `IsTimeSensitive` on every match. The matched `rule_id` becomes the metric label so admin can see which rules fire how often.

## Acceptance Criteria

- A1: `IsTimeSensitive` returns true for every positive test case in the seed-rule corpus and false for every negative case.
- A2: Detector latency benchmark passes the 2ms p99 budget at 50 rules.
- A3: Unit test coverage ≥95% per CLAUDE.md.
- A4: No goroutine leaks, no allocations on the hot path beyond what `regexp.Regexp.MatchString` requires.
- A5: The package depends only on `regexp`, `log/slog`, and the canonical-message types — no upstream cache packages (so the dependency arrow runs from `cache/core` → `cache/freshness`, not the other way around).

## Out of Scope (S1)

- Hub-shadow wiring of the rule list — that's S2.
- The actual call site that invokes `IsTimeSensitive` and stamps the skip reason — that's S2 (audit constant) + S4 (call-site wiring).
- LLM-classifier tier — explicitly out of scope per FR §7 "Won't".
