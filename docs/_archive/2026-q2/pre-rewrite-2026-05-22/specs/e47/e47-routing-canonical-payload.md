# E47 — Routing Layer Canonical Payload Alignment

## 1. Background

The smart-routing strategy in `packages/ai-gateway/internal/router/strategy_smart.go` calls a router LLM to pick the upstream model for a request whose client-submitted `model` matches a smart routing rule. The router LLM's decision quality depends on it seeing the user's actual prompt.

Today the handler-to-strategy contract for shipping that prompt is brittle:

1. `packages/ai-gateway/internal/handler/proxy.go` extracts the request's `messages` array **only when the client-submitted `model` equals the string `"auto"`** (`if modelID == "auto"` guard). For any other value it leaves the extracted-messages slot empty.
2. The extracted JSON-array string is shipped to the strategy via `RoutingContext.Headers["x-smart-messages"]` — an internal datum smuggled through a `map[string]string` whose semantic is "external HTTP headers from the client".
3. The smart strategy reads that header value and JSON-unmarshals it to reconstruct the user messages.

Any `RoutingRule.matchConditions` configuration that fires the smart strategy on a non-`auto` model (an operator-broadened match, or accidentally empty `matchConditions: {}` matching every request) breaks the contract silently: the strategy receives an empty header, builds a router-LLM request containing only the system message, and the Anthropic codec rejects it loudly (`encode request: anthropic: no user/assistant messages`) while the OpenAI codec accepts it silently and makes a routing decision without the user's actual prompt. The default-fallback path then absorbs the failure on the Anthropic codec side, masking the bug as a quality regression; on OpenAI-shape routers the customer-paid smart routing degrades to system-prompt-only decisions with no operator signal.

A field-filed bug report (2026-05-12, reverified 2026-05-13) documents the production reproduction. E46 already established a canonical `*normalize.NormalizedPayload` at ingress (L2) and wired the hooks pipeline (L4 consumer) to it; the routing engine — the other major L4 consumer — was not migrated and continues to read from the `x-smart-messages` header. E47 closes that gap.

## 2. Scope

### Must

**M1 — Routing reads canonical payload, not headers.**
- `RoutingContext` carries `Request *normalize.NormalizedPayload`; smart strategy reads it directly.
- `RoutingContext.Headers["x-smart-messages"]` plumbing is **deleted end-to-end**: the writer in `resolver.go`, the reader in `strategy_smart.go`, the simulate-endpoint writer in `routing_simulate_endpoint.go`, and the supporting `RouteRequest.Messages string` field in `router/types.go`.
- The `if modelID == "auto"` guard in `proxy.go` is **deleted**; the gateway emits a `NormalizedPayload` for every request regardless of the client-submitted model.

**M2 — Smart routing works correctly for every matchConditions shape.**
- Smart strategy returns a valid routing decision (a primary model + reason, or an explicit fallback decision) for any `RoutingRule.matchConditions` value, including `{}`, `{"requestedModelLiterals": ["auto"]}`, or any other literal.
- The router LLM call succeeds end-to-end when the router LLM is configured as any supported provider family (OpenAI, Anthropic, Gemini, DeepSeek, GLM, MiniMax). No `"no user/assistant messages"` codec error appears in the audit `routing_trace`.

**M3 — Structural trust boundary for routing-internal data.**
- `RoutingContext.Headers map[string]string` is replaced by a typed `SafeHeaders` whose API exposes `Get(HeaderName) string` over a whitelisted `HeaderName` enum and **does not expose `Set` or raw-map access to external callers**.
- The whitelist contains only headers that routing strategies legitimately read (control headers, `x-nexus-*` family, and any header used by `conditional` strategy predicates today).
- All existing strategy reads of `rctx.Headers[...]` are migrated to `SafeHeaders.Get(HeaderName)`; raw `map[string]string` access from outside the `router` package is no longer possible.

**M4 — Smart strategy depends on a typed `RouterLLMClient` interface.**
- A new `packages/ai-gateway/internal/router/routerllm/` package defines `RouterLLMClient` with `Decide(ctx, RouterRequest) (RouterDecision, error)`.
- The provider-adapter-backed implementation lives in `routerllm` and owns prompt construction, JSON parsing, and timeout handling.
- `strategy_smart.go` does not import provider adapter types; its evaluation function is `Evaluate(ctx, RoutingContext, *SmartConfig) Decision` with no HTTP / JSON / wire-format concerns inside.

**M5 — Smart strategy short-circuits the negative case.**
- When `RoutingContext.Request == nil` or `Request.Kind` is not an AI kind: smart strategy falls back to the configured default with trace `"request payload not normalizable for smart routing; using default"`.
- When `Request.Messages` has no entries with `Role == user` (incl. tools-only or assistant-only payloads): smart strategy falls back to the configured default with trace `"smart routing: no user content in request; using default"`.
- In neither case is the router LLM invoked.

