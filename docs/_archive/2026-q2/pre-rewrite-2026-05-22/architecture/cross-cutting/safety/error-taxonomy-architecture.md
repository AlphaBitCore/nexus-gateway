---
doc: error-taxonomy-architecture
area: cross-cutting
service: safety
tier: 1
updated: 2026-05-20
---

# Error Taxonomy + Retry / Circuit Breaker Architecture

> **Tier 1 architecture doc.** Read this before touching `packages/ai-gateway/internal/providers/core/types.go` (`ProviderError`), `packages/ai-gateway/internal/execution/executor/classify.go`, the 429 response shape, retry logic, or any provider-specific error mapping. The routing engine consumes `ErrorClass` for fallback chain `onClass` semantics (`routing-architecture.md`); the alerting engine fires on rate-limit aggregators (`alerting-architecture.md`).

A unified error taxonomy is the substrate for retry, circuit breaker, alerting, and the 429 response shape. Without it, each provider adapter ends up with its own ad-hoc handling and the gateway behaves inconsistently across providers.

---

## 1. `ProviderError`

```go
// packages/ai-gateway/internal/providers/core/types.go
type ProviderError struct {
    Status       int
    Code         string         // canonical: see §1a
    Type         string         // provider's own type string, preserved for observability
    Message      string
    RetryAfter   *time.Duration
    Raw          []byte         // provider error payload verbatim
    Headers      http.Header    // upstream response headers, cloned; nil for synthetic errors
    TargetMethod string
    TargetPath   string
}
```

`ProviderError` is the **unified error type** that crosses the provider boundary. Adapters translate provider-specific errors into `ProviderError`; everything downstream (executor, routing fallback, audit, alerting) consumes this single shape. The classification into an `ErrorClass` (§2) is derived from `Status` + transport-level error at `classify` time, not stored on the struct.

### 1a. Canonical `Code` values

The adapter-facing canonical set (`packages/ai-gateway/internal/providers/core/types.go`):

```
invalid_request | auth_failed | rate_limited | timeout |
upstream_error | endpoint_unsupported | not_implemented | no_compatible_provider
```

Adapters MUST use these exact strings; new codes require a single-line addition to the const block.

## 2. `ErrorClass`

```go
// packages/shared/schemas/configtypes/policy/retry_policy.go
type ErrorClass string

const (
    ErrorClassNetwork ErrorClass = "network"  // connection refused, DNS failure, transport-level error
    ErrorClassTimeout ErrorClass = "timeout"  // upstream took too long
    ErrorClassRate429 ErrorClass = "429"      // upstream said "rate limited"
    ErrorClass5xx     ErrorClass = "5xx"      // upstream returned a 5xx
)
```

Default retry behaviour (`DefaultRetryPolicy`): retry on `network | timeout | 429 | 5xx`; `MaxAttemptsPerTarget=1`; exponential backoff seeded by `BackoffInitial=250ms`, capped at `BackoffMax=5s`, `BackoffJitter=0.2`. The `MaxAttemptsPerTarget` value is clamped into `[1,5]` (`ClampMaxAttempts`).

| Class | Retry on default policy? | Fallback chain? |
|---|---|---|
| network | Yes (in `RetryOn`) | Yes (fallback chain `onClass`) |
| timeout | Yes | Yes |
| 429 | Yes (with `RetryAfter` honoured by `backoff.go`) | Yes |
| 5xx | Yes | Yes |

4xx / auth / quota / content-filtered errors are surfaced via `ProviderError.Code` (`invalid_request`, `auth_failed`, etc., §1a), not via a class enum — they aren't classified for retry because retrying a 4xx/auth/quota error doesn't change the outcome.

The executor's `classify` function in `packages/ai-gateway/internal/execution/executor/classify.go` is the canonical mapping. Inputs are the adapter `Response` HTTP status and any transport-level error; output is a `(internalClass, cfgpolicy.ErrorClass)` tuple — the second value is what the retry policy's `RetryOn` membership check consumes.

## 3. The 429 response shape

Two 429 sources cross the gateway:

| 429 origin | Source | Response shape |
|---|---|---|
| **Nexus-side 429** | Quota engine, rate limiter | An OpenAI-shape error envelope produced by `packages/ai-gateway/internal/errs` / quota handlers |
| **Upstream 429** | Provider passed through | The provider's own 429 body (and headers) forwarded, with `ProviderError.Code = "rate_limited"` recorded internally and `RetryAfter` populated when the upstream sent it |

The gateway forwards an upstream `Retry-After` header when present; the executor's `backoff.go` reads `ProviderError.RetryAfter` for the wait between attempts. No dedicated `X-Nexus-Limit-Source` discriminator header is set today — adopters who need wire-level disambiguation can read the upstream's own error body when present.

## 4. Retry policy

`RetryPolicy` is a single shape (no per-class structs) — `MaxAttemptsPerTarget` + `RetryOn` (list of `ErrorClass`) + exponential backoff seed:

```go
// packages/shared/schemas/configtypes/policy/retry_policy.go
type RetryPolicy struct {
    MaxAttemptsPerTarget int
    RetryOn              []ErrorClass
    BackoffInitial       time.Duration
    BackoffMax           time.Duration
    BackoffJitter        float64
}
```

