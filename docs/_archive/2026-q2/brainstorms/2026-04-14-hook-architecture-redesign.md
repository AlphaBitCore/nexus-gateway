# Hook Architecture Redesign — Design Spec

## Goal

Replace the current Hook interface (`Execute(ctx, *InterceptedTransaction)`) with a clean `Execute(ctx, *HookInput)` interface where hooks receive only structured, provider-agnostic input injected by the scheduler. Unify config caching across all services with Redis pub/sub invalidation and force-refresh.

## Architecture

Two-layer model matching the Provider Adapter pattern:
- **Hook Type** (class): registered at startup via `HookRegistry`, knows HOW to do its job (e.g., regex matching, PII detection). Stateless registration, stateful instances.
- **Hook Instance** (configuration): created from DB `HookConfig` rows via factory. Holds compiled state (regex patterns, CIDR lists). Cached by `PolicyResolver`, invalidated on config change.

Pipeline scheduler handles orchestration (stage filtering, priority sorting, timeout, fail behavior). Hooks are pure business logic — they receive `HookInput` and return `HookResult`.

---

## 1. Core Types

### HookInput

The **only** data hooks receive. Built by the caller (proxy handler / intercept handler) from Traffic Adapter or Provider Adapter output. No raw JSON, no transport details.

```go
type HookInput struct {
    // Stage this hook is running in
    Stage        string          // "request" / "response" / "connection"

    // Content — extracted by Traffic Adapter (proxy/agent) or Provider Adapter (ai-gateway)
    // Hooks never parse raw provider JSON.
    Content      []ContentBlock

    // AI metadata — populated from Provider Adapter Metadata or Traffic Adapter metadata
    Model        string          // AI model name (may be empty for request stage)
    FinishReason string          // response finish reason (response stage only)
    TokenCount   int             // estimated or actual token count

    // Network context — populated by the caller from request metadata
    SourceIP     string
    TargetHost   string
    Path         string
    Method       string
    IngressType  string          // "AI_GATEWAY" / "COMPLIANCE_PROXY" / "AGENT"

    // Size info
    BodySize     int64
    ContentType  string
}
```

### ContentBlock

Provider-agnostic content unit. Same type used by Provider Adapter (ai-gateway) and Traffic Adapter (shared).

```go
type ContentBlock struct {
    Role string `json:"role"`           // "user", "assistant", "system", "tool"
    Type string `json:"type"`           // "text", "image", "tool_call", "tool_result"
    Text string `json:"text,omitempty"` // text content
    Raw  []byte `json:"raw,omitempty"`  // original JSON for non-text types
}
```

Note: `ContentBlock` is already defined in `packages/ai-gateway/internal/providers/adapter.go`. It must be moved to `packages/shared/policy/hooks/types.go` (or a shared `contentblock` package) so both providers and hooks can use it without circular imports.

### Hook Interface

```go
type Hook interface {
    Execute(ctx context.Context, input *HookInput) (*HookResult, error)
}

type HookFactory func(cfg *HookConfig) (Hook, error)
```

### HookResult

Unchanged from current implementation:

```go
type HookResult struct {
    HookID             string
    ImplementationID   string
    HookName           string
    Decision           Decision           // Approve, RejectHard, RejectSoft, Modify, Abstain
    Reason             string
    ReasonCode         string
    LatencyMs          int
    DataClassification DataClassification // PUBLIC, INTERNAL, CONFIDENTIAL, RESTRICTED
    Error              string
}
```

### HookConfig

Unchanged from current implementation:

```go
type HookConfig struct {
    ID                string         `json:"id"`
    ImplementationID  string         `json:"implementationId"`  // maps to Hook Type
    Name              string         `json:"name"`
    Priority          int            `json:"priority"`
    Enabled           bool           `json:"enabled"`
    Stage             string         `json:"stage"`             // "request", "response", "connection"
    FailBehavior      string         `json:"failBehavior"`      // "fail-open", "fail-closed"
    TimeoutMs         int            `json:"timeoutMs"`
    ApplicableIngress []string       `json:"applicableIngress"` // ["ALL"], ["COMPLIANCE_PROXY"], etc.
    Config            map[string]any `json:"config"`            // hook-specific configuration
}
```

