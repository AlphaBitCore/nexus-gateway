# Go Services Config Unification — Design Spec

**Date:** 2026-04-19
**Author:** Claude Code (brainstorm with user)
**Status:** Draft — awaiting user review

## Goal

Unify Go service configuration across the five services in this repo
(`nexus-hub`, `control-plane`, `ai-gateway`, `compliance-proxy`, `agent`)
so that:

1. Every service has both a **`<service>.config.yaml`** (production
   template — full field coverage with inline comments) and a
   **`<service>.dev.yaml`** (local-dev values ready to run with
   `docker-compose`).
2. Every setting the service consumes is reachable through a YAML key.
   No hidden `os.Getenv` in `main.go` that bypasses the Config struct.
3. Every field declared in the Config struct is used by the service.
4. `.vscode/launch.json` can launch **all five** Go services via
   `-config <service>.dev.yaml`, with no per-setting env vars in the
   `env:` block.
5. Env vars remain available as runtime overrides (12-factor / k8s
   secrets) but are never the sole source of a setting.

## Approach

**Selected: "Pure YAML with env override"** (compliance-proxy pattern,
already working and explicitly praised by the user).

Per service:
- `<service>.config.yaml` — committed production template. Every field
  in the Config struct appears with a sensible default or placeholder
  comment. Secrets are left empty with a comment pointing to the env
  var name.
- `<service>.dev.yaml` — committed local-dev override. Contains only
  the fields that differ from the production template (ports, dev
  DSNs, dev secrets like `deadbeef...`). Everything else inherits from
  defaults inside `Load()`.
- `Config` struct & `Load()`:
  - Every direct `os.Getenv` in `main.go` that reads application config
    is moved into `Load()`. `main.go` only consults `cfg.*` afterwards.
  - Env-var override precedence in `Load()`: `defaults → yaml → env`.
  - `validate()` rejects obvious missing fields.

Pattern for each Load():
```go
func Load(path string) (*Config, error) {
    cfg := defaults()
    if data, err := os.ReadFile(path); err == nil {
        if err := yaml.Unmarshal(data, cfg); err != nil { return nil, err }
    } else if !os.IsNotExist(err) {
        return nil, err
    }
    applyEnvOverrides(cfg) // optional — only for settings we deliberately expose to ops
    if err := validate(cfg); err != nil { return nil, err }
    return cfg, nil
}
```

## Per-Service Changes

### 1. compliance-proxy (least work — already in target shape)

**Config struct changes:** none.

**`.config.yaml`:** already comprehensive — leave as-is.

**`.dev.yaml` additions:**
```yaml
mq:
  driver: "nats"
  nats:
    url: "nats://localhost:4222"

registry:
  controlPlaneUrl: "http://localhost:3060"  # Nexus Hub URL (thingclient)
  token: "dev-service-token"
```

**main.go:** no change.

### 2. nexus-hub

**Config struct changes:** none (struct is already richer than the YAML).

**`.config.yaml` additions** (fields in struct but missing from YAML):
```yaml
consumers:
  enabled: true
  batchSize: 100
  flushInterval: "5s"
  siem:
    enabled: false
    url: ""
    headers: {}
    format: "json"
    batchSize: 200
    flushInterval: "5s"
    eventTypes: []

hub:
  id: ""
  advertiseAddr: ""
  allowedOrigins: []

otel:
  enabled: false
  endpoint: ""
```

**`.dev.yaml` (new file):**
```yaml
server:
  port: 3060
database:
  url: "postgres://postgres:postgres@localhost:55532/nexus_gateway?sslmode=disable"
  maxConns: 20
  minConns: 5
redis:
  url: "redis://localhost:6437/0"
mq:
  driver: "nats"
  nats:
    url: "nats://localhost:4222"
consumers:
  enabled: true
  batchSize: 100
  flushInterval: "5s"
scheduler:
  enabled: true
  driftCheckInterval: "60s"
  identityEnrichInterval: "5m"
auth:
  internalServiceToken: "dev-service-token"
agentCA:
  dir: ".agent-ca"
hub:
  id: "hub-dev"
  advertiseAddr: "127.0.0.1:3060"
  allowedOrigins: ["http://localhost:3000"]
log:
  level: "info"
  format: "json"
```

**main.go:** no change (already clean).

### 3. control-plane

