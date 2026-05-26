# Routing Architecture Redesign — Design Spec

## Goal

Refactor the AI Gateway routing system into three cleanly separated components: Router (strategy selection), TargetExecutor (retry/execution), and NarrowingEngine (policy filtering). Strategies become struct types with interfaces (consistent with Adapter pattern). Handler reduces to a simple pipeline orchestrator.

## Architecture

```
Handler (pipeline orchestrator)
    │
    ├── Router.ResolveTargets(ctx, req) → flat ordered []RoutingTarget
    │       ├── NarrowingEngine.Apply() → narrowing state
    │       ├── StrategyRegistry.Get() → Strategy.Evaluate() → targets
    │       ├── NarrowingEngine.Filter() → filtered targets
    │       └── HealthRanker.Reorder() → health-aware ordering
    │
    ├── Quota.ReserveEstimate(vk, estimatedTokens)
    │
    ├── TargetExecutor.Execute(ctx, targets, req) → ExecutionResult
    │       ├── CredentialLookup (just-in-time per target)
    │       ├── AdapterRegistry.Get() → Adapter.Execute()
    │       ├── HealthTracker.Record()
    │       └── Retry logic (429 same target, 5xx/network switch target)
    │
    ├── Quota.Settle(vk, result.Target, metadata)
    │
    └── Audit.Record()
```

---

## 1. Strategy Interface

Strategies become struct types with an explicit interface, matching the Provider Adapter pattern.

```go
// Strategy evaluates a routing rule node and returns candidate targets.
type Strategy interface {
    // Type returns the strategy identifier (matches RoutingRule.strategyType in DB).
    Type() string

    // Evaluate resolves targets from a strategy node configuration.
    // recurse allows nested evaluation for composite strategies (fallback, conditional).
    Evaluate(ctx context.Context, node StrategyNode, rctx *RoutingContext, recurse RecurseFunc) ([]RoutingTarget, error)
}
```

### Strategy Registry

```go
type StrategyRegistry struct {
    strategies map[string]Strategy
    frozen     bool
}

func (r *StrategyRegistry) Register(s Strategy)
func (r *StrategyRegistry) Freeze()
func (r *StrategyRegistry) Get(typeName string) (Strategy, bool)
```

### Strategy Implementations (7 types)

| Strategy | Struct | Dependencies |
|----------|--------|-------------|
| single | `SingleStrategy` | `TargetLookupFunc` |
| fallback | `FallbackStrategy` | none (uses recurse) |
| loadbalance | `LoadBalanceStrategy` | none (random selection) |
| conditional | `ConditionalStrategy` | none (expression matching) |
| ab_split | `ABSplitStrategy` | none (weighted random) |
| policy | `PolicyStrategy` | none (returns no targets, updates narrowing) |
| smart | `SmartStrategy` | `SmartDeps` (LLM client, model store) |

### Registration

```go
registry := NewStrategyRegistry()
registry.Register(NewSingleStrategy(lookupFn))
registry.Register(NewFallbackStrategy())
registry.Register(NewLoadBalanceStrategy())
registry.Register(NewConditionalStrategy())
registry.Register(NewABSplitStrategy())
registry.Register(NewPolicyStrategy())
registry.Register(NewSmartStrategy(smartDeps))
registry.Freeze()
```

---

## 2. NarrowingEngine

Independent component responsible for policy-based target filtering. Extracted from the current Resolver's stage-0 procedural logic.

```go
type NarrowingEngine struct{}

type NarrowingState struct {
    AllowedModels    []string
    DeniedModels     []string
    AllowedProviders []string
    DeniedProviders  []string
}

// Apply evaluates stage-0 policy rules and accumulates narrowing state.
func (n *NarrowingEngine) Apply(ctx context.Context, policyRules []RoutingRule, rctx *RoutingContext) *NarrowingState

// Filter removes targets that violate narrowing constraints.
func (n *NarrowingEngine) Filter(targets []RoutingTarget, state *NarrowingState) []RoutingTarget
```

Consumers: only Router (currently). Extracted for single responsibility and future extensibility (compliance-driven filtering, cost-based filtering, time-based policies).

---

## 3. Router

Pure strategy selection. Returns a flat, ordered target list with primary + fallback + recovery already merged and sorted.

### Input

```go
type RouteRequest struct {
    Model       string
    Endpoint    string            // "chat/completions", "embeddings"
    VirtualKey  *VKMeta
    Headers     map[string]string
    Messages    string            // for smart routing (model="auto")
}
```

### Output

```go
type RouteResult struct {
    Targets  []RoutingTarget  // flat list: primary + fallback + recovery, sorted by priority
    Trace    []TraceEntry     // strategy evaluation decisions
    RuleID   string
    RuleName string
}
```

### Method

```go
func (r *Router) ResolveTargets(ctx context.Context, req *RouteRequest) (*RouteResult, error)
```

### Internal Flow

