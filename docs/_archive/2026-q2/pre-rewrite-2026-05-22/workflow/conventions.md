# Conventions

> Concrete code-style + repo-style rules. Living counterpart to the **binding** rules in CLAUDE.md. When in conflict, CLAUDE.md wins.
>
> This doc is for the **micro-decisions** that don't rise to a binding rule — but that we still want consistent across the codebase so new contributors don't have to guess.

---

## 1. Where binding rules live

The hard rules — the ones that have already cost us a prod incident — live in **CLAUDE.md**:

- English only (repository).
- Plan + Todo list (default, binding).
- Capture every user message as todos.
- Deep reasoning.
- Ask the user when confirmation is needed.
- Real implementation only (production code).
- No backward compatibility, no defer (pre-GA greenfield).
- Use the `build-agent` skill for macOS Agent builds.
- API / menu / route changes require IAM impact review.
- macOS NE proxy must fail-open.
- Worktree per session (`git worktree add ./worktrees/<topic>`). Replaces the retired 2026-05-16 same-tree directive (no-stash / no-add-A / explicit-pathspec are no longer binding; safe to use freely inside your own worktree).

In addition, the **operational** bindings are tracked as `feedback_*` and `project_*` memory items (the `.claude/projects/.../memory/` files): token-field stamp sweep, agent traffic-upload level, agent paths abstraction, SP/IdP positioning, etc.

Everything in *this* doc is **softer guidance** — useful for code review and onboarding, but not "the change is invalid if you violate this".

## 2. Go

### Package layout

- `packages/<service>/cmd/<service>/main.go` — entry point. Wire DI, register handlers, start servers. Minimal logic; rest in `internal/`.
- `packages/<service>/internal/<area>/` — internal packages, one per architectural area (e.g., `handler/`, `iam/`, `crypto/`).
- `packages/shared/<subpackage>/` — shared library; see `shared-package-architecture.md` for catalogue + dep tiers.

### Naming

- Package: lowercase, short, no underscores. Avoid stutter (`hooks.Hook`, not `hooks.HooksRegistry`).
- Exported types: `PascalCase`.
- Errors: `errors.New("specific lowercase message")` for sentinels; `fmt.Errorf("context: %w", err)` for wraps.
- Test files: `_test.go` siblings (white-box) or `_test` package (black-box).
- Constructors that take a logger / namespace / dependencies: positional args, dependencies first.

### Idioms

- **Logging**: pass `*slog.Logger` as a parameter. Don't use globals. After wiring SlogSink + `slog.SetDefault`, reassign `logger = slog.Default()` so DI-injected loggers reflect the diag pipeline (binding feedback memory).
- **Metrics**: use `promauto`. Constructors that register metrics take a `namespace string` parameter.
- **Concurrency**: `sync.Mutex` / `sync.RWMutex` for shared state; `atomic.Pointer[T]` for hot-swappable config snapshots; `sync.Pool` for high-allocation hot paths.
- **Context**: pass `context.Context` as the first parameter on any cross-package method.
- **No `sqlc`** — hand-written SQL + hand-maintained Go struct mirrors under `packages/shared/schemas/configtypes/{enums,identity,interception,observability,policy}/`. No Prisma→Go codegen script.
- **`shared` API stability** — once shipped in a released Agent binary, `packages/shared/**` API changes are additive-only.
- **Linting** — `golangci-lint` with `.golangci.yml` per module.

### `replace` directives — workspace-sibling contract (binding)

Every `packages/<svc>/go.mod` requiring a sibling at `github.com/ai-nexus-platform/nexus-gateway/packages/<sibling>` MUST:

1. Pin require to exactly `v0.0.0` (inert placeholder — never a real pseudo-version like `v0.0.0-YYYYMMDDHHMMSS-COMMITHASH`).
2. Carry a matching `replace github.com/ai-nexus-platform/nexus-gateway/packages/<sibling> => ../<sibling>`.
3. Keep `go.sum` free of any `github.com/ai-nexus-platform/nexus-gateway/packages/` lines.

