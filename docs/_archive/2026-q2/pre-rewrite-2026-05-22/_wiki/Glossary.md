# Glossary

This glossary defines the key terms used across the Nexus Gateway documentation, wiki, and codebase. Terms are listed alphabetically. For terms that have both an internal (code/DB) name and a user-facing name, the mapping is noted explicitly — this boundary is a binding convention enforced in product text and UI strings.

The IoT-vocabulary boundary is a load-bearing project rule: the internal architecture uses Thing/Shadow/desired/reported/drift; the admin UI, product docs, and this wiki use node/config sync/target config/applied config/out of sync. Contributor-facing pages use internal terms with a parenthetical gloss on first use; user-facing pages use the external terms.

---

## IoT terminology mapping

The following table is the authoritative cross-reference between internal and user-facing vocabulary. Contributor pages (recipes, architecture docs) use the left column; evaluator and operator pages use the right column.

| Internal (code / DB / dev docs) | User-facing (UI / product docs / wiki) |
|---|---|
| Thing | node / service / device (context-dependent) |
| Shadow | config sync |
| desired state | target config |
| reported state | applied config |
| drift | out of sync |
| Cat A / Cat B / Cat C config key | (not surfaced to users) |
| pull-only config sync | config sync (pull model) |
| Thing Registry | service registry |

---

## Terms A–Z

**Adapter** — A Go package under `packages/ai-gateway/internal/providers/specs/<name>/` that translates between the canonical (OpenAI-shaped) request/response format and a specific provider's wire format. Each adapter owns a codec (request/response translation), a stream session (SSE delta handling), and an error normalizer. The canonical format is always the OpenAI shape; non-OpenAI adapters own their full round-trip translation. See `provider-adapter-architecture.md`.

**Agent (Desktop Agent)** — The Go binary installed on developer workstations (macOS, Windows, Linux). It intercepts AI-bound network traffic at the OS level, enforces compliance policies locally, and uploads audit events to Nexus Hub. On macOS the intercept uses `NETransparentProxyProvider` (network-layer, with TLS bump via a bridge socket). On Linux it uses `pf`/iptables transparent redirect. On Windows it uses WinDivert kernel-mode capture. The Desktop Agent is architecturally equivalent to a client-side Compliance Proxy — both execute the same shared hook pipeline code.

**AI Gateway** — The service at `:3050` that handles `/v1/*` AI traffic. It authenticates virtual keys, runs the compliance hook pipeline, resolves the target provider via the routing engine, proxies the request upstream, and writes the audit event. This is "Path A" in the three-traffic-path model.

**Allowlist** — A configured set of bundle IDs (macOS), domain names, or IP ranges that receive special treatment — typically passthrough without inspection or guaranteed inclusion in a routing candidate set. Not to be confused with the `scripts/.coverage-allowlist` (Go test coverage exception list).

**Audit Event** — A record written to the `traffic_event` table in PostgreSQL after every AI interaction. Each audit event includes: source service, provider, model, virtual key, tokens (prompt/completion/reasoning), cost, latency phases (TTFB, upstream total), hook pipeline decisions, data classification label, and optional request/response body reference (inline JSONB or spillstore handle).

**Canonical Format** — The normalized request/response shape used internally across all provider adapters. The canonical format is the OpenAI chat completions shape: `{messages: [...], model: "...", ...}` for requests; `{choices: [...], usage: {...}}` for responses. All provider adapters translate to/from this shape. Extension fields (provider-specific metadata) ride inside `nexus.ext.<provider>.<key>` via `canonicalext` to avoid polluting the canonical contract.

**Compliance Proxy** — The transparent TLS-intercepting forward proxy service at `:3040`. It performs TLS bump on outbound HTTPS connections, extracts the plain-text body, runs the compliance hook pipeline, and relays the bytes to the upstream provider. Applications point their HTTP proxy setting at `http://<host>:3040` — no SDK changes required. This is "Path B" in the three-traffic-path model.

**Control Plane** — The admin API and BFF (Backend for Frontend) service at `:3001`. It owns IAM, OAuth+PKCE authentication, SSO federation, configuration CRUD (providers, models, routing rules, hook configs, credentials, virtual keys, quotas), analytics queries, and alert evaluation. The Control Plane UI (React + TypeScript + Vite) at `:3000` is the admin dashboard that consumes the Control Plane API.