```
1. Load rules from cache (30-min TTL + Redis invalidation)
2. Stage 0: NarrowingEngine.Apply(policyRules, routeCtx) → NarrowingState
3. Stage 1: Find primary rule (highest priority non-fallback match)
4. Evaluate primary: Strategy.Evaluate(node, routeCtx, recurse) → primaryTargets
5. Resolve inline fallback chain → fallbackTargets
6. Evaluate recovery rules → recoveryTargets
7. Concatenate: primary + fallback + recovery
8. NarrowingEngine.Filter(allTargets, narrowingState)
9. HealthRanker.Reorder(filtered) — unhealthy targets moved to end (not removed)
10. Return flat RouteResult
```

### Health-Aware Ordering

```go
type HealthRanker struct {
    tracker *HealthTracker
}

// Reorder moves unhealthy targets to the end of the list without removing them.
// Healthy targets retain their original relative order.
func (h *HealthRanker) Reorder(targets []RoutingTarget) []RoutingTarget
```

The Router checks HealthTracker state and reorders targets so that healthy providers are tried first. Unhealthy targets are not removed (they may have recovered), just deprioritized.

---

## 4. TargetExecutor

Encapsulates retry logic, credential resolution, and health recording. The handler calls it with one line.

### Interface

```go
type TargetExecutor struct {
    adapters      *providers.AdapterRegistry
    credentials   CredentialLookup
    healthTracker *HealthTracker
}

func (e *TargetExecutor) Execute(ctx context.Context, targets []RoutingTarget, req *providers.Request) *ExecutionResult
```

### Result

```go
type ExecutionResult struct {
    Response *providers.Response  // Adapter response (body, metadata, stream)
    Target   RoutingTarget        // the target that succeeded
    Attempts []Attempt            // all attempts for audit/observability
    Error    error                // non-nil if all targets exhausted
}

type Attempt struct {
    Target     RoutingTarget
    StatusCode int
    Error      string
    LatencyMs  int
}
```

### Retry Logic

```go
func (e *TargetExecutor) Execute(ctx context.Context, targets []RoutingTarget, req *providers.Request) *ExecutionResult {
    var attempts []Attempt

    for _, target := range targets {
        adapter, _ := e.adapters.Get(target.ProviderName)
        apiKey, _ := e.credentials.GetForProvider(ctx, target.ProviderID)

        req.BaseURL = target.BaseURL
        req.APIKey = apiKey

        resp := adapter.Execute(ctx, req)
        attempts = append(attempts, buildAttempt(target, resp))

        // Streaming: if stream started successfully, return immediately (can't retry mid-stream)
        if req.Stream && resp.Stream != nil && resp.StatusCode == 200 {
            e.healthTracker.RecordSuccess(target)
            return &ExecutionResult{Response: resp, Target: target, Attempts: attempts}
        }

        // Success
        if resp.Error == nil && resp.StatusCode < 400 {
            e.healthTracker.RecordSuccess(target)
            return &ExecutionResult{Response: resp, Target: target, Attempts: attempts}
        }

        // 4xx: client error, don't retry
        if resp.Error == nil && resp.StatusCode >= 400 && resp.StatusCode < 500 {
            e.healthTracker.RecordSuccess(target) // 4xx is not a provider health issue
            return &ExecutionResult{Response: resp, Target: target, Attempts: attempts}
        }

        // 429: rate limited — retry SAME target after brief wait
        if resp.Error == nil && resp.StatusCode == 429 {
            e.healthTracker.RecordFailure(target)
            time.Sleep(retryBackoff(attempts))
            // Retry same target once
            resp = adapter.Execute(ctx, req)
            attempts = append(attempts, buildAttempt(target, resp))
            if resp.Error == nil && resp.StatusCode < 400 {
                e.healthTracker.RecordSuccess(target)
                return &ExecutionResult{Response: resp, Target: target, Attempts: attempts}
            }
            continue // still failed, try next target
        }

        // 5xx or network error: switch target
        e.healthTracker.RecordFailure(target)
        continue
    }

    return &ExecutionResult{Error: ErrAllTargetsExhausted, Attempts: attempts}
}
```

---

## 5. Quota Flow

```go
// Handler:
// Before execution — rough estimate, not tied to specific target
quota.ReserveEstimate(vk, estimatedInputTokens)

// Execute
execResult := executor.Execute(ctx, targets, req)

// After execution — precise settlement with actual target pricing + actual tokens
meta := execResult.Response.GetMetadata()
quota.Settle(vk, execResult.Target, meta.PromptTokens, meta.CompletionTokens)
```

Quota is a pipeline concern (handler), not an execution concern (executor). Reservation is conservative; settlement is precise.

---

## 6. Handler After Refactor

