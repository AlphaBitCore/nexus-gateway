# E48 S3 — Runtime Cache + Hub config applier + Phase 4.5 attach

**Epic:** E48
**Requirements:** [e48-emergency-passthrough.md](../../../../docs/developers/specs/e48/e48-emergency-passthrough.md) — Must M4, M6 (runtime half)
**OpenAPI:** none
**Status:** Approved (user-approved plan 2026-05-13)
**Date:** 2026-05-13
**Builds on:** [e48-s2-resolved-request.md](e48-s2-resolved-request.md)

---

## Architecture summary

S3 lights up the runtime path that S1's schema + S2's types prepared. Three concrete changes:

1. **`passthrough.Cache`** — `atomic.Pointer[Snapshot]` holding the merged 3-tier blob; `Effective(providerID, adapterType) *Config` performs lock-free in-memory merge per request.
2. **Hub config applier** — register a callback on `gateway_passthrough_config` shadow key; on each push, parse the blob and call `cache.SetSnapshot(blob)`.
3. **Phase 4.5 attach in `handler/proxy.go`** — after routing resolves the primary target, build a `*ResolvedRequest` via `requestcontext.Resolve(rctxFull, routeResult, passthroughCfg)` and stash it on `r.Context()` via a typed context key for downstream consumers in S4-S5.

S3 ships the runtime plumbing but **no bypass branches fire yet** — the cache is empty (no production rows exist), and even if it weren't, S4 is what reads `resolved.Passthrough().BypassXxx` to actually skip layers. This staging keeps S3 reviewable: type-level changes + one new code path (config-applier branch + Phase 4.5 attach), no behavioural impact on existing traffic.

### `Snapshot` JSON shape

Mirrors the thing_config_template seed structure (S1):

```json
{
  "global": { "enabled": false, "bypassHooks": false, "bypassCache": false, "bypassNormalize": false, "expiresAt": null, "enabledBy": null, "reason": null },
  "adapters": { "anthropic": { "enabled": true, "bypassHooks": true, "expiresAt": "2026-05-13T14:00:00Z", "enabledBy": "user-uuid", "reason": "..." } },
  "providers": { "<provider-uuid>": { "enabled": true, "bypassNormalize": true, "bypassCache": true, "expiresAt": "...", "enabledBy": "...", "reason": "..." } }
}
```

Each tier entry parses into a `TierEntry` struct in the passthrough package. `Snapshot.Effective(providerID, adapterType)` walks the three tiers in inheritance order, OR's the bypass flags, and picks the tightest expiry. The expiry check filters out expired tiers at lookup time (defence-in-depth against a stale snapshot).

### `Cache` type

```go
package passthrough

type Cache struct {
    ptr atomic.Pointer[Snapshot]
}

func NewCache() *Cache  // returns empty snapshot — fail-closed cold-start

func (c *Cache) SetSnapshot(s *Snapshot)
func (c *Cache) Effective(providerID, adapterType string) *Config
```

`Effective` returns nil when no tier is active for the (provider, adapter) pair. The downstream `*Config` methods (`AnyBypassActive`, `Flags`) are already nil-safe (S2), so callers don't have to nil-check before using them.

### Phase 4.5 attach

```go
// proxy.go after routing resolves the primary target:
target := routeResult.Targets[0]
passthroughCfg := h.deps.PassthroughCache.Effective(target.ProviderID, target.AdapterType)
resolved := requestcontext.Resolve(rctxFull, routeResult, passthroughCfg)
r = r.WithContext(requestcontext.WithResolved(r.Context(), resolved))
```

Stashing on `r.Context()` lets downstream code retrieve via `requestcontext.ResolvedFrom(ctx)` without changing every function signature. In S4/S5 the hooks pipeline + audit Writer + executor each call `ResolvedFrom(ctx)` at their entry to read passthrough state.

`requestcontext.WithResolved` + `ResolvedFrom` live in resolved.go (the same file as the type). Context keys are unexported to prevent stringly-typed collisions.

---

## Story

### S3 — Runtime Cache + Hub wiring + Phase 4.5 attach

