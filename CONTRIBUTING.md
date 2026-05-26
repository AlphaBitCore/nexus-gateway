# Contributing to Nexus Gateway

Welcome! Three things to read **before** you start writing code:

1. [`CLAUDE.md`](./CLAUDE.md) — binding rules. Plan, Todo, English-only, no `git stash`, IAM impact review, macOS NE fail-open, etc.
2. [`docs/README.md`](./docs/README.md) — onboarding + the developer doc index.
3. [`docs/developers/architecture/README.md`](./docs/developers/architecture/README.md) — **the** "what to read when editing X" map.

If you have read just those three, you can navigate the rest of the codebase confidently.

---

## How to make a change (the short version)

The full workflow is in CLAUDE.md → "Mandatory Development Workflow". The compressed version:

1. **Plan** — write what you'll do (approach, scope, risks, file touch list). Even for one-line fixes.
2. **Todo list** — capture the plan as discrete, verifiable tasks. The todo list is the single source of truth for outstanding work.
3. **Architecture** — find your edit area in `docs/developers/architecture/README.md`. Read the listed doc(s) **first**. If your edit area isn't covered, raise it.
4. **Requirements / SDD / OpenAPI** — for new features; cross-link the relevant epic.
5. **Implement** — match the spec exactly. No placeholder code.
6. **Test** — `go test -race -count=1`, `npm test` per scope.
7. **Verify** — green tests + the 14-point checklist in CLAUDE.md.
8. **Ask whether to commit** — never auto-commit. Wait for explicit user instruction.

## Style + conventions

[`docs/developers/workflow/conventions.md`](./docs/developers/workflow/conventions.md) is the **soft guidance** companion to CLAUDE.md's binding rules. Read it once when you're new; refer back during review.

## Local dev

```bash
./scripts/dev-start.sh                                              # one-command bootstrap
cd packages/nexus-hub        && go run ./cmd/nexus-hub/             # port 3060
cd packages/control-plane    && go run ./cmd/control-plane/         # port 3001
cd packages/ai-gateway       && go run ./cmd/ai-gateway/            # port 3050
cd packages/compliance-proxy && go run ./cmd/compliance-proxy/      # port 3040
npm run dev:control-plane-ui                                        # port 3000
```

Admin API debugging:

```bash
source tests/lib/loadenv.sh local                                   # loads tests/.env.local (+ .example defaults)
source tests/lib/auth.sh
cp_login                                                            # caches token at /tmp/nexus_test_token_local
cp_curl /api/admin/<path>                                           # any path
```

Seed credentials: `admin@nexus.ai / admin123` (super-admin).

## Pre-commit checks

Run before pushing:

```bash
npm run check:i18n
npm run check:design-tokens
npm run check:terminology
npm run check:json-dupkeys
npm run check:tz
npm run check:arch-doc-triggers
npm run check:migration-timestamps
```

Or run all at once:

```bash
npm run check:all
```

CI runs the same checks on every push.

## High-blast-radius surfaces — extra care

Before touching any of these, read the linked doc, ping the area owner, and treat the change as safety-critical:

| Surface | Doc | Why |
|---|---|---|
| `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/` | [`agent-ne-fail-open-architecture.md`](./docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md) | A misbehaving NE provider kills the user's network. |
| Any admin endpoint, sidebar nav, or route | [`iam-identity-architecture.md`](./docs/developers/architecture/services/control-plane/iam-identity-architecture.md) + CLAUDE.md "IAM impact review" | Mismatched UI/backend actions cause silent 403s. |
| Any Prisma migration | CLAUDE.md "Migration timestamp prefix must be unique" | Duplicate timestamps make Prisma silently skip migrations. |
| Token-field stamping in AI Gateway | [`provider-adapter-architecture.md`](./docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md) §5 | Missing the cache-side stamp sites NULLs all prod cache traffic. |
| Hook `onMatch.action` | [`hook-architecture.md`](./docs/developers/architecture/services/ai-gateway/hook-architecture.md) | Mis-set action silently changes enforcement (PII scanner 2026-05-13). |
| Provider credential plaintext | [`credentials-architecture.md`](./docs/developers/architecture/cross-cutting/safety/credentials-architecture.md) | Never logs / never commits. |

## Reviewing a PR

Use the 11-point review checklist in [`docs/developers/workflow/conventions.md`](./docs/developers/workflow/conventions.md) §11.

## Tooling we use heavily

- **Cursor** — `.cursor/rules/sdd-workflow.mdc` (alwaysApply) carries the binding workflow. Additional rules in `.cursor/rules/`.
- **Claude Code** — `.claude/skills/` includes `build-agent` (macOS Agent build — binding), `prod-deploy`, `prod-login`, `prod-debug`, `smoke-gateway`, plus several adapter-test skills.
- **Lint scripts** — `scripts/check-*` files; run via the `npm run check:*` entry points above.

## Asking questions / getting help

When something is unclear:

1. Search the docs (`docs/developers/architecture/`, `docs/users/features/`, `docs/operators/ops/runbooks/`).
2. Search the binding memory items in CLAUDE.md.
3. Ask in chat — the doc set grows by signal.

We grow the docs by reacting to confusion. If you found a gap, file a tiny PR adding a paragraph; that compounds.
