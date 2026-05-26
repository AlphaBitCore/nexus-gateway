# E47 S4 — RouterLLMClient interface decoupling

**Epic:** E47
**Requirements:** [e47-routing-canonical-payload.md](../../../../docs/developers/specs/e47/e47-routing-canonical-payload.md) — Must M4
**OpenAPI:** none (internal Go interface; no external surface)
**Status:** Approved (user-approved plan 2026-05-13)
**Date:** 2026-05-13
**Builds on:** [e47-s2-routing-context-canonical-payload.md](e47-s2-routing-context-canonical-payload.md), [e47-s3-safe-headers-trust-boundary.md](e47-s3-safe-headers-trust-boundary.md)

---

## Architecture summary

After S2 and S3, the smart strategy reads `rctx.Request.Messages` and writes a router-LLM HTTP request through `s.deps.Adapters.Get(format).Execute(...)`. That call chain still couples the strategy directly to the provider adapter registry, the provtarget resolver, the canonical-OpenAI JSON wire format, and the HTTP status-code vocabulary. None of these concerns are routing-decision concerns — they are implementation details of "how does the router actually compute the decision".

S4 extracts those concerns into a `Decider` interface in a new sub-package, `internal/router/routerllm`. The interface is:

```go
package routerllm

type Decider interface {
    Decide(ctx context.Context, req Request) (Decision, error)
}

type Request struct {
    SystemPrompt     string                // already has {modelCatalog} substituted
    UserMessages     []normalize.Message   // role=user only (strategy pre-filters)
    Temperature      float64
    MaxTokens        int
    Timeout          time.Duration
    RouterProviderID string                // identifies the LLM that decides
    RouterModelID    string
}

type Decision struct {
    ModelID    string  // catalog Model.code
    ProviderID string  // optional disambiguator
    Reason     string  // human-readable; surfaces in trace
}
```

The strategy after S4 reads `rctx.Request`, filters user content, builds the system prompt with model catalog, hands a fully-prepared `routerllm.Request` to `s.deps.RouterLLM.Decide`, and consumes a `Decision`. The strategy does not import `providers`, does not invoke `provtarget.Resolver`, does not marshal JSON, does not see HTTP status codes.

The production implementation `routerllm.AdapterDecider` holds the provtarget Resolver + the adapter registry and performs the actual LLM call. Tests for the strategy use a `routerllm.Fake` that returns scripted Decisions and errors. Tests for the AdapterDecider use the existing `fakeRouterAdapter` + `fakeSmartResolver` fixtures, now living alongside the AdapterDecider rather than alongside the strategy.

### Why a sub-package, not just an interface in `router`

Putting `routerllm.Decider` in the same package as `SmartStrategy` would let the strategy reach across the interface boundary and still touch private state. A sub-package enforces the boundary at compile time — the strategy can only see the exported `routerllm.Decider`, `Request`, `Decision`, and `NewAdapterDecider` symbols. Future replacements of the decider (local classifier, rule engine, ML model) plug into the same interface; the strategy needs no change.

### Errors carry their trace text

`Decide` returns `(Decision, error)`. On any failure the error's `Error()` string is suitable for direct insertion as the trace entry's `Decision` field — the strategy does not need a typed error envelope. Error text matches the existing trace vocabulary so post-S4 audit `routing_trace` rows are byte-identical to pre-S4 for the same failure mode:

| Failure | Error text |
|---|---|
| provtarget resolve failed | `"router target resolve failed: <inner>"` |
| router provider has unsupported `adapter_type` | `"invalid adapter_type on router provider \"<name>\" (\"<format>\")"` |
| no adapter registered for format | `"no adapter for router provider \"<name>\" (format \"<format>\")"` |
| adapter call timed out | `"router LLM timeout (<n>ms)"` |
| adapter call returned non-2xx | `"router LLM error: <status>"` |
| adapter call returned other error | `"router LLM error: <inner>"` |
| response body unparseable | `"failed to parse router response"` |

The strategy's trace emission is now a single line:

```go
*trace = append(*trace, TraceEntry{
    StrategyType: "smart",
    Decision:     err.Error(),
    DurationMs:   int(time.Since(start).Milliseconds()),
})
return smartFallback(ctx, cfg, s.deps, trace, start)
```

### Package structure