Reason: under Go 1.25, real pseudo-versions are validated against the upstream Git remote even with `go.work` active. Without this contract, `GOWORK=off` builds (or Dockerfiles that forget to COPY `go.work`) silently pull a stale snapshot from GitHub instead of using local code (2026-05-16 investigation). `go mod tidy` outside the workspace will try to regress `v0.0.0` back to a real pseudo-version — the lint `npm run check:workspace-replace` is wired into pre-commit + `check:all` and blocks the regression.

**`replace` is sibling-only.** Do NOT fork a third-party dep, point at an external fork, or override a third-party module to a non-proxy version.

### Dependencies in `shared` — vetted set

Two tiers:

**Core (always available)** — kept tiny so root `shared` builds without external deps:

`log/slog`, `pgx`, `prometheus/client_golang`, `tidwall/gjson`, `tidwall/sjson`, `gopkg.in/yaml.v3`, `go.opentelemetry.io/otel*`, `golang.org/x/net`, `golang.org/x/sync`.

**Driver-scoped (relevant subpackage only)**:

| Dep | Allowed location |
|---|---|
| `nats-io/nats.go` | `transport/mq` |
| `aws-sdk-go-v2*` | `storage/spillstore/s3` |
| `redis/go-redis/v9` | `storage/configcache`, `storage/spillstore/redis`, quota |
| `coder/websocket` | `transport/thingclient` |
| `golang-jwt/jwt/v5` | `identity/iam` |
| `hashicorp/golang-lru/v2` | `storage/configcache`, cert-cache |
| `bits-and-blooms/bloom/v3` | `compliance` |
| `labstack/echo/v4` | only when a shared subpackage exposes a Handler consumers mount |
| `klauspost/compress` | `transport/normalize`, `transport/tlsbump` (HTTP-level gzip on bumped proxy traffic) |
| `google.golang.org/protobuf` | `traffic/adapters/ide/cursor/`, `transport/normalize/extract/` (Cursor IDE Tier-1 protobuf framing) |
| `joho/godotenv` | `core/bootenv` (dev-only `.env` loader; prod injects env via systemd EnvironmentFile / K8s Secrets) |

Architectural intent: driver-scoped deps living in the umbrella `go.mod` is a known shortcut — split each into its own Go module when bandwidth permits.

### Testing

- `go test -race -count=1` is the canonical local test command.
- Table-driven tests where each case has a distinct identity. Don't nest more than two levels of table-driven.
- Integration tests live under `tests/integration-go/`.

### Comments

- Default: **no comment**. The code names should already explain WHAT.
- Add a comment only when the WHY is non-obvious: a hidden constraint, a workaround for a specific bug, behaviour that would surprise a reader. Reference an issue / memory item where helpful.
- Never restate what the code does. "Used by X" / "Added for the Y flow" decays — that belongs in the PR description.

## 3. TypeScript / Control Plane UI

### Project shape

- TypeScript strict mode (no implicit any, no implicit return any).
- Vitest for unit tests; React Testing Library for component tests.
- Vite for build and dev.
- Path alias `@/` for imports from `src/`; avoid deep relative imports.

### Naming

- Components: `PascalCase` filenames + exported name.
- Hooks: `useXxx` filename + export.
- API hooks: prefix `useApi*` for query hooks, `useApiMutation*` for mutations.
- CSS modules: `<Component>.module.css` sibling to `<Component>.tsx`.

### Binding bits

The two binding rules with code-level enforcement:

- **i18n mandatory** — every JSX user-visible string goes through `t('namespace:section.key')`. Locale files in `src/i18n/locales/{en,zh,es}/*.json`; key counts must match across locales. Enforced by `npm run check:i18n`.
- **Design tokens** — visual values (color, spacing, shadow, transitions) must be CSS variables. Two layers: Layer 1 raw (`--g-*`) in `ui-shared`, Layer 2 semantic (`--color-*`, `--space-*`, …) in `light.css` / `dark.css`. Enforced by `npm run check:design-tokens`. See CLAUDE.md for the three escape hatches.
- **`useApi` queryKey** — must start with at least two string literals (domain + resource) followed by state vars. Required shape: `['admin' | 'my' | 'user' | 'proxy', '<resource>', '<variant?>', ...stateVars]`. Avoids React Query cache collisions across pages.

