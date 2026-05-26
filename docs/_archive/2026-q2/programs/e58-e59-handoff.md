# E58 / E59 Session Handoff (Updated — extended push session)

> Originally generated 2026-05-16. Updated at end of extended "push all remaining tasks" session.
> Pre-load this file at the start of the next session so the next agent has full context without re-reading the SDDs.

## Program goal

**E58** — Cost Estimation, Cache Pricing & Unified Protocol Parser. 5 stories (S0–S4) restructured 2026-05-16 to make S0 the parser-unification foundation that all others depend on.

**E59** — Cache Savings UX Honesty. Independent epic; 1 story (S1). Removes L1/L2/L3/L4 vocabulary from UI/API/i18n because L2 (Semantic) and L3 (Compressed) caches were never implemented — the labels were aspirational fiction. Runs in parallel with E58.

## Phase reached at end of extended session (5 commits landed)

| Commit | Subject | Scope |
|---|---|---|
| `95cb27a2b` | docs(e58/e59): design package | 20 docs (~4,253 LOC) |
| `e2698655b` | refactor(providers): E58-S0 unified Usage extraction | core codec migration |
| `91e6aba1f` | refactor(providers): E58-S0 extend to Cohere + Replicate | + 2 codecs |
| `78733a2ea` | refactor(providers): E58-S0 Phase A streaming session unified | spec_*/stream.go × 3 |
| `bd37f19ab` | fix(ux): E59-S1 remove L1/L2/L3/L4 fiction | UI/API/metrics/i18n |

### Completed this session

- **E58-S0 full Usage-extraction unification** (in scope of "S0 minimal viable"). All 5 main wire formats (OpenAI Chat / OpenAI Responses / Anthropic / Gemini / Cohere) + Replicate + Bedrock (transitive) + 13 OpenAI-compat siblings (transitive via IdentityCodec) + Vertex (transitive via spec_gemini) all delegate to `providers.ExtractUsage` → `shared/normalize` Tier-1 normalizers. NormalizedPayload Usage normalization for Anthropic (PromptTokens = uncached + cache_read + cache_creation) + Gemini (CompletionTokens includes thoughts). Cross-component consistency test 8 fixtures all pass.
- **E58-S0 Phase A streaming session unification.** spec_openai/stream.go uses providers.ExtractUsage on full chunks. spec_anthropic/stream.go delegates per-event extraction to `normalize.MergeAnthropicEventUsage`. spec_gemini/stream.go delegates to `normalize.ExtractGeminiEventUsage`. Helpers exported from shared/normalize.
- **E59-S1 cache UX honesty.** L1/L2/L3/L4 vocabulary removed from UI/API/i18n/Go-comments. Two real concepts shown: Gateway Cache + Provider Prompt Cache. Backend metric + JSON field rename. i18n en/zh/es src + public locales synced.

### NOT done this session — deferred to future sessions

| Item | Why deferred | Risk if attempted blindly |
|---|---|---|
| **E58-S0 full body projection unification** (canonicalbridge.DecodeViaShared returning projected canonical body + Usage; replace per-codec body building) | NormalizedPayload lacks wire-level metadata (response_id, created_at, model_version, provider-specific extensions). Body projection has provider-specific SHA1 tool-call ID synth, finish_reason mapping, content-block ordering that don't naturally factor into a generic projector. Per-codec body projection is correctly per-format. | High — codec test churn across multiple packages; subtle wire-shape regressions visible only via end-to-end traffic capture. |
| **A5 shared/traffic legacy NormalizedContent → NormalizedPayload consolidation** | Hook code paths still consume NormalizedContent. Migration requires audit + cutover across multiple hook implementations. | Medium-high — hook regression breaks compliance gates. |
| **B1 E58-S1 DB schema migration** (Model.cachedInputRead/Write + drop ModelPricing + ProviderPricing) | Parallel session already added `traffic_event.reasoning_tokens` column. Coordination needed before adding more Model schema columns. | High — collision with active parallel work; destructive table drops. |
| **B2 metrics.CalculateCost rewrite to 4-price + Cost struct** | Touches the 5 stamp sites in proxy.go + proxy_cache.go which parallel session already extended with reasoning_tokens stamping. Risk of merge conflicts. | Medium — observable cost calculation behavior change. |
| **B3 Provider template JSON schema bump (12 files)** | Mechanical but waits on B1 schema decision (where fields live). | Low — but blocks on B1. |
| **B4 Backend admin API surface reasoning + cost breakdown** | Blocked on B2 stamping changes. | Medium. |
| **B5 CP-UI types + Provider Wizard + Traffic Drawer + Cost Dashboard reasoning ratio widget** | Blocked on B4 API. | Medium — multi-file UI work. |
| **C E58-S2 estimator core package** (`packages/ai-gateway/internal/execution/estimator/`) | New package, zero collision risk, but multi-hour to do correctly with tiktoken-go vendor + tests. | Low. Can start independently. |
| **D E58-S3 nexus.dry_run flag** | Pipeline branch in proxy.go (parallel session active there) + per-ingress response encoders + traffic_event is_dry_run column. | Medium-high — proxy.go conflict risk + new schema column. |
| **E E58-S4 /v1/estimate compare endpoint** | Blocks on D. | Low — single endpoint once D ships. |
| **Bug #40 — upstream error response body not captured** (trace_id d46e12cc-8887-412e-a654-18d3097ac56d) | Needs investigation of captureResponse path; user said VERIFY FIRST — parallel sessions may have addressed. | Medium — payload-capture path is shared infra. |
| **Req #41 — add `target_path` column to traffic_event** | Multi-day cross-service work (schema + 5 services + UI + historical backfill); user said VERIFY FIRST. | High — schema migration + every audit-emit site. |

