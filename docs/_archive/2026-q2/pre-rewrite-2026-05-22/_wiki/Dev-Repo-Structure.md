# Dev Repo Structure

*Audience: first-time contributors learning the layout.*

Nexus Gateway is a Go + TypeScript monorepo using `go.work` for Go workspace management and npm workspaces for the UI. The top-level directories map cleanly to function: `packages/` holds the five services and shared libraries, `tools/` holds database migrations, `docs/` holds all documentation, `scripts/` holds lint and CI tooling, and `.claude/` + `.cursor/` hold the AI vibe-coding workbench artifacts. This page tours each directory so a new contributor can navigate without guessing.

---

## Top-level layout

```
nexus-gateway/
  CLAUDE.md              # binding rules for all contributors + AI agents
  CONTRIBUTING.md        # contribution checklist
  go.work                # Go workspace (links all packages/* Go modules)
  package.json           # npm workspaces root
  docker-compose.yml     # PostgreSQL + Valkey (Redis-compatible) + NATS for local dev
  .env.example           # env var contract; copy to .env for local dev
  packages/              # five Go services + shared libraries
  tools/                 # database migrations and seed
  docs/                  # all documentation
  scripts/               # lint, coverage, pre-commit tooling
  tests/                 # end-to-end and integration test suite
  examples/              # runnable usage examples
  .claude/               # Claude Code skills and project memory
  .cursor/               # Cursor IDE rules
```

The `go.work` file is critical — every `packages/*/go.mod` uses `replace` directives pointing at sibling modules with an inert `v0.0.0` placeholder. Breaking this contract causes `GOWORK=off` builds to pull stale snapshots from GitHub instead of using local code.

---

## packages/ — services and shared libraries

Each subdirectory is an independent Go module (or the React UI) linked via the workspace:

| Directory | Language | Role | Port |
|---|---|---|---|
| `packages/nexus-hub/` | Go | Service registry, config sync, alerting, WebSocket bus | `:3060` |
| `packages/control-plane/` | Go | Admin REST API (Echo framework) | `:3001` |
| `packages/control-plane-ui/` | TypeScript / React + Vite | Admin dashboard SPA | `:3000` |
| `packages/ai-gateway/` | Go | AI traffic proxy; all `/v1/*` endpoints | `:3050` |
| `packages/compliance-proxy/` | Go | Transparent TLS CONNECT intercept proxy | `:3040` |
| `packages/agent/` | Go + Swift (macOS NE) | Desktop agent for endpoint capture | local only |
| `packages/shared/` | Go | Shared business logic across all services | library only |

### Internal package layout (Go services)

Every Go service follows the same pattern:

```
packages/<service>/
  cmd/<service>/
    main.go              # entry point — DI wiring, handler registration, server start
  internal/
    handler/             # HTTP handlers
    store/               # database layer (pgx pool, queries)
    config/              # YAML config loader
    ...                  # area-specific packages
  <service>.dev.yaml     # local dev config (log level debug, local DB URLs)
```

No business logic lives in `cmd/` — only wiring. The `internal/` restriction enforces encapsulation at the Go level.

### packages/shared/

The shared library is grouped into 8 buckets:

| Bucket | What it contains |
|---|---|
| `audit/` | Audit event types and pipeline interfaces |
| `core/` | Logging, bootenv, context utilities |
| `identity/` | IAM engine, JWT, credential types |
| `policy/` | Policy evaluation, hook interfaces |
| `schemas/` | Hand-maintained Go struct mirrors of Prisma models; configKey catalog |
| `storage/` | Spillstore (S3 + local), configcache, MQ wrappers |
| `traffic/` | Canonical traffic event types, normalization codecs, cost estimation |
| `transport/` | thingclient (WebSocket), tlsbump, bufconn |

The dependency rule is strict: `shared/` subpackages may only import from the vetted set of external dependencies listed in `docs/developers/workflow/conventions.md` §2. Adding a new dep requires explicit approval.

### packages/control-plane-ui/

```
control-plane-ui/src/
  pages/           # page components — one per admin UI section
  api/             # API service layer (React Query hooks)
  components/      # reusable components
  i18n/locales/    # EN / ZH / ES locale JSON files
  theme/           # design token system
```

