# E56 — AI Gateway `/v1/responses` ingress (OpenAI Responses API)

**Status:** in design (2026-05-16)
**Owner:** platform
**Triggered by:** OpenAI's Responses API (`/v1/responses`) is now the recommended surface
for new OpenAI integrations (reasoning models, built-in tools, server-side
conversation state). AI Gateway today serves only `/v1/chat/completions`,
`/v1/embeddings`, `/v1/models`, `/v1/completions` (legacy) on the OpenAI-compat
family, plus `/v1/messages` (Anthropic) and Gemini-native paths. Customers
adopting Responses API in their applications cannot route that traffic through
Nexus without losing observability, hooks, quota, and policy enforcement.

## Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| F-1 | AI Gateway exposes `POST /v1/responses` with the OpenAI Responses-API request schema (`model`, `input` as string or input-message array, `instructions`, `tools[]`, `tool_choice`, `reasoning.effort`, `max_output_tokens`, `temperature`, `top_p`, `parallel_tool_calls`, `metadata`, `stream`, `text.format`, `truncation`, `include`, `previous_response_id`, `store`). Authentication is virtual-key bearer (same as `/v1/chat/completions`). | Must |
| F-2 | Non-streaming responses return the Responses-API response envelope (`id`, `object: "response"`, `created_at`, `status`, `model`, `output[]` with `message` / `function_call` / `reasoning` item types, `usage` with `input_tokens` / `output_tokens` / `total_tokens` / `output_tokens_details.reasoning_tokens` / `input_tokens_details.cached_tokens`). | Must |
| F-3 | Streaming (`stream: true`) emits the SSE event grammar: `response.created`, `response.in_progress`, `response.output_item.added`, `response.content_part.added`, `response.output_text.delta`, `response.output_text.done`, `response.content_part.done`, `response.output_item.done`, `response.function_call_arguments.delta`, `response.function_call_arguments.done`, `response.reasoning_summary_text.delta`, `response.reasoning_summary_text.done`, `response.completed`, `response.failed`, `response.incomplete`. Each event carries a monotonic `sequence_number` and (where applicable) `output_index` / `content_index`. | Must |
| F-4 | The canonical format remains OpenAI chat-completions shape (per `provider-adapter-architecture.md` §3a Rule 1). A new `FormatOpenAIResponses` constant is added to `providers.Format`; a new `EndpointResponsesAPI` is added to `providers.Endpoint`. Codecs in `spec_openai` translate Responses-API ⇄ canonical in both directions. | Must |
| F-5 | When the routing target's `Manifest.RequestShapes` includes `"responses-api"` (initially only `spec_openai`), the body is forwarded verbatim — no canonicalization, no field stripping, no JSON rewrite — beyond the existing `PassthroughRewrite` model name rewrite. Stateful fields (`previous_response_id`, `store: true`) and OpenAI-native built-in tools (`web_search`, `file_search`, `computer_use_preview`, `image_generation`, `mcp`, `code_interpreter`) reach upstream untouched. | Must |
| F-6 | When the routing target's `Manifest.RequestShapes` does NOT include `"responses-api"` (cross-format path — e.g. ingress=responses, target=Anthropic), the gateway canonicalizes the request to chat-completions, sends to the target's native wire, then re-encodes the canonical response back into Responses-API shape on egress (output items + usage in Responses form). Streaming is symmetric: the stream session emits well-formed `response.*` events even though the upstream wire was Anthropic SSE / Gemini chunked / etc. | Must |
| F-7 | Cross-format guards: on the cross-format path (F-6), the following fields trigger a structured 400 with a Responses-shape error envelope: `previous_response_id` (any value), `store: true`, `truncation` other than `"disabled"`, and any `tools[]` entry whose `type` is one of the OpenAI-native built-in tools listed in F-5. Error message identifies the offending field via `error.param` and uses `error.code = "feature_requires_native_responses_target"`. | Must |
| F-8 | Token usage stamping covers the 5 sites mandated by `provider-adapter-architecture.md` §5 (handleNonStream, handleStream, cacheStoreNonStream, cacheStoreStream, cacheRead\*). The Responses-API usage shape (`input_tokens` / `output_tokens` / `total_tokens` / `output_tokens_details.reasoning_tokens` / `input_tokens_details.cached_tokens`) is reconstructed from the canonical `Usage` struct on egress; the canonical struct itself is unchanged. | Must |
| F-9 | Error envelopes follow `provider-adapter-architecture.md` §9.5: non-stream 4xx emits Responses-shape JSON `{"error":{"message","type","param","code"}}`; mid-stream / pre-stream 4xx emits a synthetic `response.failed` SSE event payload, never an OpenAI chat-completions-shape error frame. Same-family passthrough preserves the raw upstream bytes so native OpenAI SDKs still see all upstream fields. | Must |
| F-10 | Quota counters bucket `/v1/responses` traffic under `endpointType="responses"`, isolated from `chat/completions`. Audit envelopes (`traffic_event`, `traffic_event_normalized`) carry `endpoint_type="responses"`. Routing-rule simulation accepts `"responses"` as a valid endpoint string. | Must |
| F-11 | Existing routing-rule logic (model match, smart routing, fallback chain, sticky-key credential pool hashing) works unchanged when ingress is Responses-API — the canonical payload feeding the router is the same chat-completions canonical form, regardless of ingress shape. | Must |
| F-12 | Hooks (req-stage, resp-stage, streaming) evaluate against the canonical chat-completions payload, not Responses-API wire. Hook content extraction (`ExtractText`) handles Responses-shape bodies on ingress and Responses-shape SSE on egress. | Must |
| F-13 | A new skill `.claude/skills/test-openai-responses/SKILL.md` provides a synthetic end-to-end smoke covering text non-stream, text SSE, function-call SSE, structured outputs (`text.format.json_schema`), and reasoning-effort=high non-stream. The skill is wired into `tests/run-all.sh` so `/test-all` exercises Responses-API on every release. | Must |
| F-14 | No defer / mock / stub production code. Per CLAUDE.md "Real implementation only", every shipped Responses-API code path is complete; test code mocks are normal. | Must |
| F-15 | A new per-routing-rule boolean field `preferResponsesAPI` auto-upgrades `/v1/chat/completions` ingress traffic onto OpenAI `/v1/responses` upstream when (a) target adapter is `spec_openai`, (b) the resolved target model natively supports Responses (per empirically-tested prefix list in `spec_openai/responses_model_support.go`), and (c) the flag is `true`. Client always sees chat-completions response shape (encoded by the S11 round-trip). Default `false`; flag off → classic chat-completions path runs unchanged. Cross-format / non-OpenAI targets ignore the flag. A response marker header `x-nexus-upgraded-to: responses-api` is emitted when the upgrade actually fires so callers can observe it. | Must |
| F-16 | Cache-hit token-field alias `input_tokens_details.cached_tokens` is added to `specutil.cachedTokenAliases` so Responses-API cache hits surface in `traffic_event.prompt_cache_tokens` (and the canonical `Usage.PromptCacheTokens`). The alias is exercised by a regression unit test. | Must |

