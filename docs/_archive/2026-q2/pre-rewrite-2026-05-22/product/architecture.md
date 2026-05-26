# Nexus Gateway -- Architecture

## System Context

```
                          +------------------+
                          |  Enterprise Apps |
                          | (SDKs, services, |
                          |   dev machines)  |
                          +--------+---------+
                                   |
              +--------------------+--------------------+
              |                    |                     |
              v                    v                     v
    +-------------------+  +-----------------+  +-----------------+
    |    AI Gateway      |  | Compliance Proxy|  |  Desktop Agent  |
    |  (SDK proxy,       |  | (transparent    |  | (OS-level       |
    |   /v1/* endpoints) |  |  TLS intercept) |  |  interception)  |
    +--------+-----------+  +--------+--------+  +--------+--------+
             |                       |                     |
             |     Shared Go compliance code               |
             |     (hooks, pipeline, traffic adapters)     |
             |                       |                     |
             +-------+-------+------+----------+----------+
                     |       |                  |
                     v       v                  v
            +-----------+ +----------+  +------------------+
            | Nexus Hub | | Control  |  |   AI Providers   |
            | (platform | | Plane    |  | (OpenAI, Claude, |
            |  ops      | | (admin   |  |  Azure, Gemini,  |
            |  center)  | |  API/BFF)|  |  DeepSeek, etc.) |
            +-----+-----+ +----+----+  +------------------+
                  |             |
                  +------+------+
                         v
                  +-------------+
                  | Dashboard   |
                  | UI          |
                  +-------------+
```

## Three-Layer Architecture

Nexus Gateway separates concerns into three layers:

**Nexus Hub** (Platform Operations Center): The central management hub. All services and agents connect to Hub as nodes via WebSocket. Hub manages device registration, config synchronization (target vs applied config, pulled on boot and on change-signal), metrics pipeline, scheduled jobs, and monitoring. It is the single source of truth for operational state.

**Control Plane** (Admin API / BFF): The administrative backend serving the Dashboard UI. Handles config CRUD, IAM, SSO, audit queries, and analytics. Stateless and multi-instance. Calls Hub's HTTP API for config propagation and node status queries.

**Data Plane** (`ai-gateway`, `compliance-proxy`, `agent`): Handles live AI traffic. Each service connects to Hub via WebSocket for config sync (target and applied config reporting) and publishes events to the MQ (traffic events, metrics). The data plane services share compliance logic via the `shared` Go package but run as independent processes.

## Component Diagram

```
+-------------------------------------------------------------------+
|                        Control Plane                               |
|                                                                    |
|  +-------------------+     +------------------+                    |
|  | control-plane     |     | control-plane-ui |                    |
|  | (Go + Echo)       |<--->| (React + Vite)   |                    |
|  |                   |     +------------------+                    |
|  | - Admin CRUD API  |                                             |
|  | - BFF proxy       |     Subsystems:                             |
|  | - OAuth+PKCE auth |     - IAM engine (policy eval, caching)     |
|  | - SSO/IdP federation                                            |
|  +-------------------+     - Session store (Redis-backed)          |
|                            - Analytics query layer                  |
|  (Agent CA + enrollment now in Nexus Hub; config push via Hub WS)   |
|                                                                    |
+-------------------------------------------------------------------+

+-------------------------------------------------------------------+
|                         Data Plane                                 |
|                                                                    |
|  +-------------------+  +--------------------+  +----------------+ |
|  | ai-gateway        |  | compliance-proxy   |  | agent          | |
|  | (Go, net/http)    |  | (Go, CONNECT proxy)|  | (Go, desktop)  | |
|  |                   |  |                    |  |                | |
|  | - /v1/* endpoints |  | - TLS bump (MITM)  |  | - OS intercept | |
|  | - VK auth         |  | - Domain filtering |  | - mTLS enroll  | |
|  | - Routing engine  |  | - Access control   |  | - Policy engine| |
|  | - Provider adapt. |  | - Cert cache (LRU  |  | - Config sync  | |
|  | - Quota mgmt      |  |   + Redis)         |  | - Audit queue  | |
|  | - Rate limiting   |  | - Kill switch      |  |   (SQLCipher)  | |
|  | - Hook pipeline   |  | - Alerting         |  | - Heartbeat    | |
|  | - Audit writer    |  | - SIEM forwarding  |  | - Auto-updater | |
|  | - Health tracking |  | - Runtime API      |  | - Status API   | |
|  +-------------------+  +--------------------+  +----------------+ |
|                                                                    |
|  +---------------------------------------------------------------+ |
|  |                    shared (Go library — 8 buckets)             | |
|  |                                                                | |
|  | audit/          - Audit emit primitive + event shape           | |
|  | core/           - Cross-cutting primitives (errors, ids, time) | |
|  | identity/       - Org / project / user model + scoping helpers | |
|  | policy/         - Hook interface + registry + built-in impls   | |
|  |                   (PII, keyword, content safety, rate limit,   | |
|  |                    request size, IP access), pipeline executor | |
|  | schemas/        - Hand-maintained Go struct mirrors of Prisma  | |
|  |                   (configkey, configtypes, enums)              | |
|  | storage/        - SpillStore backends (localfs, s3)            | |
|  | traffic/        - Adapter framework, domain/path matching,     | |
|  |                   content extraction (OpenAI, generic JSONPath)| |
|  | transport/      - thingclient, mq (NATS), normalize codecs     | |
|  +---------------------------------------------------------------+ |
+-------------------------------------------------------------------+
```

