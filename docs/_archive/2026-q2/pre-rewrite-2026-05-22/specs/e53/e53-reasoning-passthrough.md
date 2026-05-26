# E53 — Reasoning Content Passthrough

**Status:** Approved 2026-05-15. Architecture decision: option A (nexus.ext
passthrough) over option B (unified DSL). OpenAI Responses API deferred
to separate epic (`docs/_archive/2026-q2/programs/proposal-openai-responses-api-adapter.md`).

**Owner:** nexus

**Predecessor:** E46 (traffic normalize), E48 (passthrough config). The
E46 `NormalizedPayload.ContentReasoning` block and the E28-S6 `nexus.ext`
extension convention are the two existing primitives this epic builds on.

---

## 1. Background

`ContentReasoning` blocks (model chain-of-thought / thinking text) already
flow correctly for DeepSeek, Moonshot, and Kimi — those providers return
`reasoning_content` inline in their OpenAI-compatible Chat Completions
response, and `packages/shared/transport/normalize/openai_chat.go:75-186` extracts it.

Two cases are broken or partial:

1. **Anthropic Claude through OpenAI-spec ingress**. A client calling
   `/v1/chat/completions` with `model: "claude-opus-4-7"` cannot trigger
   Claude's extended thinking mode, because the OpenAI Chat schema has no
   `thinking` field and the AI Gateway does not currently translate any
   extension to `thinking: {type: "enabled"}` in the upstream Anthropic
   request body. The reasoning text is therefore never produced.
2. **Gemini 2.5 across all ingress paths**. The Gemini codec extracts
   `thoughtsTokenCount` for usage accounting but never reads the
   `parts[].thought = true` candidate parts as reasoning content. Even if
   the upstream Gemini API returns thinking text (e.g. when
   `generationConfig.thinkingConfig.includeThoughts = true` is set), Nexus
   drops it on the floor. Additionally, there is no client-side mechanism
   to set `includeThoughts: true` through the OpenAI-spec ingress.

OpenAI o-series and gpt-5.x reasoning content is **not** addressed in
this epic; see `docs/_archive/2026-q2/programs/proposal-openai-responses-api-adapter.md`.

## 2. Functional requirements

### FR-1 (Must) — Gemini wire-format correctness
The AI Gateway's Gemini response codec MUST extract `parts[].thought = true`
candidate parts as canonical `ContentReasoning` blocks, both for non-stream
responses and SSE streams. This is independent of whether the client
requested thinking — it ensures wire-format integrity for any upstream
response that surfaces thinking text.

### FR-2 (Must) — Anthropic thinking passthrough via OpenAI-spec ingress
A client calling `POST /v1/chat/completions` MAY include
`nexus.ext.anthropic.thinking` as a top-level object in the request body.
When the request routes to an Anthropic-protocol upstream (native Anthropic
provider or Bedrock Claude), the AI Gateway MUST inject that object as
the `thinking` field in the upstream Anthropic request body. Other
ingress paths (e.g. native `/v1/messages`) ignore the field — clients
calling Anthropic natively already use the `thinking` key directly.

### FR-3 (Must) — Gemini thinkingConfig passthrough via OpenAI-spec ingress
A client calling `POST /v1/chat/completions` MAY include
`nexus.ext.gemini.thinking_config` as a top-level object in the request
body. When the request routes to a Gemini-protocol upstream, the AI Gateway
MUST inject that object as `generationConfig.thinkingConfig` in the upstream
Gemini request body.

### FR-4 (Must) — Reasoning token accounting in audit
`traffic_event` MUST capture per-request reasoning token counts in a new
`reasoning_tokens INTEGER NULL` column. The AI Gateway's audit pipeline
populates it from `usage.completion_tokens_details.reasoning_tokens`
(OpenAI / OpenAI-compat shape) or `usage.thoughtsTokenCount` (Gemini shape)
or `usage.thinking_tokens` (Anthropic shape, when present). NULL when
upstream does not report it.

### FR-5 (Should) — Hooks pipeline reasoning coverage
The compliance hooks pipeline (PII detection, sensitive-word block,
redaction rules) MUST iterate over **all** `content[].type` values
including `reasoning`, not only `text`. Reasoning content can contain
PII and policy-relevant text; treating it as exempt would create an
audit gap. If the pipeline already iterates all types, no code change
is required, but the audit MUST be recorded with a citation in the SDD.

## 3. Non-functional requirements

### NFR-1 — Backward compatibility
Clients that do not send `nexus.ext.*` extensions MUST observe no change
in behavior. The `reasoning_tokens` column MUST default to NULL for
historical rows and tolerate NULL in all SELECT paths (dashboards,
analytics, exports).

### NFR-2 — Performance
The codec changes MUST add no measurable latency. `canonicalext.Get` is a
single gjson path read on bytes already in memory (microseconds). The new
`reasoning_tokens` column write is a single integer addition to the existing
INSERT.

### NFR-3 — Wire-format integrity
The extension passthrough MUST emit valid JSON in the upstream request
body. Specifically the Gemini path MUST handle the case where the client
sends `nexus.ext.gemini.thinking_config` but no other `generationConfig`
keys (must wrap correctly), and the case where `generationConfig` is
already partially populated by the codec for `temperature`, `topP`, etc.
(must merge correctly).

### NFR-4 — Failure isolation
Malformed `nexus.ext.*` payloads (wrong type, unknown subkey) MUST NOT
break the request. The codec logs a warning via `canonicalext.WarnOnce`
and proceeds as if the extension were absent.

