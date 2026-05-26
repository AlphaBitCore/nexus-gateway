---
doc: control-plane-internals-architecture
area: service
service: control-plane
tier: 1
---

# `packages/control-plane/internal/` — Internals Navigation Map

> **Tier 2 architecture doc.** A navigation map for the Control Plane internal tree. The internal layout is organised as 9 domain buckets plus a thin top-level `handler/` for cross-cutting wiring and a thin top-level `store/` for cross-cutting queries. This doc tells new contributors "where to look for what" rather than enumerating every endpoint.

## Top-level layout

The internal tree is organised into **9 domain buckets** plus a small set of cross-cutting helpers:

| Bucket | Contents | Reading order |
|---|---|---|
| `ai/` | `cache/`, `providers/`, `quota/`, `routing/`, `simulator/`, `virtualkeys/` | AI Gateway-facing admin surface (providers, models, credentials, VKs, routing rules, quotas, cache settings). |
| `fleet/` | `handler/`, `store/` | Node / Thing lifecycle (registry, applied-config view, force-resync). |
| `governance/` | `aiguard/`, `dsar/`, `exemptions/`, `hooks/`, `interception/`, `killswitch/`, `passthrough/`, `rulepacks/` | Compliance pipeline admin (hooks, rule packs, exemptions, DSAR, interception domains, kill switch, emergency passthrough). |
| `handler/` | 6 files (admin_routes.go, helpers.go, admin_things_applied_config.go, plus tests) | **Top-level handler bucket is intentionally small**: only the route-mount entry point + helpers shared across domains. New CRUD code does NOT land here — it lands in the relevant domain bucket. |
| `identity/` | `authn/`, `authserver/`, `iam/`, `idptest/`, `jwt/`, `scim/`, `sessions/`, `sso/`, `users/` | All identity surfaces (admin auth, IAM, IdP, SCIM, JWT verifier, session revocation). Each has its own Tier 2 doc — see "Subpackage docs" below. |
| `infrastructure/` | `infra/`, `store/` | Hub-bound infrastructure ops (jobs, retention windows, ops metadata). |
| `observability/` | `alerts/`, `diag/`, `opsmetrics/`, `retention/`, `siem/`, `thingstats/` | Alert rules + channels, diagnostic mode, ops metrics, retention windows, SIEM bridge, Thing stats rollup. |
| `platform/` | `audit/`, `configreconcile/`, `crypto/`, `hub/`, `metrics/`, `middleware/`, `pgx/` | Cross-cutting platform plumbing: admin audit writer, Hub config reconciler, credential AES-256-GCM helpers, Hub HTTP client, Prom metrics, middleware stack, pgx pool. |
| `settings/` | `handler/`, `store/` | Cluster-wide settings (the `system_metadata` table backing). |
| `store/` | 14 .go + `systemmetastore/`: cross-cutting helpers (`compliance_dashboard.go`, `compliance_exemption_*.go`, `cross_path_governance.go`, `provider.go`, `service_instance.go`, `system_metadata.go`, etc.) | **Top-level store bucket holds only cross-cutting queries that don't bind to a single domain bucket**. Per-table stores live in their owning domain bucket — see "`store/` decomposition" below. |
| `traffic/` | `analytics/`, `handler/`, `store/` | Traffic event analytics + the admin Traffic Logs handlers. |

## Subpackage docs

Most identity / IAM / settings subpackages have their own Tier-1 / Tier-2 docs:

- `identity/iam/` → `iam-identity-architecture.md`
- `identity/sso/`, `identity/authserver/`, `identity/jwt/` → `idp-sso-architecture.md`, `oauth-pkce-admin-auth-architecture.md`, `jwt-verifier-architecture.md`
- `platform/configreconcile/`, `platform/hub/` → `thing-config-sync-architecture.md` §7 + `service-call-framework.md`
- `platform/audit/` → `audit-pipeline-architecture.md`
- `platform/metrics/` → `prometheus-naming-architecture.md`
- `platform/middleware/` → covered by this doc §"middleware/ — stack order".
- `platform/crypto/` → covered below; see also `credentials-architecture.md` for the encryption story.
- `identity/idptest/` → test IdP used by SSO end-to-end tests.

## `handler/` — what stays at the top level

The top-level `handler/` is a **6-file bucket** carrying only:

- `admin_routes.go` — the route mount entry point called from `cmd/control-plane/main.go`. It threads `Deps` through each domain's `Mount<Domain>(g, deps)` constructor.
- `helpers.go` (+ `helpers_test.go`) — request-id / envelope / error helpers reused across every domain bucket.
- `admin_things_applied_config.go` (+ test) — the cross-cutting "what config is each Thing actually running?" view; lives at the top level because it joins across multiple domain reporters.
- `handler_test_helpers_test.go` — shared test helpers.

**Where to look for an existing endpoint:** find the resource's domain bucket (`ai/<resource>/handler/`, `governance/<resource>/handler/`, etc.) and grep within it.

**Where to add a new endpoint:** in the matching `<domain>/<resource>/handler/` subpackage. New flat `handler/admin_<x>.go` files are out — the per-domain layout is the binding convention.

