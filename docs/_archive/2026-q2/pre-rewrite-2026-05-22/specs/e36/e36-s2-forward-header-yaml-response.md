# E36-S2 — Forward-Header YAML Allowlist (Response Side)

## 0. References

- Requirements: `docs/developers/specs/e36/e36-forward-header-allowlist.md`
- Hand-off context brief: `docs/_archive/2026-q2/brainstorms/2026-05-06-forward-header-allowlist-context.md`
- Architecture diff: `docs/users/product/architecture.md` §"AI Gateway Provider Adapters" — "Forward-header allowlist (request + response)"
- Companion story (request side, prerequisite): `docs/developers/specs/e36/e36-s1-forward-header-yaml-request.md`
- Coupling target (deferred amendment): `docs/developers/specs/e35/e35-s1-sse-cache.md`

## 1. User story

> *"As a customer SDK consumer of the gateway, I want my SDK to
> receive the upstream provider's `x-request-id`, token-rate
> headers, and processing-time headers — without API changes —
> so support tickets can correlate to the real upstream call and
> client-side throttling has the data it needs.*
> *As a gateway operator, I want this opt-in via YAML, with sane
> defaults that preserve today's zero-passthrough posture until I
> explicitly enable specific headers."*

## 2. Scope

### 2.1 In scope

- Extend `forwardHeaders` block in `ai-gateway.dev.yaml` with a
  `response` sub-block parallel to `request`. Per-adapter-type
  entries split into `static` (cacheable) and `perRequest`
  (cache-hit-stripped).
- The `internal/config/forward_header.go` loader (built in S1) is
  extended to populate `ResolvedAllowlist.response`.
- Today's `Response.Headers` field — populated by adapters at
  `packages/ai-gateway/internal/providers/spec_adapter.go:155` and
  `:183` but never read by any handler — is wired into the
  response written to the client. This closes the dead-field gap
  flagged in the brainstorm.