**M6 — Admin API guard for smart-rule matchConditions.**
- Control-plane `RoutingRule` create/update endpoint rejects (HTTP 400) any rule with `strategyType == "smart"` whose `matchConditions` is `{}` or whose `requestedModelLiterals` contains literals other than `"auto"`, unless an explicit force flag is supplied.
- Admin UI surfaces the rejection with a confirm-to-force modal whose body explains the prior bug and links the runbook.

**M7 — Ops audit runbook.**
- `docs/operators/ops/runbooks/r-routing-rule-matchconditions-audit.md` provides the SQL query to find prod smart rules in the foot-gun state, the recommended remediation, and a sign-off checklist.
- The runbook is executed against production immediately after E47 ships.

### Should

**S1 — Reusable `RequestContext` for downstream consumers.**
- A `packages/ai-gateway/internal/pipeline/requestcontext/` package exposes an immutable `RequestContext` (Builder + view functions `ForRouting() / ForHooks() / ForAudit()`).
- The handler builds it once at Phase 3.5, reuses its `NormalizedPayload` for both routing and hooks (no double-parse), and audit consumes the same artefact.
- Existing `extractRequestContentForHooks` (`proxy.go:1298`) is the parse function; S1 promotes it to a single explicit Phase 3.5 call rather than the current hook-internal lazy invocation, **without creating a new `internal/ingress/` package**.

**S2 — Distinguished trace entries.**
- Smart strategy's fallback traces distinguish "no user content" from "payload not normalizable" so operators can triage misconfiguration from genuine non-AI traffic hitting a smart rule.

### Could

**C1 — Conditional strategy gains content-aware predicates.**
- Once `RoutingContext.Request` is available, the conditional strategy's predicate language can be extended to test `Request.Tools != nil`, `Request.Stream`, content-type composition, or prompt-length thresholds. **Not implemented in E47**; the data is available for a future story.

### Won't

**W1 — L5 wire-format egress refactor.**
- Replacing `providers.Request.Body []byte` with `*NormalizedPayload`, and giving each `spec_xxx` an `Encode(*NormalizedPayload) ([]byte, error)` is the symmetric egress work for E46's ingress. Not in E47. Tracked as **E46-S10**.

**W2 — L6 response normalize.**
- Replacing `providers.Response.Body []byte` with `*NormalizedPayload`, and giving each `spec_xxx` a `Decode([]byte) (*NormalizedPayload, error)` is the response-side completion of the canonical pipeline. Not in E47. Tracked as **E46-S11**.

**W3 — compliance-proxy normalize migration.**
- compliance-proxy already consumes `*normalize.NormalizedPayload` via `forward_handler.go:238,519` and `shared/compliance/pipeline.go`. No further migration required.

**W4 — `RoutingRule.matchConditions` historical data correction.**
- The remediation of prod rows is an ops action governed by **M7's runbook**, not code work.

## 3. Non-Functional Requirements

**NFR1 — Performance.**
- E47 does not introduce a second `NormalizedPayload` parse. The handler builds the payload once at Phase 3.5 and shares it across hooks + routing + audit. Worst-case overhead vs the pre-E47 path is the cost of one normalize call for the ~10% of request paths that do not currently invoke hooks; expected median ≤ 500 µs per request at p50.
- Routing decision wall-clock (p50/p99) for non-smart strategies must remain within +200 µs of the pre-E47 baseline.

**NFR2 — Observability.**
- The audit `routing_trace` shape (existing fields, JSON structure) is preserved. The `decision` string for smart-strategy fallback may add new variants (per S2) but no existing variant is removed or renamed.
- New variants are documented in the runbook so operators can triage by exact-match string.

**NFR3 — Test coverage.**
- Smart strategy: unit tests cover (a) populated user content, (b) payload nil, (c) payload not AI-kind, (d) AI-kind with empty `Messages`, (e) AI-kind with no `role=user` entries (assistant-only / tool-only payloads).
- Resolver: unit tests cover the new single-entry `Resolve(ctx, RoutingContext)` signature.
- Handler: integration test reproduces the prod bug exactly per the filed report's "Reproduction" section and asserts the trace's first decision is not `"router LLM error"`.
- SafeHeaders: unit tests cover whitelist enforcement and Get on unknown HeaderName.

**NFR4 — Backwards compatibility.**
- None. Per CLAUDE.md `Development-phase policy: no backward compatibility, no defer`, the old plumbing is deleted in the same PR sequence that introduces the new path. No feature flags, no parallel legacy code paths.

**NFR5 — English-only artefacts.**
- All E47-produced files (`docs/developers/specs/e47-*.md`, `docs/developers/specs/e47-*.md`, `docs/operators/ops/runbooks/r-routing-rule-matchconditions-audit.md`, all source files, all commit messages) are English-only per CLAUDE.md `English only` rule.

## 4. User Roles & Personas

**Gateway operator.** Configures `RoutingRule.matchConditions` and selects router LLMs. Today they can silently misconfigure smart routing into a state where decisions are made on system prompts alone or where the router-LLM call is rejected; post-E47 they cannot configure such a state without an explicit force flag, and the runbook tells them how to audit prior config.

**End customer of the smart-routing feature.** Sends a request, expects a routing decision grounded in the prompt's content. Today's silent-quality-regression path on OpenAI-shape routers degrades their experience; post-E47 the routing decision is correctness-grounded for every request.