Each domain handler does: parse → `iamMW(action)` → call into its store → translate via `platform/hub/` if Hub-bound → return JSON.

**IAM check first** is a binding rule: every admin endpoint passes through `iamMW(...)` before any business logic, evaluating against the resource NRN. The action string must match what `shellRouteConfig.tsx` declares in `allowedActions` for the matching sidebar item (per CLAUDE.md "API / menu / route changes require IAM impact review").

## `platform/middleware/` — global vs per-route

The global Echo stack is installed by `wiring.InitMiddleware` (`cmd/control-plane/wiring/middleware.go`) and contains exactly four entries, applied in declaration order:

1. **Recovery** — last-ditch panic catch; logs + 500.
2. **`NexusRequestID`** — generates or threads `X-Request-ID`.
3. **`AccessLog`** — per-request structured access log.
4. **`RequestMetrics`** — Prometheus request counter / histogram.

Per-route auth + authorization sit outside this global stack. `wiring.InitRoutes` (`cmd/control-plane/wiring/routes.go`) mounts them per group:

- `/api/admin/*` and `/api/my/*` groups use `middleware.AdminAuth` (validates the bearer token via the shared JWT verifier OR a hashed admin API key looked up from `apikeystore`).
- `/api/internal/*` uses `rstokenauth.Middleware(InternalServiceToken)` (constant-time compare against the shared internal-service token).
- Per-endpoint authorization is invoked from inside each handler via `RequireIAMPermission(engine, action, resourceFn)` — there is no global IAM middleware and no global audit middleware. Audit writes are issued explicitly by handlers via `platform/audit`.

Adding a new middleware: decide whether it is global (add to `InitMiddleware`) or per-group / per-route (add to the relevant group in `InitRoutes` or inside a domain `Mount<Domain>`).

## `store/` decomposition

- **Top-level `store/`** (`packages/control-plane/internal/store/`) holds cross-cutting helpers only: `compliance_dashboard.go`, `compliance_exemption_grant.go`, `compliance_exemption_unified.go`, `cross_path_governance.go`, `db.go`, `exemption_request.go`, `idp_migrate.go`, `misc_queries.go`, `provider.go`, `safe_update.go`, `service_instance.go`, `system_metadata.go`, plus the `systemmetastore/` subpackage and test helpers.
- **Per-table stores live with their owning domain**: e.g.
  - VK queries → `ai/virtualkeys/vkstore/`
  - Routing rule queries → `ai/routing/routingstore/`
  - Traffic event queries → `traffic/store/`
  - Node / fleet queries → `fleet/store/`
  - Infrastructure jobs → `infrastructure/store/`
  - Settings (`system_metadata`) → `settings/store/`

Convention inside any `*store/` package: every function takes `ctx context.Context` first, returns `(typed_struct_or_slice, error)`. Errors wrap with `fmt.Errorf("store: %s: %w", verb, err)`. No global db pool — the pool is injected at construction time into a `Store` struct that the handler layer holds.

**No sqlc** — CLAUDE.md "Go" section is explicit. All SQL is hand-written and lives in these per-domain store files.

**Where to look for a table's queries:** find the resource's domain bucket; grep within `<domain>/<resource>/<resource>store/` or `<domain>/store/`.

**Where to add a new query on an existing table:** append to its existing store file in the owning domain bucket.

**Where to add a new table's queries:** create the file under the owning domain's store directory; add a constructor to the domain's `Mount` call.

## `platform/crypto/`

Owns the AES-256-GCM encrypt/decrypt helpers used to seal Provider credentials at rest with `CREDENTIAL_ENCRYPTION_KEY`. The key itself is loaded via bootstrap config; rotation is a separate ops procedure.

When you change credential encryption: this is the only package that should touch raw bytes. Higher layers always see decrypted credentials in memory.

## `identity/idptest/`

A minimal OAuth + OIDC IdP used by SSO end-to-end tests. Implements just enough of the IdP wire format to drive the federation path through `identity/sso/` and `identity/authserver/` without needing a real IdP.

When writing a new SSO test: import this package and start the test IdP; the federation path will treat it as any other IdP.

## When you change one of these

- **New code goes in the relevant domain bucket.** Add files under `<domain>/handler/` (or `<domain>/<resource>/handler/`). Do NOT add new files under top-level `handler/` unless the work is genuinely cross-cutting (e.g. a new applied-config view that joins multiple domains).
- **`store/` adding a join across 3+ tables**: prefer a new file in the owning domain's store directory rather than overloading any single table's file. Stitch-style cross-domain queries that don't have an obvious owner can live under top-level `store/` (the `compliance_*` files are precedents).
- **`platform/middleware/` order changes**: update the §"`platform/middleware/` — stack order" map above in the same PR.

## Sources

- `packages/control-plane/internal/` — top-level layout described above.
- `packages/control-plane/internal/handler/admin_routes.go` — the route mount entry point that wires each domain bucket.
- `packages/control-plane/cmd/control-plane/main.go` — the wiring authority for stack + route registration.