**Drift** (internal) — The gap between a node's target config and applied config. When the Hub-held target config differs from what a service or agent has last reported as applied, the node is "out of sync" (user-facing term). The Control Plane "Config Sync" page surfaces drift status per node.

**Hook** — A Go implementation within the compliance hook pipeline that inspects an `InterceptedTransaction` and returns a decision: Approve, Reject (hard or soft), Modify, or Abstain. Built-in hooks include PII Detector, Keyword Filter, Content Safety, Rate Limiter, Request Size Validator, and IP Access Filter. Hooks execute in configurable priority order with fail-open/fail-closed behavior per hook.

**Hub (Nexus Hub)** — The platform operations center at `:3060`. Hub maintains the Thing Registry (service and agent roster), device shadows (target/applied config state), agent Certificate Authority, audit pipeline (NATS consumer → PostgreSQL drain), scheduled jobs (retention purge, drift checker, cert rotation), SIEM bridge, and Prometheus metrics rollup. All four other server-side services register with Hub as Things and pull configuration via WebSocket.

**IAM** — Identity and Access Management. Nexus implements an AWS IAM-style policy model: Allow/Deny effects, action patterns (e.g., `admin:ReadProvider`), resource NRNs (Nexus Resource Names), and conditions. Policies are attached to users, roles, or groups. Evaluation uses deny-overrides: any explicit deny wins over any allow. Policy decisions are cached with a 60-second TTL in Valkey.

**Ingress** — One of the four intake endpoint shapes the AI Gateway accepts: OpenAI `/v1/chat/completions`, OpenAI `/v1/responses`, Anthropic `/v1/messages`, and Gemini `:generateContent`. Each ingress has its own request decoder and response encoder; the canonical bus runs between them and the provider adapter.

**Kill Switch** — A three-tier (organization / provider / route) emergency mechanism that toggles traffic-handling policy via Hub config sync. When a kill switch is activated, the gateway runs Emergency Passthrough mode — requests bypass hooks, the audit trail is still emitted, and the switch auto-reverts after a configurable maximum duration (default 1 hour, maximum 8 hours) with Hub reconciling every 60 seconds.

**Mode** (theme) — The admin UI visual mode: light, dark, or system. Controlled by the theme system in the Control Plane UI. Separate from deployment mode (AI Gateway / Compliance Proxy / Desktop Agent).

**Node** (user-facing for Thing) — Any managed entity registered with Nexus Hub: a server-side service (AI Gateway, Control Plane, Compliance Proxy) or a desktop agent instance. The Infrastructure → Nodes page in the admin UI lists all registered nodes with their status (online / degraded / offline) and config-sync state. Internally, each node is a "Thing" in the Hub Thing Registry.

**Normalize** — The process of converting a provider's wire-format request or response into the canonical format, or extracting readable prompt/response text for audit storage. The normalization pipeline runs in `packages/shared/transport/normalize/`. Tier-1 normalizers decode known AI provider formats precisely; Tier-2 normalizers use the `NonJSONDetector` framework for formats that are not standard JSON.

**NRN (Nexus Resource Name)** — The resource identifier syntax used in IAM policies. Format: `nrn:<service>:<resource-type>:<org-id>/<project-id?>/<resource-id?>`. NRNs appear in policy `Resource` fields and are constructed by middleware for every incoming admin API request. Drift between UI `allowedActions` and handler `iamMW(...)` produces silent 403 errors — the IAM impact review binding enforces these stay in sync.

**OpenAPI** — The machine-readable API contract format used throughout Nexus. Each epic with an admin or AI Gateway endpoint produces a corresponding OpenAPI 3.1 YAML under `docs/users/api/openapi/`. The Control Plane UI's service layer is generated against these specs; Go route handlers conform to them. See [`docs/users/api/openapi/`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/) for the full catalog.

**Pass-through** — A request or flow that the gateway relays without running compliance hooks. Pass-through happens when: a kill switch is active (Emergency Passthrough), the streaming mode is `passthrough`, the macOS NE extension times out its async callbacks (2-second fail-open), or the request matches an explicit allowlist rule. In all cases the audit trail is still emitted.