```
packages/ai-gateway/internal/router/routerllm/
  client.go              // Decider interface, Request, Decision, package doc
  adapter_decider.go     // AdapterDecider — wraps provtarget.Resolver + adapter registry
  prompt.go              // routerRequestBody / routerMessage / textOf / buildRouterRequestBody / parseRouterResponse (moved from strategy_smart.go)
  adapter_decider_test.go
  prompt_test.go         // covers buildRouterRequestBody + parseRouterResponse
```

The four `TestBuildRouterRequestBody_*` tests added in S2 move from `router/strategy_smart_test.go` to `routerllm/prompt_test.go` along with the function under test. The integration-style smart strategy tests (`TestSmart_AnthropicRouterViaAdapter`, `TestSmart_ResolverFailure_FallsBack`, etc.) stay in `router/` but switch their fixtures from `fakeRouterAdapter` + `fakeSmartResolver` to a single `fakeDecider`.

### Migration impact on SmartDeps

```go
// Before
type SmartDeps struct {
    Store    SmartStore
    Lookup   TargetLookup
    Resolver provtarget.Resolver   // -> moves into routerllm.AdapterDecider
    Adapters adapterLookup         // -> moves into routerllm.AdapterDecider
    Logger   *slog.Logger
}

// After
type SmartDeps struct {
    Store     SmartStore
    Lookup    TargetLookup
    RouterLLM routerllm.Decider
    Logger    *slog.Logger
}
```

`main.go` constructs the AdapterDecider once and passes it through:

```go
routerLLM := routerllm.NewAdapterDecider(ptResolver, adapterReg, logger)
smartDeps = &router.SmartDeps{
    Store:     router.NewSmartStoreDB(cacheLayer),
    Lookup:    routerResolver.LookupTargetFunc(),
    RouterLLM: routerLLM,
    Logger:    logger,
}
```

---

## Story

### S4 — RouterLLMClient interface decoupling

**User story:** As a gateway engineer building a future smart-routing variant (local classifier, rule engine, ML model), I want the routing decision to depend on a typed `Decider` interface so that I can swap the implementation without touching `SmartStrategy.Evaluate` or any provider-adapter wiring.

**Tasks:**

- **T4.1** — Create `packages/ai-gateway/internal/router/routerllm/client.go`:
  - Package doc-comment describing the L4-internal seam and the typed error contract.
  - `Decider` interface with the single `Decide(ctx, Request) (Decision, error)` method.
  - `Request` struct as specified above (SystemPrompt, UserMessages, Temperature, MaxTokens, Timeout, RouterProviderID, RouterModelID).
  - `Decision` struct (ModelID, ProviderID, Reason).

- **T4.2** — Create `packages/ai-gateway/internal/router/routerllm/prompt.go`:
  - Move `routerRequestBody`, `routerMessage`, `defaultSmartSystemPrompt`, `buildRouterRequestBody`, `parseRouterResponse`, `textOf`, `codeBlockRe` from `router/strategy_smart.go` into this file.
  - `buildRouterRequestBody` keeps its current signature but lives here; the strategy no longer needs to call it directly. (`AdapterDecider.Decide` calls it.)
  - Adjust internal package references: this file is in `package routerllm` and may need the strategy's `SmartConfig` for the Temperature/MaxTokens defaults — instead, pass those values pre-resolved via `Request.Temperature` and `Request.MaxTokens` (the strategy resolves the defaults at call time via `cfg.temperature()` etc.).
  - The system-prompt template stays here (`defaultSmartSystemPrompt`) because it is a prompt-format implementation detail — the strategy passes `Request.SystemPrompt` empty-string when it wants the default; `AdapterDecider` substitutes it.

- **T4.3** — Create `packages/ai-gateway/internal/router/routerllm/adapter_decider.go`:
  - `AdapterLookup` interface (`Get(providers.Format) (providers.Adapter, bool)`) — local to this package so the strategy doesn't need to know about it.
  - `AdapterDecider` struct: `{resolver provtarget.Resolver; adapters AdapterLookup; logger *slog.Logger}`.
  - `NewAdapterDecider(provtarget.Resolver, AdapterLookup, *slog.Logger) *AdapterDecider` constructor.
  - `Decide` method implementing the full pipeline: resolve target → check format → get adapter → build request body → marshal → call adapter.Execute → check status → parse response → return Decision. Error messages match the table above for trace text fidelity.
  - All non-success paths log at Warn level via the supplied logger.