## Load-bearing facts the next session must know

### Canonical Usage struct location (final decision, 2026-05-16)

- **Lives in `packages/shared/transport/normalize/types.go`** as `normalize.Usage`.
- `packages/ai-gateway/internal/providers/types.go` exports `type Usage = normalize.Usage` (Go type alias — same struct, no parallel definition).
- `packages/shared/traffic.UsageMeta` is a DIFFERENT struct (carries `Status` field + lacks `TotalTokens`). NOT aliased; future story may consolidate.

### Field rename completed: `CachedTokens` → `CacheReadTokens`

- 23 production files renamed via sed. 124 references to `CacheReadTokens` post-rename.
- Test files also renamed (33 references).
- JSON keys (`cached_tokens`, `prompt_tokens_details.cached_tokens`) — unchanged (PascalCase Go identifier only renamed).

### `providers.ExtractUsage` is the single Usage extraction path

```go
// packages/ai-gateway/internal/providers/usage_extractor.go
func ExtractUsage(raw []byte, wireFormat Format) Usage
```

Wire formats it handles: `FormatOpenAI` + 13 OpenAI-compat formats → `OpenAIChatNormalizer`; `FormatAnthropic` + `FormatBedrock` → `AnthropicMessagesNormalizer`; `FormatGemini` + `FormatVertex` → `GeminiGenerateNormalizer`. Any other format returns zero Usage (silent — caller responsibility to know).

### Anthropic PromptTokens normalization (load-bearing)

`shared/normalize/anthropic_messages.go` now computes `PromptTokens = uncached_input_tokens + cache_read_input_tokens + cache_creation_input_tokens` to match OpenAI canonical convention. Raw Anthropic `input_tokens` is the uncached count. **Do not subtract cache tokens again at call sites.**

This changes the semantic of `Usage.PromptTokens` for Anthropic traffic — `TotalTokens` likewise (now `PromptTokens + CompletionTokens` = total billable). One pre-existing test was updated to reflect new convention.

### Gemini CompletionTokens normalization (also load-bearing)

`shared/normalize/gemini_generate.go` now computes `CompletionTokens = candidatesTokenCount + thoughtsTokenCount`. The two raw Gemini fields are disjoint (visible output vs thinking); canonical convention sums them. `ReasoningTokens` still reports the thinking-only subset separately.

### Cross-component consistency test

`packages/ai-gateway/internal/providers/usage_extractor_consistency_test.go`:
- `TestExtractUsage_CrossComponentConsistency` — same body, identical Usage across ai-gateway path + shared/normalize direct path. 6 fixtures covering OpenAI, Kimi, DeepSeek, Anthropic, Gemini, usage-only.
- `TestExtractUsage_AnthropicPromptTokensNormalization` — verifies the GAP-C1 fix.
- `TestExtractUsage_GeminiCompletionIncludesThinking` — verifies the GAP-D fix.

**This test is the structural invariant guard.** Any future contributor inlining extraction in one path will fail it.

## What's done in this session

