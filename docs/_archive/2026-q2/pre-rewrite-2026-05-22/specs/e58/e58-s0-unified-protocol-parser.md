# E58-S0 — Unified Protocol Parser (ai-gateway codec delegates to shared/normalize)

> Story: e58-s0
> Epic: 58 (Cost Estimation, Cache Pricing & Unified Parser)
> Status: Draft
> Requirements: `docs/developers/specs/e58/e58-cost-estimation-and-cache-pricing.md` § FR-1
> Architecture:
> - `docs/developers/architecture/services/ai-gateway/normalization-architecture.md` § "Ai-gateway codec delegation (E58-S0)" — canonical reference
> - `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` § 3a Rule 8 — decoding delegates / encoding stays
> Blocks: E58-S1, E58-S2, E58-S3, E58-S4 (every later story depends on the unified parser landing first)
> Blocked by: none

## User Story

As a Platform Admin and as an OSS contributor, I want the AI Gateway, the compliance proxy, the agent, and the Hub audit pipeline to all use **one** wire-format parser per upstream protocol (OpenAI Chat, OpenAI Responses, Anthropic Messages, Gemini generateContent), so that:

- The same upstream response intercepted via three different paths produces byte-identical `traffic_event` rows.
- Adding a new field to upstream's response shape requires editing one file in `shared/normalize/`, not chasing four parallel parser implementations.
- Adding a new provider adapter requires implementing one Tier-1 Normalizer plus the per-model wire-emission rules in `spec_*/` — the encode and decode halves are clearly separated.
- The cache-pricing fix (S1), reasoning-display work (S1), estimator (S2), and dry-run (S3) all build on a single shared substrate.

## Scope and Non-Goals

### In scope

- Capability-gap audit between `ai-gateway/spec_<format>/` and `shared/normalize/<format>` for the four wire formats and the 19 ai-gateway adapter packages.
- Enhancements to `shared/normalize/<format>` to cover every field the legacy codec extracted (cache tokens, reasoning tokens, alias chains, provider extensions).
- New bridge package `packages/ai-gateway/internal/execution/canonicalbridge/` (or extension of an existing one) implementing `DecodeViaShared(raw, wireFormat, endpoint) → (canonicalBody, providers.Usage, error)`.
- Migration of `spec_<format>/codec.DecodeResponse` and `spec_<format>/stream.go` (SSE walker) for OpenAI Chat, OpenAI Responses, Anthropic Messages, Gemini generateContent.
- Cross-component consistency test asserting all three call paths (direct `shared/normalize`, `canonicalbridge`, `shared/traffic.Adapter`) produce byte-identical `NormalizedPayload`.
- Deletion of duplicated parsing code in `spec_*/` after delegation lands.
- Type alignment: `shared/traffic.UsageMeasurement` becomes a type alias for the canonical Usage shape.

### Not in scope (deferred)

- Wire-emission changes (`EncodeRequest`, `PrepareBody`, per-model strip rules). Those stay in `spec_*/` — they encode per-upstream wire requirements (provider-adapter-architecture.md § 3a Rules 1-7) and are not parser concerns.
- Consolidating `shared/traffic/adapters/*/normalize.go`'s legacy `ExtractRequest`/`ExtractResponse` (`NormalizedContent` surface) onto `NormalizedPayload`. Several hook code paths still consume `NormalizedContent`; merging it is a separate refactor.
- Migrating `shared/traffic/adapters/*` for web/consumer surfaces (chatgpt-web, claude-web, gemini-web, cursor, deepseek-web, kimi-web, etc.). They already delegate to Tier-1 via the E46-S12 per-host adapter mechanism; no change needed.
- Replacing `providers.Canonical` (OpenAI wire-shape JSON bytes) with `NormalizedPayload`. They serve different needs.

## Tasks

### T0 — Audit and gap table

