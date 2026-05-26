# E47 S3 — SafeHeaders typed trust boundary

**Epic:** E47
**Requirements:** [e47-routing-canonical-payload.md](../../../../docs/developers/specs/e47/e47-routing-canonical-payload.md) — Must M3
**OpenAPI:** none (internal type swap; admin/simulate API contract unchanged)
**Status:** Approved (user-approved plan 2026-05-13)
**Date:** 2026-05-13
**Builds on:** [e47-s2-routing-context-canonical-payload.md](e47-s2-routing-context-canonical-payload.md)

---

## Architecture summary

E47-S2 deleted the `RoutingContext.Headers["x-smart-messages"]` write/read pair — the only place where gateway-internal data was being smuggled through the inbound-headers map. After S2 the only legitimate consumer of `RoutingContext.Headers` is the matchConditions predicate matcher (`matcher.go:96`) which evaluates operator-supplied paths like `headers.x-customer-tier` against the inbound request's HTTP headers.

S3 makes the structural separation between "external HTTP headers" and "internal routing state" enforceable by the type system. The field changes from `Headers map[string]string` to `Headers SafeHeaders` — an opaque type whose only public API is `Get(name string) string`. There is no exported `Set`, no exported `Add`, no exported raw-map accessor. Internal code that wants to smuggle data into routing must add it as a typed field on `RoutingContext` (the path E47-S2 took for canonical messages) or via a new explicit RoutingContext field; it cannot do so through the headers bucket.

### Why a type, not a comment

The previous design relied on convention ("don't write internal data to Headers") and that convention failed loudly when the smart-routing plumbing landed. A type that lacks the operations needed to violate the convention makes the violation a compile error rather than a code-review catch. The deny list for auth-bearing headers (Authorization, Cookie, X-API-Key) moves from a free-floating `flattenHeaders` helper to the type's constructor, centralising the trust filter so operators reading routing predicate logs cannot accidentally see secret material.

### `SafeHeaders` shape

```go
package router

type SafeHeaders struct {
    h http.Header // unexported; never returned by reference
}

func NewSafeHeaders(h http.Header) SafeHeaders
func (s SafeHeaders) Get(name string) string  // case-insensitive; returns "" when absent
```

`NewSafeHeaders` is the only construction path. It copies the inbound `http.Header` into a fresh internal `http.Header`, dropping deny-listed names (`Authorization`, `Cookie`, `X-API-Key`). Lookup goes through `net/http.Header.Get`, which already handles case-insensitivity, so operator predicates can use the canonical `x-customer-tier` form regardless of how the inbound header was capitalised.

Zero value (`SafeHeaders{}`) is a usable empty view — `Get` on a nil internal map returns `""`. This matches the existing semantics of `map[string]string` zero value: routing simulate / synthetic-context call sites that did not previously set `Headers` continue to work without modification.

### Decisions

- **Single-value `Get`, not multi-value `Values`**. `flattenHeaders` joined multi-value headers with `", "` so operator predicates saw the joined string. Operators predicate on identity-style values (customer tier, source-app id) which are single-valued in practice; the net/http convention of "first value wins" is appropriate. Pre-GA: behaviour change is acceptable.
- **No `Set` / no `Add` exported.** The single intended construction is via `NewSafeHeaders`. Tests that want to populate headers do so by constructing an `http.Header` first, then `NewSafeHeaders`.
- **No name whitelist (operator predicate language stays free-form).** The matchConditions language supports arbitrary `headers.<any-name>` paths; constraining `Get` to a fixed enum would break that. The trust boundary is unidirectional (no writes from gateway-internal code), not membership-restricted.
- **Deletion of `flattenHeaders`.** Its callers (the one in `resolveRoute`) move to `NewSafeHeaders`; the function is unused after migration and is deleted in the same PR.

### Migration surface

| File | Change |
|---|---|
| `router/safeheaders.go` | NEW. `SafeHeaders` + `NewSafeHeaders`. |
| `router/safeheaders_test.go` | NEW. Unit tests covering deny list, case-insensitive Get, zero value, multi-value first-wins. |
| `router/types.go` | `RoutingContext.Headers map[string]string` → `RoutingContext.Headers SafeHeaders`. Doc-comment updated to drop the "transitional" notice from S2. |
| `router/matcher.go:96` | `ctx.Headers[after]` → `ctx.Headers.Get(after)`. |
| `router/matcher_test.go` | Tests that set `Headers: map[string]string{...}` migrate to `Headers: router.NewSafeHeaders(http.Header{...})`. |
| `handler/proxy.go` | `Headers: flattenHeaders(rctxFull.Headers())` → `Headers: router.NewSafeHeaders(rctxFull.Headers())`. `flattenHeaders` function deleted. |
| `handler/routing_simulate_endpoint.go` | No change — already doesn't set Headers. |

---

## Story

### S3 — SafeHeaders typed trust boundary

