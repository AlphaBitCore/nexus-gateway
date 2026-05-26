# Nexus Gateway

Nexus Gateway is an enterprise AI traffic gateway. It sits between every application or endpoint that calls a large language model and the actual LLM provider, and runs that traffic through one compliance engine, one audit pipeline, and one control plane.

Nexus intercepts LLM traffic at three independent layers — the application SDK layer, the network layer, and the operating-system layer of developer endpoints — so a single organization can apply unified policy, observability, cost control, and access management across every way employees and applications consume AI.

## Three intercept layers

Nexus operates three independent intercept paths. Each path runs the full compliance pipeline on its own traffic and egresses directly to the upstream LLM provider.

| Layer | Where it intercepts | Code |
|---|---|---|
| **AI Gateway** | SDK layer — virtual keys on `/v1/chat/completions`, `/v1/responses`, `/v1/embeddings`, `/v1/messages` | `packages/ai-gateway/` |
| **Compliance Proxy** | Network layer — transparent TLS bump (HTTPS `CONNECT` plus MITM) | `packages/compliance-proxy/` |
| **Desktop Agent** | Operating-system layer — packet-level capture on macOS, Linux, and Windows endpoints | `packages/agent/` |

The shared hook pipeline lives in `packages/shared/policy/hooks/`; each intercept path invokes it on its own traffic.

The Desktop Agent always egresses to the upstream provider directly. When enterprise network policy routes Agent traffic through the Compliance Proxy on the way out, the Agent stamps an Ed25519-signed `X-Nexus-Attestation` header on the outbound request. The Compliance Proxy verifies this header before the TLS bump (`packages/shared/transport/tlsbump/`); on a valid signature, the connection passes through without MITM, without re-running hooks, and without producing a duplicate audit record, because the Agent already ran them.

## What Nexus manages

The control plane manages a catalog of objects that together define traffic-plane behavior. `tools/db-migrate/schema.prisma` is the source of truth for object shapes.

**Tenancy and identity.** **Organization** and **Project** form the tenant hierarchy. Quotas, virtual keys, and routing rules are scoped to one or both. **User** and **IAM Group** are principals. **IAM Policy** documents grant permissions through group attachment or direct attachment, using NRN (Nexus Resource Name) resource identifiers.

**Upstream connectivity.** **Provider** is an upstream LLM service such as OpenAI, Anthropic, Gemini, Vertex, Azure, Bedrock, or Cohere. **Credential** holds the encrypted provider API key. **Model** and **Model Pricing** describe what each provider exposes and how usage is priced.

**Traffic shaping.** **Virtual Key** is the SDK-layer authentication token; each virtual key scopes which models the caller may use. **Routing Rule** decides which provider and model serves a request, based on conditions and an active routing strategy. **Quota Policy** and **Quota Override** define usage limits per organization, project, virtual key, provider, or model.

**Compliance.** **Hook Config** is one compliance check in the pipeline (PII detection, content safety, keyword filter, classification, rate limit, IP allowlist, webhook forwarder). **Rule Pack** bundles compliance rules and is attached to hooks via **Rule Pack Install**. **Interception Domain** decides which network destinations the Compliance Proxy inspects, passes through, or blocks. **Compliance Exemption Grant** and **DSAR Request** track formal carve-outs and data-subject access requests.

**Fleet.** Each endpoint where the Desktop Agent runs registers as a node in the Hub registry. **Device Group** organizes endpoint nodes; **Device Assignment** binds an endpoint node to a user.

**Observability.** **Alert Rule** and **Alert Channel** define what gets detected and where notifications go. **Traffic Event** records every request handled by any of the three intercept paths; one row per request, joined to metric rollups for analytics.

## What Nexus does

### Provider abstraction
Applications speak the OpenAI SDK shape. Nexus normalizes every request to a canonical OpenAI shape, then translates the wire format on the way to the actual provider. Twenty in-tree adapter codecs ship today, in `packages/ai-gateway/internal/providers/specs/`:

- **First-class codecs (11)** with full bidirectional translation: `openai`, `anthropic`, `gemini`, `vertex`, `azure`, `bedrock`, `cohere`, `minimax`, `glm`, `replicate`, `voyage`.
- **OpenAI-compatible passthrough (9)**: `deepseek`, `moonshot`, `mistral`, `groq`, `fireworks`, `together`, `perplexity`, `xai`, `huggingface`.

Reasoning tokens, function calls, vision inputs, and structured outputs survive the translation.

### Multi-tier cache
- **Exact-match response cache** backed by Valkey (Redis-wire-compatible).
- **Semantic vector cache** via the `valkey-search` module (`packages/ai-gateway/internal/cache/semantic/`).
- **Provider-native cache accounting** surfaces upstream cached-token counts in billing when the provider reports them.
- **In-flight single-flight** folds concurrent identical prompts into one upstream call.

### Cost and quota control
- Multi-axis quotas: per organization, per virtual key, per provider, per model.
- Token-based or USD-based budgets.
- Hard limits reject with `429`; soft limits fire alerts.
- Real-time accounting: counters update on every traffic event, with no batch lag.
- Seven routing strategies in `packages/ai-gateway/internal/routing/strategies/`: `single`, `fallback`, `loadbalance`, `conditional`, `absplit`, `policy`, `smart`.

