# E47 S2 — RoutingContext canonical payload + delete x-smart-messages plumbing (THE BUG FIX)

**Epic:** E47
**Requirements:** [e47-routing-canonical-payload.md](../../../../docs/developers/specs/e47/e47-routing-canonical-payload.md) — Must M1, M2; Should S1
**OpenAPI:** none (internal routing wiring; admin API contract unchanged)
**Status:** Approved (user-approved plan 2026-05-13)
**Date:** 2026-05-13
**Builds on:** [e47-s1-request-context.md](e47-s1-request-context.md)

---

## Architecture summary

S2 is the architectural cleanup that fixes the smart-routing missing-user-messages bug. The bug is a brittle handler↔strategy contract — the handler only extracts the request `messages` when the client-submitted `model` equals the literal `"auto"`, and the extracted JSON-string is shipped to the smart strategy via `RoutingContext.Headers["x-smart-messages"]`. Any other `matchConditions` shape silently bypasses the extraction; the smart strategy then builds a router-LLM request with no user messages and the Anthropic codec rejects it loudly while the OpenAI codec accepts it silently with degraded decision quality.

S2 replaces the entire plumbing with a typed `RoutingContext.Request *normalize.NormalizedPayload` field populated unconditionally by a single `normalize.Registry.Normalize` call at Phase 3.5. The smart strategy reads `Request.Messages` directly, filters for `role=user`, and hands them as `[]normalize.Message` to `buildRouterRequestBody`. No JSON round-trip, no header map detour, no model-name gate.

This makes the routing layer the second L4 consumer (after E46-S6 hooks) of the canonical normalized payload. The earlier "two parses per request" path is collapsed into one: hooks continues to use its synthetic `PayloadFromTextSegments` for the moment (it does not need role discrimination; switching it is a downstream optimisation, not S2 scope), but routing's parse is the new authoritative canonical artefact, and the handler now holds a `*RequestContext` (the S1 type) at Phase 3.5 that S3-S5 will extend.

### State diff at a glance

```
                         BEFORE                                  AFTER
                         ──────                                  ─────
handler/proxy.go         if modelID == "auto" {                  rctxFull := requestcontext.NewBuilder().
                             smartMessages =                          WithIdentity(vkMeta).
                                 extractSmartMessages(body)           WithNormalized(&canonical).
                         }                                            WithEndpoint(endpointType).
                         ResolveTargets(&RouteRequest{                WithHeaders(r.Header).
                             ...                                      WithRawBody(body).Build()
                             Messages: smartMessages,             rctx := buildRoutingContext(rctxFull, modelID, endpointType)
                         })                                       ResolveTargets(rctx)

router/types.go          type RouteRequest struct {              type RouteRequest --- DELETED
                             Model, Endpoint string
                             VK *VKContext                        type RoutingContext struct {
                             Headers map[string]string                ...
                             Messages string                          Request *normalize.NormalizedPayload  // NEW
                         }                                            // Headers map preserved; S3 → SafeHeaders
                                                                  }
router/resolver.go       func ResolveTargets(ctx, *RouteRequest)  func ResolveTargets(ctx, *RoutingContext)
                             rctx.Headers["x-smart-messages"] =
                                 req.Messages                     (header write deleted)

router/strategy_smart.go userMessages :=                          var userMsgs []normalize.Message
                             rctx.Headers["x-smart-messages"]     for _, m := range rctx.Request.Messages {
                         body := buildRouterRequestBody(              if m.Role == normalize.RoleUser {
                             ..., userMessages /*JSON*/)               userMsgs = append(userMsgs, m)
                                                                      }
                                                                  }
                                                                  body := buildRouterRequestBody(..., userMsgs)

routing_simulate_       rctx.Headers = map[string]string{        rctx.Request = synthesizeNormalized(req.Messages)
endpoint.go              "x-smart-messages": json.Marshal(...)}
```

### What is NOT in S2

S2 deliberately preserves three things that future stories migrate:

1. **`RoutingContext.Headers map[string]string`** — kept as today's raw map. S3 introduces `SafeHeaders` and migrates all reads. Today no production strategy other than `smart` reads from this map; the only routing-internal write (`x-smart-messages`) is what S2 deletes, so post-S2 the map is purely transitional.
2. **Hooks pipeline payload source** — hooks continue to call `extractRequestContentForHooks` → `PayloadFromTextSegments` (synthetic). Switching hooks to the canonical `*RequestContext.Normalized` is straightforward but changes hook input shape; the optimisation is unrelated to the bug-fix scope and is tracked as an E46 follow-up.
3. **Smart strategy negative-case short-circuit** — when `Request.Messages` filtered to `role=user` is empty, S2 lets the existing code path proceed (which today calls the router LLM with system-only messages and falls back when the call errors). **S5** adds the explicit short-circuit that skips the router LLM call entirely with a typed trace entry. S2's bug-fix scope is purely "do user messages reach the strategy"; S5's scope is "what happens when they legitimately are not there".
4. **`RouterLLMClient` interface decoupling** — S2 leaves the smart strategy importing `provider adapters` and constructing wire-format bodies inline. **S4** extracts that into a typed dependency. S2 changes the inputs (`[]normalize.Message` instead of `userMessagesJSON string`); it does not change the strategy's outbound interface.

