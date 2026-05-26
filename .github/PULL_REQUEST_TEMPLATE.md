<!--
Thanks for contributing to Nexus Gateway! Before opening this PR, please
work through the checklist below. Items in **bold** are binding per
CLAUDE.md and CONTRIBUTING.md.
-->

## What this change does

<!-- One or two sentences. Focus on the WHY rather than the WHAT — the
diff already shows the what. -->

## Why

<!-- Link the issue (Fixes #123), or describe the user-visible behavior
this addresses. -->

## Pre-edit reading (3-doc rule)

- [ ] **I read the architecture doc(s) listed in `docs/developers/architecture/README.md` for the area I changed** — _which doc(s)?_:
- [ ] **I read the matching feature doc** under `docs/users/features/` if this change touches a user-visible surface — _which doc?_:
- [ ] **I read `docs/developers/workflow/conventions.md`** (code style).

If a row for your edit area didn't exist in the trigger map, that itself is a signal — either an undocumented subsystem or a new one that needs its own arch doc.

## Workflow

- [ ] **Plan + Todo were live** before code (Cursor `TodoWrite` or Claude Code `TaskCreate`).
- [ ] No code-level `TODO` / `FIXME` / `XXX` / `unimplemented` / `not implemented` / `stub` / `mock` strings in production code (test mocks are fine).
- [ ] **English only** for committed text (docs, comments, commit messages, READMEs, config-doc strings).
- [ ] No `git stash` was used (see the binding rule in CLAUDE.md — parallel sessions share the working tree).
- [ ] All my commits use **explicit pathspec** so no other session's WIP got swept in.

## High-blast-radius surfaces — check if your change touches any

- [ ] **macOS NE provider** (`packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/**`) — I respected the [fail-open invariants](../docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md) listed in CLAUDE.md.
- [ ] **Admin API endpoint / sidebar / route changes** — I ran the [IAM impact review](../CLAUDE.md#mandatory-rules) and updated `seed.ts` + `iam/managed.go` if I introduced a new resource.
- [ ] **Provider adapter / format translator** — my change conforms to the 7-rule contract in `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` §3a (run `/adapter-conformance-check` if uncertain).
- [ ] **`packages/shared/**`** — additive-only public API change.
- [ ] **Prisma migration** — the timestamp prefix is unique (no two folders share `YYYYMMDDHHMMSS`).

## Tests + CI

- [ ] `go test -race -count=1` passes for every module I touched.
- [ ] Each new Go package has ≥95% statement coverage (or an entry in `scripts/.coverage-allowlist` with a category rationale).
- [ ] `npm run check:all` passes (i18n parity + design tokens + arch-doc triggers + jobs-catalogue lockstep + …).
- [ ] If I added a new architecture doc, I also added the matching row to `docs/developers/architecture/README.md` (pre-commit lockstep guard checks).

## Commit hygiene

- [ ] Commit message follows the repo's `<type>(<scope>): <summary>` pattern (`feat` / `fix` / `refactor` / `docs` / `chore` / `test`).
- [ ] No co-author trailers from prior sessions accidentally carried over.
