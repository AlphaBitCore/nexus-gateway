# Features

This document is the capability matrix for Nexus Gateway. Capabilities are grouped by domain. Each row gives a short description and the Control Plane UI section or API endpoint where the capability is exercised. For the narrative product positioning, see [overview.md](./overview.md).

UI section names match the sidebar labels in `packages/control-plane-ui/src/i18n/locales/en/nav.json`. API endpoints are registered by the AI Gateway in `packages/ai-gateway/cmd/ai-gateway/wiring/routes.go`.

## AI traffic

| Capability | Description | Where to use it |
|---|---|---|
| **Provider adapter codecs (20)** | 11 first-class bidirectional codecs (`openai`, `anthropic`, `gemini`, `vertex`, `azure`, `bedrock`, `cohere`, `minimax`, `glm`, `replicate`, `voyage`) and 9 OpenAI-compatible passthrough codecs (`deepseek`, `moonshot`, `mistral`, `groq`, `fireworks`, `together`, `perplexity`, `xai`, `huggingface`) | Providers & Models |
| **Routing strategies (7)** | `single`, `fallback`, `loadbalance`, `conditional`, `absplit`, `policy`, `smart` | Routing Rules |
| **Virtual Key with model scope** | each Virtual Key restricts which models the caller may invoke | Virtual Keys |
| **Multi-axis quotas** | per organization, per virtual key, per provider, per model | Quota Policies, Quota Overrides |
| **Token-based or USD-based budgets** | quota expressed in tokens or dollars | Quota Policies |
| **Soft and hard limits** | soft limits fire alerts; hard limits reject with HTTP `429` | Quota Policies |
| **Exact-match response cache** | Valkey-backed cache keyed on canonical request | Cache |
| **Semantic vector cache** | `valkey-search` module with similarity-threshold lookup | Cache |
| **Single-flight prompt folding** | concurrent identical prompts fold into one upstream call | (automatic) |
| **Provider-native cache accounting** | upstream cached-token counts surface in billing when the provider reports them | Cache, Analytics & Metrics |
| **Credential vault** | AES-256-GCM encryption with key rotation | Credentials |
| **Credential reliability tracking** | per-credential health and retry telemetry | Credential Reliability |

## Compliance

| Capability | Description | Where to use it |
|---|---|---|
| **Built-in hook implementations (11)** | `keyword-filter`, `pii-detector`, `content-safety`, `rate-limiter`, `request-size-validator`, `ip-access-filter`, `data-residency`, `rulepack-engine`, `noop`, `webhook-forward`, `quality-checker` | Hooks & Policies (type = `builtin`) |
| **Webhook hook** | forward request or response to an external compliance service | Hooks & Policies (type = `webhook`) |
| **Script hook** | inline scripted check | Hooks & Policies (type = `script`) |
| **Hook stages** | `request` and `response` stages recorded independently | Hooks & Policies |
| **Hook tuning** | per-hook `priority`, `timeoutMs`, `failBehavior` (`fail-open` or `fail-closed`), `applicableIngress` filter | Hooks & Policies |
| **Rule Packs** | bundle of compliance rules | Rule Packs |
| **Rule Pack Install** | per-tenant attachment of a Rule Pack to a hook | Rule Packs |
| **Interception Domains** | which network destinations the Compliance Proxy inspects, passes through, or blocks | Interception Domains |
| **Compliance Exemption Grants** | formal carve-outs to specific compliance rules | Exemptions |
| **Data Subject Access Requests** | DSAR request lifecycle | Data Subject Requests |
| **Body capture** | request and response bodies up to 256 KiB inline, the remainder in spill storage | Payload Capture |
| **AI Guard Backend** | external content-safety service integration | AI Guard Backend |
| **Streaming compliance** | hook evaluation on streaming responses | Streaming Compliance |
| **Operation Logs** | per-stage hook execution audit trail | Operation Logs |
| **Compliance Report** | tenant-scoped compliance summary | (Compliance section route `compliance/compliance-report`) |

## Modalities and ingress

| Capability | Description | Where to use it |
|---|---|---|
| **`/v1/chat/completions`** | OpenAI chat-completions ingress | AI Gateway `:3050` |
| **`/v1/responses`** | OpenAI Responses-API ingress | AI Gateway `:3050` |
| **`/v1/embeddings`** | OpenAI embeddings ingress | AI Gateway `:3050` |
| **`/v1/messages`** | Anthropic Messages ingress | AI Gateway `:3050` |
| **Streaming and non-streaming** | per-request `stream=true` or stream-from-path | (per-call) |
| **Function and tool calling** | OpenAI tools schema preserved through codec translation | (per-call) |
| **Vision input** | image attachments preserved through codec translation | (per-call) |
| **Structured outputs** | JSON-schema response format | (per-call) |
| **Reasoning tokens** | provider reasoning fields preserved through codec translation | (per-call) |

## Identity and access

| Capability | Description | Where to use it |
|---|---|---|
| **IAM with RBAC and ABAC** | NRN (Nexus Resource Name) resource identifiers | IAM Policies |
| **IAM Policy attachment** | group attachment and direct attachment | IAM Policies, Roles, Users |
| **OIDC federation** | external IdP integration | Identity Provider |
| **JIT user provisioning** | auto-create a user on first OIDC login | (automatic on first login) |
| **Personal Virtual Keys** | dev-user-owned Virtual Keys for `/v1` calls | `/settings/personal-vks` |
| **Admin API Keys** | service-to-service tokens with rotate verb, multiple-key support, and status lifecycle | Admin console (Settings) |
| **Organization** | tenant root | Organizations |
| **Project** | sub-tenant under an Organization | Projects |
| **IAM Group** | principal grouping for policy attachment | Roles |
| **IAM Simulator** | dry-evaluate a policy decision before saving | Simulator |