`DefaultRetryPolicy` returns `MaxAttemptsPerTarget=1`, `RetryOn=[network,timeout,429,5xx]`, `BackoffInitial=250ms`, `BackoffMax=5s`, `BackoffJitter=0.2`. Per-rule overrides field-merge with this default via `MergedWith` (an empty `RetryOn` slice is meaningfully "retry nothing" — distinct from nil "use default").

## 5. Circuit breaker

Circuit-breaker state lives **per-credential**, not per (provider, model). State machine:

- **Closed** — normal. Requests flow.
- **Open** — too many auth failures (or a recent rate_limit) opened the circuit; the credential is excluded from the pool.
- **Half-open** — after the configured cool-down, the credential transitions on the next selection read; a probe is allowed.

Thresholds live in `packages/shared/schemas/credstate/credstate.go` (`Thresholds` + `DefaultThresholds`):

- `AuthFailThreshold` — consecutive 401/403 responses that open the circuit with `reason=auth_fail`. Default `3`.
- `RateLimitCooldownSeconds` — how long a `rate_limit` OPEN circuit stays open before auto-transitioning on the next selection read. Default `60`.
- Health classification thresholds (`HealthyThresholdPct=95`, `DegradedThresholdPct=50`, `HealthMinSamples=5`, `HealthWindowSeconds=300`, `HealthSustainedDegradedSeconds=900`) drive the multi-window health status that feeds the pool's exclusion logic.

The breaker is integrated with the credential pool (cross-ref `credentials-architecture.md`). A bad credential opens the breaker for itself but not for the (provider, model) globally — when every credential in the pool is open, the absence of any selectable credential surfaces as the higher-level "no compatible provider" error.

## 6. Streaming errors

Streaming errors are special-cased:

- **Pre-first-byte failure** — same as non-streaming; classify and respond.
- **Mid-stream failure** — partial response already sent. The executor closes the stream and records the outcome on the traffic_event so audit + cost still reflect what was delivered.
- **Mid-stream content-filter** — upstream cuts off; the gateway records the outcome on the partial traffic_event row. The cached partial result is not stored.

The `chunked_async` streaming mode (cross-ref `hook-architecture.md`) is the only place hooks can produce a "block bytes after some bytes already sent" outcome; same close-the-stream behaviour applies.

## 7. Audit fields

Every traffic_event row captures the outcome via the `status_code`, `error_code` / `error_message`, and the `routing_trace` JSONB blob (which carries per-attempt records including retries and fallback walks). The exact column set is in `tools/db-migrate/schema.prisma`'s `model traffic_event`.

These fields drive the alerting aggregators (cross-ref `alerting-architecture.md`) and the cost-attribution UI (a 4xx still counts the request; a 5xx after retries counts each attempt's tokens).

## 8. Alerting integration

The aggregators under `packages/nexus-hub/internal/alerts/eval/aggregators/` consume traffic-event outcomes for the rate-limit, provider-availability, and credential-health rule families. The exact rule set + aggregator filenames are catalogued in `alerting-architecture.md` — that doc is the single source of truth for which aggregators are wired and which `AlertRule.id`s they implement.

## 9. Failure modes for the taxonomy itself

| Failure | Behaviour |
|---|---|
| Transport-level error with no HTTP status (DNS, connection refused, etc.) | `classify` returns `ErrorClassNetwork` and the executor retries per `RetryPolicy.RetryOn` membership. |
| Upstream HTTP status the adapter doesn't specifically recognise | `classify` returns the band (`5xx`) or — for non-retryable bands — surfaces the underlying `ProviderError.Code` (e.g. `auth_failed`, `invalid_request`) without retry. |
| Mid-stream malformed bytes | Recorded as a transport error; the executor cannot retry mid-stream (see §6). |
| Retry exhausted | Returns last attempt's response unchanged (status, body) — we do NOT synthesise a "retry exhausted" envelope. The client sees what the provider would have shown. |
| Fallback exhausted | The executor surfaces the last `ProviderError`; the canonical `no_compatible_provider` code (§1a) is used when the resolver could not find a usable provider at all. |

## 10. Sources

- `packages/ai-gateway/internal/providers/core/types.go` — `ProviderError` struct + canonical `Code` constants.
- `packages/shared/schemas/configtypes/policy/retry_policy.go` — `ErrorClass` enum + `RetryPolicy` + `DefaultRetryPolicy` + `MergedWith`.
- `packages/ai-gateway/internal/execution/executor/classify.go` — `classify` (HTTP-status → `ErrorClass` mapping).
- `packages/ai-gateway/internal/execution/executor/backoff.go` — retry attempt scheduling + jitter (honours `ProviderError.RetryAfter`).
- `packages/ai-gateway/internal/execution/executor/executor.go` + `api.go` — dispatch loop that consults `classify` + `backoff` to decide retry-vs-give-up. Circuit-breaker state lives on the credential row + Redis hash; see `credentials-architecture.md`.
- `packages/shared/schemas/credstate/credstate.go` — `Thresholds` + `DefaultThresholds` (circuit + health classification).

## 11. Cross-references

- `routing-architecture.md` — fallback chain `onClass` semantics; ResolvedRequest carries retry context.
- `credentials-architecture.md` — per-credential circuit breaker + pool health.
- `quota-architecture.md` — Nexus-side 429 source.
- `alerting-architecture.md` — rate-limit + provider-unavailable aggregators.
- `provider-adapter-architecture.md` — how adapters translate provider errors into `ProviderError`.
- `audit-pipeline-architecture.md` — audit rows store error_class / error_code.