```go
func (h *Handler) ServeProxy(endpointType string) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        body, model, isStream := h.readRequest(r)

        // 1. VK Auth
        vk := h.authenticate(r)

        // 2. Route — one line
        routeResult, _ := h.router.ResolveTargets(ctx, &RouteRequest{
            Model: model, Endpoint: endpointType,
            VirtualKey: vk, Headers: flattenHeaders(r),
        })

        // 3. Quota — rough reserve
        h.quota.ReserveEstimate(vk, estimateTokens(body))

        // 4. Pre-hooks
        hookInput := buildRequestHookInput(body, r)
        hookResult := h.hookCache.Resolver(ctx).BuildPipeline("request", "AI_GATEWAY", ...).Execute(ctx, hookInput)

        // 5. Execute — one line, no retry/fallback logic here
        execResult := h.executor.Execute(ctx, routeResult.Targets, &providers.Request{
            Endpoint: endpointType, Model: model,
            Body: body, Stream: isStream,
        })

        // 6. Post-hooks
        meta := execResult.Response.GetMetadata()
        respHookInput := buildResponseHookInput(meta, r)
        h.hookCache.Resolver(ctx).BuildPipeline("response", "AI_GATEWAY", ...).Execute(ctx, respHookInput)

        // 7. Quota — precise settle
        h.quota.Settle(vk, execResult.Target, meta)

        // 8. Write response
        if execResult.Response.Stream != nil {
            h.writeStream(w, execResult.Response.Stream)
        } else {
            w.Write(execResult.Response.Body)
        }

        // 9. Audit
        h.audit.Record(vk, execResult, routeResult, hookResult)
    }
}
```

---

## 7. Hook System Amendments

Two additions to the existing Hook architecture (already implemented):

### 7a. HookInput — add RequestID

```go
type HookInput struct {
    RequestID   string  // x-nexus-request-id for traceability
    Stage       string
    Content     []ContentBlock
    // ... all other fields unchanged
}
```

webhook-forward and any future hooks that call external services need a request identifier for correlation.

### 7b. HookResult — add Order

```go
type HookResult struct {
    Order            int    // execution order (0-based) within the pipeline
    HookID           string
    ImplementationID string
    // ... all other fields unchanged
}
```

Pipeline sets `Order` sequentially during execution. Enables audit to show the full hook execution chain with ordering.

---

## 8. File Structure

### New files
| File | Responsibility |
|------|---------------|
| `packages/ai-gateway/internal/router/strategy.go` | Strategy interface + StrategyRegistry |
| `packages/ai-gateway/internal/router/strategy_single.go` | SingleStrategy struct |
| `packages/ai-gateway/internal/router/strategy_fallback.go` | FallbackStrategy struct |
| `packages/ai-gateway/internal/router/strategy_loadbalance.go` | LoadBalanceStrategy struct |
| `packages/ai-gateway/internal/router/strategy_conditional.go` | ConditionalStrategy struct |
| `packages/ai-gateway/internal/router/strategy_absplit.go` | ABSplitStrategy struct |
| `packages/ai-gateway/internal/router/strategy_policy.go` | PolicyStrategy struct |
| `packages/ai-gateway/internal/router/strategy_smart.go` | SmartStrategy struct |
| `packages/ai-gateway/internal/router/narrowing.go` | NarrowingEngine |
| `packages/ai-gateway/internal/router/health_ranker.go` | HealthRanker |
| `packages/ai-gateway/internal/execution/executor/executor.go` | TargetExecutor |

### Modified files
| File | Change |
|------|--------|
| `packages/ai-gateway/internal/router/resolver.go` | Refactor to Router with ResolveTargets() |
| `packages/ai-gateway/internal/router/registry.go` | Replace with StrategyRegistry |
| `packages/ai-gateway/internal/handler/proxy.go` | Remove retry loop, use Executor |
| `packages/ai-gateway/cmd/ai-gateway/main.go` | Wire new components |
| `packages/shared/policy/hooks/types.go` | Add RequestID to HookInput, Order to HookResult |
| `packages/shared/compliance/pipeline.go` | Set Order on HookResult |

### Deleted files
| File | Reason |
|------|--------|
| `packages/ai-gateway/internal/router/strategies.go` | Replaced by individual strategy_*.go files |

---

## 9. Migration Summary

| Before | After |
|--------|-------|
| StrategyFunc closures | Strategy interface + struct types |
| Registry (closures) | StrategyRegistry (typed, freezable) |
| Resolver returns RoutingPlan with Targets + RecoveryTargets | Router.ResolveTargets returns flat ordered list |
| Narrowing procedural in Resolver | NarrowingEngine (independent) |
| No health awareness in routing | HealthRanker reorders targets |
| Handler iterates targets + retry (~50 lines) | Executor.Execute (one line in handler) |
| Handler resolves credentials per target | Executor resolves credentials internally |
| Quota checked against first target | Rough reserve → Execute → precise settle |
| 429 retries same as 500 (switch target) | 429 retries same target, 500/network switches |
| Streaming retries like non-streaming | Streaming: no retry after stream starts |
| HookInput has no request ID | HookInput.RequestID for traceability |
| HookResult has no execution order | HookResult.Order for audit chain |
