# E47 S1 — Foundation: requestcontext package

**Epic:** E47
**Requirements:** [e47-routing-canonical-payload.md](../../../../docs/developers/specs/e47/e47-routing-canonical-payload.md)
**OpenAPI:** none (internal Go package; no API contract)
**Status:** Approved (user-approved plan 2026-05-13, sequential per-Story PR cadence)
**Date:** 2026-05-13

---

## Architecture summary

E47 establishes an L3 (request context) layer between L2 wire-format ingress and L4 policy plane in the ai-gateway request path. The L3 artefact is a per-request, immutable `RequestContext` carrying the authenticated identity, the canonical normalized payload, the resolved endpoint, the inbound HTTP headers, and a reference to the raw bytes for audit / spill. Downstream consumers (routing, hooks, audit) read `RequestContext` via getters; they do not parse the request body themselves and do not share mutable state.

S1 is **pure scaffolding**: it introduces the `requestcontext` package — the `RequestContext` type, the `Builder`, and the package-level documentation — without touching any handler call site. The handler integration and the consumer migration land in S2, where they pair naturally with the smart-routing bug fix; landing them in S1 alone would either ship dead code or entangle two architectural concerns in one PR.

### Why "pure scaffolding" for S1

Investigation during planning revealed that `extractRequestContentForHooks` (`proxy.go:1298`) does **not** return a fully canonical `NormalizedPayload`. It calls `goHooks.PayloadFromTextSegments`, which produces a synthetic payload with `Protocol: "synthetic"` and a single fabricated `role=user` message wrapping all extracted text segments. The smart-routing strategy needs message-level role discrimination (filter for `role=user` only) and therefore cannot use the synthetic shape. The real canonical payload comes from `normalize.Registry.Normalize` — already wired into `main.go` for the audit writer, but never invoked at the request-handling path.

Promoting the synthetic-payload function to Phase 3.5 (per the original task description) would have shipped a degraded canonical payload to a routing layer that requires the proper one. S2 will instead invoke `normalize.Registry.Normalize` directly at Phase 3.5 and route the result to all consumers. S1 lays down the type that S2 will carry that result through.

### Package structure

```
packages/ai-gateway/internal/pipeline/requestcontext/
  doc.go              // package-level documentation
  context.go          // RequestContext + Builder
  context_test.go     // unit tests for Builder + getters + immutability
```

### `RequestContext` shape

```go
type RequestContext struct {
    identity   *vkauth.VKMeta
    normalized *normalize.NormalizedPayload
    endpoint   string
    headers    http.Header
    rawBody    []byte
}

func (rc *RequestContext) Identity() *vkauth.VKMeta { ... }
func (rc *RequestContext) Normalized() *normalize.NormalizedPayload { ... }
func (rc *RequestContext) Endpoint() string { ... }
func (rc *RequestContext) Headers() http.Header { ... }
func (rc *RequestContext) RawBody() []byte { ... }
```

Fields are unexported; reads go through pointer-receiver getters. The struct is treated as immutable by convention once `Builder.Build()` returns it; getters intentionally do not return copies of slice/map fields (the convention is "do not mutate"). The `headers` and `rawBody` slices are owned by the caller; the Builder accepts them as-is and the getters hand back the same reference. This trades belt-and-braces defensive copying for the lower-overhead "trust the contract" path normal in Go's `http.Handler` chain; reviewers should treat any consumer that mutates `rc.Headers()` or `rc.RawBody()` as a defect.

The `headers` field is the inbound `http.Header` map at S1. **S3 will replace this with a typed `SafeHeaders`** that exposes only a whitelisted `HeaderName` enum via `Get(HeaderName) string`. The S1 raw-map field is transitional; consumers must not start depending on the field shape directly because it changes in S3.

### `Builder` API

```go
type Builder struct {
    rc *RequestContext
}

func NewBuilder() *Builder
func (b *Builder) WithIdentity(v *vkauth.VKMeta) *Builder
func (b *Builder) WithNormalized(p *normalize.NormalizedPayload) *Builder
func (b *Builder) WithEndpoint(e string) *Builder
func (b *Builder) WithHeaders(h http.Header) *Builder
func (b *Builder) WithRawBody(b []byte) *Builder
func (b *Builder) Build() *RequestContext
```

Fluent style. `Build()` returns the populated `*RequestContext`. After `Build()`, the Builder may be reused (a fresh `Build()` returns a new RequestContext with the same field values) — though typical usage is "one Builder per request".