## Non-Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| NF-1 | Same-shape passthrough latency stays within ±5% of `/v1/chat/completions` passthrough baseline on the same model (no extra canonicalization hop on the fast path). | Must |
| NF-2 | Cross-format latency stays within +10ms p99 of equivalent `/v1/chat/completions` cross-format. The Responses-API codec is JSON-on-JSON and should not allocate measurably more than the existing canonical bridge for chat-completions. | Should |
| NF-3 | No new third-party Go dependency outside CLAUDE.md's vetted set. The codec uses `tidwall/gjson` / `tidwall/sjson` (already vetted). | Must |
| NF-4 | English-only artifact rule from CLAUDE.md applies to all new docs, code, comments, OpenAPI strings, UI strings. | Must |
| NF-5 | All new code is exercised by Go unit tests (`go test -race -count=1` green) and at least one integration test under `packages/ai-gateway/internal/handler/integration_test.go`. | Must |
| NF-6 | Architecture conformance per `provider-adapter-architecture.md` §3a Rules 1-7. `/adapter-conformance-check` returns clean before commit. | Must |
| NF-7 | The Responses-API ingress is feature-flag-free. Per CLAUDE.md "no feature flags for rollback" (greenfield/pre-GA) — rollback is `git revert`. | Must |
| NF-8 | Out-of-scope endpoints (`GET /v1/responses/{id}`, `DELETE /v1/responses/{id}`, `POST .../cancel`, `GET .../input_items`) return 501 with a structured Responses-shape error pointing to documentation; they are NOT silently 404 (a 404 looks like a bug to the calling SDK). | Should |

## User Roles & Personas

| Role | Interaction |
|------|-------------|
| Application developer | Uses the OpenAI SDK's `client.responses.create()` (Python / TS / Go) pointed at `https://gateway.example.com/v1/responses`. Expects identical behavior to direct OpenAI for same-shape passthrough; expects best-effort cross-provider compatibility on cross-format routes. |
| Platform operator | Configures routing rules pointing Responses-API traffic at any registered provider. Observes Responses-API traffic in admin UI / metrics with the same dashboards as chat-completions, separated only by `endpointType="responses"` facet. |
| Compliance/security operator | Hooks (PII redact, content classifier, audit policy) fire on Responses-API traffic identically to chat-completions because they read the canonical payload. No special configuration needed per-endpoint. |
| Audit operator | Sees Responses-API traffic in `traffic_event_normalized` with the same `prompt` / `response` extraction quality as chat-completions. `output_items[]` from Responses serializes to canonical messages cleanly. |

## Constraints & Assumptions