- T0.1 Create the audit document `docs/developers/specs/e58/e58-s0-audit.md` (or an appendix in this SDD) listing, per wire format, what `ai-gateway/spec_<format>/codec.go` + `stream.go` extract that `shared/normalize/<format>.go` does **not**.
- T0.2 For each ai-gateway adapter package (19 total: openai, azure_openai, anthropic, bedrock, cohere, deepseek, fireworks, gemini, glm, groq, huggingface, minimax, mistral, moonshot, perplexity, replicate, together, vertex, xai), confirm which `shared/normalize/<format>` Tier-1 normalizer the adapter's wire format maps to. Most map to one of the four cores; a few (bedrock, vertex) wrap with provider envelopes that need a per-wrapper unwrap step.
- T0.3 For each gap discovered, file a sub-task under T2 to fill the gap in `shared/normalize/`.
- T0.4 Vendor docs URLs are recorded inline as the audit is performed — `https://platform.openai.com/docs/...`, `https://docs.anthropic.com/...`, `https://ai.google.dev/...`, `https://docs.aws.amazon.com/bedrock/...`, etc. — so reviewers can verify the alias chain matches the published shape.

### T1 — Type alignment and naming

- T1.1 Confirm the canonical `Usage` struct definition stays in `packages/ai-gateway/internal/providers/types.go` (or hoist to a sensible shared location — likely `shared/normalize/types.go` since `NormalizedPayload.Usage` already references it). Decision recorded as part of T0.1.
- T1.2 `packages/shared/traffic/detect.go`'s `UsageMeasurement` struct is replaced by a type alias to the canonical `Usage`. Any callers continue to compile (alias is transparent).
- T1.3 Verify there are no other parallel `Usage` / `UsageMeasurement` definitions in the repository.

### T2 — Fill gaps in shared/normalize/

For each gap identified in T0:

- T2.1 (OpenAI Chat) Likely additions: Kimi flat `cached_tokens` (Moonshot K2/K2.5/K2.6), DeepSeek-reasoner `reasoning_content` string field (advisory), Azure OpenAI response identical to OpenAI; verify all paths covered. Update `shared/normalize/openai_chat.go` alias chains; add fixtures.
- T2.2 (OpenAI Responses) Confirm `input_tokens_details.cached_tokens` and `output_tokens_details.reasoning_tokens` paths are covered. If `shared/normalize/` does not yet have a dedicated Responses normalizer (currently the Tier-1 OpenAI normalizer may only handle Chat shape), create one — `shared/normalize/openai_responses.go` — registered under a distinct key.
- T2.3 (Anthropic Messages) Confirm `cache_read_input_tokens`, `cache_creation_input_tokens`, thinking-content stream parsing, and the Bedrock envelope unwrap are covered. Add `bedrock_envelope_test.json` fixture.
- T2.4 (Gemini generateContent) Confirm `cached_content_token_count`, `thoughts_token_count`, and the Vertex envelope unwrap. Add `vertex_envelope_test.json` fixture.
- T2.5 Each gap fix lands with a golden-test fixture (`packages/shared/transport/normalize/testdata/<format>/<scenario>.json`) sourced from vendor docs or a captured trace, with a sibling `.md` recording the source URL and observation date.
- T2.6 Each gap fix lands with corresponding alias chain code + a unit test asserting the alias correctly populates the right `NormalizedPayload.Usage` field.

### T3 — Bridge package

- T3.1 Create `packages/ai-gateway/internal/execution/canonicalbridge/decoder.go` (or extend the existing `canonicalbridge` package created for cross-format requests):
    ```go
    package canonicalbridge

    import (
        "github.com/.../packages/shared/transport/normalize"
        "github.com/.../packages/ai-gateway/internal/providers"
    )

    // DecodeViaShared is the single decode path for ai-gateway codecs.
    // It calls the matching shared/normalize Tier-1 normalizer, then
    // projects the resulting NormalizedPayload into the gateway's
    // wire-shape canonical (OpenAI chat-completions JSON form) plus
    // the canonical providers.Usage.
    //
    // The wire-emission half (EncodeRequest, PrepareBody) stays in
    // each spec_*/. See normalization-architecture.md § "Ai-gateway
    // codec delegation (E58-S0)" for the full contract.
    func DecodeViaShared(
        raw []byte,
        wireFormat providers.Format,
        endpoint providers.Endpoint,
    ) (canonicalBody []byte, usage providers.Usage, err error)
    ```