- Default response-side YAML is **empty** for every adapter type:
  zero passthrough remains the out-of-the-box posture (NFR
  preserves today's compliance-strong default).
- Cache hit path strips `perRequest` response headers before
  writing to the client; `static` headers replay from cache as-is.
  See §6 for the deferred E35 amendment that owns the actual cache
  code change.
- A Prometheus counter
  `ai_gateway_forward_header_dropped_total{header,direction="response",adapter_type}`
  parallel to S1.
- Unit-test coverage for: empty default zero-passthrough, static
  vs perRequest split semantics, cache-hit perRequest strip
  contract (test against a fake cache; real cache integration
  owned by E35), Nexus `x-nexus-aigw-*` headers winning on
  conflict, hard denylist applied to response YAML.

### 2.2 Out of scope (covered elsewhere)

- Request-side allowlist — `docs/developers/specs/e36/e36-s1-forward-header-yaml-request.md`.
- Edits to `docs/developers/specs/e35/e35-s1-sse-cache.md` or the cache code path —
  deferred per §6.
- BYOK vs platform-key conditional response forwarding —
  Requirements §"Out of Scope".
- Per-Provider override layer.

## 3. YAML schema (extending S1)

```yaml
forwardHeaders:
  request:
    # … as defined in e36-s1 …
  response:
    base:
      static: []
      perRequest: []
    perAdapterType:
      openai:
        static:
          - openai-version
        perRequest:
          - x-request-id
          - openai-processing-ms
          - x-ratelimit-limit-tokens
          - x-ratelimit-remaining-tokens
          - x-ratelimit-reset-tokens
          - x-ratelimit-limit-requests
          - x-ratelimit-remaining-requests
          - x-ratelimit-reset-requests
      anthropic:
        static:
          - anthropic-version
        perRequest:
          - request-id
          - anthropic-ratelimit-input-tokens-remaining
          - anthropic-ratelimit-input-tokens-reset
          - anthropic-ratelimit-output-tokens-remaining
          - anthropic-ratelimit-output-tokens-reset
          - anthropic-ratelimit-requests-remaining
          - anthropic-ratelimit-requests-reset
      # All other adapter types: empty (default zero-passthrough preserved).
```

The above is the **shipped default** (embedded YAML; see S1
T-CONFIG-DEFAULTS for the loading model). Operators are expected
to start from this baseline and add to it. Choosing OpenAI and
Anthropic for the default opt-in matches the two largest
provider footprints on Nexus and the two largest
customer-support pain surfaces (request-id correlation,
token-rate visibility).

If an operator wants the strictest posture (zero passthrough on
every adapter), they set `response.perAdapterType: {}` (empty
map) in their config — whole-block replace per S1 T-CONFIG-DEFAULTS.

Rules:

- All header names lower-cased before comparison.
- A header in **both** `static` and `perRequest` for the same
  adapter type is an error at config load (avoid ambiguity).
- The hard denylist (S1 T-CONFIG-VALIDATE) applies to response
  YAML too, with response-specific entries: `set-cookie`,
  `www-authenticate`, `strict-transport-security`,
  `content-security-policy`, `x-frame-options`, `server`, `via`,
  `x-served-by`, `cf-ray`, `access-control-*`, `content-length`,
  `transfer-encoding`, `connection`.

## 4. Tasks

### 4.1 T-RESP-CONFIG — Extend resolver

**Files**: `packages/ai-gateway/internal/config/forward_header.go`
(extend from S1).

```go
type ResolvedResponseSet struct {
    Static     map[string]struct{}
    PerRequest map[string]struct{}
}

func (r *ResolvedAllowlist) Response(f providers.Format) ResolvedResponseSet
```

`Response(f)` returns the precomputed
`base.static ∪ perAdapterType[f].static` and
`base.perRequest ∪ perAdapterType[f].perRequest`, frozen at
startup.

The validator additionally rejects any header name appearing in
both the resolved `Static` and `PerRequest` sets for any single
Format.

### 4.2 T-RESP-FILTER — Filter function on the response path

**Files**:
- `packages/ai-gateway/internal/providers/spec_adapter.go` — add
  `filterResponseHeaders(src http.Header, set ResolvedResponseSet, isCacheHit bool) http.Header`
  that returns a new `http.Header` containing:
  - All `Static` headers from `src` that pass the lower-cased
    membership test, regardless of `isCacheHit`.
  - All `PerRequest` headers from `src` that pass the
    membership test **only when `isCacheHit == false`**. On
    cache hit, these are stripped.
  - Increments
    `ai_gateway_forward_header_dropped_total{direction="response",adapter_type=<F>}`
    for each `src` header not in either set (bucketed by name
    or `"other"`).

The function lives next to the existing request-side
`forwardHeaders()` so the symmetry is visually obvious.

### 4.3 T-RESP-WIRE-NONSTREAM — Hook into non-streaming write path

**Files**: `packages/ai-gateway/internal/handler/proxy.go`
`handleNonStream` (around line 1614 / write at 1774).

Today the non-stream path:

1. Computes `respBody`.
2. Sets `Content-Type: application/json` (hardcoded).
3. Calls `setResponseHeaders` for `x-nexus-aigw-*` headers.
4. `w.WriteHeader(http.StatusOK)` + `w.Write(respBody)`.

After this story:

1. Same compute step.
2. Call `filterResponseHeaders(execResult.Headers,
   resolved.Response(target.Format), isCacheHit=cacheHit)`.
3. Write each filtered header to `w.Header()` **before**
   `setResponseHeaders` runs — so Nexus's own `x-nexus-aigw-*`
   stamps overwrite any conflicting upstream value (Nexus wins
   on conflict per FR-FH7).
4. Set `Content-Type: application/json` (still hardcoded — the
   gateway re-encodes to canonical OpenAI shape; the upstream
   `Content-Type` is not authoritative for the response we
   actually emit).
5. `setResponseHeaders` (unchanged).
6. `w.WriteHeader` + `w.Write` (unchanged).

### 4.4 T-RESP-WIRE-STREAM — Hook into streaming write path

**Files**: same file, `handleStream` (around line 1264 / header
write at 1276).

Today the stream path:

1. Hardcodes `Content-Type: text/event-stream; charset=utf-8`,
   `Cache-Control: no-cache`, `Connection: keep-alive`.
2. Calls `setResponseHeadersStream`.
3. `w.WriteHeader(http.StatusOK)`.

After this story (insert between step 1 and step 2):

- Call `filterResponseHeaders(result.Headers,
  resolved.Response(target.Target.Format), isCacheHit=...)`.
- Write filtered headers to `w.Header()`.
- The hardcoded SSE-framing headers from step 1 still win on
  conflict (Nexus owns the SSE framing contract).

`isCacheHit` for streaming is provided by E35's cache layer —
once that work lands, the boolean is plumbed through. Until
then, treat `isCacheHit=false` (live upstream call) as the
default; the contract still holds.

### 4.5 T-RESP-NEXUS-WINS — Idempotency + override semantics

**Files**: `internal/handler/proxy.go`.

`setResponseHeaders` and `setResponseHeadersStream` use
`w.Header().Set(…)` (single value, replaces). Because they run
**after** `filterResponseHeaders` writes upstream headers, they
naturally overwrite any colliding upstream value. Add a code
comment naming this invariant:

> Order matters: filtered upstream headers go first,
> Nexus stamps go last, so Nexus wins on every conflict.
> Do not move the call sites. See FR-FH7.

### 4.6 T-METRICS — Direction label

S1 already registers the counter with a `direction` label.
Response-side filter (T-RESP-FILTER) emits with
`direction="response"`. No new metric.

### 4.7 T-DELETE-DEAD-FIELD — Optionally cement the wiring

**Files**: `packages/ai-gateway/internal/execution/executor/executor.go:274`.

The story does **not** delete `ExecutionResult.Headers` —
quite the opposite, it makes the field load-bearing for the
first time. Add a doc comment naming what reads it
(`handleNonStream` / `handleStream` via
`filterResponseHeaders`) so a future maintainer doesn't repeat
the brainstorm.

## 5. Acceptance criteria

### 5.1 Functional

- **AC-FH-S2-01** With the embedded default YAML (OpenAI +
  Anthropic opt-ins), an OpenAI live (non-cache-hit) request
  whose upstream returned
  `openai-version: 2024-02-15`, `x-request-id: req-abc`, and
  `openai-processing-ms: 137` produces a client response
  containing all three headers.

- **AC-FH-S2-02** Same request as AC-S2-01 but served from
  cache hit (cache hit fixture): `openai-version` is present
  with the cached value; `x-request-id` and
  `openai-processing-ms` are **absent**. The
  `x-nexus-aigw-*` headers, including
  `x-nexus-cache: HIT`, are present.

- **AC-FH-S2-03** A YAML setting `response.perAdapterType: {}`
  (empty) makes the gateway return zero upstream headers on
  every response. Only `x-nexus-aigw-*` headers and the
  hardcoded framing headers (`Content-Type`, etc.) are present.
  Confirms the strict-mode posture remains achievable.

- **AC-FH-S2-04** A YAML placing `set-cookie`,
  `strict-transport-security`, `server`, `via`, `cf-ray`, or
  `access-control-allow-origin` in any response list causes a
  fatal startup error.

- **AC-FH-S2-05** A YAML placing the same header in both
  `static` and `perRequest` for the same adapter type causes a
  fatal startup error naming the header.

- **AC-FH-S2-06** Upstream returning a colliding header
  (`via`, `server`) does not appear on the client response —
  these are denylisted, so they never reach the client even if
  the upstream emits them.

- **AC-FH-S2-07** When upstream and Nexus both emit a
  conflicting header (e.g. upstream provides its own
  `x-nexus-aigw-via` — adversarial), Nexus's value wins
  (because `x-nexus-aigw-*` is in the hard denylist for
  upstream-passthrough).

- **AC-FH-S2-08** Per-adapter-type isolation for response: a
  YAML adding `x-secret-debug` to
  `response.perAdapterType.openai.static`, with an Anthropic
  upstream that emits `x-secret-debug`, results in the header
  **not** appearing on the Anthropic client response.

### 5.2 Behavioral preservation

- **AC-FH-S2-09** Existing handler-level tests
  (`packages/ai-gateway/internal/handler/proxy_test.go`,
  `proxy_hook_format_test.go`) continue to pass. If those
  tests asserted on response-header shape, update fixtures so
  the embedded defaults are loaded (i.e. the new opt-ins are
  expected on OpenAI / Anthropic responses).

- **AC-FH-S2-10** SSE smoke test (`tests/...`) for streaming
  Anthropic still produces a working stream end-to-end, with
  the new opt-in headers visible at the top of the response.

### 5.3 Observability

- **AC-FH-S2-11** Prometheus counter
  `ai_gateway_forward_header_dropped_total{direction="response",adapter_type=<F>}`
  increments once per upstream-emitted header that isn't on the
  effective set for that adapter type. Cardinality bounded
  by the closed denylist + the union of allowed names + the
  `header="other"` bucket.

## 6. Coordination required (E35 SSE cache, deferred)

### 6.1 What this story commits to (the contract)

- `filterResponseHeaders(...)` accepts an `isCacheHit bool`
  parameter and strips `perRequest` headers when `true`.
- The function is the single chokepoint for "what response
  headers does the client see"; cache replay must route
  through it.

### 6.2 What E35 owns (the deferred amendment)

The cache code path must:

1. **At cache write time** — store the upstream response
   header set in the cache entry (`StreamEntry`,
   `ResponseEntry`). Store the *whole* upstream set, not the
   filtered set, so a future config change can re-evaluate
   without re-fetching upstream. Insertion in
   `docs/developers/specs/e35/e35-s1-sse-cache.md` §"T-ENTRY-TYPES": add
   `Headers http.Header` (or equivalent map) to both entry
   types.

2. **At cache read time** — pass the stored headers through
   `filterResponseHeaders(stored, resolved.Response(format),
   isCacheHit=true)` before writing to the client.
   Insertion in `docs/developers/specs/e35/e35-s1-sse-cache.md`
   §"T-PROXY-INTEG": ensure the cache-hit code path calls
   `filterResponseHeaders` exactly the same way the live path
   does.

3. **At cache key derivation** — see S1 §6 for the request-side
   allowlist hash to include in the key. Response-side adds no
   additional key contribution (the cache stores raw upstream
   headers; the active response config is applied at read
   time, so a config flip immediately changes what cached
   entries return without invalidation).

### 6.3 Why deferred

Another in-flight session is editing the SSE / non-stream cache
code and `docs/developers/specs/e35/e35-s1-sse-cache.md`. Editing the same SDD
file from this story would conflict. The handoff:

- This SDD is canonical for the **contract**.
- The E35 owner applies the §6.2 amendments inline when their
  work converges and the file becomes safe to edit.
- If §6.2's section names (`T-ENTRY-TYPES`, `T-PROXY-INTEG`)
  have moved by then, the amendment moves with them — the
  contract (1) (2) (3) above is the durable part.

## 7. Out of scope (this story)

- Request-side allowlist (S1).
- Per-Provider override layer.
- Hot reload / admin API.
- BYOK vs platform-key conditional response forwarding.
- The actual edits to `docs/developers/specs/e35/e35-s1-sse-cache.md` and
  `packages/ai-gateway/internal/cache/*` (deferred per §6).

## 8. Risks / open questions

- **R1**: Defaulting `x-request-id` and friends ON (vs starting
  fully empty) trades a small information-disclosure surface
  for a large customer-support win. Open question — confirm
  with security review whether the defaults should ship
  empty (operator opts in) instead. Flagged as a Should but
  treated as default-on in this draft.

- **R2**: Some upstreams emit `x-request-id` casing-variants
  (`X-Request-ID`, `X-Request-Id`). The lower-case
  comparison handles this; `http.Header` keys are
  canonicalized by the stdlib before user code reads them, so
  the stored value preserves the original casing. The client
  receives the upstream's casing.

- **R3**: Some response headers (e.g. Anthropic's
  `request-id`) collide in **name** with another upstream's
  header but carry different semantics. Per-adapter-type
  isolation handles this — Anthropic's `request-id` only
  passes through on Anthropic responses.

- **R4**: If a future epic opens up Bedrock / Vertex response
  forwarding, the SigV4 / GCP-specific headers (`x-amzn-*`,
  `x-goog-*`) bring their own threat model — flag as future
  work, not this story.