**Config struct additions** (to absorb `os.Getenv` in main.go):
```go
type AuthConfig struct {
    BootstrapKey          string `yaml:"bootstrapKey"`
    AllowDevAuth          bool   `yaml:"allowDevAuth"`
    InternalServiceToken  string `yaml:"internalServiceToken"` // NEW (from INTERNAL_SERVICE_TOKEN)
}

type CryptoConfig struct {
    EncryptionKey        string `yaml:"encryptionKey"`
    EncryptionPassphrase string `yaml:"encryptionPassphrase"`
    EncryptionSalt       string `yaml:"encryptionSalt"`
    CredentialKeyMap     string `yaml:"credentialKeyMap"`
    Production           bool   `yaml:"production"` // NEW (replaces NODE_ENV==production check)
}
```

**main.go changes:**
- Line 121: `Production: os.Getenv("NODE_ENV") == "production"` → `Production: cfg.Crypto.Production`
- Line 173: `os.Getenv("INTERNAL_SERVICE_TOKEN")` → `cfg.Auth.InternalServiceToken`
- Line 192: same substitution

**`Load()` additions in config.go:**
```go
if v := os.Getenv("INTERNAL_SERVICE_TOKEN"); v != "" {
    cfg.Auth.InternalServiceToken = v
}
// drop NODE_ENV — cfg.Crypto.Production replaces it
```

**`.config.yaml` (new file):** complete template with all fields.

**`.dev.yaml` (new file):**
```yaml
server:
  port: 3001
  shutdownTimeout: 10
database:
  url: "postgresql://postgres:postgres@localhost:55532/nexus_gateway?sslmode=disable"
  maxOpenConns: 25
  maxIdleConns: 5
  connMaxLifetime: 300
redis:
  url: "redis://localhost:6437"
log:
  level: "info"
  format: "json"
bff:
  complianceProxyUrl: "http://127.0.0.1:3040"
  complianceProxyRuntimeUrl: "http://127.0.0.1:3040"
  aiGatewayUrl: "http://127.0.0.1:3050"
  nexusHubUrl: "http://127.0.0.1:3060"
  complianceProxyApiToken: ""
  complianceProxyElevatedToken: ""
auth:
  bootstrapKey: "nexus-dev-bootstrap-key"
  allowDevAuth: true
  internalServiceToken: "dev-service-token"
crypto:
  encryptionKey: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
  encryptionPassphrase: ""
  encryptionSalt: ""
  credentialKeyMap: ""
  production: false
retention:
  auditLogDays: 90
  adminAuditLogDays: 365
  metricRollupDays: 365
  agentAuditDays: 90
agent:
  caDir: ".agent-ca"
otel:
  endpoint: ""
  serviceName: "nexus-control-plane"
scheduler:
  enabled: true
mq:
  driver: "nats"
  nats:
    url: "nats://localhost:4222"
```

### 4. ai-gateway (biggest refactor)

**Config struct additions:**
```go
type Config struct {
    Server   ServerConfig   `yaml:"server"`
    Database DatabaseConfig `yaml:"database"`
    Redis    RedisConfig    `yaml:"redis"`
    Auth     AuthConfig     `yaml:"auth"`
    Log      LogConfig      `yaml:"log"`
    Registry RegistryConfig `yaml:"registry"`
    MQ       MQConfig       `yaml:"mq"`
    CORS     CORSConfig     `yaml:"cors"`
    Cache    CacheConfig    `yaml:"cache"` // NEW
    Otel     OtelConfig     `yaml:"otel"`  // NEW
}

type CacheConfig struct {
    Enabled bool          `yaml:"enabled"`
    TTL     time.Duration `yaml:"ttl"`
    Prefix  string        `yaml:"prefix"`
}

type OtelConfig struct {
    Endpoint    string `yaml:"endpoint"`
    ServiceName string `yaml:"serviceName"`
}
```

**main.go changes:**
- Line 80: `if dsn := os.Getenv("DATABASE_URL"); dsn != ""` → `if cfg.Database.URL != ""` (YAML is already authoritative via Load)
- Lines 253-262: replace `os.Getenv("CACHE_*")` block with `cache.Config{Enabled: cfg.Cache.Enabled, TTL: cfg.Cache.TTL, Prefix: cfg.Cache.Prefix}`
- `loadOtelConfig` (lines 492-517): replace `os.Getenv("OTEL_ENDPOINT")` / `OTEL_SERVICE_NAME` with `cfg.Otel.Endpoint` / `cfg.Otel.ServiceName`. Requires threading `cfg` into the function.

