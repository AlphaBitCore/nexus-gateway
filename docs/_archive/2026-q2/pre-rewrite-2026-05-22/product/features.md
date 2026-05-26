# Nexus Gateway -- Features

## 1. Traffic Interception

### AI Gateway (SDK Proxy)

Applications send AI API requests to the gateway's OpenAI-compatible `/v1/chat/completions`, `/v1/embeddings`, and `/v1/models` endpoints using virtual keys instead of raw provider credentials. The gateway authenticates the request, runs the compliance hook pipeline, resolves the target provider via the routing engine, and proxies the request upstream. Supports streaming responses.

**Deployment modes**: AI Gateway only.

### Compliance Proxy (Transparent TLS Proxy)

A CONNECT-based HTTPS proxy that performs TLS interception (bump) on outbound AI traffic. Applications configure it as their HTTP proxy -- no code changes needed. The proxy intercepts TLS connections, issues dynamic certificates from a local CA, inspects request and response bodies, and runs the same compliance hooks as the AI Gateway. Includes configurable domain allowlists, source IP allowlists, connection limits, and pinning-aware exemptions for domains that reject interception.

**Deployment modes**: Compliance Proxy only.

### Desktop Agent

A Go binary installed on developer workstations that intercepts AI-bound network traffic at the OS level. **macOS is the production-validated platform** (pf-mode, E74); Linux (iptables) and Windows (WinDivert) backends are scaffolds in development and not yet production-ready — see `docs/developers/architecture/services/agent/agent-forwarder-architecture.md` §"Platform support matrix" lines 41–46. The agent enrolls with the control plane via mTLS certificate exchange, syncs policy and hook configurations, enforces compliance locally, and batches audit events for upload. Includes a local status API for native GUI integration, crash-loop detection, and auto-update capability.

**Deployment modes**: Desktop Agent only.

## 2. Compliance Hooks

The hook pipeline is the core enforcement mechanism. Each hook is a Go implementation that inspects an `InterceptedTransaction` (containing normalized request content, source IP, target host, headers, and body) and returns a decision: Approve, Reject (hard or soft), Modify, or Abstain.

### Built-in Hooks

| Hook | What It Does |
|------|-------------|
| **PII Detector** | Scans request content for personally identifiable information using regex patterns + optional Luhn validation; on match, emits the pipeline decision configured via `onMatch` (block-hard, block-soft, redact, or approve). Implementation: `packages/shared/policy/hooks/validators/pii_detector.go:30-35,156-204`. Data classification labels (`PUBLIC` / `INTERNAL` / `CONFIDENTIAL` / `RESTRICTED`) are a separate dimension consumed by downstream hooks such as `data-residency` (`packages/shared/policy/hooks/access/data_residency.go:33-65`); the detector does not assign them. |
| **Keyword Filter** | Blocks or flags requests containing configured keyword patterns. Supports exact match and pattern-based filtering. |
| **Content Safety** | Evaluates content against safety policies. |
| **Rate Limiter** | Enforces per-source request rate limits. The shared `rate-limiter` hook (`packages/shared/policy/hooks/ratelimit/rate_limiter.go:31`) uses process-local `sync.Map` counters; the AI Gateway's separate `internal/policy/ratelimit` limiter (`packages/ai-gateway/internal/policy/ratelimit/limiter.go:9-25`) dispatches to Redis with automatic local-fallback for distributed enforcement across replicas. |
| **Request Size Validator** | Rejects requests exceeding configured body size limits. |
| **IP Access Filter** | Restricts access based on source IP allowlists/denylists. |
| **Webhook Forward** | Forwards transaction data to an external webhook endpoint for custom compliance evaluation. (AI Gateway only.) |
| **Quality Checker** | Evaluates response quality against configured criteria. (AI Gateway only.) |

### Pipeline Behavior

Hooks execute in priority order (configurable per hook). The pipeline supports both sequential execution (with short-circuit on hard reject) and parallel execution. Each hook has configurable timeout, fail-open/fail-closed behavior, and per-ingress applicability (a hook can apply to all modes, or only to specific ones like Compliance Proxy or AI Gateway).

