# AI Gateway Error Taxonomy

*Audience: contributors adding provider adapters; operators building alerting and retry policies.*

Every error the AI Gateway encounters — from a DNS failure to an upstream content filter to a Nexus quota exhaustion — maps to a single canonical `ProviderError` type with a typed `ErrorClass`. This unified taxonomy is the substrate for retry decisions, circuit breakers, routing fallback chains, and alerting aggregators. Without it, each provider adapter would develop its own ad-hoc handling and the gateway would behave inconsistently across providers.

---

## `ProviderError` and `ErrorClass`

```go
type ProviderError struct {
    Code        string      // canonical code: "rate_limit", "context_too_long", "auth_failed", ...
    Message     string      // human-readable, possibly sanitised
    Class       ErrorClass  // typed classification — see table below
    HTTPStatus  int         // upstream's status code (or synthesised)
    Retryable   bool        // whether retry/fallback machinery should try again
    Cause       error       // wrapped underlying error
}
```

The `ErrorClass` enum and its retry semantics:

| Class | Retry? | Fallback? | Circuit-breaker counts? | Alert? |
|---|---|---|---|---|
| `Network` | Yes (backoff) | Yes | Yes | On burst |
| `Timeout` | Yes (backoff, idempotent only) | Yes | Yes | On burst |
| `Rate429` | No — honour upstream's back-off signal | Yes | Yes | Yes (per-model aggregator) |
| `5xx` | Yes (backoff) | Yes | Yes | On burst |
| `4xx` | No — client bug | No | No | On suspicious burst |
| `ContentFiltered` | No | Maybe (per route policy) | No | On burst |
| `Auth` | No | Maybe (try another credential in pool) | Per-credential | Yes |
| `Quota` | No — we are out of budget | Maybe (per quota policy) | No | Yes |
| `Unknown` | No | No | Yes (suspicious) | Yes |

The `classify` function in `packages/ai-gateway/internal/execution/executor/classify.go` is the canonical HTTP-status → `ErrorClass` mapping. Provider adapters supply hints (HTTP status, provider-specific error body); `classify` resolves to a single `ErrorClass`. Everything downstream — executor, routing fallback, audit pipeline, alerting — consumes this one shape.

## Two distinct 429 responses

There are two 429s the gateway can return and they have different meanings:

| 429 origin | Source | Response shape |
|---|---|---|
| **Nexus-side 429** | Quota engine, rate limiter | `{ "error": { "code": "nexus_quota_exceeded", "type": "rate_limit", "limit": …, "remaining": …, "reset_at": … } }` |
| **Upstream 429** | Provider forwarded | Provider's own 429 envelope, annotated with `X-Nexus-Provider-429: <provider>` header |

Every 429 response carries `X-Nexus-Limit-Source: nexus | upstream` so the client can distinguish. For Nexus-side 429s, `Retry-After` is computed from the quota reset time. For upstream 429s, `Retry-After` is forwarded if the upstream provided it, or a default backoff is used.

The `Retry-After` contract: back off and retry the same provider on an upstream 429 (the upstream is asking for patience); do not change provider on a Nexus 429 (Nexus is enforcing budget, not signalling a provider problem).

## Retry policy

Per-class retry config is set per route with these defaults:

```yaml
retry:
  Network:    { maxAttempts: 3, backoffMS: [100, 500, 2000], jitter: 0.2 }
  Timeout:    { maxAttempts: 2, backoffMS: [500, 2000],      jitter: 0.2 }
  Rate429:    { maxAttempts: 0 }   # never retry; let the upstream's window reset
  5xx:        { maxAttempts: 2, backoffMS: [500, 2000],      jitter: 0.2 }
  Auth:       { maxAttempts: 0 }   # use credential pool fallback instead
```

Callers that want to handle retry themselves can set `X-Nexus-No-Retry: 1`. Retry attempts are recorded in `traffic_event.routing_trace.retry_attempts` and visible in the traffic audit drawer.

## Circuit breaker

A per-(provider, model) circuit breaker transitions through three states:

- **Closed** — normal operation.
- **Open** — too many failures in a recent window. New requests are rejected immediately and routed to the configured fallback chain. Default trip condition: 10 failures in 30s.
- **Half-open** — after the cool-down (default 30s), one probe request is allowed. Success closes the breaker; failure reopens it.

The breaker integrates with the credential pool (`credentials-architecture.md`). A bad credential opens the breaker for itself without affecting the (provider, model) pair globally.

## How adapters translate provider errors

Each provider adapter under `packages/ai-gateway/internal/providers/specs/<adapter>/errors/` implements `ErrorNormalizer`. This package maps the upstream's HTTP status code and response body to a `ProviderError`. The canonical `classify` function in `packages/ai-gateway/internal/execution/executor/classify.go` then resolves it to the `ErrorClass` that drives retry and fallback decisions.

Example mapping decisions (these live in per-adapter error packages, not in shared helpers):

- OpenAI 400 with `"code": "context_length_exceeded"` → `ErrorClass4xx`, not retryable.
- Anthropic 529 (overloaded) → `ErrorClass5xx`, retryable with backoff.
- Gemini 429 → `ErrorClassRate429`, not retried; eligible for fallback chain.
- Any provider returning a 401 → `ErrorClassAuth`; no retry; credential-pool fallback may apply.

The per-adapter placement is an architectural constraint from `provider-adapter-architecture.md` §3a Rule 3: per-provider wire quirks (including error shapes) belong in the adapter that talks to that wire. No shared helper carries cross-adapter case-statements.

### Fallback chain and `onClass`

Routing rules can specify an `onClass` fallback: when the current upstream returns an error of a given class, the executor walks to the next entry in the fallback chain. For example:

```yaml
fallback_chain:
  - provider: anthropic
    model: claude-sonnet-4-6
    onClass: [Rate429, 5xx, Network, Timeout]
  - provider: openai
    model: gpt-5
```

When Anthropic returns a 429, the executor moves to OpenAI and retries the original request (re-canonicalized for the new target). The fallback decision is recorded in `traffic_event.routing_trace.fallback_attempts`.

When the entire chain is exhausted, the gateway returns 503 with:

```json
{
  "error": {
    "code": "fallback_exhausted",
    "type": "upstream_unavailable"
  }
}
```

and sets `X-Nexus-Fallback-Attempted: <count>`.

## Streaming errors

Streaming errors require special handling because partial content may already have been forwarded:

- **Pre-first-byte failure** — same as non-streaming; classify and respond with the appropriate HTTP status.
- **Mid-stream failure** — the gateway emits an SSE `event: error` chunk with a sanitized message, closes the stream, and records the traffic event with `partial=true`. The partial cached result is not stored.
- **Mid-stream content filter** — upstream cuts off the stream; the gateway records `error_class=ContentFiltered` on the partial event.

## Audit fields

Every traffic event carries error classification for downstream analysis and alerting:

| Field | Content |
|---|---|
| `error_class` | The `ErrorClass` value (empty string for success) |
| `error_code` | The canonical `ProviderError.Code` |
| `http_status` | What the client received |
| `retry_attempts` | Number of retries before the final outcome |
| `fallback_attempts` | Entries from the fallback chain that were walked |

These fields drive the two key alerting aggregators:

- **`model.rate_limited_responses`** — counts `ErrorClassRate429` per (provider, model) per window. Distinguishes Nexus-side (`source=nexus`) from upstream-provider (`source=upstream`) 429s.
- **`provider.unavailable`** — counts circuit-breaker open events per provider.

---

## Canonical docs

- [`error-taxonomy-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/error-taxonomy-architecture.md) — full `ProviderError` / `ErrorClass` spec, retry policy tables, and streaming error handling
- [`quota-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/quota-architecture.md) — Nexus-side 429 source and quota enforcement
- [`provider-adapter-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md) — how adapters translate provider errors into `ProviderError`

**Adjacent wiki pages**: [AI Gateway Virtual Keys Quotas](AI-Gateway-Virtual-Keys-Quotas) · [AI Gateway Smart Routing](AI-Gateway-Smart-Routing) · [AI Gateway Routing Rules](AI-Gateway-Routing-Rules) · [AI Gateway Streaming](AI-Gateway-Streaming)