**Gateway developer.** Reads `RoutingContext` to understand what data is available to a routing decision; writes new strategies. Today the stringly-typed `Headers map` is a noisy and unsafe surface; post-E47 the typed `Request *NormalizedPayload` + `SafeHeaders` makes the contract self-documenting and IDE-navigable.

**On-call.** Triages a smart-routing trace. Today both "router was called and failed" and "router was called but had no user content" surface as the same kind of `decision` string; post-E47 the negative cases are distinguished and the runbook maps each string to a diagnostic.

## 5. Constraints & Assumptions

**C1 — E46 foundation is in place.** `shared/normalize/` produces `*NormalizedPayload` for OpenAI Chat, Anthropic Messages, Gemini, and GenericHTTP ingress. AI-Guard hook reads NormalizedPayload. Audit redact via TransformSpan works. E47 builds on this; if E46-S5 (TransformSpan + storage redact) had not landed when E47 started, E47 would have blocked. Verified at planning time (commit `b225f5ce`).

**C2 — No concurrent NormalizedPayload churn.** The shared/normalize package schema is frozen for the duration of E47 implementation. Any subsequent E46 follow-up (S10 egress, S11 response) is sequenced **after** E47 completes; rebase-on-top from E46 follow-ups is their problem.

**C3 — `routing-simulate` admin endpoint OpenAPI contract is unchanged.** Request body, response shape, audit trace shape — all preserved. The migration is internal to the handler.

**C4 — `RoutingRule.config` shape is unchanged.** S8's matchConditions guard is a server-side validation rule layered onto the existing JSONB column; no schema migration.

**C5 — Sequential PR cadence.** Per user direction, S1 → S2 → S3 → S4 → S5 → S8 are six independent PRs landing in order. Each must pass `go test -race -count=1 ./...` plus the `/test-all` regression skill before the next starts.

## 6. Glossary

- **RequestContext** — immutable per-request object built once at Phase 3.5, holding `Identity`, `Endpoint`, `*NormalizedPayload`, `SafeHeaders`, and a reference to spilled raw bytes for audit. Has view functions `ForRouting() / ForHooks() / ForAudit()` returning role-specific subsets.
- **RoutingContext** — the routing-strategy-facing view of `RequestContext`. Carries `Requested`, `Endpoint`, `Identity`, `Request *NormalizedPayload`, `SafeHeaders`. Replaces today's struct that has `Headers map[string]string`.
- **SafeHeaders** — opaque struct exposing `Get(HeaderName) string` over a whitelisted enum. Replaces `map[string]string`.
- **HeaderName** — exported string-typed enum (Go pattern: `type HeaderName string` with named constants) of headers that strategies are allowed to read.
- **RouterLLMClient** — interface in `internal/router/routerllm/`: `Decide(ctx, RouterRequest) (RouterDecision, error)`. The smart strategy's only dependency for routing-decision computation.
- **`auto` sentinel** — historical client-submitted `model` value that signals "let the gateway choose". Becomes one valid match value out of many post-E47; not specially-cased by the handler.
- **Smart routing foot-gun** — operator-broadened `matchConditions` that makes the smart rule match non-`"auto"` traffic, exposing the today's missing-messages path. M6's admin-API guard closes the foot-gun.

## 7. Out-of-Scope Cleanups (recorded so they are not lost)

- **L5 wire-format egress (E46-S10)** — providers stop accepting raw bytes; `Encode(*NormalizedPayload) ([]byte, error)` is the only entry. Cross-format translation in `spec_adapter.go:266-310` is deleted. Each spec_xxx (`openai`, `anthropic`, `gemini`, `bedrock`, `cohere`, `mistral`, `replicate`) gets its encoder file.
- **L6 response normalize (E46-S11)** — providers return `*NormalizedPayload` instead of bytes. Streaming SSE emits `NormalizedChunk` sequence. audit + hooks response-side stop re-parsing.
- **Content-aware conditional strategy predicates** — once `RoutingContext.Request` exists, the conditional strategy's predicate language can express `Request.Tools != nil`, `Request.Stream`, content-type composition. Future story, not E47.
- **Memory entry** — both follow-ups are recorded in `memory/project_e47_routing_canonical_payload.md` so they do not get lost when the active session changes.

## 8. Phasing (informative — full breakdown in SDD)

Sequential, one PR per Story:

- **S1** — `internal/requestcontext/` package; promote `extractRequestContentForHooks` to a Phase 3.5 single call.
- **S2** — `RoutingContext.Request` typed field; delete `x-smart-messages` plumbing end-to-end; delete `RouteRequest`; delete `if modelID == "auto"` guard. **THE BUG FIX.**
- **S3** — `SafeHeaders` typed trust boundary; migrate all `rctx.Headers[...]` reads.
- **S4** — `internal/router/routerllm/` package + `RouterLLMClient` interface; strategy dependency-injects it.
- **S5** — smart-strategy negative-case short-circuit with distinguished trace.
- **S8** — admin API guard for `matchConditions`; ops runbook; one-off prod data audit.