| Story / task | Status | Deliverable |
|---|---|---|
| **Design phase** | | |
| Architecture doc edits | done | architecture.md § 12, architecture-doc-triggers.md (2 new rows), normalization-architecture.md (new § "Ai-gateway codec delegation"), provider-adapter-architecture.md § 3a Rule 8, cost-estimation-architecture.md (NEW), provider-usage-extraction-architecture.md (DELETED — subsumed) |
| E58 requirements | done | docs/requirements/e58-cost-estimation-and-cache-pricing.md |
| E59 requirements | done | docs/requirements/e59-cache-savings-ux-honesty.md |
| E58-S0 SDD | done | docs/sdd/e58-s0-unified-protocol-parser.md + docs/sdd/e58-s0-audit.md |
| E58-S1 SDD | done | docs/sdd/e58-s1-cache-pricing-and-reasoning.md |
| E58-S2 SDD | done | docs/sdd/e58-s2-estimator-core.md |
| E58-S3 SDD | done | docs/sdd/e58-s3-dry-run-flag.md |
| E58-S4 SDD | done | docs/sdd/e58-s4-compare-endpoint.md |
| E59-S1 SDD | done | docs/sdd/e59-s1-cache-ux-honesty.md |
| OpenAPI | done | 4 yaml files for E58-S1/S3/S4 + E59-S1 |
| Migration SQL draft | done | docs/sdd/e58-s1-migration-draft.sql |
| **Implementation phase** | | |
| S0-T0 audit | done | docs/sdd/e58-s0-audit.md gap table |
| S0-T1 type alignment | done | `CachedTokens` → `CacheReadTokens` rename; `providers.Usage = normalize.Usage` alias |
| S0-T2 fill normalize gaps | done | openai_chat alias chains (Kimi flat / DeepSeek / Moonshot / Responses); Anthropic PromptTokens normalization; Anthropic TotalTokens fix; Gemini CompletionTokens=candidates+thoughts |
| S0-T3 bridge (Usage only) | done | `packages/ai-gateway/internal/providers/usage_extractor.go` |
| S0-T5/6/7/8 codec migration | done | spec_openai identityCodec, spec_anthropic codec, spec_gemini codec, spec_openai responses codec — all 4 delegate |
| S0-T10 consistency test | done | usage_extractor_consistency_test.go (8 test cases green) |

## What's NOT done — picks up cleanly in next session