### Phase 3.5 contract

After auth + rate-limit and before routing, the handler performs exactly one normalize call:

```go
canonical, _ := h.deps.NormalizeRegistry.Normalize(r.Context(), body, normalize.Meta{
    AdapterType:  string(in.BodyFormat),
    Model:        modelID,
    ContentType:  r.Header.Get("Content-Type"),
    Direction:    normalize.DirectionRequest,
    EndpointPath: r.URL.Path,
})
```

Failure modes:

- `ErrUnsupported` from every chained normalizer — returns `Kind: KindUnsupported`. The handler stores this on the RequestContext anyway (so audit can see "the request was not recognisable as any known wire format"). Smart routing receiving a non-AI-kind payload falls back to default — but the explicit short-circuit lands in S5; S2 inherits today's behaviour for this case.
- Real parse error (protocol matched but malformed body) — returns a partial `NormalizedPayload` with `error`. The handler proceeds; the partial payload still allows routing to inspect `.Messages` (which may be empty). Smart routing falls back to default per the same path as above.
- Empty body — handler short-circuits the normalize call (`len(body) == 0`); rctxFull holds a nil `Normalized`.

The Phase 3.5 call is **not** the only normalize site in the gateway — `auditWriter.WithNormalizer(...)` keeps its own `BuildAuditFn` registered against the same registry, so audit writes always get a fresh parse from the raw bytes at record-write time. (Sharing the parse with audit is an optimisation, not a correctness need; it can land in a later cleanup PR.)

---

## Story

### S2 — RoutingContext canonical payload + delete x-smart-messages plumbing

**User story:** As a gateway operator, when I configure a smart routing rule with any `matchConditions` shape — including `{}` (matches everything) or literals other than `"auto"` — I want the smart strategy to make routing decisions grounded in the actual user prompt, not in an empty system-only message that codec validation rejects or that OpenAI's router silently degrades on.

**Tasks:**

- **T2.1** — `packages/ai-gateway/internal/router/types.go`:
  - Add field `Request *normalize.NormalizedPayload` to `RoutingContext`.
  - Delete the entire `RouteRequest` struct (lines 233-241).
  - Update the `RoutingContext` doc-comment to describe `Request` as "the canonical request payload built once at Phase 3.5; nil for non-body endpoints (`/v1/models` etc.) or when normalize failed".
  - Leave `Headers map[string]string` untouched; S3 migrates.

- **T2.2** — `packages/ai-gateway/internal/router/resolver.go`:
  - Change `ResolveTargets(ctx, *RouteRequest) (*RouteResult, error)` to `ResolveTargets(ctx, *RoutingContext) (*RouteResult, error)`. The body collapses to "call Resolve, flatten, health-rerank" — the local `rctx := &RoutingContext{...}` builder and the `Headers["x-smart-messages"]` injection are deleted.
  - Update doc-comments referencing `RouteRequest`.

- **T2.3** — `packages/ai-gateway/internal/router/strategy_smart.go`:
  - Replace lines 232-236 (read from `rctx.Headers["x-smart-messages"]`) with:
    ```go
    var userMsgs []normalize.Message
    if rctx.Request != nil {
        for _, m := range rctx.Request.Messages {
            if m.Role == normalize.RoleUser {
                userMsgs = append(userMsgs, m)
            }
        }
    }
    routerReqBody := buildRouterRequestBody(cfg, routerTarget.ProviderModelID, catalog, userMsgs)
    ```
  - Change `buildRouterRequestBody` signature from `(cfg *SmartConfig, routerProviderModelID, catalog, userMessagesJSON string)` to `(cfg *SmartConfig, routerProviderModelID, catalog string, userMsgs []normalize.Message)`. Delete the inner `json.Unmarshal` and the "Parse user messages from the JSON passed via header" block. The truncation-to-3 logic stays. When extracting text from `normalize.Message`, iterate `m.Content` and concatenate `Type == ContentText` entries to a single string for the router-LLM prompt (which is OpenAI-shape and takes flat strings).
  - Remove the doc-comment referencing "x-smart-messages header" on `buildRouterRequestBody`.

- **T2.4** — `packages/ai-gateway/internal/handler/proxy.go`:
  - Delete the `// Phase 3.5: Smart routing — extract user messages for model="auto".` block (lines 338-342) including the `if modelID == "auto"` guard and the `extractSmartMessages(body)` call.
  - Delete the `extractSmartMessages` function (lines 875-881).
  - Add a new Phase 3.5 block that:
    1. Calls `h.deps.NormalizeRegistry.Normalize(r.Context(), body, meta)` once.
    2. Constructs a `*requestcontext.RequestContext` via the S1 Builder.
    3. Stores the resulting `rctxFull` in a local variable for downstream use.
  - Change `resolveRoute` signature from `(ctx, model, endpointType, smartMessages string, vkMeta *vkauth.VKMeta, headers http.Header)` to `(ctx, rctxFull *requestcontext.RequestContext, modelID, endpointType string)` (or similar). Inside, build the `*router.RoutingContext` from `rctxFull` + the routing-specific fields; pass to `Router.ResolveTargets`.

