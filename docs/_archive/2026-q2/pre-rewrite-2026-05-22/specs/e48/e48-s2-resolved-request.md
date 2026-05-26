# E48 S2 — ResolvedRequest L4 view + PassthroughConfig type

**Epic:** E48
**Requirements:** [e48-emergency-passthrough.md](../../../../docs/developers/specs/e48/e48-emergency-passthrough.md) — Must M6
**OpenAPI:** none (internal Go types)
**Status:** Approved (user-approved plan 2026-05-13)
**Date:** 2026-05-13
**Builds on:** [e48-s1-schema-foundation.md](e48-s1-schema-foundation.md)

---

## Architecture summary

E47-S1 introduced `requestcontext.RequestContext` as the L3 immutable per-request artefact. Built at Phase 3.5, it carries identity / normalized / endpoint / headers / rawBody. Post-routing decisions (the picked target, the effective passthrough config) cannot live on `RequestContext` because they're not yet known at construction time AND adding mutators would break E47-S1's immutability invariant.

S2 introduces `ResolvedRequest` as the L4 view that wraps the L3 RequestContext + the post-routing decisions. It is also immutable: built once at Phase 4.5 by `requestcontext.Resolve(rc, routeResult, passthroughCfg)`, then handed to every downstream consumer (hooks pipeline, audit Writer, executor, response normalize).

S2 is **pure scaffolding** — same pattern as E47-S1. The types are defined and tested; no production consumer migrates yet. S3 lands the first real consumer (Phase 4.5 attach + handler use) along with the runtime cache.

### `PassthroughConfig`

Lives in a new package `packages/ai-gateway/internal/execution/passthrough` (will host the runtime Cache in S3). Single struct mirroring the DB JSONB shape, with sane zero values that mean "no bypass" — fail-closed cold-start invariant from M4.

```go
package passthrough

type Config struct {
    Enabled         bool
    BypassHooks     bool
    BypassCache     bool
    BypassNormalize bool
    ExpiresAt       time.Time  // zero-value when not enabled
    EnabledBy       string
    Reason          string
}

// AnyBypassActive reports whether the request should skip at least one
// L4 layer. Equivalent to Enabled && (BypassHooks || BypassCache || BypassNormalize).
func (c *Config) AnyBypassActive() bool

// Flags returns the canonical ordered slice of bypass-kind strings that
// fired, for audit logging. Empty when no bypass.
func (c *Config) Flags() []string
```

A nil receiver is treated as the empty config — `(*Config)(nil).AnyBypassActive() == false`, `Flags() == nil`. This matches the cold-start fail-closed invariant.

### `ResolvedRequest`

Lives in `packages/ai-gateway/internal/pipeline/requestcontext/resolved.go`. Imports `passthrough` and `router`. The base `RequestContext` stays in `context.go` untouched.

```go
package requestcontext

import (
    "github.com/.../internal/passthrough"
    "github.com/.../internal/router"
)

type ResolvedRequest struct {
    base        *RequestContext
    route       *router.RouteResult
    passthrough *passthrough.Config
}

// Resolve constructs a ResolvedRequest by wrapping the immutable
// RequestContext + the post-routing RouteResult + the effective
// PassthroughConfig. All three pointers are retained as-is; the
// constructor does not copy or clone.
func Resolve(rc *RequestContext, route *router.RouteResult, ptc *passthrough.Config) *ResolvedRequest

// Base / Route / Passthrough return the wrapped pointers. nil-safe.
func (r *ResolvedRequest) Base() *RequestContext
func (r *ResolvedRequest) Route() *router.RouteResult
func (r *ResolvedRequest) Passthrough() *passthrough.Config

// Ergonomic delegating shortcuts (forward to base.*).
func (r *ResolvedRequest) Identity() *vkauth.VKMeta
func (r *ResolvedRequest) Normalized() *normalize.NormalizedPayload
func (r *ResolvedRequest) Endpoint() string
func (r *ResolvedRequest) Headers() http.Header
func (r *ResolvedRequest) RawBody() []byte
```

### Why a wrapper and not "RequestContext fields"

