# Conventions

This is the code-level style reference. It complements the hard bindings in
`CLAUDE.md` and the always-on rules under `.cursor/rules/` — sections marked
**binding** are CI-enforced, the rest are idioms the codebase follows. The Go
conventions here are the developer-facing form of `.cursor/rules/go-conventions.mdc`.

## Go

**Workspace and module paths.** All modules are linked through `go.work` at the
repo root. The module path is
`github.com/AlphaBitCore/nexus-gateway/packages/<module>`, and imports use the
full module path — Go has no relative imports.

**Naming.** Packages are lowercase, short, and underscore-free
(`configtypes`, `configcache`). Avoid stutter — `hooks.Hook`, not
`hooks.HooksRegistry`. Exported types are `PascalCase`.

**Errors.** Return errors; do not panic in library code. Sentinels are
`errors.New("specific lowercase message")`; wrap with
`fmt.Errorf("context: %w", err)`.

**HTTP error envelope — known per-service inconsistency (F-0319).** Each service
currently owns its own JSON error-envelope helper rather than sharing one
package: Control Plane has `packages/control-plane/internal/platform/httperr`
(`{"error":{"message","type","code"}}`); the AI Gateway emits a
provider-shaped envelope from
`packages/ai-gateway/internal/ingress/envelope/error_envelope.go` (OpenAI/Anthropic
wire compatibility is the constraint there); Nexus Hub and the Compliance Proxy
have their own small helpers. This duplication is a recognised inconsistency, not
an endorsed pattern. Consolidating into a single shared `httperr` package is a
deliberately deferred future-sprint task — it is V2-scope because it touches all
four services at once AND the AI Gateway envelope is intentionally NOT the
admin-API shape (it must mimic the upstream provider so SDK clients parse it), so
any shared package has to model both shapes. Until that lands, a new admin
endpoint should reuse its own service's existing helper (CP handlers use
`httperr.ErrJSON`); do NOT hand-roll a fifth envelope shape.

**Concurrency.** Use `sync.Mutex` / `sync.RWMutex` for shared state,
`atomic.Pointer` for hot-swappable config snapshots, and `sync.Pool` for
high-frequency allocations. A `context.Context` is the first parameter of any
cross-package method.

**Logging (binding).** A `*slog.Logger` is passed as a constructor parameter,
never a global. After wiring `SlogSink` and `slog.SetDefault(...)`, the
module-scope `logger` must be reassigned to `slog.Default()` — otherwise
DI-injected loggers silently bypass the diagnostics pipeline.

**Metrics.** Register through `promauto` (`prometheus/client_golang`).
Constructors that register metrics take a `namespace string` parameter.

**Testing.** Run `go test -race -count=1`, table-driven where it fits, in the
same package (white-box) or a `_test` package (black-box).

**Linting.** `golangci-lint` runs against the root `.golangci.yml`, which enables
`errorlint`, `bodyclose`, `noctx`, `copyloopvar`, and `intrange` among others.

**`replace` sibling contract (binding).** Every `packages/<svc>/go.mod` that
requires a sibling module must pin the require to exactly `v0.0.0` (an inert
placeholder, never a real pseudo-version) and carry a matching
`replace … => ../<sibling>` directive, with its `go.sum` free of any
`packages/` lines. Since Go 1.25 a real pseudo-version is validated against the
upstream remote even with `go.work` active, so without this a `GOWORK=off` build
silently pulls a stale GitHub snapshot instead of local code.
`scripts/check-workspace-replace.mjs` (pre-commit and `check:all`) blocks the
regression. `replace` is sibling-only — never fork a third-party dependency
through it.

**Forbidden.** No `sqlc` — write SQL by hand, and keep the Go struct types as
hand-maintained mirrors of the Prisma schema (there is no codegen step). No
breaking API change in `packages/shared/*` once it has shipped in a released Agent
binary; that surface is additive-only. New dependencies in `packages/shared`
outside the vetted set need explicit approval (see
[shared-packages-architecture.md](../architecture/cross-cutting/shared/shared-packages-architecture.md)).

## TypeScript and the Control Plane UI

Each of these is enforced by a guard in `check:all`:

- **i18n mandatory (binding).** Every user-visible string goes through `t()`, and
  the `en` / `es` / `zh` locale files stay at parity across both bundles
  (`scripts/check-i18n-parity.mjs`).
- **Design tokens strict (binding).** No hex or raw numeric values in
  `*.module.css` or inline `style={{}}` blocks — CSS variables only
  (`scripts/check-design-tokens.mjs`).
- **`useApi` query keys (binding).** Every query key starts with a domain prefix
  and a resource: `['admin' | 'my' | 'user' | 'proxy', '<resource>', '<variant?>',
  …stateVars]` (`scripts/check-useapi-querykey.mjs`).