### What S1 does NOT do

- No changes to `packages/ai-gateway/internal/handler/`. No Phase 3.5 wiring, no `Deps.NormalizeRegistry` field.
- No changes to `packages/ai-gateway/internal/router/`. No `RoutingContext.Request` field yet.
- No changes to hooks or audit consumers. They continue to use `extractRequestContentForHooks` (synthetic payload) until S2.
- No deletion of `x-smart-messages` plumbing. The bug remains live. S2 is the bug-fix PR.
- No SafeHeaders. The Headers getter returns `http.Header` at S1 and migrates to `SafeHeaders` at S3.

---

## Story

### S1 — Foundation: requestcontext package

**User story:** As a Nexus gateway developer, I want an immutable typed `RequestContext` available in `internal/requestcontext/` so that subsequent stories (S2-S5) can plumb the canonical normalized payload from the handler down to routing, hooks, and audit without inventing a new container at every consumer.

**Tasks:**

- **T1.1** — Create `packages/ai-gateway/internal/pipeline/requestcontext/doc.go` describing:
  - The L3 position in the L1-L6 layering (see `docs/users/product/architecture.md` E47 section).
  - The immutability convention: once `Builder.Build()` returns, the struct is read-only by convention; getters hand back references to caller-supplied slices/maps without defensive copying.
  - The promise that the type does not depend on, and is not depended upon by, `internal/router/`. (Strategies receive a `RequestContext` view at the resolver boundary; they do not import the package.)
- **T1.2** — Create `packages/ai-gateway/internal/pipeline/requestcontext/context.go` with:
  - `RequestContext` struct (unexported fields: `identity`, `normalized`, `endpoint`, `headers`, `rawBody`).
  - Pointer-receiver getters: `Identity()`, `Normalized()`, `Endpoint()`, `Headers()`, `RawBody()`. Each getter is safe to call on a nil receiver; nil receiver returns the zero value of the return type.
  - `Builder` struct with internal `*RequestContext`.
  - `NewBuilder()` constructor that returns a Builder over a fresh `RequestContext{}`.
  - `WithIdentity`, `WithNormalized`, `WithEndpoint`, `WithHeaders`, `WithRawBody` fluent setters. Each returns the Builder for chaining.
  - `Build() *RequestContext` returning the populated struct. Calling `Build()` multiple times returns the same pointer (Builder is a one-shot factory); callers wanting an independent context construct a fresh Builder.
- **T1.3** — Create `packages/ai-gateway/internal/pipeline/requestcontext/context_test.go` covering:
  - Builder fluent chain — `NewBuilder().WithIdentity(v).WithNormalized(p).Build()` returns a RequestContext whose getters surface the supplied values.
  - Each getter returns the field as-supplied (identity / normalized / endpoint / headers / rawBody).
  - Nil-receiver safety — calling `(*RequestContext)(nil).Normalized()` returns nil; same for every getter.
  - Partial population — missing setters leave the corresponding getter returning the zero value (`nil`, `""`).
  - Reuse — calling `Build()` twice returns the same pointer (documented one-shot semantic); a separate Builder is needed for an independent RequestContext.
- **T1.4** — Run `go build ./packages/ai-gateway/...` and `go test -race -count=1 ./packages/ai-gateway/internal/pipeline/requestcontext/...` to verify the package compiles and tests pass. Run `go vet ./packages/ai-gateway/internal/pipeline/requestcontext/...` for lint.

**Acceptance:**

- The `requestcontext` package builds without errors under the workspace.
- `go test -race -count=1 ./packages/ai-gateway/internal/pipeline/requestcontext/...` is green.
- The package imports only `net/http`, `packages/shared/transport/normalize`, and `packages/ai-gateway/internal/pipeline/vkauth` (or the equivalent identity type). No cycle with `internal/router/` or `internal/handler/`.
- `RequestContext` is not constructed anywhere outside the test file in S1; the handler integration lands in S2.
- The S1 PR description references this SDD and notes that it is the first of six sequential PRs for E47.

**Validation script (for the reviewer):**

```bash
go build ./packages/ai-gateway/...
go test -race -count=1 ./packages/ai-gateway/internal/pipeline/requestcontext/...
go vet ./packages/ai-gateway/internal/pipeline/requestcontext/...
grep -rn "requestcontext\." packages/ai-gateway/internal/handler/  # expected: no matches
grep -rn "requestcontext\." packages/ai-gateway/internal/router/   # expected: no matches
```
