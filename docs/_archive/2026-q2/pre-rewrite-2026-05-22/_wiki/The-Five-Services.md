# The Five Services

*Audience: contributors orienting to the codebase and evaluators understanding inter-service responsibilities.*

Nexus Gateway is composed of five Go services plus a React SPA — not three. The familiar "AI Gateway / Compliance Proxy / Agent" triad names the three traffic interception paths, but every traffic path is supervised and configured by a central platform tier (Hub + Control Plane). Each service has a precise boundary: it does one job, exposes one set of interfaces, and communicates with the others through the Hub-centric Thing model contract. This page covers each service's purpose, port, package location, internal responsibilities, and primary call contracts. The closing section summarizes who calls whom at runtime.

---

## Nexus Hub `:3060`

**What it does.** Hub is the platform operations center. It is the authoritative source of truth for the entire platform's operational state.

Hub owns:
- **Thing registry** — the roster of all online services and agents, their types, status, and extension metadata.
- **Device shadow** — desired config and reported config for every Thing, stored as JSONB columns on the `thing` row in PostgreSQL.
- **Config-sync orchestration** — change-signal dispatch over WebSocket when an admin saves a config change via the Control Plane.
- **Scheduled jobs** — the `jobs` framework runs periodic tasks including `kill_switch.reconcile` (auto-revert expired passthrough rows every 60 seconds) and others.
- **Agent CA** — a self-hosted ECDSA P-256 certificate authority. Issues device certificates to Desktop Agents on enrollment via CSR.
- **SIEM bridge** — optional fan-out of audit events to external SIEM systems (webhooks, OTEL, etc.).
- **Alert evaluation** — evaluates `BuiltinRules` and DB `AlertRule` rows against traffic and system events.
- **Audit sink** — the Hub-side consumer of the NATS audit stream; writes records to PostgreSQL.
- **Metrics pipeline** — aggregates metrics samples from all Things.

Hub is the only service that **writes** to the device shadow `desired` field. When an admin saves a routing rule, updates a hook config, or flips the kill switch, the Control Plane forwards the write to Hub via an HTTP API; Hub persists to PostgreSQL, increments the config key's version counter, and emits a change-signal over the affected Things' WebSocket sessions.

**Hub is itself a Thing** in its own registry. This is intentional — the same shadow contract that applies to AI Gateway or the Desktop Agent applies to Hub, giving uniform observability and a single config model across the entire platform.

**Package:** `packages/nexus-hub`

**Primary interfaces:**
- Hub HTTP API (`:3060/api/hub/...`) — Thing CRUD, shadow CRUD, audit upload, spillstore presign; consumed by the Control Plane and all data-plane services.
- Hub WebSocket — change-signal push and heartbeat for every registered Thing.
- NATS JetStream — MQ consumer group coordinator; Hub operates the audit-sink and metrics-sink consumers.

---

## Control Plane `:3001`

**What it does.** The Control Plane is the admin API and backend-for-frontend (BFF) for the dashboard UI. Every admin operation — creating a virtual key, defining a routing rule, enabling a hook, managing users — flows through the Control Plane's admin REST API.

The Control Plane owns:
- **IAM** — resource/action/NRN-based policy evaluation; the `iamMW` middleware guards every admin endpoint.
- **OAuth+PKCE admin authentication** — the Control Plane acts as the OAuth Authorization Server for local admin accounts.
- **SSO/IdP federation** — SAML/OIDC federation with external IdPs (Okta, Azure AD). Nexus is always the SP; the external IdP is never a peer. JIT user provisioning on first successful federation.
- **All admin CRUD** — credentials, virtual keys, routing rules, hook configs, IAM policies, provider catalog, model catalog, alerts, SIEM forwarder settings.
- **Analytics queries** — cost, traffic, cache savings summaries against `traffic_event` and related tables.
- **CP-side drift watchdog** (`configreconcile`) — periodically compares the Control Plane's source-of-truth config tables against `thing.desired.<key>` and re-emits `Hub.NotifyConfigChange` to heal divergence.

