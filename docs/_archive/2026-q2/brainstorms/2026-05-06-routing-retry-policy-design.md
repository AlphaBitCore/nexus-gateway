# Routing Retry Policy — Design

**Date:** 2026-05-06
**Status:** Draft (awaiting user spec review)
**Owner:** Platform / ai-gateway
**Scope:** `tools/db-migrate` (Prisma schema), `shared/configtypes`, `ai-gateway/internal/{executor,store,handler}`, `ai-gateway/cmd/ai-gateway/*.dev.yaml`, `control-plane/internal/handler/admin_routing.go`, `control-plane-ui/src/pages/router/`, OpenAPI for routing rule CRUD.

## 1. Problem

Today the ai-gateway `TargetExecutor` has hardcoded retry/failover behaviour:

- 2xx → return immediately
- 4xx (non-429) → return immediately (surface error to caller)
- 429 → if `Retry-After ≤ 5s`, retry **same target once**; otherwise treat as failure and move to next target
- 5xx / timeout / network → move to next target

There is no way for an admin to say "for this routing rule, retry transient failures up to 3× per target before failing over" or "for this rule, never retry 429 (the upstream is rate-limited; failover is the right answer)". Worse, the log line's `attempt` field is stuck at `1` because no caller populates `httpclient.WithAttempt`, so the whole observability win of having that field is wasted.

This spec introduces an explicit `RetryPolicy` on every routing rule (with a sensible global default in YAML), an error-classification step the executor uses to decide retry-vs-failover, exponential backoff with jitter between L2 retries, and wires the global attempt counter into the outbound HTTP debug log line.

## 2. Non-goals

- Per-Provider or per-Org retry policy. Routing rule is the only override surface; rationale in §3.
- Idempotency keys / dedup on retry. (LLM POSTs are non-deterministic but not idempotent in the http-method sense; we accept that retry may bill twice. Spec §5.)
- L1 transport-level retry (TCP / TLS handshake). `net/http` already handles connection-level reconnect; adding another layer would corrupt the per-request timeout budget.
- Configurable backoff in the UI. Backoff knobs (initial / max / jitter) live in YAML only — operators rarely tune them and exposing 3 more fields per rule is UI bloat.
- Routing-strategy changes. The set of strategies (single / fallback / loadbalance / conditional / ab_split / smart / policy) is unchanged. RetryPolicy is orthogonal — it controls per-target behavior; strategy controls target selection.

## 3. Locked design decisions

| # | Decision | Reason |
|---|---|---|
| Q1 | RetryPolicy lives on `RoutingRule`; global default in `ai-gateway.yaml`; rule overrides default; **no per-Provider, no per-Org cascade**. | Mirrors existing `ratelimit` / `quota` / `httpClients.External.timeoutSec` patterns. Two layers is the floor; three is bloat. |
| Q2 | UI exposes `MaxAttemptsPerTarget` + `RetryOn` only. Backoff knobs in YAML. | Backoff tuning is SRE work, not admin work. |
| Q3 | LLM POSTs are retried by default on retryable error classes. | Industry standard (OpenAI SDK / LangChain / Bedrock SDK all do). Cost = duplicated tokens; benefit = client doesn't see transient failures. |
| Q4 | `attempt` field in the outbound HTTP log = global counter for one `nexus_request_id`, covering L2 retries + L3 failovers. Response header renames `x-nexus-aigw-retry-count` → `x-nexus-attempts`. `Attempt` struct gains `RetryReason`. | One grep gives the full call chain. Header rename is honest (it was always counting attempts, not retries). |

Sub-decisions inside the locked options:

- **Field-level merge** between YAML default and per-rule override. If a rule sets `maxAttemptsPerTarget` but omits `retryOn`, `retryOn` falls back to the YAML default. A rule that fully omits the `retryPolicy` field uses the YAML default in full. (No deep-merge of arrays; `retryOn: [5xx]` on a rule means "5xx only", not "default-set ∪ {5xx}".)
- **Failover gate** (3-way classifier):
  1. Errors classified as `classNoFailoverNoRetry` (4xx, auth failed, endpoint unsupported, etc.) → return immediately. No L2, no L3. Same as today's hardcoded behavior.
  2. Retryable error class (network/timeout/429/5xx) AND class IS in `RetryOn` → L2 retry up to `MaxAttemptsPerTarget`; if still failing after the budget is exhausted, L3 failover to the next target.
  3. Retryable error class but class is NOT in `RetryOn` (e.g. admin removed `429` from `retryOn` because they trust failover more than waiting on the same rate-limited provider) → no L2 retry, immediate L3 failover.
- **Backoff is bounded by the request context deadline.** If the next backoff would push past the request deadline, the executor returns the last error rather than sleeping past it.
- **Health tracking is per-call, not per-target.** Each L2 retry that hits the same target records its own success/failure on `HealthTracker`. (No semantic change vs. today; just clarity.)

## 4. Data model

### 4.1 Prisma schema (`tools/db-migrate/schema.prisma`)

Add a nullable `retryPolicy` JSON column to `RoutingRule`:

```prisma
model RoutingRule {
  id              String   @id @default(uuid())
  name            String   @unique
  description     String?
  strategyType    String
  config          Json
  matchConditions Json?
  priority        Int      @default(0)
  pipelineStage   Int      @default(1)
  fallbackChain   Json?
  retryPolicy     Json?    // RetryPolicy override; null = use ai-gateway YAML default
  enabled         Boolean  @default(true)
  createdAt       DateTime @default(now()) @db.Timestamptz(3)
  updatedAt       DateTime @updatedAt @db.Timestamptz(3)
  // …indices unchanged…
}
```

Migration name: `add_routing_rule_retry_policy`. Backfills nothing; existing rules get `null` and pick up the YAML default at runtime.

### 4.2 Go type (`shared/configtypes/retry_policy.go`)

```go
package configtypes

import "time"

// RetryPolicy controls per-target retry behavior in the ai-gateway
// executor. Rule-level overrides field-merge with the YAML default
// (any field set on the rule wins; absent fields fall back to default).
type RetryPolicy struct {
    // MaxAttemptsPerTarget is the L2 retry budget for one routing target.
    // 1 means "try once, no retry" (the historical default).
    // Range: 1..5. Values outside the range are clamped on read.
    MaxAttemptsPerTarget int `json:"maxAttemptsPerTarget"`

    // RetryOn lists the error classes that trigger an L2 retry on the
    // same target. Anything not in this list short-circuits to the L3
    // failover decision (or returns immediately for non-failover errors
    // like 4xx).
    RetryOn []ErrorClass `json:"retryOn"`

    // BackoffInitial is the first inter-attempt sleep. Subsequent
    // attempts double up to BackoffMax. YAML-only.
    BackoffInitial time.Duration `json:"backoffInitial"`

    // BackoffMax caps the inter-attempt sleep. YAML-only.
    BackoffMax time.Duration `json:"backoffMax"`

    // BackoffJitter is the multiplicative jitter applied to each backoff
    // value. 0.2 means ±20%. YAML-only. Range: 0..0.5.
    BackoffJitter float64 `json:"backoffJitter"`
}

// ErrorClass identifies why an attempt failed, for retry classification.
type ErrorClass string

const (
    ErrorClassNetwork ErrorClass = "network" // dial / TLS / read-header
    ErrorClassTimeout ErrorClass = "timeout" // request context deadline / read-body
    ErrorClassRate429 ErrorClass = "429"     // provider responded HTTP 429
    ErrorClass5xx     ErrorClass = "5xx"     // provider responded 5xx
)

// DefaultRetryPolicy returns the platform default. YAML config builds on
// top of this; per-rule overrides build on top of YAML.
func DefaultRetryPolicy() RetryPolicy {
    return RetryPolicy{
        MaxAttemptsPerTarget: 1,
        RetryOn:              []ErrorClass{ErrorClassNetwork, ErrorClassTimeout, ErrorClassRate429, ErrorClass5xx},
        BackoffInitial:       250 * time.Millisecond,
        BackoffMax:           5 * time.Second,
        BackoffJitter:        0.2,
    }
}

// MergedWith returns this policy with any zero-valued fields replaced
// from override. Use as: yamlDefault.MergedWith(rulePolicy).
// Fields treated as "absent": MaxAttemptsPerTarget == 0; RetryOn == nil
// (length 0 is a meaningful "retry nothing" — distinguish nil vs []).
func (p RetryPolicy) MergedWith(o *RetryPolicy) RetryPolicy { /* impl */ }
```