**Policy** — An IAM policy document (in the Nexus IAM context) or a compliance enforcement policy (in the hook-pipeline context). Both are configured through the admin UI. IAM policies govern which admin users can take which actions. Compliance policies govern which traffic gets inspected, classified, or blocked.

**Provider** — A row in the `Provider` table representing a configured AI service (OpenAI, Anthropic, Google Gemini, etc.). Each Provider row references a spec adapter package, stores an encrypted credential, and lists associated Model rows. The AI Gateway uses the Provider + Model row pair to resolve the upstream target for each request.

**Quota** — A per-organization, per-project, or per-virtual-key spending or request-count limit. Quotas are tracked in PostgreSQL with Valkey-accelerated sliding-window counters. The AI Gateway checks quota before routing and reconciles on completion. Burst allowance and overage handling are configurable per quota policy.

**Routing Rule** — A declarative rule in the AI Gateway that matches an incoming request (by model ID, model type, provider, virtual key, or project) and resolves it to a target provider/model via one of seven strategy types: Single, Fallback, Load Balance, Conditional, A/B Split, Policy (Narrowing), or Smart (LLM-dispatch). Routing rules are ordered by priority; the first matching rule wins.

**Service** — In Nexus architecture, one of the five Go binaries: Nexus Hub, Control Plane, AI Gateway, Compliance Proxy, and Desktop Agent. In the admin UI, "node" is the user-facing term for a registered service instance.

**Shadow** (internal) — The Hub-side record of a node's desired state and reported state, stored as JSONB columns on the `thing` table row. The shadow is keyed and versioned per config key. User-facing term: "config sync". There is no separate `thing_shadow` table — the shadow has always been JSONB columns on the `thing` row.

**Spec** — Short for "spec adapter" or "provider spec". A directory under `packages/ai-gateway/internal/providers/specs/<name>/` that bundles the codec, stream session, and error normalizer for one provider's wire format. The term is also used for SDD (Software Design Document) files under `docs/developers/specs/`.

**Spillstore** — The pluggable storage backend for AI request/response bodies that exceed 256 KiB inline storage. Bodies spill to S3 (production) or local filesystem (development). The audit row stores a content-hashed reference; operators retrieve the original bytes via a presigned URL. This keeps the PostgreSQL `traffic_event` row bounded while preserving full-fidelity audit data for large prompts.

**SSE (Server-Sent Events)** — The streaming protocol used by AI providers for real-time token delivery. Nexus AI Gateway and Compliance Proxy relay SSE streams through the compliance pipeline with configurable modes (`buffer_full_block` vs `chunked_async`). SSE streams are normalized to the canonical format per ingress — a Gemini streaming response is re-encoded to OpenAI SSE shape before being forwarded to the caller.

**Thing** (internal) — Every managed entity registered with Nexus Hub — a server-side service or a desktop agent — is a "Thing" in the internal data model. The unified Thing abstraction gives all five entity types a single registry, config shadow, heartbeat contract, and enrollment/revocation lifecycle. User-facing term: "node". See the IoT terminology mapping table at the top of this page.

**traffic_event** — The PostgreSQL table that stores every AI interaction audit record. Fields include source (which Nexus service captured the request), kind (e.g., `ai-chat`), endpoint type, provider, model, virtual key ID, token counts, cost, latency phases, hook decisions, data classification label, and spillstore reference. The `TrafficEventNormalized` sidecar table holds extracted plain-text prompt and completion.

**Trust Boundary** — A point in the architecture where authentication or authorization is enforced between two components. Nexus has four primary trust boundaries: (1) the caller → AI Gateway boundary (virtual key authentication); (2) the admin → Control Plane boundary (OAuth+PKCE session + IAM policy); (3) the service → Hub boundary (internal-service token or mTLS device cert); (4) the AI Gateway → provider boundary (encrypted provider API key decrypted in-memory). See `docs/developers/architecture/overview.md` §10 for the full boundary map.

**Virtual Key (VK)** — A proxy credential issued to applications or developers through the Control Plane. Each virtual key maps to an organization/project scope and can restrict accessible models and quotas. Applications authenticate to the AI Gateway with a bearer-format VK (`Authorization: Bearer`, `x-nexus-virtual-key`, or provider-conventional headers); they never see the underlying provider API key. The raw VK string is never stored — only an HMAC-SHA256 hash is persisted.