**`Load()` additions:**
```go
if v := os.Getenv("DATABASE_URL"); v != "" {
    cfg.Database.URL = v
}
if v := os.Getenv("CACHE_ENABLED"); v == "true" || v == "1" {
    cfg.Cache.Enabled = true
}
if v := os.Getenv("CACHE_TTL"); v != "" {
    if d, err := time.ParseDuration(v); err == nil { cfg.Cache.TTL = d }
}
if v := os.Getenv("CACHE_PREFIX"); v != "" {
    cfg.Cache.Prefix = v
}
if v := os.Getenv("OTEL_ENDPOINT"); v != "" {
    cfg.Otel.Endpoint = v
}
if v := os.Getenv("OTEL_SERVICE_NAME"); v != "" {
    cfg.Otel.ServiceName = v
}
```

**`.config.yaml` (new file):** complete template.

**`.dev.yaml` (new file):**
```yaml
server:
  port: 3050
  readTimeoutSec: 30
  writeTimeoutSec: 60
database:
  url: "postgresql://postgres:postgres@localhost:55532/nexus_gateway?sslmode=disable"
redis:
  addr: "localhost:6437"
  password: ""
  db: 0
auth:
  hmacSecret: "nexus-dev-hmac-secret-do-not-use-in-prod"
  credentialMasterKey: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
  credentialKeyMap: ""
log:
  level: "info"
  format: "json"
registry:
  controlPlaneUrl: "http://127.0.0.1:3060"  # Nexus Hub
  token: "dev-service-token"
mq:
  driver: "nats"
  nats:
    url: "nats://localhost:4222"
cors:
  enabled: true
  allowedOrigins: ["http://localhost:3000"]
  allowedMethods: ["GET", "POST", "OPTIONS"]
  allowedHeaders: ["Content-Type", "Authorization", "x-nexus-virtual-key", "x-request-id"]
  maxAgeSec: 600
cache:
  enabled: false
  ttl: "5m"
  prefix: "ai-gw:"
otel:
  endpoint: ""
  serviceName: "nexus-ai-gateway"
```

### 5. agent

**Config struct changes:** none.

**`.config.yaml` (new file):** template documenting every field with
comments, secret paths blank.

**`agent.dev.yaml` (new file):**
```yaml
log:
  level: "debug"
  format: "text"
gatewayURL: "https://localhost:3050"  # placeholder — agent exits before use unless enrolled
hubURL: "ws://localhost:3060/ws"
hubHTTPURL: "http://localhost:3060"
certFile: ".nexus-agent/device.crt"
keyFile: ".nexus-agent/device.key"
caCertFile: ".nexus-agent/ca.crt"
hubCACertFile: ""
auditDBPath: ".nexus-agent/audit.db"
heartbeatIntervalSec: 60
auditDrainIntervalSec: 30
auditBatchSize: 200
auditRetentionDays: 30
configRefreshSec: 300
defaultAction: "passthrough"
policyRules: []
updaterEnabled: false
updaterCheckSec: 3600
exemptionEnabled: true
exemptionFailureThreshold: 3
exemptionWindowSec: 3600
exemptionDurationSec: 86400
exemptionAllowlist: []
exemptionDenylist: []
otelEnabled: false
otelEndpoint: "http://localhost:4318"
otelServiceName: "nexus-agent"
otelSamplingRate: 1.0
```

**main.go:** no change needed (only env read is `XDG_RUNTIME_DIR`, a
system convention unrelated to app config — leave alone).

## `.vscode/launch.json` (new)

