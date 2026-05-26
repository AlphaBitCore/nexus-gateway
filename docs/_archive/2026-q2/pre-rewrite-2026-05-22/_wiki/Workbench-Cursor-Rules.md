# Workbench Cursor Rules

*Audience: contributors and fork-adopters who want to understand or replicate the `.cursor/rules/` catalog.*

The `.cursor/rules/` directory contains 35 `.mdc` files that surface binding rules into Cursor IDE (and compatible agents). Each file has a YAML frontmatter block with either `alwaysApply: true` (loaded into every prompt) or a `globs:` list (fires when a matching file is open or staged). The always-apply rules are meta-rules — they index and enforce the workflow discipline from `CLAUDE.md`. The glob-scoped rules carry domain bindings that are only relevant in specific areas of the codebase. This page catalogs every rule, grouped by type.

---

## Always-apply rules (14 rules)

These rules load into every agent prompt regardless of which files are open. They enforce the core workflow discipline.

| Rule file | What it enforces |
|---|---|
| `adversarial-product-review.mdc` | Before implementing any feature, knob, or surface: steel-man then attack. Surface the value question, frequency question, default-sufficiency question, placement question, and cost-vs-value question in chat before writing code. |
| `architecture-doc-triggers.mdc` | Read the architecture doc listed in `docs/developers/architecture/README.md` before editing any covered area. Editing an area with no row is itself a signal. |
| `binding-rules-quick-reference.mdc` | Index of the most important binding rules; surfaces them in the IDE without re-reading `CLAUDE.md`. When this index disagrees with `CLAUDE.md`, `CLAUDE.md` wins. |
| `code-doc-lockstep.mdc` | Code changes in areas covered by architecture docs, feature docs, OpenAPI specs, runbooks, or SDD stories must update the matching docs in the same commit. Enforced mechanically by `scripts/check-doc-lockstep.mjs`. |
| `completion-time-self-audit.mdc` | The 4-question × 2-round self-audit runs before claiming work done: Q1 all todos? Q2 no stubs in prod? Q3 tests cover changes? Q4 no "fix later"? Two consecutive clean rounds required. |
| `complex-task-plan-todo.mdc` | For complex tasks (more than 2 files, cross-cutting, SDD-tracked, API contract change, high-blast-radius surface), Plan and Todo list are non-waivable. |
| `english-only.mdc` | All project artifacts committed to the repo must be English. Conversation language can match the user; files stay English. |
| `no-backward-compatibility.mdc` | Pre-GA: delete obsolete code outright when replacing it. No phased compatibility rollouts, no `@deprecated` markers kept alive, no feature flags for rollback. "Phase" in plans means order of work. |
| `pre-edit-reading.mdc` | Read all three pre-edit docs before writing any code change: architecture doc(s), feature doc(s), and `docs/developers/workflow/conventions.md`. |
| `sdd-workflow.mdc` | The full SDD pipeline: Plan + Todo → Architecture → Requirements → SDD → OpenAPI → Code → Unit Tests → Verify → Ask-about-commit. No step skipped above triviality. |
| `session-handoff.mdc` | When approaching context-full (auto-compact near, multi-phase program closing, high turn count, post-major-push), proactively offer a handoff document. |
| `sub-agent-dispatch.mdc` | Discipline for Agent-tool dispatches: what to delegate (mechanical work, parallel research, bounded audits), what not to (understanding new problems, scope-changing decisions, any git commit or push), and the required prompt shape. |
| `unit-test-coverage-95.mdc` | Every Go package must hit ≥95% statement coverage or be in the allowlist with category and rationale. Pre-commit hook enforces on staged packages; full sweep via `npm run check:coverage`. |
| `worktree-per-session.mdc` | Each parallel Claude Code or Cursor session runs in its own `git worktree`. Working tree, index, and `.git/index.lock` are private; `git stash`, `git add -A`, `git restore` are safe inside the worktree. |

---

## Glob-scoped rules (21 rules)

These rules fire only when a file matching the `globs:` pattern is open or staged. Each enforces a domain-specific binding.

### AI Gateway and provider adapters

