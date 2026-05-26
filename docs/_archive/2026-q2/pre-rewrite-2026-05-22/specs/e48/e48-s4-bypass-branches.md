# E48 S4 — Bypass branches (hooks / cache / response-normalize)

**Epic:** E48
**Requirements:** [e48-emergency-passthrough.md](../../../../docs/developers/specs/e48/e48-emergency-passthrough.md) — Must M1 (runtime behaviour)
**OpenAPI:** none
**Status:** Approved (user-approved plan 2026-05-13)
**Date:** 2026-05-13
**Builds on:** [e48-s3-runtime-wiring.md](e48-s3-runtime-wiring.md)

---

## Architecture summary

S3 made `*ResolvedRequest` available on `r.Context()` at Phase 4.5 — but no consumer reads from it yet. S4 wires the three bypass branches the L4 policy plane consumers need:

1. **`bypassHooks`** — skip the Phase 5 request-hooks pipeline AND the Phase 7 response-hooks pipeline (which also covers the SSE live compliance pipeline).
2. **`bypassCache`** — skip cache lookup (and therefore cache write, which only runs after a miss).
3. **`bypassNormalize`** — skip the audit Writer's response-side normalize emission (the `traffic_event_normalized.response_*` columns).

### bypassNormalize scope clarification

The requirements doc originally said `bypassNormalize` skips BOTH the Phase 3.5 request normalize AND the response-side normalize emission. Implementation revealed an ordering problem: Phase 3.5 normalize runs BEFORE Phase 4 routing, which is BEFORE Phase 4.5 passthrough resolution — so we cannot decide per-provider whether to skip the request normalize without knowing the routed target. The Phase 3.5 normalize call is also fast (~500µs) and fail-open (E46 contract), so skipping it is a minor optimisation, not a correctness need.

S4 therefore implements `bypassNormalize` as **response-side only** — skip the audit Writer's normalize call that produces `traffic_event_normalized.response_normalized`. This is exactly the failure mode the toggle was designed to guard: response-side normalize panics on new provider response fields are the empirically-observed failure shape. The architecture doc's wording is adjusted in S4's commit.

Request-side normalize bug-tolerance is delivered "for free" by E46's existing fail-open behaviour — when the normalizer returns `unsupported`, the canonical payload is empty but the request still flows.

### Audit Record fields (in-memory) — wired to DB columns in S5

S4 adds Go struct fields to `audit.Record` that capture the passthrough decision for the row:

```go
type Record struct {
    // ... existing fields ...

    // E48: emergency passthrough fan-out. Stamped at Phase 4.5 from
    // *passthrough.Config when AnyBypassActive() is true. Empty when
    // no bypass fired.
    PassthroughFlags  []string  // canonical order: ["bypassHooks","bypassCache","bypassNormalize"]
    PassthroughReason string    // from PassthroughConfig.Reason
}
```

In S4 these fields are populated but the audit Writer's MQ envelope does NOT yet ship them to a `traffic_event` column (S5 adds the columns + wires the persistence). For S4 the fields drive the in-handler branch logic only — once S5 lands, the DB row also carries them.

### Pre-routing-fallback safety

When the handler reaches Phase 4.5 with zero matched targets (the `routeResult.Targets == 0` no-match passthrough path at line ~370), `*ResolvedRequest` is still constructed but `Effective` was called with empty `(providerID, adapterType)` — returns nil — so `Passthrough()` is nil and all bypass branches are no-ops. The handler runs as before.

---

## Story

### S4 — Bypass branches

**User story:** As an operator who has enabled passthrough for a provider in an incident, I want the AI Gateway to actually skip the hooks pipeline / cache lookup / response normalize for matched traffic — degrading the gateway to a dumb pipe for the affected provider while keeping the rest of the fleet on the full compliance path.

**Tasks:**

- **T4.1** — `packages/ai-gateway/internal/observability/audit/audit.go` `Record` struct:
  - Add `PassthroughFlags []string` (canonical-order slice from `passthrough.Config.Flags()`).
  - Add `PassthroughReason string`.

- **T4.2** — `packages/ai-gateway/internal/handler/proxy.go` Phase 4.5:
  - After `resolvedReq := requestcontext.Resolve(...)`, stamp the audit Record:
    ```go
    if pt := resolvedReq.Passthrough(); pt.AnyBypassActive() {
        rec.PassthroughFlags = pt.Flags()
        rec.PassthroughReason = pt.Reason
    }
    ```

- **T4.3** — `packages/ai-gateway/internal/handler/proxy.go` hooks bypass:
  - Wrap the `runRequestHooks` call site (line ~492):
    ```go
    var rewrittenBody []byte
    var reqHookResult *goHooks.CompliancePipelineResult
    if pt := resolvedReq.Passthrough(); !pt.AnyBypassActive() || !pt.BypassHooks {
        rewrittenBody, reqHookResult, rejected = h.runRequestHooks(...)
        if rejected { return }
        if rewrittenBody != nil { body = rewrittenBody }
    }
    ```
  - Same shape around the response-hooks pipeline call site (line ~1740).
  - When skipped, set `rec.HookDecision = "BYPASSED"` so operators can SQL-filter for "request bypassed hooks". (Reuses existing column.)

- **T4.4** — `packages/ai-gateway/internal/handler/proxy.go` cache bypass:
  - `classifyCachePreLookup` extends with a new `audit.CacheStatusPassthroughSkip` constant, returned when `resolvedReq.Passthrough().BypassCache` is true.
  - `audit.CacheStatusPassthroughSkip` declared in `audit/types.go` (or wherever CacheStatus is defined).
  - The downstream switch picks this branch and skips BOTH the lookup and the broker registration.

- **T4.5** — `packages/ai-gateway/internal/observability/audit/audit.go` response-normalize bypass:
  - In `recordToMessage` (line ~820), wrap the `if len(rec.ResponseBody) > 0` block that calls `w.normalize("response", ...)`:
    ```go
    skipResponseNormalize := false
    for _, f := range rec.PassthroughFlags {
        if f == "bypassNormalize" { skipResponseNormalize = true; break }
    }
    if w.normalize != nil && !skipResponseNormalize {
        // existing logic
    }
    ```

- **T4.6** — Unit tests in `packages/ai-gateway/internal/handler/passthrough_bypass_test.go`:
  - Three table-driven cases each verifying:
    - bypassHooks only → hooks pipeline NOT called, request passes through to upstream
    - bypassCache only → cache lookup NOT called, request passes through normally
    - All three flags → all skipped
  - Sentinel: with passthrough off → hooks called, cache lookup happens (no behaviour change vs E48-pre).

- **T4.7** — Build + test:
  - `go build ./packages/ai-gateway/...`
  - `go test -race -count=1 ./packages/ai-gateway/...`

**Acceptance:**

- A request whose primary target has bypassHooks=true skips both the request-stage and response-stage hooks pipelines AND audit row carries `HookDecision = "BYPASSED"` + `PassthroughFlags includes "bypassHooks"`.
- A request whose primary target has bypassCache=true takes the new `audit.CacheStatusPassthroughSkip` branch — no cache lookup, no broker registration.
- A request whose primary target has bypassNormalize=true has `traffic_event_normalized.response_*` columns NULL (skipped by the audit Writer); the row itself still exists.
- A request whose primary target has no passthrough config takes every existing branch unchanged.
- Existing handler unit tests continue to pass (no regression).

**Validation:**

```bash
go test -race -count=1 -run "TestPassthroughBypass" ./packages/ai-gateway/internal/handler/...
```