### Code style

- Functional components only. No class components.
- React Query for server state. Local state via `useState` / `useReducer`; lift to context only when crossing component subtrees.
- Imports order: external libs → internal aliases → relative → CSS.
- One component per file (export default the main component; named exports for subcomponents only when reused).

## 4. CSS / styling

- Module CSS (`*.module.css`) for component-scoped styles.
- Global variables in `packages/ui-shared/src/styles/global.css` (Layer 1 raw tokens).
- Semantic tokens in `light.css` / `dark.css`.
- **Hex / rgb literals in *.module.css are forbidden** — only escape: Recharts colours, which must import from `packages/ui-shared/src/theme/chartColors.ts`.
- `style={{}}` literals: design-token bridge (`'--foo': dynamicValue`) and runtime-computed dimensions only. No raw `padding: 8`, no `borderRadius: 4`.

## 5. Database / migrations

- Prisma schema is the source of truth (`tools/db-migrate/schema.prisma`).
- Migrations: `npx prisma migrate dev --name <descriptive-snake-case>` (folders under `tools/db-migrate/migrations/`).
- **Migration timestamp prefix must be unique** (binding). Pre-commit guard `ls tools/db-migrate/migrations/ | cut -c1-14 | sort | uniq -d` must be empty (enforced by `scripts/check-migration-timestamps.sh`).
- Go struct mirrors are **hand-maintained** under `packages/shared/schemas/configtypes/{enums,identity,interception,observability,policy}/`. There is no Prisma→Go codegen script (consistent with the §2 idiom "No `sqlc` — hand-written SQL + hand-maintained Go struct mirrors"). Update the mirror in the same PR as any Prisma schema change.
- Hand-written SQL + pgx at runtime. No `sqlc`.
- Seed: `tools/db-migrate/seed/seed.ts`. The canonical IAM seed lives there.

## 6. API / OpenAPI

- Per the SDD workflow (CLAUDE.md → Mandatory Development Workflow), every story with an API endpoint has an OpenAPI 3.1 spec in `docs/users/api/openapi/`.
- Spec naming: `e{epic}-s{story}-{name}.yaml`.
- Spec is binding: handler signature + UI service layer must match.
- Schemas: prefer flat field names; embed types via `$ref` when reused across paths.

## 6.5. IoT terminology boundary (product-surface enforcement)

The Thing Model (`docs/developers/architecture/cross-cutting/foundation/thing-model.md`) is an internal architecture kernel. **"Thing", "Shadow", "desired/reported", "drift" are internal terms** for code, DB, and developer docs only.

User-facing surfaces (admin API responses, UI strings, product docs, error messages) use **product terms**:

| Internal (code / DB / dev docs) | User-facing (API / UI / product docs / errors) |
|---|---|
| Thing | node / service / device |
| Shadow | config sync |
| drift / out-of-sync | out of sync |
| desired / reported | target config / applied config |

CI enforcement: `npm run check:terminology` audits product surfaces; see `docs/developers/architecture/cross-cutting/foundation/thing-model.md` §10 for the full mapping.

## 7. Documentation

- Tier-1 architecture docs live in `docs/developers/architecture/**/*-architecture.md`. Two exceptions to the suffix: `thing-model.md` and `admin-audit-log-coverage.md` (functional names; listed explicitly in the trigger map).
- Every `docs/developers/architecture/**/*-architecture.md` MUST have a row in `docs/developers/architecture/README.md`. CI lockstep check (`npm run check:arch-doc-triggers`) enforces.
- Feature docs in `docs/users/features/{cp-ui,agent-ui,flows}/`. Cross-link to the underlying architecture docs.
- Runbooks in `docs/operators/ops/runbooks/`. Operational procedures.
- Requirements + SDD + OpenAPI per the workflow.
- English only across the tree (binding).
- Onboarding entry: `docs/README.md`.