JSON shape stored in the DB:

```json
{
  "maxAttemptsPerTarget": 3,
  "retryOn": ["timeout", "5xx"]
}
```

Backoff fields are **omitted** from rule-level JSON (UI doesn't expose them, so DB never stores them). The Go marshaller honors `omitempty` so absent fields stay absent on the wire.

### 4.3 ai-gateway YAML (`ai-gateway.dev.yaml` + `ai-gateway.yaml`)

```yaml
routing:
  defaultRetryPolicy:
    maxAttemptsPerTarget: 1
    retryOn: ["network", "timeout", "429", "5xx"]
    backoffInitial: 250ms
    backoffMax: 5s
    backoffJitter: 0.2
```

Loaded at startup into a singleton `RetryPolicy` value. If the YAML omits `routing.defaultRetryPolicy` entirely, `DefaultRetryPolicy()` is used. If individual sub-fields are omitted, the corresponding `DefaultRetryPolicy()` value fills in.

## 5. Executor rewrite

### 5.1 Algorithm pseudocode

```
Execute(ctx, targets, base, rulePolicy):
    yamlDefault = ai-gateway global YAML default
    policy = yamlDefault.MergedWith(rulePolicy)
    attemptCounter = 0
    var attempts []Attempt

    for each target in targets:
        callTarget, err = resolver.Resolve(target)
        if err: attempts += {Target, Error: "resolve:..."}; continue (next target)
        adapter, ok = registry.Get(callTarget.Format)
        if !ok: attempts += {Target, Error: "no adapter ..."}; continue (next target)
        req = build req from base + callTarget (+ optional bridge translation)

        for tryIdx := 1 to policy.MaxAttemptsPerTarget:
            attemptCounter += 1
            attemptCtx = httpclient.WithAttempt(ctx, attemptCounter)
            outcome = attempt(attemptCtx, adapter, req, target)
            attempts += outcome.Attempt   // includes RetryReason if applicable

            if outcome.Class == ClassSuccess:
                return ExecutionResult{success}
            if outcome.Class == ClassNoFailoverNoRetry:  // 4xx etc.
                return ExecutionResult{their error}      // surface immediately
            if outcome.Class not in policy.RetryOn:
                break (out of L2 loop) // failover decision below
            if tryIdx == policy.MaxAttemptsPerTarget:
                break (out of L2 loop)
            // Schedule L2 retry
            backoff = computeBackoff(tryIdx, policy)
            if ctx.Deadline - now < backoff:
                break (out of L2 loop)  // not enough budget; failover instead
            sleep backoff (or until ctx done)

        // L2 exhausted — fall through to L3 failover (next target)

    return ExecutionResult{Error: ErrAllTargetsExhausted, Attempts: attempts}
```

### 5.2 Error classification (`internal/executor/classify.go`)

```go
type errClass int

const (
    classSuccess errClass = iota
    classNoFailoverNoRetry          // 4xx, auth failed, endpoint unsupported, etc.
    classNetwork                    // pure transport error (no ProviderError)
    classTimeout                    // CodeTimeout
    classRate429                    // CodeRateLimited
    class5xx                        // CodeUpstreamError
)

// classify maps a (resp, err) pair from adapter.Execute into an errClass
// + a configtypes.ErrorClass for the RetryOn membership check.
func classify(resp *providers.Response, err error) (errClass, configtypes.ErrorClass) { /* impl */ }
```

Mapping table:

| Result of `adapter.Execute` | errClass | ErrorClass for `RetryOn` |
|---|---|---|
| `(resp, nil)` with 2xx | `classSuccess` | n/a |
| `*ProviderError{Code: CodeRateLimited}` | `classRate429` | `"429"` |
| `*ProviderError{Code: CodeTimeout}` | `classTimeout` | `"timeout"` |
| `*ProviderError{Code: CodeUpstreamError}` | `class5xx` | `"5xx"` |
| `*ProviderError{Code: CodeInvalidRequest \| CodeAuthFailed \| CodeEndpointUnsupported \| CodeNotImplemented \| CodeNoCompatibleProvider}` | `classNoFailoverNoRetry` | n/a |
| Any other `error` (no `*ProviderError` wrap) | `classNetwork` | `"network"` |

### 5.3 Backoff (`internal/executor/backoff.go`)

```go
// computeBackoff returns the sleep duration before the (tryIdx+1)-th
// attempt. tryIdx is 1-based: tryIdx=1 means "between attempt 1 and 2".
func computeBackoff(tryIdx int, p configtypes.RetryPolicy) time.Duration {
    base := p.BackoffInitial * (1 << (tryIdx - 1))
    if base > p.BackoffMax {
        base = p.BackoffMax
    }
    if p.BackoffJitter > 0 {
        delta := float64(base) * p.BackoffJitter
        // ±delta uniform jitter using crypto-free rand source seeded once.
        base += time.Duration(rand.Float64()*2*delta - delta)
    }
    if base < 0 {
        base = 0
    }
    return base
}
```

Jitter uses a package-local `*rand.Rand` (NOT the global, to avoid lock contention) seeded from `time.Now().UnixNano()` at process start.

### 5.4 Attempt struct extension (`internal/executor/executor.go`)

```go
type Attempt struct {
    Target         router.RoutingTarget
    CredentialID   string
    CredentialName string
    StatusCode     int
    Error          string
    LatencyMs      int
    RetryReason    string  // NEW: "" on success / no-retry; otherwise "network"|"timeout"|"429"|"5xx"
}
```

`RetryReason` is set on every `Attempt` whose outcome was a retryable failure (regardless of whether retry actually happened — it captures *why* this attempt failed, so the audit row can answer "was this a transient blip?").

The audit row's `attempts` JSON column gets the new field automatically (it serializes the slice).

## 6. Public surface changes

### 6.1 Response header rename

`x-nexus-aigw-retry-count` → `x-nexus-attempts`. Value semantics also shift slightly:

- Old: `len(attempts) - 1` (number of failovers; 0 means "first target succeeded first try")
- New: `len(attempts)` (total attempts including the successful one; 1 means "first target, first try, success")

Per project policy (no backcompat in pre-GA), the old header is **deleted**, not aliased. Affected call sites: `packages/ai-gateway/internal/handler/proxy.go:1170-1171` and `:1193-1194`.

### 6.2 Admin Routing CRUD API

`POST /api/admin/routing-rules` and `PATCH /api/admin/routing-rules/:id` accept a new optional field:

```json
{
  "name": "production-critical",
  "strategyType": "fallback",
  "config": { ... },
  "retryPolicy": {
    "maxAttemptsPerTarget": 3,
    "retryOn": ["timeout", "5xx"]
  }
}
```

`GET /api/admin/routing-rules` and `GET /api/admin/routing-rules/:id` include `retryPolicy` in the response when set; absent when null.

OpenAPI schema (`docs/openapi/e<epic>-s<story>-routing-retry-policy.yaml`) defines `RetryPolicy` as:

```yaml
RetryPolicy:
  type: object
  properties:
    maxAttemptsPerTarget:
      type: integer
      minimum: 1
      maximum: 5
    retryOn:
      type: array
      items:
        type: string
        enum: [network, timeout, "429", "5xx"]
```

### 6.3 Control Plane UI

`packages/control-plane-ui/src/pages/router/RoutingRuleEditPage.tsx` (or whichever the actual edit page is — locate first):

- Add a "Retry Policy" section to the routing-rule edit form, AFTER the strategy / targets section.
- Two controls only:
  - `MaxAttemptsPerTarget` — number input, min 1, max 5, default placeholder "Use platform default (1)"
  - `RetryOn` — checkbox group: `network`, `timeout`, `429`, `5xx`. Default placeholder "Use platform default (all 4)"
- "Use platform default" is a dedicated radio above both controls. When selected, the form sends `retryPolicy: null`. When the user picks "Custom", the controls become editable.
- Form summary in the rule list page: badge showing `Custom retry: 3× / 5xx,timeout` for rules with non-null policy; nothing for null.
- i18n keys: add to `pages.json` (en/zh/es). Sections: `routing.retryPolicy.title`, `.platformDefault`, `.custom`, `.maxAttempts`, `.retryOn`, `.retryOn.network`, `.retryOn.timeout`, `.retryOn.429`, `.retryOn.5xx`. Strings in `network`/`timeout`/`429`/`5xx` themselves stay technical (no translation).
- TypeScript type in `packages/control-plane-ui/src/api/types/routing.ts`:

```typescript
export type ErrorClass = 'network' | 'timeout' | '429' | '5xx';
export interface RetryPolicy {
  maxAttemptsPerTarget?: number;
  retryOn?: ErrorClass[];
}
```

## 7. Operational & log changes

### 7.1 Outbound HTTP log line

The wrapper already reads `httpclient.AttemptFromContext(ctx)`. Once the executor calls `httpclient.WithAttempt(ctx, n)` per attempt, the log shows the global counter:

```
nexus_request_id=X attempt=1 caller=provider-upstream host=openai.com  status=503 ...
nexus_request_id=X attempt=2 caller=provider-upstream host=openai.com  status=503 ...   (L2 retry, same target)
nexus_request_id=X attempt=3 caller=provider-upstream host=anthropic.com status=200 ... (L3 failover, success)
```

### 7.2 ai-gateway access log

`internal/middleware.Logger` already includes `requestId`. No change needed; the chain is joinable as before via `requestId == nexus_request_id`.

### 7.3 Metrics

Add one new Prometheus counter to `ai-gateway/internal/metrics`:

```go
RouterRetryTotal = promauto.NewCounterVec(
    prometheus.CounterOpts{
        Namespace: ns,
        Subsystem: "router",
        Name:      "retry_total",
        Help:      "Number of L2 retries attempted per target, labelled by class and outcome.",
    },
    []string{"provider", "class", "outcome"}, // outcome ∈ {"retried_succeeded","retried_failed","retried_failover","exhausted"}
)
```

Existing target-attempt metrics stay unchanged.

## 8. Testing plan

### 8.1 `shared/configtypes/retry_policy_test.go`

- `TestDefaultRetryPolicy_Values` — defaults match spec
- `TestMergedWith_FullOverride` — rule replaces every default field
- `TestMergedWith_PartialOverride_FieldMerge` — rule sets one field, others fall back
- `TestMergedWith_NilOverride_UsesDefault` — `nil` rule policy returns default unchanged
- `TestMergedWith_EmptyRetryOnIsRespected` — `retryOn: []` (length 0, not nil) means "retry nothing" and must NOT be replaced by default
- `TestRetryPolicy_JSONRoundTrip` — marshal/unmarshal preserves shape
- `TestRetryPolicy_MaxAttemptsClamping` — values <1 clamp to 1, values >5 clamp to 5

### 8.2 `ai-gateway/internal/executor/classify_test.go`

Table-driven over every (`*ProviderError.Code`, network err, success) input → expected `errClass` + `ErrorClass`.

### 8.3 `ai-gateway/internal/executor/backoff_test.go`

- `TestComputeBackoff_Doubles_UntilMax` — 250ms, 500ms, 1s, 2s, 4s, 5s, 5s, 5s
- `TestComputeBackoff_JitterRange` — over 1000 iterations, the produced value stays within `[base*(1-jitter), base*(1+jitter)]`
- `TestComputeBackoff_NoNegative` — extreme jitter floors to 0

### 8.4 `ai-gateway/internal/executor/executor_test.go` extensions

- `TestExecute_L2_Retries_429_UntilSuccess`
- `TestExecute_L2_Retries_Timeout_UntilExhaustion_ThenFailover`
- `TestExecute_L2_NoRetry_On4xx_ReturnsImmediately` — 4xx bypasses both L2 and L3
- `TestExecute_L3_FailoverWhen_RetryOnDoesNotInclude` — class not in `RetryOn` → no L2 retry, immediate L3 failover
- `TestExecute_AttemptCounter_GlobalAcrossL2andL3` — each attempt's `httpclient.AttemptFromContext` returns 1, 2, 3, ... regardless of target switch
- `TestExecute_BackoffSkipped_WhenContextDeadlineImmediate` — context with `<backoff` remaining → no sleep, returns last error
- `TestExecute_RetryReasonRecorded_OnEachAttempt`

### 8.5 `ai-gateway/internal/handler/proxy_test.go` extensions

- `TestSetResponseHeaders_AttemptsHeader` — header is `x-nexus-attempts: <count>`, value reflects total attempts (NOT `count-1`).
- Existing tests for `x-nexus-aigw-retry-count` are deleted (the header itself is gone).

### 8.6 `control-plane/internal/handler/admin_routing_test.go` extensions

- `TestCreate_AcceptsRetryPolicy` — POST with `retryPolicy` field round-trips through DB
- `TestCreate_AcceptsNullRetryPolicy` — POST without `retryPolicy` stores `null`
- `TestPatch_UpdatesRetryPolicy` — PATCH overwrites
- `TestPatch_ClearsRetryPolicy` — PATCH with `retryPolicy: null` returns to YAML default
- Validation: `maxAttemptsPerTarget` outside [1,5] returns 400; `retryOn` with unknown enum value returns 400

### 8.7 UI tests (`control-plane-ui` Vitest)

- `RoutingRuleEditPage` form submission with custom retry policy
- "Use platform default" radio toggles between null and the controls
- i18n key coverage for all 3 locales

## 9. Migration & rollout

Per project pre-GA policy (no backcompat shims, no phased rollouts):

1. **Prisma migration** runs in dev: `cd tools/db-migrate && npx prisma migrate dev --name add_routing_rule_retry_policy`. Fresh column, default `null`. No data backfill needed.
2. **Header rename** (`retry-count` → `attempts`) is a hard cut. No alias retained. Any external consumer reading the old header (none in this repo; verify by grep) must update.
3. **Executor rewrite** replaces the existing hardcoded retry block in one commit. The new behavior with default policy `MaxAttemptsPerTarget=1` is **functionally equivalent** to today's behavior for all error classes EXCEPT the 429-with-RetryAfter-≤5s special case (which today retries once even with the policy default of 1). The new behavior: with the default, 429 does NOT retry on the same target — it goes to L3 failover. **This is intentional** and aligns the implementation with what the policy says. If any installation relied on the old 429-retry behavior, they must set `maxAttemptsPerTarget: 2` (and add `429` to `retryOn`, which is the default anyway).
4. **No feature flag.** RetryPolicy is on once the migration applies.

## 10. Implementation order (informs writing-plans)

1. Prisma migration + Go type (`shared/configtypes/retry_policy.go`) + tests
2. ai-gateway YAML default loading
3. Error classification helper + tests
4. Backoff helper + tests
5. Executor rewrite + tests
6. Attempt struct `RetryReason` extension (audit row)
7. Response header rename + tests
8. CP admin Routing CRUD: accept/return `retryPolicy` field; validation
9. OpenAPI spec
10. CP UI: RoutingRuleEdit form + types + i18n + tests
11. Workspace test sweep + smoke verify with VK

## 11. Open questions

None. All design questions Q1-Q4 + sub-decisions in §3 are locked.