---

## 2. Hook Type Registration

At startup, each service registers Hook Types via `HookRegistry`:

```go
// Shared built-in hooks (packages/shared/policy/hooks/builtins.go)
var Registry = func() *HookRegistry {
    r := NewHookRegistry()
    r.Register("keyword-filter", NewKeywordFilter)
    r.Register("pii-detector", NewPiiDetector)
    r.Register("content-safety", NewContentSafety)
    r.Register("rate-limiter", NewRateLimiter)
    r.Register("request-size-validator", NewRequestSizeValidator)
    r.Register("ip-access-filter", NewIPAccessFilter)
    r.Freeze()
    return r
}()

// AI Gateway extends with Clone()
gwRegistry := hooks.Registry.Clone()
gwRegistry.Register("webhook-forward", NewWebhookForward)
gwRegistry.Register("quality-checker", NewQualityChecker)
gwRegistry.Freeze()
```

`HookRegistry` is freezable (no mutation after startup). `Clone()` creates an unfrozen copy for service-specific extensions.

---

## 3. Hook Instance Lifecycle

```
DB: HookConfig row (dynamic, managed via Control Plane UI)
    │
    ▼
PolicyResolver.resolve(stage, ingressType)
    │
    ├── Filter by: stage, enabled, applicableIngress
    ├── Sort by: priority (ascending)
    ├── For each matching config:
    │       ├── Cache hit? → reuse Hook instance
    │       └── Cache miss? → factory(config) → cache new instance
    │
    ▼
Pipeline.Execute(ctx, input *HookInput)
```

Hook instances are cached by `HookConfig.ID`. Cache is cleared on config `Swap()` (atomic snapshot replacement) so hooks are re-created with new config.

---

## 4. Pipeline Execution

### Interface

```go
// PolicyResolver builds pipelines from config snapshot
func (r *PolicyResolver) BuildPipeline(stage, ingressType string, perHookTimeout, totalTimeout time.Duration, parallel bool, logger *slog.Logger) (*Pipeline, error)

// Pipeline executes hooks
func (p *Pipeline) Execute(ctx context.Context, input *HookInput) *CompliancePipelineResult
```

Note: `BuildPipeline` no longer receives `*InterceptedTransaction`. Stage and ingress type are passed as parameters. The `HookInput` is only needed at `Execute()` time.

### Execution Modes

- **Sequential** (ai-gateway, agent): hooks run in priority order, short-circuit on `RejectHard`
- **Parallel** (compliance-proxy): all hooks run concurrently, context cancelled on first `RejectHard`

### Pipeline Options

- `SetAllowModify(bool)` — AI Gateway enables this; when false, `Modify` downgraded to `Approve`
- `SetClearSoftOnApprove(bool)` — AI Gateway and Agent enable this; `Approve` clears pending `RejectSoft`

### Result Merging

- First `RejectHard` wins (immediate return)
- Any `RejectSoft` without `RejectHard` → pipeline result is `RejectSoft`
- All `Approve`/`Abstain` → pipeline result is `Approve`
- Data classification: highest sensitivity wins across all hooks

---

## 5. Caller Integration

### AI Gateway (proxy handler)