E47-S1 documented `RequestContext` as immutable after `Builder.Build()`. Adding post-routing fields would either require breaking immutability (e.g. a `setRoute(...)` method) or pre-building all fields including the post-routing ones (impossible — routing hasn't run yet).

Two clean alternatives were considered:

1. **Wrapper (S2's choice)**: `ResolvedRequest` is a separate type that holds three pointers. RequestContext stays pristine. Downstream consumers that need post-routing data take `*ResolvedRequest`; consumers that operate before routing keep taking `*RequestContext`.
2. **Lazy/optional field on RequestContext**: add `route *router.RouteResult` field that's nil until "the handler stamps it". Violates E47-S1's immutability promise; reintroduces the kind of mutable-after-Build coupling E47-S3 eliminated for Headers.

Option 1 wins: the type system pins the "pre-routing vs post-routing" boundary, downstream consumers see exactly the data they're entitled to, and E47-S1's invariants stay enforced.

### S2's deliverables vs S3's deliverables

| Concern | S2 (this story) | S3 (next story) |
|---|---|---|
| `PassthroughConfig` Go type + nil-safety | ✓ | — |
| `ResolvedRequest` type + Resolve() + getters | ✓ | — |
| Unit tests for both types | ✓ | — |
| `Cache` type (atomic snapshot, 3-tier merge) | — | ✓ |
| Hub config-applier wiring (read from `gateway_passthrough_config` shadow) | — | ✓ |
| handler/proxy.go Phase 4.5 invocation of Resolve | — | ✓ |
| First downstream consumer taking `*ResolvedRequest` | — | ✓ (intentionally just one — bypass branches land in S4) |
| Audit Writer signature change | — | S5 |
| Hook pipeline signature change | — | S4 |

S2 ships type-level infrastructure only. The Hub-driven runtime Cache + Phase 4.5 attach come together in S3 because they form a single coherent change ("when the gateway boots, it now resolves passthrough per request").

---

## Story

### S2 — ResolvedRequest L4 view + PassthroughConfig type

**User story:** As a Nexus gateway developer, I want a typed L4 view that bundles RequestContext + RouteResult + PassthroughConfig so that subsequent stories (S3-S7) can plumb post-routing decisions through downstream consumers without breaking E47-S1's RequestContext immutability invariant.

**Tasks:**

- **T2.1** — Create `packages/ai-gateway/internal/execution/passthrough/` package:
  - `doc.go`: package-level doc explaining the runtime kill-switch role, 3-tier resolution, fail-closed semantics.
  - `config.go`: `Config` struct with fields per the design above. `AnyBypassActive()` method. `Flags()` method (returns canonical ordered slice — `["bypassHooks", "bypassCache", "bypassNormalize"]` in that order for any active bypasses). Both methods nil-receiver safe.

- **T2.2** — Create `packages/ai-gateway/internal/execution/passthrough/config_test.go`:
  - `TestConfig_NilReceiver_AllSafe`: nil-receiver returns false / nil from both methods.
  - `TestConfig_EmptyConfig_NoBypass`: zero-value `Config{}` → `AnyBypassActive() == false`, `Flags() == nil`.
  - `TestConfig_DisabledButFlagsSet_NoBypass`: `{Enabled: false, BypassHooks: true}` → `AnyBypassActive() == false`. The Enabled gate is the master switch; individual flags are inert when disabled.
  - `TestConfig_OneBypassActive`: each of the three flags individually → correct Flags() return.
  - `TestConfig_AllBypassActive`: all three flags + Enabled=true → `Flags() == ["bypassHooks", "bypassCache", "bypassNormalize"]` in canonical order.

- **T2.3** — Create `packages/ai-gateway/internal/pipeline/requestcontext/resolved.go`:
  - `ResolvedRequest` struct with three unexported pointer fields.
  - `Resolve(rc, route, ptc) *ResolvedRequest` constructor — retains pointers as-is, no copy. Nil-safe: any nil input is preserved (downstream getters return nil for that slice; tests pin this).
  - Pointer-receiver getters: `Base() / Route() / Passthrough()`.
  - Ergonomic delegating shortcuts: `Identity() / Normalized() / Endpoint() / Headers() / RawBody()`. Each returns the corresponding `Base().<field>` via the existing RequestContext getter.
  - All getters nil-receiver-safe (return the zero value of their return type on nil ResolvedRequest).

- **T2.4** — Create `packages/ai-gateway/internal/pipeline/requestcontext/resolved_test.go`:
  - `TestResolve_AllNonNil_ReturnsWrappedPointers`: build a base + route + passthrough; assert each getter returns the supplied pointer identity.
  - `TestResolve_NilInputs_AreRetained`: `Resolve(nil, nil, nil)` returns non-nil ResolvedRequest where every getter returns nil.
  - `TestResolvedRequest_Delegates_ForwardToBase`: Identity / Normalized / Endpoint / Headers / RawBody all delegate to `Base()`'s getters and return identical values.
  - `TestResolvedRequest_NilReceiver_AllSafe`: `(*ResolvedRequest)(nil).Base() == nil` for every getter.

- **T2.5** — Build + test:
  - `go build ./packages/ai-gateway/...`
  - `go test -race -count=1 ./packages/ai-gateway/internal/execution/passthrough/... ./packages/ai-gateway/internal/pipeline/requestcontext/...`

**Acceptance:**

- `packages/ai-gateway/internal/execution/passthrough/` exists with `Config` type + 5 unit tests passing.
- `packages/ai-gateway/internal/pipeline/requestcontext/resolved.go` exists with `ResolvedRequest` + `Resolve()` + 4 unit tests passing.
- No production consumer references `*ResolvedRequest` yet — S2 is pure scaffolding. `grep -rn "ResolvedRequest" packages/ai-gateway/internal/handler/` returns zero matches.
- `go build ./packages/ai-gateway/...` clean.
- E47-S1's `RequestContext` is unchanged in this PR.

**Validation script:**

```bash
go build ./packages/ai-gateway/...
go test -race -count=1 -v -run "TestConfig|TestResolve|TestResolvedRequest" \
  ./packages/ai-gateway/internal/execution/passthrough/... \
  ./packages/ai-gateway/internal/pipeline/requestcontext/...

grep -rn "requestcontext\.ResolvedRequest\|passthrough\.Config" \
  packages/ai-gateway/internal/handler/ packages/ai-gateway/internal/router/
# expected: zero — S2 is pure scaffolding, S3 wires the first consumer.
```
