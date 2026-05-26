# API OpenAPI Index

This page catalogs every OpenAPI 3.1 YAML file in `docs/users/api/openapi/`, grouped by surface. Each file defines a bounded slice of the API — request/response schemas, error shapes, and examples for one resource category or one ingress format. Links are absolute paths into the main repository. For the surface-level map and auth strategy per surface, see [API-Overview](API-Overview).

---

## AI Gateway surface

The AI Gateway spec files cover the public traffic endpoints on `:3050`.

| File | What it defines |
|---|---|
| [`ai-gateway-v1.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/ai-gateway/ai-gateway-v1.yaml) | `POST /v1/chat/completions`, `POST /v1/embeddings`, `GET /v1/models`, `GET /v1/models/{model}`, `GET /v1/usage`, `GET /v1/usage/daily` — core AI Gateway endpoints, virtual-key auth, response headers, error shapes |
| [`e56-s1-responses.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/ai-gateway/e56-s1-responses.yaml) | `POST /v1/responses` — OpenAI Responses-API ingress; stateful fields, built-in tool handling, cross-format routing |
| [`e62-s2-embeddings.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/ai-gateway/e62-s2-embeddings.yaml) | Multi-format embeddings: `POST /v1/embeddings` (canonical), `POST /openai/deployments/{d}/embeddings` (Azure), `POST /v1/embed` (Cohere), Gemini single + batch; cross-format routing and capability rejection |

---

## Admin API surface

Admin API spec files cover the Control Plane's `/api/admin/*` endpoints on `:3001`.

### AI Gateway configuration

| File | What it defines |
|---|---|
| [`e18-s11-traffic-signal-fields.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e18-s11-traffic-signal-fields.yaml) | Traffic signal field extensions for routing rules |
| [`e19-s1-routing-rule-simulate.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e19-s1-routing-rule-simulate.yaml) | `POST /api/admin/routing-rules/simulate` — dry-run route resolution |
| [`e20-s2-routing-rule-crud.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e20-s2-routing-rule-crud.yaml) | Routing rule CRUD — create, read, update, delete, reorder |
| [`e34-s2-routing-rule-matchconditions.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e34-s2-routing-rule-matchconditions.yaml) | Extended routing rule match conditions |
| [`e34-s3-routing-retry-policy.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e34-s3-routing-retry-policy.yaml) | Per-rule retry policy configuration |
| [`e38-s13-prompt-cache-3tier-config.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e38-s13-prompt-cache-3tier-config.yaml) | Prompt cache 3-tier fleet configuration |
| [`e41-s5-admin-credentials-state.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e41-s5-admin-credentials-state.yaml) | Provider credential health state, circuit-breaker status |
| [`e50-s6-latency-phases.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e50-s6-latency-phases.yaml) | Per-phase latency fields on traffic events |
| [`e58-s1-cache-pricing-admin.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e58-s1-cache-pricing-admin.yaml) | Response cache pricing configuration |
| [`e59-s1-cache-ui-fields.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e59-s1-cache-ui-fields.yaml) | Cache indicator fields surfaced in the traffic UI |
| [`e61-s6-cache-admin.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e61-s6-cache-admin.yaml) | Response cache fleet admin controls |
| [`e61-s7-cache-roi-fields.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e61-s7-cache-roi-fields.yaml) | Cache ROI fields for cost-savings analytics |
| [`e72-extract-cache-admin.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e72-extract-cache-admin.yaml) | Extract cache admin controls |
| [`e31-s1-admin-traffic-cache-status.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e31-s1-admin-traffic-cache-status.yaml) | Traffic-event cache status fields |

### Compliance and hooks

| File | What it defines |
|---|---|
| [`admin-hooks-test.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/admin-hooks-test.yaml) | Hook test harness endpoints |
| [`e20-s9-agent-exemptions.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e20-s9-agent-exemptions.yaml) | Agent compliance exemption management |
| [`e22-s1-payload-capture-settings.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e22-s1-payload-capture-settings.yaml) | Request/response payload capture configuration |
| [`e25-s1-interception-domains.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e25-s1-interception-domains.yaml) | Compliance proxy interception domain management |
| [`e26-s1-hook-test.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e26-s1-hook-test.yaml) | Hook execution test endpoint |
| [`e27-s01-ai-guard-classify.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e27-s01-ai-guard-classify.yaml) | AI Guard classification endpoints |
| [`e27-s02-rule-packs.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e27-s02-rule-packs.yaml) | Rule pack list and assignment |
| [`e27-s1-compliance-exemption-grants.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e27-s1-compliance-exemption-grants.yaml) | Compliance exemption grant management |
| [`e29-s4-rulepack-crud.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e29-s4-rulepack-crud.yaml) | Rule pack CRUD |
| [`e31-s2-aiguard-compliance-webhook-integration.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e31-s2-aiguard-compliance-webhook-integration.yaml) | AI Guard compliance webhook configuration |

### Observability and operations

| File | What it defines |
|---|---|
| [`e21-s1-unified-alerting.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e21-s1-unified-alerting.yaml) | Alert rule CRUD and channel configuration |
| [`e31-s7-runtime-introspection.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e31-s7-runtime-introspection.yaml) | Runtime introspection endpoints (live config dump, diag) |
| [`e49-diag-silence-and-buckets.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e49-diag-silence-and-buckets.yaml) | Diag event silence rules and metric buckets |

### Infrastructure and safety

| File | What it defines |
|---|---|
| [`e34-s1-thing-override-and-force-sync.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e34-s1-thing-override-and-force-sync.yaml) | Node config override and force-sync |
| [`e37-s2-agent-presigned-spill.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e37-s2-agent-presigned-spill.yaml) | Agent presigned URL for spillstore upload |
| [`e48-s6-passthrough-admin.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e48-s6-passthrough-admin.yaml) | Emergency passthrough toggle and status |

### IAM and setup

| File | What it defines |
|---|---|
| [`e40-s3-setup-guide-apis.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e40-s3-setup-guide-apis.yaml) | First-time setup guide progress endpoints |
| [`e43-s1-iam-action-catalog.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e43-s1-iam-action-catalog.yaml) | IAM action catalog — all resource types and actions |

---

## Auth surface

| File | What it defines |
|---|---|
| [`authserver-login.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/auth/authserver-login.yaml) | `GET /authserver/idps`, `POST /authserver/password` — SPA-facing interactive login JSON endpoints |
| [`e44-s01-identity-providers.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/auth/e44-s01-identity-providers.yaml) | Identity provider CRUD (Okta, Azure AD, SAML, local) |
| [`e44-s09-local-login-device-auth.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/auth/e44-s09-local-login-device-auth.yaml) | Local IdP login and device-auth grant |

---

## Hub surface

| File | What it defines |
|---|---|
| [`e3-hub-api.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/hub/e3-hub-api.yaml) | All Hub HTTP endpoints: `/api/hub/*` (CP → Hub) and `/api/internal/things/*` (services → Hub); both auth variants; schemas for config update, node registry, shadow, drift, jobs, enrollment tokens, audit upload |

---

## Canonical docs

- [`docs/users/api/openapi/`](https://github.com/AlphaBitCore/nexus-gateway/tree/main/docs/users/api/openapi) — root directory of all OpenAPI specs
- [`ai-gateway-v1.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/ai-gateway/ai-gateway-v1.yaml) — AI Gateway primary spec
- [`e3-hub-api.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/hub/e3-hub-api.yaml) — Hub API primary spec

**Adjacent wiki pages**: [API-Overview](API-Overview) · [API-AI-Gateway](API-AI-Gateway) · [API-Admin](API-Admin) · [API-Hub](API-Hub) · [API-Authentication](API-Authentication)