Every user-visible string goes through `t()` — raw English in JSX is caught by `npm run check:i18n`. CSS values use design-token variables — hex literals are caught by `npm run check:design-tokens`.

---

## tools/ — database migrations

```
tools/db-migrate/
  schema.prisma          # source of truth for the database schema
  migrations/            # Prisma migration folders (YYYYMMDDHHMMSS_name/)
  seed/
    seed.ts              # canonical seed: IAM roles, built-in hooks, system config
    prod-data.sql        # snapshot of non-secret prod reference data
```

Go struct types that mirror Prisma models live in `packages/shared/schemas/configtypes/{enums,identity,interception,observability,policy}/` and are hand-maintained alongside schema changes — there is no codegen script.

Migration timestamp prefixes must be unique. The pre-commit check `npm run check:migration-timestamps` enforces this; duplicate prefixes cause Prisma to silently skip migrations.

---

## docs/ — documentation tree

```
docs/
  README.md              # navigation hub
  users/
    product/             # overview, features, deployment models, competitive landscape
    features/            # per-UI-section docs (cp-ui/, agent-ui/, flows/)
    api/openapi/         # OpenAPI 3.1 specs (e{epic}-s{story}-{name}.yaml)
  developers/
    architecture/        # system architecture docs (the trigger map + per-service + cross-cutting)
    workflow/            # SDD pipeline, conventions, AI vibe-coding workflow
    specs/               # requirements + SDD per epic
    roadmap.md           # in-flight + queued work tracker
  operators/
    ops/                 # deployment, monitoring, runbooks
  _wiki/                 # GitHub Wiki pages (authored here; pushed to wiki repo)
  _archive/              # frozen historical artifacts
```

The `docs/developers/architecture/README.md` trigger map is the entry point for finding which architecture doc to read before touching any package. Every `*-architecture.md` file has a row there; CI enforces it with `npm run check:arch-doc-triggers`.

---

## scripts/ — lint and CI tooling

Every `scripts/check-*.mjs` or `scripts/check-*.sh` enforces one binding rule. They run as pre-commit hooks (staged-file scoped, sub-second) and as CI checks on every PR push:

| Script | Binding rule it enforces |
|---|---|
| `check-go-coverage.sh` | ≥95% statement coverage per Go package |
| `check-doc-lockstep.mjs` | Code and docs update in the same commit |
| `check-no-prod-todos.mjs` | No `TODO`/`FIXME`/`XXX`/stubs in production code |
| `check-no-yaml-secrets.mjs` | Secrets are env-only, never in YAML |
| `check-migration-timestamps.sh` | Unique migration timestamp prefixes |
| `check-arch-doc-triggers.mjs` | Every architecture doc has a trigger-map row |
| `check-workspace-replace.mjs` | Sibling `replace` directives intact with `v0.0.0` |
| `check-i18n-parity.mjs` | Locale files have matching key sets |

Run `npm run check:all` to execute the full suite locally.

---

## .claude/ and .cursor/ — the AI vibe-coding workbench

```
.claude/skills/          # invocable Claude Code procedures (/skill-name)
.cursor/rules/           # Cursor IDE rule files (.mdc)
```

`.cursor/rules/` files have two flavors: `alwaysApply: true` meta-rules loaded into every prompt (SDD workflow, pre-edit reading, completion self-audit, session handoff), and `globs:` rules that fire when a matching file is open (agent NE safety, IAM impact review, adapter conformance, IoT terminology boundary).

`.claude/skills/` are invocable as `/skill-name` in Claude Code. Each skill is a self-contained runbook covering pre-conditions, numbered steps, binding rules, and a verification gate. The full catalog is at [`docs/developers/workflow/ai-skill-catalog.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/ai-skill-catalog.md).

See [Workbench Overview](Workbench-Overview) for the full methodology.

---

## Canonical docs

- [`docs/developers/architecture/project-structure.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/project-structure.md) — authoritative per-service directory trees
- [`docs/README.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/README.md) — documentation navigation hub
- [`docs/developers/workflow/conventions.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/conventions.md) — Go + TypeScript coding conventions

**Adjacent wiki pages**: [Contributing](Contributing) · [Dev Local Development](Dev-Local-Development) · [The Five Services](The-Five-Services) · [Architecture Overview](Architecture-Overview) · [Workbench Overview](Workbench-Overview)