```go
// Request stage — content from Provider Adapter
reqInput := &hooks.HookInput{
    Stage:       "request",
    Content:     providers.ExtractOpenAIRequestContent(body),
    IngressType: "AI_GATEWAY",
    Method:      r.Method,
    Path:        r.URL.Path,
    ContentType: r.Header.Get("Content-Type"),
    BodySize:    int64(len(body)),
}
pipeline := resolver.BuildPipeline("request", "AI_GATEWAY", 5*time.Second, 15*time.Second, false, logger)
result := pipeline.Execute(ctx, reqInput)

// Response stage — content from Provider Adapter Metadata
respInput := &hooks.HookInput{
    Stage:        "response",
    Content:      resp.GetMetadata().ResponseContent,
    Model:        resp.GetMetadata().Model,
    FinishReason: resp.GetMetadata().FinishReason,
    TokenCount:   resp.GetMetadata().TotalTokens,
    IngressType:  "AI_GATEWAY",
    Path:         r.URL.Path,
}
pipeline := resolver.BuildPipeline("response", "AI_GATEWAY", ...)
result := pipeline.Execute(ctx, respInput)
```

### Compliance Proxy (forward handler)

```go
// Request stage — content from Traffic Adapter
nc, _ := adapter.ExtractRequest(ctx, body, path)
reqInput := &hooks.HookInput{
    Stage:       "request",
    Content:     normalizedToContentBlocks(nc), // convert []string → []ContentBlock
    SourceIP:    sourceIP,
    TargetHost:  targetHost,
    Path:        path,
    Method:      method,
    IngressType: "COMPLIANCE_PROXY",
    BodySize:    int64(len(body)),
    ContentType: contentType,
}
```

### Agent (intercept handler)

```go
// Request stage — content from Traffic Adapter
nc, _ := adapter.ExtractRequest(ctx, body, path)
reqInput := &hooks.HookInput{
    Stage:       "request",
    Content:     normalizedToContentBlocks(nc),
    TargetHost:  host,
    Path:        path,
    IngressType: "AGENT",
}
```

### Helper: NormalizedContent → ContentBlock conversion

```go
// normalizedToContentBlocks converts Traffic Adapter output to ContentBlocks
func normalizedToContentBlocks(nc traffic.NormalizedContent) []ContentBlock {
    blocks := make([]ContentBlock, len(nc.Segments))
    for i, seg := range nc.Segments {
        blocks[i] = ContentBlock{Role: "user", Type: "text", Text: seg}
    }
    return blocks
}
```

---

## 6. Streaming Integration

The Pipeline is stateless — same `Execute()` for streaming and non-streaming. The caller manages checkpoints:

```go
// In AI Gateway handleStream():
// Provider Adapter StreamSession accumulates content across chunks.
// At each checkpoint, build HookInput from accumulated content and run pipeline.

accumulatedText := ""
checkpoint := 400 // initial

for chunk := range result.Stream {
    accumulatedText += extractDeltaText(chunk)
    
    if len(accumulatedText) >= checkpoint {
        input := &hooks.HookInput{
            Stage:       "response",
            Content:     []ContentBlock{{Role: "assistant", Type: "text", Text: accumulatedText}},
            IngressType: "AI_GATEWAY",
        }
        pipelineResult := pipeline.Execute(ctx, input)
        if pipelineResult.Decision == RejectHard {
            // cancel stream, send error
            break
        }
        checkpoint += 128 // next checkpoint
    }
    
    // forward chunk to client
}
```

---

## 7. Config Caching — HookConfigCache (shared)

### Component

```go
// packages/shared/compliance/config_cache.go
type HookConfigCache struct {
    db       *sql.DB
    redis    *redis.Client
    resolver *PolicyResolver
    ttl      time.Duration    // default 2 minutes
    logger   *slog.Logger
    mu       sync.Mutex       // guards reload
    lastLoad time.Time
}

func NewHookConfigCache(db *sql.DB, redis *redis.Client, registry *HookRegistry, ttl time.Duration, logger *slog.Logger) *HookConfigCache

// Start begins the Redis subscription and initial DB load.
func (c *HookConfigCache) Start(ctx context.Context) error

// Resolver returns the PolicyResolver with current config snapshot.
func (c *HookConfigCache) Resolver() *PolicyResolver

// Reload forces an immediate reload from DB (called on Redis invalidation or TTL expiry).
func (c *HookConfigCache) Reload(ctx context.Context) error
```

