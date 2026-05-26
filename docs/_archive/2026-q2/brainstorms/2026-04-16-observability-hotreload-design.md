# Observability Config Hot-Reload Design

**Date:** 2026-04-16  
**Status:** Approved  
**Scope:** All 4 Go services + frontend

## Problem

The Observability settings page only allows editing `traceViewerUrl`. The `otelEnabled` and `samplingRate` fields are read-only, and OTEL is initialized once at startup with no runtime reconfiguration. Changes to these values require a service restart to take effect.

## Goals

1. Make `otelEnabled` and `samplingRate` editable from the admin UI
2. Changes take effect at runtime without service restart
3. All 4 services support OTEL initialization + hot-reload: Control Plane, AI Gateway, Compliance Proxy, Agent
4. `otelEndpoint` and `otelServiceName` remain read-only (deployment-time config)

## Architecture

```
Admin UI → PUT /api/admin/settings/observability
         → DB (system_metadata key "observability.config")
         → Redis pub/sub channel "nexus:config:shared" topic "observability"

Control Plane / AI Gateway / Compliance Proxy:
  Redis subscriber → read DB → SwappableTracerProvider.Reconfigure()

Agent:
  configsync poll → detect otel config change → SwappableTracerProvider.Reconfigure()
```

## Component Design

### 1. Shared Telemetry Package — `packages/shared/runtime/telemetry/`

New package providing a swappable OTEL tracer provider.

**Types:**

```go
type Config struct {
    Enabled     bool
    Endpoint    string
    ServiceName string
    SamplingRate float64
}

type SwappableTracerProvider struct {
    provider atomic.Pointer[sdktrace.TracerProvider]
    mu       sync.Mutex // serializes Reconfigure calls
    logger   *slog.Logger
}
```

**Functions:**

- `Init(cfg Config, logger *slog.Logger) (*SwappableTracerProvider, error)` — Creates initial provider, registers as global via `otel.SetTracerProvider()`.
- `(*SwappableTracerProvider).Reconfigure(cfg Config) error` — Acquires mutex, creates new provider with updated config, atomically swaps pointer, gracefully shuts down old provider (5s timeout). If `Enabled=false`, swaps in a no-op provider.
- `(*SwappableTracerProvider).Shutdown(ctx context.Context) error` — Clean shutdown for process exit.
- `(*SwappableTracerProvider).TracerProvider() trace.TracerProvider` — Returns current underlying provider (implements `trace.TracerProvider` interface by delegation).

**Design decisions:**
- `atomic.Pointer` for lock-free reads on the hot path (every span creation)
- `sync.Mutex` only for Reconfigure (rare, serialized)
- Old provider shutdown in a goroutine with 5s timeout to avoid blocking
- Minimal dependencies: only `go.opentelemetry.io/otel` SDK + `log/slog`

### 2. Control Plane — Handler Changes

**File:** `packages/control-plane/internal/handler/admin_extras.go`

Update `PUT /api/admin/settings/observability`:
- Accept `{ otelEnabled: bool, samplingRate: number, traceViewerUrl: string }`
- Validate `samplingRate` is in range [0, 1]
- Write to `system_metadata` with key `"observability.config"`
- Publish invalidation: `h.PubSub.PublishInvalidation(ctx, "observability")`

**OTEL self-initialization:**
- Control Plane `main.go` reads `system_metadata["observability.config"]` at startup → `telemetry.Init()`
- Subscribes to Redis `"observability"` topic → reads DB → `telemetry.Reconfigure()`

### 3. AI Gateway — Add OTEL

**File:** `packages/ai-gateway/cmd/ai-gateway/main.go`

- Startup: read `system_metadata["observability.config"]` → `telemetry.Init()`
- Add `"observability"` case to existing `subscribeConfigInvalidation()` switch
- On invalidation: read DB → `telemetry.Reconfigure()`

### 4. Compliance Proxy — Add OTEL

**File:** `packages/compliance-proxy/cmd/compliance-proxy/main.go`

- Startup: read `system_metadata["observability.config"]` → `telemetry.Init()`
- Add `"observability"` handler to existing config cache subscriber
- On invalidation: read DB → `telemetry.Reconfigure()`

### 5. Agent — Migrate to Shared Package

**Current:** `packages/agent/core/observability/telemetry/telemetry.go` — one-time init

**Change:**
- Replace with `packages/shared/runtime/telemetry` import
- In `configsync`: detect otel config changes (compare old vs new) → call `Reconfigure()`
- Remove `packages/agent/core/observability/telemetry/` (or keep as thin wrapper if needed)

### 6. Frontend — Observability Tab

**File:** `packages/control-plane-ui/src/pages/settings/SettingsObservabilityTab.tsx`

- `otelEnabled` → `Switch` component
- `samplingRate` → `Input` (type=number, step=0.01, min=0, max=1) wrapped in `FormField`
- `otelEndpoint` / `otelServiceName` → read-only display (unchanged)
- Update `updateObservabilityConfig` API call to include `otelEnabled` + `samplingRate`

**File:** `packages/control-plane-ui/src/api/services/system.ts`

- Update `updateObservabilityConfig` input type to `{ otelEnabled: boolean; samplingRate: number; traceViewerUrl: string }`

## Constraints

- `otelEndpoint` and `otelServiceName` are read-only — set via environment variables at deployment
- Old provider shutdown timeout: 5s
- Agent hot-reload latency: up to one poll interval (typically 30-60s)
- `shared/telemetry` dependencies: minimal vetted set only (OTEL SDK + slog)
- No new `replace` directives in `go.mod`

## Testing

- Unit test for `SwappableTracerProvider`: Init → Reconfigure → verify new sampler is active → Shutdown
- Unit test for Reconfigure with `Enabled=false` → verify no-op provider
- Integration: verify Redis invalidation triggers Reconfigure in subscriber