| Rule file | Globs | What it enforces |
|---|---|---|
| `ai-gateway-smoke-mandatory.mdc` | `packages/ai-gateway/**`, `packages/shared/transport/normalize/**`, `tools/db-migrate/schema.prisma`, `traffic_event` migration files | AI Gateway or `traffic_event` changes must run the AI Gateway smoke before completion. Catches cross-ingress asymmetry, cost/token accuracy, cache classification bugs. |
| `provider-adapter-canonical-openai.mdc` | `packages/ai-gateway/internal/providers/**`, `packages/shared/traffic/adapters/**`, `canonicalbridge/**`, `canonicalext/**` | Canonical format is OpenAI shape. Non-OpenAI adapters own full bidirectional translation. Extension fields ride `nexus.ext.<provider>.<key>`. Cross-format callers canonicalize before the codec. Streaming + non-streaming parity required. |
| `token-field-stamp-sweep.mdc` | `packages/ai-gateway/internal/**/proxy*.go`, `packages/ai-gateway/internal/providers/**` | Adding a new usage/token field requires 5 stamp sites: `handleNonStream`, `handleStream`, `proxy_cache.go:cacheStoreNonStream`, `proxy_cache.go:cacheStoreStream`, and the DryRun path. Missing the cache-side sites = all cache traffic NULL on the new column. |
| `text-first-normalizer.mdc` | `packages/compliance-proxy/internal/**`, `packages/agent/internal/**`, `packages/shared/transport/normalize/**`, `packages/shared/traffic/**` | For consumer-surface traffic (chatgpt-web, claude-web, cursor, gemini-web), the normalizer's only required output is readable text. Token/usage stats are acceptable losses at this stage. |

### Agent runtime and macOS NE

| Rule file | Globs | What it enforces |
|---|---|---|
| `agent-runtime-invariants.mdc` | `packages/agent/internal/**`, `packages/agent/cmd/**`, `packages/shared/audit/**` | Three agent runtime bindings: paths come from `platform.DefaultPaths()` (never hardcoded), traffic upload level enum {all,processed,blocked} filtered at agent emit-time, audit events must stamp unconditionally or strip-empty for CHECK-constrained columns. |
| `ne-fail-open.mdc` | `packages/agent/platform/darwin/**`, `packages/agent/internal/daemon/**`, `packages/agent/internal/quicbundles/**` | The 5 NE fail-open safety rules: synchronous `handleNewFlow` decisions, fail-open timeouts on every async callback (2s for `requestDecision`, 500ms for `peekSNIThenRelay`), no hardcoded enforcement lists in Swift, `isLikelyXyz = true` patterns banned, system DNS/DHCP/Push processes must never have their UDP closed. |

### Control Plane and IAM

| Rule file | Globs | What it enforces |
|---|---|---|
| `iam-impact-review.mdc` | `packages/control-plane/internal/handler/**`, `packages/control-plane-ui/src/routes/**`, `Sidebar/**`, `packages/shared/identity/iam/**`, `tools/db-migrate/seed/seed.ts` | 5-step IAM audit for any admin endpoint, sidebar nav, or route change. UI `allowedActions` and backend `iamMW(...)` must reference the same action. Seed parity must hold. Record the IAM decision in the commit message. |
| `nrn-builder-canonical.mdc` | `packages/control-plane/internal/handler/**`, `packages/control-plane/internal/iam/**`, `packages/shared/identity/iam/**` | `iamMW(action)` builds the request NRN via `iam.BuildRequestNRNForAction`. Never hardcode the resourceType string in handler glue — hardcoded strings drift from the IAM catalog and produce silent 403s. |
| `vk-org-resolution.mdc` | `packages/ai-gateway/internal/store/virtualkey.go`, related vk store files, `packages/control-plane/internal/ai/virtualkeys/**` | VK org resolution has two join chains: application VKs go via `VirtualKey.projectId → Project → Organization`; personal VKs go via `VirtualKey.ownerId → NexusUser → Organization`. `vkSelectSQL` must COALESCE both or personal VKs silently return NULL org. |

### UI and frontend