- T3.2 Internal implementation:
    1. Construct `normalize.Meta` from `wireFormat` + `endpoint` (AdapterType, Direction=Response, EndpointType).
    2. Look up the Tier-1 normalizer in the global registry: `normalize.Registry.Normalize(raw, meta)`.
    3. Convert `NormalizedPayload` to the wire-shape canonical OpenAI JSON using a per-format projection function (`projectToOpenAICanonical(NormalizedPayload) []byte`).
    4. Project `NormalizedPayload.Usage` to `providers.Usage` (alias / direct copy — same fields).
- T3.3 The projection function is per-format because each wire shape has slightly different conventions (Anthropic content blocks → OpenAI content text; Gemini parts → OpenAI content; Anthropic tool_use → OpenAI tool_calls). The projections mirror what the existing codecs do today; we move the code, not redesign it.
- T3.4 Provider-specific extensions ride along: when `NormalizedPayload` carries an Anthropic `cache_creation_input_tokens` or a Gemini `thoughts_token_count` that lacks an OpenAI canonical home, `DecodeViaShared` calls `canonicalext.Set(canonicalBody, "<provider>", key, value)` to stamp them per provider-adapter Rule 4. This preserves today's `hub_ingress` round-trip behavior.
- T3.5 Unit tests per format covering: vanilla request, cache-hit, reasoning, error response. Coverage ≥95 %.

### T4 — Streaming bridge

- T4.1 The streaming variants of each codec (`spec_*/stream.go`) currently maintain their own SSE walker / accumulator. Migrate them to use `shared/normalize/extract/sse.go`'s `WalkSSE(raw, fn)` + the per-format accumulator (`extract/accumulator.go`).
- T4.2 The streaming session interface in ai-gateway (`providers.StreamSession`) keeps its current shape; only the internal parsing is replaced. Each `*Session.Next()` call walks SSE bytes through the shared walker, applies the format-specific accumulator state machine (also from `shared/normalize/extract`), and emits canonical chunks projected from the partial `NormalizedPayload`.
- T4.3 For Anthropic thinking blocks (delta + content blocks) and Gemini thoughts streams, the projection emits canonical-shape chunks with the OpenAI reasoning_content delta convention. The streaming round-trip for downstream clients is preserved.
- T4.4 Unit tests use captured SSE traces from real upstream responses. Coverage ≥95 %.

### T5 — Migrate spec_openai/codec.go

- T5.1 `spec_openai/codec.go`'s `DecodeResponse` becomes:
    ```go
    func (codec) DecodeResponse(endpoint providers.Endpoint, nativeBody []byte) ([]byte, providers.Usage, error) {
        return canonicalbridge.DecodeViaShared(nativeBody, providers.FormatOpenAI, endpoint)
    }
    ```
- T5.2 Delete the now-dead inline extraction code in the file. Also delete any helper functions in `spec_openai/codec.go` that exist only to support `DecodeResponse` (case-by-case).
- T5.3 `spec_openai/stream.go`'s SSE walker is replaced per T4.
- T5.4 Run `go test ./packages/ai-gateway/internal/providers/spec_openai/...` — all existing tests must pass.

### T6 — Migrate spec_openai/codec_responses_response.go (Responses API)

- T6.1 Same pattern as T5 for the Responses API decode path. The wire format is `providers.FormatOpenAIResponses` (or however the Responses-API constant is named today).
- T6.2 `spec_openai/stream_responses.go` likewise.

### T7 — Migrate spec_anthropic

