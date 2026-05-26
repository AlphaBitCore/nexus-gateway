# Control Plane

The admin API + BFF (Backend-For-Frontend) for the Nexus admin UI.
Exposes every operator-facing endpoint (IAM, SSO, providers, virtual
keys, routing rules, analytics, audit) and proxies a small number of
read paths through to the data-plane services. Write paths flow through
Nexus Hub — the Control Plane never mutates Thing shadows directly.

## Where it sits

| | |
|---|---|
| **Port** | `3001` (HTTP, behind Vite proxy in dev) |
| **DB** | PostgreSQL (write-through to Hub for Thing data; direct for IAM, sessions, virtual keys, audit queries) |
| **Cache** | Redis (sessions, IAM cache, response cache) |
| **Auth** | OAuth + PKCE bearer tokens; admin-key HMAC for service-to-service |
| **Consumes** | Nexus Hub (admin internal APIs over `INTERNAL_SERVICE_TOKEN`), AI Gateway (provider test, routing simulate), Compliance Proxy (`/runtime/*` admin proxy) |

## Build

```bash
make control-plane-build   # outputs to dist/bin/control-plane/control-plane
# or
cd packages/control-plane && go run ./cmd/control-plane/ -config control-plane.dev.yaml
```

## Test

```bash
make control-plane-test    # go test -race -count=1 ./...
```

## Key directories

| Path | Purpose |
|---|---|
| `cmd/control-plane/` | Process entry. Wires DB, Redis, Hub client, IAM, OAuth server, every handler group. |
| `internal/handler/` | Echo handlers — admin routes, oauth, traffic/audit query, BFF proxy to data-plane services. |
| `internal/iam/` | Policy CRUD, NRN derivation, managed-policy fixtures (`NexusViewer`, super-admin). |
| `internal/authserver/` | Internal OAuth2 + PKCE issuer; SSO/OIDC federation; JWT keystore. |
| `internal/store/` | Hand-written SQL + pgx access to PostgreSQL. No sqlc — Prisma is the dev-time schema source. |
| `internal/middleware/` | Bearer auth, IAM enforcement (`iamMW(action)`), request ID, rate-limit. |

## Configuration

- `control-plane.dev.yaml` — local boot defaults.
- `control-plane.prod.yaml.example` — production template.
- `control-plane.config.yaml` — common shape (overridable per env).
- Secrets via env: `INTERNAL_SERVICE_TOKEN` (must match Hub),
  `ADMIN_KEY_HMAC_SECRET` (must match AI Gateway),
  `CREDENTIAL_ENCRYPTION_KEY`, `COMPLIANCE_PROXY_API_TOKEN`,
  database/Redis URLs.

## Architecture references

- `docs/developers/architecture/services/control-plane/control-plane-internals-architecture.md`
- `docs/developers/architecture/services/control-plane/iam-identity-architecture.md`
- `docs/developers/architecture/services/control-plane/idp-sso-architecture.md`
- `docs/developers/architecture/services/control-plane/oauth-pkce-admin-auth-architecture.md`