- **T2.5** — `packages/ai-gateway/internal/handler/interfaces.go`:
  - Update the `RouteResolver` interface signature from `ResolveTargets(ctx, *router.RouteRequest) (*router.RouteResult, error)` to `ResolveTargets(ctx, *router.RoutingContext) (*router.RouteResult, error)`.

- **T2.6** — `packages/ai-gateway/internal/handler/proxy.go` Deps:
  - Add field `NormalizeRegistry *normalize.Registry` to the `Deps` struct.
  - Update `packages/ai-gateway/cmd/ai-gateway/main.go` to pass the existing `normalizeRegistry` into `Deps`. (The variable already exists at `main.go:441` — currently only used by audit.)

- **T2.7** — `packages/ai-gateway/internal/handler/routing_simulate_endpoint.go`:
  - Delete lines 201-206 (the `Headers["x-smart-messages"]` write block).
  - Replace with a synthesise-NormalizedPayload helper: for each `simulateRequest.Messages` entry (typed `[]map[string]any` with optional `role` and `content` keys), build a `normalize.Message{Role: ..., Content: [{Type: ContentText, Text: ...}]}`. Set `rctx.Request` to the synthetic `*NormalizedPayload`.

- **T2.8** — Tests:
  - `packages/ai-gateway/internal/handler/routing_simulate_endpoint_test.go`: replace the `gotHeaders["x-smart-messages"]` assertions (lines 215-232) with assertions on `rctx.Request.Messages`. Both positive case (messages present → `rctx.Request.Messages` populated) and negative case (no messages → `rctx.Request == nil` or empty Messages).
  - `packages/ai-gateway/internal/handler/embeddings_crossformat_test.go` and `proxy_cache_capture_test.go`: replace mock `ResolveTargets(ctx, *router.RouteRequest)` signatures with `ResolveTargets(ctx, *router.RoutingContext)`. Re-verify the test passes through routing as expected.
  - `packages/ai-gateway/internal/router/strategy_smart_test.go`: rewrite test fixtures to set `rctx.Request` directly (typed `*normalize.NormalizedPayload`) instead of `rctx.Headers["x-smart-messages"]`. Cover at minimum: (a) populated user-content path → router-LLM mock returns selection, (b) empty `rctx.Request` → falls back, (c) `rctx.Request` with only assistant role → falls back (today's behaviour; S5 distinguishes the trace).
  - `packages/ai-gateway/internal/router/resolver_test.go` and any nearby tests constructing `RouteRequest`: migrate to direct `RoutingContext` construction.

- **T2.9** — Reproduce the prod bug locally per the filed bug report's "Reproduction" section and assert it is fixed:
  1. Set a local routing rule with `strategyType=smart` and `matchConditions={}`.
  2. Configure the router LLM as an Anthropic-shape model.
  3. Send a request with `model=claude-opus-4-7` and a real user prompt.
  4. Inspect the audit `routing_trace`: the first `decision` entry must start with `"selected …"` and must not contain the substring `"router LLM error"`.
  5. Repeat with `model=auto` to confirm the previously-working path is unchanged.

- **T2.10** — Run `go build ./packages/ai-gateway/...`, `go test -race -count=1 ./packages/ai-gateway/...`, and `go vet ./packages/ai-gateway/...`. All must be clean.

**Acceptance:**

- `grep -rn "x-smart-messages" packages/ai-gateway/` returns **zero matches** (header literal eliminated end-to-end).
- `grep -rn "extractSmartMessages" packages/ai-gateway/` returns **zero matches** (function deleted).
- `grep -rn "RouteRequest" packages/ai-gateway/` returns **zero matches** (type deleted; callers migrated).
- `grep -n 'if modelID == "auto"' packages/ai-gateway/internal/handler/proxy.go` returns **zero matches** (or matches only the unrelated embeddings-endpoint guard at proxy.go:761 which is outside S2 scope).
- `go test -race -count=1 ./packages/ai-gateway/...` passes.
- The prod bug reproduction (T2.9) shows the routing trace's first decision is a `"selected ..."` entry, not `"router LLM error: ... anthropic: no user/assistant messages"`.
- `RoutingContext.Headers map[string]string` is preserved (S3 migrates).
- No new TODO / FIXME / stub markers in production code.

**Validation script (for the reviewer):**

```bash
go build ./packages/ai-gateway/...
go test -race -count=1 ./packages/ai-gateway/...
go vet ./packages/ai-gateway/...

# Plumbing fully deleted
grep -rn "x-smart-messages" packages/ai-gateway/   # expected: zero
grep -rn "extractSmartMessages" packages/ai-gateway/   # expected: zero
grep -rn "RouteRequest" packages/ai-gateway/   # expected: zero

# Canonical wiring present
grep -n "Request.*\*normalize.NormalizedPayload" packages/ai-gateway/internal/router/types.go   # expected: one
grep -n "NormalizeRegistry" packages/ai-gateway/internal/handler/proxy.go   # expected: at least one
```
