---
doc: project-structure
area: index
service: platform
tier: 1
updated: 2026-05-22
---

# Project Structure

> Directory listings below regenerated from `ls -1 packages/*/internal/` on 2026-05-22; for a code-grep on a subpackage that doesn't appear here, the dir was either renamed or removed in a refactor — open a PR to update.

## Top Level

```
nexus-gateway/
  go.work                    # Go workspace (links all Go modules)
  package.json               # npm workspaces root
  docker-compose.yml         # PostgreSQL + Redis for local dev
  Makefile                   # Aggregate build/test/dev targets
  scripts/
    dev-start.sh             # One-command bootstrap script
    check-i18n-parity.mjs    # i18n locale parity checker
  packages/
    nexus-hub/               # Go -- Thing registry + shadow + WS fanout (port 3060)
    control-plane/           # Go -- Admin API server (port 3001)
    control-plane-ui/        # React -- Dashboard frontend (port 3000)
    ai-gateway/              # Go -- AI traffic proxy (port 3050)
    compliance-proxy/        # Go -- Transparent TLS proxy (port 3040)
    agent/                   # Go -- Desktop agent (macOS/Windows)
    shared/                  # Go -- Shared business logic
  tools/
    db-migrate/              # Prisma schema, migrations, Go codegen
  docs/
    users/                   # User-facing docs (features, API, product)
    developers/              # Developer docs (architecture, specs, workflow)
    operators/               # Operator docs (runbooks, monitoring, ops)
    _archive/                # Archived programs and historical reference
    _wiki/                   # GitHub Wiki source pages
    handoffs/                # Session handoff documents
```

## packages/control-plane

```
control-plane/
  cmd/control-plane/
    main.go                  # Entry point -- Echo server setup
  internal/
    ai/                      # Virtual key + provider/model CRUD + cache + routing rule handlers
    fleet/                   # Node, config-sync, agent-cert-rotation admin handlers
    governance/              # IAM, audit, alerting, SIEM-bridge, exemptions handlers
    handler/                 # Echo route mount, middleware wiring, shared handler utilities
    identity/                # IAM core, authserver (OAuth + PKCE), IdP store, JWT verifier
    infrastructure/          # Kill switch, jobs, scheduler, config reconciler
    observability/           # Diag log, metrics, traces, dashboard data
    platform/                # Server runtime, bootstrap, cron, Hub client
    settings/                # System config, token-field admin, theme
    store/                   # Postgres access layer (pgx pool wrappers + per-area stores)
    traffic/                 # Traffic event + analytics + dashboard handlers
  control-plane.config.yaml  # Default config file
```

## packages/control-plane-ui

```
control-plane-ui/
  src/
    main.tsx                 # React entry point
    App.tsx                  # Root component + router
    api/                     # API service layer
    auth/                    # Auth context and hooks
    components/              # Reusable UI components
    constants/               # App-wide constants
    context/                 # React contexts
    hooks/                   # Custom React hooks
    i18n/                    # Internationalization (i18next)
    lib/                     # Utility functions
    pages/                   # Page components (route targets)
    routes/                  # Route definitions
    test/                    # Test setup and utilities
    theme/                   # Theme/styling
  e2e/                       # Playwright E2E tests
  vite.config.ts             # Vite + Vitest config
  tsconfig.json              # TypeScript config
```

## packages/ai-gateway

```
ai-gateway/
  cmd/ai-gateway/
    main.go                  # Entry point -- net/http server, errgroup lifecycle
  internal/
    auth/                    # Virtual key auth + caller identity resolution
    cache/                   # Response cache (Redis-backed, semantic + exact)
    config/                  # YAML loader + hot-reload config receiver
    credentials/             # Provider credential decryption + per-VK selection
    embeddings/              # /v1/embeddings canonical bridge + per-provider codecs
    execution/               # Request lifecycle orchestrator (hooks → route → forward → cache)
    ingress/                 # /v1/* HTTP ingress handlers (chat, responses, messages, gemini, embeddings)
    platform/                # Server bootstrap, Hub thingclient, lifecycle
    policy/                  # Hook pipeline + rule-pack execution + decision engine
    providers/               # Provider adapter registry (OpenAI, Anthropic, Gemini, …) + spec adapters
    routing/                 # Routing engine with strategies (model, region, weighted, fallback)
    runtimeapi/              # Internal runtime API (stats, kill switch, debug)
  ai-gateway.config.yaml     # Default config file
```

## packages/compliance-proxy

```
compliance-proxy/
  cmd/compliance-proxy/
    main.go                  # Entry point -- TCP listener, proxy server
    init.go                  # Initialization helpers (Redis, certs, audit, compliance)
  internal/
    access/                  # IP/domain allowlist + interception-domain matching
    audit/                   # Buffered audit writer (traffic_event + audit_event)
    compliance/              # Compliance kernel (hook execution on intercepted traffic)
    config/                  # YAML loader + hot-reload config receiver
    exemption/               # Temporary compliance exemption store
    health/                  # Health/readiness handler
    metrics/                 # Prometheus metrics
    proxy/                   # Core proxy server (CONNECT, TLS bump, upstream transport)
    runtime/                 # Runtime API (kill switch, stats, exemptions, hot-reload)
    siem/                    # SIEM log forwarder
    testutil/                # Test helpers
    tls/                     # Dynamic TLS certificate issuance + caching (TLS bump CA)
```

## packages/agent