### Behavior

1. **Startup**: Load all enabled HookConfig rows from DB, build PolicyResolver snapshot
2. **Redis subscription**: Listen on `nexus:config:invalidated` for `topic=hooks` — trigger `Reload()`
3. **TTL fallback**: If no Redis event and TTL expires, reload from DB on next `Resolver()` call
4. **Atomic swap**: `Reload()` calls `PolicyResolver.Swap()` atomically
5. **Singleflight**: Concurrent reload requests are deduplicated

### Usage in Each Service

```go
// AI Gateway
cache := compliance.NewHookConfigCache(db, redisClient, gwRegistry, 2*time.Minute, logger)
cache.Start(ctx)
// In request handler:
resolver := cache.Resolver()
pipeline := resolver.BuildPipeline("request", "AI_GATEWAY", ...)

// Compliance Proxy
cache := compliance.NewHookConfigCache(configDB, redisClient, hooks.Registry, 2*time.Minute, logger)
cache.Start(ctx)

// Agent: unchanged (pulls from Gateway API, not DB)
```

---

## 8. Force-Refresh API

### Control Plane Endpoint

```
POST /api/admin/hooks/refresh
```

Handler:
```go
func (h *AdminHandler) HookForceRefresh(c echo.Context) error {
    h.PubSub.PublishInvalidation(c.Request().Context(), "hooks")
    return c.JSON(http.StatusOK, map[string]any{"refreshed": true})
}
```

This publishes to Redis `nexus:config:invalidated` with topic `hooks`. All services with a `HookConfigCache` subscribed to this channel will reload within seconds.

---

## 9. Hook Implementations — Migration

All 8 hooks change from `Execute(ctx, *InterceptedTransaction)` to `Execute(ctx, *HookInput)`:

### keyword-filter
```go
// Before: tx.NormalizedContent
// After:  input.Content[].Text
func (kf *KeywordFilter) Execute(_ context.Context, input *HookInput) (*HookResult, error) {
    for _, block := range input.Content {
        for _, p := range kf.patterns {
            if p.re.MatchString(block.Text) { ... }
        }
    }
}
```

### pii-detector
```go
// Before: tx.NormalizedContent
// After:  input.Content[].Text
func (pd *PiiDetector) Execute(_ context.Context, input *HookInput) (*HookResult, error) {
    for _, block := range input.Content {
        if pd.scanForPII(block.Text) { ... }
    }
}
```

### content-safety
```go
// Before: tx.NormalizedContent
// After:  input.Content[].Text
// Same pattern as keyword-filter
```

### rate-limiter
```go
// Before: tx.SourceIP, tx.TargetHost
// After:  input.SourceIP, input.TargetHost
func (rl *RateLimiter) Execute(_ context.Context, input *HookInput) (*HookResult, error) {
    key := input.SourceIP // or input.TargetHost based on config
    ...
}
```

### request-size-validator
```go
// Before: tx.BodySize, tx.ContentType
// After:  input.BodySize, input.ContentType
```

### ip-access-filter
```go
// Before: tx.SourceIP
// After:  input.SourceIP
```

### webhook-forward (ai-gateway)
```go
// Before: tx.TransactionID, tx.Method, tx.Path, tx.TargetHost, tx.SourceIP, tx.NormalizedContent, etc.
// After:  input.Method, input.Path, input.TargetHost, input.SourceIP, input.Content, etc.
func (w *WebhookForward) Execute(ctx context.Context, input *HookInput) (*HookResult, error) {
    payload := map[string]any{
        "stage":      input.Stage,
        "method":     input.Method,
        "path":       input.Path,
        "targetHost": input.TargetHost,
        "sourceIp":   input.SourceIP,
        "content":    input.Content,
        "model":      input.Model,
    }
    // POST to webhook endpoint...
}
```

