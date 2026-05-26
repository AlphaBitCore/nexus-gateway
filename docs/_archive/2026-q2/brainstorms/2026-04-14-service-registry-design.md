# Service Registry & Health Monitoring

**Date:** 2026-04-14  
**Status:** Draft  
**Scope:** Service instance registration, heartbeat-based health monitoring, and Status page integration for multi-instance data plane services (ai-gateway, compliance-proxy).

---

## Context & Problem

The Status page (`/status`) currently only shows control-plane's own DB and Redis status. There is no visibility into data plane services (ai-gateway, compliance-proxy), which:

- May run as multiple instances behind load balancers
- May be in different network segments (control-plane cannot always reach them)
- Already connect to control-plane for config reads and audit writes
- Already have `/healthz` endpoints but nobody consumes them centrally

The user needs a unified view of all running service instances, their health, and their versions.

---

## Design

### Architecture: Push-Based Heartbeat Registration

Each service instance pushes its state to control-plane. This is chosen over pull-based probing because:

1. **Network reachability** — Instances can always reach control-plane (they already do for config/audit). Control-plane cannot always reach instances (NAT, LB, firewall).
2. **Simplicity** — One direction of communication, no service discovery needed.
3. **Crash detection** — Missing heartbeats naturally surface dead instances.
4. **Rich health data** — Heartbeat payload carries component-level health, equivalent to what pull would retrieve.

### Instance Lifecycle

```
Start   → POST /api/internal/registry/register     (initial registration)
Every 15s → POST /api/internal/registry/heartbeat   (health + metrics)
Shutdown  → POST /api/internal/registry/deregister  (graceful removal)
Crash     → control-plane marks unhealthy after 45s (3 missed heartbeats)
```

### Internal API Endpoints

These are on a separate `/api/internal/` prefix — not admin-authenticated, but protected by a shared secret (service token). Admin UI never calls these directly.

#### POST /api/internal/registry/register

Called once at startup.

```json
{
  "instanceId": "ai-gw-abc123",
  "service": "ai-gateway",
  "version": "0.1.0",
  "address": "10.0.1.5:3050"
}
```

- `instanceId` — Unique per process. Generated at startup (hostname + PID or UUID).
- `service` — One of: `ai-gateway`, `compliance-proxy`, `agent` (extensible).
- `version` — Build version string.
- `address` — `host:port` for debugging/display. Not used for probing.

Response: `201 Created` with `{ "instanceId": "...", "heartbeatIntervalSec": 15 }`.

If an instance with the same `instanceId` already exists (e.g., restart before TTL), the registration is treated as an upsert.

#### POST /api/internal/registry/heartbeat

Called every 15 seconds.

```json
{
  "instanceId": "ai-gw-abc123",
  "status": "healthy",
  "uptime": 3600,
  "checks": {
    "redis": "connected",
    "configLoaded": true,
    "database": "ok"
  }
}
```

- `status` — `healthy`, `degraded`, or `unhealthy` (self-assessed by the instance).
- `checks` — Free-form key/value map of sub-component health. Each service reports what's relevant to it.
- `uptime` — Seconds since process start.

Response: `200 OK` with `{ "ack": true }`.

If `instanceId` is not registered, returns `404` — the instance should re-register.

#### POST /api/internal/registry/deregister

Called during graceful shutdown.

```json
{
  "instanceId": "ai-gw-abc123"
}
```

Response: `200 OK`. Row is deleted from DB.

### Authentication for Internal API

The `/api/internal/` endpoints use a shared service token, not the admin session/API-key auth. The token is configured via environment variable `INTERNAL_SERVICE_TOKEN` on both control-plane and data plane services. If not set, the endpoints are unauthenticated (dev mode).

Middleware checks `Authorization: Bearer <token>` header.

### Data Model

#### New table: `service_instance`

```
service_instance
  instance_id        String PK (not UUID — provided by the service)
  service            String (ai-gateway | compliance-proxy | agent)
  version            String
  address            String?
  status             String (healthy | degraded | unhealthy | offline)
  uptime             Int? (seconds)
  checks             JSONB? (component health map)
  registered_at      DateTime
  last_heartbeat_at  DateTime
```

No auto-generated UUID — the instance provides its own ID.

**Index:** `(service, status)` for efficient Status page queries.

### Staleness Detection

Control-plane runs a lightweight periodic task (goroutine, every 30 seconds) that:

1. Queries `service_instance WHERE last_heartbeat_at < NOW() - INTERVAL '45 seconds' AND status != 'offline'`
2. Updates matched rows to `status = 'offline'`
3. Logs a warning for each transition to offline