## Data Flow by Interception Mode

### AI Gateway Flow

```
Client App
  |
  |  POST /v1/chat/completions (Authorization: Bearer vk-...)
  v
AI Gateway
  1. VK authentication (HMAC verify, lookup project/org scope)
  2. Rate limit check (Redis distributed / local fallback)
  3. Quota check (per-project, per-VK)
  4. Hook pipeline -- request stage (PII, keyword, content safety, webhooks)
  5. Routing engine (match rules -> strategy tree -> resolve target)
  6. Credential decryption (AES-256-GCM, in-memory only)
  7. Upstream request to AI provider
  8. Hook pipeline -- response stage (quality check, content safety)
  9. Audit record (async batch write to PostgreSQL)
  10. Response to client
```

### Compliance Proxy Flow

```
Client App (configured with HTTPS proxy)
  |
  |  CONNECT api.openai.com:443
  v
Compliance Proxy
  1. Access control (source IP allowlist, domain allowlist)
  2. Connection manager (concurrency limits)
  3. Pinning check + exemption evaluation
  4. TLS bump: issue dynamic cert from local CA
  5. Domain + path matching (InterceptionDomain rules)
  6. Content extraction via traffic adapter (OpenAI-compat or generic JSONPath)
  7. Hook pipeline -- request stage (same shared hooks)
  8. Forward to upstream provider
  9. Hook pipeline -- response stage
  10. Audit event emission (async, with SIEM forwarding)
  11. Response back through TLS tunnel to client
```

### Desktop Agent Flow

```
Developer Workstation
  |
  |  OS-level network interception (platform-specific)
  v
Desktop Agent
  1. Policy engine evaluation (inspect / passthrough / deny)
  2. Exemption store check (auto-exempt on repeated TLS failures)
  3. Hook pipeline -- request stage (shared compliance code)
  4. Forward to destination
  5. Audit event recorded to local SQLCipher database
  6. Batch drain: upload audit events to control plane (mTLS)
  7. Config sync: pull updated policies + hook configs (ETag-based)
```

## Config Propagation Model

```
Dashboard UI (React)
      |
      | REST API call (OAuth+PKCE bearer)
      v
Control Plane (Go + Echo)
      |
      | 1. Validate + persist via Hub HTTP API
      v
Nexus Hub (platform ops center)
      |
      | 1. Write to PostgreSQL (durable)
      | 2. Update each node's target config (config sync record)
      | 3. Emit change-signal over WebSocket to affected nodes
      v
  +---+---+---+
  |   |   |   |
  v   v   v   v
Data plane nodes (AI Gateway / Compliance Proxy / Agent)
  - Receive change-signal (no full payload pushed)
  - PULL updated config keys from Hub (Category B keys carry needsPull: true)
  - Apply locally (atomic swap of hook config / routing rules / credentials)
  - Report applied-config version back to Hub
  - Out-of-sync detection: differences between target and applied config are surfaced for operators
```

This is a **pull-only** model: the Hub never pushes full state. The change-signal triggers a fetch, the node applies the new config, and the apply receipt closes the loop. Configuration **invalidation is not Redis pub/sub** — Redis is used for caching only (sessions, IAM, quota counters, response cache, cert cache). All five services — Hub itself, Control Plane, AI Gateway, Compliance Proxy, Agent — follow the same config-sync contract.

## Technology Summary

| Component | Technology | Purpose |
|-----------|-----------|---------|
| Control Plane | Go 1.25+, Echo framework | Admin API, agent API, BFF, IAM, scheduled jobs |
| Dashboard UI | React, TypeScript, Vite | Configuration management, analytics, monitoring |
| AI Gateway | Go 1.25+, net/http | SDK proxy for /v1/* AI API traffic |
| Compliance Proxy | Go 1.25+, CONNECT proxy | Transparent TLS-intercepting HTTPS proxy |
| Desktop Agent | Go 1.25+ | OS-level traffic interception (macOS, Windows, Linux) |
| Shared Library | Go | Compliance hooks, pipeline, traffic adapters, config types |
| Database | PostgreSQL | Configuration, audit logs, credentials, IAM policies |
| Cache | Redis | Session store, IAM cache, distributed rate limiting, cert cache, response cache, quota counters. **Cache only — no pub/sub.** |
| Message Queue | NATS JetStream (via `shared/mq`) | Traffic events, audit events, ops metrics, MQ-coordinated Hub consumers |
| Encryption | AES-256-GCM | Credential vault, agent audit database (SQLCipher) |
| Metrics | Prometheus | Per-service metrics (hook latency, pipeline decisions, request rates) |
| Observability | OpenTelemetry (agent) | Distributed tracing for audit upload and agent operations |
| Agent Auth | mTLS (X.509 client certs) | Agent-to-control-plane authentication |
| AI Providers | Shipped + smoke-tested adapters: OpenAI, Anthropic, Gemini, Moonshot, DeepSeek. Adapters shipped pending production verification: Azure OpenAI, GLM, MiniMax. See roadmap `docs/developers/roadmap.md` §E72 ("AI Gateway adapter verification") for the production-readiness gating. | Upstream AI model providers |
