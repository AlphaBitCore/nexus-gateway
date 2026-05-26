# Control Plane Overview

*Audience: contributors and operators working with the Control Plane service.*

The Control Plane is the administrative backbone of Nexus Gateway. It exposes a REST admin API secured by OAuth+PKCE bearer tokens and enforced by a deny-overrides IAM layer, hosts the CP UI React application, and coordinates configuration across the entire 5-service fleet by propagating changes through Nexus Hub. Every admin action — from creating a routing rule to toggling the kill switch — passes through the Control Plane.

---

## Package layout

The Control Plane's internal tree is organized into **9 domain buckets** plus a thin top-level `handler/` for cross-cutting wiring. Code lives under `packages/control-plane/internal/`:

| Bucket | Contents |
|---|---|
| `ai/` | Cache, providers, quota, routing, simulator, virtual keys — the AI Gateway-facing admin surface |
| `fleet/` | Node / Thing lifecycle: registry, applied-config view, force-resync |
| `governance/` | Compliance pipeline admin: hooks, rule packs, exemptions, DSAR, interception domains, kill switch, emergency passthrough |
| `handler/` | Route mount entry point (`admin_routes.go`) + cross-cutting helpers only; per-domain endpoints live in their own bucket |
| `identity/` | Admin auth, IAM, external IdP federation, SCIM, JWT verifier, session revocation, users |
| `infrastructure/` | Hub-bound ops: jobs catalogue view, retention windows, ops metadata |
| `observability/` | Alert rules + channels, diagnostic mode, ops metrics, retention, SIEM bridge, Thing stats |
| `platform/` | Cross-cutting plumbing: admin audit writer, Hub config reconciler, AES-256-GCM helpers, Hub HTTP client, Prometheus metrics, middleware stack, pgx pool |
| `settings/` | Cluster-wide settings backed by the `system_metadata` table |
| `traffic/` | Traffic event analytics and the admin Traffic Logs handlers |

New CRUD code goes into the relevant domain bucket, not the top-level `handler/`. The route mount in `admin_routes.go` threads a `Deps` struct through each domain's `Mount<Domain>(g, deps)` constructor.

## Middleware stack

The middleware stack (in evaluation order) wraps every admin request:

1. **Panic recovery** — catch-all; logs and returns 500.
2. **Request ID stamp** — generates or threads `X-Request-ID`.
3. **Metrics counter/histogram** — Prometheus instrumentation for every endpoint.
4. **CORS** — preflight plus origin gating.
5. **Auth (bearer)** — validates the OAuth-issued JWT via `identity/jwt/`; attaches `User` to context.
6. **IAM cache warmup** — pre-loads the user's effective policies from Redis into request context; per-route `iamMW(action)` checks are O(1) after this point.
7. **Audit emit (post-handler)** — captures response status, duration, and resource for the admin audit log.

Every admin route passes through `iamMW(action)` before any business logic. The action string must match what `shellRouteConfig.tsx` declares in `allowedActions` for the matching sidebar item. Drift between these two produces silent 403s — the binding IAM impact review rule governs this (see [Control Plane IAM Model](Control-Plane-IAM-Model)).

## Store decomposition

Each domain owns its own store files. Per-domain stores live alongside their handler in `<domain>/<resource>/<resource>store/`. The top-level `store/` holds only cross-cutting queries that span multiple domains (compliance dashboard, exemption views, provider, service instance, system metadata). All SQL is hand-written — no `sqlc`. Every store function takes `ctx context.Context` first and returns `(typed_struct_or_slice, error)`; errors wrap with `fmt.Errorf("store: %s: %w", verb, err)`.

## Relationship to Nexus Hub

The Control Plane does not have its own config-push channel to data-plane services. Instead:

1. Admin modifies a resource via the CP admin API.
2. CP writes to Postgres, then calls Hub's HTTP API to update the shadow blob.
3. Hub signals all affected Things (AI Gateway, Compliance Proxy, Agent) via WebSocket.
4. Each Thing receives the change signal and pulls its new config from Hub.

No Redis pub/sub is in this path. Config propagation is Hub-centric and pull-only.

## Finding an existing endpoint

When investigating an existing admin endpoint (or adding a new one), the lookup path is:

1. Identify the resource domain (`ai/`, `governance/`, `identity/`, `fleet/`, `observability/`, `infrastructure/`, `settings/`, `traffic/`).
2. Navigate to `<domain>/<resource>/handler/` and grep within it.
3. The route mount entry point `handler/admin_routes.go` shows all `Mount<Domain>(g, deps)` calls — it is the authoritative list of domains.

New endpoints always land in the matching `<domain>/<resource>/handler/` subpackage. New flat files under top-level `handler/` are reserved for genuinely cross-cutting surfaces (such as the multi-domain applied-config view in `admin_things_applied_config.go`).

## Tech notes

- Echo framework for HTTP routing (`:3001` in local dev).
- Postgres via `platform/pgx/` — hand-written pgx pool; no ORM at runtime.
- Redis for sessions, IAM policy cache, and rate limiting — no Redis pub/sub.
- `platform/crypto/` owns AES-256-GCM helpers for sealing provider credentials at rest with `CREDENTIAL_ENCRYPTION_KEY`.
- The CP UI is a React + TypeScript + Vite SPA (`:3000` in local dev) served separately; it communicates with the CP admin API exclusively over bearer tokens.

---

## Canonical docs

- [`control-plane-internals-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/control-plane-internals-architecture.md) — 9-bucket layout, middleware stack order, store decomposition
- [`iam-identity-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/iam-identity-architecture.md) — IAM model, NRN grammar, iamMW mechanics

**Adjacent wiki pages**: [Control Plane Admin UI Tour](Control-Plane-Admin-UI-Tour) · [Control Plane IAM Model](Control-Plane-IAM-Model) · [Control Plane Authentication](Control-Plane-Authentication) · [Hub Coordination](Hub-Coordination) · [The Five Services](The-Five-Services)