Pipeline results are aggregated: the first hard reject wins; any soft reject (without a hard reject) produces a soft rejection; all approves yield approval. Data classification is tracked as the highest sensitivity level across all hooks.

Both the request-stage and the response-stage pipelines are recorded independently on each audit row (`request_hook_decision` / `request_hooks_pipeline` and `response_hook_decision` / `response_hooks_pipeline`). Investigators can see exactly which hooks ran in which stage and which one made the call, including a per-stage `blocking_rule` attribution when a rule pack rejected the traffic.

### Streaming Compliance Modes

Streaming responses (Server-Sent Events) are handled per-host (via `interception_domain` for Compliance Proxy and Agent) or per-provider (via the `Provider` table for AI Gateway) with three selectable modes:

- **passthrough** — relay only, no hook, no body capture. Use for non-AI traffic that is allowed through but should not be inspected.
- **buffer_full_block** — assemble the full extracted prompt + completion before forwarding any byte to the client. The response-stage hook runs once at stream end; on hard reject the proxy returns HTTP 451 and never forwards the upstream body. Trades real-time UX for the ability to block.
- **chunked_async** — relay bytes to the client in real time and asynchronously accumulate the extracted content in chunks. The response-stage hook runs per chunk and once at stream end. Cannot stop bytes already sent, but produces a complete audit trail and triggers post-hoc alerting when content violates policy.

Each scope additionally chooses its `fail_behavior` (`fail_open` or `fail_close`) for hook errors, hook timeouts, and oversize buffers.

### Body Capture (Inline + Spill)

When body capture is enabled (per-host or per-provider), the gateway extracts the canonical prompt + completion text from the provider's wire format (per-provider extractor — OpenAI, Anthropic, Gemini, ChatGPT.com web). Two-tier storage keeps the audit hot path bounded:

- Bodies under 256 KiB are written inline into `traffic_event_payload` JSONB columns.
- Larger bodies are written through a pluggable `SpillStore` backend (built-in: local filesystem and S3; planned: Azure Blob / GCS) and the row stores a content-hashed reference. Operators retrieve the original bytes from the admin UI by following the reference.

Captured content round-trips JSON, SSE text, multipart, and binary bytes losslessly — the audit pipeline does not assume any specific encoding.

**Deployment modes**: All three (same shared Go code).

## 3. Intelligent Routing

The routing engine resolves which AI provider and model should handle each request. Routing rules are configured in the dashboard and evaluated as a strategy tree.

### Strategy Types

| Strategy | Behavior |
|----------|----------|
| **Single** | Routes to a specific provider + model. |
| **Fallback** | Tries providers in order; falls back to the next on failure (configurable by HTTP status code). |
| **Load Balance** | Weighted random distribution across providers. Supports sticky routing. |
| **Conditional** | Evaluates match expressions against request context (model type, virtual key, headers) and routes to the matching branch. |
| **A/B Split** | Weighted random split across provider+model pairs for experimentation. |
| **Policy (Narrowing)** | Restricts which providers and models are eligible before other strategies evaluate. |
| **Smart** | Uses an LLM-dispatch router (configurable router provider + model + system prompt) to pick the target model from canonical request semantics. |

Routing rules include match conditions (by model, model type, provider, virtual key, or project) and support fallback chain entries for inline recovery targets. The engine traces every evaluation step for debugging.

**Deployment modes**: AI Gateway only.

## 4. Credential Management

### Credential Vault

Provider API keys are stored encrypted at rest using AES-256-GCM. The control plane manages the encryption lifecycle. When the AI Gateway needs to make an upstream request, it decrypts the credential in memory for the duration of the request.

### Virtual Keys

Virtual keys are proxy credentials issued to applications and developers. Each virtual key maps to an organization/project scope and can restrict which AI models are accessible. Applications authenticate to the AI Gateway with a bearer-format virtual key (carried via `Authorization: Bearer`, `x-nexus-virtual-key`, or provider-conventional headers like `x-api-key` / `api-key` / `?key=` depending on the ingress route); they never see the underlying provider API key. Presented keys are matched server-side against HMAC-SHA256 hashes stored at rest — the raw key value never lands in the database.

