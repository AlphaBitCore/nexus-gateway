# Scheduler Single-Instance Guard + Registry-Based Discovery

**Date:** 2026-04-16

## Problem Statement

The control-plane Scheduler runs all background jobs (rollup, quota alerts, VK expiry, SIEM bridge, etc.) without any single-instance guard. In a multi-instance deployment, every instance runs the full job set concurrently, causing duplicate processing, duplicate alerts, and potential data corruption (e.g. rollup double-counting).

Additionally, when a user triggers a job from the Dashboard (via `POST /api/admin/rollup-jobs/:name/trigger`), the request hits whichever instance the load balancer picks — which may not be the one running the scheduler.

## Design

### 1. Configuration

Add a `scheduler` section to the control-plane config:

```yaml
scheduler:
  enabled: false  # default off; set true on exactly one instance
```

**Config struct addition** (`internal/config/config.go`):

```go
type SchedulerConfig struct {
    Enabled bool `yaml:"enabled"`
}
```

Add `Scheduler SchedulerConfig` to the top-level `Config` struct.

**Environment variable override:** `SCHEDULER_ENABLED=true|false`

### 2. ServiceInstance Role Field

Add a `role` column to the `ServiceInstance` table to distinguish instance types.

**Prisma schema change:**

```prisma
model ServiceInstance {
  // ... existing fields ...
  role  String  @default("api")  // "api" | "scheduler"
}
```

**Migration:** `ALTER TABLE "ServiceInstance" ADD COLUMN "role" TEXT NOT NULL DEFAULT 'api'`

**Go struct update** (`internal/store/service_instance.go`):

```go
type ServiceInstance struct {
    // ... existing fields ...
    Role string // "api" or "scheduler"
}
```

**Registration behavior:**

- When `cfg.Scheduler.Enabled == true`: register with `role = "scheduler"`
- Otherwise: register with `role = "api"` (the default)
- Heartbeat updates also carry the role to keep it current

### 3. Scheduler Startup Guard

In `main.go`, conditionally create and start the scheduler:

```go
if cfg.Scheduler.Enabled {
    jobScheduler = jobs.New(pool, logger, retentionCfg, ...)
    jobScheduler.Start()
    adminHandler.Scheduler = jobScheduler
    logger.Info("scheduler enabled on this instance")
} else {
    logger.Info("scheduler disabled — running as API-only instance")
    // adminHandler.Scheduler remains nil
}
```

When `Scheduler == nil`, existing `ListRollupJobs` / `TriggerRollupJob` handlers already return `503 SCHEDULER_UNAVAILABLE`. This behavior is replaced with proxy forwarding (see below).

### 4. BFF Proxy Forwarding (Session Passthrough)

When a non-scheduler instance receives a job management request, it discovers the scheduler instance via the ServiceInstance table and transparently proxies the request.

**New store method:**

```go
func (db *DB) FindSchedulerInstance(ctx context.Context) (*ServiceInstance, error)
// SELECT * FROM "ServiceInstance"
// WHERE service = 'control-plane' AND role = 'scheduler' AND status = 'healthy'
// ORDER BY "lastHeartbeatAt" DESC LIMIT 1
```

**Handler logic change in `admin_extras.go`:**

Both `ListRollupJobs` and `TriggerRollupJob` are updated:

```
if h.Scheduler != nil {
    // local execution (existing logic)
} else {
    // proxy to scheduler instance
    schedulerInstance := db.FindSchedulerInstance(ctx)
    if schedulerInstance == nil || schedulerInstance.Address == nil {
        return 503 "No scheduler instance available"
    }
    proxyURL = http://{schedulerInstance.Address}/api/admin/rollup-jobs/...
    forward request with original headers (including Cookie)
    return proxied response
}
```

**Session passthrough:**

- The original request's `Cookie` header is forwarded to the scheduler instance
- The scheduler instance validates the session against the same shared database
- The original user identity is preserved for audit logging on the scheduler side
- CSRF token is also forwarded (same header passthrough)

**Proxy implementation:** Use `net/http` directly (not Echo proxy middleware) for simplicity — construct a new `http.Request` with the target URL, copy relevant headers (Cookie, Content-Type, Accept, X-CSRF-Token), execute, and pipe the response back.

### 5. Error Handling

| Scenario | Behavior |
|----------|----------|
| No scheduler instance registered | 503 `{"error": "No scheduler instance available"}` |
| Scheduler instance unhealthy | 503 (same — query filters by `status='healthy'`) |
| Scheduler instance unreachable | 502 `{"error": "Scheduler instance unreachable"}` with upstream error detail |
| Proxy timeout | 504 after 10s timeout |
| Scheduler returns error | Passthrough the error response as-is |

### 6. Files Changed

| File | Change |
|------|--------|
| `internal/config/config.go` | Add `SchedulerConfig` struct, add to `Config`, env override `SCHEDULER_ENABLED` |
| `cmd/control-plane/main.go` | Conditional scheduler start, pass role to service registration |
| `tools/db-migrate/schema.prisma` | `ServiceInstance` add `role String @default("api")` |
| New migration `20260416130000_service_instance_role` | `ALTER TABLE "ServiceInstance" ADD COLUMN "role" TEXT NOT NULL DEFAULT 'api'` |
| `internal/store/service_instance.go` | Add `Role` to struct, update scan/register/heartbeat, add `FindSchedulerInstance` |
| `internal/handler/admin_extras.go` | `ListRollupJobs` / `TriggerRollupJob` — add proxy forwarding when `Scheduler == nil` |

### 7. Deployment

To enable the scheduler on exactly one instance:

```yaml
# Instance A (scheduler)
scheduler:
  enabled: true

# Instance B, C, ... (API-only)
scheduler:
  enabled: false  # or omit (default)
```

Or via environment variable:
```bash
# Instance A
SCHEDULER_ENABLED=true

# Instance B, C, ...
# (don't set, defaults to false)
```

### Out of Scope

- Automatic leader election / failover (PG advisory lock, Redis lock) — can be added later if needed
- Job-level granularity (e.g. some jobs on instance A, others on instance B) — all-or-nothing for now
- UI changes — the Dashboard already calls the same API endpoints; proxy is transparent