- **T4.4** — Create `packages/ai-gateway/internal/router/routerllm/prompt_test.go`:
  - Move the four `TestBuildRouterRequestBody_*` tests from `router/strategy_smart_test.go`.
  - Move `TestParseRouterResponse` if it exists (likely yes — let me check during implementation).

- **T4.5** — Create `packages/ai-gateway/internal/router/routerllm/adapter_decider_test.go`:
  - Use a fake `Adapter` and fake `provtarget.Resolver` (copy / adapt the existing fakes from `router/strategy_smart_test.go`).
  - Cover at least: (a) happy path — resolve succeeds, adapter returns valid JSON, Decision parsed; (b) resolve fails; (c) adapter returns 500 status; (d) adapter call times out; (e) response body unparseable.

- **T4.6** — Modify `packages/ai-gateway/internal/router/strategy_smart.go`:
  - Delete `routerRequestBody`, `routerMessage`, `defaultSmartSystemPrompt`, `buildRouterRequestBody`, `parseRouterResponse`, `textOf`, `codeBlockRe` from this file (moved to routerllm).
  - Delete the `adapterLookup` interface (moved to routerllm).
  - Update `SmartDeps`: drop `Resolver provtarget.Resolver` and `Adapters adapterLookup`; add `RouterLLM routerllm.Decider`.
  - Rewrite the call site in `Evaluate`: build system prompt with catalog, filter user messages, call `s.deps.RouterLLM.Decide`, on error append trace + smartFallback, on success use `decision.ModelID`/`decision.ProviderID` for the existing `resolveSelectedModelID` call.
  - Remove now-unused imports: `encoding/json`, `regexp`, `providers`, `provtarget`. Keep `strings` if anything else still uses it.

- **T4.7** — Modify `packages/ai-gateway/internal/router/strategy_smart_test.go`:
  - Delete the four `TestBuildRouterRequestBody_*` tests (moved).
  - Delete `fakeRouterAdapter` and `fakeSmartResolver` fixtures (moved or replaced).
  - Introduce `fakeDecider` implementing `routerllm.Decider` with scripted return values.
  - Update `newSmartFixture` to wire `fakeDecider` into `SmartDeps.RouterLLM`.
  - Update `TestSmart_AnthropicRouterViaAdapter` and friends to assert on `fakeDecider.lastRequest` instead of the adapter request.

- **T4.8** — Modify `packages/ai-gateway/cmd/ai-gateway/main.go`:
  - Build the `AdapterDecider` once: `routerLLM := routerllm.NewAdapterDecider(ptResolver, adapterReg, logger)`.
  - Pass via `SmartDeps.RouterLLM`. Drop the `Resolver` and `Adapters` fields.

- **T4.9** — Run `go build`, `go test -race -count=1`, and `go vet` across `packages/ai-gateway/...`. Router + routerllm + requestcontext packages must be green. The 3 pre-existing handler test failures (latent since 59b286b3) remain unrelated.

**Acceptance:**

- `grep -rn "encoding/json" packages/ai-gateway/internal/router/strategy_smart.go` returns **zero**.
- `grep -rn "providers\." packages/ai-gateway/internal/router/strategy_smart.go` returns **zero**.
- `grep -rn "provtarget" packages/ai-gateway/internal/router/strategy_smart.go` returns **zero** (besides any stale comment).
- The strategy file no longer contains `routerRequestBody`, `parseRouterResponse`, or `buildRouterRequestBody`.
- `SmartDeps.Resolver` and `SmartDeps.Adapters` fields removed; `SmartDeps.RouterLLM` field present.
- Existing smart-strategy tests continue to pass, exercising the strategy via `fakeDecider` rather than against a fake provider adapter.
- The error text vocabulary documented in the table above is preserved verbatim — audit traces emitted by the strategy in post-S4 production are byte-identical to pre-S4 for the same failure case.

**Validation script (for the reviewer):**

```bash
go build ./packages/ai-gateway/...
go test -race -count=1 ./packages/ai-gateway/internal/router/... ./packages/ai-gateway/internal/router/routerllm/...
go vet ./packages/ai-gateway/...

# Strategy is now adapter-free
grep -n "encoding/json\|providers\.\|provtarget\." packages/ai-gateway/internal/router/strategy_smart.go  # expected: zero

# Sub-package exists and is self-contained
ls packages/ai-gateway/internal/router/routerllm/
test -f packages/ai-gateway/internal/router/routerllm/client.go
```
