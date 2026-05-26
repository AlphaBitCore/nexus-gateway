# Service Call Framework + Kill Switch Multi-Instance Architecture

**Date:** 2026-04-16
**Status:** Draft (v2 — generalized from kill-switch-only to framework)
**Author:** nexus@alphabitcore.com + Claude

## Problem

1. **BFF routes to a single hardcoded compliance-proxy URL** — no multi-instance awareness.
2. **No verification** — BFF cannot confirm whether all instances applied a config change.
3. **No generic "call service instances" facility** — every handler that needs to reach ai-gateway or compliance-proxy re-invents discovery and HTTP calls.
4. **Kill-switch-specific**: pubsub unreliable, audit in wrong table, operator identity lost.

## Design Principles

- **Generic framework first, kill switch as first consumer** — the multi-instance call pattern will be reused for domain allowlist, hook config, route rules, provider enable/disable, observability config, rate limits.
- **BFF is the single state-change entry point** — data-plane instances only receive instructions and report status.
- **Verify, don't trust** — BFF calls all instances and confirms each one applied the change.
- **Audit in control-plane** — generic `config_change_event` table with per-instance results.
- **Three operation modes** — PushConfig (mutate + verify + record), QueryOne (read from one instance), QueryAll (read from all + aggregate).

## Scope

**This iteration delivers:**
- Layer 1: `servicecall` package (instance discovery, HTTP client, auth)
- Layer 2: `PushConfig()`, `QueryOne()`, `QueryAll()` operations
- Layer 3: Kill switch handlers as the first consumer
- Generic `config_change_event` table
- Compliance-proxy cleanup (remove pubsub, keep core + persistence)
- Frontend updates (kill switch only)

**Future consumers (not implemented now):**
- Domain allowlist updates → `config_type = "domain_allowlist"`
- Hook pipeline enable/disable → `config_type = "hook_config"`
- AI Gateway route rules → `config_type = "route_rules"`
- AI Gateway provider enable/disable → `config_type = "provider_config"`
- Observability config → `config_type = "observability"`
- Rate limit adjustments → `config_type = "rate_limit"`

## Architecture Overview

```
┌───────────────────────────────────────────────────────────────────┐
│  Layer 3: Business Handlers                                       │
│  KillswitchToggle, KillswitchForceClose, KillswitchHistory       │
│  (future: AllowlistUpdate, RouteRulesUpdate, QueryRoutes, ...)   │
└──────────────────────────┬────────────────────────────────────────┘
                           │ uses
┌──────────────────────────▼────────────────────────────────────────┐
│  Layer 2: Operation Modes       (servicecall package)             │
│                                                                   │
│  PushConfig(opts)  — discover → write Redis expected → concurrent │
│                      POST all → retry failed + GET verify →       │
│                      INSERT config_change_event → return summary  │
│                                                                   │
│  QueryOne(opts)    — discover → call first healthy → return       │
│                                                                   │
│  QueryAll(opts)    — discover → concurrent GET all → aggregate    │
└──────────────────────────┬────────────────────────────────────────┘
                           │ uses
┌──────────────────────────▼────────────────────────────────────────┐
│  Layer 1: Infrastructure        (servicecall package)             │
│                                                                   │
│  Discover(service) — query service_instance table                 │
│  Call(instance, method, path, body, headers) — HTTP + auth        │
│  Token management  — API token + elevated token per service       │
└───────────────────────────────────────────────────────────────────┘
```

### Data Flow: PushConfig (Kill Switch Example)

```
User clicks "Disable Bump"
  │
  ▼
BFF Handler: KillswitchToggle
  │
  ├─ Build PushConfigRequest{
  │    Service:     "compliance-proxy",
  │    ConfigType:  "killswitch",
  │    Action:      "disable",
  │    Path:        "/killswitch",
  │    Body:        {"enabled": false},
  │    ExpectedState: {"enabled": false},
  │    RedisKey:    "nexus:killswitch:state",
  │    RedisValue:  {"enabled":false, "changedBy":"...", "lastChanged":"..."},
  │    VerifyPath:  "/killswitch",          // GET to verify
  │    VerifyCheck: func(body) bool { ... } // check enabled==false
  │  }
  │
  ▼
servicecall.PushConfig()
  │
  ├─ 1. Discover("compliance-proxy") → instances[]
  ├─ 2. Redis SET (if RedisKey provided)
  ├─ 3. Concurrent POST to all instances
  ├─ 4. Retry failed + GET VerifyPath to confirm
  ├─ 5. INSERT config_change_event
  └─ 6. Return PushConfigResult
```

## Data Model

### New Table: `config_change_event`

```sql
CREATE TABLE config_change_event (
    id               TEXT PRIMARY KEY,
    timestamp        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    service          TEXT NOT NULL,
    config_type      TEXT NOT NULL,
    action           TEXT NOT NULL,
    actor_id         TEXT NOT NULL,
    actor_name       TEXT NOT NULL,
    expected_state   JSONB NOT NULL,
    total_instances  INT NOT NULL,
    success_count    INT NOT NULL,
    fail_count       INT NOT NULL,
    instance_results JSONB NOT NULL,
    source_ip        TEXT
);

CREATE INDEX config_change_event_timestamp_idx ON config_change_event(timestamp DESC);
CREATE INDEX config_change_event_service_type_idx ON config_change_event(service, config_type);
```