**User story:** As a Nexus platform engineer, after S1's schema + S2's types ship, I want the AI Gateway to actually read `gateway_passthrough_config` from Hub config push + resolve the effective config per request so that subsequent stories (S4 bypass branches, S5 audit fields) have a non-nil `*ResolvedRequest` to consume.

**Tasks:**

- **T3.1** — `packages/ai-gateway/internal/execution/passthrough/cache.go`:
  - `TierEntry` struct mirroring DB JSONB row shape.
  - `Snapshot` struct with `Global TierEntry`, `Adapters map[string]TierEntry`, `Providers map[string]TierEntry`. JSON tags match the seed shape.
  - `Snapshot.Effective(providerID, adapterType string) *Config` — 3-tier merge with expiry filter.
  - `Cache` struct + `NewCache()` + `SetSnapshot()` + `Effective(providerID, adapterType) *Config`.
  - All methods nil-safe on the cache (nil cache returns nil config).

- **T3.2** — `packages/ai-gateway/internal/execution/passthrough/cache_test.go`:
  - Cold-start (empty snapshot): every lookup returns nil.
  - Single-tier active: global only / adapter only / provider only → correct merged Config.
  - Inheritance: provider > adapter > global override.
  - Expiry: tier active in DB but past expires_at → filtered out at Effective time.
  - SetSnapshot atomic swap: read after Set sees new snapshot.

- **T3.3** — `packages/ai-gateway/internal/pipeline/requestcontext/resolved.go` additions:
  - Unexported context key type + `WithResolved(ctx, *ResolvedRequest) context.Context` + `ResolvedFrom(ctx) *ResolvedRequest`. The latter returns nil when no ResolvedRequest is on context.
  - Unit tests for the round-trip + nil cases.

- **T3.4** — `packages/ai-gateway/internal/handler/proxy.go` modifications:
  - Add `PassthroughCache *passthrough.Cache` to `Deps`.
  - In `handleProxy`, immediately after `routeResult` is obtained (the existing `if len(routeResult.Targets) == 0` block ends) and the primary target is in scope:
    ```go
    target := routeResult.Targets[0]
    passthroughCfg := h.deps.PassthroughCache.Effective(target.ProviderID, target.AdapterType)
    resolved := requestcontext.Resolve(rctxFull, routeResult, passthroughCfg)
    r = r.WithContext(requestcontext.WithResolved(r.Context(), resolved))
    ```
  - No other behavioural change.

- **T3.5** — `packages/ai-gateway/cmd/ai-gateway/main.go`:
  - Construct `passthroughCache := passthrough.NewCache()` at startup.
  - Add `case "gateway_passthrough_config"` branch in the OnConfigChanged switch (next to `case "cache_config"`): parse the state JSON into `passthrough.Snapshot`, call `passthroughCache.SetSnapshot(&snap)`.
  - Wire `PassthroughCache` into `handler.Deps`.

- **T3.6** — Build + test:
  - `go build ./packages/ai-gateway/...`
  - `go test -race -count=1 ./packages/ai-gateway/internal/execution/passthrough/... ./packages/ai-gateway/internal/pipeline/requestcontext/... ./packages/ai-gateway/internal/handler/...`

**Acceptance:**

- AI Gateway boots with empty passthrough cache → `Effective` returns nil → handler's `requestcontext.Resolve(rctxFull, routeResult, nil)` produces a non-nil `*ResolvedRequest` with `Passthrough() == nil`.
- Hub pushes `gateway_passthrough_config` shadow → applier callback parses + replaces snapshot. Next request's `Effective` returns the new merged Config.
- `ResolvedFrom(ctx)` round-trips through `WithResolved`.
- No bypass branches fire yet (S4) — handler downstream code paths are unchanged, all existing tests continue to pass.

**Validation script:**

```bash
go build ./packages/ai-gateway/...
go test -race -count=1 ./packages/ai-gateway/internal/execution/passthrough/... ./packages/ai-gateway/internal/pipeline/requestcontext/...

# Smoke (manual, against local stack with passthrough cache empty):
curl -H "Authorization: Bearer $VK" -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \
  http://localhost:3050/v1/chat/completions   # expect HTTP 200 unchanged behaviour
```