- T7.1 Same pattern as T5 for Anthropic codec + stream.
- T7.2 The Anthropic-specific `nexus.ext.anthropic.cache_creation_input_tokens` stamping (currently in `spec_anthropic/codec.go`'s DecodeResponse) is moved into the bridge's projection per T3.4. After migration, `spec_anthropic/codec.go` contains only `EncodeRequest` + per-model parameter handling.

### T8 — Migrate spec_gemini

- T8.1 Same pattern as T5 for Gemini codec + stream.
- T8.2 `thoughts_token_count` extraction moves to the shared normalizer; the bridge projects it correctly.

### T9 — Migrate the 14 OpenAI-compat adapters

- T9.1 For each of: spec_azure_openai, spec_deepseek, spec_glm, spec_groq, spec_moonshot, spec_xai, spec_mistral, spec_perplexity, spec_fireworks, spec_together, spec_huggingface, spec_replicate, spec_cohere, spec_minimax —
    - Confirm the adapter's DecodeResponse used the OpenAI Chat parser (either directly or via specutil).
    - Replace with one-line `canonicalbridge.DecodeViaShared(body, providers.FormatOpenAI, endpoint)` call.
    - Delete any per-adapter helpers that wrapped the OpenAI parser.
- T9.2 For wrapper adapters (spec_bedrock for Anthropic-on-AWS, spec_vertex for Gemini-on-GCP):
    - The provider envelope is unwrapped first (Bedrock `responseMetadata` envelope, Vertex top-level shape).
    - The unwrapped inner body is passed to `DecodeViaShared` with the inner wire format (`FormatAnthropic` for bedrock-anthropic, `FormatGemini` for vertex-gemini).
    - The envelope-handling code stays in the wrapper adapter; only the inner parsing delegates.

### T10 — Cross-component consistency test

- T10.1 Create `packages/shared/transport/normalize/consistency_test.go`. For every fixture under `testdata/<format>/*.json`, the test calls all three call paths:
    - Direct: `shared/normalize.Registry.Normalize(body, meta)`.
    - Bridge: `canonicalbridge.DecodeViaShared(body, format, endpoint)` (re-projected back through `parseOpenAICanonical` to `NormalizedPayload` for comparability).
    - Adapter: `shared/traffic/adapters/<format>.Adapter.Normalize(body, meta)`.
- T10.2 Assert the three resulting `NormalizedPayload` values are byte-identical via `cmp.Diff`. Specifically: same Kind, same DetectedSpec (modulo the prefix difference between `pattern:` and direct), same Messages, same Usage.
- T10.3 Assert the projected `providers.Usage` from the bridge equals the projected Usage when the adapter's path is run through the same projection. Identical means identical.
- T10.4 Tests run as part of `go test ./packages/shared/transport/normalize/...` and execute in < 5 s total.

### T11 — Delete duplicated code

- T11.1 Delete `packages/ai-gateway/internal/providers/specutil/usage.go`'s `ExtractOpenAIUsage` and `ExtractReasoningTokens` (now done by the shared normalizer).
- T11.2 Delete any per-adapter `extract*` helpers in `spec_*/` that the migration rendered unused.
- T11.3 Delete the per-adapter SSE walker code in `spec_*/stream.go` superseded by the shared walker.
- T11.4 `grep -r "prompt_tokens_details" packages/` after this task should return hits only in `packages/shared/transport/normalize/` plus a small number of legitimate references (e.g., a comment in `provider-adapter-architecture.md`).
- T11.5 `grep -r "cache_read_input_tokens" packages/` likewise scoped to `shared/normalize/`.
- T11.6 `grep -r "completion_tokens_details" packages/` likewise.
- T11.7 `grep -r "thoughts_token_count" packages/` likewise.

### T12 — Verification: smoke parity across the three consumer paths

- T12.1 Run `tests/scripts/smoke-gateway.py` end-to-end with at least one Anthropic request (cache hit + thinking) and one OpenAI gpt-5 request (reasoning). Capture the resulting `traffic_event` row IDs.
- T12.2 Run `tests/scripts/test-compliance-proxy` for the same vendor responses (or replay them via the compliance proxy). Capture those `traffic_event` row IDs.
- T12.3 Diff the rows via a SQL query: every Usage-derived column should be identical. Cost columns differ only if pricing data differs (validate price-row identity first).
- T12.4 If the diff is empty, the parity invariant holds. If non-empty, root-cause and fix before merging.

### T13 — Architecture-doc trigger update

- T13.1 `architecture-doc-triggers.md` has already been updated to point at `normalization-architecture.md` for the parser-code areas (`packages/shared/transport/normalize/**`, `packages/ai-gateway/internal/execution/canonicalbridge/**`, `spec_*/codec.go` / `stream.go` decode path).
- T13.2 Verify the `provider-adapter-architecture.md` § 3a Rule 8 addition (decoding delegates / encoding stays) is in place.
- T13.3 No changes to the existing `provider-adapter-architecture.md` trigger row; it still covers `spec_*/` for the encoding side.

### T14 — Documentation

- T14.1 `normalization-architecture.md` has been extended with the "Ai-gateway codec delegation (E58-S0)" section (done as part of the requirements-doc-and-arch-doc phase before this SDD).
- T14.2 `architecture.md` § 12.1 has been updated to describe the unified parser layer.
- T14.3 `cost-estimation-architecture.md` § 1 / § 9 have been updated to reference `normalization-architecture.md` instead of the obsoleted `provider-usage-extraction-architecture.md`.
- T14.4 The (now-deleted) `provider-usage-extraction-architecture.md` content is fully subsumed; no doc gap remains.

## Acceptance Criteria

| ID | Acceptance |
|---|---|
| AC-1 | Capability gap audit (T0) is complete; every gap has a corresponding T2 enhancement landed; the audit table is in `docs/developers/specs/e58/e58-s0-audit.md` or this SDD's appendix. |
| AC-2 | `packages/ai-gateway/internal/execution/canonicalbridge/decoder.go` exists with `DecodeViaShared` exported. Unit tests for the four wire formats pass; coverage ≥95 % for the bridge package. |
| AC-3 | `spec_openai/codec.go.DecodeResponse`, `spec_openai/codec_responses_response.go.DecodeResponsesResponse`, `spec_anthropic/codec.go.DecodeResponse`, `spec_gemini/codec.go.DecodeResponse` are one-liner delegations to `DecodeViaShared`. The bespoke parsing code in each has been deleted. |
| AC-4 | `spec_*/stream.go` SSE walkers delegate to `shared/normalize/extract`. The per-adapter SSE walker code has been deleted. |
| AC-5 | All 14 OpenAI-compatible ai-gateway adapters (T9.1) plus the 2 wrappers (T9.2) delegate to `DecodeViaShared`. |
| AC-6 | The cross-component consistency test (T10) is green for every fixture under `testdata/<format>/*.json`. |
| AC-7 | `grep -r "prompt_tokens_details\|cache_read_input_tokens\|completion_tokens_details\|thoughts_token_count" packages/` returns hits only in `packages/shared/transport/normalize/` plus documented exceptions. |
| AC-8 | The smoke parity runs (T12) produce byte-identical Usage-derived `traffic_event` columns between the AI Gateway path and the compliance-proxy path for the same upstream responses. |
| AC-9 | `go test -race -count=1 ./packages/ai-gateway/...` is green (no regressions). |
| AC-10 | `go test -race -count=1 ./packages/shared/transport/normalize/...` is green and includes the new consistency test. |
| AC-11 | Per-package coverage gates from CLAUDE.md hold for `shared/normalize`, `ai-gateway/internal/canonicalbridge`, and the touched `spec_*` packages (≥95 % or grandfathered allowlist entry). |
| AC-12 | `tests/scripts/test-compliance-proxy` end-to-end run passes; the `traffic_event` rows produced match expectations on every Usage column. |

## Data Model

### Canonical `Usage` (single source of truth)

```go
// packages/shared/transport/normalize/types.go (or ai-gateway/providers/types.go,
// finalized in T1.1; the other location becomes a type alias)

type Usage struct {
    PromptTokens        *int   // total input (uncached + cached); Anthropic-normalized to match OpenAI convention
    CompletionTokens    *int   // includes reasoning tokens for billing
    TotalTokens         *int
    CachedTokens        *int   // read-side cache hit
    CacheCreationTokens *int   // write-side cache surcharge (Anthropic only as of 2026-05)
    ReasoningTokens     *int   // transparency only — already in CompletionTokens
}

// In whichever package doesn't host the original definition:
type Usage = <package>.Usage
```

### `canonicalbridge.DecodeViaShared` signature

```go
// packages/ai-gateway/internal/execution/canonicalbridge/decoder.go

type Format = providers.Format

func DecodeViaShared(
    raw []byte,
    wireFormat Format,
    endpoint providers.Endpoint,
) (canonicalBody []byte, usage providers.Usage, err error)
```

### Migration shape per codec

```go
// Before
func (codec) DecodeResponse(endpoint providers.Endpoint, nativeBody []byte) ([]byte, providers.Usage, error) {
    // 80–200 lines of bespoke parsing, alias-chain lookups,
    // SSE re-assembly, canonicalext stamping
}

// After
func (codec) DecodeResponse(endpoint providers.Endpoint, nativeBody []byte) ([]byte, providers.Usage, error) {
    return canonicalbridge.DecodeViaShared(nativeBody, providers.FormatAnthropic, endpoint)
}
```

## Testing strategy

- **Unit (white-box) on `shared/normalize/<format>`**: gap-fill PRs each add fixtures + tests.
- **Unit (white-box) on `canonicalbridge`**: fixture-driven decode tests asserting the projection to canonical + Usage matches expected.
- **Unit (white-box) on each migrated `spec_*/codec.go`**: tests that previously asserted DecodeResponse output continue to pass — the migration must be observably equivalent.
- **Cross-component consistency test (T10)**: the new structural invariant; runs on every PR.
- **Integration smoke (T12)**: end-to-end parity between gateway path and compliance-proxy path on real upstream responses.

## Rollback plan

S0 is a refactor. Rollback options, ranked safest to riskiest:

- **Per-codec revert.** Each `spec_*/codec.go` migration is one commit; reverting any one rolls that codec back to inline parsing while the others keep delegating. The bridge package stays.
- **Full revert.** If the bridge itself has a critical bug, `git revert` the bridge-introduction commit and the codec-migration commits together. The pre-S0 inline-parsing code is recovered from git history. No database schema change is involved.

The cross-component consistency test (T10) is designed to catch parity bugs at PR time, so the rollback path is the safety net not the expected path.

## Open questions for review

1. **Where does the canonical `Usage` struct live?** Today in `packages/ai-gateway/internal/providers/types.go`. Hoisting to `shared/normalize/types.go` makes the audit-pipeline + UI consumer's view feel like the source of truth. Hoisting to `packages/shared/providerusage/` (a new dedicated package) keeps it provider-domain-neutral. Defer the decision to T1.1 with a brief design note before coding.
2. **Tier-1 normalizer registration order.** Today `RegisterDefaultAIBuiltins` registers the OpenAI/Anthropic/Gemini Tier-1 normalizers under their `adapter_type` strings plus 14 OpenAI-compatible aliases. The bridge looks up by `providers.Format`. We need a small mapping `providers.Format → adapter_type string` for the lookup; alternatively register Tier-1 normalizers under `providers.Format` keys directly. Decide before T3.
3. **Should the migration land per-vendor (4 PRs) or all-at-once (1 PR)?** Per-vendor is safer to review but means the consistency test passes for 3 vendors and fails for 1 between PRs. All-at-once is a bigger review burden but the test is green or red atomically. Current draft: per-vendor with the consistency test gated by a per-vendor flag during the migration window, then the flag is removed.
4. **`shared/traffic/adapters/*/normalize.go`'s legacy `ExtractRequest`/`ExtractResponse` surface.** Some hook code consumes the `NormalizedContent` shape, not `NormalizedPayload`. Should we audit and migrate those callers in S0, or leave for a separate story? Current draft: leave for a separate story; document the surface in T0 audit so it's visible.

## Appendix — Per-codec migration checklist template

For each `spec_<format>` package being migrated, fill in:

```
## spec_<format>

- Wire format: providers.Format<X>
- Vendor docs URLs consulted (date):
  - https://...
- Tier-1 normalizer in shared/normalize: <name>.go
- Tier-1 normalizer registration key: <adapter_type>
- Capability gaps filled in T2:
  - <bullet list>
- Fixtures added under testdata/<format>/:
  - <bullet list>
- Codec migration:
  - DecodeResponse: now one-line delegation
  - Stream session: now uses shared/normalize/extract walker + accumulator
- Deleted code (file:lines):
  - <bullet list>
- Tests passing:
  - go test ./packages/ai-gateway/internal/providers/spec_<format>/... ✓
  - cross-component consistency test ✓
```