## 8. Commit style

- Subject: `<type>(<scope>): <imperative summary>` (e.g., `feat(cp-ui): ...`, `fix(ai-gateway): ...`, `docs: ...`).
- Subject under ~72 chars.
- Body explains the WHY (the PR description in commit form).
- Co-author lines for AI assistance (see CLAUDE.md template).
- Use `--no-verify` only with explicit user approval; record the reason in the commit message (matches `.githooks/pre-commit` header).

## 9. Branching

- `main` is the development trunk.
- Feature branches off `main`; PR back to `main`.
- Prod releases tagged `prod-YYYYMMDD` (memory `project_prod_releases`).

## 10. Tooling

| Command | What |
|---|---|
| `./scripts/dev-start.sh` | One-command bootstrap (Docker / DB / npm install). |
| `go test -race -count=1 ./...` | Run Go tests with race detector. |
| `npm test` | Workspace tests (UI + tools). |
| `npm run lint` | ESLint across workspaces. |
| `npm run check:i18n` | i18n parity. |
| `npm run check:design-tokens` | CSS / style design-token enforcement. |
| `npm run check:terminology` | IoT-terminology boundary on product surfaces. |
| `npm run check:json-dupkeys` | Duplicate JSON keys in locale files. |
| `npm run check:tz` | Timezone-handling correctness. |
| `npm run check:arch-doc-triggers` | Every `docs/developers/architecture/**/*-architecture.md` is referenced in the trigger map. |
| `npm run check:migration-timestamps` | Prisma migration folders have unique timestamp prefixes. |
| `npm run check:doc-lockstep` | Every PR-diffed code file in a mapped glob ships with its lockstep docs. |
| `npm run check:brand-strings` | No raw brand strings in user-facing UI (use the brand registry). |
| `npm run check:effect-tokens` | Visual effects (shadows / transitions) bound to design tokens, not literals. |
| `npm run check:theme-completeness` | Every semantic token defined for every theme (light / dark / branded). |
| `npm run check:ui-shared-boundary` | `packages/ui-shared/` only imported from sanctioned consumers. |
| `npm run check:sidebar-icon-mapping` | Sidebar entries have icon-id mappings and vice versa. |
| `npm run check:workspace-replace` | Sibling `replace` directives intact + `v0.0.0` inert placeholder pinned (workspace contract). |
| `npm run check:jobs-catalogue` | Hub scheduled-job catalogue stays in lockstep with code. |
| `npm run check:no-prod-todos` | No `TODO` / `FIXME` / `XXX` / `unimplemented` / `stub` strings in production code. |
| `npm run check:no-yaml-secrets` | No secret fields in committed yaml — env-only contract. |
| `npm run check:coverage` | Go package coverage ≥ 95% or explicit allowlist entry (`--strict-allowlist` variant). |
| `npm run check:useapi-querykey` | `useApi` queryKey starts with `['admin' \| 'my' \| 'user' \| 'proxy', '<resource>', ...]`. |
| `npm run check:no-redis-pubsub` | Redis is cache-only — no `.Publish(` / `.Subscribe(` / `nexus:config*` channel literals. |

CI runs all of the above on every push.

## 11. PR review checklist (quick reference)

The reviewer should confirm:

- [ ] Plan / Todo list was followed (binding).
- [ ] All edits are English (binding).
- [ ] No `TODO` / `FIXME` in production code without explicit user approval.
- [ ] Tier-1 architecture docs were consulted per `architecture-doc-triggers.md`.
- [ ] If IAM-impacted: 5-step IAM impact review explicit in PR.
- [ ] If migration: unique timestamp prefix.
- [ ] If token field added: 5 stamp sites swept (binding).
- [ ] If NE-touched: 5 fail-open rules respected (binding, safety-critical).
- [ ] If new arch doc: trigger map row appended in same PR.
- [ ] Tests pass locally (`go test -race -count=1`, `npm test`).

For everything else, lean on the binding rules in CLAUDE.md.