```
agent/
  cmd/agent/
    main.go                  # Entry point -- run/enroll/unenroll commands
    platform_other.go        # Platform dispatch (non-Windows)
    platform_windows.go      # Platform dispatch (Windows service commands)
  internal/
    compliance/              # Agent-side hook pipeline + exemption + TLS-bump intercept
    host/                    # Local proxy + status API (unix socket / named pipe) + GUI status
    identity/                # Enrollment (mTLS cert provisioning) + keystore + cert rotation
    lifecycle/               # Run loop, supervisor, updater, crash-loop detector
    network/                 # Platform-agnostic network interception bridge
    observability/           # Local audit queue, heartbeat, telemetry (OTEL)
    platform/                # Platform-specific interception (darwin pf / NE, windows WFP)
    policy/                  # Local policy engine (inspect / passthrough / deny)
    sync/                    # Config + interception-domain sync from Hub (pull model)
```

## packages/nexus-hub

```
nexus-hub/
  cmd/nexus-hub/
    main.go                  # Entry point -- HTTP + WebSocket server (port 3060)
  internal/
    alerts/                  # Alert rule evaluation + dispatch
    compliance/              # Server-side compliance pipeline (Things upload audit/traffic)
    config/                  # YAML loader + thing_config_template authority
    fleet/                   # Thing registry, shadow store, enrollment, cert lifecycle
    handler/                 # HTTP route mount + middleware
    identity/                # Thing identity verification, mTLS, JWT issuance
    jobs/                    # Scheduler + retention jobs + reconciler ticks
    jwks/                    # JWKS endpoint for downstream JWT verification
    observability/           # Diag log, metrics, traces
    quota/                   # Global quota counters (Redis) + reconciliation
    self/                    # Hub's own Thing registration + health
    storage/                 # Postgres + Redis + S3 spillstore access
    traffic/                 # Traffic event ingest + normalization + cost stamping
    ws/                      # WebSocket fanout to Things (config push + commands)
```

## packages/shared

```
shared/
  audit/                     # Shared audit event types + body redaction
  core/                      # Cross-service primitives (bootenv, diag, logging, metrics, telemetry)
    bootenv/                 # .env loader + env precedence (yaml + env + flags)
    diag/                    # Structured diag log writer
    logging/                 # slog setup + SlogSink → diag pipeline
    metrics/                 # Prometheus registry + standard buckets
    telemetry/               # OpenTelemetry tracing setup
  identity/                  # IAM engine, PKCE helpers, refresh-token auth
    iam/                     # NRN, action catalog, policy evaluation
    pkce/                    # OAuth + PKCE flow helpers
    rstokenauth/             # Refresh-token auth verifier
  policy/                    # Compliance + hook + rule-pack runtime
    decision/                # Policy decision shape + reasons
    device/                  # Device-class detection (consumer-web / IDE / SDK)
    domain/                  # Interception-domain engine (CP + agent shared)
    hooks/                   # Hook interface + built-in hooks (keyword, PII, content-safety, …)
    payloadcapture/          # Body capture + redaction policy
    pipeline/                # Hook pipeline orchestrator (3-stage)
    rulepack/                # Rule pack loader + matcher
  schemas/                   # Hand-maintained Go mirrors of Prisma models
    configkey/               # Canonical configKey constants + ValidByThingType + TypedRegistry
    configtypes/             # Per-area struct types (enums, identity, interception, observability, policy)
    credstate/               # Credential state shape (encrypted + metadata)
    domain/                  # Domain entity shapes
    thingtype/               # Thing-type constants + validation
  storage/                   # Cross-service storage clients
    cacheconfig/             # Cache config struct + receiver
    configcache/             # Pull-based config cache + invalidation
    configstore/             # Postgres-backed config store
    redisfactory/            # Universal Redis client factory (REDIS_MODE / REDIS_ADDRS)
    spillstore/              # S3 / on-disk payload spill (large bodies)
    spillupload/             # Agent → Hub presigned spill upload
  traffic/                   # Traffic matching, adaptation, observability
    adapters/                # Per-protocol adapters (IDE / Web / SDK normalizers)
    classify/                # Traffic classification (ai-chat / embeddings / completion / …)
    (top-level .go)          # Matchers, detect, phasetimer, latencybreakdown, snapshot, tracing
  transport/                 # Cross-service transport: thingclient, MQ, normalize, TLS bump
    bufconn/                 # In-memory net.Conn for tests
    configloader/            # Generic config-receiver registration helpers
    http/                    # HTTP client/server primitives + middleware
    inputstaging/            # Body buffering + size guards
    mq/                      # NATS JetStream wrappers (publish + subscribe)
    normalize/               # Canonical request/response normalize (codecs + extract + core)
    responseio/              # Streaming response framing (SSE + plain)
    streaming/               # SSE stream session + buffer pool
    thingclient/             # Thing-side client (WebSocket primary, HTTP fallback) + OnConfigChanged
    tlsbump/                 # TLS bump certificate factory
    wirerewrite/             # Wire-level rewrite passes (header strip + path rewrite)
```

## tools/db-migrate

```
db-migrate/
  schema.prisma              # Database schema (source of truth)
  migrations/                # Prisma migration files
  seed/                      # canonical seed + seed-baseline.sql snapshot
  package.json               # Prisma + dev dependencies
```

Go struct types that mirror Prisma models live in `packages/shared/schemas/configtypes/{enums,identity,interception,observability,policy}/` and are hand-maintained alongside schema changes — see `db-migration-mechanics-architecture.md` §5.