The Control Plane is **stateless** — it holds no authoritative config state itself. Every config write is forwarded to Hub. Every query runs directly against PostgreSQL. Admin session state lives in Valkey.

The Control Plane is itself a Thing registered with Hub (type `control-plane`), so it participates in the same health-monitoring and config-sync model as the data-plane services.

**Package:** `packages/control-plane`

**Primary interfaces:**
- Admin REST API on `:3001` — consumed by the Control Plane UI and the `cp_curl` dev helper; gated by `iamMW`.
- Hub HTTP API calls — forwards shadow writes, Thing management operations, audit queries.

---

## AI Gateway `:3050`

**What it does.** The AI Gateway serves the `/v1/*` API surface and is the primary integration point for applications that want programmatic AI access with compliance, routing, and cost tracking.

The AI Gateway owns:
- **Provider adapter layer** — 50+ adapters covering OpenAI, Anthropic, Google Gemini, Azure, Bedrock, Vertex, and a long tail of OpenAI-compatible providers and consumer surfaces, under `packages/ai-gateway/internal/providers/specs/<name>/`.
- **Canonical bridge** — translates between the OpenAI chat-completions canonical shape and each provider's wire format. `canonicalbridge.IngressChatToCanonical` is the required entry point for cross-format routing.
- **Routing engine** — 7 strategies: single, load-balance, fallback chain, conditional, A/B split, policy-narrowing, smart LLM-dispatch. Produces a `ResolvedRequest` that carries the provider, model, credential reference, and bypass flags.
- **Hook pipeline** — request-stage and response-stage hooks, streaming compliance modes (`passthrough`, `buffer_full_block`, `chunked_async`).
- **Prompt cache** — Anthropic explicit `cache_control` blocks, OpenAI automatic prefix caching, Google `contextCache`.
- **Response cache** — exact-match Valkey layer. A cache hit replays the stored response; cost is attributed correctly.
- **Quota enforcement** — per-Virtual-Key sliding-window rate and cost limits backed by Valkey counters.
- **Cost stamping** — `metrics.CalculateCost(usage, prices)` stamps every `traffic_event` row with per-component USD cost.
- **Emergency passthrough** — reads Cat A shadow blob; applies `bypassHooks`, `bypassCache`, `bypassNormalize` flags per request.

**Package:** `packages/ai-gateway`

**Primary interfaces:**
- `/v1/chat/completions`, `/v1/messages`, `/v1/responses`, `/v1/embeddings`, `/v1/models` — consumed by applications via Virtual Key bearer auth.
- Hub WebSocket — config change-signal receiver; pulls hook configs, routing rules, credentials via Cat B shadow.
- NATS JetStream — emits `traffic_event` and `audit_event` messages.

---

## Compliance Proxy `:3040`

**What it does.** The Compliance Proxy is a transparent TLS-intercepting forward proxy. Applications configure their HTTPS proxy setting to point at `:3040`; no SDK changes are required.

The Compliance Proxy owns:
- **CONNECT handling** — receives HTTP CONNECT requests, checks source-IP and domain allowlists.
- **TLS bump** — dynamically mints a leaf certificate from a local CA (LRU + Valkey cert cache) and establishes separate TLS sessions with the client and the upstream.
- **Traffic adapter detection** — matches the `interception_domain` ruleset to select the appropriate traffic adapter from the shared `packages/shared/traffic/adapters/` registry.
- **Hook pipeline** — same `packages/shared/policy/hooks` as the AI Gateway; same three stages (request, response, per-chunk streaming).
- **Text-first normalizer** — for consumer-surface traffic (ChatGPT web, Claude.ai, Cursor IDE), the required output is readable text. The Tier-2 `NonJSONDetector` framework (`packages/shared/transport/normalize/extract/detector.go`) handles binary and non-JSON wire formats.
- **SIEM bridge integration** — optionally fans out audit events to external SIEM systems.

**Package:** `packages/compliance-proxy`

**Primary interfaces:**
- HTTP CONNECT proxy on `:3040` — consumed by any application with an HTTPS proxy setting.
- Hub WebSocket — config change-signal receiver; pulls hook configs and domain/device predicates.
- NATS JetStream — emits `traffic_event` and `audit_event` messages.