### Compliance pipeline
PII detection, data classification, keyword filtering, content safety, rate limiting, IP allowlists, request-size validation, webhook forwarders, per-stage audit (request hooks and response hooks recorded independently), body capture (inline up to 256 KiB, the remainder in spill storage at `packages/shared/storage/spillstore/`), SIEM forwarding (`packages/nexus-hub/internal/observability/siem/`), three-tier kill switch, and emergency passthrough.

### Modalities
Chat, embeddings, structured outputs, function and tool calling, vision input, and reasoning tokens.

### Enterprise governance
- **IAM** with RBAC and ABAC over NRN resource identifiers (`packages/shared/identity/iam/`).
- **Virtual keys** with per-key model scope.
- **OIDC federation** with just-in-time user provisioning.
- **Organization and project hierarchy** with per-organization quota.
- **Credential vault** with AES-256-GCM and key rotation (`packages/control-plane/internal/platform/crypto/aes_gcm.go`).
- **Agent fleet management** with a node CA, target-config sync, and out-of-sync detection.

## Architecture in one minute

Five Go services and one React control console make up Nexus.

| Component | Port | Purpose |
|---|---|---|
| **Nexus Hub** | 3060 | Node registry, target-config store, config sync, scheduled jobs, agent CA, SIEM bridge |
| **Control Plane** | 3001 | Admin API and BFF, IAM, SSO, analytics |
| **AI Gateway** | 3050 | `/v1` AI traffic, provider adapters, routing, quota |
| **Compliance Proxy** | 3128 | HTTPS `CONNECT`, MITM TLS bump, compliance pipeline |
| **Desktop Agent** | local | Endpoint traffic interception on macOS, Linux, and Windows |
| **Control Plane UI** | 3000 | React + Vite + TypeScript admin dashboard |

All four backend services register with the Nexus Hub as nodes through `packages/shared/transport/thingclient/` (WebSocket primary, HTTP fallback) and pull configuration from the Hub on boot and on change-signal. The Hub never pushes full state.

**Storage.** PostgreSQL 16 holds durable state. Prisma is the dev-time source of truth for migrations; runtime code uses hand-written SQL with `pgx`. Valkey 8 is Redis-wire-compatible and BSD-licensed; Nexus uses it for sessions, the IAM cache, rate limiting, the response cache, the target-config cache, quota counters, and the semantic vector cache module. NATS JetStream carries event streams and Hub coordination through `packages/shared/transport/mq/`.

For the user-facing architecture diagram and detailed boundaries, see [architecture.md](./architecture.md).

## Who uses Nexus

Nexus serves four roles, each defined by the Control Plane UI surface they reach.

**Administrator and Compliance Officer.** Reaches the Control Plane UI sections that define policy: Providers and Credentials, Routing Rules, Virtual Keys, Hooks and Rule Packs, Quota Policies, Identity Providers, IAM Policies, and Alerts.

**Fleet Operator.** Reaches the Control Plane UI Infrastructure section: Nodes, Overrides, Config Sync, Scheduled Jobs, Kill Switch, Recent Errors, Crash Reports, Agent Diag Mode, Proxy Rollout, Agent Setup, Observability, Observability Retention, and SIEM.

**Developer and Application Owner.** Holds a Personal Virtual Key and calls the AI Gateway on `/v1`. Reaches the Personal Virtual Keys section (`packages/control-plane-ui/src/pages/account/personal-vks/`) and the AI Gateway Simulator (`packages/control-plane-ui/src/pages/tools/`).

**Endpoint User.** Has AI traffic intercepted by either the Compliance Proxy (when network policy routes traffic through it) or the Desktop Agent (when one is installed on the endpoint). Does not interact with the Control Plane UI directly.

## Where to go next

- [features.md](./features.md) — capability matrix grouped by domain.
- [architecture.md](./architecture.md) — user-facing architecture with the traffic-plane diagram and the control-plane summary.
- [deployment-models.md](./deployment-models.md) — SaaS, self-host, and air-gapped deployment options.
- `docs/users/features/cp-ui/` — one document per Control Plane UI section.
- `docs/users/features/agent-ui/` — one document per Desktop Agent UI section.

## References

- `packages/ai-gateway/` — `/v1` AI traffic, provider adapters, routing, quota
- `packages/compliance-proxy/` — HTTPS CONNECT, MITM TLS bump, compliance pipeline
- `packages/agent/` — desktop traffic interception
- `packages/nexus-hub/` — node registry, target-config store, config sync
- `packages/control-plane/` — admin API, IAM, SSO, analytics
- `packages/control-plane-ui/` — React admin dashboard
- `packages/shared/transport/thingclient/` — service-to-Hub registration and config-sync client
- `packages/shared/transport/tlsbump/` — Compliance Proxy TLS bump and attestation verification
- `packages/shared/identity/iam/` — NRN-based RBAC and ABAC
- `packages/shared/transport/mq/` — NATS JetStream binding
- `packages/shared/policy/hooks/` — shared compliance hook pipeline
- `packages/shared/storage/spillstore/` — request and response body spillover storage
- `packages/ai-gateway/internal/providers/specs/` — in-tree provider adapter codecs
- `packages/ai-gateway/internal/cache/semantic/` — semantic vector cache module
- `packages/ai-gateway/internal/routing/strategies/` — routing strategy implementations
- `packages/control-plane/internal/platform/crypto/aes_gcm.go` — credential vault AES-256-GCM
- `packages/nexus-hub/internal/observability/siem/` — SIEM forwarder
- `tools/db-migrate/schema.prisma` — Prisma schema, source of truth for object shapes
