# E42-S5 — ai-gateway Hot-Swap Completion (SDD)

## User story

As an SRE I extend the AI Gateway's upstream timeout from 30 s to 60 s
during a slow-provider incident, and the change takes effect on the
NEXT outbound request without bouncing the gateway. Same for narrowing
the forward-header allowlist when compliance flags a new sensitive
header — no restart needed.

## Background

Two ai-gateway runtime tunables are partially shadow-ready but stop
short of a true hot-swap:

1. **`upstream_timeouts`** — `packages/ai-gateway/internal/providers/specutil/http.go`
   already uses `atomic.Pointer[HTTPConfig]` (the `activeConfig`
   global) with a `Configure()` API that swaps the pointer. New
   provider adapters built AFTER a swap read the new config; existing
   adapters that captured the config at construction continue using the
   old values. Per the audit, the gap is per-request reads on the hot
   path.
2. **`forward_headers_config`** — defined as `*forwardheader.Config`
   (already pointer-shaped) but plumbed through provider adapters at
   construction. Adapters cache the allowlist and don't re-read it.

This story finishes both, adds the shadow `OnConfigChanged` cases, and
seeds the template registry.

## Tasks

### 1. `upstream_timeouts` — per-request `ActiveConfig()`

- Audit every call site that holds `*HTTPConfig` from
  `specutil.ActiveConfig()` and confirm the value is read per-request,
  not cached on the adapter struct.
- If any adapter caches it, replace the cache with a
  `specutil.ActiveConfig()` call in the request method (the global is
  itself an atomic.Pointer so the hot-path cost is one atomic load).
- Add `case "upstream_timeouts":` in
  `cmd/ai-gateway/main.go`'s `OnConfigChanged` that decodes the JSON
  into an `*HTTPConfig` and calls `specutil.Configure(cfg)`.

### 2. `forward_headers_config` — atomic.Pointer

- Wrap the runtime config in `packages/ai-gateway/internal/execution/forwardheader/`
  with a `Live` struct holding `atomic.Pointer[Config]`. Adapters keep
  reading `live.Load()` per request so the swap is uniform with
  upstream_timeouts.
- Replace the existing `*Config` injection with the `*Live` injection
  at the adapter constructor sites.
- Add `case "forward_headers_config":` in `OnConfigChanged` that
  decodes JSON into `*Config` and calls `live.Swap(cfg)`.

### 3. Migration — 2 new template rows

- ai-gateway / upstream_timeouts: default state matches the YAML
  defaults (`{"timeoutSec": 60, "dialTimeoutSec": 10, "tlsHandshakeTimeoutSec": 10, ...}`),
  so the first apply is a no-op semantically.
- ai-gateway / forward_headers_config: default state is the empty
  passthrough (`{"request": {"mode": "allowlist", "headers": []}, "response": {"mode": "allowlist", "headers": []}}`).
- Backfill existing things' desired in the same migration following
  the 20260514000000 pattern.

### 4. Tests

- Unit test for `specutil.Configure(cfg)` already exists; extend to
  confirm `ActiveConfig()` returns the new pointer after Configure.
- Unit test for `forwardheader.Live`: Swap then Load returns the new
  pointer; concurrent Load is race-free.

## Non-tasks (explicitly out of scope)

- `cache_config`, `cors_config`, `http_client_timeouts.webhook`,
  `http_client_timeouts.external` — these require rebuilding the cache
  layer, the middleware chain, or the http.Client respectively and
  belong to a future epic with its own architecture work.

## Acceptance criteria

- [ ] `go test ./packages/ai-gateway/internal/providers/specutil/...
      ./packages/ai-gateway/internal/execution/forwardheader/... -race -count=1`
      passes.
- [ ] Setting an override on `<ai-gateway-id>/upstream_timeouts` with
      `timeoutSec=90` causes a subsequent outbound provider request to
      use the 90 s timeout (observable in the request log /
      `Transport` properties).
- [ ] Setting an override on `<ai-gateway-id>/forward_headers_config`
      causes the next request to apply the new allowlist (header that
      WAS forwarded before is now stripped, or vice versa).
- [ ] `SELECT config_key FROM thing_config_template WHERE type='ai-gateway' AND config_key IN ('upstream_timeouts','forward_headers_config');`
      returns 2 rows.