---

## Desktop Agent (local)

**What it does.** The Desktop Agent is an endpoint binary that intercepts AI-bound outbound traffic at the OS network layer. Once a flow is intercepted, it runs a forwarding pipeline structurally identical to the Compliance Proxy.

The Desktop Agent owns:
- **OS-level intercept** — macOS `NETransparentProxyProvider` (Network Extension), Linux `iptables` transparent proxy, Windows WinDivert.
- **Forwarding pipeline** — intercept → req_hooks → upstream_ttfb measurement → upstream_total measurement → resp_hooks. The agent measures upstream TTFB and total latency the same way the Compliance Proxy does; it is a full forwarding proxy, not a passive sniffer.
- **Local hook pipeline** — same `packages/shared/policy/hooks` code as the server-side services. Note: as of the 2026-05-14 compliance audit, the agent runs hooks locally but does not yet consult exemptions delivered via shadow, and does not consume rule packs. Server-side enforcement remains the effective compliance layer for agent-intercepted traffic until this wires up.
- **SQLCipher audit queue** — encrypted local store drained over mTLS to Hub. Survives connectivity interruptions; the device picks up on reconnect.
- **Enrollment + mTLS** — on first boot, receives a one-time enrollment token, submits a CSR to Hub, and stores the issued device cert in the platform keystore.
- **Auto-updater** — verifies Ed25519 signatures on update packages before applying.
- **Config sync** — same pull-only shadow contract as server-side services, using the mTLS WebSocket connection to Hub.

The macOS Network Extension (`NETransparentProxyProvider`) is safety-critical: it sits in every outbound packet path on the host, and a misbehaving provider takes down DNS, DHCP, mDNS, NTP, Apple Push, and VPNs. The five fail-open invariants are mandatory; see [Fail Open Posture](Fail-Open-Posture) for the full contract.

**Package:** `packages/agent`

**Primary interfaces:**
- Hub mTLS WebSocket — config change-signal receiver, heartbeat, audit upload.
- Local OS intercept layer — macOS NE / Linux iptables / Windows WinDivert.

---

## Who calls whom

The Control Plane and Hub form the control tier: the Control Plane is the only admin-facing entry point, and it writes everything through Hub. The three data-plane services (AI Gateway, Compliance Proxy, Desktop Agent) register with Hub on boot and pull config from it; they do not call each other or the Control Plane at runtime. The Control Plane optionally proxies the Compliance Proxy's `/runtime/*` admin API for incident-response operations.

At steady state: the admin UI calls the Control Plane; the Control Plane calls Hub; Hub signals all Things; Things pull from Hub. AI traffic goes directly from the calling application to the AI Gateway (or Compliance Proxy or Desktop Agent) and then to the upstream provider — Hub and the Control Plane are not in the request-serving path for AI traffic.

## Package locations and entry points

| Service | Package | Go entry point |
|---|---|---|
| Nexus Hub | `packages/nexus-hub` | `cmd/nexus-hub/main.go` |
| Control Plane | `packages/control-plane` | `cmd/control-plane/main.go` |
| AI Gateway | `packages/ai-gateway` | `cmd/ai-gateway/main.go` |
| Compliance Proxy | `packages/compliance-proxy` | `cmd/compliance-proxy/main.go` |
| Desktop Agent | `packages/agent` | `cmd/agent/main.go` |
| Control Plane UI | `packages/control-plane-ui` | `src/main.tsx` (Vite + React) |

All five Go services are built together via `go.work` at the repo root. Each service has its own `go.mod` but resolves `packages/shared/` from the workspace. The `replace` directives in each `go.mod` are sibling-only — they must never point at upstream GitHub paths, otherwise `GOWORK=off` builds silently pull stale GitHub snapshots.

Configuration for each service is loaded from a YAML file (`<service>.dev.yaml` for local development, `<service>.prod.yaml.example` as the production template) and overlaid with environment variables via `bootenv`. Secrets are always env-only — no secret field appears in committed YAML (binding: CLAUDE.md "secrets are env-only").

## Shared `packages/shared/` — the common library

