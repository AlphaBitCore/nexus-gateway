# API Overview

Nexus Gateway exposes four distinct API surfaces, each serving a different audience and secured with a different credential type. AI traffic clients use the AI Gateway (`/v1/*`) with a virtual key. Administrators use the admin API (`/api/admin/*`) with an OAuth+PKCE bearer token. Internal services register and coordinate through Nexus Hub (`/api/hub/*`, `/api/internal/things/*`) using a shared service token or a per-service token. Authentication endpoints (`/oauth/*`, `/authserver/*`) manage the admin OAuth flow and identity-provider federation. This page maps each surface to its port, auth method, and dedicated reference page.

---

## AI Gateway — public traffic surface

The AI Gateway runs on `:3050` and accepts inbound AI requests from applications. It presents an OpenAI-compatible interface across five endpoints:

| Endpoint | Purpose | Auth |
|---|---|---|
| `POST /v1/chat/completions` | Chat completions (OpenAI format) | Virtual key |
| `POST /v1/messages` | Chat completions (Anthropic format) | Virtual key |
| `POST /v1/responses` | Stateful responses (OpenAI Responses-API format) | Virtual key |
| `POST /v1/embeddings` | Embedding vectors | Virtual key |
| `GET /v1/models` | List enabled models | None required |
| `GET /v1/usage` | Current-period usage + quota status | Virtual key |
| `GET /v1/usage/daily` | Daily usage time-series (max 90 days) | Virtual key |

All traffic-bearing endpoints authenticate via a virtual key, either in the `x-nexus-virtual-key` header or as a bearer token in the `Authorization` header. The virtual key encodes org/project scope, model restrictions, and quota policy. Every request passing through this surface produces a `traffic_event` row in Postgres and emits a NATS message for audit.

Full endpoint reference: [API-AI-Gateway](API-AI-Gateway).

## Admin API — control plane surface

The Control Plane runs on `:3001` and exposes the admin REST API under `/api/admin/*`. This surface is consumed by the admin UI (a browser SPA) and by CLI helpers such as `cp_login` / `cp_curl`. All endpoints require a valid OAuth+PKCE bearer token with `scope: admin` in the `Authorization: Bearer <token>` header.

Admin endpoints span multiple resource categories:

- Virtual keys, providers, credentials, models, routing rules
- IAM policies, identity providers, user and org management
- Compliance hooks, PII redaction policies, domain/device predicates
- Traffic analytics, audit log, SIEM configuration
- Infrastructure controls: nodes, config sync, jobs, kill switch, emergency passthrough

For the complete per-category catalog: [API-Admin](API-Admin).

## Hub API — service coordination surface

Nexus Hub runs on `:3060` and exposes two route groups:

- `/api/hub/*` — called by the Control Plane to push config updates, query node status, manage jobs, and generate agent enrollment tokens. Auth: `INTERNAL_SERVICE_TOKEN` shared between Hub and CP.
- `/api/internal/things/*` — called by all other services (AI Gateway, Compliance Proxy, Agent) for registration, heartbeat, config pull, shadow reporting, and audit batch upload. Auth: per-service bearer token (server services use a service token; agents use a per-device token).

The primary transport for service-to-Hub coordination is a WebSocket connection. The HTTP endpoints listed in `/api/internal/things/*` are the fallback path used when the WebSocket is unavailable and for large payloads (audit batch uploads always use HTTP regardless of WebSocket state).

Full Hub protocol reference: [API-Hub](API-Hub).

## Authentication endpoints

Authentication endpoints live on the Control Plane at `:3001` alongside the admin API. These endpoints are NOT part of the admin API — they use no `Authorization` header and are consumed before a bearer token exists.

| Path group | Purpose |
|---|---|
| `/oauth/authorize`, `/oauth/token`, `/oauth/revoke`, `/oauth/introspect` | RFC 6749 / RFC 7009 / RFC 7662 OAuth 2.0 endpoints |
| `/.well-known/jwks.json`, `/.well-known/openid-configuration` | JWKS and OIDC discovery |
| `/authserver/idps` | List enabled identity providers for a pending auth context |
| `/authserver/password` | Submit credentials against the local IdP |
| `/authserver/oidc/begin`, `/authserver/oidc/callback` | External IdP (Okta / Azure AD) federation flow |

Virtual-key auth (for AI traffic) and internal-service token auth (for Hub calls) are not OAuth-based and do not flow through these endpoints. See [API-Authentication](API-Authentication) for the full auth flow reference.

---

## Surface-to-auth summary

| Surface | Port | Auth credential |
|---|---|---|
| AI Gateway — `/v1/*` | `:3050` | Virtual key (`nvk_...`) |
| Admin API — `/api/admin/*` | `:3001` | OAuth+PKCE bearer (RS256 JWT) |
| Hub control — `/api/hub/*` | `:3060` | `INTERNAL_SERVICE_TOKEN` (shared) |
| Hub service — `/api/internal/things/*` | `:3060` | Per-service bearer token |
| Auth — `/oauth/*`, `/authserver/*` | `:3001` | None (produces credentials) |
| Model catalog — `GET /v1/models` | `:3050` | None (public) |

The [API-OpenAPI-Index](API-OpenAPI-Index) lists every OpenAPI YAML file grouped by surface. For a walkthrough of how to acquire each credential type, see [API-Authentication](API-Authentication).

---

## Canonical docs

- [`overview.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/overview.md) — system topology and service port map
- [`ai-gateway-v1.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/ai-gateway/ai-gateway-v1.yaml) — AI Gateway OpenAPI spec
- [`e3-hub-api.yaml`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/api/openapi/hub/e3-hub-api.yaml) — Hub API OpenAPI spec
- [`credentials-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/credentials-architecture.md) — virtual key + provider credential model

**Adjacent wiki pages**: [API-AI-Gateway](API-AI-Gateway) · [API-Admin](API-Admin) · [API-Hub](API-Hub) · [API-Authentication](API-Authentication) · [API-OpenAPI-Index](API-OpenAPI-Index) · [Architecture-Overview](Architecture-Overview)