```json
{
  "version": "0.2.0",
  "configurations": [
    {
      "name": "Nexus Hub (dev)",
      "type": "go",
      "request": "launch",
      "mode": "debug",
      "program": "${workspaceFolder}/packages/nexus-hub/cmd/nexus-hub",
      "cwd": "${workspaceFolder}/packages/nexus-hub",
      "args": ["-config", "nexus-hub.dev.yaml"]
    },
    {
      "name": "Control Plane (dev)",
      "type": "go",
      "request": "launch",
      "mode": "debug",
      "program": "${workspaceFolder}/packages/control-plane/cmd/control-plane",
      "cwd": "${workspaceFolder}/packages/control-plane",
      "args": ["-config", "control-plane.dev.yaml"]
    },
    {
      "name": "AI Gateway (dev)",
      "type": "go",
      "request": "launch",
      "mode": "debug",
      "program": "${workspaceFolder}/packages/ai-gateway/cmd/ai-gateway",
      "cwd": "${workspaceFolder}/packages/ai-gateway",
      "args": ["-config", "ai-gateway.dev.yaml"]
    },
    {
      "name": "Compliance Proxy (dev)",
      "type": "go",
      "request": "launch",
      "mode": "debug",
      "program": "${workspaceFolder}/packages/compliance-proxy/cmd/compliance-proxy",
      "cwd": "${workspaceFolder}/packages/compliance-proxy",
      "args": ["-config", "compliance-proxy.dev.yaml"]
    },
    {
      "name": "Agent (dev, run)",
      "type": "go",
      "request": "launch",
      "mode": "debug",
      "program": "${workspaceFolder}/packages/agent/cmd/agent",
      "cwd": "${workspaceFolder}/packages/agent",
      "args": ["run", "-config", "agent.dev.yaml"]
    }
  ],
  "compounds": [
    {
      "name": "Platform Services (Hub + CP + AI GW + Proxy)",
      "configurations": [
        "Nexus Hub (dev)",
        "Control Plane (dev)",
        "AI Gateway (dev)",
        "Compliance Proxy (dev)"
      ]
    }
  ]
}
```

## Audit Checklist (what "complete / no invalid" means here)

For each of the five services, the implementation MUST verify:

1. **Struct → YAML**: every exported field of the top-level `Config`
   struct appears as a YAML key in `<service>.config.yaml` (including
   nested structs, recursively). Ran with `yaml.Marshal(cfg)` on the
   `defaults()` output as a reference.
2. **YAML → Struct**: every top-level key in `<service>.config.yaml`
   and `<service>.dev.yaml` round-trips through `yaml.Unmarshal` without
   producing unmapped fields (check with `yaml.Node` strict mode or a
   manual key-set diff against the struct).
3. **main.go → Struct**: `grep os.Getenv packages/<service>/cmd/**/*.go`
   returns only system vars (`XDG_RUNTIME_DIR`, etc.), never
   application config.
4. **launch.json**: each service appears; each uses `-config` flag; no
   per-setting env vars in `env:` (except `DEBUG`, `GOTRACEBACK`, etc.
   for Go-runtime-level tuning).
5. **Compound**: launches all four server services concurrently
   (agent excluded — requires enrollment).

## Out of Scope

- Hardcoded timeouts/intervals inside main.go that are not today in the
  Config struct (e.g. `webhookClient` timeout `10s`, rate-limiter
  cleanup tick `5m`, audit prune tick `1h`, exemption upload tick
  `30s`). These remain hardcoded for now — widening them into config
  is a separate task and can be done only when ops asks for it.
- Moving secret storage off env for production deployments. Env var
  overrides remain the recommended secret channel; this spec only
  ensures there is also a YAML key so config is never hidden.
- CLAUDE.md / docs updates beyond listing the new files and CLI
  behavior.

## Risks

- **Field-name drift**: renaming `Auth.InternalServiceToken` in
  control-plane must not collide with any existing usage. Verified
  against `grep InternalServiceToken packages/control-plane/` — field
  does not yet exist, safe to add.
- **`NODE_ENV` removal**: the env is only used at
  `main.go:121` for vault production flag. Replaced by
  `cfg.Crypto.Production`. No other code path reads it. Verified.
- **ai-gateway `DATABASE_URL` change**: current code reads env
  directly at line 80, ignoring `cfg.Database.URL`. Swapping to
  `cfg.Database.URL` means the YAML becomes authoritative. Any
  production manifest that sets `DATABASE_URL` env continues to work
  through the `Load()` override. No observable behavior change if
  both are set.
- **Compound launch**: starting all four services at once may hit port
  or DB-connection limits on the dev laptop. Low risk; compound
  configs are convenience only.

## Testing / Verification

- `go build ./...` from repo root — compiles cleanly.
- `go test ./packages/{nexus-hub,control-plane,ai-gateway,compliance-proxy,agent}/internal/config/...`
  — existing tests plus new ones covering env override of added
  fields.
- Manual: run each service via the VS Code launch config from a fresh
  shell with no env vars; confirm it starts and `curl /healthz` (or
  `/readyz`) returns 200.
- Manual: run `compound: Platform Services` and confirm all four
  services come up healthy.