- **`ui-shared` boundary (binding).** `packages/ui-shared` is a dependency leaf and
  must never import from a consumer bundle
  (`scripts/check-ui-shared-boundary.mjs`); see
  [ui-shell-architecture.md](../architecture/cross-cutting/ui/ui-shell-architecture.md).

## Cross-cutting bindings

- **English only.** Committed artifacts — docs, source comments, UI copy, config
  strings, and commit messages — are English (`.cursor/rules/english-only.mdc`).
- **IoT terminology boundary (binding).** Internal code uses the IoT vocabulary
  (Thing / Shadow / desired / reported / drift); user-facing surfaces use the
  product vocabulary (node / config sync / target config / applied config / out of
  sync). `scripts/check-terminology.sh` enforces the split.
- **Secrets are env-only (binding).** No secret field appears in committed YAML;
  cross-service shared secrets are tagged `[MUST MATCH]`
  (`scripts/check-no-yaml-secrets.mjs`); see
  [local-dev-debugging.md](local-dev-debugging.md).
- **Redis is cache-only (binding).** No Redis pub/sub — config invalidation flows
  through the Hub WebSocket (`scripts/check-no-redis-pubsub.mjs`).
- **File-size ratchet (binding).** A production source file (`.go` / `.ts` / `.tsx` /
  `.swift` / `.py` under `packages/` and `tools/`; test files and generated files
  excluded) may not grow past `max(baseline, 300) + 10%` lines of its entry in
  `scripts/.file-size-baseline`; a file not in the baseline is capped at 500 lines.
  Shrinking is never penalized — the baseline only ratchets downward
  (`--update-baseline`). Exceeding a cap means the file needs decomposing along its
  responsibility seams, not a bigger cap; waivers (`scripts/.file-size-waivers`,
  for genuinely irreducible files such as declaration tables) require explicit
  user approval (`scripts/check-file-size-ratchet.sh`, `npm run check:file-size`).
  Oversized test files are governed by the split-on-touch policy instead: any
  session editing a >800-line test file relocates the touched test group into a
  per-resource sibling file before adding new tests.

## Commit style

Commits follow conventional-commits: `<type>(<scope>): <summary>`, with types such
as `docs`, `chore`, `fix`, and `feat`. Agent-authored commits carry a
`Co-Authored-By` trailer. Documentation commits land one doc per commit so review
and revert stay surgical.

## PR review checklist

Before a change is "done", run the two-round self-audit (four questions, twice,
until two consecutive rounds are clean):

1. Every todo is completed — not deferred or silently dropped.
2. No `TODO` / `FIXME` / `stub` / `unimplemented` markers in production code (test
   doubles are fine).
3. Every changed code path is exercised by a real test, or explicitly acknowledged
   as untested with a reason.
4. No "we'll fix this later" claims unless the user agreed to them.

Then the verify gate: the workspace tests are green, the plan was approved, every
doc mapped to the changed code is updated in the same change, new content is
English, and the commit reminder was raised.

## Tooling

| Area | Tool |
|------|------|
| Go build / modules | `go.work` workspace, full-module-path imports |
| Go lint | `golangci-lint` against the root `.golangci.yml` |
| Database | Prisma migrations (`tools/db-migrate`); Go struct types hand-maintained as Prisma-schema mirrors; hand-written `pgx` at runtime |
| UI unit tests | Vitest |
| UI E2E | Playwright (`tests/e2e-ui`) |
| JS monorepo | npm workspaces |
| Style / lockstep gates | the `check:*` scripts aggregated by `npm run check:all` |

`check:all` runs the design-token, i18n, theme-completeness, brand-string,
effect-token, timezone, terminology, JSON-dup-key, arch-doc-trigger,
e2e-coverage-matrix, doc-lockstep, migration-timestamp, `useApi` query-key,
no-Redis-pubsub, `ui-shared`-boundary,
sidebar-icon-mapping, workspace-replace, jobs-catalogue, no-prod-TODO,
no-yaml-secrets, coverage, and file-size-ratchet guards.

## References

- `.cursor/rules/go-conventions.mdc` — the Go conventions rule
- `go.work`, `.golangci.yml` — the workspace and lint configuration
- `scripts/check-*.mjs`, `scripts/check-*.sh` — the convention guards
- `package.json` — the `check:all` aggregate
- [shared-packages-architecture.md](../architecture/cross-cutting/shared/shared-packages-architecture.md) — the shared-dependency policy
- [ui-shell-architecture.md](../architecture/cross-cutting/ui/ui-shell-architecture.md) — the `useApi` and `ui-shared` contracts
- [local-dev-debugging.md](local-dev-debugging.md) — the env-variable and secrets contract