## Fleet management

| Capability | Description | Where to use it |
|---|---|---|
| **Endpoint node registration** | each Desktop Agent installation registers as a node in the Hub | Nodes |
| **Device Group** | organize endpoint nodes for policy and config scoping | Device Groups |
| **Device Assignment** | bind an endpoint node to a user | (auto via login; manual override on node detail) |
| **Device Auth** | per-device authentication policy | Device Auth |
| **Device Defaults** | default configuration applied to newly enrolled devices | Device Defaults |
| **Target-config sync** | nodes pull configuration from the Hub on boot and on change-signal | Config Sync |
| **Out-of-sync detection** | the Hub flags nodes whose applied config diverges from the target | Nodes |
| **Agent CA** | node certificate issuance for mTLS | (automatic; visible on node detail) |
| **Agent Diag Mode** | toggle verbose logging on a remote Agent | Agent Diag Mode |
| **Crash Report collection** | Desktop Agent crash reports stream into the Hub | Crash Reports |
| **Proxy Rollout** | guided deployment for the Compliance Proxy | Proxy Rollout |
| **Agent Setup** | guided installer and configuration for the Desktop Agent | Agent Setup |
| **Overrides** | per-node configuration overrides | Overrides |

## Observability

| Capability | Description | Where to use it |
|---|---|---|
| **Traffic Event log** | one row per request handled by any of the three intercept paths | Traffic |
| **Metric rollup windows (4)** | 5-minute, 1-hour, 1-day, 1-month | (powers Analytics, Cache ROI, Quota Usage) |
| **Global and per-node rollups** | tenant-wide rollups plus per-node rollups | (powers Analytics) |
| **Cache ROI dashboard** | cache hit rate, token and dollar savings | Cache ROI |
| **Quota Usage dashboard** | current usage versus budget per quota axis | Quota Usage |
| **Analytics & Metrics** | cost, latency, error rate, model mix, by tenant and provider | Analytics & Metrics |
| **Alert Rules** | event-detection rules | Alerts → Rules |
| **Alert Channels (4 types)** | `webhook`, `slack`, `email`, `pagerduty` | Alerts → Channels |
| **Provider health monitoring** | live per-provider health checks | Status & Health |
| **Status & Health page** | live state of all five services and their dependencies | Status & Health |
| **Recent Errors** | recent service errors aggregated across the fleet | Recent Errors |

## Operability

| Capability | Description | Where to use it |
|---|---|---|
| **Three-tier kill switch** | escalating disable scopes | Kill Switch |
| **Emergency passthrough** | three bypass flags (`bypassHooks`, `bypassCache`, `bypassNormalize`) | Emergency Passthrough |
| **Config Sync console** | inspect and force-push target configuration | Config Sync |
| **Scheduled Jobs** | recurring job orchestration via the Hub | Scheduled Jobs |
| **Setup Wizard** | first-run administrator onboarding | Setup Wizard |
| **Observability configuration** | log level, trace sampling, exporter wiring | Observability |
| **Observability Retention** | per-table retention windows | Observability Retention |
| **SIEM forwarder** | stream traffic events and audit records to an external SIEM | SIEM |
| **Admin Settings** | global administrator settings | Settings |
| **AI Gateway Simulator** | dry-call the AI Gateway from the Control Plane UI | (Tools → AI Gateway Simulator) |

## References

- `packages/control-plane-ui/src/i18n/locales/en/nav.json` — sidebar labels referenced in the "Where to use it" column
- `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` — route registry
- `packages/ai-gateway/cmd/ai-gateway/wiring/routes.go` — `/v1` ingress endpoints
- `packages/ai-gateway/internal/providers/specs/` — 11 first-class adapter codec directories
- `packages/ai-gateway/internal/providers/specs/compat/` — 9 OpenAI-compatible passthrough codec directories
- `packages/ai-gateway/internal/routing/strategies/` — 7 routing strategy implementations
- `packages/ai-gateway/internal/cache/semantic/` — semantic vector cache module
- `packages/ai-gateway/internal/execution/passthrough/` — emergency passthrough bypass flags
- `packages/ai-gateway/internal/ingress/proxy/ingress_model.go` — streaming detection
- `packages/shared/policy/hooks/builtins/` — built-in hook implementations registry
- `packages/shared/policy/hooks/validators/` — keyword-filter, pii-detector, content-safety, request-size-validator, rulepack-engine, quality-checker
- `packages/shared/policy/hooks/ratelimit/` — rate-limiter
- `packages/shared/policy/hooks/access/` — ip-access-filter, data-residency
- `packages/shared/policy/hooks/webhook/` — webhook-forward
- `packages/shared/storage/spillstore/` — body spillover storage
- `packages/nexus-hub/internal/observability/siem/` — SIEM forwarder
- `packages/control-plane/internal/platform/crypto/aes_gcm.go` — credential vault cipher
- `packages/control-plane/internal/identity/scim/scimstore/scim_store.go` — OIDC JIT user provisioning
- `packages/shared/identity/iam/` — NRN-based RBAC and ABAC
- `packages/shared/transport/thingclient/` — node registration and target-config pull
- `tools/db-migrate/schema.prisma` — Prisma schema for all managed objects