**`expected_state` examples by config_type:**

| config_type | expected_state |
|-------------|---------------|
| killswitch | `{"enabled": false}` |
| domain_allowlist | `{"domains": ["a.com", "b.com"]}` |
| route_rules | `{"rules": [...]}` |
| provider_config | `{"provider": "openai", "enabled": false}` |

**`instance_results` JSONB structure (same for all config_types):**

```json
[
  {
    "instanceId": "proxy-abc123",
    "address": "10.0.0.5:3002",
    "success": true,
    "response": { ... },
    "latencyMs": 45,
    "error": null
  }
]
```

The `response` field is opaque JSONB — different config_types return different shapes. The framework stores whatever the instance returned.

### Altered Table: `service_instance`

```sql
ALTER TABLE service_instance ADD COLUMN runtime_api_url TEXT;
```

Populated during heartbeat registration. Example: `http://10.0.0.5:3002`.

### Redis Keys

Convention: `nexus:{config_type}:state`

| Key | Written by | Read by |
|-----|-----------|---------|
| `nexus:killswitch:state` | BFF (primary) + compliance-proxy (supplementary) | compliance-proxy on startup |

Future config types will follow the same pattern if they need startup recovery.

### Heartbeat `checks` Addition

```json
{
  "redis": "connected",
  "killswitch": "enabled"
}
```

The `killswitch` check always returns `ok=true` (does not affect instance health status) — purely informational for reconciliation.

## `servicecall` Package Design

### Package: `packages/control-plane/internal/servicecall/`

**Files:**
- `client.go` — Client struct, Discover, Call
- `push.go` — PushConfig operation
- `query.go` — QueryOne, QueryAll operations
- `types.go` — shared types (InstanceResult, PushConfigRequest, etc.)

### Types (`types.go`)

```go
// InstanceResult records one instance's outcome.
type InstanceResult struct {
    InstanceID string          `json:"instanceId"`
    Address    string          `json:"address"`
    Success    bool            `json:"success"`
    Response   json.RawMessage `json:"response"`
    LatencyMs  int64           `json:"latencyMs"`
    Error      *string         `json:"error"`
}

// PushConfigRequest defines a config push operation.
type PushConfigRequest struct {
    Service       string            // target service name
    ConfigType    string            // e.g. "killswitch"
    Action        string            // e.g. "disable"
    Method        string            // HTTP method (default POST)
    Path          string            // runtime API path, e.g. "/killswitch"
    Body          []byte            // request body (nil for no body)
    Headers       map[string]string // extra headers (X-Nexus-Actor-*)
    Elevated      bool              // use elevated token
    ExpectedState json.RawMessage   // stored in config_change_event
    RedisKey      string            // optional: write expected state to Redis
    RedisValue    []byte            // optional: value to SET
    VerifyPath    string            // optional: GET path to verify after failure
    VerifyCheck   func(body []byte) bool // optional: check if state matches expected
    ActorID       string
    ActorName     string
    SourceIP      string
}

// PushConfigResult is the outcome of a PushConfig operation.
type PushConfigResult struct {
    TotalInstances  int              `json:"totalInstances"`
    SuccessCount    int              `json:"successCount"`
    FailCount       int              `json:"failCount"`
    InstanceResults []InstanceResult `json:"instanceResults"`
}

// QueryRequest defines a query to service instances.
type QueryRequest struct {
    Service  string
    Method   string // default GET
    Path     string
    Headers  map[string]string
    Elevated bool
}
```

### Client (`client.go`)

```go
type Client struct {
    db     *store.DB
    redis  *redis.Client // nil when unavailable
    tokens map[string]ServiceTokens // per-service tokens
    logger *slog.Logger
}

type ServiceTokens struct {
    APIToken      string
    ElevatedToken string
}

func New(db *store.DB, redis *redis.Client, logger *slog.Logger) *Client

// Discover returns healthy instances of the given service that have a runtime API URL.
func (c *Client) Discover(ctx context.Context, service string) ([]store.ServiceInstance, error)

// Call sends one HTTP request to one instance and returns the result.
func (c *Client) Call(ctx context.Context, inst store.ServiceInstance, method, path string, body []byte, headers map[string]string, elevated bool) InstanceResult

// RegisterTokens sets API and elevated tokens for a service.
func (c *Client) RegisterTokens(service string, tokens ServiceTokens)
```

### PushConfig (`push.go`)

```go
// PushConfig executes the full push-verify-record flow.
func (c *Client) PushConfig(ctx context.Context, req PushConfigRequest) (*PushConfigResult, error)
```

Flow:
1. `Discover(req.Service)` → instances; error if 0
2. If `req.RedisKey != ""` → Redis SET
3. Concurrent `Call()` to all instances
4. For failed: retry once via `Call()`, then `GET req.VerifyPath` + `req.VerifyCheck`
5. `INSERT INTO config_change_event`
6. Return `PushConfigResult`

