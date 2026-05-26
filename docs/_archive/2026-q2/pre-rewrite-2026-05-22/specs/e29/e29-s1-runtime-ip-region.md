# E29 S1 — Runtime SourceIP + ProviderRegion Injection

## Story

As a compliance operator I need the ai-gateway request pipeline to
inject authoritative `SourceIP` and `ProviderRegion` into hook
inputs so that `ip-access-filter` and `data-residency` evaluate the
same data the rest of the platform sees.

## Scope

- `packages/ai-gateway/internal/handler/proxy.go`
- `packages/ai-gateway/internal/middleware/connection_stage.go`
  (export the shared `ClientIP` helper)
- `packages/ai-gateway/internal/router/types.go` and
  `packages/ai-gateway/internal/router/resolver.go`
- `packages/ai-gateway/internal/store/provider.go`
- `packages/ai-gateway/internal/streaming/live.go`
- `packages/control-plane/internal/store/provider.go`
- `packages/control-plane/internal/handler/admin_providers.go`
- `tools/db-migrate/schema.prisma`
- `tools/db-migrate/migrations/20260424000000_provider_region/migration.sql`

## Tasks

1. Add `region TEXT NULL` column to the `Provider` table (Prisma
   schema + raw SQL migration).
2. Plumb `Region` as a `*string` through the ai-gateway and
   control-plane provider stores. The ai-gateway reads the value
   and the control-plane admin API writes it.
3. Extend `router.RoutingTarget` with `Region string` and populate
   it in `Resolver.lookupTarget`.
4. Export a single `middleware.ClientIP(r)` helper with the existing
   `X-Forwarded-For (first hop)` → `X-Real-IP` → `RemoteAddr`
   precedence.
5. In `proxy.ServeProxy`, populate `audit.Record.SourceIP` with
   `middleware.ClientIP(r)` instead of the raw `RemoteAddr`.
6. In `proxy.runRequestHooks`, accept the primary
   `router.RoutingTarget` and set `HookInput.SourceIP` and
   `HookInput.ProviderRegion` before executing the pipeline.
7. In `proxy.handleNonStream`, set the same two fields on the
   response-stage `HookInput`.
8. In `proxy.handleStream`, extend `streaming.StreamHookContext`
   with `SourceIP` and `ProviderRegion`; apply both at every
   streaming checkpoint.
9. Update `admin_providers` API to accept `region` on create and to
   distinguish missing / string / explicit null on update.

## Acceptance criteria

- `TestRunRequestHooks_PopulatesSourceIPAndProviderRegion` and
  `TestRunRequestHooks_PrefersXForwardedFor` pass; both assert that
  a capturing hook sees `SourceIP` and `ProviderRegion` set by the
  pipeline.
- Prisma migration applies cleanly; `Provider.region` is nullable
  and defaults to `NULL`.
- Existing provider admin API regressions stay green.
- Streaming live pipeline emits the same `SourceIP` + `Region` on
  every response-stage checkpoint.