- AI Gateway is pre-GA per CLAUDE.md "no backward compatibility, no defer". No feature flag, no phased rollout, no parallel old/new paths.
- The Responses-API request/response schema in this epic targets OpenAI's public Responses API as of 2026-05-16 (per context7 reference snapshot). Future schema drift (new event types, new tool types) is handled by appending to known-key lists and the event-handler switch.
- **Stateful semantics live at OpenAI, not at Nexus.** `previous_response_id` / `store` work only on the same-shape passthrough path. The cross-format guard rejects these explicitly so callers cannot get into a state where Nexus appears to support them but silently doesn't.
- The canonical chat-completions shape is rich enough to model Responses-API's text content + function calls + reasoning. Edge cases requiring richer canonical (e.g. annotation-bearing output_text) are documented as known reductions in `provider-adapter-architecture.md`.
- IAM impact: minimal. `/v1/responses` is a VK-authenticated data-plane route under `ai-gateway`, not an admin endpoint registered in `shellRouteConfig.tsx` / `internal/handler/*` admin handlers. F-15's `preferResponsesAPI` field rides on the existing `RoutingRule` resource (kept on `admin:routing-rules.write`, no new IAM resource, no new sidebar item) — recorded in S11's commit message per CLAUDE.md "API / menu / route changes require IAM impact review" rule.
- DB migration impact: one additive column. `RoutingRule.preferResponsesAPI Boolean @default(false)` is the only schema change in this epic; `traffic_event.endpoint_type` is already a free `text` column so `"responses"` lands without altering it. Migration timestamp uniqueness enforced per CLAUDE.md "Migration timestamp prefix must be unique" rule.
- Model whitelist scope: by user decision 2026-05-16, the gateway does NOT pre-validate that the model supports Responses on the same-shape passthrough path. If a caller sends `gpt-3.5-turbo` on `/v1/responses`, OpenAI returns 400 and we relay verbatim. Conforms to §3a Rule 7 (no speculative rules). The F-15 auto-upgrade path does maintain a per-model support list — but that list is empirically tested (real 200s captured) and only governs the upgrade decision, not the direct-ingress acceptance gate.

## Glossary

| Term | Meaning |
|------|---------|
| Responses-API / Responses ingress | OpenAI's `/v1/responses` request/response shape and SSE event grammar. Distinct from `/v1/chat/completions` shape. |
| FormatOpenAIResponses | New `providers.Format` constant identifying the Responses-API wire format on the ingress side. Sibling to `FormatOpenAI`. |
| EndpointResponsesAPI | New `providers.Endpoint` constant identifying the Responses endpoint kind. Sibling to `EndpointChatCompletions`. |
| Same-shape passthrough | Bridge short-circuit: when target adapter's `Manifest.RequestShapes` includes the ingress shape, body forwarded verbatim. Activated for Responses ingress only when target supports `"responses-api"`. |
| Cross-format canonical bridge | Bridge full path: ingress → canonical chat-completions → target wire → canonical → ingress shape on egress. Activated for Responses ingress when target does NOT support `"responses-api"`. |
| Built-in tools | OpenAI-native Responses-API tool types (`web_search`, `file_search`, `computer_use_preview`, `image_generation`, `mcp`, `code_interpreter`). Allowed on same-shape passthrough only. |
| Stateful fields | Responses-API request fields whose semantics require server-side state (`previous_response_id`, `store: true`, certain `truncation` modes). Same-shape passthrough only. |
| Response item | An entry in the Responses-API output array. Types covered by this epic: `message`, `function_call`, `reasoning`. Other types (web_search_call, file_search_call, image_generation_call, mcp_call, computer_call, code_interpreter_call) flow through on same-shape passthrough only. |

## MoSCoW Summary

- **Must:** F-1 through F-16, NF-1, NF-3, NF-4, NF-5, NF-6, NF-7.
- **Should:** NF-2, NF-8.
- **Could:** Surface annotation-bearing `output_text` (citations, URL refs) in canonical via a new `ContentBlock.Annotations` field. Out of scope for this epic; would land as its own epic alongside Anthropic citations / Gemini groundingMetadata if it ships.
- **Won't:**
  - Server-side state implementation (Nexus-side `previous_response_id` store, response retrieval endpoints, conversation listing). Stateful semantics remain at the OpenAI side via same-shape passthrough only.
  - Native built-in tool execution by Nexus (web_search / file_search / computer / image_gen / mcp / code_interpreter). They ride through on same-shape passthrough; cross-format rejects them per F-7.
  - `GET /v1/responses/{id}` / `DELETE /v1/responses/{id}` / `POST /v1/responses/{id}/cancel` / `GET .../input_items` returning anything beyond a structured 501. No state to retrieve from. (NF-8 covers the 501 shape.)
  - **Streaming SSE-event coalescing.** Responses-API emits more granular events than chat-completions delta (every token can produce `output_item.added` + `content_part.added` + `output_text.delta` + `output_text.done`). User decision 2026-05-16: do NOT merge adjacent deltas in this epic. Re-evaluate only if prod p99 latency or downstream SSE-frame count creates a measured problem.
  - **Gateway-side model whitelist on the direct ingress path.** User decision 2026-05-16: callers sending unsupported models (e.g. gpt-3.5-turbo) on `/v1/responses` get the upstream 400 verbatim. The F-15 auto-upgrade flag maintains a separate empirically-tested support list, but it only governs the upgrade decision, not the direct ingress.