**User story:** As a Nexus gateway developer, when I read the `router.RoutingContext` definition, I want the inbound-headers field to be a type whose API makes it syntactically impossible to write internal data into it — so the smart-routing plumbing class of bug cannot be reintroduced by a future feature.

**Tasks:**

- **T3.1** — Create `packages/ai-gateway/internal/router/safeheaders.go`:
  - Package-level doc-comment describing the trust-boundary rationale and pointing at the E47-S2 commit as the precedent for what this prevents.
  - `SafeHeaders` struct with one unexported field `h http.Header`.
  - `var blockedHeaders` (unexported map[string]struct{} with keys `"authorization"`, `"cookie"`, `"x-api-key"`).
  - `func NewSafeHeaders(h http.Header) SafeHeaders` — returns zero value when `len(h) == 0`; otherwise builds a fresh `http.Header` filtered through `blockedHeaders`. Sensitive names are matched case-insensitively (`strings.ToLower` on the input keys).
  - `func (s SafeHeaders) Get(name string) string` — returns `""` when the internal map is nil, otherwise delegates to `http.Header.Get`.

- **T3.2** — Create `packages/ai-gateway/internal/router/safeheaders_test.go`:
  - `TestSafeHeaders_FiltersBlockedHeaders` — Authorization / Cookie / X-API-Key are stripped.
  - `TestSafeHeaders_DenyListIsCaseInsensitive` — `Authorization`, `AUTHORIZATION`, `authorization` all dropped.
  - `TestSafeHeaders_GetIsCaseInsensitive` — `Get("x-customer-tier")` matches an inbound `X-Customer-Tier`.
  - `TestSafeHeaders_GetOnAbsentHeader_ReturnsEmpty`.
  - `TestSafeHeaders_ZeroValue_IsUsable` — `SafeHeaders{}.Get("anything") == ""`.
  - `TestNewSafeHeaders_EmptyInput_ReturnsZeroValue` — `NewSafeHeaders(nil)` and `NewSafeHeaders(http.Header{})` both yield the zero value (no allocation).
  - `TestSafeHeaders_MultiValueHeader_ReturnsFirst` — when the same header appears with two values, `Get` returns the first one (matches `http.Header.Get` semantics).

- **T3.3** — `packages/ai-gateway/internal/router/types.go`:
  - Change field type: `Headers SafeHeaders` (no longer `map[string]string`).
  - Update the doc-comment: drop the "transitional" notice from S2 and replace with the structural-trust-boundary statement (no writes from gateway-internal code).

- **T3.4** — `packages/ai-gateway/internal/router/matcher.go`:
  - Line 96: change `return ctx.Headers[after]` to `return ctx.Headers.Get(after)`.

- **T3.5** — `packages/ai-gateway/internal/router/matcher_test.go`:
  - Update every fixture that builds a `RoutingContext` with `Headers: map[string]string{...}` to use `Headers: router.NewSafeHeaders(http.Header{...})`. Since matcher_test.go is inside the `router` package, the call is `NewSafeHeaders` without the package qualifier.

- **T3.6** — `packages/ai-gateway/internal/handler/proxy.go`:
  - `resolveRoute`: change `Headers: flattenHeaders(rctxFull.Headers())` to `Headers: router.NewSafeHeaders(rctxFull.Headers())`.
  - Delete the `flattenHeaders` function and any test fixtures referencing it.

- **T3.7** — Run `go build`, `go test -race -count=1`, and `go vet` across `packages/ai-gateway/...`. All three pass green for the router and requestcontext packages; the three pre-existing failing tests in `internal/handler/` (quality-checker + 2 PII modify paths, latent since commit 59b286b3) remain unrelated and unchanged.

**Acceptance:**

- `RoutingContext.Headers` field type is `SafeHeaders`. Compile-time impossible to assign a `map[string]string` or write `ctx.Headers["x-foo"] = "..."` from any package.
- `Authorization`, `Cookie`, `X-API-Key` (and case variants) are filtered out of the routing-visible header set.
- Matcher predicates of the form `{ "headers.x-customer-tier": "enterprise" }` continue to evaluate correctly against `SafeHeaders.Get("x-customer-tier")`.
- `flattenHeaders` is deleted; no caller remains.
- `go test -race -count=1 ./packages/ai-gateway/internal/router/...` is green.

**Validation script (for the reviewer):**

```bash
go build ./packages/ai-gateway/...
go test -race -count=1 ./packages/ai-gateway/internal/router/...
go vet ./packages/ai-gateway/...

# Trust-boundary invariants
grep -rn "ctx\.Headers\[" packages/ai-gateway/   # expected: zero (no direct map index access)
grep -rn "rctx\.Headers\[" packages/ai-gateway/  # expected: zero
grep -rn "Headers: map\[string\]string" packages/ai-gateway/  # expected: zero (raw-map RoutingContext writes)
grep -rn "flattenHeaders" packages/ai-gateway/   # expected: zero
```