| Item | Why deferred | Difficulty / risk |
|---|---|---|
| **Body projection unification** (the "true" canonicalbridge.DecodeViaShared returning projected canonical body + Usage) | Pragmatic scope-trim to deliver consistency win this session. Each codec still does its OWN body projection (Anthropic→OpenAI shape in spec_anthropic, Gemini→OpenAI in spec_gemini). | Medium. The codec body projections are 100-200 lines each but mechanical. Can be a follow-up story. |
| **Streaming session migration** (spec_*/stream.go using shared/normalize/extract walker) | Out of S0 minimal scope. | Medium. Stream code is hot path; tests must cover SSE replay desync. |
| **shared/traffic/adapters/*/normalize.go legacy `ExtractRequest`/`ExtractResponse`** consolidation onto NormalizedPayload | Out of S0 scope per the SDD § "Not in scope". | Low priority. Several hook code paths still consume `NormalizedContent`. Future cleanup. |
| **E58-S1 implementation** (cache pricing fix + reasoning storage/display) | Blocked-by S0; now unblocked. | Large — schema migration + 5 stamp sites + UI changes across 8+ files + 12 provider templates + seed data. Detailed in docs/sdd/e58-s1-cache-pricing-and-reasoning.md. |
| **E58-S2 implementation** (estimator core package) | Blocked-by S0+S1 ModelPrices type. | Medium. tiktoken-go new dep in ai-gateway/go.mod. |
| **E58-S3 implementation** (`nexus.dry_run` flag) | Blocked-by S2. | Medium. New pipeline branch + 4 ingress response encoders. |
| **E58-S4 implementation** (`/v1/estimate` compare endpoint) | Blocked-by S3. | Small. Wraps S3 with concurrent dispatch. |
| **E59-S1 implementation** (UI/API/i18n L1-L4 removal) | Independent. Can run NOW in parallel with E58-S1+ if desired. | Small. Mostly rename + i18n + dashboard rework. ~2-3 days. |

## Files touched this session (uncommitted)

```
packages/ai-gateway/internal/providers/types.go                                   modified
packages/ai-gateway/internal/providers/usage_extractor.go                         NEW
packages/ai-gateway/internal/providers/usage_extractor_consistency_test.go        NEW
packages/ai-gateway/internal/providers/spec_openai/codec.go                       modified
packages/ai-gateway/internal/providers/spec_openai/codec_responses_response.go    modified
packages/ai-gateway/internal/providers/spec_anthropic/codec.go                    modified
packages/ai-gateway/internal/providers/spec_gemini/codec.go                       modified
packages/shared/transport/normalize/openai_chat.go                                          modified (struct + extractCanonicalUsage)
packages/shared/transport/normalize/anthropic_messages.go                                   modified (PromptTokens + TotalTokens normalization)
packages/shared/transport/normalize/anthropic_messages_test.go                              modified (test expectations updated)
packages/shared/transport/normalize/gemini_generate.go                                      modified (CompletionTokens includes thoughts)
+ ~21 other .go files: mechanical CachedTokens → CacheReadTokens rename via sed
docs/dev/architecture.md                                                          modified
docs/dev/architecture-doc-triggers.md                                             modified
docs/dev/cost-estimation-architecture.md                                          NEW
docs/dev/normalization-architecture.md                                            modified (+ ~180 lines new section)
docs/dev/provider-adapter-architecture.md                                         modified (Rule 8 added)
docs/dev/provider-usage-extraction-architecture.md                                DELETED
docs/requirements/e58-cost-estimation-and-cache-pricing.md                        NEW
docs/requirements/e59-cache-savings-ux-honesty.md                                 NEW
docs/sdd/e58-s0-unified-protocol-parser.md                                        NEW
docs/sdd/e58-s0-audit.md                                                          NEW
docs/sdd/e58-s1-cache-pricing-and-reasoning.md                                    NEW
docs/sdd/e58-s1-migration-draft.sql                                               NEW
docs/sdd/e58-s2-estimator-core.md                                                 NEW
docs/sdd/e58-s3-dry-run-flag.md                                                   NEW
docs/sdd/e58-s4-compare-endpoint.md                                               NEW
docs/sdd/e59-s1-cache-ux-honesty.md                                               NEW
docs/openapi/e58-s1-cache-pricing-admin.yaml                                      NEW
docs/openapi/e58-s3-dry-run.yaml                                                  NEW
docs/openapi/e58-s4-estimate-compare.yaml                                         NEW
docs/openapi/e59-s1-cache-ui-fields.yaml                                          NEW
```

## Verification at end of session

```
$ go build ./packages/ai-gateway/... ./packages/shared/...
(clean)

$ go test -count=1 ./packages/ai-gateway/...
... ALL PASS ...

$ go test -count=1 ./packages/shared/...
... ALL PASS ...

$ go test -count=1 -run TestExtractUsage ./packages/ai-gateway/internal/providers/
=== PASS: TestExtractUsage_CrossComponentConsistency (6 sub-tests, all PASS)
=== PASS: TestExtractUsage_AnthropicPromptTokensNormalization
=== PASS: TestExtractUsage_GeminiCompletionIncludesThinking
```

## Binding rules / memories to pre-load next session

- **CLAUDE.md** — bindings; especially "no backward compat", "Plan + Todo non-waivable for complex tasks", "real implementation only", "shared API stability"
- **Memory: feedback_autonomous_execution** — user authorizes self-driven execution after plan is approved
- **Memory: feedback_unit_test_coverage_95** — ≥95% per Go package, list in `.coverage-allowlist` with rationale if not
- **Memory: project_parallel_worktree_sessions** — explicit pathspec on commit, never git stash
- **This session's user decisions:**
  - Canonical Usage in `shared/normalize/types.go` (T1.1)
  - PR strategy: hybrid (Bridge + first codec = 1 PR; remaining codec = 1 PR each)
  - Session goal: T0+T1+T3+all 4 codec migration + consistency test (ACHIEVED with scope trim on T3 — see "What's NOT done")
  - Branch: main (current — parallel sessions, mind pathspec on commits)
  - Field rename `CachedTokens` → `CacheReadTokens` accepted

## How the next session should resume

Two natural paths, depending on user priority:

### Path A — continue S0 to its full scope (recommended for architectural completeness)

1. Implement `canonicalbridge.DecodeViaShared` properly — returns both projected canonical body + Usage. Removes the body-projection duplication still present in spec_anthropic/codec.go (40 lines) and spec_gemini/codec.go (60 lines).
2. Migrate streaming sessions (spec_*/stream.go) to use shared/normalize/extract walker.
3. ~1 week of work. Builds on the solid foundation from this session.

### Path B — start S1 (cache pricing fix) on the S0 foundation

1. The cross-component consistency invariant is already enforced. S1 can stamp correct cache costs immediately.
2. Schema migration (Model.cachedInputRead + cachedInputWrite + traffic_event.reasoning_tokens), `metrics.CalculateCost` rewrite, 12 provider templates, CP-UI Wizard, traffic detail API, dashboards.
3. ~1 week. Detailed in docs/sdd/e58-s1-cache-pricing-and-reasoning.md.

User can pick either; both unblocked.

## One open question carried forward

The dropped `ModelPricing` + `ProviderPricing` tables (S1 migration) are destructive. The migration SQL has a pre-deploy `pg_dump` step. **Make sure prod-deploy skill executes the backup automatically OR the operator does it manually** — verify before pushing the S1 migration to prod.