45 seconds = 3 missed heartbeats at 15-second intervals. This provides:
- Fast enough detection (~1 minute worst case)
- Tolerant of occasional network hiccups (1-2 missed heartbeats don't trigger)

### Admin Query Endpoint

#### GET /api/admin/instances

Replaces the placeholder we just implemented. Now returns all registered instances from the DB.

```json
{
  "instances": [
    {
      "instanceId": "ai-gw-abc123",
      "service": "ai-gateway",
      "version": "0.1.0",
      "address": "10.0.1.5:3050",
      "status": "healthy",
      "uptime": 3600,
      "checks": { "redis": "connected", "configLoaded": true },
      "registeredAt": "2026-04-14T10:00:00Z",
      "lastHeartbeatAt": "2026-04-14T11:00:00Z"
    },
    {
      "instanceId": "proxy-xyz789",
      "service": "compliance-proxy",
      "version": "0.1.0",
      "address": "10.0.1.6:3040",
      "status": "offline",
      "uptime": null,
      "checks": null,
      "registeredAt": "2026-04-14T09:00:00Z",
      "lastHeartbeatAt": "2026-04-14T10:30:00Z"
    }
  ],
  "count": 2,
  "services": {
    "ai-gateway": { "total": 1, "healthy": 1, "degraded": 0, "unhealthy": 0, "offline": 0 },
    "compliance-proxy": { "total": 1, "healthy": 0, "degraded": 0, "unhealthy": 0, "offline": 1 }
  }
}
```

The `services` summary gives the Status page a quick per-service health overview.

### Shared Heartbeat Client — `packages/shared/heartbeat/`

A reusable Go package that any service imports to participate in the registry.

```go
// In ai-gateway/main.go — 3 lines to integrate
reg := heartbeat.NewClient(heartbeat.Config{
    ControlPlaneURL: cfg.ControlPlaneURL,
    ServiceName:     "ai-gateway",
    ServiceVersion:  version,
    ListenAddress:   listenAddr,
    Token:           cfg.InternalServiceToken,
    Logger:          logger,
})
go reg.Start(ctx)  // register + heartbeat loop; deregisters on ctx.Done()
```

**Client behavior:**

1. On `Start()`:
   - Generate `instanceId` as `{hostname}-{pid}-{random4chars}` (human-readable + unique).
   - POST register. Retry with backoff on failure (control-plane may not be up yet).
   - Start heartbeat ticker (15s).
   - Each heartbeat calls a `HealthChecker` interface to collect sub-component status.

2. On each heartbeat tick:
   - Collect health checks from registered checkers.
   - POST heartbeat.
   - If 404 (instance not found), re-register automatically.

3. On `ctx.Done()` (shutdown signal):
   - POST deregister.
   - Best-effort with 2s timeout — if it fails, staleness detection handles it.

**HealthChecker interface:**

```go
// HealthChecker reports the health of a sub-component.
type HealthChecker interface {
    Name() string
    Check(ctx context.Context) (status string, ok bool)
}
```

Services register their own checkers:

```go
reg.AddChecker(heartbeat.CheckerFunc("redis", func(ctx context.Context) (string, bool) {
    if err := redisClient.Ping(ctx).Err(); err != nil {
        return "disconnected", false
    }
    return "connected", true
}))
```

### Status Page UI Changes

The Overview tab currently shows: Uptime, Version, Instances (count), Node, LogLevel, Maintenance, Infrastructure (DB/Redis).

**Add a "Services" section** below Infrastructure in the Overview tab, showing per-service health:

```
┌─────────────────────────────────────────────────────────────┐
│ Services                                                     │
│                                                              │
│ ● ai-gateway          2 instances   2 healthy   v0.1.0       │
│ ● compliance-proxy    1 instance    1 healthy   v0.1.0       │
│ ○ agent               0 instances                            │
│                                                              │
│ [View all instances]                                         │
└─────────────────────────────────────────────────────────────┘
```

**Clicking "View all instances"** or expanding shows the full instance table:

| Instance ID | Service | Version | Address | Status | Uptime | Last Heartbeat | Checks |
|-------------|---------|---------|---------|--------|--------|----------------|--------|
| ai-gw-abc123 | ai-gateway | 0.1.0 | 10.0.1.5:3050 | healthy | 1h | 5s ago | redis: ok, config: ok |
| ai-gw-def456 | ai-gateway | 0.1.0 | 10.0.1.6:3050 | healthy | 45m | 12s ago | redis: ok, config: ok |
| proxy-xyz789 | compliance-proxy | 0.1.0 | 10.0.1.7:3040 | offline | — | 5m ago | — |

Status dot colors:
- Green: `healthy`
- Yellow: `degraded`
- Red: `unhealthy`
- Gray: `offline`

**Instance count card** in the stats row updates to show total across all services (including control-plane itself).

---

## Files Affected

### Database
- `tools/db-migrate/schema.prisma` — Add `ServiceInstance` model
- New Prisma migration

### Shared Library (new)
- `packages/shared/heartbeat/client.go` — Heartbeat client (Start/Stop/AddChecker)
- `packages/shared/heartbeat/checker.go` — HealthChecker interface + CheckerFunc helper

### Control-Plane — Backend
- `packages/control-plane/internal/handler/admin_extras.go` — Rewrite `ListInstances` to query DB; add internal registry endpoints
- `packages/control-plane/internal/handler/internal_registry.go` (new) — Register/Heartbeat/Deregister handlers
- `packages/control-plane/internal/handler/internal_routes.go` (new) — Route registration for `/api/internal/`
- `packages/control-plane/internal/store/service_instance.go` (new) — CRUD for service_instance table
- `packages/control-plane/internal/registry/staleness.go` (new) — Periodic staleness checker goroutine
- `packages/control-plane/cmd/control-plane/main.go` — Start staleness checker; register internal routes; register control-plane itself as an instance

### Data Plane — Integration
- `packages/ai-gateway/cmd/ai-gateway/main.go` — Add heartbeat client startup (3 lines + health checkers)
- `packages/compliance-proxy/cmd/compliance-proxy/main.go` — Add heartbeat client startup (3 lines + health checkers)

### Frontend
- `packages/control-plane-ui/src/api/services/system.ts` — Update `listInstances` response types
- `packages/control-plane-ui/src/pages/status/StatusPage.tsx` — Add Services section with per-service summary and instance detail table

---

## Out of Scope

- **Agent registration** — The desktop agent has a different lifecycle (managed by fleet management). It can integrate with the heartbeat client later if needed.
- **Historical instance data** — No retention of deregistered instances. Once gone, the row is deleted.
- **Alerting on instance status changes** — Could be added later by watching the `service_instance` table.
- **Instance-to-instance communication** — The registry is for visibility only, not for service mesh routing.
- **mTLS or certificate-based auth** — Internal API uses a shared token for simplicity.