| Rule file | Globs | What it enforces |
|---|---|---|
| `agent-ui-terminology.mdc` | `packages/agent/ui/frontend/src/**`, `packages/ui-shared/src/i18n/locales/**` | Agent UI uses "traffic event" not "AI call". Forbidden: quota concept (agent has none), "disable agent" (it's "Protection Pause"), "platform connection" (it's "Connection"). |
| `design-tokens.mdc` | `packages/control-plane-ui/src/**/*.module.css`, `*.tsx`, `packages/ui-shared/src/**`, `packages/agent/ui/frontend/src/**/*.module.css` | All visual values (color, background, border, shadow, spacing, font-size, radius, z-index) must be referenced through CSS variables. No hex literals, no raw `rgb()`, no numeric pixel values outside of the token system. |
| `i18n-mandatory.mdc` | `packages/control-plane-ui/src/**/*.tsx`, `packages/ui-shared/src/**/*.tsx`, `packages/agent/ui/frontend/src/**/*.tsx` | Every user-visible string in JSX goes through `t('namespace:section.key')` from `react-i18next`. No hardcoded English strings in components. |
| `iot-terminology-boundary.mdc` | Control Plane UI, Agent UI, `packages/ui-shared/src/i18n/**`, CP and Hub handler files, `docs/users/**` | IoT vocabulary (Thing, Shadow, desired, reported, drift) is internal-only. User-facing surfaces use product terms: node, config sync, target config, applied config, out of sync. |
| `ui-shared-boundary.mdc` | `packages/ui-shared/**` | `packages/ui-shared` is a leaf in the import graph. It must never import from `control-plane-ui` or `agent/ui/frontend`. Both consumers import from it; it does not import from them. |
| `useapi-querykey.mdc` | `packages/control-plane-ui/src/**/*.tsx`, `*.ts` | Every `useApi(fetcher, queryKey)` call starts `queryKey` with at least two string literals: a domain prefix (`admin`, `my`, `user`, or `proxy`) and a resource name, followed by state variables. |

### Infrastructure

| Rule file | Globs | What it enforces |
|---|---|---|
| `configuration-architecture.mdc` | `**/*.yaml`, `**/*.yml`, `packages/shared/schemas/configkey/**`, `packages/**/config/**`, `tools/db-migrate/schema.prisma`, `seed/**` | Config changes conform to the 4-layer model. New configKeys update the §7 catalog and `packages/shared/schemas/configkey/` (constants, `ValidByThingType`, `TypedRegistry`) in the same PR. |
| `migration-timestamp-unique.mdc` | `tools/db-migrate/prisma/migrations/**`, `tools/db-migrate/prisma/schema.prisma` | Every migration folder name starts with a unique `YYYYMMDDHHMMSS` prefix. Two folders sharing a prefix cause Prisma to silently skip one. |
| `redis-cache-only.mdc` | `packages/**/*.go`, `tools/**/*.go` | Redis is cache-only: sessions, IAM cache, rate limiting, response cache, desired-state cache, quota counters. No `Subscribe`, `PSubscribe`, or `Publish` for cross-service event notification. Config invalidation goes via Hub WebSocket. |
| `secrets-env-only.mdc` | `**/*.yaml`, `**/*.yml`, `.env.example`, `packages/**/config/**` | No secret field in committed YAML. Auth tokens, HMAC keys, encryption keys, internal-service tokens, DB passwords ride env variables documented in `.env.example`. Cross-service shared secrets tagged `[MUST MATCH]`. |
| `test-env-files.mdc` | `tests/**`, `.claude/skills/prod-login/**`, `.claude/skills/prod-deploy/**`, `.claude/skills/prod-debug/**` | Tests and `prod-*` skills read from `tests/.env.<target>` only. Missing `NEXUS_TEST_TARGET` on non-TTY runs is a fail-closed error. Hostname allowlist per target — `local` allows localhost only. |
| `go-conventions.mdc` | `**/*.go`, `**/go.mod`, `**/go.sum` | Full-module-path imports, `go.work` workspace discipline, `replace` directives sibling-only, `packages/shared/` vetted dependency set, no `sqlc`, `shared/` API additive-only once shipped in a released Agent binary. |

---

## Rule anatomy

Every `.mdc` file follows the same structure:

```yaml
---
description: <one-line summary for IDE rule picker>
alwaysApply: true        # OR
globs:
  - packages/some/path/**
---
```

Below the frontmatter: a Markdown document with the binding rules, required code patterns, forbidden patterns, and recovery guidance. Rules are kept dense and specific — a rule that is too general is interpreted loosely by the agent. Each rule references the `CLAUDE.md` section or incident that motivated it; when the two disagree, `CLAUDE.md` wins.

The most effective rules are the ones that carry both a positive pattern (what to do) and a negative pattern (what to never do), with concrete code examples for each. A rule that only says "don't do X" leaves the agent guessing about the correct alternative; a rule that says "don't do X, do Y instead" closes the loop.

## Canonical docs

- [`ai-workflow.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/ai-workflow.md) — the four artifact layers including the cursor rules role
- [`CLAUDE.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CLAUDE.md) — canonical binding rules that the cursor rules surface

**Adjacent wiki pages**: [Workbench Overview](Workbench-Overview) · [Workbench CLAUDE md Anatomy](Workbench-CLAUDE-md-Anatomy) · [Workbench Claude Code Skills](Workbench-Claude-Code-Skills) · [Workbench Forking Guide](Workbench-Forking-Guide) · [Workbench Lessons Learned](Workbench-Lessons-Learned)