All five services import from `packages/shared/`. The library is organized into eight buckets:

| Bucket | Contents |
|---|---|
| `audit/` | Audit event types, sink interfaces. |
| `core/` | Context types, error primitives, middleware. |
| `identity/` | IAM evaluation, VK resolution helpers. |
| `policy/` | Hook pipeline engine, PII scanner, decision types. |
| `schemas/` | Config types (`configtypes/`), configKey constants, config state types (`credstate/`, `configkey/`). |
| `storage/` | Spillstore interface, DB helper utilities. |
| `traffic/` | Traffic adapter registry (`adapters/`), normalized payload types. |
| `transport/` | Thing client (`thingclient/`), MQ interface (`mq/`), normalize pipeline (`normalize/`). |

The `shared/` API is additive-only once shipped in a released Desktop Agent binary — removing or renaming exported symbols breaks older agent versions that are still installed on endpoints.

## Local development port summary

During local development, each service runs independently. The `./scripts/dev-start.sh` bootstrap script starts the infrastructure (PostgreSQL, Valkey, NATS) via Docker Compose, then each service is started manually or via the run-local skill:

| Service | Command | URL |
|---|---|---|
| Control Plane UI | `npm run dev:control-plane-ui` | http://localhost:3000 |
| Control Plane | `cd packages/control-plane && go run ./cmd/control-plane/ -config control-plane.dev.yaml` | http://localhost:3001 |
| AI Gateway | `cd packages/ai-gateway && go run ./cmd/ai-gateway/ -config ai-gateway.dev.yaml` | http://localhost:3050 |
| Compliance Proxy | `cd packages/compliance-proxy && go run ./cmd/compliance-proxy/ -config compliance-proxy.dev.yaml` | http://localhost:3040 |
| Nexus Hub | `cd packages/nexus-hub && go run ./cmd/nexus-hub/ -config nexus-hub.dev.yaml` | http://localhost:3060 |

All services require the repo-root `.env` file loaded via `bootenv`. Missing env vars (especially `INTERNAL_SERVICE_TOKEN`, `DB_URL`, `REDIS_URL`, `NATS_URL`) cause startup failures with descriptive errors. The `-config <service>.dev.yaml` flag is required — starting without it causes the service to exit immediately.

## Service health and observability

Each service exposes a `/healthz` endpoint for liveness and a `/readyz` endpoint for readiness. Prometheus metrics are available at `/metrics` on each service's port. The CP UI Infrastructure → Nodes page aggregates health across all registered Things in real time.

Hub's scheduler runs a heartbeat-staleness sweep every 30 seconds to compute Thing status transitions (online → degraded → offline). Service status on the Nodes page reflects the last reported heartbeat + applied-config version map.

Log output follows structured slog format (JSON in production, human-readable text in development via `log.format: "text"` in the dev YAML). Log level is controlled per service via the `log.level` YAML key (`debug`, `info`, `warn`, `error`). Debug level activates pre-wired body-level logs in the AI Gateway's adapter layer — useful for investigating upstream request/response formatting but verbose for routine development.

## Compliance Proxy: domain predicate matching

The Compliance Proxy uses a `domain_predicates` config key (Cat B shadow) to decide which domains to bump for TLS inspection. The predicate set is ordered; the first matching rule wins.

| Predicate type | Example | Effect |
|---|---|---|
| Exact match | `api.openai.com` | Always bump |
| Wildcard | `*.anthropic.com` | Bump all subdomains |
| Regex | `^api\\..*\\.com$` | Bump matching domains |
| Passthrough | `*.internal.corp` | Never bump (relay raw CONNECT) |

Domains not matched by any predicate fall through to a default rule: by default, unknown domains are relayed without TLS bumping (safe default — no inspection). Operators can invert this to "bump everything unless explicitly excluded" via a catch-all bump predicate at the bottom of the list followed by explicit passthrough rules for internal domains.

The `domain_predicates` key is a Cat B shadow key and propagates to the Compliance Proxy via the normal change-signal / pull path. Changes take effect on the next new connection to the affected domain — in-flight connections are not re-evaluated mid-stream.

## AI Gateway routing engine: the `ResolvedRequest` struct

