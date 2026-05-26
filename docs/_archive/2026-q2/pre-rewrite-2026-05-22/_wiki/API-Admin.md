# API Admin

*Audience: integrators automating admin operations; contributors adding new admin endpoints.*

The admin API is the Control Plane's REST interface for managing every resource in Nexus Gateway. It runs on `:3001` under the `/api/admin/` prefix and is consumed by the Control Plane UI (a browser SPA) and by CLI helpers. Every endpoint requires a valid OAuth+PKCE bearer token with `scope: admin`; see [API-Authentication](API-Authentication) for the token-acquisition flow. The full per-endpoint request/response schemas live in the OpenAPI YAML files listed below — this page provides the resource catalog and the path to each spec.

---

## Resource categories

The admin API groups endpoints by the resource they manage. The table below maps each category to its OpenAPI spec file; links are absolute paths into the repository.

### AI Gateway configuration

| Category | OpenAPI spec |
|---|---|
| Routing rules (CRUD + simulate) | [`e19-s1-routing-rule-simulate.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e19-s1-routing-rule-simulate.yaml), [`e20-s2-routing-rule-crud.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e20-s2-routing-rule-crud.yaml), [`e34-s2-routing-rule-matchconditions.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e34-s2-routing-rule-matchconditions.yaml) |
| Routing retry policy | [`e34-s3-routing-retry-policy.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e34-s3-routing-retry-policy.yaml) |
| Provider credentials state | [`e41-s5-admin-credentials-state.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e41-s5-admin-credentials-state.yaml) |
| Prompt cache 3-tier config | [`e38-s13-prompt-cache-3tier-config.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e38-s13-prompt-cache-3tier-config.yaml) |
| Response cache admin + ROI fields | [`e61-s6-cache-admin.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e61-s6-cache-admin.yaml), [`e61-s7-cache-roi-fields.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e61-s7-cache-roi-fields.yaml) |
| Cache pricing admin | [`e58-s1-cache-pricing-admin.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e58-s1-cache-pricing-admin.yaml) |
| Cache UI fields | [`e59-s1-cache-ui-fields.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e59-s1-cache-ui-fields.yaml) |
| Extract cache admin | [`e72-extract-cache-admin.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e72-extract-cache-admin.yaml) |
| Traffic cache status | [`e31-s1-admin-traffic-cache-status.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e31-s1-admin-traffic-cache-status.yaml) |
| Latency phase fields | [`e50-s6-latency-phases.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e50-s6-latency-phases.yaml) |
| Traffic signal fields | [`e18-s11-traffic-signal-fields.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e18-s11-traffic-signal-fields.yaml) |

### Compliance and hooks

| Category | OpenAPI spec |
|---|---|
| Compliance hooks (test) | [`admin-hooks-test.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/admin-hooks-test.yaml) |
| Hook test | [`e26-s1-hook-test.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e26-s1-hook-test.yaml) |
| AI guard + classify + rule packs | [`e27-s01-ai-guard-classify.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e27-s01-ai-guard-classify.yaml), [`e27-s02-rule-packs.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e27-s02-rule-packs.yaml), [`e29-s4-rulepack-crud.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e29-s4-rulepack-crud.yaml) |
| Compliance exemption grants | [`e27-s1-compliance-exemption-grants.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e27-s1-compliance-exemption-grants.yaml) |
| AI Guard / compliance webhook | [`e31-s2-aiguard-compliance-webhook-integration.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e31-s2-aiguard-compliance-webhook-integration.yaml) |
| Interception domains | [`e25-s1-interception-domains.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e25-s1-interception-domains.yaml) |
| Agent exemptions | [`e20-s9-agent-exemptions.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e20-s9-agent-exemptions.yaml) |
| Payload capture settings | [`e22-s1-payload-capture-settings.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e22-s1-payload-capture-settings.yaml) |

### Observability and operations

| Category | OpenAPI spec |
|---|---|
| Unified alerting | [`e21-s1-unified-alerting.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e21-s1-unified-alerting.yaml) |
| Runtime introspection | [`e31-s7-runtime-introspection.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e31-s7-runtime-introspection.yaml) |
| Diag silence and buckets | [`e49-diag-silence-and-buckets.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e49-diag-silence-and-buckets.yaml) |

### Infrastructure and nodes

| Category | OpenAPI spec |
|---|---|
| Thing override and force sync | [`e34-s1-thing-override-and-force-sync.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e34-s1-thing-override-and-force-sync.yaml) |
| Passthrough admin (emergency passthrough) | [`e48-s6-passthrough-admin.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e48-s6-passthrough-admin.yaml) |
| Agent presigned spill | [`e37-s2-agent-presigned-spill.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e37-s2-agent-presigned-spill.yaml) |

### IAM and identity

| Category | OpenAPI spec |
|---|---|
| IAM action catalog | [`e43-s1-iam-action-catalog.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e43-s1-iam-action-catalog.yaml) |
| Identity providers (SSO) | [`e44-s01-identity-providers.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/auth/e44-s01-identity-providers.yaml) |
| Local login + device auth | [`e44-s09-local-login-device-auth.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/auth/e44-s09-local-login-device-auth.yaml) |

### Setup and onboarding

| Category | OpenAPI spec |
|---|---|
| Setup guide APIs | [`e40-s3-setup-guide-apis.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e40-s3-setup-guide-apis.yaml) |

---

## Authentication

All admin endpoints require a bearer token acquired via the OAuth+PKCE flow. The browser SPA performs the full PKCE exchange automatically when an admin signs in. For scripts and CLI tools, `cp_login` (defined in `tests/lib/auth.sh`) drives the same flow against the local or production Control Plane and caches the token for subsequent `cp_curl` calls.

```bash
source tests/lib/auth.sh
cp_login
cp_curl /api/admin/routing-rules
```

Tokens are RS256-signed JWTs with a default 1-hour expiry. The Control Plane verifies the signature against its own JWKS (`/.well-known/jwks.json`). A revoked, expired, or invalid token returns `401`.

---

## IAM enforcement

Every admin endpoint is gated by the IAM middleware (`iamMW`), which checks the authenticated user's policy for the `action` required by that route. The IAM action catalog lists every action and its resource type. Adding a new endpoint without registering an IAM action produces a silent `403` for all callers. See [Control-Plane-IAM-Model](Control-Plane-IAM-Model) for the policy model.

---

## Canonical docs

- [`e43-s1-iam-action-catalog.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/admin/e43-s1-iam-action-catalog.yaml) — full IAM action catalog
- [`iam-identity-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/iam-identity-architecture.md) — IAM resource/action/policy model
- [`oauth-pkce-admin-auth-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/oauth-pkce-admin-auth-architecture.md) — admin bearer token acquisition

**Adjacent wiki pages**: [API-Overview](API-Overview) · [API-Authentication](API-Authentication) · [API-OpenAPI-Index](API-OpenAPI-Index) · [Control-Plane-IAM-Model](Control-Plane-IAM-Model) · [Control-Plane-Overview](Control-Plane-Overview)