### quality-checker (ai-gateway)
```go
// Before: parses tx.RawBody with gjson for choices[0].message.content, finish_reason
// After:  reads input.Content, input.FinishReason directly
func (qc *QualityChecker) Execute(_ context.Context, input *HookInput) (*HookResult, error) {
    // Check response content length
    for _, block := range input.Content {
        if block.Role == "assistant" && len(block.Text) < qc.minResponseLength {
            // short response anomaly
        }
    }
    // Check finish reason
    if input.FinishReason == "length" { /* truncation anomaly */ }
    // Check refusal patterns
    for _, block := range input.Content {
        if qc.refusalRe.MatchString(block.Text) { /* refusal anomaly */ }
    }
}
```

---

## 10. Removed Types

- `InterceptedTransaction` — replaced by `HookInput`
- `TransactionPool` / `AcquireTransaction` / `ReleaseTransaction` — removed (HookInput is small, no pooling needed)

---

## 11. ContentBlock Shared Location

`ContentBlock` is currently defined in `packages/ai-gateway/internal/providers/adapter.go`. It needs to be in a shared location accessible by both providers and hooks.

Options:
- Move to `packages/shared/policy/hooks/types.go` (hooks package owns it)
- Create `packages/shared/types/content.go` (neutral shared package)

Recommendation: define in `packages/shared/policy/hooks/types.go` since hooks are the primary consumer. Provider Adapter imports `shared/hooks` for the type. This avoids a new package.

---

## 12. File Structure

### Modified files
| File | Change |
|------|--------|
| `packages/shared/policy/hooks/types.go` | Replace `InterceptedTransaction` with `HookInput`, add `ContentBlock`, remove pool |
| `packages/shared/policy/hooks/keyword_filter.go` | Rewrite Execute to use `*HookInput` |
| `packages/shared/policy/hooks/pii_detector.go` | Rewrite Execute to use `*HookInput` |
| `packages/shared/policy/hooks/content_safety.go` | Rewrite Execute to use `*HookInput` |
| `packages/shared/policy/hooks/rate_limiter.go` | Rewrite Execute to use `*HookInput` |
| `packages/shared/policy/hooks/request_size.go` | Rewrite Execute to use `*HookInput` |
| `packages/shared/policy/hooks/ip_access.go` | Rewrite Execute to use `*HookInput` |
| `packages/shared/compliance/pipeline.go` | `Execute(ctx, *HookInput)` instead of `Execute(ctx, *InterceptedTransaction)` |
| `packages/shared/compliance/policy.go` | `BuildPipeline(stage, ingressType, ...)` without tx param |
| `packages/ai-gateway/internal/pipeline/hooks/webhook.go` | Rewrite to use `*HookInput` |
| `packages/ai-gateway/internal/pipeline/hooks/quality.go` | Rewrite to use `*HookInput` |
| `packages/ai-gateway/internal/handler/proxy.go` | Build `HookInput` from Provider Adapter output |
| `packages/ai-gateway/internal/providers/adapter.go` | Remove `ContentBlock` (moved to shared) |
| `packages/ai-gateway/cmd/ai-gateway/main.go` | Use `HookConfigCache` |
| `packages/compliance-proxy/internal/proxy/forward_handler.go` | Build `HookInput` from Traffic Adapter |
| `packages/compliance-proxy/cmd/compliance-proxy/main.go` | Use `HookConfigCache` |
| `packages/agent/core/network/intercept/handler.go` | Build `HookInput` from Traffic Adapter |
| `packages/control-plane/internal/handler/admin_hooks.go` | Add `POST /hooks/refresh` |

### New files
| File | Responsibility |
|------|---------------|
| `packages/shared/compliance/config_cache.go` | `HookConfigCache` — shared config caching with Redis + TTL |

### Deleted types (from existing files)
| Type | Reason |
|------|--------|
| `InterceptedTransaction` | Replaced by `HookInput` |
| `TransactionPool` | No longer needed |
| `AcquireTransaction` / `ReleaseTransaction` | No longer needed |