**Deployment modes**: AI Gateway (virtual keys), Control Plane (vault management).

## 5. Fleet Management

### Agent Enrollment

Desktop agents enroll with the Nexus Hub using one-time enrollment tokens. The Hub runs a self-issued ECDSA P-256 Certificate Authority and signs the agent's CSR to issue an mTLS client certificate. Enrolled agents authenticate all subsequent API calls with their client certificate. The CA, enrollment token store, and revocation list are all centralised in the Hub.

### Device Groups

Devices are organized into groups for policy targeting. Each group can have its own configuration profile, compliance hook set, and interception domain rules.

### Config Sync

Agents are managed as devices in the Hub registry. The Hub maintains a per-device **config-sync record** (target vs applied config). On a configuration change, the Hub records the new target config and signals the agent over WebSocket; the agent then **pulls** the updated configuration. Agents never receive a full state push — the change-signal triggers an explicit pull, keeping the wire small and the apply logic auditable. Categories of keys (A: inline, B: pull-on-signal, C: template-driven) determine the apply path. Configuration changes propagate without agent restarts.

### Heartbeat and Monitoring

Agents report status to the Hub (version, queue depth, connectivity, applied-config version, out-of-sync detection). Operators can revoke a device, triggering the agent to stop and require re-enrollment. Fleet analytics provide aggregate views of agent health, version distribution, and config-sync status. Note: today's agent receives exemption and rule-pack configs through config sync but does not yet consume them for local enforcement; server-side enforcement covers those policies until that wires up.

**Deployment modes**: Desktop Agent + Control Plane.

## 6. Dashboard and Analytics

### Real-Time Traffic Monitor

Live view of AI API traffic flowing through the gateway. Includes filtering by provider, model, status, virtual key, and time range. Each request shows full audit detail: routing trace, hook decisions, latency, token usage, and data classification.

### Traffic Analytics

Aggregated views of traffic volume, latency distributions, provider usage, model popularity, error rates, and cost estimates. Supports time-range selection and drill-down.

### Audit Log

Searchable audit log of all AI interactions across all deployment modes (AI Gateway, Compliance Proxy, Agent). Includes admin audit trail for configuration changes. Supports SIEM forwarding for integration with external security platforms.

### Compliance Dashboard

Dedicated compliance view showing hook rejection rates, PII detection frequency, data classification distribution, and policy violation trends. Includes a compliance gauge and alert history.

### Metrics Explorer

Prometheus-compatible metrics with dashboard-based exploration. Includes rollup aggregation for long-term trend analysis.

**Deployment modes**: Control Plane UI (all data sources).

## 7. IAM and Access Control

### Policy Engine

AWS IAM-style policy documents with Allow/Deny effects, action patterns (e.g., `admin:ReadProvider`, `admin:CreateRoutingRule`), resource patterns (using Nexus Resource Names), and conditions. Policies are evaluated with deny-overrides: any explicit deny wins over any allow.

### Roles and Groups

Users are assigned to roles and groups. Roles bundle a set of policies. Groups can have policies attached directly. Policy evaluation loads all applicable policies (direct + group-inherited) and caches them with a 60-second TTL.

### Organization and Project Scoping

Resources are scoped to organizations and projects. Virtual keys, quotas, and routing rules are project-scoped. IAM policies can target specific organizations or projects via resource NRNs (Nexus Resource Names).

### Authentication

OAuth 2.0 with PKCE for admin / dashboard login. Bearer tokens are issued by the local authorization server and cached client-side. External Identity Provider federation is currently shipped via **OIDC**; **SAML support is planned** (the `IdPType.saml` enum + `SAMLAdminConfig` / `SAMLClaimConfig` structs are in place, but the runtime AuthnRequest emitter + signed-assertion verifier are pending — see roadmap E87). On first successful federation the user is provisioned just-in-time. Nexus Local is the implicit fallback IdP — it is not a peer.