### NFR-5 — Observability
A Prometheus counter `nexus_aigw_reasoning_passthrough_total{provider, action}`
MUST increment on every extension injection (`action=injected`) and every
malformed-payload skip (`action=skipped_malformed`). This lets operators
verify the feature is actually being used and detect client misuse.

## 4. User roles

| Role | Interest |
|------|----------|
| API consumer (developer) | Wants reasoning content visible in responses without provider-specific code branches. Accepts that they must opt in via `nexus.ext.<provider>.*`. |
| Audit operator | Wants reasoning content visible in `traffic_event_normalized` for compliance review and incident investigation. |
| Billing / finance | Wants `reasoning_tokens` separately tracked for cost analysis (reasoning often dominates token cost for o-series-equivalent models). |
| Hook operator | Wants confidence that PII / sensitive-content rules apply to reasoning text, not just visible text. |

## 5. Constraints & assumptions

### Constraints
- C-1: No new `provider_pricing` rows or pricing logic — reasoning cost is
  already billed by upstream as part of `output_tokens`. `reasoning_tokens`
  is a sub-count, not an additional billable category.
- C-2: No OpenAPI breaking changes. The `nexus.ext` field is additive on
  the request side; `reasoning_tokens` is additive on the audit row.
- C-3: No new endpoint or admin API. All changes are within the existing
  `/v1/chat/completions` request/response cycle and the audit pipeline.

### Assumptions
- A-1: Upstream Anthropic and Gemini APIs honor the `thinking` /
  `thinkingConfig` parameters as documented. If a provider silently
  drops them, the response just lacks reasoning content — the gateway
  did its job and is not at fault.
- A-2: Operators are willing to opt clients in via `nexus.ext.<provider>.*`
  rather than have the gateway auto-enable thinking by default. Default
  remains "no thinking" to preserve cost predictability for clients that
  did not explicitly request it.
- A-3: The hooks pipeline today iterates content blocks by index, not by
  type-name filter. (Verified during s5; SDD records the citation.)

## 6. Glossary

- **`nexus.ext.<provider>.<key>`** — passthrough convention defined in
  `docs/developers/specs/e28/e28-s6-canonical-hub-completeness.md` §2.5. Allows clients to
  pass provider-specific fields through the canonical body without breaking
  the canonical schema.
- **`ContentReasoning`** — canonical content block type (`content[].type =
  "reasoning"`) per `docs/users/product/architecture.md` §791. Used to carry chain-of-
  thought text alongside visible assistant text.
- **`thinking` (Anthropic)** — extended thinking mode. Client sets
  `thinking: {type: "enabled", budget_tokens: N}` in request, Anthropic
  emits `thinking_blocks` in response.
- **`thoughtsTokenCount` (Gemini)** — token count of internal reasoning,
  reported in `usageMetadata`. Reasoning text appears in `candidates[].
  content.parts[].thought = true` only when `thinkingConfig.includeThoughts
  = true` was set in the request.

## 7. Priority

| Requirement | MoSCoW |
|-------------|--------|
| FR-1 Gemini wire-format correctness | **Must** |
| FR-2 Anthropic thinking passthrough | **Must** |
| FR-3 Gemini thinkingConfig passthrough | **Must** |
| FR-4 reasoning_tokens audit column | **Must** |
| FR-5 hooks pipeline reasoning coverage | **Should** |
| NFR-1 backward compatibility | **Must** |
| NFR-2 performance | **Must** |
| NFR-3 wire-format integrity | **Must** |
| NFR-4 failure isolation | **Must** |
| NFR-5 Prometheus counter | **Should** |

## 8. Out of scope

- **OpenAI o-series / gpt-5.x reasoning content visibility** — requires
  Responses API adapter; see proposal doc.
- **Unified reasoning DSL** — explicitly rejected in design phase; see
  rationale in this epic's brainstorm trail.
- **Reasoning-aware routing rules** — clients deciding "send to Claude if
  reasoning needed" is a routing-rules feature, separate from passthrough.
- **Cost re-pricing of reasoning_tokens** — `provider_pricing.output_usd_per_m`
  already covers them; no separate column or formula.

## 9. Architecture impact

**NONE.** This epic uses two existing architectural primitives:

- `nexus.ext.<provider>.<key>` passthrough convention — `docs/users/product/architecture.md:102` and SDD `docs/developers/specs/e28/e28-s6-canonical-hub-completeness.md` §2.5.
- `ContentReasoning` canonical block — `docs/users/product/architecture.md:791-792`.

No service boundaries, deployment topology, or external integrations change.

## 10. Stories

| Story | Title | File |
|-------|-------|------|
| s1 | Gemini response thought-part extraction (wire-format fix) | `docs/developers/specs/e53/e53-s1-gemini-response-thought-extraction.md` |
| s2 | Anthropic thinking passthrough via nexus.ext | `docs/developers/specs/e53/e53-s2-anthropic-thinking-passthrough.md` |
| s3 | Gemini thinkingConfig passthrough via nexus.ext | `docs/developers/specs/e53/e53-s3-gemini-thinking-passthrough.md` |
| s4 | reasoning_tokens audit column + Hub consumer write | `docs/developers/specs/e53/e53-s4-reasoning-tokens-audit-column.md` |
| s5 | Hooks pipeline reasoning coverage audit | `docs/developers/specs/e53/e53-s5-hooks-pipeline-reasoning-coverage.md` |

OpenAPI: `docs/users/api/openapi/ai-gateway/e53-reasoning-passthrough.yaml`
