# E28 — Story 3: Internal caller migration to new adapter API

## Context

With the new `Adapter` contract (s1) and concrete AdapterSpecs (s2) in place, rewire every internal LLM caller through `provtarget.Resolver` + `Adapter.Execute`. Today three independent code paths each re-implement credential lookup and URL assembly:

| Caller | File | What it does today |
|---|---|---|
| Target executor | `packages/ai-gateway/internal/execution/executor/executor.go` | Resolves API key, reads provider base URL, calls the old `providers.Adapter.Execute` with a manually built request. Handles retries. |
| Smart routing router-LLM call | `packages/ai-gateway/internal/router/strategy_smart.go` | Still contained a hard-coded `"/v1/chat/completions"` before the in-thread patch; now calls `adapter.Execute` but duplicates credential / base-URL lookup inline. |
| AI Guard `configured_provider` | `packages/ai-gateway/internal/pipeline/aiguard/backend_provider.go` + `packages/ai-gateway/cmd/ai-gateway/wiring_aiguard.go` | Builds its own `aiguard.ProviderBackend` with a hardcoded `/chat/completions` path, its own http.Client, its own OpenAI body marshal. |

This story collapses all three onto the shared stack.

## User Story

**As an** AI Gateway maintainer,
**I want** executor, smart routing, and AI Guard to call LLMs through a single (`Resolver` + `Adapter`) path,
**so that** credential rotation, health checks, and schema changes happen in one place.

## Tasks

### 1. Target executor — `packages/ai-gateway/internal/execution/executor/executor.go`

- Constructor accepts `provtarget.Resolver` and `providers.Registry` instead of the current `credentialStore` + `providerStore` mix.
- For each resolved routing target:
  1. `target := resolver.Resolve(ctx, providerID, modelID, ResolveHints{Purpose: "egress"})`.
  2. `adapter, ok := registry.Get(providerFormat(target.ProviderName))` → if not ok, return `ProviderError{Code: "no_compatible_provider"}` and escalate to next target. Provider→Format mapping lives in one helper (s4 also uses it).
  3. `adapter.Execute(ctx, providers.Request{Endpoint, BodyFormat: ingressFormat, Body: rawBody, Headers: filteredHeaders, Stream, Target: target})`.
  4. Retry / fallback logic is unchanged — it now branches on `*providers.ProviderError.Code` (canonical) not on ad-hoc string matching.
- Delete `executor`'s inline API-key resolution, health pick, and base-URL lookup — all moved to `provtarget.Resolver`.
- Keep metrics + observability attributes; add `format` label to the executor metrics so "passthrough vs translated" is visible.

### 2. Smart routing — `packages/ai-gateway/internal/router/strategy_smart.go`

- `SmartDeps` drops `Adapters` and gains `Resolver provtarget.Resolver`, `Adapters providers.Registry`.
- `callRouterLLM()` becomes:
  1. Resolve target once: `target, err := deps.Resolver.Resolve(ctx, smart.RouterProviderID, smart.RouterModelID, ResolveHints{Purpose: "smart_router"})`.
  2. Build canonical OpenAI body with `buildRouterRequestBody(...)` (unchanged), `Body: canonicalBytes`, `BodyFormat: providers.FormatOpenAI`.
  3. `adapter, _ := deps.Adapters.Get(providerFormat(target.ProviderName))` — now the adapter handles URL + body translation + SSE itself; no `/v1/chat/completions` string anywhere in the router.
  4. Parse the returned `Response.Body` (canonical OpenAI) → `RouterDecision`.
- Error handling: if `Resolver.Resolve` returns no healthy key or the registry has no matching adapter, fall through to `smartFallback` as today.
- Delete `smart_store.GetProviderBaseURL` (already declared removed in s1; this is the call-site removal).

### 3. AI Guard `configured_provider` — `packages/ai-gateway/internal/pipeline/aiguard/backend_provider.go` + `cmd/ai-gateway/wiring_aiguard.go`

- Replace the current `ProviderBackend{baseURL, apiKey, http.Client}` with a thin `AdapterBackend{resolver, registry, providerID, modelID, log}`.
- `Classify(ctx, prompt) → Decision` implementation:
  1. Resolve target via `resolver.Resolve(..., ResolveHints{Purpose: "ai_guard", PreferKeyID: cfg.ApiKeyID})`.
  2. Build canonical OpenAI chat-completion body with the classifier system prompt + user prompt; `Stream=false`.
  3. `adapter.Execute(...)` → parse canonical response → return `Decision`.
- `wiring_aiguard.go`:
  - `buildBackend(cfg)` for `mode == "configured_provider"` returns `AdapterBackend` wired with the shared `Resolver` and `Registry`.
  - `buildBackend(cfg)` for `mode == "external_url"` is **unchanged** — its `aiguard.ExternalBackend` keeps its own HTTP client because it targets customer-owned services outside the provider catalog.
- Delete `aiguard.ProviderBackend` struct and any duplicate credential / base-URL helpers the guard owned.

### 4. Provider→Format helper — `packages/ai-gateway/internal/providers/lookup.go`

One helper used by every caller. Source of truth is the provider catalog row's `format` column (already present via Prisma); no string-matching on `ProviderName`.

```go
func FormatOfProvider(providerName string) (Format, bool)
```

Seeded at startup from the provider catalog snapshot; refreshed on catalog shadow updates (same path that already refreshes `providerStore`).

### 5. Metrics and logs

- Add `format` and `purpose` labels to executor request metrics.
- Smart routing log includes `{router_format: "anthropic", router_endpoint: "chat_completions"}` for traceability.
- AI Guard log includes `{guard_format, guard_model, guard_latency_ms}`.

### 6. Unit tests

- `executor_test.go` — table-driven: verify retry-on-next-target when `ProviderError.Code == "rate_limited"`; verify `no_compatible_provider` is terminal; verify `Resolver.Resolve` is called exactly once per target per attempt.
- `strategy_smart_test.go` — use a fake `Registry` returning a scripted `Response`; verify the router parses a routed-model decision correctly; verify fallback on resolver failure; verify no upstream URL strings appear in the router source.
- `aiguard_backend_provider_test.go` — verify `AdapterBackend` issues exactly one `Execute` call per classify with `Endpoint: chat_completions`, `BodyFormat: openai`; verify `external_url` backend still uses the legacy http client.

## Acceptance Criteria

- `grep -R "chat/completions" packages/ai-gateway/internal/{executor,router,aiguard}` returns **zero** matches (all path strings live inside `spec_*/transport.go`).
- `grep -R "providerStore\\.GetBaseURL\\|credentialStore\\.Resolve\\b" packages/ai-gateway/internal/{executor,router,aiguard}` returns zero matches.
- `cmd/ai-gateway/main.go` wires a single `provtarget.Resolver` and passes it to executor + smart deps + AI Guard.
- `go test -race -count=1 ./packages/ai-gateway/internal/{executor,router,aiguard}/...` passes with the rewired callers.
- Smart routing simulate still works end-to-end — and the fix for the bug that originated the redesign (Anthropic router-LLM 404 from hard-coded path) is demonstrably gone in a table-driven test case against a fake anthropic Transport.

## Out of scope

- AI Guard `external_url` rewrite.
- Any change to routing strategies other than `smart`.
- Any change to the provider catalog shadow push pipeline.