**Wire Format** — The specific HTTP request/response shape that a provider's upstream API expects or produces — distinct from the canonical format. For example, Anthropic's wire format uses `messages` with `role`+`content` arrays but no `choices` wrapper; the adapter translates between this and the canonical OpenAI shape. Per-model wire quirks (extra fields, stripped fields, streaming deltas) stay in the adapter that talks to that wire, never in `spec_adapter.go`.

---

## Additional terminology

**Breakglass** — An emergency access path for scenarios where normal authentication or policy cannot be used. In Nexus, local accounts act as a breakglass fallback when the federated IdP is unavailable. The admin super-user seeded during initial deployment is the canonical breakglass account.

**Canonical Bus** — The internal data-flow path through which all AI requests pass after ingress decoding and before provider adapter encoding. The bus carries the canonical (OpenAI-shaped) request; any ingress shape can route to any provider because the bus decouples them.

**Config Key** — A named slot in the Hub config-sync system. Each config key maps to a specific piece of configuration (e.g., `hook_config`, `agent_settings`, `kill_switch`). Config keys are classified as Category A (inline in the shadow blob, pushed to Things), Category B (pull-on-signal, Things pull when signaled), or Category C (template-driven, sourced from `thing_config_template`). Category classifications determine the propagation path and apply semantics.

**Device Shadow** (internal) — See Shadow. The term "device shadow" comes from AWS IoT and is used in internal architecture docs; the user-facing term is "config sync".

**Hook Config** — The configuration object (a `HookConfig` row + associated rules) that defines which hooks are active, their ordering, failure behavior, and per-hook parameters. Hook configs are stored in the Control Plane database and propagated to services via the shadow config-sync path. Each service loads its hook config on startup and updates it in-memory when the Hub signals a change.

**Ingress Shape** — See Ingress. Used interchangeably in contexts that emphasize the format distinction (e.g., "the Anthropic ingress shape" means calls arriving at `/v1/messages`).

**Interception Domain** — A `interception_domain` row in the Control Plane that configures the Compliance Proxy or Desktop Agent to intercept traffic to a specific host or domain. Each row specifies the domain, streaming mode, body capture settings, and hook applicability. This is the per-domain configuration surface for the Compliance Proxy and Agent — distinct from provider/model configuration (which is for the AI Gateway).

**MITM (Man-in-the-Middle)** — The TLS interception technique used by the Compliance Proxy and Desktop Agent. The proxy terminates the client's TLS connection, issues a dynamic certificate from the admin-trusted CA, inspects the plain-text body, runs hooks, and re-encrypts to the upstream provider. This is intentional and requires organizations to install Nexus's CA certificate as trusted on managed devices.

**Normalized Payload** — The extracted plain-text form of a request or response body after the Tier-1 normalizer runs. Stored in the `traffic_event_normalized` table as a sidecar to the audit row. The normalized payload is what the admin UI shows in the Traffic Audit drawer's "Normalized" tab.

**Reported State** (internal) — The per-config-key snapshot that a Thing last reported as applied. Stored in the `thing.reported` JSONB column on the Thing row. User-facing term: "applied config".

**Smart Routing** — The "Smart" routing strategy that uses an LLM to pick the optimal target model from the canonical request semantics. The router LLM and system prompt are configurable per routing rule. The latency budget for a smart routing decision is constrained to ≤200 ms p95 (a hard requirement in E78) to avoid the routing decision dominating total request latency.

**Thing Registry** — The Hub-side database of all registered managed entities (server-side services and Desktop Agents). Each row carries status, enrollment metadata, shadow columns, and heartbeat timestamps. User-facing term: "Nodes" (visible in Infrastructure → Nodes in the admin UI).

---

## Canonical docs

- [`docs/developers/architecture/cross-cutting/foundation/thing-model.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/foundation/thing-model.md) — internal Thing model, shadow columns, terminology boundary (§10)
- [`docs/users/product/overview.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/product/overview.md) — user-facing product glossary and capability summary
- [`docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md) — §3a canonical format rules and adapter contract

**Adjacent wiki pages**: [FAQ Product](FAQ-Product) · [Architecture Overview](Architecture-Overview) · [The Five Services](The-Five-Services) · [Thing Model And Config Sync](Thing-Model-And-Config-Sync) · [Canonical Vs Wire Format](Canonical-Vs-Wire-Format)