### Query (`query.go`)

```go
// QueryOne calls the first healthy instance and returns its response.
func (c *Client) QueryOne(ctx context.Context, req QueryRequest) (json.RawMessage, error)

// QueryAll calls all healthy instances concurrently and returns all responses.
func (c *Client) QueryAll(ctx context.Context, req QueryRequest) ([]InstanceResult, error)
```

## BFF Kill Switch Handlers (Layer 3)

Handlers in `packages/control-plane/internal/handler/admin_killswitch.go` use `servicecall.Client`:

```go
func (h *AdminHandler) KillswitchToggle(c echo.Context) error {
    // Parse request, build PushConfigRequest, call h.ServiceCall.PushConfig()
}

func (h *AdminHandler) KillswitchForceClose(c echo.Context) error {
    // Build PushConfigRequest with Elevated=true, path="/killswitch/force-close"
}

func (h *AdminHandler) KillswitchHistory(c echo.Context) error {
    // Query config_change_event WHERE config_type='killswitch'
}

func (h *AdminHandler) KillswitchGetState(c echo.Context) error {
    // h.ServiceCall.QueryOne() to get current state from first healthy instance
}
```

**Route registrations** replace the old proxy-forward handlers:

```go
g.GET("/proxy/killswitch", h.KillswitchGetState, ...)
g.POST("/proxy/killswitch", h.KillswitchToggle, ...)
g.POST("/proxy/killswitch/force-close", h.KillswitchForceClose, ...)
g.GET("/proxy/compliance/killswitch-history", h.KillswitchHistory, ...)
```

## Compliance-Proxy Changes

### Removed

| Component | What |
|-----------|------|
| `KillSwitchRedisPublisher` | Publisher struct + Publish method |
| `KillSwitchRedisSubscriber` | Subscriber struct + Start/Stop + subscribeLoop |
| `KillSwitchChannel` constant | `"nexus:killswitch"` |
| `killSwitchMessage` struct | Pubsub payload type |
| `generateInstanceID()` | Random instance ID for pubsub |
| `publisher` field on KillSwitch | + `SetPublisher()` |
| `auditFunc` field on KillSwitch | + `SetAuditFunc()` |
| Publish/audit calls in Toggle/ForceClose | Post-unlock side effects |
| Pubsub init in main.go | Publisher/subscriber creation + goroutine |
| Audit callback in main.go | `killSwitch.SetAuditFunc(...)` block |
| `GetKillswitchHistory()` in misc_queries.go | Old query from traffic_event |

### Retained

| Component | Why |
|-----------|-----|
| `KillSwitch` core | `atomic.Bool`, `Toggle()`, `ForceClose()`, `IsEnabled()` |
| `KillSwitchPersistence` + Redis impl | Startup recovery from `nexus:killswitch:state` |
| `persistAsync()` | Supplementary Redis write (idempotent with BFF write) |
| Runtime API handlers | `/killswitch`, `/killswitch/force-close`, `/killswitch/history` |
| In-memory history ring buffer | Local operational debugging |

### Added

1. **Heartbeat killswitch checker** — reports `checks.killswitch = "enabled"/"disabled"`
2. **Heartbeat `RuntimeAPIURL`** — registered during heartbeat, stored in `service_instance`

## Frontend Changes

### Toggle API Response

```typescript
interface ConfigPushResponse {
  totalInstances: number;
  successCount: number;
  failCount: number;
  instanceResults: InstanceResult[];
}

interface InstanceResult {
  instanceId: string;
  address: string;
  success: boolean;
  response: Record<string, unknown> | null;
  latencyMs: number;
  error: string | null;
}
```

### History Table

Source: `GET /api/admin/proxy/compliance/killswitch-history` → reads `config_change_event` WHERE `config_type='killswitch'`.

Columns: Timestamp, Action, Operator, Expected State, Instances (success/total), Details (expandable).

### Toast Notifications

- All success → green: "Bump disabled (3/3 instances synced)"
- Partial → yellow: "Bump disabled (2/3 synced, 1 failed)"
- All failed → red: "Operation failed, all instances unreachable"

## Edge Cases

| Scenario | Handling |
|----------|----------|
| BFF crashes mid-operation | Redis expected value already written; called instances have correct state; uncalled instances keep old state; heartbeat reconciliation detects mismatch |
| Instance restarts during toggle | Reads Redis expected value on startup → restores correct state |
| Redis unavailable | BFF logs warning, still calls instances; notes in event |
| 0 healthy instances | Returns 400, does not write Redis or event |
| Heartbeat state mismatch | Control-plane logs warning; frontend status page can highlight |
| Concurrent toggles | Redis SET is atomic (last writer wins); both events recorded; instances converge |

## Migration Notes

- `config_change_event` table is new — no data migration.
- Existing `traffic_event` rows with `target_host='killswitch'` become historical artifacts.
- `service_instance.runtime_api_url` is nullable — populated on next heartbeat.
- Redis pubsub removal is code deletion only.