The AI Gateway routing engine's single output is a `ResolvedRequest` struct. Every request that completes the routing phase carries one. Its fields determine what the executor does:

```go
type ResolvedRequest struct {
    ProviderID     string          // which provider wins (e.g., "openai", "anthropic")
    ModelID        string          // resolved model slug
    CredentialRef  CredentialRef   // opaque reference; executor calls credstate.Acquire(ref)
    BypassHooks    bool            // from Cat A shadow; true during emergency passthrough
    BypassCache    bool            // from Cat A shadow
    BypassNormalize bool           // from Cat A shadow
    RoutingStrategy string         // for audit: which strategy selected this provider
    RequestContext  *RequestContext // org, project, VK, quota policy
}
```

The bypass flags are not set by the routing rules. They come exclusively from the Cat A emergency-passthrough shadow blob and are applied by the routing engine _after_ strategy resolution. This design ensures that routing rules cannot grant bypass — only an explicit admin action on the kill-switch page can set them.

The `CredentialRef` pattern keeps provider credentials narrowly scoped. The routing engine resolves a credential reference (opaque ID + version), not the plaintext. The executor calls `credstate.Acquire(ref)` at dispatch time; the plaintext key lives in the executor's stack frame for the duration of the upstream call and nowhere else.

## Shared hook pipeline — single source of truth

The hook pipeline in `packages/shared/policy/hooks` is the sole location where compliance enforcement logic lives. All three data-plane services import it at the same version. The consequence: a bug fix in the PII scanner, a new built-in hook type, or a change to the `Approve/Modify/Reject/Abstain` decision model lands identically in the AI Gateway, Compliance Proxy, and Desktop Agent in the same binary release cycle.

The shared policy package layout:

| Sub-package | What it contains |
|---|---|
| `policy/hooks/engine.go` | Pipeline orchestrator: stage loop, decision aggregation, fail-open on evaluation errors. |
| `policy/hooks/pii/` | PII detection scanner with configurable entity types (email, SSN, phone, credit card, custom regex). |
| `policy/hooks/keyword/` | Keyword block list with case-insensitive and regex matching modes. |
| `policy/hooks/webhook/` | Outbound webhook caller with retry logic and response-based decision mapping. |
| `policy/hooks/ratelimit/` | Sliding-window request and cost counters backed by Valkey. |
| `policy/hooks/iplimit/` | Source-IP allowlist/blocklist enforcement. |

Hook types are extensible via the `HookEvaluator` interface. Adding a new built-in hook type means adding a sub-package here and registering it in the engine. Third-party hook logic runs via the webhook evaluator (the engine calls an external endpoint; no in-process plugin support).

## Hub's certificate authority

Hub runs a self-hosted ECDSA P-256 certificate authority for Desktop Agent enrollment. This CA is purpose-built for the platform and is not a public CA. Its properties:

- **Self-hosted.** Hub generates and holds the CA private key. It never shares the CA key with any other service.
- **ECDSA P-256 curve.** Chosen for compact key size, fast signing, and strong security compared to RSA-2048 at equivalent strength.
- **Device certs are per-enrollment.** Each enrollment produces one unique device certificate bound to one `device_id`. Certificate validity is configurable (default 1 year).
- **Revocation via status.** Hub does not implement CRL or OCSP for device certs. Revocation is enforced at the Hub mTLS handshake layer: Hub checks `thing.status` on every new mTLS connection; `revoked` Things are rejected.
- **CA root distribution.** The CA root is pinned in the Desktop Agent binary at build time. Admins can optionally export it for use in other trust stores, but the mTLS path between agents and Hub is the only relying party.

## Config-sync wiring for each service

Each service wires its `OnConfigChanged` callbacks before the WebSocket session is established. The wiring location by service:

| Service | Wiring file | Keys registered |
|---|---|---|
| AI Gateway | `packages/ai-gateway/cmd/ai-gateway/wiring/config.go` | `hooks`, `routing_rules`, `credentials`, `killswitch`, `emergency_passthrough` |
| Compliance Proxy | `packages/compliance-proxy/cmd/compliance-proxy/wiring/config.go` | `hooks`, `domain_predicates`, `killswitch` |
| Desktop Agent | `packages/agent/cmd/agent/wiring/config.go` | `hooks`, `agent_settings`, `killswitch` |
| Control Plane | `packages/control-plane/cmd/control-plane/wiring/config.go` | `iam_policy`, `sso_config` |

A key that is not registered via `OnConfigChanged` will never be applied even if Hub sends a change-signal for it. The order matters: callbacks must be registered before `thingclient.Connect` is called, otherwise the first batch of change-signals after WebSocket establishment may arrive before the handlers are wired.

## Agent platform abstractions

The Desktop Agent is a cross-platform binary. Platform-specific code is isolated behind the `platform.DefaultPaths()` interface, defined in `packages/agent/platform/`. All filesystem paths — log directories, keystore paths, the daemon socket path, the SQLCipher database path — must come from `platform.DefaultPaths()`. Hardcoding `/Library/`, `/var/`, `/etc/`, `/tmp/`, or `C:\` paths in any Go or Swift file is a binding violation (CLAUDE.md agent platform paths abstraction incident 2026-05-13).

Platform-specific interception code lives in:
- `packages/agent/platform/darwin/NexusAgent/` — macOS Swift app and Network Extension
- `packages/agent/platform/linux/` — Linux iptables transparent proxy driver
- `packages/agent/platform/windows/` — Windows WinDivert driver

The Go service side of the agent (hook pipeline, SQLCipher queue, mTLS, Hub sync) is platform-agnostic and lives in `packages/agent/internal/`.

## Hub's scheduled jobs

Hub runs a jobs framework (`packages/nexus-hub/internal/jobs/`) with several built-in periodic tasks:

| Job | Cadence | What it does |
|---|---|---|
| `kill_switch.reconcile` | Every 60s | Auto-reverts expired emergency-passthrough and kill-switch rows. Emits `system:kill_switch.auto_reverted` to the audit log. |
| `thing.staleness_sweep` | Every 30s | Recomputes Thing `status` (online → degraded → offline) based on heartbeat age. |
| `alert.evaluate` | Every 60s (configurable) | Evaluates all active `AlertRule` rows against recent traffic events and system metrics. Emits alert events if thresholds are breached. |
| `metrics.sample` | Per-sample-interval | Aggregates Prometheus metric samples from all Things into the metrics store. |

Job execution is Hub-local — these jobs do not touch data-plane services directly. The `kill_switch.reconcile` job is safety-critical: it is the authoritative expiry enforcement for emergency passthrough and kill-switch activations.

## Control Plane analytics queries

The Control Plane exposes analytics queries for admin dashboards and traffic inspection. These queries run directly against PostgreSQL — there is no analytics-specific cache tier:

- `/admin/analytics/traffic` — aggregate traffic event summaries (request count, token usage, cost, cache hit rate, hook decision breakdown) over configurable time windows and group-by dimensions (org, project, VK, provider, model, path).
- `/admin/analytics/cost` — per-model, per-VK, per-project cost breakdown with time-series granularity.
- `/admin/traffic` — traffic event list with full row-level detail for incident investigation.
- `/admin/traffic/{event_id}` — single traffic event with body retrieval via presigned S3 URL.

Analytics aggregations are computed at query time, not pre-aggregated. For large deployments, the Control Plane admin may configure indexed materialized views; these are outside current scope.

---

## Canonical docs

- [`overview.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/overview.md) — §1 five-service table, §3 control-plane vs data-plane split, §6 three traffic paths
- [`thing-model.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/foundation/thing-model.md) — Thing types, extension tables, enrollment summary, status model
- [`provider-adapter-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md) — AI Gateway adapter layer, §3a canonical format rules

**Adjacent wiki pages**: [Architecture Overview](Architecture-Overview) · [Three Traffic Paths](Three-Traffic-Paths) · [Thing Model And Config Sync](Thing-Model-And-Config-Sync) · [Control Plane Vs Data Plane](Control-Plane-Vs-Data-Plane) · [Canonical Vs Wire Format](Canonical-Vs-Wire-Format)