**Deployment modes**: Control Plane.

## External Identity Provider Federation

Enterprise customers federate logins through their existing IdP. **Today's runtime supports OIDC** (Okta, Azure AD, Google Workspace, or any generic OIDC provider). **SAML support is planned** (the type enum + admin-config structs ship, but the SAML AuthnRequest emitter, signed-assertion verifier, and JIT-provisioning callback are not yet wired — see roadmap E87). Nexus is the SP; on first successful federation a Nexus user is provisioned just-in-time and mapped to roles by IdP assertion claims. Local accounts remain available as a fallback for break-glass scenarios.

**Deployment modes**: Control Plane.

## 8. Alerting and Kill Switch

### Alerting

Configurable alert thresholds on compliance metrics (rejection rates, error rates, latency, provider availability, virtual-key expiry, agent offline, credential health). Alert channels support webhook, SIEM, and email delivery. Built-in rule definitions (Go) are paired with database-managed `AlertRule` rows so admins can extend without code changes; cross-instance state lives in the Hub-managed config-sync layer.

### Kill Switch & Emergency Passthrough

Three-tier emergency mechanism (organization / provider / route) that toggles traffic-handling policy via Hub config sync. When enforcement is killed, the gateway runs a fail-open **Emergency Passthrough** mode (E48): requests bypass hooks, audit trail is still emitted, max 8-hour expiry (default 1 hour), Hub reconciles every 60 s and auto-reverts. State lives in the config-sync layer; activation is accessible through the dashboard and runtime API.

**Deployment modes**: Compliance Proxy + AI Gateway + Agent (kill switch & emergency passthrough), Control Plane + Nexus Hub (alerting configuration and reconciliation).

## 9. Quota Management

Per-organization, per-project, and per-virtual-key quota enforcement. Quotas are tracked in PostgreSQL with Redis-accelerated counters using a sliding-window algorithm. The AI Gateway checks quota before routing and reconciles on completion. Burst allowance and overage handling are configurable per quota policy. Organization hierarchies propagate quotas through ancestor paths so a parent-org cap constrains child orgs.

**Deployment modes**: AI Gateway + Nexus Hub (quota counter store).

## 9.5 Prompt Cache

Two layers cooperate for prompt-cache savings on repeated prefixes:

- **Provider-side cached content** (Anthropic prompt cache, Bedrock-Claude, Gemini cachedContent). The gateway can inject `cache_control` markers on outbound Anthropic / Bedrock-Claude bodies, and `cachedContent` references on Gemini bodies, so the provider returns `cache_creation_input_tokens` / `cache_read_input_tokens` and applies the discount. Injection is **opt-in per provider** via admin config (`providerInjectEnabled` in the cache settings; `packages/shared/transport/wirerewrite/engine.go:252`). If the client already set any `cache_control` field, the gateway short-circuits and forwards the body unchanged (`rule_cache_inject.go:32-44`) — explicit caller intent wins.
- **Nexus exact-match response cache** (Valkey/Redis). Canonical-shape cache key, configurable TTL, surfaced separately under §9.6.

Hit/miss metrics surface in the dashboard. There is no per-request in-memory memoisation tier in the current build.

**Deployment modes**: AI Gateway.

## 9.6 Response Cache

Optional response cache keyed on canonical request body. Suitable for deterministic prompts (embeddings, structured extractors). TTL and key-prefix policy are fleet-wide; configured under Cache settings (one global setting, with optional per-adapter / per-provider tiers — never per route).

**Deployment modes**: AI Gateway.

## 10. Data Classification

Every AI interaction is automatically classified into one of four sensitivity levels based on hook analysis:

| Level | Meaning |
|-------|---------|
| **Public** | No sensitive content detected. |
| **Internal** | Contains internal business information. |
| **Confidential** | Contains confidential data (e.g., customer PII). |
| **Restricted** | Contains highly restricted data (e.g., credentials, financial records). |

Classification rules are configurable through the dashboard. The highest classification from any hook in the pipeline becomes the transaction's classification label, recorded in the audit log.

**Deployment modes**: All three.
